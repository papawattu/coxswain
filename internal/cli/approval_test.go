package cli

import (
	"bytes"
	"context"
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
	if err := rejectLoop(context.Background(), dc, "ns", loopName, "the plan misses the edge case", &out); err != nil {
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
	plan string
}

func (f *fakeWorkspace) Read(ctx context.Context, loopName, path string) (string, error) {
	return f.plan, nil
}

// newFakeLoop builds a fake Loop (an *unstructured.Unstructured) with the given
// status.conditions (nil-safe: a nil conditions slice is an empty status).
func newFakeLoop(name string, status map[string]any) *unstructured.Unstructured {
	statusMap := map[string]any{}
	for k, v := range status {
		statusMap[k] = v
	}
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
