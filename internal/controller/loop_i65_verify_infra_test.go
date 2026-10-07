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
// P1-A (REVIEW-PHASE1-R23): the operator's infra-retry deletes the failed Job
// (background propagation) and recreates it with the SAME NAME on the next
// reconcile; on a real cluster the old Job's pod lingers until the GC removes
// it. readVerifyJobPod filters to the current Job's UID (not just its name)
// so the stale pod is not re-read, and applyVerifyInfraFailure counts at most
// ONE infra attempt per Job UID (status.verify.infraJobUID) so a re-read of
// the same failing Job does not burn another attempt (one flake burning every
// retry).
//
// P1-B (REVIEW-PHASE1-R23): infraAttempts lives on status.verify and is reset
// by a new pin (status.currentVerify), which the Implementing -> Verifying
// advance rebuilds. Infra flakes therefore do NOT accumulate across pins:
// pin A's exhausted retries do not doom pin B.
//
// The envtest fixtures set pod init statuses with Status().Update AFTER
// Create (the API drops status on create), drive the Job's batch conditions
// directly (envtest has no batch Job controller), and set the pod's
// ownerReferences (the batch controller sets them; envtest has no Job
// controller, so the fixture models the owning-Job UID so readVerifyJobPod's
// UID filter selects the pod).
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

	// i65GetJob returns the named Job (the test's read of the operator-created
	// Job; the caller checks IsNotFound when the Job is deleted by the
	// operator's infra-retry).
	i65GetJob := func(ns, jobName string) *batchv1.Job {
		job := &batchv1.Job{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			Fail("get verify job: " + err.Error())
			return nil
		}
		return job
	}

	// i65PlantPod plants a stand-in verify Job pod for jobName with the given
	// init statuses, owned by the Job (the ownerReferences carry the Job's
	// UID so readVerifyJobPod's UID filter selects it). The pod's name is
	// <name>-verify-pod (the s5aClaimPod oneShotRun idiom: delete-then-create
	// so a re-plant is fresh). The pod's job-name label equals jobName (the
	// batch controller sets it; the fixture models it).
	i65PlantPod := func(ns, name, jobName string, job *batchv1.Job, inits []corev1.ContainerStatus) {
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
		// envtest has no batch controller, so the fixture sets both).
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

	It("recreates a failed verify Job (clone-base failed) within the retry bound — no decision burned, no iterate, an Event per retry — and the stale pod is not re-counted (P1-A)", func() {
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
		firstJob := i65GetJob(ns, jobName)
		Expect(firstJob).NotTo(BeNil(), "the verify Job exists after the drive")

		By("failing the clone-base init (the P2h G2 DNS flake: exit 128) and marking the Job Failed")
		i65PlantPod(ns, name, jobName, firstJob, []corev1.ContainerStatus{
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
		Expect(string(fresh.Status.Verify.InfraJobUID)).To(Equal(string(firstJob.UID)),
			"P1-A: the counted Job UID is recorded (the one-attempt-per-UID guard)")
		jobGone := i65GetJob(ns, jobName)
		Expect(jobGone).To(BeNil(), "the failed Job is deleted within the bound (a fresh one is created next reconcile)")

		By("emitting a Warning Event naming the failed container (per retry, the auditability norm)")
		events := i65Events(recorder)
		Expect(events).To(ContainElement(ContainSubstring(verifyInfraRetryEventReason)))
		Expect(events).To(ContainElement(ContainSubstring("clone-base")))

		By("reconciling several more times WITHOUT deleting the old pod (P1-A: the GC has not run; the stale pod lingers): the new Job is created, the stale pod is not re-read (the UID filter excludes it), and infraAttempts stays 1 (the one-attempt-per-UID guard)")
		// The old pod (owned by the first Job's UID) is still present — the
		// test does NOT delete it (the manual delete was the P1-A bug: it hid
		// the stale-pod re-read). The operator's ensureVerifyJob creates a
		// fresh Job (a new UID); readVerifyJobPod filters to the new Job's
		// UID, so the stale pod (the first Job's UID) is not re-read.
		for i := 0; i < 4; i++ {
			again := s5aReconcile(r, ns, name)
			Expect(again.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
				"P1-A: the stale pod must not re-count an attempt (phase stays Verifying, no failure)")
			Expect(again.Status.Verify).NotTo(BeNil())
			Expect(again.Status.Verify.InfraAttempts).To(Equal(int32(1)),
				"P1-A: the one-attempt-per-UID guard — the stale pod (the old Job's UID) is not re-read; infraAttempts stays 1 across reconciles with the old pod still present")
		}
		// The fresh Job (the retry) exists (ensureVerifyJob created it after
		// the delete; the name is the same, the UID is new).
		retryJob := i65GetJob(ns, jobName)
		Expect(retryJob).NotTo(BeNil(), "the retried Job exists (a fresh one, a new UID)")
		Expect(retryJob.UID).NotTo(Equal(firstJob.UID), "the retried Job has a new UID (a NEW Job, not a mutation of the failed one)")
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
		firstJob := i65GetJob(ns, jobName)
		Expect(firstJob).NotTo(BeNil())

		By("failing the import-agent init and marking the Job Failed")
		i65PlantPod(ns, name, jobName, firstJob, []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: i65ContainerErrReason, Message: "import-agent: no such file or directory"}}},
		})
		i65JobFailedNS(ns, jobName)

		By("reconciling: the operator recreates the Job and names import-agent in the Event")
		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(fresh.Status.Verify).NotTo(BeNil())
		Expect(fresh.Status.Verify.InfraAttempts).To(Equal(int32(1)))
		events := i65Events(recorder)
		Expect(events).To(ContainElement(ContainSubstring("import-agent")))
	})

	It("fails the Loop with VerifyInfraFailed after the retry bound is exhausted", func() {
		ns := i65Ns("i65-exhaust")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "i65exhaust"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: recorder}
		i65Drive(ns, name, r)
		s5aReconcile(r, ns, name)
		jobName := name + "-verify-1"
		inits := []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 128, Reason: i65ContainerErrReason}}},
		}
		attempts := int32(0)
		for attempts <= verifyInfraRetries {
			// Each cycle: fail the current Job's pod + mark the Job Failed
			// (a persistent infra failure — the batch controller would
			// re-fail the recreated Job in a real cluster), reconcile
			// (the operator deletes it and counts the attempt), then the
			// next reconcile recreates the Job (a new UID) which the fixture
			// re-fails. The first iteration may have no Job yet (created in
			// the same reconcile as the first fail — envtest has no Job
			// controller, so the fixture models the Job + pod together).
			job := i65GetJob(ns, jobName)
			if job == nil {
				// The Job is not created yet (the operator's ensureVerifyJob
				// creates it in the same reconcile as the first infra-fail
				// decision — the fixture models it by creating the Job + pod
				// together). Reconcile to trigger the create, then loop back.
				s5aReconcile(r, ns, name)
				continue
			}
			i65PlantPod(ns, name, jobName, job, inits)
			i65JobFailedNS(ns, jobName)
			fresh := s5aReconcile(r, ns, name)
			attempts = fresh.Status.Verify.InfraAttempts
			if fresh.Status.Phase == coxv1alpha1.LoopPhaseFailed {
				break
			}
			// The operator deleted the Job; the next reconcile creates a
			// fresh one (a new UID). Reconcile to trigger the create.
			s5aReconcile(r, ns, name)
		}
		Expect(attempts).To(Equal(int32(verifyInfraRetries+1)),
			"the bound is verifyInfraRetries recreations; attempt verifyInfraRetries+1 is the last allowed run")

		By("the Loop is Failed with the DISTINCT reason VerifyInfraFailed (not MaxIterationsExceeded — an infra failure is not the agent's work)")
		final := s5aReconcile(r, ns, name)
		Expect(final.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed))
		cond := i65Condition(final, "Failed")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(verifyInfraFailedReason),
			"the Failed condition names the distinct VerifyInfraFailed reason (the operator's own infra failure, not the agent's work)")
		Expect(final.Status.Verify).NotTo(BeNil())
		Expect(final.Status.Verify.InfraAttempts).To(Equal(int32(verifyInfraRetries + 1)))

		By("emitting a final Warning Event (the exhaustion is observable — the wedge that I65 fixes is visible)")
		events := i65Events(recorder)
		Expect(events).To(ContainElement(ContainSubstring(verifyInfraFinalEventReason)))
	})

	It("treats a still-running trusted init as NO decision (the I49 norm: in-progress is never a failure)", func() {
		ns := i65Ns("i65-running")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "i65running"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		i65Drive(ns, name, r)
		s5aReconcile(r, ns, name)
		jobName := name + "-verify-1"
		job := i65GetJob(ns, jobName)
		Expect(job).NotTo(BeNil())

		By("planting the pod with clone-base still Running (the evidence inits cannot have started)")
		i65PlantPod(ns, name, jobName, job, []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		})

		By("reconciling: NO decision (the Loop stays Verifying, no attempt is counted, no Event, no failure)")
		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a still-running trusted init is NO decision (the I49 norm: in-progress is never a failure — no recreate, no fail, no iterate)")
		if fresh.Status.Verify != nil {
			Expect(fresh.Status.Verify.InfraAttempts).To(BeZero(),
				"a still-running init must not count an attempt (the Job has not failed yet)")
		}
		Expect(i65Condition(fresh, "Failed")).To(BeNil(), "no Failed condition for a still-running init")
	})

	It("treats a pending (Waiting) trusted init as NO decision", func() {
		ns := i65Ns("i65-waiting")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "i65waiting"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		i65Drive(ns, name, r)
		s5aReconcile(r, ns, name)
		jobName := name + "-verify-1"
		job := i65GetJob(ns, jobName)
		Expect(job).NotTo(BeNil())

		By("planting the pod with import-agent still Waiting (PodInitializing — clone-base has not finished)")
		i65PlantPod(ns, name, jobName, job, []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aImportAgent, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
		})

		By("reconciling: NO decision")
		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a pending (Waiting) trusted init is NO decision (in-progress is never a failure)")
		if fresh.Status.Verify != nil {
			Expect(fresh.Status.Verify.InfraAttempts).To(BeZero())
		}
	})

	It("resets the infra attempt count on a new pin (P1-B): pin A's flakes do not doom pin B", func() {
		ns := i65Ns("i65-pins")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "i65pins"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: recorder}
		i65Drive(ns, name, r)
		s5aReconcile(r, ns, name)
		jobName := name + "-verify-1"

		By("failing the verify Job twice on pin A (the original attempt + one retry; the bound is NOT yet exhausted — attempt 2 of verifyInfraRetries+1=3)")
		for fail := int32(1); fail <= 2; fail++ {
			job := i65GetJob(ns, jobName)
			Expect(job).NotTo(BeNil(), "the verify Job exists before the fail (attempt %d)", fail)
			i65PlantPod(ns, name, jobName, job, []corev1.ContainerStatus{
				{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 128, Reason: i65ContainerErrReason}}},
			})
			i65JobFailedNS(ns, jobName)
			fresh := s5aReconcile(r, ns, name)
			Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
				"pin A attempt %d is within the bound (a retry, not a fail)", fail)
			Expect(fresh.Status.Verify).NotTo(BeNil())
			Expect(fresh.Status.Verify.InfraAttempts).To(Equal(fail),
				"pin A's attempt count climbs (1, then 2)")
			// The operator deleted the Job; the next reconcile creates a
			// fresh one (a new UID). Reconcile to trigger the create.
			s5aReconcile(r, ns, name)
		}

		By("pin A advances to a new pin B (a verify-iterate — a check failed, NOT an infra failure — the iterate clears status.currentVerify and re-pins on the next advance)")
		// The Loop is still Verifying (pin A's retry Job is created but not
		// failed yet). To advance to pin B, the verify Job must ITERATE (a
		// check failed, not an infra failure). Plant the retry Job's pod with
		// a check init that failed (exit 1) — the operator's decision is an
		// iterate (Verifying -> Implementing, iteration+1), which clears
		// status.currentVerify. The next advance (Implementing -> Verifying)
		// re-pins a new verifiedCommit (pin B), which resets infraAttempts.
		job := i65GetJob(ns, jobName)
		Expect(job).NotTo(BeNil(), "the retry Job exists (pin A's fresh Job, a new UID)")
		// Plant the pod with a FAILED check (the tamper init passed, the
		// check-0 init failed — the operator's decision is an iterate, not
		// an infra failure).
		i65PlantPod(ns, name, jobName, job, []corev1.ContainerStatus{
			{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aArtifact, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aCheckName(0), State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: i65ContainerErrReason, Message: "check failed"}}},
		})
		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"a check failure (not an infra failure) is an ITERATE (Verifying -> Implementing, the verify-iterate) — not a retry, not a fail")

		By("pin B: a new advance (Implementing -> Verifying) re-pins a new verifiedCommit, which resets infraAttempts to 0")
		// The iterate cleared status.currentVerify and set Iteration+1. The
		// next Implementing claim (a new headCommit) advances to Verifying
		// and pins a NEW verifiedCommit (pin B), which resets the
		// per-verified-commit fields (infraAttempts).
		// the next Implementing claim (a new headCommit) advances to Verifying
		// and pins a NEW verifiedCommit (pin B), which resets the
		// per-verified-commit fields (infraAttempts). The claim's iteration
		// must match the current iteration (2, after the iterate bumped it
		// from 1 to 2), or the stale-iteration guard ignores it.
		s5aClaimPodWithIteration(ns, name, "Implementing", s5aHeadCommit2, 2)
		fresh = s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"the advance to Verifying (pin B) re-pins a new verifiedCommit")
		Expect(fresh.Status.CurrentVerify).NotTo(BeNil())
		Expect(fresh.Status.CurrentVerify.VerifiedCommit).To(Equal(s5aHeadCommit2),
			"pin B is a NEW verifiedCommit (a different SHA than pin A)")

		By("one infra failure on pin B gets a retry (NOT a fail — pin A's two flakes did not accumulate into pin B's bound)")
		jobName2 := name + "-verify-2" // iteration 2 (the iterate bumped it)
		jobB := i65GetJob(ns, jobName2)
		if jobB == nil {
			// The Job is not created yet (the name is taken by pin A's deleted
			// Job, or the reconcile has not run ensureVerifyJob). Reconcile to
			// trigger the create.
			fresh = s5aReconcile(r, ns, name)
			jobB = i65GetJob(ns, jobName2)
		}
		Expect(jobB).NotTo(BeNil(), "pin B's verify Job exists (a fresh one for the new pin)")
		i65PlantPod(ns, name, jobName2, jobB, []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 128, Reason: i65ContainerErrReason}}},
		})
		i65JobFailedNS(ns, jobName2)
		fresh = s5aReconcile(r, ns, name)
		// P1-B: the infra failure on pin B must be a RETRY (attempt 1 of pin
		// B's own bound), NOT a fail. The loop may be Verifying (retry
		// counted, Job deleted) or Failed (the bound was exhausted — but the
		// bound is verifyInfraRetries=2, so attempt 1 is within the bound and
		// the phase MUST be Verifying, not Failed).
		Expect(fresh.Status.Verify).NotTo(BeNil())
		Expect(fresh.Status.Verify.InfraAttempts).To(Equal(int32(1)),
			"P1-B: pin B's attempt count is 1 (fresh — pin A's count is reset by the new pin)")
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"P1-B: pin B's infra failure (attempt 1 of 3) is a RETRY, not a fail (pin A's two flakes did not accumulate into pin B's bound)")
	})
})

// s5aHeadCommit2 is a second head commit (pin B's verifiedCommit — a
// different SHA than s5aHeadCommit, so the new pin is a genuinely new
// verifiedCommit and the per-verified-commit fields reset). It must be a
// valid 40-hex SHA (the parsePhaseClaim validation).
const s5aHeadCommit2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// s5aCheckName returns the check-<k> init container name (the operator's
// check-inits are named check-0, check-1, ... in spec order).
func s5aCheckName(k int) string {
	return "check-" + string(rune('0'+k))
}
