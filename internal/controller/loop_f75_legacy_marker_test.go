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
// marker written before F35's target key existed must be compared with the
// target it NAMES, not treated as complete or incomplete wholesale.
//
// The M1 half (the defect F75 was written for): a keyless marker cannot vouch
// for a restore target the spec names NOW. The defect only shows while the
// workspace PVC is PRESENT: restoreCompletedForSpec used to answer "complete"
// for a keyless marker whatever the spec said, and b2RestoreTarget then returns
// nil for a present PVC — so a rollback requested on a Loop that had already
// completed a (pre-F35) LOST-PVC restore was silently dropped.
//
// The P1 half (pi3's first review of PR #337, F1): the first version of the fix
// answered "incomplete" for EVERY keyless marker once the spec named a target,
// which re-armed rollbacks and forks that had already completed — deleting the
// PVC and restoring the snapshot over everything done since. Every restore
// completed on v0.1.1/v0.1.2 is that shape, because restoreFrom/forkOf are
// immutable and nothing clears restoreFrom after a rollback, so the marker's
// target IS the spec's target. The marker's own text carries the kind and
// iteration it restored, and the two kinds must be told apart: a lost-PVC
// restore does not vouch for a rollback, a rollback does vouch for its own iter.

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

// f75MarkKeyless writes a marker exactly as F35's predecessors did: the human
// text only, with NO " [spec-target=…]" block. kind/iteration go into the text,
// as every completion since the machinery landed has written them.
func f75MarkKeyless(loop *coxv1alpha1.Loop, kind b2Kind, iteration int) {
	loop.Status.Conditions = []metav1.Condition{{
		Type:   coxv1alpha1.RestoreCondition,
		Status: metav1.ConditionTrue,
		Reason: coxv1alpha1.RestoreReasonRestored,
		Message: "workspace PVC restored from snap (" + string(kind) + ", iter-" +
			itoa(iteration) + ")",
		ObservedGeneration: 1,
	}}
}

