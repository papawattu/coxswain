package cli

import (
	"context"
	"io"

	"github.com/spf13/cobra"
)

// newHistoryCommand builds `cox history <loop>`: print the Loop's audit trail.
// The plan (CLI-PLAN.md C2, PLAN-ALPHA.md A3) names `status.history[]`; the
// concrete history array the operator records today is `status.stallHistory[]`
// (the ring of the last 10 verify-failure iterations). This command renders
// that array plus the current phase/iteration so the trail is readable
// regardless of which history fields are populated.
func newHistoryCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "history",
		Short: "Print a Loop's audit trail (phase + the verify-failure history ring)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return historyLoop(cmd.Context(), currentDeps, namespaceOf(), args[0], cmd.OutOrStdout())
		},
	}
}

// historyLoop renders the Loop's history (the history command's core action).
func historyLoop(ctx context.Context, dc Deps, _, name string, out io.Writer) error {
	u, err := getLoop(ctx, dc, name)
	if err != nil {
		return err
	}
	w := newWriter(out)

	phase, _ := getNestedString(u, "status", "phase")
	iter := getNestedInt(u, "status", "iteration")
	w.printf("Loop:       %s\n", u.GetName())
	w.printf("Phase:      %s\n", phaseOrDash(phase))
	w.printf("Iteration:  %d\n", iter)

	// The verify-failure history ring (status.stallHistory[]).
	hist, _ := getNestedSlice(u, "status", "stallHistory")
	w.printf("\nVerify-failure history (stallHistory, last 10):\n")
	if len(hist) == 0 {
		w.printf("  <none>\n")
	}
	for i, e := range hist {
		em, _ := e.(map[string]any)
		if em == nil {
			continue
		}
		it, _ := em["iteration"].(float64)
		check, _ := em["check"].(string)
		at, _ := em["at"].(string)
		w.printf("  %d. iteration=%v check=%s at=%s\n", i+1, int64(it), check, at)
	}
	return w.err()
}
