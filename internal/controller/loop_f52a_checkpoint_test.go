package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// F52 A2: applyCheckpointConditions reported changed unconditionally, so the
// caller persisted the status on every reconcile even when the
// CheckpointsAvailable condition said exactly what it said last time. The
// spec pins the two halves: the FIRST call changes (the condition appears) and
// the second does not (nothing differs).
//
// The reconciler is bare: with the CRD reported absent, checkpointSupport
// returns the CRD-missing condition without reading the API at all, so no
// client is needed.
func TestF52ApplyCheckpointConditionsChangedOnlyOnChange(t *testing.T) {
	r := &LoopReconciler{
		snapshotCRDPresent: func(context.Context) bool { return false },
	}
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "f52a-a2", Namespace: "default"},
		Spec:       coxv1alpha1.LoopSpec{Checkpoints: coxv1alpha1.CheckpointPolicyBestEffort},
	}

	changed, terminal := r.applyCheckpointConditions(context.Background(), loop)
	if !changed {
		t.Fatal("the first call must report changed (the condition did not exist before it)")
	}
	if terminal {
		t.Fatal("BestEffort without snapshot support is not terminal (the Loop carries on with the A2 PVC-resume)")
	}
	cond := findConditionIn(loop.Status.Conditions, coxv1alpha1.CheckpointsAvailableCondition)
	if cond == nil {
		t.Fatal("the CheckpointsAvailable condition must be set on the first call")
	}
	if cond.Reason != CheckpointReasonCRDMissing {
		t.Fatalf("the first call must set the CRD-missing reason, got %q", cond.Reason)
	}

	changed, terminal = r.applyCheckpointConditions(context.Background(), loop)
	if changed {
		t.Fatal("a second call with the same support must report changed=false (F52 A2: the status write is what this drives)")
	}
	if terminal {
		t.Fatal("the terminal answer must not flip between two identical calls")
	}
	if got := len(loop.Status.Conditions); got != 1 {
		t.Fatalf("the second call must not add a condition (got %d conditions)", got)
	}
}

// The Required half: terminal is read from the condition as stored (F52 A2),
// and it stays true across two identical calls while changed goes true ->
// false.
func TestF52ApplyCheckpointConditionsTerminalFromStoredCondition(t *testing.T) {
	r := &LoopReconciler{
		snapshotCRDPresent: func(context.Context) bool { return false },
	}
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "f52a-a2-required", Namespace: "default"},
		Spec:       coxv1alpha1.LoopSpec{Checkpoints: coxv1alpha1.CheckpointPolicyRequired},
		Status:     coxv1alpha1.LoopStatus{Phase: coxv1alpha1.LoopPhasePlanning},
	}

	changed, terminal := r.applyCheckpointConditions(context.Background(), loop)
	if !changed || !terminal {
		t.Fatalf("first call: want changed=true terminal=true, got changed=%t terminal=%t", changed, terminal)
	}
	changed, terminal = r.applyCheckpointConditions(context.Background(), loop)
	if changed {
		t.Fatal("second call must report changed=false")
	}
	if !terminal {
		t.Fatal("second call must still report terminal=true (spec.checkpoints=Required with no support)")
	}
}

