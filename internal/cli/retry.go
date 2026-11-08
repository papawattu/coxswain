package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// retryAnnotation is the operator's one-shot retry trigger (D116): the
// operator's constant must match this key. Set on a Loop that is Failed with
// reason SandboxFailed; the operator restarts the sandbox from the current
// checkpoint.
const retryAnnotation = "coxswain.io/retry"

// newRetryCommand builds `cox retry <loop>`: set the D116 annotation on a
// Failed:SandboxFailed Loop. It refuses every other Failed reason — a
// TamperedVerify retry would be a gate bypass.
func newRetryCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "retry <loop>",
		Short: "Retry a Loop that failed with SandboxFailed (sets the coxswain.io/retry annotation)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return retryLoop(cmd.Context(), currentDeps, namespaceOf(), args[0], cmd.OutOrStdout())
		},
	}
}

// retryLoop sets the D116 annotation on a Failed:SandboxFailed Loop (the
// retry command's core action).
func retryLoop(ctx context.Context, dc Deps, _, name string, out io.Writer) error {
	u, err := getLoop(ctx, dc, name)
	if err != nil {
		return err
	}
	phase, _ := getNestedString(u, "status", "phase")
	if phase != phaseFailed {
		return fmt.Errorf("cox retry: %s is %s, not Failed; retry applies only to Failed:SandboxFailed", name, phase)
	}
	reason := failedConditionReason(u)
	if reason != "SandboxFailed" {
		return fmt.Errorf("cox retry: %s failed with reason %s; retry applies only to SandboxFailed (D116)", name, reason)
	}
	if err := setAnnotation(ctx, dc, name, retryAnnotation, "true"); err != nil {
		return err
	}
	newWriter(out).printf("retry requested for %s (coxswain.io/retry set; the operator restarts the sandbox from the current checkpoint)", name)
	return nil
}

// failedConditionReason returns the reason of the Loop's Failed status
// condition ("Failed" if the condition is absent).
func failedConditionReason(u *unstructured.Unstructured) string {
	conds, _ := getNestedSlice(u, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cm["type"] == "Failed" {
			reason, _ := cm["reason"].(string)
			return reason
		}
	}
	return "Failed"
}
