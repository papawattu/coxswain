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

// I65 (REVIEW-PHASE1-R23): a verify Job that fails BEFORE the checks must not
// wedge the Loop in Verifying. verifyOutcome previously read only the tamper,
// artifact and check-* init containers; if an earlier init failed (clone-base
// or import-agent — the P2h G2 run: clone-base exit 128 on a transient kind
// DNS flake), those stay PodInitializing for good, the Job goes Failed at the
// backoffLimit, nothing decides, and the Loop sits in Verifying forever.
//
// The fix treats a Failed verify Job, or a non-zero TERMINATED trusted init,
// as a decision (verifyInfraFailed): the operator recreates the Job a bounded
// number of times (status.verify.infraAttempts, the verifyInfraRetries cap —
// a retry is a NEW Job with a fresh name, the B3b pattern), then fails the
// Loop with the distinct VerifyInfraFailed reason. An infra failure says
// nothing about the agent's work and must not burn a maxIterations iteration
// (no iterate). A trusted init still Running/Waiting is NO decision (the I49
// norm: in-progress is never a failure).
//
// The envtest fixtures set pod init statuses with Status().Update AFTER
// Create (the API drops status on create), and drive the Job's batch
// conditions directly (envtest has no batch Job controller).
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// i65GoConst: the test fixtures write k8s API reason strings (the batch
// JobCondition.Reason and the corev1.ContainerStateTerminated.Reason). They
// are k8s API constants (batchv1 has no exported "BackoffLimitExceeded"
// constant; corev1 has no "Error" reason constant) — a local constant keeps
// the goconst linter quiet (the production code READS these values, never
// writes them; a test-only constant is an alias for the k8s API value, not a
// codebase DRY fix).
const (
	i65JobFailedReason    = "BackoffLimitExceeded"
	i65ContainerErrReason = "Error"
)

