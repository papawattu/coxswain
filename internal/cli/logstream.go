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
	args := kubeFlagArgs(k.ns)
	args = append(args, "logs")
	if k.follow {
		args = append(args, "-f")
	}
	if k.tail > 0 {
		args = append(args, "--tail", fmt.Sprintf("%d", k.tail))
	}

	switch component {
	case compAgent, "proxy":
		args = append(args, "pod/"+loop+"-sandbox", "-c", component)
	case "verify":
		args = append(args, "-l", "app.kubernetes.io/loop="+loop, "component=verify")
	case "deliver":
		args = append(args, "-l", "app.kubernetes.io/loop="+loop, "component=deliver")
	default:
		return fmt.Errorf("cli: unknown component %q", component)
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
