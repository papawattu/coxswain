package cli

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// A peek pod status whose container terminated non-zero (the F7 shape: cat
// could not open the file, the shell exited 1, the pod is Failed).
const peekPodFailedJSON = `{
  "status": {
    "phase": "Failed",
    "containerStatuses": [{
      "state": {
        "terminated": {
          "exitCode": 1,
          "reason": "Error",
          "message": "cat: can't open '/workspace/.coxswain/PLAN.md': No such file or directory"
        }
      }
    }]
  }
}`

// A peek pod that succeeded (the container exited 0).
const peekPodSucceededJSON = `{
  "status": {
    "phase": "Succeeded",
    "containerStatuses": [{
      "state": {
        "terminated": {
          "exitCode": 0,
          "reason": "Completed"
        }
      }
    }]
  }
}`

// A peek pod that is still running (not terminal yet).
const peekPodRunningJSON = `{
  "status": {
    "phase": "Running",
    "containerStatuses": [{
      "state": {
        "running": {}
      }
    }]
  }
}`

// newPeekTestReader builds a peekWorkspaceReader with the kubectl seams stubbed:
// the pod-name lookup returns podName, the pod-json lookup returns podJSON, and
// `logs` returns logsOut. This isolates the outcome parsing from a cluster.
func newPeekTestReader(podJSON, logsOut string) *peekWorkspaceReader {
	return &peekWorkspaceReader{
		ns: "ns",
		kubectlGetFn: func(ctx context.Context, args ...string) (string, error) {
			// The pod-name lookup has "-l"; the pod-json lookup does not.
			if slices.Contains(args, "-l") {
				return "peek-abc", nil
			}
			return podJSON, nil
		},
		kubectlLogsFn: func(ctx context.Context, args ...string) (string, error) {
			return logsOut, nil
		},
		kubectlCreate: func(ctx context.Context, stdin string, args ...string) error { return nil },
		kubectlDelete: func(ctx context.Context, args ...string) error { return nil },
	}
}

// TestPeekPodOutcome_FailedIsError verifies that a Failed peek pod (the
// container terminated non-zero) is an ERROR outcome — the F7 condition that
// must never be returned as content. This is the unit-level catch for the
// mutation where the reader returned the container log as the file content on
// a Failed pod.
func TestPeekPodOutcome_FailedIsError(t *testing.T) {
	p := newPeekTestReader(peekPodFailedJSON, "cat: can't open ...")
	out := p.peekPodOutcome(context.Background(), "peek-abc")
	if !out.error {
		t.Fatal("a Failed peek pod (container terminated non-zero) must be an error outcome")
	}
	if out.phase != "Failed" {
		t.Errorf("phase = %q, want Failed", out.phase)
	}
	if out.exitCode != "1" {
		t.Errorf("exitCode = %q, want 1", out.exitCode)
	}
	if out.terminationMsg == "" {
		t.Error("terminationMsg should carry the container's stderr (the cat error)")
	}
}

// TestPeekPodOutcome_SucceededIsNotError verifies that a Succeeded peek pod
// (container exited 0) is NOT an error outcome (the file was read).
func TestPeekPodOutcome_SucceededIsNotError(t *testing.T) {
	p := newPeekTestReader(peekPodSucceededJSON, "step one\n")
	out := p.peekPodOutcome(context.Background(), "peek-abc")
	if out.error {
		t.Fatal("a Succeeded peek pod (container exited 0) must NOT be an error outcome")
	}
	if out.phase != "Succeeded" {
		t.Errorf("phase = %q, want Succeeded", out.phase)
	}
}

// TestPeekPodOutcome_RunningIsNotTerminal verifies that a running (non-
// terminal) peek pod is not an error and not terminal (the caller keeps
// polling).
func TestPeekPodOutcome_RunningIsNotTerminal(t *testing.T) {
	p := newPeekTestReader(peekPodRunningJSON, "")
	out := p.peekPodOutcome(context.Background(), "peek-abc")
	if out.error {
		t.Fatal("a running peek pod must NOT be an error outcome")
	}
	if out.phase != "Running" {
		t.Errorf("phase = %q, want Running", out.phase)
	}
}

// TestPeekWorkspaceRead_FailedPodIsError verifies that Read returns an ERROR
// (not the container log) when the peek pod ends Failed — the F7 regression.
// The mutation (return the log as content on a Failed pod) makes this FAIL.
func TestPeekWorkspaceRead_FailedPodIsError(t *testing.T) {
	p := newPeekTestReader(peekPodFailedJSON, "cat: can't open '/workspace/.coxswain/PLAN.md': No such file or directory")
	_, err := p.Read(context.Background(), "loop1", planPath)
	if err == nil {
		t.Fatal("Read with a Failed peek pod: want an error, got nil (the container log must not be returned as content)")
	}
	if !strings.Contains(err.Error(), "plan read failed") {
		t.Errorf("error = %q, want it to say 'plan read failed'", err.Error())
	}
}

// TestPeekWorkspaceRead_SucceededReturnsLog verifies that Read returns the
// container's stdout (the plan bytes) UNCHANGED when the peek pod succeeds.
func TestPeekWorkspaceRead_SucceededReturnsLog(t *testing.T) {
	const planBytes = "step one\nstep two\n" // trailing newline is content
	p := newPeekTestReader(peekPodSucceededJSON, planBytes)
	got, err := p.Read(context.Background(), "loop1", planPath)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != planBytes {
		t.Errorf("Read returned %q, want the log UNCHANGED %q (no trim — the hash is over the exact bytes)", got, planBytes)
	}
}
