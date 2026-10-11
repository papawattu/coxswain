package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// kubectlLogStreamer is the real LogStreamer: it shells out to `kubectl logs`
// for the component's pod (CLI-PLAN.md: the logs command shells out behind a
// thin runner interface that tests stub).
type kubectlLogStreamer struct {
	ns     string
	follow bool
	tail   int
}

// Stream runs `kubectl logs` for the component's pod and writes to
// os.Stdout. The follow flag adds -f. For the verify component, the verify
// Job pod has multiple containers (a noop main + check-N acceptance-check
// containers); the meaningful output is in the check-N containers, so Stream
// resolves the verify pod and streams its check-N containers (the pure
// logArgs covers the selector; this adds the container selection).
func (k *kubectlLogStreamer) Stream(ctx context.Context, loop, component string) error {
	if component == compVerify {
		return k.streamVerifyContainers(ctx, loop)
	}
	args := logArgs(k.ns, loop, component, k.follow, k.tail)
	if len(args) == 0 {
		return fmt.Errorf("cli: unknown component %q", component)
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// streamVerifyContainers streams the verify Job pod's check-N containers (the
// acceptance checks' output). The verify pod's main (noop) container is empty;
// the checks run in check-N. It resolves the verify pod by the
// coxswain.io/verify-for label and streams each check-N container.
func (k *kubectlLogStreamer) streamVerifyContainers(ctx context.Context, loop string) error {
	nsArgs := kubeFlagArgs(k.ns)
	// Find the verify pod (the verify-for label is on the Job's pod).
	podName, err := kubectlOutput(ctx, append(nsArgs, "get", "pod", "-l", verifyForLabel+"="+loop,
		"-o", "jsonpath={.items[0].metadata.name}"))
	podName = strings.TrimSpace(podName)
	if err != nil || podName == "" {
		// The verify pod is not present (the Job has not run yet, or the Loop is
		// past Verifying and it was cleaned up). An empty log is the no-verify case.
		return nil
	}
	// Enumerate the pod's containers and pick the check-N ones. The verify Job's
	// acceptance checks run in init containers (check-0, check-1, ...); the main
	// container (noop) is empty.
	containers, err := kubectlOutput(ctx, append(nsArgs, "get", "pod", podName,
		"-o", "jsonpath={.spec.initContainers[*].name}"))
	if err != nil {
		return fmt.Errorf("cli: could not list the verify pod's containers: %w", err)
	}
	streamed := false
	fields := strings.FieldsSeq(containers)
	for name := range fields {
		if !strings.HasPrefix(name, "check-") {
			continue
		}
		args := append(append([]string{}, nsArgs...), "logs", podName, "-c", name)
		if k.tail > 0 {
			args = append(args, "--tail", fmt.Sprintf("%d", k.tail))
		}
		cmd := exec.CommandContext(ctx, "kubectl", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		_ = cmd.Run() // a missing/empty check container is not an error
		streamed = true
	}
	if !streamed {
		// No check-N containers (the verify pod has not run its checks yet); fall
		// back to the default container so the caller gets something.
		args := logArgs(k.ns, loop, compVerify, k.follow, k.tail)
		cmd := exec.CommandContext(ctx, "kubectl", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	return nil
}

// logArgs builds the `kubectl logs` argument list for a component. It is a
// pure function so the argument building is unit-testable (the streamer itself
// shells out to kubectl, which the tests stub). It returns nil for an unknown
// component.
//
// The selector keys mirror the controller's Job/pod labels (internal/
// controller/loop_verify_job.go verifyForLabel, loop_deliver_job.go
// deliverForLabel); a spec in the controller package asserts they are equal.
func logArgs(ns, loop, component string, follow bool, tail int) []string {
	var args []string
	args = append(args, kubeFlagArgs(ns)...)
	args = append(args, "logs")
	if follow {
		args = append(args, "-f")
	}
	if tail > 0 {
		args = append(args, "--tail", fmt.Sprintf("%d", tail))
	}
	switch component {
	case compAgent:
		// The agent runs as the "agent" container in the sandbox pod.
		args = append(args, "pod/"+loop+"-sandbox", "-c", compAgent)
	case compProxy:
		// The egress proxy is a standalone pod (<loop>-proxy), not a container
		// in the sandbox pod.
		args = append(args, "pod/"+loop+"-proxy")
	case compVerify:
		// The verify Job stamps coxswain.io/verify-for on its pod (one -l key).
		args = append(args, "-l", verifyForLabel+"="+loop)
	case compDeliver:
		// The deliver Job stamps coxswain.io/deliver-for on its pod.
		args = append(args, "-l", deliverForLabel+"="+loop)
	default:
		return nil
	}
	return args
}

// Component selectors for `cox logs <loop> --component` (A3: agent, proxy,
// verify, deliver).
const (
	compAgent   = "agent"
	compProxy   = "proxy"
	compVerify  = "verify"
	compDeliver = "deliver"
)

// verifyForLabel / deliverForLabel mirror the controller's verify/deliver Job
// pod-selector labels (internal/controller/loop_verify_job.go verifyForLabel,
// loop_deliver_job.go deliverForLabel). A spec in the controller package asserts
// the two are equal, so a key rename in the controller is caught.
const (
	verifyForLabel  = "coxswain.io/verify-for"
	deliverForLabel = "coxswain.io/deliver-for"
)

// kubectlOutput runs a kubectl subcommand and returns its stdout (for reading
// pod names / container lists; the logs stream itself uses exec directly).
func kubectlOutput(ctx context.Context, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), fmt.Errorf("%s", strings.TrimSpace(string(ee.Stderr)))
		}
		return string(out), err
	}
	return string(out), nil
}

// VerifyForLabel / DeliverForLabel are the exported forms of the mirrored
// label keys, so a cross-package spec in the controller package can assert
// they equal the controller's unexported constants.
const (
	VerifyForLabel  = verifyForLabel
	DeliverForLabel = deliverForLabel
)
