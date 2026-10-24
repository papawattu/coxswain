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
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

func f34Loop(status metav1.ConditionStatus, reason string) *coxv1alpha1.Loop {
	l := &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns"}}
	if reason != "" {
		l.Status.Conditions = []metav1.Condition{{
			Type: coxv1alpha1.RestoreCondition, Status: status, Reason: reason,
		}}
	}
	return l
}

// TestB2RestoreHoldSuspendedReasons (F34 P3-4): the hold covers all three
// in-progress reasons loop_memory_writer_job.go documents, and none of the
// non-hold states (rejected, deferred, restored, no condition).
func TestB2RestoreHoldSuspendedReasons(t *testing.T) {
	r := &LoopReconciler{}
	for _, tc := range []struct {
		name   string
		status metav1.ConditionStatus
		reason string
		want   bool
	}{
		{"StoppingWorkloads", metav1.ConditionFalse, coxv1alpha1.RestoreReasonStoppingWorkloads, true},
		{"StoppingSandbox", metav1.ConditionFalse, coxv1alpha1.RestoreReasonStoppingSandbox, true},
		{"DeletingPVC", metav1.ConditionFalse, coxv1alpha1.RestoreReasonDeletingPVC, true},
		{"RestoreSourceMissing", metav1.ConditionFalse, coxv1alpha1.RestoreReasonSourceMissing, false},
		{"RestoreSourceNotReady", metav1.ConditionFalse, coxv1alpha1.RestoreReasonSourceNotReady, false},
		{"Restored", metav1.ConditionTrue, coxv1alpha1.RestoreReasonRestored, false},
		{"no condition", "", "", false},
	} {
		if got := r.b2RestoreHoldSuspended(f34Loop(tc.status, tc.reason)); got != tc.want {
			t.Errorf("%s: hold=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestMemoryWriterHeldWhileStoppingWorkloads (F34 P3-4): with the restore in
// StoppingWorkloads the writer Job is not created. The hold returns before
// any API call, so the zero reconciler is enough: a regression that lets it
// through would dereference the nil client and panic.
func TestMemoryWriterHeldWhileStoppingWorkloads(t *testing.T) {
	r := &LoopReconciler{}
	requeue, err := r.ensureMemoryWriterJobAndNetPolicy(context.Background(),
		f34Loop(metav1.ConditionFalse, coxv1alpha1.RestoreReasonStoppingWorkloads))
	if requeue || err != nil {
		t.Fatalf("held: requeue=%v err=%v, want false,nil", requeue, err)
	}
}