// TestF75KeylessMarkerDoesNotCoverANewTarget is the F75 detector: a keyless
// LOST-PVC marker plus a spec-named rollback target and a present PVC still
// yields a restore target, so the rollback runs. The kinds differ, which is the
// only thing that can make this safe to distinguish (F1).
func TestF75KeylessMarkerDoesNotCoverANewTarget(t *testing.T) {
	ctx := context.Background()
	r, loop := f75Loop(t, "iter-3")
	f75MarkKeyless(loop, b2KindLostPVC, 3)

	target := r.b2RestoreTarget(ctx, loop)
	if target == nil {
		t.Fatal("a keyless lost-PVC marker must not cover spec.restoreFrom=iter-3: b2RestoreTarget returned nil, so the rollback would never run")
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

// TestF75KeylessRollbackMarkerCoversItsOwnTarget is the P1 detector (pi3's F1,
// PR #337): a rollback completed before F35 wrote "(rollback, iter-3)" in its
// message, and the spec still names that target because restoreFrom is
// immutable. The marker is COMPLETE — re-arming it would delete the PVC and
// restore the old snapshot over everything done since.
func TestF75KeylessRollbackMarkerCoversItsOwnTarget(t *testing.T) {
	ctx := context.Background()
	r, loop := f75Loop(t, "iter-3")
	f75MarkKeyless(loop, b2KindRollback, 3)

	if target := r.b2RestoreTarget(ctx, loop); target != nil {
		t.Fatalf("a keyless rollback marker for the iter the spec still names must stay complete (no PVC wipe), got target %+v", target)
	}
}

// TestF75KeylessRollbackMarkerDoesNotCoverADifferentIter keeps the re-arm reachable
// for a rollback marker whose recorded iter is not the spec's (a bypassing
// client; the CRD makes restoreFrom immutable).
func TestF75KeylessRollbackMarkerDoesNotCoverADifferentIter(t *testing.T) {
	ctx := context.Background()
	r, loop := f75Loop(t, "iter-3")
	f75MarkKeyless(loop, b2KindRollback, 2)

	target := r.b2RestoreTarget(ctx, loop)
	if target == nil || target.iteration != 3 {
		t.Fatalf("a keyless rollback marker for iter-2 must not cover spec.restoreFrom=iter-3, got target %+v", target)
	}
}

// TestF75KeylessForkMarkerCoversItsOwnFork is the fork arm of F1 (there was no
// fork test at all before it): a completed fork's keyless marker names the fork
// source, and spec.forkOf is immutable, so it stays complete.
func TestF75KeylessForkMarkerCoversItsOwnFork(t *testing.T) {
	ctx := context.Background()
	r, loop := f75Loop(t, "")
	loop.Spec.ForkOf = &coxv1alpha1.ForkOf{Loop: "f75-src", Iteration: 2}
	f75MarkKeyless(loop, b2KindFork, 2)

	if target := r.b2RestoreTarget(ctx, loop); target != nil {
		t.Fatalf("a keyless fork marker for the fork source the spec still names must stay complete (no PVC wipe), got target %+v", target)
	}
}

// TestF75KeylessForkMarkerDoesNotCoverADifferentFork is the fork control: a
// marker that names another source iteration does not cover the spec's fork, so
// the fork restore runs.
func TestF75KeylessForkMarkerDoesNotCoverADifferentFork(t *testing.T) {
	ctx := context.Background()
	r, loop := f75Loop(t, "")
	loop.Spec.ForkOf = &coxv1alpha1.ForkOf{Loop: "f75-src", Iteration: 2}
	f75MarkKeyless(loop, b2KindFork, 1)

	target := r.b2RestoreTarget(ctx, loop)
	if target == nil {
		t.Fatal("a keyless marker for fork iter-1 must not cover spec.forkOf{iteration: 2}")
	}
	if target.kind != b2KindFork || target.iteration != 2 {
		t.Fatalf("target = %+v, want the fork of f75-src iter-2", target)
	}
}

// TestF75UnparseableKeylessMarkerStaysComplete pins the conservative choice: a
// legacy marker whose text carries no readable target is complete. A skipped
// rollback is recoverable (set the target again); a wiped workspace is not.
func TestF75UnparseableKeylessMarkerStaysComplete(t *testing.T) {
	ctx := context.Background()
	r, loop := f75Loop(t, "iter-3")
	loop.Status.Conditions = []metav1.Condition{{
		Type:               coxv1alpha1.RestoreCondition,
		Status:             metav1.ConditionTrue,
		Reason:             coxv1alpha1.RestoreReasonRestored,
		Message:            "workspace PVC restored from snap", // no "(<kind>, iter-<N>)" block
		ObservedGeneration: 1,
	}}

	if target := r.b2RestoreTarget(ctx, loop); target != nil {
		t.Fatalf("an unparseable keyless marker must stay complete, got target %+v", target)
	}
}

// TestF75KeylessMarkerStaysCompleteWithNoSpecTarget is the control that keeps
// the legacy behaviour the marker was written for: with no spec target it stays
// complete, so no restore is re-armed on every reconcile.
func TestF75KeylessMarkerStaysCompleteWithNoSpecTarget(t *testing.T) {
	ctx := context.Background()
	r, loop := f75Loop(t, "")
	f75MarkKeyless(loop, b2KindRollback, 3)

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

// TestLegacyRestoreTarget pins the parser over the shapes the two pre-F35
// completion sites actually wrote (the message text is the same in both).
func TestLegacyRestoreTarget(t *testing.T) {
	cases := []struct {
		msg       string
		wantKind  b2Kind
		wantIter  int
		wantParse bool
	}{
		{"workspace PVC restored from snap (rollback, iter-3)", b2KindRollback, 3, true},
		{"workspace PVC restored from snap (fork, iter-0)", b2KindFork, 0, true},
		{"workspace PVC restored from snap (lostPVC, iter-12)", b2KindLostPVC, 12, true},
		{"workspace PVC restored from snap", "", 0, false},
		{"workspace PVC restored from snap (rollback, iter-x)", "", 0, false},
		{"workspace PVC restored from snap (rollback iter-3)", "", 0, false},
		{"workspace PVC restored from snap (rollback, iter-3", "", 0, false},
	}
	for _, tc := range cases {
		kind, iter, ok := legacyRestoreTarget(tc.msg)
		if ok != tc.wantParse || (ok && (kind != tc.wantKind || iter != tc.wantIter)) {
			t.Fatalf("legacyRestoreTarget(%q) = (%q, %d, %t), want (%q, %d, %t)",
				tc.msg, kind, iter, ok, tc.wantKind, tc.wantIter, tc.wantParse)
		}
	}
}
