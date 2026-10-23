package cli

// MemoryReader reads the memory PVC (the `cox memory` command's real
// boundary: a read-only peek pod against the memory PVC, the same pattern as
// peekWorkspaceReader reads the workspace PVC). The CLI inspects the shared
// lessons for a repo (P5a); it does NOT write (P5b's writer Job writes; P5c
// export/import is manual).
//
// Read returns (the memory's contents, whether any file was found, error).
// found is false when the PVC exists but is empty (a fresh repo with no
// lessons yet); a missing PVC (a repo the operator has never run a Loop on)
// is memoryNotFoundErr (the CLI prints a note, not an error).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/papawattu/coxswain/pkg/memkey"
)

type MemoryReader interface {
	// Read returns the memory PVC's contents (all files, one per block with
	// the file name as a header), whether any file was found, and an error. A
	// missing PVC is memoryNotFoundErr (a fresh repo: the CLI prints a note).
	Read(ctx context.Context, pvcName string) (string, bool, error)
}

// peekMemoryReader is the real MemoryReader: a read-only peek pod against the
// memory PVC (the same kubectl-seam pattern as peekWorkspaceReader). The peek
// pod mounts the memory PVC READ-ONLY (the CLI never writes the memory PVC)
// and cats its files (one per block, the file name as a header). It reuses
// the peekWorkspaceReader's kubectl exec seams (create/get/logs/delete) so
// the outcome parsing is testable without a cluster (the same F7 rule: a
// failed cat is an error, never content).
type peekMemoryReader struct {
	ns string

	// kubectlCreate / kubectlGet / kubectlLogs / kubectlDelete are the
	// kubectl exec seams (the same as peekWorkspaceReader; nil = the real
	// kubectl). They default to the real kubectl when nil.
	kubectlCreate func(ctx context.Context, stdin string, args ...string) error
	kubectlGetFn  func(ctx context.Context, args ...string) (string, error)
	kubectlLogsFn func(ctx context.Context, args ...string) (string, error)
	kubectlDelete func(ctx context.Context, args ...string) error
}

// Read runs `find /memory -type f | cat` in a read-only peek pod mounted on
// the memory PVC and returns the contents (one file per block, the file name
// as a header). A missing PVC is memoryNotFoundErr (a fresh repo: the CLI
// prints a note, not an error).
func (p *peekMemoryReader) Read(ctx context.Context, pvcName string) (string, bool, error) {
	return p.runPod(ctx, pvcName, true, []string{memShell, "-c", memoryReadScript})
}

// memoryReadScript is the read pod's script (one file per block, the file
// name as a header).
const memoryReadScript = `if [ -z "$(find /memory -mindepth 1 -maxdepth 1 -type f 2>/dev/null)" ]; then exit 0; fi; find /memory -mindepth 1 -maxdepth 1 -type f | sort | while IFS= read -r f; do echo "-- ${f#/memory/}:"; cat "$f"; echo; done 2>&1 || { echo memory-read-failed-exit-$? >&2; exit 1; }`

