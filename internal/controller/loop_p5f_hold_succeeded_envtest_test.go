// P5f (I98, D69 option i): Succeeded is held until Delivered is terminal.
// While delivery is on and the Delivered condition is not terminal, the Loop
// stays at phase Verifying (condition Verifying=True reason Delivering) with
// the sandbox Suspended; both phase and desiredPhase become Succeeded once
// Delivered is True or False (D64). Delivery off goes straight to Succeeded.
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// p5fToVerifyPassed drives a fresh Loop through Planning/Implementing and a
// passing verify Job, WITHOUT forcing Succeeded. delivery=false clears
// spec.delivery; lessons sets lessonsAvailable on the Implementing claim.
func p5fToVerifyPassed(name, ns string, delivery, lessons bool) *LoopReconciler {
	ctx := context.Background()
	r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
	s5aEnsureSandbox(ns, name)
	loop := s6Loop(name, ns, "")
	if !delivery {
		loop.Spec.Delivery = nil
	}
	Expect(k8sClient.Create(ctx, loop)).To(Succeed())
	s6Reconcile(r, ns, name)
	loop = &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	loop.Status.BaseCommit = s5aBaseCommit
	Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
	s5aClaimPod(ns, name, "Planning", "")
	s6Reconcile(r, ns, name)
	s5aEnsureSandbox(ns, name)
	if lessons {
		s5aClaimPodWithLessonsAvailable(ns, name, "Implementing", s6HeadCommit, true)
	} else {
		s5aClaimPod(ns, name, "Implementing", s6HeadCommit)
	}
	s6Reconcile(r, ns, name)
	d61ReadyVerifyProxy(r, ns, name)
	s5aVerifyPod(ctx, ns, name, name+"-verify-1", 0)
	s6Reconcile(r, ns, name)
	return r
}

func p5fGet(ns, name string) *coxv1alpha1.Loop {
	l := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, l)).To(Succeed())
	return l
}

func p5fSandboxMode(ns, name string) sandboxv1beta1.SandboxOperatingMode {
	sbx := &sandboxv1beta1.Sandbox{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sbx)).To(Succeed())
	return sbx.Spec.OperatingMode
}

var _ = Describe("P5f: Succeeded is held until Delivered is terminal (I98)", func() {
	ctx := context.Background()
	freshNS := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })
		return ns
	}
	pushDone := func(ns, name string, exit int32, msg string) {
		s6DeliverPod(ns, name, corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: msg, Reason: i65ContainerErrReason},
		})
	}

	It("holds at Verifying/Delivering (not Succeeded) with the sandbox Suspended while Delivered is in progress", func() {
		ns := freshNS("p5f-hold")
		name := "hold1"
		r := p5fToVerifyPassed(name, ns, true, false)

		loop := p5fGet(ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "verify passed but delivery is not terminal")
		Expect(loop.Status.DesiredPhase).ToNot(Equal(coxv1alpha1.LoopPhaseSucceeded))
		Expect(loopHoldingForDelivery(loop)).To(BeTrue())

		By("the deliver Job runs during the hold; still in progress -> still held, across reconciles")
		s6Reconcile(r, ns, name)
		s6DeliverPod(ns, name, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
		for range 3 {
			loop = s6Reconcile(r, ns, name)
		}
		_ = s6GetJob(ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(loopHoldingForDelivery(loop)).To(BeTrue())
		ok, status, reason := s6Cond(loop)
		Expect(ok).To(BeTrue())
		Expect(status).To(Equal(metav1.ConditionFalse))
		Expect(reason).To(Equal(coxv1alpha1.ReasonDeliveryInProgress))

		By("the sandbox is Suspended during the hold (extended loopSandboxStopped gate)")
		Expect(p5fSandboxMode(ns, name)).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended))
	})

	It("sets Succeeded (phase AND desiredPhase) once Delivered=True", func() {
		ns := freshNS("p5f-ok")
		name := "ok1"
		r := p5fToVerifyPassed(name, ns, true, false)
		s6Reconcile(r, ns, name)
		loop := p5fGet(ns, name)
		pushDone(ns, name, 0, s6ValidTermination(loop))
		loop = s6Reconcile(r, ns, name)
		loop = s6Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		_, status, _ := s6Cond(loop)
		Expect(status).To(Equal(metav1.ConditionTrue))
		Expect(loopHoldingForDelivery(loop)).To(BeFalse())
		Expect(p5fSandboxMode(ns, name)).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended))
	})

	It("a user script waiting for Succeeded does not return until the PR exists", func() {
		ns := freshNS("p5f-wait")
		name := "wait1"
		r := p5fToVerifyPassed(name, ns, true, false)
		s6Reconcile(r, ns, name)
		s6DeliverPod(ns, name, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
		for range 4 {
			l := s6Reconcile(r, ns, name)
			Expect(l.Status.Phase).ToNot(Equal(coxv1alpha1.LoopPhaseSucceeded), "waiter must not see Succeeded before the PR exists")
			Expect(l.Status.Delivery).To(BeNil())
		}
		loop := p5fGet(ns, name)
		pushDone(ns, name, 0, s6ValidTermination(loop))
		for range 3 {
			loop = s6Reconcile(r, ns, name)
		}
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		Expect(loop.Status.Delivery).ToNot(BeNil(), "by the time the waiter sees Succeeded the PR is recorded")
		Expect(loop.Status.Delivery.PRNumber).To(BeEquivalentTo(7))
	})

	It("a FAILED delivery is still Succeeded with Delivered=False (D64)", func() {
		ns := freshNS("p5f-fail")
		name := "fail1"
		r := p5fToVerifyPassed(name, ns, true, false)
		s6Reconcile(r, ns, name)
		pushDone(ns, name, 1, "")
		var loop *coxv1alpha1.Loop
		for range 3 {
			loop = s6Reconcile(r, ns, name)
		}
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		_, status, reason := s6Cond(loop)
		Expect(status).To(Equal(metav1.ConditionFalse))
		Expect(reason).To(Equal(coxv1alpha1.ReasonDeliveryFailed))
	})

	It("delivery off: Succeeded immediately, no Delivered condition, no hold", func() {
		ns := freshNS("p5f-off")
		name := "off1"
		_ = p5fToVerifyPassed(name, ns, false, false)
		loop := p5fGet(ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		present, _, _ := s6Cond(loop)
		Expect(present).To(BeFalse())
		Expect(loopHoldingForDelivery(loop)).To(BeFalse())
	})

	Context("memory writer interaction (P5b/P5c)", func() {
		It("does NOT start the memory-writer Job during the Verifying/Delivering hold, and starts it once Succeeded after delivery", func() {
			ns := freshNS("p5f-mem")
			name := "mem1"
			r := p5fToVerifyPassed(name, ns, true, true)
			s6Reconcile(r, ns, name)
			s6DeliverPod(ns, name, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
			for range 3 {
				loop := s6Reconcile(r, ns, name)
				Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
			}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: memoryWriterJobName(p5bMustLoop(ns, name)), Namespace: ns}, &batchv1.Job{})
			Expect(err).To(HaveOccurred(), "no memory-writer Job while held")

			By("delivery terminal (False: D64) -> Succeeded -> the writer Job is created")
			pushDone(ns, name, 1, "")
			var loop *coxv1alpha1.Loop
			for range 4 {
				loop = s6Reconcile(r, ns, name)
			}
			Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
			Expect(p5bGetMemoryJob(ns, name)).ToNot(BeNil())
		})
	})
})
