package cli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// peekWorkspaceReader is the real WorkspaceReader: it runs a read-only peek pod
// against the workspace PVC (the same pattern as test/e2e's peek_workspace) to
// read a file out of the workspace. The runner image is used because it runs
// as the same UID as the sandbox (the repo is written by a different UID, and
// git refuses to operate on a repo it doesn't own).
type peekWorkspaceReader struct {
	ns          string
	runnerImage string
}

// Read runs `cat <path>` in a read-only peek pod mounted on the
// <loop>-workspace PVC and returns the file contents.
func (p *peekWorkspaceReader) Read(ctx context.Context, loop, path string) (string, error) {
	image := p.runnerImage
	if image == "" {
		image = "coxswain-runner:dev"
	}
	// Build the peek pod manifest (RO mount on the workspace PVC).
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: cox-plan-peek-%s
  namespace: %s
spec:
  activeDeadlineSeconds: 60
  restartPolicy: Never
  containers:
  - name: peek
    image: %s
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
`, loop, p.ns, image, path, loop)

	// Apply from stdin, wait for Succeeded, read the log, delete.
	if err := p.kubectl(manifest, "apply", "-f", "-"); err != nil {
		return "", fmt.Errorf("apply peek pod: %w", err)
	}
	podName := "cox-plan-peek-" + loop
	// best-effort cleanup: delete the peek pod when we're done (ignore the
	// error — the pod is short-lived and activeDeadlineSeconds bounds it).
	defer func() { _ = p.kubectl("", "delete", "pod", podName, "--wait=false") }()

	// Wait for the pod to be Succeeded/Failed (bounded by activeDeadlineSeconds).
	var phase string
	for range 30 {
		out, err := p.kubectlGet("get", "pod", podName, "-o", "jsonpath={.status.phase}")
		if err == nil {
			phase = out
		}
		if phase == "Succeeded" || phase == "Failed" {
			break
		}
	}
	if phase != "Succeeded" {
		return "", fmt.Errorf("peek pod did not succeed (phase=%s)", phase)
	}
	var buf bytes.Buffer
	cmd := p.kubectlCmd("logs", podName)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("read peek pod log: %w", err)
	}
	return buf.String(), nil
}

// kubectl runs a kubectl subcommand, reading stdin when -f - is used.
func (p *peekWorkspaceReader) kubectl(stdin string, args ...string) error {
	full := append(append([]string{}, p.kubeArg()...), args...)
	cmd := exec.Command("kubectl", full...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	return cmd.Run()
}

// kubectlGet runs a kubectl subcommand and returns its stdout.
func (p *peekWorkspaceReader) kubectlGet(args ...string) (string, error) {
	full := append(append([]string{}, p.kubeArg()...), args...)
	var buf bytes.Buffer
	cmd := exec.Command("kubectl", full...)
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

// kubectlCmd builds a kubectl *exec.Cmd for the caller to run.
func (p *peekWorkspaceReader) kubectlCmd(args ...string) *exec.Cmd {
	full := append(append([]string{}, p.kubeArg()...), args...)
	return exec.Command("kubectl", full...)
}

// kubeArg returns the kubectl global flag args for this reader's namespace
// (--context/--namespace/--kubeconfig; each omitted when empty).
func (p *peekWorkspaceReader) kubeArg() []string {
	return kubeFlagArgs(p.ns)
}
