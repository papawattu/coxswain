package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// peekWorkspaceReader is the real WorkspaceReader: it runs a read-only peek pod
// against the workspace PVC (the same pattern as test/e2e's peek_workspace) to
// read a file out of the workspace. The runner image is used because it runs
// as the same UID as the sandbox (the repo is written by a different UID, and
// git refuses to operate on a repo it doesn't own).
// peekWorkspaceReader reads files from a Loop's workspace by running a command
// in a read-only "peek" pod (a cat command) on the workspace PVC. The kubectl
// exec functions are injectable fields (defaulting to the real kubectl) so the
// outcome parsing is testable without a cluster (F7: a failed cat must be an
// error, never content).
type peekWorkspaceReader struct {
	ns          string
	runnerImage string
	Loops       LoopClient

	// kubectlCreate / kubectlGet / kubectlLogs / kubectlDelete are the kubectl
	// exec seams (the test fakes return canned output). They default to the
	// real kubectl when nil.
	kubectlCreate func(ctx context.Context, stdin string, args ...string) error
	kubectlGetFn  func(ctx context.Context, args ...string) (string, error)
	kubectlLogsFn func(ctx context.Context, args ...string) (string, error)
	kubectlDelete func(ctx context.Context, args ...string) error
}

// Read runs `cat <path>` in a read-only peek pod mounted on the
// <loop>-workspace PVC and returns the file contents.
func (p *peekWorkspaceReader) Read(ctx context.Context, loop, path string) (string, error) {
	// The image is the Loop's sandbox agent image (spec.agent.image, or the
	// operator-injected default the sandbox actually runs): it is present on the
	// cluster (the sandbox uses it), so the peek pod schedules without a pull,
	// and it exists for a released install (A5) without baking a dev tag in.
	// The Loop's spec.agent.image is authoritative when set; otherwise the
	// operator injects its --runner-image default, which we read from the
	// sandbox pod. Fall back to a small pinned busybox if neither is found.
	image := p.runnerImage
	if image == "" {
		image = p.sandboxAgentImage(ctx, loop)
	}
	if image == "" {
		image = "busybox"
	}

	// Build the peek pod manifest (RO mount on the workspace PVC). generateName
	// gives a unique name (kubectl appends a random suffix), so a leftover peek
	// pod never collides with a new one. The container is hardened: non-root,
	// read-only rootfs, drop all caps, seccomp (runs in restricted namespaces).
	// It runs as UID 65532 (the coxswain non-root UID the controller uses for
	// the agent/verify/deliver pods — internal/controller/loop_controller.go
	// nonRootUID, loop_deliver_job.go deliverNonRootUID) so it can read the
	// workspace files, which are written by that same UID.
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  generateName: cox-plan-peek-%s-
  namespace: %s
  labels:
    coxswain.io/plan-peek: "%s"
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    seccompProfile:
      type: RuntimeDefault
  containers:
  - name: peek
    image: %s
    imagePullPolicy: IfNotPresent
    securityContext:
      runAsNonRoot: true
      runAsUser: 65532
      runAsGroup: 65532
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities:
        drop: ["ALL"]
    command: ['/bin/sh', '-c', 'cat %s 2>&1 || { echo plan-read-failed-cat-exit-$? >&2; exit 1; }']
    volumeMounts:
    - name: ws
      mountPath: /workspace
      readOnly: true
  volumes:
  - name: ws
    persistentVolumeClaim:
      claimName: %s-workspace
      readOnly: true
`, loop, p.ns, loop, image, path, loop)

	// Create from stdin (generateName requires create, not apply). The pod is
	// created fresh with a unique name each time and cleaned up by label delete.
	if err := p.peekKubectlCreate(ctx, manifest, "create", "-f", "-"); err != nil {
		return "", fmt.Errorf("create peek pod: %w", err)
	}
	// Best-effort cleanup: delete the peek pod when we're done (ignore the
	// error — the pod is short-lived; the delete is by label so the random
	// generateName suffix is irrelevant).
	defer func() {
		_ = p.peekKubectlDelete(ctx, "delete", "pod", "-l", "coxswain.io/plan-peek="+loop, "--wait=false", "--ignore-not-found")
	}()

	// Find the peek pod's name (generateName appends a random suffix).
	podName, err := p.peekKubectlGet(ctx, "get", "pod", "-l", "coxswain.io/plan-peek="+loop,
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil || podName == "" {
		return "", fmt.Errorf("cli: plan peek pod not found for loop %q", loop)
	}

	// Wait for the peek pod to reach a terminal phase and read its outcome. The
	// `cat` command exits immediately, so the pod goes Succeeded once the file
	// is read (or Failed if the file is missing or unreadable). Poll with a
	// delay up to a deadline (no back-to-back busy loop): a few 1s checks cover
	// the cat completing in ~2s, and a truly-stuck pod (scheduling/pull) is
	// caught by the deadline. A still-Pending pod after the deadline is an
	// error.
	// The B6 run 5/6 found the peek pod takes ~15s to start and fail (image
	// pull + PVC mount + container start); 8s is too short. 30s covers the
	// worst case on a slow node.
	outcome, err := p.pollPeekPodOutcome(ctx, podName)
	if err != nil {
		return "", err
	}
	if outcome.error {
		// F7 (dogfood 2 finding): a peek pod that ends Failed (or whose
		// container exited non-zero) is an ERROR, never content. Returning the
		// container log of a failed cat as the "file content" made cox approve
		// hash the cat ERROR STRING ("cat: can't open ...") instead of the plan
		// — three different plans with one approved hash. The exit code is
		// authoritative: the `cat` wrapper above exits non-zero when the file
		// is missing or unreadable (or "" when the shell is absent), and 0 only
		// when the file was read. The termination message (truncated to 4096
		// bytes by the runtime) carries the shell's stderr for the error text.
		return "", fmt.Errorf("cli: plan read failed (peek pod %s: phase=%s exit=%s): %s", podName, outcome.phase, outcome.exitCode, firstNonEmpty(outcome.terminationMsg, "the peek pod exited non-zero (file missing or unreadable)"))
	}
	// The cat output is the container's stdout, recorded in the pod's log. The
	// SUCCEEDED bytes are returned UNCHANGED: the approval contract hashes the
	// exact file bytes (the runner's plan-change check hashes the PLAN.md bytes
	// it wrote, runner/phase.go planChanged), so no trim — a trailing newline
	// is part of the hash.
	out, err := p.peekKubectlLogs(ctx, "logs", podName)
	if err != nil {
		return "", fmt.Errorf("read peek pod log: %w", err)
	}
	return out, nil
}

// peekPodOutcome is the outcome of a single read of the peek pod's status.
// It separates the pod-state parsing (testable without a cluster) from the
// polling loop (which sleeps). error is true when the pod ended in a state
// that is NOT a successful cat (phase Failed, or a container that terminated
// non-zero) — the F7 condition that must never be returned as content.
type peekPodOutcome struct {
	phase          string
	exitCode       string
	terminationMsg string
	error          bool
}

// peekPodOutcome reads the peek pod once and returns its outcome. It does not
// poll or sleep (the caller does that): one Get, one parse. A pod that has not
// reached a terminal phase yet (phase "" or Pending, or a not-yet-terminated
// container) returns error=false with the phase recorded (the caller keeps
// polling). A pod that ended Failed, or a container that terminated non-zero,
// returns error=true (the F7 condition).
func (p *peekWorkspaceReader) peekPodOutcome(ctx context.Context, podName string) peekPodOutcome {
	podJSON, _ := p.peekKubectlGet(ctx, "get", "pod", podName, "-o", "json")
	var pod struct {
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				State struct {
					Terminated *struct {
						ExitCode int64  `json:"exitCode"`
						Message  string `json:"message"`
					} `json:"terminated"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(podJSON), &pod); err != nil {
		// A parse failure is NOT an error outcome: the pod exists (the Get
		// succeeded), its status is just not readable this tick. The caller
		// keeps polling (phase "" is not terminal).
		return peekPodOutcome{phase: ""}
	}
	out := peekPodOutcome{phase: pod.Status.Phase}
	if len(pod.Status.ContainerStatuses) > 0 {
		if t := pod.Status.ContainerStatuses[0].State.Terminated; t != nil {
			out.exitCode = fmt.Sprint(t.ExitCode)
			out.terminationMsg = t.Message
			// A non-zero exit is an error outcome regardless of the pod phase
			// (the cat wrapper exits non-zero when the file is missing or
			// unreadable; 0 only on a successful read).
			if t.ExitCode != 0 {
				out.error = true
			}
		}
	}
	if pod.Status.Phase == "Failed" {
		out.error = true
	}
	return out
}

