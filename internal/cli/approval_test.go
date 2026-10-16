package cli

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestApproveLoop_SetsPlanApprovedCondition verifies that approveLoop reads the
// plan from the workspace, computes its SHA-256, and sets the PlanApproved
// condition (True, message "Approved: <hash>") on the Loop's status.
func TestApproveLoop_SetsPlanApprovedCondition(t *testing.T) {
	const plan = "step one\nstep two\n"
	const loopName = "approve-me"

	f := newFakeLoopClient()
	f.setLoop(newFakeLoop(loopName, map[string]any{}))
	dc := Deps{
		Loops:     f,
		Workspace: &fakeWorkspace{plan: plan},
	}
	var out bytes.Buffer
	if err := approveLoop(context.Background(), dc, "ns", loopName, &out); err != nil {
		t.Fatalf("approveLoop: %v", err)
	}
	// The PlanApproved condition must be present (True, the hash in the message).
	// The command does Get then Update: the Update carries the whole object with
	// the condition added to status.conditions.
	if len(f.updates) == 0 {
		t.Fatal("no Update calls recorded (the condition was not written)")
	}
	u, ok := f.updates[len(f.updates)-1].(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("last Update was not an *unstructured.Unstructured: %T", f.updates[len(f.updates)-1])
	}
	conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if !found {
		t.Fatal("status.conditions not found after approve")
	}
	var foundCond bool
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cm["type"] == "PlanApproved" {
			foundCond = true
			if cm["status"] != "True" {
				t.Errorf("PlanApproved.status = %v, want True", cm["status"])
			}
			wantHash := planSHA256([]byte(plan))
			msg, _ := cm["message"].(string)
			if !strings.Contains(msg, wantHash) {
				t.Errorf("PlanApproved.message = %q, want to contain hash %q", msg, wantHash)
			}
		}
	}
	if !foundCond {
		t.Error("PlanApproved condition not found after approve")
	}
	if !strings.Contains(out.String(), "Approved the plan") {
		t.Errorf("output = %q, want to contain 'Approved the plan'", out.String())
	}
}

// TestApproveLoop_ReplaceOnReapprove verifies that a re-approval (a plan change
// after a prior approval) REPLACES the prior PlanApproved condition in place
// (no duplicate condition is appended).
func TestApproveLoop_ReplaceOnReapprove(t *testing.T) {
	const loopName = "reapprove"
	// A prior approval (the operator or a prior cox approve set the condition).
	priorCond := map[string]any{
		"type":    "PlanApproved",
		"status":  "True",
		"reason":  "Approved",
		"message": "Approved: deadbeef",
	}
	f := newFakeLoopClient()
	f.setLoop(newFakeLoop(loopName, map[string]any{
		"conditions": []any{priorCond},
	}))
	dc := Deps{
		Loops:     f,
		Workspace: &fakeWorkspace{plan: "a different plan\n"},
	}
	var out bytes.Buffer
	if err := approveLoop(context.Background(), dc, "ns", loopName, &out); err != nil {
		t.Fatalf("approveLoop (re-approve): %v", err)
	}
	if len(f.updates) == 0 {
		t.Fatal("no Update calls recorded")
	}
	u, ok := f.updates[len(f.updates)-1].(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("last Update was not an *unstructured.Unstructured: %T", f.updates[len(f.updates)-1])
	}
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	count := 0
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cm["type"] == "PlanApproved" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("PlanApproved condition count = %d, want 1 (the re-approval replaced the prior condition)", count)
	}
}

// TestRejectLoop_SetsPlanRejectedCondition verifies that rejectLoop sets the
// PlanRejected condition (True, message "Rejected: <reason>") on the Loop's
// status.
func TestRejectLoop_SetsPlanRejectedCondition(t *testing.T) {
	const loopName = "reject-me"
	f := newFakeLoopClient()
	f.setLoop(newFakeLoop(loopName, map[string]any{}))
	dc := Deps{
		Loops: f,
	}
	var out bytes.Buffer
	if err := rejectLoop(context.Background(), dc, loopName, "the plan misses the edge case", &out); err != nil {
		t.Fatalf("rejectLoop: %v", err)
	}
	if len(f.updates) == 0 {
		t.Fatal("no Update calls recorded")
	}
	u, ok := f.updates[len(f.updates)-1].(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("last Update was not an *unstructured.Unstructured: %T", f.updates[len(f.updates)-1])
	}
	conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if !found {
		t.Fatal("status.conditions not found after reject")
	}
	var foundCond bool
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cm["type"] == "PlanRejected" {
			foundCond = true
			if cm["status"] != "True" {
				t.Errorf("PlanRejected.status = %v, want True", cm["status"])
			}
			msg, _ := cm["message"].(string)
			if !strings.Contains(msg, "the plan misses the edge case") {
				t.Errorf("PlanRejected.message = %q, want to contain the reason", msg)
			}
		}
	}
	if !foundCond {
		t.Error("PlanRejected condition not found after reject")
	}
	if !strings.Contains(out.String(), "Rejected the plan") {
		t.Errorf("output = %q, want to contain 'Rejected the plan'", out.String())
	}
}

