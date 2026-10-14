package cli

import (
	"context"
	"io"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// redeliverAnnotation is the operator's supported re-delivery trigger (I52,
// internal/controller/loop_deliver_job.go const redeliverAnnotation): key
// present = any value, even empty.
const redeliverAnnotation = "coxswain.io/redeliver"

// newRedeliverCommand builds `cox redeliver <loop>`: set the I52 annotation to
// trigger a re-delivery of a failed delivery (A3).
func newRedeliverCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "redeliver <loop>",
		Short: "Re-trigger delivery for a failed Loop (set the I52 coxswain.io/redeliver annotation)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return redeliverLoop(cmd.Context(), currentDeps, namespaceOf(), args[0], cmd.OutOrStdout())
		},
	}
}

// redeliverLoop sets the I52 annotation (the redeliver command's core action).
func redeliverLoop(ctx context.Context, dc Deps, _, name string, out io.Writer) error {
	// Read the current delivery state so the message is accurate.
	u, err := getLoop(ctx, dc, name)
	if err != nil {
		return err
	}
	deliveredMsg := deliveredConditionMessage(u)
	if err := setAnnotation(ctx, dc, name, redeliverAnnotation, "true"); err != nil {
		return err
	}
	newWriter(out).printf("redeliver requested for %s (%s set; %s)", name, redeliverAnnotation, deliveredMsg)
	return nil
}

// deliveredConditionMessage returns a human-readable summary of the Loop's
// Delivered condition (I82: the old message 'status.delivery is nil' read as
// 'never delivered' for a failed delivery). It returns 'a fresh deliver Job
// will be created for the pinned verifiedCommit' when status.delivery exists
// (a successful delivery), or the condition's status and reason otherwise.
func deliveredConditionMessage(u *unstructured.Unstructured) string {
	if _, ok := getNestedMap(u, "status", "delivery"); ok {
		return "a fresh deliver Job will be created for the pinned verifiedCommit"
	}
	// Check the Delivered condition for a more specific message.
	conds, _ := getNestedSlice(u, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cm["type"] != "Delivered" {
			continue
		}
		status, _ := cm["status"].(string)
		reason, _ := cm["reason"].(string)
		if status == conditionStatusTrue {
			return "a fresh deliver Job will be created for the pinned verifiedCommit"
		}
		if reason != "" {
			return "previous delivery failed: " + reason
		}
		return "previous delivery failed: " + status
	}
	return "status.delivery is nil; the operator will re-run delivery if the verifiedCommit is pinned"
}
