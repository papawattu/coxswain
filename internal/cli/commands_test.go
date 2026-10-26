package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

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
		// F90: the desired phase half of the run key, which describe now prints.
		l.Status.DesiredPhase = "Implementing"
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
			// F88: the wall clock describe never printed (OPERATOR.md:403).
			ActiveSeconds: 3600,
		}
		// F88: the cap prints as the operator wrote it in the spec.
		l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxWallClock: "2h"}
		l.Status.Delivery = &coxv1alpha1.DeliverStatus{Branch: "ai/loop1", PRNumber: 1, PRURL: "http://gitea/pulls/1"}
	})

	fc := newFakeLoopClient()
	fc.setLoop(l)
	deps := Deps{Loops: fc}

	out := runCmd(t, deps, func(ctx context.Context, dc Deps, ns, name string, out *bytes.Buffer) error {
		return describeLoop(ctx, dc, ns, name, out)
	}, "loop1")

	for _, want := range []string{"Name:         loop1", "Phase:        Succeeded", "Iteration:    2",
		// F90: OPERATOR.md promises describe prints the desired phase; it is
		// the operator's half of the run key (sandboxRunKey), so a paused Loop
		// can be read without guessing why.
		"DesiredPhase: Implementing",
		"Goal:         make the thing work", "Ready", "Delivered", "PR #1", "branch:     ai/loop1",
		// F85: the sum of promptTokens+completionTokens, the F44 fields, and the
		// F43 commit pin the docs tell the operator to compare.
		"tokens:     1234", "requests:   7", "unmetered:  2", "readFails:  3", "lastRead:   2026-03-04T05:06:07Z",
		"cost:       $0.42", "Base commit:  aaaaaaaaaaaa", "Verified:     bbbbbbbbbbbb",
		// F88: the wall clock OPERATOR.md:403 promises (status.budget.activeSeconds)
		// with the spec's cap, rendered like the delivered PR body.
		"active:     1h0m0s of 2h cap",
		// F85 review: prNumber is int64 (was read as float64) and the tag is prURL.
		"pr:         #1", "url:        http://gitea/pulls/1",
		// P3-2: the Delivery heading had no assertion anywhere in the suite.
		"Delivery:"} {
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
	// F88: the heading is not bare even for this fixture — `active:` prints
	// unconditionally, which is what makes the heading gate a guard rather than
	// the fix (the F85 symptom was a heading with nothing under it).
	if !strings.Contains(out, "active:     0s (no cap)") {
		t.Errorf("describe must print the wall clock (F88)\noutput:\n%s", out)
	}
}

// F88: the delivered PR body prints "(no cap)" for a Loop with no
// spec.budget.maxWallClock (loop_deliver_description.go:430); describe prints
// the same tail so the two surfaces cannot drift.
func TestDescribe_NoCapPrintsNoCap(t *testing.T) {
	l := typedLoop(t, func(l *coxv1alpha1.Loop) {
		l.Status.Budget = &coxv1alpha1.BudgetStatus{ActiveSeconds: 90}
	})
	fc := newFakeLoopClient()
	fc.setLoop(l)
	deps := Deps{Loops: fc}
	out := runCmd(t, deps, func(ctx context.Context, dc Deps, ns, name string, out *bytes.Buffer) error {
		return describeLoop(ctx, dc, ns, name, out)
	}, "loop1")
	if !strings.Contains(out, "active:     1m30s (no cap)") {
		t.Errorf("describe output missing the no-cap wall clock\noutput:\n%s", out)
	}
}

// F90 negative control: the DesiredPhase line is gated on the field being set,
// so a Loop that has never been given one prints no empty line. Two shapes:
// the field ABSENT (a CR created by hand, not yet reconciled) and the field
// present but EMPTY (an unstructured object can carry `desiredPhase: ""` even
// though the typed field is omitempty). Paired with the positive assertion in
// TestDescribe_WithConditionsBudgetDelivery, both directions of the one
// decision are pinned: dropping the print fails the positive, dropping the
// `dp != ""` half prints a bare `DesiredPhase: ` and fails here.
func TestDescribe_NoDesiredPhasePrintsNoLine(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setEmpty bool
	}{
		{name: "absent"},
		{name: "present but empty", setEmpty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := typedLoop(t, nil) // the fixture sets no DesiredPhase
			if tc.setEmpty {
				unstructuredSet(l, "desiredPhase", "")
			}
			fc := newFakeLoopClient()
			fc.setLoop(l)
			deps := Deps{Loops: fc}
			out := runCmd(t, deps, func(ctx context.Context, dc Deps, ns, name string, out *bytes.Buffer) error {
				return describeLoop(ctx, dc, ns, name, out)
			}, "loop1")
			if strings.Contains(out, "DesiredPhase:") {
				t.Errorf("an unset/empty status.desiredPhase must print nothing\noutput:\n%s", out)
			}
		})
	}
}

