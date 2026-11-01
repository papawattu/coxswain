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

// F94 (session 2, owner decision 2026-11-01 (a)+(b)+(c)): the gates for the
// canary's no-test exits. The specs plant the verify Job's pod init statuses
// (envtest has no batch Job controller or kubelet) and drive the reconciles,
// exactly like the F60 canary envtest spec (the helpers — f60DriveToVerifying,
// f60PlantPod, f60TrustedPrefix, f60Events — are reused from there):
//
//  1. canary exit 46 → Failed:TestsRemoved + a Warning Event + NO new verify
//     Job (terminal, like SubvertedVerify: the agent removed every test, so a
//     re-run would import the same pin).
//  2. canary exit 44 → BOUNDED infra retry (F94c): attempt 1 recreates the
//     Job (a different UID is counted, so the bound is reachable), and a
//     failure after verifyInfraRetries recreations ends in
//     Failed:VerifyInfraFailed — a permanent 44 must NOT hold Verifying
//     forever (the pre-F94 one-shot no-decision path is gone).
//  3. a canary still Running or Waiting → NO decision (the I49 norm: an
//     in-progress gate takes no decision and requeues).
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("F94: the verify canary's no-test exits (session 2 gates)", func() {
	ctx := context.Background()

	// f94Reconcile is one full Reconcile + a fresh Loop read: the
	// verify-outcome decision runs INSIDE Reconcile
	// (readVerifyJobPod → applyVerifyOutcome), so the spec drives the real
	// Reconcile — the same shape s5aReconcile uses.
	f94Reconcile := func(r *LoopReconciler, ns, name string) *coxv1alpha1.Loop {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred(), "Reconcile must not error")
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		return loop
	}

	f94GetLoop := func(ns, name string) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		return loop
	}

	f94Events := func(recorder *record.FakeRecorder) []string {
		out := []string{}
		for {
			select {
			case e := <-recorder.Events:
				out = append(out, e)
			default:
				return out
			}
		}
	}

	It("fails the Loop with TestsRemoved (terminal) when the canary exits 46", func() {
		ns := f60Ns("f94-tests-removed")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f94trem"
		recorder := record.NewFakeRecorder(64)
		r := f60Reconciler(recorder)
		jobName := f60DriveToVerifying(r, ns, name)
		job := f60GetJob(ns, jobName)
		Expect(job).NotTo(BeNil())

		// The shape the canary's script produces (session 1, cases (a) and
		// (iv)): HEAD has no *_test.go but the BASE commit had tests — the
		// agent removed every test, so go test would pass vacuously. The
		// canary exits 46. The Loop must be Failed:TestsRemoved (terminal,
		// like SubvertedVerify), and NO new verify Job is created (a re-run
		// would import the same pin).
		f60PlantPod(ns, name, jobName, job, append(f60TrustedPrefix(),
			corev1.ContainerStatus{Name: verifyCanaryInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: verifyCanaryTestsRemovedExit}}},
		))

		fresh := f94Reconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"tests removed is a TERMINAL verdict: the Loop is Failed (a re-run would import the same pin)")
		// The Failed condition carries the TestsRemoved reason.
		var failed *metav1.Condition
		for i := range fresh.Status.Conditions {
			if fresh.Status.Conditions[i].Type == string(coxv1alpha1.LoopPhaseFailed) &&
				fresh.Status.Conditions[i].Status == metav1.ConditionTrue {
				failed = &fresh.Status.Conditions[i]
			}
		}
		Expect(failed).NotTo(BeNil(), "the Loop must carry a Failed condition")
		Expect(failed.Reason).To(Equal(TestsRemovedReason), "the Failed reason must be TestsRemoved")
		// A Warning Event is recorded (the verdict is observable).
		evs := f94Events(recorder)
		Expect(evs).To(ContainElement(ContainSubstring(TestsRemovedReason)),
			"the verdict must be surfaced with a Warning Event")
		// No new verify Job: the verdict is terminal, like SubvertedVerify.
		by := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, by, client.InNamespace(ns))).To(Succeed())
		var verifyJobs int
		for _, j := range by.Items {
			if j.Labels[verifyForLabel] == name {
				verifyJobs++
			}
		}
		Expect(verifyJobs).To(Equal(1),
			"tests removed is terminal: no new verify Job is created (only the one that produced the evidence)")
	})

	It("treats a canary exit 44 as a bounded infra retry (F94c): retry at attempt 1, Failed after the bound", func() {
		ns := f60Ns("f94-unavail")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f94unav"
		recorder := record.NewFakeRecorder(128)
		r := f60Reconciler(recorder)
		jobName := f60DriveToVerifying(r, ns, name)
		job := f60GetJob(ns, jobName)
		Expect(job).NotTo(BeNil())

		// F94c: the canary is unavailable (no go toolchain, every package
		// skipped, or the base tree unreadable — the script fails closed with
		// 44 rather than reading a git failure as "no tests"). That is an
		// INFRA failure: a bounded retry via status.verify.infraAttempts,
		// then Failed after the bound — a permanent 44 must NOT hold
		// Verifying forever (the pre-F94 one-shot no-decision path wedged
		// every testless repo).
		//
		// Attempt 1: the original Job's pod shows the canary terminated with
		// 44 (the canary init sits before check-0, so a canary exit fails the
		// pod — the pod's canary status is the evidence canaryGateDecision
		// reads, the same shape the F60 45 spec plants).
		f60PlantPod(ns, name, jobName, job, append(f60TrustedPrefix(),
			corev1.ContainerStatus{Name: verifyCanaryInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: verifyCanaryUnavailableExit}}},
		))
		first := f94Reconcile(r, ns, name)
		Expect(first.Status.Verify).NotTo(BeNil(), "the infra attempt is recorded in status.verify")
		Expect(first.Status.Verify.InfraAttempts).To(Equal(int32(1)),
			"attempt 1 is counted (the bounded retry counter), not a forever-hold")
		evs := f94Events(recorder)
		Expect(evs).To(ContainElement(ContainSubstring(verifyInfraRetryEventReason)),
			"attempt 1 must be surfaced with the VerifyInfraRetry event")
		for _, e := range evs {
			Expect(e).NotTo(ContainSubstring(SubvertedVerifyReason),
				"an unavailable canary must never be reported as a subversion verdict")
			Expect(e).NotTo(ContainSubstring(TestsRemovedReason),
				"an unavailable canary (44) is infra, not tests removed (46)")
		}
		// The original Job is gone or Terminating (the operator deleted it to
		// recreate a fresh one).
		// The operator deleted the failed Job (background propagation): it is
		// either already gone or in Terminating. A fresh Job with a new UID is
		// created on a later reconcile (the next loop iteration plants its
		// failing pod and relies on the new UID for the attempt guard).
		cur := f60GetJob(ns, jobName)
		if cur != nil && cur.DeletionTimestamp == nil && cur.UID == job.UID {
			Fail("within the bound the operator deletes the failed Job (a fresh one is created next reconcile)")
		}

		// Attempts 2..bound+1: the bound is verifyInfraRetries RECREATIONS,
		// so attempt verifyInfraRetries+1 is the last allowed run. The
		// attempt guard is the Job UID (a recreated Job has a new UID, so
		// each fresh Job's first failure IS counted — the shape a real
		// cluster shows; the i65-exhaust spec drives the identical loop).
		//
		// The pod alone is the decision's evidence: canaryGateDecision reads
		// the POD's canary init (a 44 is verifyInfraFailed, the infra-failure
		// outcome), so the fixture plants ONLY the pod — exactly the F60 45
		// spec's shape. No Job batch conditions are stamped: envtest's
		// 1.34 API server requires status.startTime plus the FailureTarget
		// condition before a Failed batch condition is accepted, and the
		// decision does not read the Job's conditions at all.
		attempts := int32(1)
		for attempts <= verifyInfraRetries {
			// The current attempt's Job (recreated with a new UID on the
			// previous iteration) has its failing pod planted; the reconcile
			// counts the attempt and deletes the failed Job.
			job = f60GetJob(ns, jobName)
			if job == nil {
				// No Job yet (the delete was still in flight): the next
				// reconcile recreates it (ensureVerifyJob).
				f94Reconcile(r, ns, name)
				continue
			}
			f60PlantPod(ns, name, jobName, job, append(f60TrustedPrefix(),
				corev1.ContainerStatus{Name: verifyCanaryInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: verifyCanaryUnavailableExit}}},
			))
			first = f94Reconcile(r, ns, name)
			attempts = first.Status.Verify.InfraAttempts
			if first.Status.Phase == coxv1alpha1.LoopPhaseFailed {
				break
			}
		}
		// The last attempt (verifyInfraRetries+1) fails the Loop on the
		// reconcile that counted it: the bound is verifyInfraRetries
		// recreations, so attempt verifyInfraRetries+1 is the final failure.
		fresh := first

		// The bound is exhausted: the Loop is Failed:VerifyInfraFailed — a
		// permanent 44 ends in Failed after the bound, never a
		// forever-Verifying hold. (fresh is the last counted attempt's
		// reconcile read above.)
		_ = f94GetLoop(ns, name) // the namespace is torn down with the spec
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"a permanent 44 must end in Failed after the infra-retry bound (F94c: no forever-Verifying hold)")
		var failed *metav1.Condition
		for i := range fresh.Status.Conditions {
			if fresh.Status.Conditions[i].Type == string(coxv1alpha1.LoopPhaseFailed) &&
				fresh.Status.Conditions[i].Status == metav1.ConditionTrue {
				failed = &fresh.Status.Conditions[i]
			}
		}
		Expect(failed).NotTo(BeNil(), "the Loop must carry a Failed condition")
		Expect(failed.Reason).To(Equal(verifyInfraFailedReason), "the Failed reason must be VerifyInfraFailed")
		Expect(fresh.Status.Verify.InfraAttempts).To(Equal(int32(verifyInfraRetries+1)),
			"the bound is verifyInfraRetries recreations: attempt verifyInfraRetries+1 is the final failure")
		_ = f94Events(recorder) // drain the final Warning Event
	})

	It("takes no decision while the canary is still Running or Waiting (the I49 norm)", func() {
		ns := f60Ns("f94-canary-running")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f94run"
		recorder := record.NewFakeRecorder(64)
		r := f60Reconciler(recorder)
		jobName := f60DriveToVerifying(r, ns, name)
		job := f60GetJob(ns, jobName)
		Expect(job).NotTo(BeNil())

		// A canary still Running (not terminated) is IN PROGRESS: no decision
		// (the I49 norm, exactly like the tamper/artifact gates) — the
		// operator requeues and re-reads the pod.
		f60PlantPod(ns, name, jobName, job, append(f60TrustedPrefix(),
			corev1.ContainerStatus{Name: verifyCanaryInit, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		))
		fresh := f94Reconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a still-running canary is IN PROGRESS: no decision (no Succeeded, no Failed)")
		// The same with a canary that has not STARTED yet (Waiting).
		f60PlantPod(ns, name, jobName, job, append(f60TrustedPrefix(),
			corev1.ContainerStatus{Name: verifyCanaryInit, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
		))
		fresh = f94Reconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a still-waiting canary is IN PROGRESS: no decision (no Succeeded, no Failed)")
		_ = f94Events(recorder) // drain (the no-decision reconciles record none)
	})
})