// TestDiffLoop_NoPriorApproval verifies that diffLoop shows the current plan
// (with a "no prior approval" note) when the PlanApproved condition is absent.
func TestDiffLoop_NoPriorApproval(t *testing.T) {
	const loopName = "diff-noprior"
	const plan = "step one\n"
	f := newFakeLoopClient()
	f.setLoop(newFakeLoop(loopName, map[string]any{}))
	dc := Deps{
		Loops:     f,
		Workspace: &fakeWorkspace{plan: plan},
	}
	var out bytes.Buffer
	if err := diffLoop(context.Background(), dc, "ns", loopName, &out); err != nil {
		t.Fatalf("diffLoop (no prior approval): %v", err)
	}
	if !strings.Contains(out.String(), "No prior approval") {
		t.Errorf("output = %q, want to contain 'No prior approval'", out.String())
	}
	if !strings.Contains(out.String(), plan) {
		t.Errorf("output = %q, want to contain the plan", out.String())
	}
}

// TestDiffLoop_PlanUnchanged verifies that diffLoop shows a "no diff" message
// when the current plan's hash matches the approved hash.
func TestDiffLoop_PlanUnchanged(t *testing.T) {
	const loopName = "diff-same"
	const plan = "step one\n"
	hash := planSHA256([]byte(plan))
	approvedCond := map[string]any{
		"type":    "PlanApproved",
		"status":  "True",
		"reason":  "Approved",
		"message": "Approved: " + hash,
	}
	f := newFakeLoopClient()
	f.setLoop(newFakeLoop(loopName, map[string]any{
		"conditions": []any{approvedCond},
	}))
	dc := Deps{
		Loops:     f,
		Workspace: &fakeWorkspace{plan: plan},
	}
	var out bytes.Buffer
	if err := diffLoop(context.Background(), dc, "ns", loopName, &out); err != nil {
		t.Fatalf("diffLoop (unchanged plan): %v", err)
	}
	if !strings.Contains(out.String(), "No diff") {
		t.Errorf("output = %q, want to contain 'No diff'", out.String())
	}
}

// TestDiffLoop_PlanChanged verifies that diffLoop shows a "plan changed"
// message (with both hashes) when the current plan's hash differs from the
// approved hash.
func TestDiffLoop_PlanChanged(t *testing.T) {
	const loopName = "diff-changed"
	const approvedPlan = "step one\n"
	const currentPlan = "step one\nstep two\n"
	approvedHash := planSHA256([]byte(approvedPlan))
	approvedCond := map[string]any{
		"type":    "PlanApproved",
		"status":  "True",
		"reason":  "Approved",
		"message": "Approved: " + approvedHash,
	}
	f := newFakeLoopClient()
	f.setLoop(newFakeLoop(loopName, map[string]any{
		"conditions": []any{approvedCond},
	}))
	dc := Deps{
		Loops:     f,
		Workspace: &fakeWorkspace{plan: currentPlan},
	}
	var out bytes.Buffer
	if err := diffLoop(context.Background(), dc, "ns", loopName, &out); err != nil {
		t.Fatalf("diffLoop (changed plan): %v", err)
	}
	if !strings.Contains(out.String(), "Plan changed") {
		t.Errorf("output = %q, want to contain 'Plan changed'", out.String())
	}
	if !strings.Contains(out.String(), currentPlan) {
		t.Errorf("output = %q, want to contain the current plan", out.String())
	}
}

// fakeWorkspace is a stub WorkspaceReader for the approval command tests.
type fakeWorkspace struct {
	plan   string
	err    error // injected read error (nil = success)
	failed bool  // the peek pod ended Failed (F7: Read must error, never return logs)
}

