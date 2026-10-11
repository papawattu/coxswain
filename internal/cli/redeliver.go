package cli

import (
	"context"
	"io"

	"github.com/spf13/cobra"
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
	_, hasDelivery := getNestedMap(u, "status", "delivery")
	if err := setAnnotation(ctx, dc, name, redeliverAnnotation, "true"); err != nil {
		return err
	}
	if hasDelivery {
		newWriter(out).printf("redeliver requested for %s (%s set; a fresh deliver Job will be created for the pinned verifiedCommit)", name, redeliverAnnotation)
	} else {
		newWriter(out).printf("redeliver requested for %s (%s set; note: status.delivery is nil, so the operator will re-run delivery if the verifiedCommit is pinned)", name, redeliverAnnotation)
	}
	return nil
}
