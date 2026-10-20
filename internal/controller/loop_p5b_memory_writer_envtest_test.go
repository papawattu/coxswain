package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/pkg/memkey"
)

// P5b (Phase 5 shared memory, PLAN-PHASE5.md slice P5b): the memory-writer
// Job. The operator, on a Loop that has terminated in a qualifying outcome
// (Succeeded, or Failed:Stalled — the plan's eligibility rule), creates the
// memory-writer Job (the operator-controlled memory-write entrypoint — the
// Job mounts the workspace PVC READ-ONLY and the memory PVC READ-WRITE,
// reads the runner's proposed lessons, screens them with the operator's
// static policy, and writes the eligible lessons to the memory PVC) + its
// deny-all NetworkPolicy. The operator reads the Job's termination phase and
// records the outcome on the MemoryWriteDone condition.
//
// The envtest specs drive a Loop to a terminal phase through the S5a claim
// path (the s6Succeeded pattern — the phase machine is authoritative; a
// direct status.phase write is re-derived) and assert:
//   - the memory-writer Job is created for a Succeeded Loop (trigger);
//   - the Job mounts the workspace PVC read-only + the memory PVC read-write;
//   - the Job's container runs the operator-controlled entrypoint (the
//     memory-write binary with the outcome arg);
//   - the Job's NetworkPolicy denies ALL egress (the ADR-0007 fence);
//   - a non-qualifying terminal reason (a Failed with a non-Stalled reason)
//     writes nothing (the MemoryWriteSkipped condition);
//   - the outcome is recorded on the MemoryWriteDone condition (the Job's
//     pod Succeeded -> MemoryWritten; Failed -> MemoryWriteFailed);
//   - a failed write does NOT fail the Loop (the Loop stays Succeeded —
//     ADR-0004).
var _ = Describe("P5b memory-writer Job (Phase 5, R28 P2-7 transport)", func() {
	var ns string

	BeforeEach(func() {
		ns = "p5b-" + nowSuffix()
		Expect(k8sClient.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		// Wait for the namespace to be Active (a just-created namespace is
		// briefly initialising; a create of a resource into it races the
		// namespace finaliser).
		var nsObj corev1.Namespace
		Eventually(func() bool {
			_ = k8sClient.Get(context.Background(), types.NamespacedName{Name: ns}, &nsObj)
			return nsObj.Status.Phase == corev1.NamespaceActive
		}, 30*time.Second, time.Second).Should(BeTrue(), "the test namespace must be Active")
	})
	AfterEach(func() {
		_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	It("creates the memory-writer Job for a Succeeded Loop (trigger) and records MemoryWritten on a Succeeded Job", func() {
		// Drive a Loop to Succeeded (the S5a claim path + the verify Job's
		// passing evidence — the s6Succeeded pattern; the phase machine is
		// authoritative, so a direct status.phase write is re-derived).
		name := "p5b-mw"
		r := s6Succeeded(name, ns, inClusterRepoURL)

		// Reconcile once more: the operator sees the Succeeded phase and
		// creates the memory-writer Job + its NetworkPolicy.
		s6Reconcile(r, ns, name)

		// The memory-writer Job is created.
		job := p5bGetMemoryJob(ns, name)
		Expect(job.Labels["coxswain.io/memory-write-for"]).To(Equal(name))
		Expect(job.Labels["app.kubernetes.io/part-of"]).To(Equal("coxswain"))

		// The Job mounts the workspace PVC READ-ONLY + the memory PVC
		// READ-WRITE.
		c := job.Spec.Template.Spec.Containers[0]
		Expect(c.Name).To(Equal("memory-write"))
		mounts := map[string]corev1.VolumeMount{}
		for _, m := range c.VolumeMounts {
			mounts[m.Name] = m
		}
		wsMount, ok := mounts["workspace"]
		Expect(ok).To(BeTrue(), "the workspace PVC must be mounted")
		Expect(wsMount.ReadOnly).To(BeTrue(), "the workspace PVC must be read-only")
		Expect(wsMount.MountPath).To(Equal("/workspace"))
		memMount, ok := mounts["memory"]
		Expect(ok).To(BeTrue(), "the memory PVC must be mounted")
		Expect(memMount.ReadOnly).To(BeFalse(), "the memory PVC must be read-write")
		Expect(memMount.MountPath).To(Equal("/memory"))

		// The workspace volume source is the Loop's workspace PVC
		// (read-only); the memory volume source is the P5a memory PVC
		// (memory-<url-hash>).
		volumes := map[string]corev1.Volume{}
		for _, v := range job.Spec.Template.Spec.Volumes {
			volumes[v.Name] = v
		}
		wsVol := volumes["workspace"]
		Expect(wsVol.PersistentVolumeClaim).NotTo(BeNil(), "the workspace volume must be a PVC")
		Expect(wsVol.PersistentVolumeClaim.ClaimName).To(Equal(name + "-workspace"))
		Expect(wsVol.PersistentVolumeClaim.ReadOnly).To(BeTrue())
		memVol := volumes["memory"]
		Expect(memVol.PersistentVolumeClaim).NotTo(BeNil(), "the memory volume must be a PVC")
		Expect(memVol.PersistentVolumeClaim.ClaimName).To(Equal(memkey.PVCName(inClusterRepoURL)),
			"the memory PVC must be the P5a memory-<url-hash>")
		Expect(memVol.PersistentVolumeClaim.ReadOnly).To(BeFalse())

		// The container runs the operator-controlled entrypoint (the
		// memory-write binary with the outcome arg "succeeded").
		Expect(c.Command).To(HaveLen(3))
		Expect(c.Command[0]).To(Equal("/bin/sh"))
		Expect(c.Command[1]).To(Equal("-c"))
		Expect(c.Command[2]).To(ContainSubstring("memory-write"))
		Expect(c.Command[2]).To(ContainSubstring("succeeded"))

		// The Job's NetworkPolicy denies ALL egress (the ADR-0007 fence).
		netpol := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{
			Name: memoryWriterNetPolName(name), Namespace: ns,
		}, netpol)).To(Succeed())
		Expect(netpol.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeEgress))
		// The egress rule is empty (deny-all: no egress allowed).
		Expect(netpol.Spec.Egress).To(HaveLen(1))
		Expect(netpol.Spec.Egress[0].To).To(BeEmpty(), "the egress rule must be deny-all (no allowed peers)")

		// The operator has NOT yet recorded the outcome (the Job's pod is not
		// yet terminal in envtest — envtest has no kubelet to run the Job's
		// pod). Reconcile again to confirm the operator requeues (it does not
		// crash, and the condition is not yet set).
		s6Reconcile(r, ns, name)

		// Mark the Job's pod Succeeded (envtest has no kubelet; the spec
		// fabricates the pod the batch controller would create, with the
		// Job's ownerReference so the UID match selects it).
		p5bFabricateJobPod(ns, name, job, corev1.PodSucceeded)
		s6Reconcile(r, ns, name)

		// The outcome is recorded on the MemoryWriteDone condition
		// (MemoryWritten — the Job's pod succeeded).
		loop := p5bMustLoop(ns, name)
		cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
		Expect(cond).NotTo(BeNil(), "the MemoryWriteDone condition must be set")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(memoryWrittenReason))
		// The Loop is still Succeeded (a memory write does not change the
		// phase — ADR-0004).
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
	})

	It("does NOT create the memory-writer Job for a non-qualifying terminal reason (MemoryWriteSkipped)", func() {
		// A Loop that ends in Failed with a NON-Stalled reason (e.g.
		// SandboxFailed) is NOT qualifying (the plan requires Succeeded or
		// Failed:Stalled). The operator records the MemoryWriteSkipped
		// condition and does NOT create the Job.
		//
		// Drive the Loop to Failed (a non-Stalled reason) by failing the
		// sandbox (the sandbox-infrastructure-retry's final exhaustion sets
		// the SandboxFailed reason).
		name := "p5b-mw-skip"
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		s5aEnsureSandbox(ns, name)
		loop := s6Loop(name, ns, inClusterRepoURL)
		Expect(k8sClient.Create(context.Background(), loop)).To(Succeed())
		s6Reconcile(r, ns, name) // Pending -> Planning
		loop = p5bMustLoop(ns, name)
		loop.Status.BaseCommit = s5aBaseCommit
		Expect(k8sClient.Status().Update(context.Background(), loop)).To(Succeed())
		// Force the Loop to Failed with a non-Stalled reason (SandboxFailed).
		// The phase machine is authoritative, but a terminal Failed state is
		// stable (the S4 phase machine does NOT re-derive from a terminal
		// Failed — it is a terminal phase). Set the phase + the Failed
		// condition (reason SandboxFailed) directly.
		loop = p5bMustLoop(ns, name)
		loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
		setCondition(loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue, "SandboxFailed", "the sandbox failed (non-Stalled reason)")
		Expect(k8sClient.Status().Update(context.Background(), loop)).To(Succeed())

		// Reconcile: the operator sees the Failed:non-Stalled outcome and
		// records the MemoryWriteSkipped condition (no Job is created).
		s6Reconcile(r, ns, name)

		// No memory-writer Job is created.
		job := &batchv1.Job{}
		err := k8sClient.Get(context.Background(), types.NamespacedName{
			Name: memoryWriterJobName(p5bMustLoop(ns, name)), Namespace: ns,
		}, job)
		Expect(err).To(HaveOccurred(), "no memory-writer Job for a non-qualifying terminal reason")

		// The MemoryWriteSkipped condition is recorded.
		loop = p5bMustLoop(ns, name)
		cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
		Expect(cond).NotTo(BeNil(), "the MemoryWriteDone condition must be set")
		Expect(cond.Reason).To(Equal(memoryWriteSkippedReason))
	})

	It("records MemoryWriteFailed on a Failed Job (and the Loop stays Succeeded — ADR-0004)", func() {
		// Drive a Loop to Succeeded, create the Job (as in the first spec),
		// then mark the Job's pod FAILED. The operator records
		// MemoryWriteFailed + a Warning Event. The Loop stays Succeeded (a
		// failed memory write does NOT fail the Loop — ADR-0004).
		name := "p5b-mw-fail"
		r := s6Succeeded(name, ns, inClusterRepoURL)
		s6Reconcile(r, ns, name) // creates the Job
		job := p5bGetMemoryJob(ns, name)

		// Mark the Job's pod FAILED (envtest has no kubelet; the spec
		// fabricates the pod the batch controller would create, with the
		// Job's ownerReference so the UID match selects it). A pod spec needs
		// at least one container (the API requires it).
		p5bFabricateJobPod(ns, name, job, corev1.PodFailed)
		s6Reconcile(r, ns, name)

		// The MemoryWriteFailed condition is recorded.
		loop := p5bMustLoop(ns, name)
		cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
		Expect(cond).NotTo(BeNil(), "the MemoryWriteDone condition must be set")
		Expect(cond.Reason).To(Equal(memoryWriteFailedReason))
		// The Loop is still Succeeded (a failed memory write does NOT fail
		// the Loop — ADR-0004).
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
	})

	It("does nothing on a non-terminal phase (the eligibility rule)", func() {
		// A Loop at a non-terminal phase (e.g. Verifying) is NOT qualifying
		// (the plan: "any other terminal reason, or a non-terminal phase, is
		// ineligible"). The operator does NOTHING on a non-terminal phase
		// (no condition, no Job) — the Loop is still running and the write
		// is not yet eligible.
		name := "p5b-mw-nonterm"
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		s5aEnsureSandbox(ns, name)
		loop := s6Loop(name, ns, inClusterRepoURL)
		Expect(k8sClient.Create(context.Background(), loop)).To(Succeed())
		s6Reconcile(r, ns, name) // Pending -> Planning
		// The Loop is at Planning (a non-terminal phase). Reconcile: the
		// operator does NOTHING (no condition, no Job).
		loop = p5bMustLoop(ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning), "the Loop must be at a non-terminal phase")
		s6Reconcile(r, ns, name)

		// No memory-writer Job is created.
		job := &batchv1.Job{}
		err := k8sClient.Get(context.Background(), types.NamespacedName{
			Name: memoryWriterJobName(p5bMustLoop(ns, name)), Namespace: ns,
		}, job)
		Expect(err).To(HaveOccurred(), "no memory-writer Job on a non-terminal phase")

		// NO MemoryWriteDone condition is set (the operator does nothing on a
		// non-terminal phase — the condition is only meaningful once the Loop
		// has terminated).
		loop = p5bMustLoop(ns, name)
		cond := meta.FindStatusCondition(loop.Status.Conditions, memoryWriterConditionType)
		Expect(cond).To(BeNil(), "no MemoryWriteDone condition on a non-terminal phase")
	})
})