// runPod runs one hardened pod against the memory PVC with the given command
// and returns its log (found=false when the log is empty). readOnly selects
// the PVC mount mode: Read (inspect, export) is read-only; only the P5d
// import writes (readOnly=false). A missing PVC is memoryNotFoundErr.
func (p *peekMemoryReader) runPod(ctx context.Context, pvcName string, readOnly bool, command []string) (string, bool, error) {
	// The memory PVC must exist (a missing PVC is a fresh repo: the CLI
	// prints a note, not an error). A real read failure (a PVC that exists
	// but the API rejects) is an error.
	pvcJSON, err := p.peekKubectlGet(ctx, "get", "pvc", pvcName, "-o", "json")
	if err != nil || pvcJSON == "" {
		return "", false, memoryNotFoundErr{}
	}

	// Build the peek pod manifest (RO mount on the memory PVC). generateName
	// gives a unique name (a leftover peek pod never collides with a new one).
	// The container is hardened: non-root, read-only rootfs, drop all caps,
	// seccomp (the same as the workspace peek pod). It runs as UID 65532
	// (the coxswain non-root UID the controller uses for the agent/verify/
	// deliver pods — the memory PVC's files are written by P5b's writer Job,
	// which runs as that same UID).
	cmdJSON, jerr := json.Marshal(command) // JSON is valid YAML flow syntax.
	if jerr != nil {
		return "", false, fmt.Errorf("encode memory pod command: %w", jerr)
	}
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  generateName: cox-memory-peek-
  namespace: %s
  labels:
    coxswain.io/memory-peek: "%s"
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    seccompProfile:
      type: RuntimeDefault
  containers:
  - name: peek
    image: busybox
    imagePullPolicy: IfNotPresent
    securityContext:
      runAsNonRoot: true
      runAsUser: 65532
      runAsGroup: 65532
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities:
        drop: ["ALL"]
    command: %s
    volumeMounts:
    - name: mem
      mountPath: /memory
      readOnly: %t
  volumes:
  - name: mem
    persistentVolumeClaim:
      claimName: %s
      readOnly: %t
`, p.ns, memkey.Key(pvcName), cmdJSON, readOnly, pvcName, readOnly)

	// Create from stdin (generateName requires create, not apply).
	if err := p.peekKubectlCreate(ctx, manifest, "create", "-f", "-"); err != nil {
		return "", false, fmt.Errorf("create memory peek pod: %w", err)
	}
	// Best-effort cleanup: delete the peek pod when we're done (by label).
	defer func() {
		_ = p.peekKubectlDelete(ctx, "delete", "pod", "-l", "coxswain.io/memory-peek="+memkey.Key(pvcName), "--wait=false", "--ignore-not-found")
	}()

	// Find the peek pod's name (generateName appends a random suffix).
	podName, err := p.peekKubectlGet(ctx, "get", "pod", "-l", "coxswain.io/memory-peek="+memkey.Key(pvcName),
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil || podName == "" {
		return "", false, fmt.Errorf("cli: memory peek pod not found for %q", pvcName)
	}

	// Wait for the peek pod to reach a terminal phase and read its outcome
	// (the same bounded-poll pattern as the workspace peek pod; the F7 rule:
	// a Failed peek pod is an error, never content).
	outcome, err := p.pollPeekPodOutcome(ctx, podName)
	if err != nil {
		return "", false, err
	}
	if outcome.error {
		return "", false, fmt.Errorf("cli: memory read failed (peek pod %s: phase=%s exit=%s): %s", podName, outcome.phase, outcome.exitCode, firstNonEmpty(outcome.terminationMsg, "the peek pod exited non-zero (the memory PVC is not Bound or is empty)"))
	}
	out, err := p.peekKubectlLogs(ctx, "logs", podName)
	if err != nil {
		return "", false, fmt.Errorf("read memory peek pod log: %w", err)
	}
	if len(out) == 0 {
		return "", false, nil // an empty PVC (no lessons yet).
	}
	return out, true, nil
}

// peekKubectlCreate / peekKubectlGet / peekKubectlLogs / peekKubectlDelete are
// the kubectl exec seams (the same as peekWorkspaceReader; nil = the real
// kubectl, built from the CLI's --kubeconfig/--context/--namespace globals).
func (p *peekMemoryReader) peekKubectlCreate(ctx context.Context, stdin string, args ...string) error {
	if p.kubectlCreate != nil {
		return p.kubectlCreate(ctx, stdin, args...)
	}
	// The command args (create -f -) MUST follow the global flags: the
	// kind run found this call passed only the globals, so kubectl never
	// created the peek pod ("memory peek pod not found").
	full := append(append([]string{}, memoryKubeArgs()...), args...)
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (p *peekMemoryReader) peekKubectlGet(ctx context.Context, args ...string) (string, error) {
	if p.kubectlGetFn != nil {
		return p.kubectlGetFn(ctx, args...)
	}
	full := append(append([]string{}, memoryKubeArgs()...), args...)
	out, err := exec.CommandContext(ctx, "kubectl", full...).Output()
	return string(out), err
}

func (p *peekMemoryReader) peekKubectlLogs(ctx context.Context, args ...string) (string, error) {
	if p.kubectlLogsFn != nil {
		return p.kubectlLogsFn(ctx, args...)
	}
	full := append(append([]string{}, memoryKubeArgs()...), args...)
	out, err := exec.CommandContext(ctx, "kubectl", full...).Output()
	return string(out), err
}

func (p *peekMemoryReader) peekKubectlDelete(ctx context.Context, args ...string) error {
	if p.kubectlDelete != nil {
		return p.kubectlDelete(ctx, args...)
	}
	full := append(append([]string{}, memoryKubeArgs()...), args...)
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// memoryKubeArgs returns the kubectl args for the CLI's current --kubeconfig /
// --context / --namespace (the same as peekWorkspaceReader's kubeArg; the
// memory peek pod lives in the CLI's namespace).
func memoryKubeArgs() []string {
	var args []string
	if Kubeconfig != "" {
		args = append(args, "--kubeconfig", Kubeconfig)
	}
	if Context != "" {
		args = append(args, "--context", Context)
	}
	if Namespace != "" {
		args = append(args, "--namespace", Namespace)
	}
	return args
}

// pollPeekPodOutcome polls a peek pod's phase/termination (the same outcome
// parsing as peekWorkspaceReader; a Failed pod or a non-zero exit is an
// error, never content — the F7 rule).
func (p *peekMemoryReader) pollPeekPodOutcome(ctx context.Context, podName string) (peekPodOutcome, error) {
	var last peekPodOutcome
	for range 30 {
		last = memoryPeekPodOutcome(ctx, podName)
		if last.phase == peekPodPhaseSucceeded || last.phase == peekPodPhaseFailed || last.error {
			return last, nil
		}
		time.Sleep(time.Second)
	}
	if last.phase == "" || last.phase == "Pending" {
		return last, fmt.Errorf("cli: memory peek pod did not run (phase=%s)", last.phase)
	}
	return last, nil
}

// memoryPeekPodOutcome reads a peek pod's phase + container termination (the
// same as peekWorkspaceReader's peekPodOutcome; a read of the pod via
// kubectl get -o json).
func memoryPeekPodOutcome(ctx context.Context, podName string) peekPodOutcome {
	out, err := peekKubectlGetRaw(ctx, "get", "pod", podName, "-o", "json")
	if err != nil {
		return peekPodOutcome{error: true, terminationMsg: err.Error()}
	}
	return parsePeekPodOutcome(out)
}

// peekKubectlGetRaw is the raw kubectl get (the memory peek reader's seam;
// testable without a cluster).
func peekKubectlGetRaw(ctx context.Context, args ...string) (string, error) {
	full := append(append([]string{}, memoryKubeArgs()...), args...)
	out, err := exec.CommandContext(ctx, "kubectl", full...).Output()
	return string(out), err
}
