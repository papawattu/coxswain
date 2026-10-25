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

// F83b: the verify Job is one snapshot and one trust decision.
//
// Two defects, both about WHO the Job is and WHICH read it is:
//
//   - applyVerifyOutcome read the Job twice (readVerifyJobPod bound the pod to
//     one Job's UID, then getVerifyJob re-read the Job for the batch JobFailed
//     condition). The operator's infra-retry deletes and recreates the Job under
//     the same name, so between the two reads the pod can come from Job A while
//     jobFailed comes from Job B: verifyOutcome(nil, n, true) then spends an
//     infra attempt on the other Job's evidence.
//   - nothing checked that the Job carrying the Loop's verify name is the
//     Loop's. Creating a Job reaches that name, so a foreign Job was both
//     trusted as evidence and deleted as "stale" by ensureVerifyJob.
package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

const f83bPin = "0123456789abcdef0123456789abcdef01234567"

// f83bLoop is the Verifying fixture these specs need: a pin and one check.
func f83bLoop() *coxv1alpha1.Loop {
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "f83b", Namespace: "f83b-ns", UID: types.UID("f83b-loop-uid")},
		Spec: coxv1alpha1.LoopSpec{
			Goal:      "F83b verify job trust",
			Workspace: testWorkspace(),
			Verify: coxv1alpha1.VerifyConfig{
				Preset:           "none",
				AcceptanceChecks: []string{"true"},
			},
		},
	}
	loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
	loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseVerifying
	loop.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: f83bPin}
	// ensureVerifyJob holds with no base to diff against (the tamper check
	// would be meaningless), so the fixture carries one.
	loop.Status.BaseCommit = "1111111111111111111111111111111111111111"
	return loop
}

// f83bJob builds the Job with the Loop's verify name. commit is the D27
// annotation; failed sets the batch JobFailed condition.
func f83bJob(loop *coxv1alpha1.Loop, commit string, failed bool) *batchv1.Job {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:        verifyJobName(loop),
		Namespace:   loop.Namespace,
		Annotations: map[string]string{verifyCommitAnnotation: commit},
	}}
	if failed {
		job.Status.Conditions = []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
		}}
	}
	return job
}

// testF83bOneJobSnapshot pins B1: one decision reads ONE Job.
func TestF83bApplyVerifyOutcomeReadsOneJobSnapshot(t *testing.T) {
	ctx := context.Background()
	scheme := newI42cForeignNPScheme(t)
	loop := f83bLoop()

	// Job A: ours, not Failed (the batch controller has not given up).
	jobA := f83bJob(loop, f83bPin, false)
	if err := controllerutil.SetControllerReference(loop, jobA, scheme); err != nil {
		t.Fatalf("own job A: %v", err)
	}
	// Job B: ours, Failed — an infra-retry replacement for the same name.
	jobB := f83bJob(loop, f83bPin, true)
	if err := controllerutil.SetControllerReference(loop, jobB, scheme); err != nil {
		t.Fatalf("own job B: %v", err)
	}

	jobReads := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			job, ok := obj.(*batchv1.Job)
			if !ok {
				return cl.Get(ctx, key, obj, opts...)
			}
			jobReads++
			if jobReads == 1 {
				*job = *jobA
			} else {
				*job = *jobB
			}
			return nil
		},
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok {
				return nil // the Job's pod is not created yet
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
	rec := record.NewFakeRecorder(16)
	r := &LoopReconciler{Client: c, Scheme: scheme, apiReader: c, Recorder: rec}

	_, requeue := r.applyVerifyOutcome(ctx, loop)
	if !requeue {
		t.Fatal("a Job with no pod yet is no decision: the pass must requeue")
	}
	if jobReads != 1 {
		t.Fatalf("the verify Job must be read ONCE per decision, got %d reads (the pod came from Job A, jobFailed from Job B)", jobReads)
	}
	if loop.Status.Phase != coxv1alpha1.LoopPhaseVerifying {
		t.Fatalf("no decision must leave the phase at Verifying, got %s", loop.Status.Phase)
	}
	if loop.Status.Verify != nil && loop.Status.Verify.InfraAttempts != 0 {
		t.Fatalf("another Job's JobFailed must not spend an infra attempt, got %d", loop.Status.Verify.InfraAttempts)
	}
}

// testF83bForeignJobIsNeverEvidence pins B2's trust half: the exact shape that
// burned an attempt (a Failed Job with no pod) decides nothing when the Job is
// not the Loop's, and the degradation is surfaced.
func TestF83bForeignVerifyJobIsNeverEvidence(t *testing.T) {
	ctx := context.Background()
	scheme := newI42cForeignNPScheme(t)
	loop := f83bLoop()

	foreign := f83bJob(loop, f83bPin, true) // never owned: no controller reference
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign, loop).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok {
				return nil
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
	rec := record.NewFakeRecorder(16)
	r := &LoopReconciler{Client: c, Scheme: scheme, apiReader: c, Recorder: rec}

	_, requeue := r.applyVerifyOutcome(ctx, loop)
	if !requeue {
		t.Fatal("a foreign Job is no decision: the pass must requeue, not advance")
	}
	if loop.Status.Phase != coxv1alpha1.LoopPhaseVerifying {
		t.Fatalf("a foreign Job must never decide the phase, got %s", loop.Status.Phase)
	}
	if loop.Status.Verify != nil && loop.Status.Verify.InfraAttempts != 0 {
		t.Fatalf("a foreign Job must not spend an infra attempt, got %d", loop.Status.Verify.InfraAttempts)
	}
	if n := f83bEventCount(drainEvents(rec)); n != 1 {
		t.Fatalf("the refusal must be visible: want 1 %s Event, got %d", verifyJobForeignEventReason, n)
	}
	if _, ok := loop.Annotations[verifyJobForeignAnnotation]; !ok {
		t.Fatal("the Event must be transition-gated by the operator-owned marker")
	}
}

// testF83bForeignJobIsNeverDeleted pins B2's destruction half: a foreign Job
// with stale evidence is left alone (the old code deleted it as "stale").
func TestF83bForeignVerifyJobIsNeverDeleted(t *testing.T) {
	ctx := context.Background()
	scheme := newI42cForeignNPScheme(t)
	loop := f83bLoop()

	// A DIFFERENT commit: the D27 stale-evidence branch would delete it.
	foreign := f83bJob(loop, "ffffffffffffffffffffffffffffffffffffffff", false)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign, loop).Build()
	rec := record.NewFakeRecorder(16)
	r := &LoopReconciler{Client: c, Scheme: scheme, apiReader: c, Recorder: rec}

	if err := r.ensureVerifyJob(ctx, loop); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	got := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: loop.Namespace, Name: verifyJobName(loop)}, got); err != nil {
		t.Fatalf("the operator must never delete a Job that is not this Loop's: %v", err)
	}
	if n := f83bEventCount(drainEvents(rec)); n != 1 {
		t.Fatalf("the refusal must be visible: want 1 %s Event, got %d", verifyJobForeignEventReason, n)
	}
}

// f83bEventCount counts the VerifyJobForeign Events in a drained recorder.
func f83bEventCount(events []string) int {
	n := 0
	for _, e := range events {
		if strings.Contains(e, verifyJobForeignEventReason) {
			n++
		}
	}
	return n
}