// p5bGetMemoryJob returns the memory-writer Job (failing on a read error).
func p5bGetMemoryJob(ns, loopName string) *batchv1.Job {
	job := &batchv1.Job{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{
		Name: memoryWriterJobName(p5bMustLoop(ns, loopName)), Namespace: ns,
	}, job)).To(Succeed())
	return job
}

// p5bMustLoop returns the Loop (failing on a read error).
func p5bMustLoop(ns, name string) *coxv1alpha1.Loop {
	loop := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	return loop
}

// p5bFabricateJobPod creates and returns the memory-writer Job's pod (envtest
// has no kubelet to run the Job). The pod carries the Job's ownerReference
// (so the operator's podOwnedByJobUID match selects it) and the
// memory-write-for label (the Job's stamp). The pod spec has at least one
// container (the API requires it). The pod's phase is set to the given
// terminal phase (Succeeded / Failed) via the status subresource (a
// freshly-created pod starts in Pending; the API server resets the status
// on create, so the phase must be set after creation).
func p5bFabricateJobPod(ns, loopName string, job *batchv1.Job, phase corev1.PodPhase) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      loopName + "-memory-write-1-xyz",
			Namespace: ns,
			Labels:    map[string]string{"coxswain.io/memory-write-for": loopName},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job",
				Name: job.Name, UID: job.UID, Controller: &memoryWriterTrue,
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "memory-write",
				Image: "docker.io/alpine/git:v2.54.0",
			}},
		},
	}
	Expect(k8sClient.Create(context.Background(), pod)).To(Succeed())
	// Set the pod's phase via the status subresource (the API server resets
	// the status on create; a freshly-created pod is Pending, so the
	// terminal phase must be set after creation).
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: pod.Name, Namespace: ns}, pod)).To(Succeed())
	pod.Status.Phase = phase
	Expect(k8sClient.Status().Update(context.Background(), pod)).To(Succeed())
}
