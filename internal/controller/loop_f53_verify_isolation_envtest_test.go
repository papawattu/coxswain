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

// F53: two ADR-vs-code gaps in the verify Job (ADR-0005 D19 and D12/D17).
//
// (a) The verified checkout (/verify) was mounted WRITABLE into every check
// container. Init containers run in order, so check-0 could rewrite a
// *_test.go that check-1 compiles — the exact "check 2 compiles doctored
// tests" attack D19 rules out unconditionally — and one shared check-tmp
// emptyDir let check-0 poison check-1's HOME/GOCACHE/TMPDIR. The fix mounts
// /verify read-only in the check containers (the artifact gate, an INIT that
// writes to /verify, keeps its writable mount) and gives each check its own
// scratch (check-tmp-<i>), plus PYTHONPYCACHEPREFIX so a `compileall` check
// keeps working against a read-only tree.
//
// (b) The verify Job had no activeDeadlineSeconds (unlike its two sibling
// Jobs), so a check that never terminates held the Loop in Verifying for
// ever with no condition, no Event and no bound. The fix bounds the Job and
// maps a deadline kill to VerifyInfraFailed — decided from the Job's own
// terminal condition, so a stale Waiting/Running init in the raced pod read
// cannot mean "still working" (the I49 rule is about genuine progress).
//
// The template specs build the JobSpec in memory (buildVerifyJobSpec is a
// pure function of the Loop + reconciler config); the decision specs drive a
// real Loop through Reconcile, with the Job's batch conditions and the pod's
// init statuses stamped by the fixture (envtest has no batch Job controller).
package controller