// P3-2: status.delivery is allocated before its branch/PR are known, so the
// block can be present with nothing to print. The heading is gated on at least
// one line (the same shape as the Budget block, and what OPERATOR.md now
// promises: "omitted until there is one").
func TestDescribe_EmptyDeliveryPrintsNoHeading(t *testing.T) {
	l := typedLoop(t, func(l *coxv1alpha1.Loop) {
		l.Status.Delivery = &coxv1alpha1.DeliverStatus{}
	})
	fc := newFakeLoopClient()
	fc.setLoop(l)
	deps := Deps{Loops: fc}
	out := runCmd(t, deps, func(ctx context.Context, dc Deps, ns, name string, out *bytes.Buffer) error {
		return describeLoop(ctx, dc, ns, name, out)
	}, "loop1")
	if strings.Contains(out, "Delivery:") {
		t.Errorf("an empty status.delivery must not print the heading\noutput:\n%s", out)
	}
}

// P3-6: the condition type column is sized from the conditions printed, so the
// status/reason columns line up for every condition the operator adds — the
// 12-char constant broke for SandboxNotStarting (18) and
// CheckpointsAvailable (20).
func TestDescribe_ConditionColumnAligns(t *testing.T) {
	l := typedLoop(t, func(l *coxv1alpha1.Loop) {
		l.Status.Conditions = []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Done"},
			{Type: "CheckpointsAvailable", Status: metav1.ConditionFalse, Reason: "NoStorageClass"},
		}
	})
	fc := newFakeLoopClient()
	fc.setLoop(l)
	deps := Deps{Loops: fc}
	out := runCmd(t, deps, func(ctx context.Context, dc Deps, ns, name string, out *bytes.Buffer) error {
		return describeLoop(ctx, dc, ns, name, out)
	}, "loop1")
	var idx []int
	for line := range strings.SplitSeq(out, "\n") {
		if i := strings.Index(line, "reason="); i >= 0 {
			idx = append(idx, i)
		}
	}
	if len(idx) != 2 {
		t.Fatalf("expected two condition lines with a reason, got %d\noutput:\n%s", len(idx), out)
	}
	if idx[0] != idx[1] {
		t.Errorf("the reason column is misaligned (indices %v)\noutput:\n%s", idx, out)
	}
}

// P3-4: humanSeconds must agree with the delivered PR body's
// controller.humanDurationSeconds for whole seconds — including the two values
// the describe specs do not reach (65, 90061).
func TestHumanSeconds(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0s"},
		{65, "1m5s"},
		{3600, "1h0m0s"},
		{90061, "25h1m1s"},
	} {
		if got := humanSeconds(tc.in); got != tc.want {
			t.Errorf("humanSeconds(%d) = %q, want %q", tc.in, got, tc.want)
		}
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

// F52 C3: the Use strings must name the <loop> argument — cobra prints Use in
// --help and in the usage line of an argument error, and both commands take
// exactly one loop, so a bare "describe" / "history" hides the only argument.
func TestCommands_NameTheLoopArgument(t *testing.T) {
	for _, cmd := range []*cobra.Command{newDescribeCommand(), newHistoryCommand()} {
		if !strings.Contains(cmd.Use, "<loop>") {
			t.Fatalf("%q must name the <loop> argument (F52 C3)", cmd.Use)
		}
	}
}

// F52 C3: describe prints the OBJECT's namespace. With -n unset the client
// resolves the kubeconfig context's namespace (NewDeps), while namespaceOf()
// answers "default" — so the old line named a namespace the Loop does not live
// in. The command seam is fed "default" here and the fixture lives in
// "samples", which is the case the flag path could not distinguish.
func TestDescribe_NamespaceIsTheObjectsNotTheFlags(t *testing.T) {
	fc := newFakeLoopClient()
	fc.setLoop(typedLoop(t, nil)) // ObjectMeta.Namespace = samples
	out := &bytes.Buffer{}
	if err := describeLoop(context.Background(), Deps{Loops: fc}, "default", "loop1", out); err != nil {
		t.Fatalf("describe: %v", err)
	}
	if !strings.Contains(out.String(), "Namespace:    samples") {
		t.Fatalf("describe must print the object's namespace (samples), got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Namespace:    default") {
		t.Fatalf("describe printed the flag-derived namespace instead of the object's:\n%s", out.String())
	}
}
