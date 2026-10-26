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

// F58: the verify Job runs agent-authored code (the acceptance checks run
// `go test` over the agent's tree) and had NO resource requests, limits or
// emptyDir SizeLimit — so the pod was BestEffort (the first pod the kubelet
// evicts, and an unbounded check could take the node with it), and an
// OOM-killed check was misread as the agent's failure.
//
// The fix has three halves, and each has a spec here:
//
//  1. requests AND limits on every container of the Job — the trusted inits and
//     the no-op main at operator constants, the check-* containers at the
//     operator defaults with spec.verify.resources merged PER NAME over them.
//  2. a SizeLimit on the per-check /tmp emptyDir (the scratch the check's
//     toolchain writes) and deliberately NO SizeLimit on the `verify` checkout
//     (its size is the user's repo).
//  3. an OOMKilled check maps to verifyInfraFailed (an infra retry, never a
//     blamed iterate) while exit 137 with any other reason stays an iterate.
//
// The shape specs (1-3) assert the Job the OPERATOR CREATED — the Loop is
// driven through Reconcile to Verifying and the Job is read back — so they also
// cover ensureVerifyJob's wiring, not only buildVerifyJobSpec. The decision
// specs (4-8) plant the batch Job conditions and the pod's init statuses (envtest
// has no batch Job controller or kubelet) and drive the same reconciles.
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("F58: the verify Job's containers are bounded", func() {
	ctx := context.Background()

	// f58Ns creates a fresh namespace (the specs create real Loops).
	f58Ns := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	// f58NewLoop creates a Loop from s5aLoopSpec with optional refinements.
	f58NewLoop := func(ns, name string, refine ...func(*coxv1alpha1.LoopSpec)) {
		spec := s5aLoopSpec()
		for _, f := range refine {
			f(&spec)
		}
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       spec,
		})).To(Succeed())
	}

	// f58Reconciler builds a reconciler with the shared test prober (the D38
	// gate needs one: a nil prober fails closed) and an optional event recorder.
	f58Reconciler := func(recorder *record.FakeRecorder) *LoopReconciler {
		return &LoopReconciler{
			CNIProber: testCNIProber(),
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			apiReader: k8sClient,
			Recorder:  recorder,
		}
	}

	// f58GetJob reads the named verify Job (nil when absent).
	f58GetJob := func(ns, jobName string) *batchv1.Job {
		job := &batchv1.Job{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			Fail("get verify job: " + err.Error())
		}
		return job
	}

	// f58GetLoop reads the fresh Loop.
	f58GetLoop := func(ns, name string) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		return loop
	}

	// f58InfraAttempts reads status.verify.infraAttempts, treating a nil
	// status.verify as zero (the object appears only once the operator has read
	// the verify pod).
	f58InfraAttempts := func(loop *coxv1alpha1.Loop) int32 {
		if loop.Status.Verify == nil {
			return 0
		}
		return loop.Status.Verify.InfraAttempts
	}

	// f58Events drains a FakeRecorder's events into one slice.
	f58Events := func(recorder *record.FakeRecorder) []string {
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

	// f58DriveToVerifying drives a fresh Loop to Verifying and returns the
	// reconciler plus the verify Job name. The Job exists afterwards: the
	// operator creates it on the advance (ensureVerifyJob), which is what the
	// shape specs assert. The drive's own phase-advance Events are drained, so a
	// decision spec's event assertions are about ITS reconcile only (PhaseAdvanced
	// from the drive is not the subject of any spec here).
	f58DriveToVerifying := func(r *LoopReconciler, ns, name string) string {
		s5aDriveToVerifyingWithReconciler(r, ns, name)
		if r.Recorder != nil {
			if recorder, ok := r.Recorder.(*record.FakeRecorder); ok {
				_ = f58Events(recorder)
			}
		}
		return verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}

	// f58Container returns the named container of the Job's pod template
	// (init containers first, then the main containers), or nil.
	f58Container := func(podSpec corev1.PodSpec, name string) *corev1.Container {
		for i := range podSpec.InitContainers {
			if podSpec.InitContainers[i].Name == name {
				return &podSpec.InitContainers[i]
			}
		}
		for i := range podSpec.Containers {
			if podSpec.Containers[i].Name == name {
				return &podSpec.Containers[i]
			}
		}
		return nil
	}

	// f58Qty returns the quantity for a resource name as its API string form
	// (a resource.Quantity comparison via DeepEqual is unreliable: the type
	// caches its own string).
	f58Qty := func(l corev1.ResourceList, name corev1.ResourceName, owner string) string {
		q, ok := l[name]
		ExpectWithOffset(1, ok).To(BeTrue(), "%s must declare a %s resource", owner, name)
		return q.String()
	}

	// f58ExpectCheckDefaults asserts a check container carries the documented
	// operator defaults (the values in verifyCheckDefaultResources, pinned here
	// so a silent default change fails a spec and not only a review).
	f58ExpectCheckDefaults := func(ct *corev1.Container) {
		ExpectWithOffset(1, ct).NotTo(BeNil())
		ExpectWithOffset(1, f58Qty(ct.Resources.Requests, corev1.ResourceCPU, ct.Name)).To(Equal("250m"))
		ExpectWithOffset(1, f58Qty(ct.Resources.Requests, corev1.ResourceMemory, ct.Name)).To(Equal("256Mi"))
		ExpectWithOffset(1, f58Qty(ct.Resources.Limits, corev1.ResourceCPU, ct.Name)).To(Equal("2"))
		ExpectWithOffset(1, f58Qty(ct.Resources.Limits, corev1.ResourceMemory, ct.Name)).To(Equal("2Gi"))
		ExpectWithOffset(1, f58Qty(ct.Resources.Requests, corev1.ResourceEphemeralStorage, ct.Name)).To(Equal("1Gi"))
		ExpectWithOffset(1, f58Qty(ct.Resources.Limits, corev1.ResourceEphemeralStorage, ct.Name)).To(Equal("2Gi"))
	}

	// f58PlantPod creates the verify Job's pod with the given init statuses, so
	// the operator reads them (readVerifyJobPod) — the envtest stand-in for the
	// kubelet. podInits is the pod's own init-container SPEC (the message
	// helpers read the applied limits from it).
	f58PlantPod := func(ns, name, jobName string, job *batchv1.Job, podInits []corev1.Container, statuses []corev1.ContainerStatus) {
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
				InitContainers: podInits,
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

	// f58JobFailed stamps the Job's Failed condition (K8s 1.34 also requires
	// JobFailureTarget alongside JobFailed on a status update — the batch
	// controller sets both; envtest has none).
	f58JobFailed := func(ns, jobName, reason string) {
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

	// f58Terminated builds a terminated init status, exited 0 (the reason is
	// the kubelet's; the fixtures that need a non-zero code or an OOM reason
	// build their own status inline).
	f58Terminated := func(name string) corev1.ContainerStatus {
		return corev1.ContainerStatus{
			Name:  name,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}
	}

	// f58TrustedInits is the trusted prefix every decision fixture needs (all
	// exited 0) so the walk reaches the checks.
	f58TrustedInits := func() []corev1.ContainerStatus {
		return []corev1.ContainerStatus{
			f58Terminated(s5aCloneBase),
			f58Terminated(s5aImportAgent),
			f58Terminated(s5aTamper),
			f58Terminated(s5aArtifact),
		}
	}

	It("gives every container of the operator-created Job cpu+memory requests AND limits", func() {
		ns := f58Ns("f58-resources")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f58resources"
		f58NewLoop(ns, name)
		r := f58Reconciler(nil)
		jobName := f58DriveToVerifying(r, ns, name)

		job := f58GetJob(ns, jobName)
		Expect(job).NotTo(BeNil(), "the operator creates the verify Job on the Verifying advance")
		podSpec := job.Spec.Template.Spec

		// No container may be BestEffort: requests AND limits, always.
		for _, ct := range append(append([]corev1.Container{}, podSpec.InitContainers...), podSpec.Containers...) {
			Expect(ct.Resources.Requests).NotTo(BeEmpty(),
				"container %s has no requests, so the pod would be BestEffort (the first evicted, able to take the node)", ct.Name)
			Expect(ct.Resources.Limits).NotTo(BeEmpty(),
				"container %s has no limits, so it can consume the node unbounded", ct.Name)
			f58Qty(ct.Resources.Requests, corev1.ResourceCPU, ct.Name)
			f58Qty(ct.Resources.Requests, corev1.ResourceMemory, ct.Name)
			f58Qty(ct.Resources.Limits, corev1.ResourceCPU, ct.Name)
			f58Qty(ct.Resources.Limits, corev1.ResourceMemory, ct.Name)
		}

		// The trusted inits run operator scripts: operator constants, and no
		// spec.verify.resources entry may move them.
		for _, want := range []string{verifyCloneBaseInit, verifyImportAgentInit, verifyTamperInit, verifyArtifactInit} {
			ct := f58Container(podSpec, want)
			Expect(ct).NotTo(BeNil(), "the Job must carry the trusted init %s", want)
			Expect(f58Qty(ct.Resources.Requests, corev1.ResourceCPU, want)).To(Equal("100m"))
			Expect(f58Qty(ct.Resources.Requests, corev1.ResourceMemory, want)).To(Equal("128Mi"))
			Expect(f58Qty(ct.Resources.Limits, corev1.ResourceCPU, want)).To(Equal("1"))
			Expect(f58Qty(ct.Resources.Limits, corev1.ResourceMemory, want)).To(Equal("1Gi"))
		}

		// The check containers run agent-authored code: the defaults.
		check := f58Container(podSpec, "check-0")
		Expect(check).NotTo(BeNil(), "the Loop's one acceptance check is check-0")
		f58ExpectCheckDefaults(check)

		// The no-op main is /bin/true: the smallest sensible pair.
		noop := f58Container(podSpec, verifyNoopContainer)
		Expect(noop).NotTo(BeNil())
		Expect(f58Qty(noop.Resources.Requests, corev1.ResourceCPU, verifyNoopContainer)).To(Equal("10m"))
		Expect(f58Qty(noop.Resources.Requests, corev1.ResourceMemory, verifyNoopContainer)).To(Equal("16Mi"))
		Expect(f58Qty(noop.Resources.Limits, corev1.ResourceCPU, verifyNoopContainer)).To(Equal("100m"))
		Expect(f58Qty(noop.Resources.Limits, corev1.ResourceMemory, verifyNoopContainer)).To(Equal("64Mi"))
	})

	It("merges spec.verify.resources PER NAME over the defaults, leaves the trusted set alone, and defaults a Loop without it", func() {
		// The knob: a Loop whose checks need a bigger toolchain, with only ONE
		// request name and ONE limit name set. Every other name must keep the
		// operator default — that per-name merge is the property a later
		// refactor breaks, so it is asserted, not just documented.
		knobNs := f58Ns("f58-knob")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: knobNs}}) }()
		knobName := "f58knob"
		f58NewLoop(knobNs, knobName, func(spec *coxv1alpha1.LoopSpec) {
			spec.Verify.AcceptanceChecks = []string{loopCheckCmd, loopCheckCmd}
			spec.Verify.Resources = &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
			}
		})
		r := f58Reconciler(nil)
		knobJob := f58GetJob(knobNs, f58DriveToVerifying(r, knobNs, knobName))
		Expect(knobJob).NotTo(BeNil())

		for _, want := range []string{"check-0", "check-1"} {
			ct := f58Container(knobJob.Spec.Template.Spec, want)
			Expect(ct).NotTo(BeNil())
			Expect(f58Qty(ct.Resources.Limits, corev1.ResourceMemory, want)).To(Equal("4Gi"),
				"the Loop's limits.memory replaces the default for %s", want)
			Expect(f58Qty(ct.Resources.Requests, corev1.ResourceCPU, want)).To(Equal("500m"),
				"the Loop's requests.cpu replaces the default for %s", want)
			// The names the Loop did NOT set keep the defaults.
			Expect(f58Qty(ct.Resources.Requests, corev1.ResourceMemory, want)).To(Equal("256Mi"))
			Expect(f58Qty(ct.Resources.Limits, corev1.ResourceCPU, want)).To(Equal("2"))
			Expect(f58Qty(ct.Resources.Requests, corev1.ResourceEphemeralStorage, want)).To(Equal("1Gi"),
				"a knob that sets no ephemeral-storage key keeps the operator's scratch accounting")
			Expect(f58Qty(ct.Resources.Limits, corev1.ResourceEphemeralStorage, want)).To(Equal("2Gi"))
		}
		// The trusted inits are NOT moved by the knob.
		for _, want := range []string{verifyCloneBaseInit, verifyImportAgentInit, verifyTamperInit, verifyArtifactInit} {
			ct := f58Container(knobJob.Spec.Template.Spec, want)
			Expect(ct).NotTo(BeNil())
			Expect(f58Qty(ct.Resources.Limits, corev1.ResourceMemory, want)).To(Equal("1Gi"),
				"the trusted init %s must keep the operator's 1Gi memory limit whatever the checks are given", want)
			Expect(f58Qty(ct.Resources.Limits, corev1.ResourceCPU, want)).To(Equal("1"))
		}

		// The inverse: a Loop with no spec.verify.resources gets the defaults.
		defNs := f58Ns("f58-default")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defNs}}) }()
		defName := "f58default"
		f58NewLoop(defNs, defName)
		defJob := f58GetJob(defNs, f58DriveToVerifying(f58Reconciler(nil), defNs, defName))
		Expect(defJob).NotTo(BeNil())
		f58ExpectCheckDefaults(f58Container(defJob.Spec.Template.Spec, "check-0"))
	})

	It("lowers a request that the Loop's own lower limit would leave above it, so the Job is never invalid", func() {
		// The foot-gun (E2): each name merges independently, so LOWERING one limit
		// kept the operator's HIGHER default request (256Mi memory, 1Gi
		// ephemeral-storage) and built a container with request > limit. The API
		// server rejects that at Job create and ensureVerifyJob's error only
		// requeues: no Event, no condition, the Loop sits in Verifying looking
		// stuck. The operator now lowers the request to the limit.
		//
		// Mutation: delete the clamp loop in verifyCheckResources → the two
		// assertions below fail (request back at the default, above the limit).
		cases := []struct {
			name     string
			resource corev1.ResourceName
			value    string
		}{
			{"memory", corev1.ResourceMemory, "128Mi"},
			// The field doc advertises a lower ephemeral-storage limit, so it is
			// the case a user is most likely to type.
			{"ephemeralstorage", corev1.ResourceEphemeralStorage, "512Mi"},
		}
		for _, tc := range cases {
			ns := f58Ns("f58-clamp-" + tc.name)
			defer func(n string) { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}}) }(ns)
			name := "f58clamp" + tc.name
			limit := resource.MustParse(tc.value)
			f58NewLoop(ns, name, func(spec *coxv1alpha1.LoopSpec) {
				spec.Verify.Resources = &corev1.ResourceRequirements{
					Limits: corev1.ResourceList{tc.resource: limit},
				}
			})
			job := f58GetJob(ns, f58DriveToVerifying(f58Reconciler(nil), ns, name))
			Expect(job).NotTo(BeNil(), "the Job must be CREATED for a lowered %s limit, not rejected by the API server", tc.resource)
			ct := f58Container(job.Spec.Template.Spec, "check-0")
			Expect(ct).NotTo(BeNil())
			Expect(f58Qty(ct.Resources.Limits, tc.resource, "check-0")).To(Equal(tc.value))
			Expect(f58Qty(ct.Resources.Requests, tc.resource, "check-0")).To(Equal(tc.value),
				"the request must be lowered to the Loop's %s limit: a request above its own limit is an invalid Job", tc.resource)
		}
	})

	It("bounds each check's scratch with a SizeLimit but leaves the checkout volume unbounded (the user's repo)", func() {
		ns := f58Ns("f58-scratch")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f58scratch"
		f58NewLoop(ns, name, func(spec *coxv1alpha1.LoopSpec) {
			spec.Verify.AcceptanceChecks = []string{loopCheckCmd, loopCheckCmd}
		})
		job := f58GetJob(ns, f58DriveToVerifying(f58Reconciler(nil), ns, name))
		Expect(job).NotTo(BeNil())

		volByName := map[string]corev1.Volume{}
		for _, v := range job.Spec.Template.Spec.Volumes {
			volByName[v.Name] = v
		}
		for i := range 2 {
			volName := verifyCheckTmpVolName(i)
			vol, ok := volByName[volName]
			Expect(ok).To(BeTrue(), "the Job must declare the per-check scratch %s", volName)
			Expect(vol.EmptyDir).NotTo(BeNil())
			Expect(vol.EmptyDir.SizeLimit).NotTo(BeNil(),
				"the check scratch %s must be BOUNDED: it holds the toolchain caches of agent-authored code", volName)
			Expect(vol.EmptyDir.SizeLimit.String()).To(Equal("1Gi"))
		}
		// The decision, asserted so it cannot be "fixed" by accident: the
		// checkout volume's size is the USER's repo. A limit here would fail
		// large-but-legitimate repositories with an ephemeral-storage
		// eviction, and the artifact gate already caps ADDED files at 1 MiB.
		checkout, ok := volByName[verifyVol]
		Expect(ok).To(BeTrue())
		Expect(checkout.EmptyDir).NotTo(BeNil())
		Expect(checkout.EmptyDir.SizeLimit).To(BeNil(),
			"the /verify checkout must stay unbounded: its size is the user's repository, not a check's scratch")
	})

	It("maps an OOM-killed check to a bounded infra retry, naming the limit and the remedy, and burns no iteration", func() {
		ns := f58Ns("f58-oom")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f58oom"
		f58NewLoop(ns, name)
		recorder := record.NewFakeRecorder(64)
		r := f58Reconciler(recorder)
		jobName := f58DriveToVerifying(r, ns, name)
		job := f58GetJob(ns, jobName)
		Expect(job).NotTo(BeNil())
		iterBefore := f58GetLoop(ns, name).Status.Iteration

		// The motivating shape: check-0 is OOM-killed (exit 137, the kubelet's
		// Reason=OOMKilled). The pod's own container spec carries the 256Mi
		// limit the kubelet enforced, which is what the message must name.
		f58PlantPod(ns, name, jobName, job,
			[]corev1.Container{{
				Name:  s5aCheck0,
				Image: verifyDefaultCheckImage,
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
				},
			}},
			append(f58TrustedInits(), corev1.ContainerStatus{
				Name:  s5aCheck0,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: tamperOOMKilledReason}},
			}))

		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"an OOM-killed check is an INFRA failure: a bounded retry, not a terminal failure")
		Expect(f58InfraAttempts(fresh)).To(Equal(int32(1)),
			"the OOM kill burns ONE infra attempt (the retry counter), not an iteration")
		Expect(fresh.Status.Iteration).To(Equal(iterBefore),
			"the agent's work was never judged, so the iteration must not move")
		Expect(fresh.Status.StallHistory).To(BeEmpty(),
			"a resource limit is not a stalled check — the stall detector must not record an entry")

		events := f58Events(recorder)
		Expect(events).To(ContainElement(ContainSubstring(verifyInfraRetryEventReason)))
		Expect(events).To(ContainElement(ContainSubstring("OOM-killed")),
			"the Event must say the check was OOM-killed, not that check-0 failed (the message says 'OOM-killed' — the kubelet's Terminated.Reason 'OOMKilled' is not in the user-facing text)")
		Expect(events).To(ContainElement(ContainSubstring("256Mi")),
			"the Event must name the memory limit the kubelet enforced")
		Expect(events).To(ContainElement(ContainSubstring("spec.verify.resources.limits.memory")),
			"the Event must name the field that raises the limit — the remedy")
		for _, e := range events {
			Expect(e).NotTo(ContainSubstring(verifyIteratedReason),
				"an OOM kill must never appear as a verify iterate: that blames the agent for a resource limit")
		}
	})

	It("still iterates on exit 137 with a non-kubelet reason (the mapping is narrow)", func() {
		ns := f58Ns("f58-selfkill")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f58selfkill"
		f58NewLoop(ns, name)
		recorder := record.NewFakeRecorder(64)
		r := f58Reconciler(recorder)
		jobName := f58DriveToVerifying(r, ns, name)
		job := f58GetJob(ns, jobName)
		Expect(job).NotTo(BeNil())
		iterBefore := f58GetLoop(ns, name).Status.Iteration

		// 137 is SIGKILL either way; only the kubelet's own OOMKilled reason is
		// infra. A check that kills itself (Reason=Error) is the agent's work
		// failing and must iterate exactly as before F58.
		f58PlantPod(ns, name, jobName, job, nil,
			append(f58TrustedInits(), corev1.ContainerStatus{
				Name:  s5aCheck0,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "Error"}},
			}))

		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"exit 137 with reason Error is a failing check: the Loop iterates back to Implementing")
		Expect(fresh.Status.Iteration).To(BeNumerically(">", iterBefore),
			"a failing check burns an iteration")
		Expect(f58InfraAttempts(fresh)).To(Equal(int32(0)),
			"a failing check is not an infra failure, so no infra attempt is counted")
		for _, e := range f58Events(recorder) {
			Expect(e).NotTo(ContainSubstring(verifyInfraRetryEventReason),
				"a self-killed check must not be reported as an infra retry")
		}
	})

	It("decides nothing while an evicted pod is still there, and retries once it is gone (the I49 rule)", func() {
		ns := f58Ns("f58-evicted")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f58evicted"
		f58NewLoop(ns, name)
		recorder := record.NewFakeRecorder(64)
		r := f58Reconciler(recorder)
		jobName := f58DriveToVerifying(r, ns, name)
		job := f58GetJob(ns, jobName)
		Expect(job).NotTo(BeNil())

		// Same-pass: the kubelet evicted the pod mid-check (trusted inits and
		// the gates exited 0, check-0 is Running) and the Job went Failed. The
		// pod still exists, so the init statuses read as "in progress" — the
		// I49 rule: NO decision yet, however dead the pod really is.
		f58PlantPod(ns, name, jobName, job, nil, append(f58TrustedInits(), corev1.ContainerStatus{
			Name:  s5aCheck0,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}))
		evicted := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-verify-pod", Namespace: ns}, evicted)).To(Succeed())
		evicted.Status.Reason = "Evicted"
		evicted.Status.Phase = corev1.PodFailed
		Expect(k8sClient.Status().Update(ctx, evicted)).To(Succeed())
		f58JobFailed(ns, jobName, batchv1.JobReasonBackoffLimitExceeded)

		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(f58InfraAttempts(fresh)).To(Equal(int32(0)),
			"no infra attempt may be burned while the evicted pod still reads as in progress (I49)")
		Expect(f58Events(recorder)).To(BeEmpty(), "a no-decision reconcile emits no Event")

		// The pod goes (the GC removes an evicted pod): now the Job's own Failed
		// condition is the fact that the evidence will never arrive, so the
		// decision lands — an infra retry, not a blamed check.
		Expect(k8sClient.Delete(ctx, evicted)).To(Succeed())
		fresh = s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(f58InfraAttempts(fresh)).To(Equal(int32(1)),
			"once the evicted pod is gone, the Failed Job is an infra failure (a retry)")
		Expect(f58Events(recorder)).To(ContainElement(ContainSubstring(verifyInfraRetryEventReason)))
		Expect(f58GetJob(ns, jobName)).To(BeNil(), "the failed Job is deleted so a fresh one is created")
	})

	It("fails the Loop with VerifyInfraFailed once the eviction exhausts the infra bound", func() {
		ns := f58Ns("f58-evictedbound")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f58evictedbound"
		f58NewLoop(ns, name)
		r := f58Reconciler(record.NewFakeRecorder(64))
		jobName := f58DriveToVerifying(r, ns, name)

		// The last allowed run fails the same way: the count is seeded at the
		// bound (its reset belongs to a new pin, F48e) and the Job the retry
		// would have created is planted Failed with NO pod — the residue an
		// eviction leaves once the GC has removed the pod.
		loop := f58GetLoop(ns, name)
		if loop.Status.Verify == nil {
			loop.Status.Verify = &coxv1alpha1.VerifyStatus{}
		}
		loop.Status.Verify.InfraAttempts = verifyInfraRetries
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		job := f58GetJob(ns, jobName)
		Expect(job).NotTo(BeNil())
		Expect(k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() *batchv1.Job { return f58GetJob(ns, jobName) }).Should(BeNil(),
			"the failed Job must be gone before the replacement is planted (same name)")
		plantVerifyJob(ctx, ns, name, jobName)
		f58JobFailed(ns, jobName, batchv1.JobReasonBackoffLimitExceeded)

		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed))
		var cond *metav1.Condition
		for i := range fresh.Status.Conditions {
			if fresh.Status.Conditions[i].Type == string(coxv1alpha1.LoopPhaseFailed) {
				cond = &fresh.Status.Conditions[i]
			}
		}
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(verifyInfraFailedReason),
			"the terminal reason is the distinct infra one, never a check-failure iteration")
	})

	It("decides nothing while a check is still Running (the in-progress norm)", func() {
		ns := f58Ns("f58-inprog")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f58inprog"
		f58NewLoop(ns, name)
		recorder := record.NewFakeRecorder(64)
		r := f58Reconciler(recorder)
		jobName := f58DriveToVerifying(r, ns, name)
		job := f58GetJob(ns, jobName)
		Expect(job).NotTo(BeNil())
		iterBefore := f58GetLoop(ns, name).Status.Iteration

		// Everything the gates need has passed; check-0 is still running. The
		// operator must take NO decision: not an iterate, not an infra attempt,
		// not a stall entry, not an Event.
		f58PlantPod(ns, name, jobName, job, nil, append(f58TrustedInits(), corev1.ContainerStatus{
			Name:  s5aCheck0,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}))

		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a running check is in progress, never a failure (the S5a regression)")
		Expect(fresh.Status.Iteration).To(Equal(iterBefore))
		Expect(f58InfraAttempts(fresh)).To(Equal(int32(0)))
		if fresh.Status.Verify != nil {
			Expect(fresh.Status.Verify.LastCheckResults).To(BeNil(),
				"no check result may be recorded while the check is still running")
		}
		Expect(fresh.Status.StallHistory).To(BeEmpty())
		Expect(f58Events(recorder)).To(BeEmpty(), "no Event may fire for an in-progress verify")
		Expect(f58GetJob(ns, jobName)).NotTo(BeNil(),
			"the in-progress Job must not be deleted or replaced")

		// A second reconcile with the same state still decides nothing (the
		// hold is level-based, not one-shot).
		s5aReconcile(r, ns, name)
		Expect(f58GetLoop(ns, name).Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
	})
})