func (f *fakeWorkspace) Read(ctx context.Context, loopName, path string) (string, error) {
	if f.failed {
		// The F7 regression shape: the peek pod exited non-zero and the reader
		// would have returned the container's log (the cat error string) as
		// content. The fixed reader returns an ERROR instead.
		return "", fmt.Errorf("plan read failed (peek pod ended Failed): cat: can't open '/workspace/.coxswain/PLAN.md': No such file or directory")
	}
	if f.err != nil {
		return "", f.err
	}
	return f.plan, nil
}

// newFakeLoop builds a fake Loop (an *unstructured.Unstructured) with the given
// status.conditions (nil-safe: a nil conditions slice is an empty status).
func newFakeLoop(name string, status map[string]any) *unstructured.Unstructured {
	statusMap := maps.Clone(status)
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "coxswain.wattu.com/v1alpha1",
		"kind":       "Loop",
		"metadata": map[string]any{
			"name":      name,
			"namespace": "ns",
		},
		"status": statusMap,
	}}
}

// --- F7 (dogfood 2 finding): cox approve must never hash a failed plan read ---

// TestApproveLoop_PeekFailed_FailsWithoutCondition verifies that a plan read
// that FAILED (the peek pod ended Failed / exited non-zero) makes approve FAIL
// without writing any condition. The F7 regression: the reader returned the
// container log (the cat error string) as content, and approve hashed that
// error string — the approval bound to "cat: can't open ..." instead of a plan.
// The condition list must be UNCHANGED (no PlanApproved, no write at all).
func TestApproveLoop_PeekFailed_FailsWithoutCondition(t *testing.T) {
	const loopName = "approve-failed-peek"
	f := newFakeLoopClient()
	f.setLoop(newFakeLoop(loopName, map[string]any{}))
	dc := Deps{
		Loops:     f,
		Workspace: &fakeWorkspace{failed: true},
	}
	var out bytes.Buffer
	err := approveLoop(context.Background(), dc, "ns", loopName, &out)
	if err == nil {
		t.Fatal("approveLoop with a failed plan read: want an error, got nil")
	}
	if len(f.updates) != 0 {
		t.Fatalf("approveLoop with a failed plan read wrote %d Update(s); the condition must NOT be written", len(f.updates))
	}
	if !strings.Contains(err.Error(), "read the plan") {
		t.Errorf("error = %q, want it to say the plan read failed", err)
	}
}

// TestApproveLoop_EmptyPlan_Refuses verifies that approve REFUSES when the
// plan is missing or empty (the reader returned "" successfully — e.g. a blank
// PLAN.md). Hashing an empty plan would write an approval that binds to
// sha256(""); the guard refuses before the hash.
func TestApproveLoop_EmptyPlan_Refuses(t *testing.T) {
	const loopName = "approve-empty-plan"
	f := newFakeLoopClient()
	f.setLoop(newFakeLoop(loopName, map[string]any{}))
	for i, plan := range []string{"", "   \n  "} {
		dc := Deps{
			Loops:     f,
			Workspace: &fakeWorkspace{plan: plan},
		}
		var out bytes.Buffer
		err := approveLoop(context.Background(), dc, "ns", loopName, &out)
		if err == nil {
			t.Fatalf("case %d: approveLoop with empty plan %q: want an error, got nil", i, plan)
		}
		if len(f.updates) != 0 {
			t.Fatalf("case %d: approveLoop with empty plan wrote %d Update(s); the condition must NOT be written", i, len(f.updates))
		}
		if !strings.Contains(err.Error(), "no plan to approve") {
			t.Errorf("case %d: error = %q, want 'no plan to approve'", i, err)
		}
	}
}

// TestApproveLoop_HashIsExactPlanBytes verifies the hash is over the plan's
// EXACT bytes (no trim): a trailing newline is part of the hash. This is the
// contract the runner's plan-change check relies on (runner/phase.go
// planChanged hashes the PLAN.md bytes it wrote; the CLI must hash the same
// bytes, or every re-plan reports a spurious plan change).
func TestApproveLoop_HashIsExactPlanBytes(t *testing.T) {
	const loopName = "approve-exact-bytes"
	const plan = "step one\n" // trailing newline is content
	f := newFakeLoopClient()
	f.setLoop(newFakeLoop(loopName, map[string]any{}))
	dc := Deps{
		Loops:     f,
		Workspace: &fakeWorkspace{plan: plan},
	}
	var out bytes.Buffer
	if err := approveLoop(context.Background(), dc, "ns", loopName, &out); err != nil {
		t.Fatalf("approveLoop: %v", err)
	}
	u := f.updates[len(f.updates)-1].(*unstructured.Unstructured)
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	var msg string
	for _, c := range conds {
		cm, _ := c.(map[string]any)
		if cm["type"] == "PlanApproved" {
			msg, _ = cm["message"].(string)
		}
	}
	wantExact := planSHA256([]byte(plan)) // the raw bytes, newline included
	if !strings.Contains(msg, wantExact) {
		t.Errorf("message = %q, want hash of the exact plan bytes %s", msg, wantExact)
	}
	trimmed := planSHA256([]byte(strings.TrimRight(plan, "\n")))
	if trimmed != wantExact && strings.Contains(msg, trimmed) {
		t.Errorf("message = %q, must NOT be the hash of the trimmed plan %s (the newline is content)", msg, trimmed)
	}
}

