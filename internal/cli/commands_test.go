package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// --- describe (C2: renders spec+status) ---

// typedLoop converts a typed Loop to the unstructured shape the CLI fetches, so
// a fixture can only carry fields the CRD actually defines (F85: the old
// fixture invented status.budget."tokens", and describe printed a bare heading
// for every real Loop because the field does not exist).
func typedLoop(t *testing.T, mutate func(*coxv1alpha1.Loop)) *unstructured.Unstructured {
	t.Helper()
	l := &coxv1alpha1.Loop{
		TypeMeta:   metav1.TypeMeta{APIVersion: "coxswain.wattu.com/v1alpha1", Kind: "Loop"},
		ObjectMeta: metav1.ObjectMeta{Name: "loop1", Namespace: "samples"},
		Spec:       coxv1alpha1.LoopSpec{Goal: "make the thing work"},
		Status:     coxv1alpha1.LoopStatus{Phase: "Succeeded", Iteration: 2},
	}
	if mutate != nil {
		mutate(l)
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(l)
	if err != nil {
		t.Fatalf("convert typed Loop: %v", err)
	}
	return &unstructured.Unstructured{Object: m}
}

func TestDescribe_WithConditionsBudgetDelivery(t *testing.T) {
	readAt := metav1.NewTime(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC))
	l := typedLoop(t, func(l *coxv1alpha1.Loop) {
		l.Status.Conditions = []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Done", Message: "verified + delivered"},
			{Type: "Delivered", Status: metav1.ConditionTrue, Reason: "Delivered", Message: "PR #1 open"},
		}
		l.Status.BaseCommit = "aaaaaaaaaaaa1111111111222222222233333333"
		l.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: "bbbbbbbbbbbb1111111111222222222233333333"}
		l.Status.Budget = &coxv1alpha1.BudgetStatus{
			PromptTokens:            1000,
			CompletionTokens:        234,
			Requests:                7,
			UnmeteredRequests:       2,
			LastUsageReadAt:         &readAt,
			ConsecutiveReadFailures: 3,
			CostUsd:                 "0.42",
		}
		l.Status.Delivery = &coxv1alpha1.DeliverStatus{Branch: "ai/loop1", PRNumber: 1, PRURL: "http://gitea/pulls/1"}
	})

	fc := newFakeLoopClient()
	fc.setLoop(l)
	deps := Deps{Loops: fc}

	out := runCmd(t, deps, func(ctx context.Context, dc Deps, ns, name string, out *bytes.Buffer) error {
		return describeLoop(ctx, dc, ns, name, out)
	}, "loop1")

	for _, want := range []string{"Name:         loop1", "Phase:        Succeeded", "Iteration:    2",
		"Goal:         make the thing work", "Ready", "Delivered", "PR #1", "branch:     ai/loop1",
		// F85: the sum of promptTokens+completionTokens, the F44 fields, and the
		// F43 commit pin the docs tell the operator to compare.
		"tokens:     1234", "requests:   7", "unmetered:  2", "readFails:  3", "lastRead:   2026-03-04T05:06:07Z",
		"cost:       $0.42", "Base commit:  aaaaaaaaaaaa", "Verified:     bbbbbbbbbbbb",
		// F85 review: prNumber is int64 (was read as float64) and the tag is prURL.
		"pr:         #1", "url:        http://gitea/pulls/1"} {
		if !strings.Contains(out, want) {
			t.Errorf("describe output missing %q\noutput:\n%s", want, out)
		}
	}
}

// F85 (the anti-regression for the original bug): a bogus status.budget key
// cannot make describe print anything — the fixture here CAN invent one (it is
// unstructured), and describe must still not read it.
func TestDescribe_BogusBudgetKeyPrintsNoTokens(t *testing.T) {
	l := makeLoop("Succeeded", 2)
	unstructuredSet(l, "budget", map[string]any{"tokens": 1234.0})
	fc := newFakeLoopClient()
	fc.setLoop(l)
	deps := Deps{Loops: fc}
	out := runCmd(t, deps, func(ctx context.Context, dc Deps, ns, name string, out *bytes.Buffer) error {
		return describeLoop(ctx, dc, ns, name, out)
	}, "loop1")
	if strings.Contains(out, "tokens:") {
		t.Errorf("describe must not read a field the schema does not define (F85)\noutput:\n%s", out)
	}
	if !strings.Contains(out, "Budget:") {
		t.Errorf("the Budget heading must still render\noutput:\n%s", out)
	}
}

func TestDescribe_NotFound(t *testing.T) {
	fc := newFakeLoopClient() // no loop
	deps := Deps{Loops: fc}
	out := &bytes.Buffer{}
	err := describeLoop(context.Background(), deps, "samples", "missing", out)
	if err == nil {
		t.Fatal("expected an error for a missing Loop")
	}
}