// pollPeekPodOutcome polls peekPodOutcome up to 30 times (1s apart) until the
// peek pod reaches a terminal phase (Succeeded or Failed) or its container
// terminates (an error outcome), then returns the outcome. A still-Pending pod
// after the deadline is an error.
func (p *peekWorkspaceReader) pollPeekPodOutcome(ctx context.Context, podName string) (peekPodOutcome, error) {
	var last peekPodOutcome
	for range 30 {
		last = p.peekPodOutcome(ctx, podName)
		// Stop when the pod is terminal (Succeeded/Failed) or the container has
		// terminated (an error outcome, e.g. a non-zero exit while the phase is
		// still settling). A non-terminated, non-terminal pod keeps polling.
		if last.phase == "Succeeded" || last.phase == "Failed" || last.error {
			return last, nil
		}
		time.Sleep(time.Second)
	}
	if last.phase == "" || last.phase == "Pending" {
		return last, fmt.Errorf("cli: plan peek pod did not run (phase=%s)", last.phase)
	}
	return last, nil
}

// firstNonEmpty returns the first non-empty string (error detail fallbacks).
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// sandboxAgentImage resolves the Loop's sandbox agent image: first the Loop's
// spec.agent.image (authoritative when set), then the sandbox pod's agent
// container image (the operator-injected --runner-image default when the Loop
// does not set one). Returns "" if neither is found.
func (p *peekWorkspaceReader) sandboxAgentImage(ctx context.Context, loop string) string {
	if p.Loops != nil {
		if obj, err := p.Loops.Get(ctx, loop); err == nil {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				if ai, ok := getNestedString(u, "spec", "agent", "image"); ok && ai != "" {
					return ai
				}
			}
		}
	}
	// The operator injects the --runner-image default into the sandbox pod;
	// read the agent container's image from the sandbox pod.
	out, err := p.kubectlGet(ctx, "get", "pod", loop+"-sandbox",
		"-o", "jsonpath={.spec.containers[?(@.name=='agent')].image}")
	if err != nil || out == "" {
		return ""
	}
	return strings.TrimSpace(out)
}

