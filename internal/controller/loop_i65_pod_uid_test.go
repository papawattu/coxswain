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

// I65 (REVIEW-PHASE1-R23, P1-A follow-up): the verify Job's pod is matched to
// the CURRENT Job's UID (ownerReferences), not just its name, and at most ONE
// infra attempt is counted per Job UID.
//
// The operator's infra-retry deletes the failed verify Job (background
// propagation) and ensureVerifyJob recreates it with the SAME NAME on the next
// reconcile. On a real cluster the old Job's pod lingers until the garbage
// collector removes it; a name-only pod match would read that stale failed pod
// again on every reconcile and burn another infra attempt (one transient flake
// could exhaust the whole retry bound before a single retried Job runs, and
// once the new pod exists the read errors until GC finishes). The batch Job
// controller stamps the owning Job's UID on each pod's ownerReferences, so
// matching the current Job's UID selects only the pod the current Job created
// (a recreated Job has a new UID; its pod is not created yet, so the read
// returns nil and the hold/recreate is clean). A pod with a deletionTimestamp
// is being removed by the GC and is skipped (its status is stale).
//
// The counter itself carries the guard: status.verify.infraJobUID records the
// UID of the Job whose infra-failure was most recently counted, and
// applyVerifyInfraFailure increments infraAttempts only when the failing Job's
// UID differs (a re-read of the SAME failing Job — the reconcile re-running
// before the delete completes — never double-counts).
//
// The specs here model the batch controller on a real cluster: each fixture
// pod's ownerReferences carry its Job's UID, and the specs that recreate the
// Job (the infra-retry) create a FRESH Job (a new UID) and re-plant the pod
// against the new Job.
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

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("I65: the verify Job's pod is matched to the current Job's UID (P1-A)", func() {
	ctx := context.Background()

	It("reads only the pod owned by the CURRENT Job's UID: a stale pod from a prior Job (same name, different UID) is not read", func() {
		ns := i65ansNs("i65ans-uid")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "i65ansuid"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		i65ansDrive(r, ns, name)
		s5aReconcile(r, ns, name) // the verify Job is created (the pin exists)
		jobName := name + "-verify-1"

		By("reading the operator's own Job pod (the happy path: the operator-created Job's pod, the batch-controller shape)")
		firstJob := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, firstJob)).To(Succeed())
		i65ansPlantPod(ns, name, jobName, firstJob, []corev1.ContainerStatus{
			{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aArtifact, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aCheck0, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		})
		By("reconciling: the passing evidence is read (a pod owned by the current Job's UID is read)")
		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"a pod owned by the current Job's UID is read (the happy path: the passing evidence decides)")

		By("re-seeding the Loop back to Verifying (the same pin, iteration 1), REMOVING the happy-path pod (the operator's advance deletes nothing here — the stale-pod shape needs an empty pod list to model the GC-lingering case), and planting a STALE pod: job-name label == the current Job's name, ownerReference = a PRIOR Job's UID")
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseVerifying
		loop.Status.Iteration = 1
		loop.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: s5aHeadCommit}
		loop.Status.Conditions = nil
		loop.Status.Verify = nil
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		// The prior Job (a different UID, a different name): its pod is the
		// STALE one the GC has not removed yet on a real cluster (the
		// infra-retry recreates the Job with the SAME NAME and a NEW UID; the
		// stale pod keeps the old ownerReference).
		priorJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:        jobName + "-prior",
				Namespace:   ns,
				Labels:      verifyJobLabels(name),
				Annotations: map[string]string{verifyCommitAnnotation: s5aHeadCommit},
			},
		}
		priorJob.Spec.Template.Spec.Containers = []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}}
		priorJob.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
		Expect(k8sClient.Create(ctx, priorJob)).To(Succeed())
		// The stale pod: job-name label == jobName (the SAME name as the
		// current Job's pod — a name-only match would read it), but the
		// ownerReference is the PRIOR Job's UID. The happy-path pod was
		// removed by the advance path's cleanup? No — the S5a happy-path
		// cleanup only deletes the verify Job's pod when it is the SAME
		// iteration's Job; here the Loop advanced to Succeeded (the pin
		// cleared), so nothing cleaned the pod. Delete it explicitly: the
		// stale-pod shape models the GC-lingering case where the ONLY pod
		// under the Job name is the stale one.
		oldPod := &corev1.Pod{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name + "-verify-pod", Namespace: ns}, oldPod); err == nil {
			Expect(k8sClient.Delete(ctx, oldPod)).To(Succeed())
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-verify-pod",
				Namespace: ns,
				Labels:    map[string]string{s5aJobNameLabel: jobName, verifyForLabel: name},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: batchv1.SchemeGroupVersion.String(),
					Kind:       "Job",
					Name:       jobName + "-prior",
					UID:        priorJob.UID,
				}},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 128, Reason: "Error"}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		By("reconciling: the stale pod (the prior Job's UID) is NOT read — no infra attempt is counted and the current Job is NOT deleted")
		fresh = s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"the stale pod is not read: the Loop stays Verifying (no infra-failure decision)")
		if fresh.Status.Verify != nil {
			Expect(fresh.Status.Verify.InfraAttempts).To(BeZero(),
				"the stale pod is not read: no infra attempt is counted")
		}
		currentJob := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, currentJob)).To(Succeed(),
			"the current Job is NOT deleted (a decision on the stale pod would have deleted it)")
	})

	It("counts at most ONE infra attempt per Job UID: a re-read of the same failing Job does not double-count", func() {
		ns := i65ansNs("i65ans-once")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "i65ansonce"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: recorder}
		i65ansDrive(r, ns, name)
		s5aReconcile(r, ns, name)
		jobName := name + "-verify-1"
		firstJob := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, firstJob)).To(Succeed())

		By("failing clone-base (exit 128, the DNS-flake shape) and marking the Job Failed")
		i65ansPlantPod(ns, name, jobName, firstJob, []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 128, Reason: "Error"}}},
		})
		i65ansJobFailed(ns, jobName)

		By("reconciling: the operator deletes the failed Job and counts attempt 1 (within the bound: a retry)")
		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a within-bound infra failure is a retry, never a fail (phase stays Verifying)")
		Expect(fresh.Status.Verify).NotTo(BeNil())
		Expect(fresh.Status.Verify.InfraAttempts).To(Equal(int32(1)),
			"the attempt count is 1 (the original attempt)")
		Expect(fresh.Status.Verify.InfraJobUID).To(Equal(string(firstJob.UID)),
			"the counted Job's UID is recorded (the one-attempt-per-UID guard)")

		By("reconciling several more times with the SAME failed pod still present (the GC has not run; the Job is gone, so the stale pod is not re-read at all): NO second attempt is counted and the retried Job is created")
		// The Job was deleted by the operator's infra-retry (background
		// propagation). The pod lingers (envtest has no GC — the real-cluster
		// case exactly). readVerifyJobPod filters to the current Job's UID:
		// the Job is gone, so the stale pod is not read (no double-count from
		// the read path). Even in the defensive case where the same failing
		// Job were re-read (the reconcile re-running before the delete
		// completes), applyVerifyInfraFailure's UID guard counts at most one
		// attempt per Job UID.
		for i := 0; i < 3; i++ {
			again := s5aReconcile(r, ns, name)
			Expect(again.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
				"re-read %d: the stale failed pod must not burn another attempt (phase stays Verifying)", i+1)
			if again.Status.Verify != nil {
				Expect(again.Status.Verify.InfraAttempts).To(Equal(int32(1)),
					// The read path (readVerifyJobPod's UID filter) excludes the stale
					// pod (its Job is gone), so no infra failure is read and the
					// count cannot move. (The defensive UID guard inside
						// applyVerifyInfraFailure covers the re-read BEFORE the delete
						// completes; the read-path filter is what runs here.)
					"re-read %d: infraAttempts stays 1 (the stale pod is not read)", i+1)
			}
		}
		retryJob := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, retryJob)).To(Succeed(),
			"the retried Job exists (a fresh one — the recreate is not wedged by the stale pod)")
		// NOTE: envtest's Job status validation (K8s 1.34+) does not accept the
		// fixture's JobFailed status re-set on a recreated Job (the
		// applyVerifyInfraFailure delete+recreate path is the one that recreates
		// it here — the Job was created by the operator, not the fixture), so
		// the "new UID" assertion is verified via the UID DIFFERENCE (a
		// delete+recreate necessarily produces a new UID).
		Expect(string(retryJob.UID)).NotTo(Equal(string(firstJob.UID)),
			"the retried Job has a new UID (a NEW Job, not a mutation of the failed one)")
	})
})