// --- history (C2: renders the audit trail) ---

func TestHistory_WithStallHistory(t *testing.T) {
	l := makeLoop("Implementing", 3)
	unstructuredSet(l, "stallHistory", []any{
		map[string]any{"iteration": 1.0, "check": "go.test", "at": "2026-01-01T00:00:00Z", "jobName": "loop1-verify-1"},
		map[string]any{"iteration": 2.0, "check": "go.test", "at": "2026-01-01T00:01:00Z", "jobName": "loop1-verify-2"},
	})
	fc := newFakeLoopClient()
	fc.setLoop(l)
	deps := Deps{Loops: fc}

	out := runCmd(t, deps, func(ctx context.Context, dc Deps, ns, name string, out *bytes.Buffer) error {
		return historyLoop(ctx, dc, ns, name, out)
	}, "loop1")

	for _, want := range []string{"Phase:      Implementing", "Iteration:  3", "go.test", "iteration=1", "iteration=2"} {
		if !strings.Contains(out, want) {
			t.Errorf("history output missing %q\noutput:\n%s", want, out)
		}
	}
}

// --- logs (asserts the component is validated; the Stream is stubbed) ---

type fakeLogStreamer struct{ calledComponent string }

func (f *fakeLogStreamer) Stream(ctx context.Context, loop, component string) error {
	f.calledComponent = component
	return nil
}

func TestLogs_ValidComponent(t *testing.T) {
	fs := &fakeLogStreamer{}
	deps := Deps{Logs: fs}
	out := runCmd(t, deps, func(ctx context.Context, dc Deps, ns, name string, out *bytes.Buffer) error {
		return logsLoop(ctx, dc, ns, name, "proxy", false, 0, out)
	}, "loop1")
	if fs.calledComponent != "proxy" {
		t.Errorf("expected the streamer to be called with proxy, got %q", fs.calledComponent)
	}
	_ = out
}

func TestLogs_UnknownComponent(t *testing.T) {
	deps := Deps{Logs: &fakeLogStreamer{}}
	out := &bytes.Buffer{}
	err := logsLoop(context.Background(), deps, "samples", "loop1", "bogus", false, 0, out)
	if err == nil {
		t.Fatal("expected an error for an unknown component")
	}
}

// --- plan (asserts the reader is called with the right path; stubbed) ---

type fakeWorkspaceReader struct{ readPath string }

func (f *fakeWorkspaceReader) Read(ctx context.Context, loop, path string) (string, error) {
	f.readPath = path
	return "# the plan\nstep one", nil
}

func TestPlan_ReadsPlanPath(t *testing.T) {
	fw := &fakeWorkspaceReader{}
	deps := Deps{Workspace: fw}
	out := runCmd(t, deps, func(ctx context.Context, dc Deps, ns, name string, out *bytes.Buffer) error {
		return planLoop(ctx, dc, ns, name, out)
	}, "loop1")
	if fw.readPath != planPath {
		t.Errorf("expected the reader to be called with %s, got %q", planPath, fw.readPath)
	}
	if !strings.Contains(out, "# the plan") {
		t.Errorf("plan output should carry the file contents, got %q", out)
	}
}

// --- pause / resume (C4: patches spec.suspend; resume adds the annotation for a Stall pause) ---

func TestPause_SetsSuspendTrue(t *testing.T) {
	l := makeLoop("Implementing", 0)
	fc := newFakeLoopClient()
	fc.setLoop(l)
	deps := Deps{Loops: fc}

	out := runCmd(t, deps, func(ctx context.Context, dc Deps, ns, name string, out *bytes.Buffer) error {
		return suspendLoop(ctx, dc, ns, name, true, out)
	}, "loop1")

	p := fc.lastPatch()
	if p == nil {
		t.Fatal("expected a Patch call for pause")
	}
	var patch map[string]any
	if err := json.Unmarshal(p.patch, &patch); err != nil {
		t.Fatalf("bad patch JSON %q: %v", p.patch, err)
	}
	spec, _ := patch["spec"].(map[string]any)
	if spec["suspend"] != true {
		t.Errorf("pause patch should set spec.suspend=true, got %v", patch)
	}
	if !strings.Contains(out, "paused loop1") {
		t.Errorf("pause output: %q", out)
	}
}