// kubectl runs a kubectl subcommand, reading stdin when -f - is used.
// peekKubectlCreate / peekKubectlGet / peekKubectlLogs / peekKubectlDelete are
// the kubectl exec seams for the peek reader. They default to the real kubectl
// (the p.kubectl* methods) when the injectable field is nil (production); a
// test sets the field to return canned output (F7: the outcome parsing must be
// testable without a cluster).
func (p *peekWorkspaceReader) peekKubectlCreate(ctx context.Context, stdin string, args ...string) error {
	if p.kubectlCreate != nil {
		return p.kubectlCreate(ctx, stdin, args...)
	}
	return p.kubectl(ctx, stdin, args...)
}

func (p *peekWorkspaceReader) peekKubectlGet(ctx context.Context, args ...string) (string, error) {
	if p.kubectlGetFn != nil {
		return p.kubectlGetFn(ctx, args...)
	}
	return p.kubectlGet(ctx, args...)
}

func (p *peekWorkspaceReader) peekKubectlLogs(ctx context.Context, args ...string) (string, error) {
	if p.kubectlLogsFn != nil {
		return p.kubectlLogsFn(ctx, args...)
	}
	return p.kubectlOut(ctx, args...)
}

func (p *peekWorkspaceReader) peekKubectlDelete(ctx context.Context, args ...string) error {
	if p.kubectlDelete != nil {
		return p.kubectlDelete(ctx, args...)
	}
	return p.kubectl(ctx, "", args...)
}

func (p *peekWorkspaceReader) kubectl(ctx context.Context, stdin string, args ...string) error {
	full := append(append([]string{}, p.kubeArg()...), args...)
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	// The create/delete of the peek pod is housekeeping; keep the user's stdout
	// clean (only the plan itself, read via kubectl logs/exec, reaches it). Errors
	// still go to stderr.
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	defer func() { _ = devNull.Close() }()
	cmd.Stdout = devNull
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// kubectlGet runs a kubectl subcommand and returns its stdout.
func (p *peekWorkspaceReader) kubectlGet(ctx context.Context, args ...string) (string, error) {
	full := append(append([]string{}, p.kubeArg()...), args...)
	var buf []byte
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	out, err := cmd.Output()
	buf = out
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(buf), fmt.Errorf("%s", strings.TrimSpace(string(ee.Stderr)))
		}
		return string(buf), err
	}
	return strings.TrimSpace(string(buf)), nil
}

// kubectlOut runs a kubectl subcommand and returns its stdout (for `logs`).
func (p *peekWorkspaceReader) kubectlOut(ctx context.Context, args ...string) (string, error) {
	full := append(append([]string{}, p.kubeArg()...), args...)
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), fmt.Errorf("%s: %s", strings.TrimSpace(string(ee.Stderr)), strings.TrimSpace(string(out)))
		}
		return string(out), err
	}
	return string(out), nil
}

// kubeArg returns the kubectl global flag args for this reader's namespace
// (--context/--namespace/--kubeconfig; each omitted when empty).
func (p *peekWorkspaceReader) kubeArg() []string {
	return kubeFlagArgs(p.ns)
}