// findConditionIn returns the condition of the given type (nil when absent).
func findConditionIn(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

// F52 A2 (#302 P2): the TRANSITION case. The first fix compared the stored
// condition before and after the set, but FindStatusCondition returns a
// pointer into the slice SetStatusCondition mutates in place, so the compare
// ran against itself and reported false on every real transition. These two
// cases are what that bug made impossible to see: a stored True replaced by a
// computed False (the support broke), and a stored False replaced by a
// computed True (the support appeared).
func TestF52ApplyCheckpointConditionsReportsFalseToTrue(t *testing.T) {
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "f52a-a2-up", Namespace: "default"},
		Spec:       coxv1alpha1.LoopSpec{Checkpoints: coxv1alpha1.CheckpointPolicyBestEffort},
		Status: coxv1alpha1.LoopStatus{
			Phase: coxv1alpha1.LoopPhasePlanning,
			// The stored state: no support (the CRD was missing last pass).
			Conditions: []metav1.Condition{{
				Type:    coxv1alpha1.CheckpointsAvailableCondition,
				Status:  metav1.ConditionFalse,
				Reason:  CheckpointReasonCRDMissing,
				Message: "the VolumeSnapshot CRD is not installed",
			}},
		},
	}
	r := supportedCheckpointReconciler(t, "hostpath.csi.k8s.io")

	changed, terminal := r.applyCheckpointConditions(context.Background(), loop)
	if !changed {
		t.Fatal("a stored False replaced by a computed True must report changed=true (F52 A2 / #302 P2)")
	}
	if terminal {
		t.Fatal("BestEffort with support is not terminal")
	}
	cond := findConditionIn(loop.Status.Conditions, coxv1alpha1.CheckpointsAvailableCondition)
	if cond.Status != metav1.ConditionTrue || cond.Reason != CheckpointReasonAvailable {
		t.Fatalf("the stored condition must be the computed True/Available, got %s/%s", cond.Status, cond.Reason)
	}
}

func TestF52ApplyCheckpointConditionsReportsTrueToFalse(t *testing.T) {
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "f52a-a2-down", Namespace: "default"},
		Spec:       coxv1alpha1.LoopSpec{Checkpoints: coxv1alpha1.CheckpointPolicyBestEffort},
		Status: coxv1alpha1.LoopStatus{
			Phase: coxv1alpha1.LoopPhasePlanning,
			// The stored state: support was available last pass; this pass the
			// CRD is gone.
			Conditions: []metav1.Condition{{
				Type:    coxv1alpha1.CheckpointsAvailableCondition,
				Status:  metav1.ConditionTrue,
				Reason:  CheckpointReasonAvailable,
				Message: "volume-snapshot checkpoints are being taken after each successful verify (driver hostpath.csi.k8s.io)",
			}},
		},
	}
	r := &LoopReconciler{
		snapshotCRDPresent: func(context.Context) bool { return false },
	}

	changed, terminal := r.applyCheckpointConditions(context.Background(), loop)
	if !changed {
		t.Fatal("a stored True replaced by a computed False must report changed=true (F52 A2 / #302 P2)")
	}
	if terminal {
		t.Fatal("BestEffort without support is not terminal (the Loop carries on)")
	}
	cond := findConditionIn(loop.Status.Conditions, coxv1alpha1.CheckpointsAvailableCondition)
	if cond.Status != metav1.ConditionFalse || cond.Reason != CheckpointReasonCRDMissing {
		t.Fatalf("the stored condition must be the computed False/CRDMissing, got %s/%s", cond.Status, cond.Reason)
	}
}

// supportedCheckpointReconciler builds a reconciler whose checkpoint support
// check answers True without a cluster: the CRD is reported present, the
// workspace driver comes from the seam, and one VolumeSnapshotClass for that
// driver is in a fake client (the only API read the support check makes).
func supportedCheckpointReconciler(t *testing.T, driver string) *LoopReconciler {
	t.Helper()
	vscListGVK := volumeSnapshotClassListGVK
	vscGVK := schema.GroupVersionKind{Group: snapshotGroup, Version: "v1", Kind: "VolumeSnapshotClass"}
	sc := runtime.NewScheme()
	sc.AddKnownTypeWithName(vscGVK, &unstructured.Unstructured{})
	sc.AddKnownTypeWithName(vscListGVK, &unstructured.UnstructuredList{})
	vsc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": snapshotGV,
		"kind":       "VolumeSnapshotClass",
		"metadata":   map[string]any{"name": "f52a-vsc"},
		"driver":     driver,
	}}
	c := fake.NewClientBuilder().WithScheme(sc).WithObjects(vsc).Build()
	return &LoopReconciler{
		Client:             c,
		snapshotCRDPresent: func(context.Context) bool { return true },
		workspaceDriverSeam: func(context.Context, *coxv1alpha1.Loop) (string, bool) {
			return driver, true
		},
	}
}