// --- F7 (dogfood 2 finding): cox plan / cox diff on a missing or empty plan ---

// TestPlanLoop_MissingPlan_Fails verifies that cox plan FAILS (not prints the
// error string as the plan) when the plan read errors — the F7 shape where the
// cat error text used to reach the user's stdout as if it were the plan.
func TestPlanLoop_MissingPlan_Fails(t *testing.T) {
	fw := &failingWorkspaceReader{}
	deps := Deps{Workspace: fw}
	var out bytes.Buffer
	err := planLoop(context.Background(), deps, "ns", "loop1", &out)
	if err == nil {
		t.Fatal("planLoop with a failed read: want an error, got nil")
	}
	if strings.Contains(out.String(), "No such file") {
		t.Errorf("plan output must not carry the cat error string as the plan: %q", out.String())
	}
}

// TestPlanLoop_EmptyPlan_SaysNoPlan verifies that a blank plan (the reader
// returned "") prints a "No plan" line instead of nothing.
func TestPlanLoop_EmptyPlan_SaysNoPlan(t *testing.T) {
	deps := Deps{Workspace: &fakeWorkspace{plan: ""}}
	var out bytes.Buffer
	if err := planLoop(context.Background(), deps, "ns", "loop1", &out); err != nil {
		t.Fatalf("planLoop: %v", err)
	}
	if !strings.Contains(out.String(), "No plan") {
		t.Errorf("output = %q, want a 'No plan' line for an empty plan", out.String())
	}
}

// TestDiffLoop_MissingPlan_Fails verifies that cox diff FAILS (not diffs
// nothing) when the plan read errors.
func TestDiffLoop_MissingPlan_Fails(t *testing.T) {
	f := newFakeLoopClient()
	f.setLoop(newFakeLoop("diff-missing", map[string]any{}))
	dc := Deps{
		Loops:     f,
		Workspace: &fakeWorkspace{failed: true},
	}
	var out bytes.Buffer
	if err := diffLoop(context.Background(), dc, "ns", "diff-missing", &out); err == nil {
		t.Fatal("diffLoop with a failed plan read: want an error, got nil")
	}
}

// TestDiffLoop_EmptyPlan_SaysNoPlan verifies that an empty plan shows "No
// plan" (not "Plan changed" against the approved hash — the diff of nothing).
func TestDiffLoop_EmptyPlan_SaysNoPlan(t *testing.T) {
	approvedHash := planSHA256([]byte("some approved plan\n"))
	approvedCond := map[string]any{
		"type":    "PlanApproved",
		"status":  "True",
		"reason":  "Approved",
		"message": "Approved: " + approvedHash,
	}
	f := newFakeLoopClient()
	f.setLoop(newFakeLoop("diff-empty", map[string]any{
		"conditions": []any{approvedCond},
	}))
	dc := Deps{
		Loops:     f,
		Workspace: &fakeWorkspace{plan: ""},
	}
	var out bytes.Buffer
	if err := diffLoop(context.Background(), dc, "ns", "diff-empty", &out); err != nil {
		t.Fatalf("diffLoop: %v", err)
	}
	if !strings.Contains(out.String(), "No plan") {
		t.Errorf("output = %q, want 'No plan' for an empty plan", out.String())
	}
	if strings.Contains(out.String(), "Plan changed") {
		t.Errorf("output = %q, must NOT say 'Plan changed' for an empty plan (the diff of nothing)", out.String())
	}
}

// failingWorkspaceReader is a WorkspaceReader that always fails (the F7 peek
// pod Failed shape).
type failingWorkspaceReader struct{}

func (f *failingWorkspaceReader) Read(ctx context.Context, loop, path string) (string, error) {
	return "", fmt.Errorf("plan read failed (peek pod %s ended Failed): cat: can't open '/workspace/%s': No such file or directory", "fake-peek", path)
}
