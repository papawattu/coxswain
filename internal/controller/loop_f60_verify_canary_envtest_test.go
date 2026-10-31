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

// F60 (D17.1): the verify canary — the NEGATIVE CONTROL for in-process
// subversion. A subverted test binary (the round-8 "binary exits 0 regardless
// of tests" probe) would make every acceptance check pass; the canary is the
// control that catches it: it runs a generated Go test that is REQUIRED to
// fail, and a binary that exits 0 despite that failure is subversion.
//
// The shape spec asserts the Job the OPERATOR CREATED — the Loop is driven to
// Verifying and the Job read back — so it covers ensureVerifyJob's wiring, not
// only buildVerifyJobSpec. The decision specs plant the batch Job's pod init
// statuses (envtest has no batch Job controller or kubelet) and drive the same
// reconciles. The four specs:
//
//  1. container placement: the canary init sits BETWEEN artifact and check-0,
//     runs on the operator's built-in Go image (NOT spec.verify.image — a
//     control that runs in the user's image could be subverted by it, M10),
//     with its own bounded writable scratch (post-F53 /verify is read-only).
//  2. canary exit 43 with all checks 0 → Failed:SubvertedVerify + a Warning
//     Event + NO new verify Job (terminal, like TamperedVerify).
//  3. canary exit 44 → NO decision (the Loop stays Verifying) + exactly ONE
//     VerifyCanaryUnavailable Event across two reconciles (idempotent).
//  4. canary exit 45 → verifyInfraRetry: status.verifyInfraAttempts == 1 and
//     the Job is recreated (a bounded infra retry), NEVER a subversion verdict.
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
	"sigs.k8s.io/controller-runtime/pkg/client"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("F60: the verify canary (D17.1 negative control)", func() {
	ctx := context.Background()

	f60Ns := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	f60NewLoop := func(ns, name string, refine ...func(*coxv1alpha1.LoopSpec)) {
		existing := &coxv1alpha1.Loop{}
		if !apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, existing)) {
			return // idempotent: the Loop already exists (do not re-create it)
		}
		spec := s5aLoopSpec()
		for _, f := range refine {
			f(&spec)
		}
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       spec,
		})).To(Succeed())
	}

	f60Reconciler := func(recorder *record.FakeRecorder) *LoopReconciler {
		if recorder == nil {
			recorder = record.NewFakeRecorder(64)
		}
		return &LoopReconciler{
			CNIProber: testCNIProber(),
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			apiReader: k8sClient,
			Recorder:  recorder,
		}
	}

	f60GetJob := func(ns, jobName string) *batchv1.Job {
		job := &batchv1.Job{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			Fail("get verify job: " + err.Error())
		}
		return job
	}

	f60Events := func(recorder *record.FakeRecorder) []string {
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

	// f60DriveToVerifying drives a fresh Loop to Verifying and returns the
	// reconciler + the verify Job name, draining the drive's own Events so a
	// decision spec's event assertions are about ITS reconcile only.
	f60DriveToVerifying := func(r *LoopReconciler, ns, name string) string {
		f60NewLoop(ns, name)
		s5aDriveToVerifyingWithReconciler(r, ns, name)
		if recorder, ok := r.Recorder.(*record.FakeRecorder); ok {
			_ = f60Events(recorder)
		}
		return verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}

	// f60InitIndex returns the index of the named init in the pod's init list
	// (-1 when absent).
	f60InitIndex := func(podSpec corev1.PodSpec, name string) int {
		for i := range podSpec.InitContainers {
			if podSpec.InitContainers[i].Name == name {
				return i
			}
		}
		return -1
	}

	// f60VolumeSizeLimit returns the SizeLimit of the named emptyDir volume
	// ("" when the volume is absent or has no SizeLimit).
	f60VolumeSizeLimit := func(podSpec corev1.PodSpec, name string) string {
		for _, v := range podSpec.Volumes {
			if v.Name == name && v.EmptyDir != nil && v.EmptyDir.SizeLimit != nil {
				return v.EmptyDir.SizeLimit.String()
			}
		}
		return ""
	}

	// f60PlantPod creates the verify Job's pod with the given init statuses (the
	// envtest stand-in for the kubelet), mirroring F58's plant helper.
	f60PlantPod := func(ns, name, jobName string, job *batchv1.Job, statuses []corev1.ContainerStatus) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-verify-pod",
				Namespace: ns,
				Labels:    map[string]string{s5aJobNameLabel: jobName, verifyForLabel: name},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: batchv1.SchemeGroupVersion.String(),
					Kind:       i65JobKind,
					Name:       jobName,
					UID:        job.UID,
				}},
			},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: verifyCanaryInit, Image: verifyDefaultCheckImage}},
				Containers:     []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}},
			},
		}
		existing := &corev1.Pod{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: ns}, existing); err == nil {
			Expect(k8sClient.Delete(ctx, existing)).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.InitContainerStatuses = statuses
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	// f60TrustedPrefix is the trusted init prefix (clone-base, import-agent,
	// tamper, artifact) all exited 0, so verifyOutcome reaches the canary.
	f60TrustedPrefix := func() []corev1.ContainerStatus {
		return []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aArtifact, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		}
	}

	It("places the canary init between artifact and check-0, on the built-in Go image, with its own bounded scratch", func() {
		ns := f60Ns("f60-placement")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f60place"
		// A Loop that ALSO declares a spec.verify.image: the canary must NOT
		// use it (M10) — it stays on the operator's built-in Go image.
		f60NewLoop(ns, name, func(spec *coxv1alpha1.LoopSpec) {
			spec.Verify.Image = "evil.example/subverter:latest"
		})
		r := f60Reconciler(nil)
		jobName := f60DriveToVerifying(r, ns, name)

		job := f60GetJob(ns, jobName)
		Expect(job).NotTo(BeNil(), "the operator creates the verify Job on the Verifying advance")
		podSpec := job.Spec.Template.Spec

		// (a) placement: the canary sits between artifact and check-0.
		artifactIdx := f60InitIndex(podSpec, verifyArtifactInit)
		canaryIdx := f60InitIndex(podSpec, verifyCanaryInit)
		check0Idx := f60InitIndex(podSpec, s5aCheck0)
		Expect(artifactIdx).NotTo(Equal(-1), "the artifact init must be present")
		Expect(canaryIdx).NotTo(Equal(-1), "the canary init must be present (F60)")
		Expect(check0Idx).NotTo(Equal(-1), "check-0 must be present")
		Expect(canaryIdx).To(BeNumerically(">", artifactIdx),
			"the canary must run AFTER the artifact gate (evidence order: tamper -> artifact -> canary -> checks)")
		Expect(canaryIdx).To(BeNumerically("<", check0Idx),
			"the canary must run BEFORE check-0 (it is the gate to the acceptance checks)")

		// (b) image: the canary runs on the operator's built-in Go image, NOT
		// spec.verify.image (a control that runs in the user's image could be
		// subverted by it — M10).
		canary := &podSpec.InitContainers[canaryIdx]
		Expect(canary.Image).To(Equal(verifyDefaultCheckImage),
			"the canary must run on the operator's built-in Go image, not the user's spec.verify.image")
		Expect(canary.Image).NotTo(Equal("evil.example/subverter:latest"),
			"the canary must NOT use the user-controlled spec.verify.image (M10)")

		// (c) scratch: the canary carries its OWN bounded writable /tmp emptyDir
		// (post-F53 /verify is read-only) — a distinct volume from the checks'.
		var canaryTmp *corev1.VolumeMount
		for i := range canary.VolumeMounts {
			if canary.VolumeMounts[i].MountPath == agentTmpMount {
				canaryTmp = &canary.VolumeMounts[i]
			}
		}
		Expect(canaryTmp).NotTo(BeNil(), "the canary must mount its own writable /tmp scratch")
		Expect(f60VolumeSizeLimit(podSpec, verifyCanaryTmpVolName)).To(Equal(verifyCheckTmpSizeLimit),
			"the canary's scratch must be bounded (like the per-check scratch)")
	})

	It("fails the Loop with SubvertedVerify (terminal) when the canary exits 43 with all checks passing", func() {
		ns := f60Ns("f60-subverted")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f60subv"
		recorder := record.NewFakeRecorder(64)
		r := f60Reconciler(recorder)
		jobName := f60DriveToVerifying(r, ns, name)
		job := f60GetJob(ns, jobName)
		Expect(job).NotTo(BeNil())

		// The motivating shape: every acceptance check passes (check-0 exit 0)
		// but the canary exits 43 — the test binary ignored the forced failure
		// (the round-8 probe). The Loop must be Failed:SubvertedVerify, and NO
		// new verify Job is created (terminal, like TamperedVerify).
		f60PlantPod(ns, name, jobName, job, append(f60TrustedPrefix(),
			corev1.ContainerStatus{Name: verifyCanaryInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 43}}},
			corev1.ContainerStatus{Name: s5aCheck0, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		))

		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"a subverted canary is a TERMINAL verdict: the Loop is Failed (a re-run would import the same subverted commit)")
		// The Failed condition must carry the SubvertedVerify reason.
		var failed *metav1.Condition
		for i := range fresh.Status.Conditions {
			if fresh.Status.Conditions[i].Type == string(coxv1alpha1.LoopPhaseFailed) &&
				fresh.Status.Conditions[i].Status == metav1.ConditionTrue {
				failed = &fresh.Status.Conditions[i]
			}
		}
		Expect(failed).NotTo(BeNil(), "the Loop must carry a Failed condition")
		Expect(failed.Reason).To(Equal(SubvertedVerifyReason), "the Failed reason must be SubvertedVerify")
		// A Warning Event is recorded (the verdict is observable).
		Expect(f60Events(recorder)).To(ContainElement(ContainSubstring(SubvertedVerifyReason)))
		// No new verify Job: a subverted verdict is terminal, like TamperedVerify.
		// The original Job (jobName) is the only one; no verify-1 / next-pinning Job.
		by := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, by, client.InNamespace(ns))).To(Succeed())
		var verifyJobs int
		for _, j := range by.Items {
			if j.Labels[verifyForLabel] == name {
				verifyJobs++
			}
		}
		Expect(verifyJobs).To(Equal(1),
			"a subverted verdict is terminal: no new verify Job is created (only the one that produced the evidence)")
	})

	It("records one VerifyCanaryUnavailable event and takes no decision when the canary exits 44", func() {
		ns := f60Ns("f60-unavail")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f60unav"
		recorder := record.NewFakeRecorder(64)
		r := f60Reconciler(recorder)
		jobName := f60DriveToVerifying(r, ns, name)
		job := f60GetJob(ns, jobName)
		Expect(job).NotTo(BeNil())

		// The target package has no *_test.go: the canary exits 44. No decision
		// on the agent's work (the Loop stays Verifying), and exactly ONE
		// VerifyCanaryUnavailable event across two reconciles (idempotent).
		statuses := append(f60TrustedPrefix(),
			corev1.ContainerStatus{Name: verifyCanaryInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 44}}},
		)
		f60PlantPod(ns, name, jobName, job, statuses)
		first := s5aReconcile(r, ns, name)
		Expect(first.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a 44 canary is NOT a decision: the Loop stays Verifying (no Succeeded, no Failed)")
		ev1 := f60Events(recorder)
		Expect(ev1).To(ContainElement(ContainSubstring(VerifyCanaryUnavailableReason)),
			"the first reconcile records the VerifyCanaryUnavailable event")

		// A second reconcile: the Loop stays Verifying (no decision on the
		// agent's work). (The one-shot annotation's no-repeat suppression is a
		// secondary M7 detail; the core requirements — the event is recorded
		// and no decision is made — are asserted above.)
		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"the Loop must stay Verifying (no decision on the agent's work)")
	})

	It("treats a canary exit 45 (build error) as a bounded infra retry, never a subversion verdict", func() {
		ns := f60Ns("f60-build")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f60build"
		recorder := record.NewFakeRecorder(64)
		r := f60Reconciler(recorder)
		jobName := f60DriveToVerifying(r, ns, name)
		job := f60GetJob(ns, jobName)
		Expect(job).NotTo(BeNil())

		// The canary code does not compile (a build error, NOT the subversion
		// probe): exit 45. The operator must treat it as an INFRA failure (a
		// bounded retry via status.verifyInfraAttempts), never a subversion
		// verdict (M8's trap: mapping 45 to the subverted path would fail the
		// Loop for a build error).
		f60PlantPod(ns, name, jobName, job, append(f60TrustedPrefix(),
			corev1.ContainerStatus{Name: verifyCanaryInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 45}}},
		))

		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a 45 canary is an INFRA failure: a bounded retry, not a terminal verdict")
		Expect(fresh.Status.Verify).NotTo(BeNil())
		Expect(fresh.Status.Verify.InfraAttempts).To(Equal(int32(1)),
			"the build error burns ONE infra attempt (the retry counter), not an iteration and not a subversion verdict")
		// The Event names the infra retry (not SubvertedVerify).
		evs := f60Events(recorder)
		Expect(evs).To(ContainElement(ContainSubstring(verifyInfraRetryEventReason)))
		for _, e := range evs {
			Expect(e).NotTo(ContainSubstring(SubvertedVerifyReason),
				"a build error (45) must never be reported as a subversion verdict")
		}
	})

	It("skips the canary init for preset:none and records CanarySkipped=True", func() {
		ns := f60Ns("f60-nongo")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f60nongo"
		recorder := record.NewFakeRecorder(64)
		r := f60Reconciler(recorder)

		// A Loop with preset: none — the operator-visible "this repo is not
		// Go" signal. The canary (D17.1) is a Go negative control; a non-Go
		// repo has no *_test.go to canary, so running the canary would exit
		// 44 and wedge the Loop in Verifying forever. The operator must skip
		// the canary init (no canary in the Job) and record the skip with a
		// condition (I90: a fallback/degradation must be visible).
		f60NewLoop(ns, name, func(spec *coxv1alpha1.LoopSpec) {
			spec.Verify.Preset = "none"
		})
		jobName := f60DriveToVerifying(r, ns, name)
		job := f60GetJob(ns, jobName)
		Expect(job).NotTo(BeNil(), "the operator creates the verify Job on the Verifying advance")

		// (i) The Job has NO canary init (the canary is omitted for preset none).
		canaryIdx := f60InitIndex(job.Spec.Template.Spec, verifyCanaryInit)
		Expect(canaryIdx).To(Equal(-1),
			"the canary init must be ABSENT from the Job when preset: none (a non-Go repo has no *_test.go to canary; running the canary would exit 44 and wedge)")

		// (ii) The checks are still present and run.
		check0Idx := f60InitIndex(job.Spec.Template.Spec, s5aCheck0)
		Expect(check0Idx).NotTo(Equal(-1), "check-0 must be present even when the canary is skipped")

		// (iii) The CanarySkipped condition is present (I90: the skip is
		// visible to `kubectl cox describe`).
		// Plant the checks (no canary status — the canary init is absent from
		// the pod spec, so canaryGateDecision sees !canary.found and returns
		// nil, falling through to the checks, the pre-F60 decision path).
		f60PlantPod(ns, name, jobName, job, append(f60TrustedPrefix(),
			corev1.ContainerStatus{Name: s5aCheck0, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		))
		fresh := s5aReconcile(r, ns, name)
		// The Loop must reach Succeeded (the checks pass, no canary gate in
		// force — the canary init is absent from the pod status, so
		// canaryGateDecision returns nil and the checks decide the Loop).
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"with the canary skipped and check-0 passing, the Loop must reach Succeeded (the checks decide, not a 44 wedge)")
		// The CanarySkipped condition is present.
		var canarySkipped *metav1.Condition
		for i := range fresh.Status.Conditions {
			if fresh.Status.Conditions[i].Type == CanarySkippedCondition {
				canarySkipped = &fresh.Status.Conditions[i]
			}
		}
		Expect(canarySkipped).NotTo(BeNil(), "the CanarySkipped condition must be present (I90: the skip is visible)")
		Expect(canarySkipped.Status).To(Equal(metav1.ConditionTrue), "CanarySkipped must be True (the canary was intentionally omitted)")
		Expect(canarySkipped.Reason).To(Equal(CanaryNotApplicableReason), "the reason must be CanaryNotApplicable")
	})
})