import (
	"context"
	"strings"

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

var _ = Describe("F53: the verify Job isolates the checks and is time-bounded", func() {
	ctx := context.Background()

	// f53JobSpec builds the verify JobSpec for a Loop with the given checks
	// (in memory — no cluster). The reconciler is zero-valued: the fixture
	// repo is in-cluster (repoPeer non-nil), so verifyNeedsProxy is false and
	// no proxy/config seam is read.
	f53JobSpec := func(checks ...string) batchv1.JobSpec {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "f53loop", Namespace: "default"},
			Spec:       s5aLoopSpec(),
		}
		loop.Spec.Verify.AcceptanceChecks = checks
		loop.Status.BaseCommit = s5aBaseCommit
		loop.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: s5aHeadCommit}
		r := &LoopReconciler{}
		return r.buildVerifyJobSpec(loop)
	}

	// f53Mount returns the named volume mount on a container (nil when absent).
	f53Mount := func(ct corev1.Container, name string) *corev1.VolumeMount {
		for i := range ct.VolumeMounts {
			if ct.VolumeMounts[i].Name == name {
				return &ct.VolumeMounts[i]
			}
		}
		return nil
	}

	It("mounts the verified checkout read-only in every check container (D19) and keeps the artifact gate's writable mount", func() {
		spec := f53JobSpec("go build -o /tmp/app ./...", "go test ./...")
		var checks, trusted int
		for _, ct := range spec.Template.Spec.InitContainers {
			if strings.HasPrefix(ct.Name, "check-") {
				checks++
				m := f53Mount(ct, verifyVol)
				Expect(m).NotTo(BeNil(), "check %s must mount the checkout /verify", ct.Name)
				Expect(m.MountPath).To(Equal(verifyScratchPath))
				Expect(m.ReadOnly).To(BeTrue(),
					"check %s must NOT be able to write the verified checkout (D19): check-0 could otherwise rewrite the test check-1 compiles", ct.Name)
				continue
			}
			trusted++
			m := f53Mount(ct, verifyVol)
			Expect(m).NotTo(BeNil(), "trusted init %s must mount /verify", ct.Name)
			Expect(m.ReadOnly).To(BeFalse(),
				"trusted init %s writes /verify (the clone, the import, the artifact gate's mktemp) and must keep the writable mount", ct.Name)
		}
		Expect(checks).To(Equal(2))
		Expect(trusted).To(BeNumerically(">=", 4),
			"clone-base, import-agent, tamper and artifact are the trusted inits")
	})

	It("gives each check container its OWN /tmp scratch (D19), so one check cannot poison the next", func() {
		spec := f53JobSpec("echo c0", "echo c1", "echo c2")
		seen := map[string]int{}
		idx := 0
		for _, ct := range spec.Template.Spec.InitContainers {
			if !strings.HasPrefix(ct.Name, "check-") {
				continue
			}
			wantVol := verifyCheckTmpVolName(idx)
			idx++
			var got []string
			for _, m := range ct.VolumeMounts {
				if m.MountPath == agentTmpMount {
					got = append(got, m.Name)
					Expect(m.ReadOnly).To(BeFalse(),
						"check %s's scratch at /tmp must be writable", ct.Name)
				}
			}
			Expect(got).To(Equal([]string{wantVol}),
				"check %s must mount exactly its own %s at /tmp", ct.Name, wantVol)
			seen[wantVol]++
		}
		Expect(idx).To(Equal(3))
		for vol, n := range seen {
			Expect(n).To(Equal(1), "volume %s is mounted by %d checks — each check needs its own scratch", vol, n)
		}
		// Every per-check scratch is a declared emptyDir on the pod.
		declared := map[string]bool{}
		for _, v := range spec.Template.Spec.Volumes {
			if v.EmptyDir != nil {
				declared[v.Name] = true
			}
		}
		for vol := range seen {
			Expect(declared[vol]).To(BeTrue(), "the pod must declare the %s emptyDir", vol)
		}
	})

	It("keeps the check tee on the writable scratch, not on the read-only checkout", func() {
		// checkTeed writes the check's output to the termination-message path so
		// the operator can read a failing check's output without a pod-log read.
		// If that path were under the (now read-only) checkout, every check
		// would fail on its own tee — assert the invariant, not just the value.
		Expect(strings.HasPrefix(verifyTerminationLogPath, verifyScratchPath+"/")).To(BeFalse(),
			"the termination-log path (%s) must not be under the read-only checkout (%s)",
			verifyTerminationLogPath, verifyScratchPath)
		Expect(strings.HasPrefix(verifyTerminationLogPath, agentTmpMount+"/")).To(BeTrue(),
			"the termination-log path must live on the writable scratch")
		spec := f53JobSpec("echo c0")
		for _, ct := range spec.Template.Spec.InitContainers {
			if !strings.HasPrefix(ct.Name, "check-") {
				continue
			}
			Expect(ct.TerminationMessagePath).To(Equal(verifyTerminationLogPath))
		}
	})

	It("carries activeDeadlineSeconds so a hung check cannot hold Verifying for ever (D12/D17)", func() {
		spec := f53JobSpec("sleep 100000")
		Expect(spec.ActiveDeadlineSeconds).NotTo(BeNil(),
			"the verify Job must bound itself like the deliver and memory-writer Jobs")
		Expect(*spec.ActiveDeadlineSeconds).To(Equal(verifyJobActiveDeadlineSeconds))
		Expect(verifyJobActiveDeadlineSeconds).To(Equal(int64(900)),
			"the shipped default is 15 minutes")
	})

	// The decision specs: envtest helpers (the i65 fixture shape).
	f53Ns := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	f53GetJob := func(ns, jobName string) *batchv1.Job {
		job := &batchv1.Job{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			Fail("get verify job: " + err.Error())
		}
		return job
	}

	f53PlantPod := func(ns, name, jobName string, job *batchv1.Job, inits []corev1.ContainerStatus) {
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
		existing := &corev1.Pod{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: ns}, existing); err == nil {
			Expect(k8sClient.Delete(ctx, existing)).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.InitContainerStatuses = inits
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	// f53JobFailed stamps the Job's batch conditions, with the given Reason.
	// K8s 1.34+ requires JobFailureTarget alongside JobFailed on a status
	// update (the batch controller sets both; envtest has none).
	f53JobFailed := func(ns, jobName, reason string) {
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())
		if job.Status.StartTime == nil {
			now := metav1.Now()
			job.Status.StartTime = &now
		}
		job.Status.Conditions = []batchv1.JobCondition{{
			Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue,
			Reason: reason, LastProbeTime: metav1.Now(),
		}, {
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: reason, Message: "job failed",
			LastProbeTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
	}

	f53Events := func(recorder *record.FakeRecorder) []string {
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

	// f53WaitingClone is the pod status of a verify pod whose clone-base has
	// not terminated (the raced read a deadline kill can leave behind).
	f53WaitingClone := []corev1.ContainerStatus{{
		Name:  s5aCloneBase,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
	}}

	It("decides an infra failure for a deadline-killed Job even while the pod still shows a Waiting init, and retries within the bound", func() {
		ns := f53Ns("f53-deadline")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f53deadline"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: recorder}
		s5aDriveToVerifyingWithReconciler(r, ns, name)
		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name}})
		job := f53GetJob(ns, jobName)
		Expect(job).NotTo(BeNil(), "the verify Job exists after the drive")

		f53PlantPod(ns, name, jobName, job, f53WaitingClone)
		f53JobFailed(ns, jobName, batchv1.JobReasonDeadlineExceeded)

		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"the first deadline kill is a bounded retry, not a terminal failure")
		Expect(fresh.Status.Verify).NotTo(BeNil())
		Expect(fresh.Status.Verify.InfraAttempts).To(Equal(int32(1)),
			"the deadline kill is counted as an infra attempt")
		Expect(f53GetJob(ns, jobName)).To(BeNil(),
			"the deadline-killed Job is deleted so a fresh one is created")
		events := f53Events(recorder)
		Expect(events).To(ContainElement(ContainSubstring(verifyInfraRetryEventReason)))
		Expect(events).To(ContainElement(ContainSubstring("activeDeadlineSeconds")),
			"the Event must name the deadline, so the operator knows the check hung")
	})

	It("decides an infra failure when the deadline kills the Job mid-check (check-0 Running)", func() {
		ns := f53Ns("f53-midcheck")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f53midcheck"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: recorder}
		s5aDriveToVerifyingWithReconciler(r, ns, name)
		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name}})
		job := f53GetJob(ns, jobName)
		Expect(job).NotTo(BeNil())

		f53PlantPod(ns, name, jobName, job, []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: verifyTamperInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: verifyArtifactInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			// The motivating shape: a check that never terminates.
			{Name: "check-0", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		})
		f53JobFailed(ns, jobName, batchv1.JobReasonDeadlineExceeded)

		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(fresh.Status.Verify).NotTo(BeNil())
		Expect(fresh.Status.Verify.InfraAttempts).To(Equal(int32(1)),
			"a hung check that the deadline killed is an infra failure, never an iterate (the agent's work was never judged)")
		Expect(f53GetJob(ns, jobName)).To(BeNil())
	})

	It("fails the Loop with VerifyInfraFailed, naming the deadline, once the bound is exhausted", func() {
		ns := f53Ns("f53-bound")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f53bound"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: recorder}
		s5aDriveToVerifyingWithReconciler(r, ns, name)
		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name}})
		job := f53GetJob(ns, jobName)
		Expect(job).NotTo(BeNil())

		// Seed the attempt count at the bound: the next counted attempt is
		// verifyInfraRetries+1, the last allowed run failing.
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		if loop.Status.Verify == nil {
			loop.Status.Verify = &coxv1alpha1.VerifyStatus{}
		}
		loop.Status.Verify.InfraAttempts = verifyInfraRetries
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		f53PlantPod(ns, name, jobName, job, f53WaitingClone)
		f53JobFailed(ns, jobName, batchv1.JobReasonDeadlineExceeded)

		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed))
		var cond *metav1.Condition
		for i := range fresh.Status.Conditions {
			if fresh.Status.Conditions[i].Type == string(coxv1alpha1.LoopPhaseFailed) {
				cond = &fresh.Status.Conditions[i]
			}
		}
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(verifyInfraFailedReason))
		Expect(cond.Message).To(ContainSubstring("activeDeadlineSeconds"))
		Expect(cond.Message).To(ContainSubstring("900"))
	})

	It("decides an infra failure for a deadline-killed Job with NO pod at all (the pod is GC'd or evicted first)", func() {
		// The deadline decision is read from the Job's own terminal condition, so
		// it does not need the pod, and it must not depend on the caller also
		// setting jobFailed (both come from the same JobFailed condition, but the
		// decision should stand on its own).
		outcome, requeue, _, _ := verifyOutcome(nil, 1, false, true)
		Expect(outcome).To(Equal(verifyInfraFailed))
		Expect(requeue).To(BeFalse())
		// Control: no pod and no Job failure is still no decision.
		outcome, requeue, _, _ = verifyOutcome(nil, 1, false, false)
		Expect(outcome).To(Equal(verifyNoDecision))
		Expect(requeue).To(BeTrue())
	})

	// pi3 prereview-306 note 5: the same decision driven through Reconcile with
	// NO pod in the API — the shape a pod GC'd (or evicted) between the kill and
	// the operator's read actually hits. The spec above pins the helper's half
	// only; this one pins the call site (assert the decision Reconcile makes).
	It("retries a deadline-killed Job whose pod is GONE (no pod to read, Reconcile-driven)", func() {
		ns := f53Ns("f53-nopod")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f53nopod"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: recorder}
		s5aDriveToVerifyingWithReconciler(r, ns, name)
		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name}})
		Expect(f53GetJob(ns, jobName)).NotTo(BeNil(), "the verify Job exists after the drive")

		// Deliberately NO f53PlantPod: the pod is already gone.
		f53JobFailed(ns, jobName, batchv1.JobReasonDeadlineExceeded)

		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a deadline kill is a bounded retry, not a terminal failure")
		Expect(fresh.Status.Verify).NotTo(BeNil())
		Expect(fresh.Status.Verify.InfraAttempts).To(Equal(int32(1)),
			"the deadline kill is an infra attempt even with no pod to read")
		Expect(f53GetJob(ns, jobName)).To(BeNil(),
			"the deadline-killed Job is deleted so a fresh one is created")
		// This assertion is what pins the deadline arm ABOVE the pod == nil
		// branch in the no-pod shape (pi3 early-344 finding 3): with the arm moved
		// below that branch — or deleted — the Job-level "no check init
		// terminated" path still decides verifyInfraFailed and every other
		// assertion here holds, but the Event then carries the generic message and
		// no deadline. The deadline is the operator-facing fact (a check hung, not
		// the agent's work), so the Event must name it.
		events := f53Events(recorder)
		Expect(events).To(ContainElement(ContainSubstring(verifyInfraRetryEventReason)))
		Expect(events).To(ContainElement(ContainSubstring("activeDeadlineSeconds")),
			"the retry Event must name the deadline, not the generic no-check-init failure")
	})

	It("does NOT decide when the Job failed for another reason and the pod is still starting (the I49 control)", func() {
		ns := f53Ns("f53-control")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f53control"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: recorder}
		s5aDriveToVerifyingWithReconciler(r, ns, name)
		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name}})
		job := f53GetJob(ns, jobName)
		Expect(job).NotTo(BeNil())

		// A backoff-limit failure with an init still Waiting is NOT a deadline
		// kill: the I49 rule keeps it a no-decision (the pod may still settle).
		f53PlantPod(ns, name, jobName, job, f53WaitingClone)
		f53JobFailed(ns, jobName, "BackoffLimitExceeded")

		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a non-deadline JobFailed with a Waiting init decides nothing")
		if fresh.Status.Verify != nil {
			Expect(fresh.Status.Verify.InfraAttempts).To(Equal(int32(0)),
				"no infra attempt may be burned while the pod is still starting")
		}
		for _, e := range f53Events(recorder) {
			Expect(e).NotTo(ContainSubstring(verifyInfraRetryEventReason),
				"no retry Event may fire for a no-decision reconcile")
		}
	})
})
