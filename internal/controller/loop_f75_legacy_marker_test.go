// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

// F75 (R32 I113(b); pi3 design .samples/design-f74-f82.md §PR A): a Restored
// marker written before F35's target key existed must not vouch for a restore
// target the spec names NOW. The defect only shows while the workspace PVC is
// PRESENT: restoreCompletedForSpec used to answer "complete" for a keyless
// marker whatever the spec said, and b2RestoreTarget then returns nil for a
// present PVC — so a rollback requested on a Loop that had already completed a
// (pre-F35) restore was silently dropped.

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// f75Loop builds a Loop with a workspace PVC present and the given spec restore
// target.
func f75Loop(t *testing.T, restoreFrom string) (*LoopReconciler, *coxv1alpha1.Loop) {
	t.Helper()
	scheme := newI42cForeignNPScheme(t)
	const (
		ns   = "f75-ns"
		name = "f75-loop"
	)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: workspacePVCName(name), Namespace: ns},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()
	r := &LoopReconciler{CNIProber: testCNIProber(), Client: c, Scheme: scheme}
	loop := &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	loop.Spec.RestoreFrom = restoreFrom
	return r, loop
}

func f75MarkKeyless(loop *coxv1alpha1.Loop) {
	// A marker exactly as F35's predecessors wrote it: no " [spec-target=…]" block.
	loop.Status.Conditions = []metav1.Condition{{
		Type:               coxv1alpha1.RestoreCondition,
		Status:             metav1.ConditionTrue,
		Reason:             coxv1alpha1.RestoreReasonRestored,
		Message:            "workspace PVC restored from snap (rollback, iter-3)",
		ObservedGeneration: 1,
	}}
}

// TestF75KeylessMarkerDoesNotCoverANewTarget is the F75 detector: a keyless
// Restored marker plus a spec-named target and a present PVC still yields a
// restore target, so the rollback runs.
func TestF75KeylessMarkerDoesNotCoverANewTarget(t *testing.T) {
	ctx := context.Background()
	r, loop := f75Loop(t, "iter-3")
	f75MarkKeyless(loop)

	target := r.b2RestoreTarget(ctx, loop)
	if target == nil {
		t.Fatal("a keyless marker must not cover spec.restoreFrom=iter-3: b2RestoreTarget returned nil, so the rollback would never run")
	}
	if target.kind != b2KindRollback {
		t.Fatalf("target kind = %q, want %q", target.kind, b2KindRollback)
	}
	if target.iteration != 3 {
		t.Fatalf("target iteration = %d, want 3 (spec.restoreFrom=iter-3)", target.iteration)
	}
	if want := checkpointName(loop, 3); target.snapshot != want {
		t.Fatalf("target snapshot = %q, want %q", target.snapshot, want)
	}
}

// TestF75KeylessMarkerStaysCompleteWithNoSpecTarget is the control that keeps
// the legacy behaviour the marker was written for: with no spec target it stays
// complete, so no restore is re-armed on every reconcile.
func TestF75KeylessMarkerStaysCompleteWithNoSpecTarget(t *testing.T) {
	ctx := context.Background()
	r, loop := f75Loop(t, "")
	f75MarkKeyless(loop)

	if target := r.b2RestoreTarget(ctx, loop); target != nil {
		t.Fatalf("a keyless marker with no spec target must stay complete (no re-restore), got target %+v", target)
	}
}

// TestF75KeyedMarkerStillHoldsTheOneShot is the control that the F35 key keeps
// working: a marker whose key matches the spec target and a present PVC still
// means "this restore is done — do not wipe the work since".
func TestF75KeyedMarkerStillHoldsTheOneShot(t *testing.T) {
	ctx := context.Background()
	r, loop := f75Loop(t, "iter-3")
	setRestoreCondition(loop, true, coxv1alpha1.RestoreReasonRestored,
		restoredMessage(loop, &b2RestoreTarget{snapshot: "snap", kind: b2KindRollback, iteration: 3}))
	loop.Generation = 9 // an unrelated edit must not re-arm it

	if target := r.b2RestoreTarget(ctx, loop); target != nil {
		t.Fatalf("a completed restore for the current target must stay complete, got target %+v", target)
	}
}
