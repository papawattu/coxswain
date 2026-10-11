package cli

import (
	"context"
	"io"

	"github.com/spf13/cobra"
)

// newDescribeCommand builds `cox describe <loop>`: a human-readable summary of
// phase, iteration, conditions, budget, and delivery (A3).
func newDescribeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "describe",
		Short: "Show a Loop's phase, iteration, conditions, budget, and delivery",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return describeLoop(cmd.Context(), currentDeps, namespaceOf(), args[0], cmd.OutOrStdout())
		},
	}
	return cmd
}

// namespaceOf resolves the -n flag (or "default").
func namespaceOf() string {
	if Namespace != "" {
		return Namespace
	}
	return "default"
}

// describeLoop renders a Loop's summary (the describe command's core action).
func describeLoop(ctx context.Context, dc Deps, ns, name string, out io.Writer) error {
	u, err := getLoop(ctx, dc, name)
	if err != nil {
		return err
	}
	w := newWriter(out)

	phase, _ := getNestedString(u, "status", "phase")
	iter := getNestedInt(u, "status", "iteration")
	suspend, _ := getNestedBool(u, "spec", "suspend")

	w.printf("Name:         %s\n", u.GetName())
	w.printf("Namespace:    %s\n", ns)
	w.printf("Phase:        %s\n", phaseOrDash(phase))
	w.printf("Iteration:    %d\n", iter)
	if suspend {
		w.printf("Suspended:    true\n")
	}
	if goal, ok := getNestedString(u, "spec", "goal"); ok {
		w.printf("Goal:         %s\n", goal)
	}

	// Conditions (status.conditions[]).
	conds, _ := getNestedSlice(u, "status", "conditions")
	w.printf("\nConditions:\n")
	if len(conds) == 0 {
		w.printf("  <none>\n")
	}
	for _, c := range conds {
		cm, _ := c.(map[string]any)
		if cm == nil {
			continue
		}
		t, _ := cm["type"].(string)
		s, _ := cm["status"].(string)
		reason, _ := cm["reason"].(string)
		msg, _ := cm["message"].(string)
		w.printf("  %-12s %s  reason=%s\n", t, s, reason)
		if msg != "" {
			for _, line := range splitLines(msg) {
				w.printf("      %s\n", line)
			}
		}
	}

	// Budget (status.budget).
	if bmap, ok := getNestedMap(u, "status", "budget"); ok {
		w.printf("\nBudget:\n")
		if t, _ := bmap["tokens"].(float64); t > 0 {
			w.printf("  tokens:     %.0f\n", t)
		}
		if ex, _ := bmap["exceeded"].(bool); ex {
			reason, _ := bmap["exceededReason"].(string)
			w.printf("  exceeded:   %s\n", reason)
		}
	}

	// Delivery (status.delivery).
	if dmap, ok := getNestedMap(u, "status", "delivery"); ok {
		w.printf("\nDelivery:\n")
		if branch, _ := dmap["branch"].(string); branch != "" {
			w.printf("  branch:     %s\n", branch)
		}
		if pr, _ := dmap["prNumber"].(float64); pr > 0 {
			w.printf("  pr:         #%v\n", int64(pr))
		}
		if url, _ := dmap["prUrl"].(string); url != "" {
			w.printf("  url:        %s\n", url)
		}
	}
	return w.err()
}

func phaseOrDash(p string) string {
	if p == "" {
		return "-"
	}
	return p
}
