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

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// TestRestoreCompletedForSpec (F35): the one-shot marker is keyed on the spec's
// restore target, never on metadata.generation.
func TestRestoreCompletedForSpec(t *testing.T) {
	mk := func(restoreFrom string, fork *coxv1alpha1.ForkOf, gen int64) *coxv1alpha1.Loop {
		l := &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: "l", Generation: gen}}
		l.Spec.RestoreFrom = restoreFrom
		l.Spec.ForkOf = fork
		return l
	}
	mark := func(l *coxv1alpha1.Loop, msg string, status metav1.ConditionStatus, reason string) {
		l.Status.Conditions = []metav1.Condition{{
			Type: coxv1alpha1.RestoreCondition, Status: status, Reason: reason, Message: msg,
			ObservedGeneration: 1, // deliberately stale: the generation must not matter
		}}
	}
	done := func(l *coxv1alpha1.Loop) string {
		return restoredMessage(l, &b2RestoreTarget{snapshot: "snap", kind: b2KindRollback, iteration: 3})
	}

	t.Run("rollback marker survives later generations", func(t *testing.T) {
		l := mk("iter-3", nil, 1)
		mark(l, done(l), metav1.ConditionTrue, coxv1alpha1.RestoreReasonRestored)
		l.Generation = 9
		if !restoreCompletedForSpec(l) {
			t.Fatal("an unrelated spec edit (generation bump) must not re-arm a completed rollback")
		}
	})
	t.Run("a different restore target re-arms", func(t *testing.T) {
		l := mk("iter-3", nil, 1)
		mark(l, done(l), metav1.ConditionTrue, coxv1alpha1.RestoreReasonRestored)
		l.Spec.RestoreFrom = "iter-4"
		if restoreCompletedForSpec(l) {
			t.Fatal("a changed restoreFrom must re-arm")
		}
	})
	t.Run("restoreFrom set on a Loop that had none re-arms", func(t *testing.T) {
		l := mk("", nil, 1)
		mark(l, done(l), metav1.ConditionTrue, coxv1alpha1.RestoreReasonRestored)
		l.Spec.RestoreFrom = "iter-2"
		if restoreCompletedForSpec(l) {
			t.Fatal("a new rollback target on a previously restored Loop must re-arm")
		}
	})
	t.Run("fork marker is keyed on the fork source", func(t *testing.T) {
		l := mk("", &coxv1alpha1.ForkOf{Loop: "src", Iteration: 2}, 1)
		mark(l, done(l), metav1.ConditionTrue, coxv1alpha1.RestoreReasonRestored)
		l.Generation = 5
		if !restoreCompletedForSpec(l) {
			t.Fatal("a completed fork must stay complete across spec edits")
		}
	})
	t.Run("legacy marker without a key stays complete for a Loop with no spec target", func(t *testing.T) {
		l := mk("", nil, 7)
		mark(l, "workspace PVC restored from snap (rollback, iter-3)", metav1.ConditionTrue, coxv1alpha1.RestoreReasonRestored)
		if !restoreCompletedForSpec(l) {
			t.Fatal("a marker written before the key existed must not be re-armed by an edit")
		}
	})
	t.Run("legacy marker names its target: a LOST-PVC restore does not cover a rollback (F75)", func(t *testing.T) {
		// The marker predates the key. Its text says it was a lost-PVC restore,
		// so it cannot vouch for a rollback the spec now names: with the marker
		// counting as complete and the PVC present, b2RestoreTarget would return
		// nil and the rollback would never run.
		l := mk("iter-3", nil, 7)
		mark(l, "workspace PVC restored from snap (lostPVC, iter-3)", metav1.ConditionTrue, coxv1alpha1.RestoreReasonRestored)
		if restoreCompletedForSpec(l) {
			t.Fatal("a keyless lost-PVC marker must not cover a rollback target the spec names")
		}
	})
	t.Run("legacy marker names its target: a restored rollback covers the same spec target (PR #337 P1)", func(t *testing.T) {
		// restoreFrom is immutable and nothing clears it, so every rollback
		// completed on v0.1.1/v0.1.2 (B2 yes, F35 no) is this shape. Treating it
		// as incomplete deletes the PVC and re-restores the old snapshot on the
		// first reconcile after upgrade.
		l := mk("iter-3", nil, 7)
		mark(l, "workspace PVC restored from snap (rollback, iter-3)", metav1.ConditionTrue, coxv1alpha1.RestoreReasonRestored)
		if !restoreCompletedForSpec(l) {
			t.Fatal("a keyless rollback marker for the iter the spec still names is complete; re-arming it wipes the workspace")
		}
	})
	t.Run("in-progress or failed restore is not complete", func(t *testing.T) {
		l := mk("iter-3", nil, 1)
		mark(l, done(l), metav1.ConditionFalse, coxv1alpha1.RestoreReasonDeletingPVC)
		if restoreCompletedForSpec(l) {
			t.Fatal("a False condition is not a completed marker")
		}
	})
}