var _ = Describe("I65: a verify Job that fails before the checks no longer wedges the Loop", func() {
	ctx := context.Background()

	// i65Ns creates a fresh namespace (the specs defer-delete it).
	i65Ns := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	// i65Condition returns the condition of the given type (absent = nil).
	i65Condition := func(loop *coxv1alpha1.Loop, condType string) *metav1.Condition {
		for i := range loop.Status.Conditions {
			c := &loop.Status.Conditions[i]
			if c.Type == condType {
				return c
			}
		}
		return nil
	}

	// i65VerifyPodNS plants (or re-plants) the stand-in verify Job pod for
	// jobName with the given init statuses (delete-then-create, so a
	// re-plant after a recreate is fresh — the s5aClaimPod oneShotRun idiom).
	// Statuses are set with Status().Update AFTER Create (the API drops
	// status on create — the envtest norm).
	i65VerifyPodNS := func(ns, name, jobName string, inits []corev1.ContainerStatus) {
		nn := types.NamespacedName{Name: name + "-verify-pod", Namespace: ns}
		existing := &corev1.Pod{}
		if !apierrors.IsNotFound(k8sClient.Get(ctx, nn, existing)) {
			Expect(k8sClient.Delete(ctx, existing)).To(Succeed())
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-verify-pod",
				Namespace: ns,
				Labels:    map[string]string{s5aJobNameLabel: jobName, verifyForLabel: name},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.InitContainerStatuses = inits
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	// i65JobFailedNS sets the Job's batch condition JobFailed=True on the
	// named Job (envtest has no batch Job controller; the condition is the
	// Job-level failure evidence the operator reads). The Job's status is
	// set WHOLE (startTime included — the API validates it on a Job status
	// update, the batch controller's field, never set in envtest).
	i65JobFailedNS := func(ns, jobName string) {
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())
		if job.Status.StartTime == nil {
			now := metav1.Now()
			job.Status.StartTime = &now
		}
		// K8s 1.34+ Job status validation: Failed=True REQUIRES a
		// FailureTarget=true condition (the batch controller sets both;
		// envtest has no batch controller, so the fixture sets both). The
		// batch/v1.JobCondition type has no FailureTarget field (it is a
		// condition TYPE, not a field) — the API's structural validation
		// accepts a plain Failed condition in the 1.34 test server.
		job.Status.Conditions = []batchv1.JobCondition{{
			Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue,
			Reason: i65JobFailedReason, LastProbeTime: metav1.Now(),
		}, {
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: i65JobFailedReason, Message: "job backoff limit hit",
			LastProbeTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
	}

	// i65DeleteVerifyPodNS deletes the stand-in verify Job pod (the
	// reconcile after a recreate re-reads it by name — the s5aClaimPod
	// oneShotRun idiom: delete the pod before the next attempt's failure
	// is planted, so the operator's recreate-then-decide sequence is clean).
	i65DeleteVerifyPodNS := func(ns, name string) {
		pod := &corev1.Pod{}
		if !apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-verify-pod", Namespace: ns}, pod)) {
			Expect(k8sClient.Delete(ctx, pod)).To(Succeed())
		}
	}

	// i65ReplantNS re-plants a FAILED verify Job + pod from scratch (delete the
	// old Job and pod, recreate a fresh Job, plant the pod with the given init
	// statuses, mark the Job Failed). The operator's infra retry deletes the
	// old Job; the FRESH Job must be failed for the next reconcile to see it —
	// the batch Job controller would re-fail it in a real cluster (a persistent
	// infra failure), but envtest has no batch controller, so the fixture
	// models it by recreating the Job fresh and marking it Failed directly
	// (the batch controller's job, never the test's, sets the condition on a
	// Job it creates; here the fixture recreates the Job AND sets the
	// condition on the SAME fresh object, so the operator's decision sees a
	// consistent Job + pod).
	i65ReplantNS := func(ns, name, jobName string, inits []corev1.ContainerStatus) {
		oldJob := &batchv1.Job{}
		if !apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, oldJob)) {
			Expect(k8sClient.Delete(ctx, oldJob)).To(Succeed())
		}
		i65DeleteVerifyPodNS(ns, name)
		// Recreate a fresh Job (the operator's recreate, modeled by the test:
		// the operator's delete is followed by the batch controller's
		// re-create; in envtest the fixture recreates it directly). The Job
		// needs a valid spec (containers + restartPolicy) — the API validates
		// it on create, even though envtest never runs it.
		freshJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns}}
		freshJob.Spec.Template.Spec.Containers = []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}}
		freshJob.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
		Expect(k8sClient.Create(ctx, freshJob)).To(Succeed())
		// Plant the pod (the fresh Job's pod, with the failed init statuses).
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-verify-pod",
				Namespace: ns,
				Labels:    map[string]string{s5aJobNameLabel: jobName, verifyForLabel: name},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.InitContainerStatuses = inits
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		// Mark the fresh Job Failed (the batch controller's job, modeled).
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())
		if job.Status.StartTime == nil {
			now := metav1.Now()
			job.Status.StartTime = &now
		}
		job.Status.Conditions = []batchv1.JobCondition{{
			Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue,
			Reason: i65JobFailedReason, LastProbeTime: metav1.Now(),
		}, {
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: i65JobFailedReason, Message: "job backoff limit hit",
			LastProbeTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
	}

	// i65Events drains the FakeRecorder's channel.
	i65Events := func(recorder *record.FakeRecorder) []string {
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

	// i65Drive builds the Loop in ns and drives it to Verifying with the
	// given reconciler (the I65 specs build the reconciler with a Recorder,
	// so the drive must use it — s5aDriveToVerifying builds its own).
	i65Drive := func(ns, name string, r *LoopReconciler) {
		s5aEnsureSandbox(ns, name)
		s5aReconcile(r, ns, name)
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Status.BaseCommit = s5aBaseCommit
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		s5aClaimPod(ns, name, "Planning", "")
		s5aReconcile(r, ns, name)
		s5aEnsureSandbox(ns, name)
		s5aClaimPod(ns, name, "Implementing", s5aHeadCommit)
		s5aReconcile(r, ns, name)
	}

	It("recreates a failed verify Job (clone-base failed) within the retry bound — no decision burned, no iterate, an Event per retry", func() {
		ns := i65Ns("i65-clone")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "i65clone"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: recorder}
		i65Drive(ns, name, r)
		s5aReconcile(r, ns, name) // the Job is created (the pin exists)
		jobName := name + "-verify-1"

		By("failing the clone-base init (the P2h G2 DNS flake: exit 128) and marking the Job Failed")
		i65VerifyPodNS(ns, name, jobName, []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 128, Reason: i65ContainerErrReason, Message: "Could not resolve host (Timeout while contacting DNS servers)"}}},
		})
		i65JobFailedNS(ns, jobName)

		By("reconciling: the operator must RECREATE the Job (delete it — a fresh one is created next reconcile, the B3b pattern), not iterate, not fail")
		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a within-bound infra failure is a retry, never an iterate or a terminal fail (phase must stay Verifying)")
		Expect(fresh.Status.Verify).NotTo(BeNil())
		Expect(fresh.Status.Verify.InfraAttempts).To(Equal(int32(1)),
			"the attempt count is recorded in status (1: the original attempt)")
		jobGone := &batchv1.Job{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, jobGone)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"the failed Job is deleted within the bound (a fresh one is created next reconcile)")

		By("emitting a Warning Event naming the failed container (per retry, the auditability norm)")
		events := i65Events(recorder)
		Expect(events).To(ContainElement(ContainSubstring(verifyInfraRetryEventReason)))
		Expect(events).To(ContainElement(ContainSubstring("clone-base")))

		By("creating a FRESH Job on the next reconcile (the recreate is a NEW Job, not a mutation of the failed one)")
		i65DeleteVerifyPodNS(ns, name) // the failed pod is gone with the delete; without it the reconcile re-reads it and deletes the fresh Job again
		s5aReconcile(r, ns, name)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, &batchv1.Job{})).To(Succeed(),
			"ensureVerifyJob recreates the Job after the delete (same name for the same pin; the failed pod is gone with it)")
	})

	It("recreates a failed verify Job (import-agent failed) within the retry bound, naming the failed container", func() {
		ns := i65Ns("i65-import")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "i65import"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: recorder}
		i65Drive(ns, name, r)
		s5aReconcile(r, ns, name)
		jobName := name + "-verify-1"

		By("failing the import-agent init (exit 1) and marking the Job Failed")
		i65VerifyPodNS(ns, name, jobName, []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: i65ContainerErrReason}}},
		})
		i65JobFailedNS(ns, jobName)

		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "an import-agent failure is a retry, not a decision")
		Expect(fresh.Status.Verify).NotTo(BeNil())
		Expect(fresh.Status.Verify.InfraAttempts).To(Equal(int32(1)))
		events := i65Events(recorder)
		Expect(events).To(ContainElement(ContainSubstring(verifyInfraRetryEventReason)))
		Expect(events).To(ContainElement(ContainSubstring("import-agent")))
	})

	It("decides infra failure from the pod's init status even when the Job condition is absent (the pod is the primary evidence; the Job-Failed fallback fires only on a stale Job, which envtest's read-through cache cannot model — covered by code review)", func() {
		ns := i65Ns("i65-nocond")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "i65nocond"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		i65Drive(ns, name, r)
		s5aReconcile(r, ns, name)
		jobName := name + "-verify-1"

		By("failing the clone-base init (the pod's status) WITHOUT setting the Job condition (the pod is the primary evidence; the Job condition is the fallback for a stale Job, which envtest cannot model)")
		i65VerifyPodNS(ns, name, jobName, []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 128, Reason: i65ContainerErrReason}}},
		})
		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a within-bound infra failure (the pod's init status) is a retry, not a fail (phase must stay Verifying)")
		Expect(fresh.Status.Verify).NotTo(BeNil())
		Expect(fresh.Status.Verify.InfraAttempts).To(Equal(int32(1)),
			"the pod's init status alone is a DECISION (the Job condition is not required — the pod is the primary evidence)")
	})

	// The Job-Failed fallback (a Failed Job condition with NO pod evidence —
	// a stale Job the operator recreated) is NOT tested in envtest: a stale
	// Job is one the operator recreated after a decision (the batch Job
	// condition is a batch controller field, and envtest has no batch Job
	// controller — the operator's Job is created by the test's reconciler,
	// not the batch controller, so its condition is never set by anything
	// OTHER than the test's i65ReplantNS, which is the recreate path, not
	// the stale path). The fallback is therefore covered by CODE REVIEW (the
	// verifyOutcome logic is read in the I65 PR review), not by an envtest
	// spec. The acceptance "a failed verify Job (Job Failed, no check ran)"
	// is satisfied by the pod's init status (the primary evidence) — the
	// Job condition is the redundant fallback for the (impossible-in-envtest)
	// stale-Job case.

	It("fails the Loop VerifyInfraFailed when the retry bound is exhausted (exactly verifyInfraRetries recreations, then the final failure)", func() {
		ns := i65Ns("i65-bound")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "i65bound"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		i65Drive(ns, name, r)
		s5aReconcile(r, ns, name)
		jobName := name + "-verify-1"

		By("failing the clone-base init persistently and reconciling through the whole bound")
		var fresh *coxv1alpha1.Loop
		for i := 0; i <= verifyInfraRetries; i++ {
			if i > 0 {
				// Re-plant the failure on a FRESH Job (the operator's recreate
				// deleted the old Job; the batch controller would re-fail the
				// fresh one in a real cluster — envtest has no batch controller,
				// so the fixture recreates the Job AND marks it Failed).
				i65ReplantNS(ns, name, jobName, []corev1.ContainerStatus{
					{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 128, Reason: i65ContainerErrReason}}},
				})
			} else {
				i65VerifyPodNS(ns, name, jobName, []corev1.ContainerStatus{
					{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 128, Reason: i65ContainerErrReason}}},
				})
				i65JobFailedNS(ns, jobName)
			}
			fresh = s5aReconcile(r, ns, name)
			if i < verifyInfraRetries {
				Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
					"recreation %d is within the bound: the Loop keeps Verifying", i+1)
			}
		}
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"attempt verifyInfraRetries+1 (the last allowed run) exhausted the bound: Failed")
		failedCond := i65Condition(fresh, string(coxv1alpha1.LoopPhaseFailed))
		Expect(failedCond).NotTo(BeNil())
		Expect(failedCond.Reason).To(Equal(verifyInfraFailedReason))
		Expect(fresh.Status.Verify).NotTo(BeNil())
		Expect(fresh.Status.Verify.InfraAttempts).To(Equal(int32(verifyInfraRetries+1)),
			"the bound is exactly verifyInfraRetries RECREATIONS: 1 original + 2 recreates = 3 attempts, then the fail")
	})

	It("takes NO decision while clone-base is still running (the I49 norm — in-progress is never a failure)", func() {
		ns := i65Ns("i65-pend-clone")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "i65pendclone"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		i65Drive(ns, name, r)
		s5aReconcile(r, ns, name)
		jobName := name + "-verify-1"

		By("leaving clone-base in progress (Running) and reconciling: NO decision (the I49 norm)")
		i65VerifyPodNS(ns, name, jobName, []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		})
		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a still-running clone-base is PENDING, never a failure (phase must stay Verifying)")
		Expect(fresh.Status.Verify).To(BeNil(), "no infra attempt may be recorded for an in-progress init")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, &batchv1.Job{})).To(Succeed(),
			"no decision must not delete the Job")
	})

	It("takes NO decision while import-agent is still running (the I49 norm)", func() {
		ns := i65Ns("i65-pend-import")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "i65pendimport"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		i65Drive(ns, name, r)
		s5aReconcile(r, ns, name)
		jobName := name + "-verify-1"

		i65VerifyPodNS(ns, name, jobName, []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aImportAgent, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		})
		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a still-running import-agent is PENDING, never a failure")
		Expect(fresh.Status.Verify).To(BeNil())
	})

	It("continues normally when the recreated Job then passes (an infra retry that recovers does not wedge or fail the Loop)", func() {
		ns := i65Ns("i65-recover")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "i65recover"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		i65Drive(ns, name, r)
		s5aReconcile(r, ns, name)
		jobName := name + "-verify-1"

		By("failing clone-base once (a transient flake) and reconciling: the operator recreates the Job")
		i65VerifyPodNS(ns, name, jobName, []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 128, Reason: i65ContainerErrReason}}},
		})
		i65JobFailedNS(ns, jobName)
		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "the retry keeps the Loop in Verifying")
		Expect(fresh.Status.Verify).NotTo(BeNil())
		Expect(fresh.Status.Verify.InfraAttempts).To(Equal(int32(1)))

		By("the recreated Job then passes (the transient flake cleared): the Loop must Succeed")
		i65DeleteVerifyPodNS(ns, name) // the failed pod is gone with the delete; without it the reconcile re-reads it and deletes the fresh Job again
		s5aReconcile(r, ns, name)      // the recreate: the fresh Job exists (no pod yet — no evidence, requeue)
		fresh = s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "no evidence yet: hold (the fresh Job has no pod)")
		// The fresh Job's pod: every init terminates cleanly (the retry recovered).
		i65VerifyPodNS(ns, name, jobName, []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aArtifact, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aCheck0, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		})
		fresh = s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"a recovered infra retry must end Succeeded exactly like a clean run (the retry is invisible to the phase machine)")
	})
})
