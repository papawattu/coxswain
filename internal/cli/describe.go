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
	// F43/F85: the commit pin the verify evidence is measured against (they are
	// what docs/TROUBLESHOOTING.md tells the operator to compare).
	if bc, ok := getNestedString(u, "status", "baseCommit"); ok && bc != "" {
		w.printf("Base commit:  %s\n", shortSHA(bc))
	}
	if vc, ok := getNestedString(u, "status", "currentVerify", "verifiedCommit"); ok && vc != "" {
		w.printf("Verified:     %s\n", shortSHA(vc))
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
		// F85: status.budget has promptTokens/completionTokens, never a
		// "tokens" field; the label is the sum (kept for a familiar output).
		prompt := numberField(bmap, "promptTokens")
		completion := numberField(bmap, "completionTokens")
		if t := prompt + completion; t > 0 {
			w.printf("  tokens:     %.0f\n", t)
		}
		if r := numberField(bmap, "requests"); r > 0 {
			w.printf("  requests:   %.0f\n", r)
		}
		// F44's durable signal: requests forwarded without a usage object.
		if n := numberField(bmap, "unmeteredRequests"); n > 0 {
			w.printf("  unmetered:  %.0f\n", n)
		}
		if c, _ := bmap["costUsd"].(string); c != "" {
			w.printf("  cost:       $%s\n", c)
		}
		if at, _ := bmap["lastUsageReadAt"].(string); at != "" {
			w.printf("  lastRead:   %s\n", at)
		}
		// F44: consecutive failed usage reads (BudgetReadingStale's counter).
		if n := numberField(bmap, "consecutiveReadFailures"); n > 0 {
			w.printf("  readFails:  %.0f\n", n)
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
		// F85: prNumber is an int64 in the status (so it is an int64 in the
		// unstructured object), and the JSON tag is prURL — a float64/"prUrl"
		// read printed neither line for a real Loop.
		if pr := numberField(dmap, "prNumber"); pr > 0 {
			w.printf("  pr:         #%v\n", int64(pr))
		}
		if url, _ := dmap["prURL"].(string); url != "" {
			w.printf("  url:        %s\n", url)
		}
	}
	return w.err()
}

// numberField reads a numeric field from an unstructured map. An integer status
// field (status.budget counts are int64) is an int64 here, a CRD float a
// float64 — both are numbers to describe.
func numberField(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	}
	return 0
}

// shortSHA abbreviates a 40-hex SHA for the describe header (the full value is
// in -o yaml); a non-SHA value is printed as it is.
func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func phaseOrDash(p string) string {
	if p == "" {
		return "-"
	}
	return p
}
