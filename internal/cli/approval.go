package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/spf13/cobra"
)

// planApprovedConditionType is the PlanApproved condition's type string (the
// goconst for the CLI's approve command — the operator's constant is in a
// different module, so the CLI defines its own).
const planApprovedConditionType = "PlanApproved"

// conditionStatusTrue is the condition's status "True" (the goconst for the
// CLI's approve/reject commands — the operator's constant is in a different
// module, so the CLI defines its own).
const conditionStatusTrue = "True"

// planRejectedConditionType is the PlanRejected condition's type string (the
// goconst for the CLI's reject command — the operator's constant is in a
// different module, so the CLI defines its own).
const planRejectedConditionType = "PlanRejected"

// newApproveCommand builds `cox approve <loop>`: set the PlanApproved condition
// on the Loop (the B3 plan approval gate). The CLI computes the SHA-256 of the
// plan (read from the workspace via the existing plan reader) and sets the
// condition's message to "Approved: <hash>". The operator's approval gate sees
// the condition True (with a valid hash) and advances the Loop to Implementing
// (if it is at AwaitingApproval).
func newApproveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "approve <loop>",
		Short: "Approve the Loop's plan (set the PlanApproved condition with the plan's SHA-256)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return approveLoop(cmd.Context(), currentDeps, namespaceOf(), args[0], cmd.OutOrStdout())
		},
	}
}

// approveLoop is the approve command's core action. It reads the plan from the
// workspace (the existing plan reader), computes its SHA-256, and sets the
// PlanApproved condition (True, message "Approved: <hash>") on the Loop's
// status subresource. The operator's approval gate then advances the Loop.
func approveLoop(ctx context.Context, dc Deps, ns, name string, out io.Writer) error {
	// Read the plan from the workspace (the existing plan reader). If the plan
	// is absent (a fresh Loop that has not reached Planning, or a Loop whose
	// workspace has not been provisioned), the reader returns an error — the
	// user must wait for the plan to be written before approving.
	if dc.Workspace == nil {
		dc.Workspace = &peekWorkspaceReader{ns: ns}
	}
	plan, err := dc.Workspace.Read(ctx, name, planPath)
	if err != nil {
		return fmt.Errorf("read the plan (to compute its hash): %w", err)
	}
	hash := planSHA256([]byte(plan))
	// Build the condition (the user is the sole writer of PlanApproved). The
	// message carries the hash (the operator parses it from the message; the
	// condition's status is True, the reason is Approved).
	cond := map[string]any{
		"type":    planApprovedConditionType,
		"status":  conditionStatusTrue,
		"reason":  "Approved",
		"message": "Approved: " + hash,
	}
	// Patch the status subresource (the condition is on the status, not the
	// main resource). The dynamic client's Patch does not support subresources
	// (a CLI limitation — see the client.go comment), so we read the whole
	// object, add the condition to status.conditions, and Update the whole
	// object. The status subresource is NOT separately addressable via a JSON
	// merge patch on the main resource (a merge patch on the main resource does
	// not write status.conditions; it is a no-op on the status subresource).
	// The Update (a full replace) is the correct path: read the object, add the
	// condition, write it back. The operator's applyApprovalGate then reads the
	// condition on the next reconcile and advances the Loop.
	obj, err := dc.Loops.Get(ctx, name)
	if err != nil {
		return fmt.Errorf("get loop %q: %w", name, err)
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("cli: expected *unstructured.Unstructured, got %T", obj)
	}
	// Ensure status.conditions exists (a fresh Loop may not have the status
	// subresource populated yet — the operator creates it on the first
	// reconcile; the CLI must not assume it exists).
	conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if !found {
		conds = []any{}
	}
	// Replace (or insert) the PlanApproved condition. A single condition of
	// this type is the contract (the user approves one plan at a time; a
	// re-approval after a plan change REPLACES the prior condition). A found
	// marker avoids the "append only when absent" bug: if the loop already has
	// a PlanApproved condition (the user re-approves after a plan change), the
	// condition is REPLACED in place, not appended (a duplicate condition
	// would break the operator's read — the operator takes the first match).
	replaced := false
	for i, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cm["type"] == planApprovedConditionType {
			conds[i] = cond
			replaced = true
			break
		}
	}
	if !replaced {
		conds = append(conds, cond)
	}
	if err := unstructured.SetNestedSlice(u.Object, conds, "status", "conditions"); err != nil {
		return fmt.Errorf("set status.conditions: %w", err)
	}
	if err := dc.Loops.Update(ctx, u); err != nil {
		return fmt.Errorf("update loop %q (set PlanApproved): %w", name, err)
	}
	_, _ = fmt.Fprintf(out, "Approved the plan for loop %q (SHA-256 %s)\n", name, hash)
	return nil
}

// planSHA256 returns the lowercase-hex SHA-256 of a plan's content (the same
// hash the operator's approval gate parses from the PlanApproved condition's
// message, and the same hash the runner's plan-change check compares against
// the approved hash file). The hash is over the plan's EXACT bytes (the bytes
// read from the workspace — the same bytes the runner wrote to PLAN.md, so a
// trailing newline or cap difference would mismatch).
func planSHA256(plan []byte) string {
	h := sha256.Sum256(plan)
	return hex.EncodeToString(h[:])
}

