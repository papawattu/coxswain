package cli

import (
	"context"
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
type peekWorkspaceReader struct {
	ns          string
	runnerImage string
	Loops       LoopClient
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
    command: ["/bin/sh", "-c", "cat %s"]
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
	if err := p.kubectl(ctx, manifest, "create", "-f", "-"); err != nil {
		return "", fmt.Errorf("create peek pod: %w", err)
	}
	// Best-effort cleanup: delete the peek pod when we're done (ignore the
	// error — the pod is short-lived; the delete is by label so the random
	// generateName suffix is irrelevant).
	defer func() {
		_ = p.kubectl(ctx, "", "delete", "pod", "-l", "coxswain.io/plan-peek="+loop, "--wait=false", "--ignore-not-found")
	}()

	// Find the peek pod's name (generateName appends a random suffix).
	podName, err := p.kubectlGet(ctx, "get", "pod", "-l", "coxswain.io/plan-peek="+loop,
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil || podName == "" {
		return "", fmt.Errorf("cli: plan peek pod not found for loop %q", loop)
	}

	// Wait for the peek pod to reach a terminal phase. The `cat` command exits
	// immediately, so the pod goes Succeeded once the file is read (or Failed if
	// the file is missing and the shell's exit is non-zero). Poll with a delay up
	// to a deadline (no back-to-back busy loop): a few 1s checks cover the cat
	// completing in ~2s, and a truly-stuck pod (scheduling/pull) is caught by
	// the deadline. A still-Pending pod after the deadline is an error.
	// The B6 run 5/6 found the peek pod takes ~15s to start and fail (image
	// pull + PVC mount + container start); 8s is too short. 30s covers the
	// worst case on a slow node.
	phase := ""
	for range 30 {
		phase, _ = p.kubectlGet(ctx, "get", "pod", podName, "-o", "jsonpath={.status.phase}")
		if phase == "Succeeded" || phase == "Failed" {
			break
		}
		time.Sleep(time.Second)
	}
	if phase == "" || phase == "Pending" {
		return "", fmt.Errorf("cli: plan peek pod did not run (phase=%s, loop %q)", phase, loop)
	}
	// The cat output is the container's stdout, recorded in the pod's log.
	out, err := p.kubectlOut(ctx, "logs", podName)
	if err != nil {
		// A missing PLAN.md is the no-plan case (the runner writes it at the
		// start of the Implementing phase; PLAN.md is optional), not an error.
		if strings.Contains(err.Error(), "No such file") || strings.Contains(err.Error(), "no such file") {
			return "", nil
		}
		return "", fmt.Errorf("read peek pod log: %w", err)
	}
	return strings.TrimSpace(out), nil
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
