package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
// os.Stdout. The follow flag adds -f.
func (k *kubectlLogStreamer) Stream(ctx context.Context, loop, component string) error {
	args := logArgs(k.ns, loop, component, k.follow, k.tail)
	if len(args) == 0 {
		return fmt.Errorf("cli: unknown component %q", component)
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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

// VerifyForLabel / DeliverForLabel are the exported forms of the mirrored
// label keys, so a cross-package spec in the controller package can assert
// they equal the controller's unexported constants.
const (
	VerifyForLabel  = verifyForLabel
	DeliverForLabel = deliverForLabel
)