// newRejectCommand builds `cox reject <loop> --reason <reason>`: set the
// PlanRejected condition on the Loop (the B3 plan approval gate). The CLI sets
// the condition's message to "Rejected: <reason>". The operator's approval
// gate sees the condition True and applies the onReject action (Replan:
// re-enter Planning with the reason; Fail: terminal).
func newRejectCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reject <loop>",
		Short: "Reject the Loop's plan (set the PlanRejected condition with a reason)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reason := cmd.Flag("reason").Value.String()
			if reason == "" {
				return fmt.Errorf("--reason is required (the reason the plan was rejected)")
			}
			return rejectLoop(cmd.Context(), currentDeps, args[0], reason, cmd.OutOrStdout())
		},
	}
	cmd.Flags().String("reason", "", "the reason the plan was rejected (required)")
	return cmd
}

// rejectLoop is the reject command's core action. It sets the PlanRejected
// condition (True, message "Rejected: <reason>") on the Loop's status
// subresource. The operator's approval gate then applies the onReject action
// (Replan: re-enter Planning with the reason; Fail: terminal).
func rejectLoop(ctx context.Context, dc Deps, name, reason string, out io.Writer) error {
	// Build the condition (the user is the sole writer of PlanRejected). The
	// message carries the reason (the operator reads it from the message; the
	// condition's status is True, the reason is Rejected).
	cond := map[string]any{
		"type":    planRejectedConditionType,
		"status":  conditionStatusTrue,
		"reason":  "Rejected",
		"message": "Rejected: " + reason,
	}
	// Read the whole object, add the condition to status.conditions, and
	// Update (the same path as approveLoop — the dynamic client's Patch does
	// not support subresources, so the Update is the correct path).
	obj, err := dc.Loops.Get(ctx, name)
	if err != nil {
		return fmt.Errorf("get loop %q: %w", name, err)
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("cli: expected *unstructured.Unstructured, got %T", obj)
	}
	conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if !found {
		conds = []any{}
	}
	// Replace (or insert) the PlanRejected condition (the same single-condition
	// contract as PlanApproved).
	replaced := false
	for i, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cm["type"] == planRejectedConditionType {
			conds[i] = cond
			replaced = true
			break
		}
	}
	if !replaced {
		conds = append(conds, cond)
	}
	if err := unstructured.SetNestedSlice(u.Object, conds, "status", "conditions"); err != nil {
		return fmt.Errorf("set status.conditions: %w", err)
	}
	if err := dc.Loops.Update(ctx, u); err != nil {
		return fmt.Errorf("update loop %q (set PlanRejected): %w", name, err)
	}
	_, _ = fmt.Fprintf(out, "Rejected the plan for loop %q: %s\n", name, reason)
	return nil
}

// newDiffCommand builds `cox diff <loop>`: show the plan diff (the plan the
// user is about to approve vs. the last approved plan). The CLI reads the
// current plan from the workspace and the last approved plan's hash from the
// PlanApproved condition (if present). A diff is shown when the two differ
// (the plan changed since the last approval).
func newDiffCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "diff <loop>",
		Short: "Show the plan diff (the current plan vs. the last approved plan)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return diffLoop(cmd.Context(), currentDeps, namespaceOf(), args[0], cmd.OutOrStdout())
		},
	}
}

// diffLoop is the diff command's core action. It reads the current plan from
// the workspace and the last approved plan's hash from the PlanApproved
// condition (if present). When the two differ (the plan changed since the last
// approval), the command shows the diff (the current plan's content, with a
// note that it differs from the approved hash). When the two match (the plan
// is the same the user last approved), the command shows a "no diff" message.
// When the PlanApproved condition is absent (a fresh Loop, or a plan the user
// has not approved yet), the command shows the current plan (there is no
// prior approval to diff against).
func diffLoop(ctx context.Context, dc Deps, ns, name string, out io.Writer) error {
	// Read the current plan from the workspace (the existing plan reader).
	if dc.Workspace == nil {
		dc.Workspace = &peekWorkspaceReader{ns: ns}
	}
	plan, err := dc.Workspace.Read(ctx, name, planPath)
	if err != nil {
		return fmt.Errorf("read the plan: %w", err)
	}
	// Read the Loop's PlanApproved condition (the last approved plan's hash).
	obj, err := dc.Loops.Get(ctx, name)
	if err != nil {
		return fmt.Errorf("get loop %q: %w", name, err)
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("cli: expected *unstructured.Unstructured, got %T", obj)
	}
	approvedHash := ""
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cm["type"] == planApprovedConditionType && cm["status"] == conditionStatusTrue {
			if msg, ok := cm["message"].(string); ok {
				// The message format is "Approved: <hash>" (the same format
				// approveLoop writes). Parse the hash (a 64-hex string at the
				// end of the message).
				if len(msg) >= 64 {
					approvedHash = msg[len(msg)-64:]
				}
			}
			break
		}
	}
	// Compare the current plan's hash to the approved hash. A match means the
	// plan is the same the user last approved (no diff); a mismatch means the
	// plan changed (show the diff); an absent approved hash means there is no
	// prior approval (show the current plan).
	currentHash := planSHA256([]byte(plan))
	switch {
	case approvedHash == "":
		_, _ = fmt.Fprintf(out, "No prior approval. Current plan (SHA-256 %s):\n\n%s\n", currentHash, plan)
	case currentHash == approvedHash:
		_, _ = fmt.Fprintf(out, "No diff: the plan is the same the user last approved (SHA-256 %s)\n", approvedHash)
	default:
		_, _ = fmt.Fprintf(out, "Plan changed since the last approval (approved SHA-256 %s, current SHA-256 %s):\n\n%s\n", approvedHash, currentHash, plan)
	}
	return nil
}