// i65ansNs creates a fresh namespace (the specs defer-delete it).
func i65ansNs(prefix string) string {
	ns := prefix + "-" + nowSuffix()
	Expect(k8sClient.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	return ns
}

// i65ansDrive builds the Loop in ns and drives it to Verifying with the given
// reconciler (the s5a drive; the specs pass their own reconciler built with a
// Recorder).
func i65ansDrive(r *LoopReconciler, ns, name string) {
	ctx := context.Background()
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

// i65ansPlantPod plants a verify Job pod for jobName with the given init
// statuses, owned by the Job (the ownerReferences carry the Job's UID — the
// batch controller sets them on a real cluster; envtest has no Job controller,
// so the fixture models them). The pod's name is <name>-verify-pod (the
// delete-then-create idiom: a re-plant is fresh).
func i65ansPlantPod(ns, name, jobName string, job *batchv1.Job, inits []corev1.ContainerStatus) {
	ctx := context.Background()
	nn := types.NamespacedName{Name: name + "-verify-pod", Namespace: ns}
	existing := &corev1.Pod{}
	if err := k8sClient.Get(ctx, nn, existing); err == nil {
		Expect(k8sClient.Delete(ctx, existing)).To(Succeed())
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-verify-pod",
			Namespace: ns,
			Labels:    map[string]string{s5aJobNameLabel: jobName, verifyForLabel: name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(),
				Kind:       "Job",
				Name:       jobName,
				UID:        job.UID,
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}}},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	pod.Status.InitContainerStatuses = inits
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

// i65ansJobFailed sets the Job's batch condition JobFailed=True on the named
// Job (envtest has no batch Job controller; the condition is the Job-level
// failure evidence the operator reads). The Job's status is set WHOLE (the
// K8s 1.34+ Job status validation: Failed=True requires a
// FailureTarget=true condition, and startTime is required on a Job status
// update in envtest).
func i65ansJobFailed(ns, jobName string) {
	ctx := context.Background()
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
		Reason: i65JobFailedReason, Message: "job backoff limit hit", LastProbeTime: metav1.Now(),
	}}
	Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
}
