package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	// F90: docs/OPERATOR.md promises the desired phase too, and it is half of
	// the run key (sandboxRunKey) that TROUBLESHOOTING tells the operator to
	// read — without it a paused Loop cannot be told apart from one the budget
	// paused (desired still Implementing). Omitted when unset, like the other
	// optional status lines.
	if dp, ok := getNestedString(u, "status", "desiredPhase"); ok && dp != "" {
		w.printf("DesiredPhase: %s\n", dp)
	}
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
	// P3-6: the type column is sized from the conditions printed, not a 12-char
	// constant — every condition added since F42 overflowed it
	// (CheckpointsAvailable is 20), so the status column stopped lining up in
	// the output operators paste into issues.
	width := len("<none>")
	statusWidth := len("False")
	for _, c := range conds {
		if cm, _ := c.(map[string]any); cm != nil {
			if t, _ := cm["type"].(string); len(t) > width {
				width = len(t)
			}
			if s, _ := cm[conditionStatusField].(string); len(s) > statusWidth {
				statusWidth = len(s)
			}
		}
	}
	for _, c := range conds {
		cm, _ := c.(map[string]any)
		if cm == nil {
			continue
		}
		t, _ := cm["type"].(string)
		s, _ := cm[conditionStatusField].(string)
		reason, _ := cm["reason"].(string)
		msg, _ := cm["message"].(string)
		w.printf("  %-*s %-*s reason=%s\n", width, t, statusWidth, s, reason)
		if msg != "" {
			for _, line := range splitLines(msg) {
				w.printf("      %s\n", line)
			}
		}
	}

	// Budget (status.budget).
	if bmap, ok := getNestedMap(u, "status", "budget"); ok {
		lines := budgetLines(u, bmap)
		// The heading is gated on at least one printed line (P3-1): a heading
		// with nothing under it was the reported F85 defect. `active:` prints
		// unconditionally, so this is a guard, not the fix.
		if len(lines) > 0 {
			w.printf("\nBudget:\n")
			for _, line := range lines {
				w.printf("  %s\n", line)
			}
		}
	}

	// Delivery (status.delivery).
	if dmap, ok := getNestedMap(u, "status", "delivery"); ok {
		lines := deliveryLines(dmap)
		// Same gate as the Budget block: status.delivery is allocated before
		// its branch/PR are known, so it can be present with nothing to print.
		if len(lines) > 0 {
			w.printf("\nDelivery:\n")
			for _, line := range lines {
				w.printf("  %s\n", line)
			}
		}
	}
	return w.err()
}

// budgetLines renders the status.budget block, one line per field present.
func budgetLines(u *unstructured.Unstructured, bmap map[string]any) []string {
	var lines []string
	// F88: the wall clock. docs/OPERATOR.md promises it and F85 left it out, so
	// `exceeded: WallClock` said the cap fired and never how much time was
	// spent. Printed UNCONDITIONALLY (0s included): status.budget exists on
	// every non-paused Loop and its only fields are often zero, so a
	// conditional print is what produced the bare heading (R34 I122(a)).
	active := humanSeconds(int64(numberField(bmap, "activeSeconds")))
	if cap, ok := getNestedString(u, "spec", "budget", "maxWallClock"); ok && cap != "" {
		lines = append(lines, fmt.Sprintf("active:     %s of %s cap", active, cap))
	} else {
		lines = append(lines, fmt.Sprintf("active:     %s (no cap)", active))
	}
	// F85: status.budget has promptTokens/completionTokens, never a
	// "tokens" field; the label is the sum (kept for a familiar output).
	prompt := numberField(bmap, "promptTokens")
	completion := numberField(bmap, "completionTokens")
	if t := prompt + completion; t > 0 {
		lines = append(lines, fmt.Sprintf("tokens:     %.0f", t))
	}
	if r := numberField(bmap, "requests"); r > 0 {
		lines = append(lines, fmt.Sprintf("requests:   %.0f", r))
	}
	// F44's durable signal: requests forwarded without a usage object.
	if n := numberField(bmap, "unmeteredRequests"); n > 0 {
		lines = append(lines, fmt.Sprintf("unmetered:  %.0f", n))
	}
	if c, _ := bmap["costUsd"].(string); c != "" {
		lines = append(lines, fmt.Sprintf("cost:       $%s", c))
	}
	if at, _ := bmap["lastUsageReadAt"].(string); at != "" {
		lines = append(lines, fmt.Sprintf("lastRead:   %s", at))
	}
	// F44: consecutive failed usage reads (BudgetReadingStale's counter).
	if n := numberField(bmap, "consecutiveReadFailures"); n > 0 {
		lines = append(lines, fmt.Sprintf("readFails:  %.0f", n))
	}
	if ex, _ := bmap["exceeded"].(bool); ex {
		reason, _ := bmap["exceededReason"].(string)
		lines = append(lines, fmt.Sprintf("exceeded:   %s", reason))
	}
	return lines
}

// deliveryLines renders the status.delivery block, one line per field present.
func deliveryLines(dmap map[string]any) []string {
	var lines []string
	if branch, _ := dmap["branch"].(string); branch != "" {
		lines = append(lines, fmt.Sprintf("branch:     %s", branch))
	}
	// F85: prNumber is an int64 in the status (so it is an int64 in the
	// unstructured object), and the JSON tag is prURL — a float64/"prUrl"
	// read printed neither line for a real Loop.
	if pr := numberField(dmap, "prNumber"); pr > 0 {
		lines = append(lines, fmt.Sprintf("pr:         #%v", int64(pr)))
	}
	if url, _ := dmap["prURL"].(string); url != "" {
		lines = append(lines, fmt.Sprintf("url:        %s", url))
	}
	return lines
}

// humanSeconds renders whole active seconds the way the delivered PR body does
// ("Budget (active time)", controller.humanDurationSeconds: 0s, 1m5s, 1h0m0s),
// without importing the controller package — the CLI imports no internal/
// package and pulling the reconciler in for a 3-line formatter is not worth it.
func humanSeconds(total int64) string {
	return (time.Duration(total) * time.Second).String()
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