func TestResume_SuspendPause_ClearsSuspendOnly(t *testing.T) {
	l := makeLoop("Paused", 0)
	// A Suspend pause: pausedReason empty (spec.suspend was the trigger).
	fc := newFakeLoopClient()
	fc.setLoop(l)
	deps := Deps{Loops: fc}

	out := runCmd(t, deps, func(ctx context.Context, dc Deps, ns, name string, out *bytes.Buffer) error {
		return resumeLoop(ctx, dc, ns, name, out)
	}, "loop1")

	// Exactly one patch (spec.suspend=false); no annotation patch.
	if n := len(fc.patches); n != 1 {
		t.Fatalf("expected 1 patch for a Suspend-pause resume, got %d", n)
	}
	var patch map[string]any
	if err := json.Unmarshal(fc.patches[0].patch, &patch); err != nil {
		t.Fatalf("bad patch JSON: %v", err)
	}
	spec, _ := patch["spec"].(map[string]any)
	if spec["suspend"] != false {
		t.Errorf("resume should clear spec.suspend, got %v", patch)
	}
	if strings.Contains(out, resumeAnnotation) {
		t.Errorf("a Suspend pause must NOT set the %s annotation, got %q", resumeAnnotation, out)
	}
}

func TestResume_StallPause_AddsAnnotation(t *testing.T) {
	l := makeLoop("Paused", 0)
	unstructuredSet(l, "pausedReason", "Stall")
	fc := newFakeLoopClient()
	fc.setLoop(l)
	deps := Deps{Loops: fc}

	out := runCmd(t, deps, func(ctx context.Context, dc Deps, ns, name string, out *bytes.Buffer) error {
		return resumeLoop(ctx, dc, ns, name, out)
	}, "loop1")

	// Two patches: spec.suspend=false, then the coxswain.io/resume annotation.
	if n := len(fc.patches); n != 2 {
		t.Fatalf("expected 2 patches for a Stall-pause resume, got %d", n)
	}
	var annPatch map[string]any
	if err := json.Unmarshal(fc.patches[1].patch, &annPatch); err != nil {
		t.Fatalf("bad annotation patch JSON: %v", err)
	}
	meta, _ := annPatch["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	if v, ok := ann[resumeAnnotation].(string); !ok || v != "true" {
		t.Errorf("resume of a Stall pause should set the %s=true annotation, got %v", resumeAnnotation, annPatch)
	}
	if !strings.Contains(out, resumeAnnotation) {
		t.Errorf("resume output should mention the annotation, got %q", out)
	}
}

// --- redeliver (asserts the I52 annotation patch) ---

func TestRedeliver_SetsAnnotation(t *testing.T) {
	l := makeLoop("Succeeded", 1)
	unstructuredSet(l, "delivery", map[string]any{"branch": "ai/loop1", "prNumber": 1.0})
	fc := newFakeLoopClient()
	fc.setLoop(l)
	deps := Deps{Loops: fc}

	out := runCmd(t, deps, func(ctx context.Context, dc Deps, ns, name string, out *bytes.Buffer) error {
		return redeliverLoop(ctx, dc, ns, name, out)
	}, "loop1")

	p := fc.lastPatch()
	if p == nil {
		t.Fatal("expected a Patch call for redeliver")
	}
	var patch map[string]any
	if err := json.Unmarshal(p.patch, &patch); err != nil {
		t.Fatalf("bad patch JSON: %v", err)
	}
	meta, _ := patch["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	if _, ok := ann[redeliverAnnotation]; !ok {
		t.Errorf("redeliver should set the %s annotation, got %v", redeliverAnnotation, patch)
	}
	if !strings.Contains(out, redeliverAnnotation) {
		t.Errorf("redeliver output should mention the annotation, got %q", out)
	}
}

// TestRedeliver_FailedDeliveryMessage (I82): a Loop whose delivery failed has
// no status.delivery map, but the Delivered condition carries status=False
// reason=DeliveryFailed. The redeliver message must print the condition's
// reason, not 'status.delivery is nil'.
func TestRedeliver_FailedDeliveryMessage(t *testing.T) {
	l := makeLoop("Succeeded", 1)
	// No status.delivery (the map is only set on a successful delivery).
	unstructuredSet(l, "conditions", []any{
		map[string]any{"type": "Delivered", "status": "False", "reason": "DeliveryFailed", "message": "deliver Job failed"},
	})
	fc := newFakeLoopClient()
	fc.setLoop(l)
	deps := Deps{Loops: fc}

	out := runCmd(t, deps, func(ctx context.Context, dc Deps, ns, name string, out *bytes.Buffer) error {
		return redeliverLoop(ctx, dc, ns, name, out)
	}, "loop1")

	if strings.Contains(out, "status.delivery is nil") {
		t.Errorf("redeliver output must not say 'status.delivery is nil' for a failed delivery, got %q", out)
	}
	if !strings.Contains(out, "DeliveryFailed") {
		t.Errorf("redeliver output should mention the DeliveryFailed reason, got %q", out)
	}
}
