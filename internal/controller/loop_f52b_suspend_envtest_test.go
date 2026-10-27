// F52 B1 (queue-b2-after-f30.md:66; design .samples/design-f52-f53.md §F52-B):
// a delivery that FAILED is terminal, so it is no longer "in flight" and
// spec.suspend on it must not be refused. The old predicate tested
// `status.delivery == nil`, and the deliver Job's failure paths never write
// status.delivery (only the success path does — they record
// Delivered=False/DeliveryFailed), so a Succeeded Loop whose delivery failed
// refused suspend for ever.
//
// These specs drive Reconcile: the refusal's only surface is the Paused
// condition (Paused=False reason SuspendRefused), so that is what they read.
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("F52 B1: suspend is refused only while the delivery is not terminal", func() {
	ctx := context.Background()
	freshNS := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })
		return ns
	}
	// suspend sets spec.suspend=true on a Loop that already exists (the CLI's
	// `cox suspend`), returning the reconciled Loop.
	suspend := func(r *LoopReconciler, ns, name string) *coxv1alpha1.Loop {
		l := p5fGet(ns, name)
		l.Spec.Suspend = true
		Expect(k8sClient.Update(ctx, l)).To(Succeed())
		var loop *coxv1alpha1.Loop
		for range 2 {
			loop = s6Reconcile(r, ns, name)
		}
		return loop
	}

	It("does not refuse spec.suspend once the delivery FAILED", func() {
		ns := freshNS("f52b1-fail")
		name := "fail1"
		r := p5fToVerifyPassed(name, ns, true, false)
		s6Reconcile(r, ns, name)

		By("the deliver Job's push exits non-zero -> Delivered=False/DeliveryFailed")
		s6DeliverPod(ns, name, corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: i65ContainerErrReason},
		})
		var loop *coxv1alpha1.Loop
		for range 3 {
			loop = s6Reconcile(r, ns, name)
		}
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded), "a failed delivery is still Succeeded (D64)")
		_, status, reason := s6Cond(loop)
		Expect(status).To(Equal(metav1.ConditionFalse))
		Expect(reason).To(Equal(coxv1alpha1.ReasonDeliveryFailed))
		Expect(loop.Status.Delivery).To(BeNil(),
			"the failure path records the outcome as the Delivered condition; status.delivery is written only on success (which is why the old predicate refused for ever)")

		By("suspend=true is not refused: the delivery is terminal, so no deliver Job is in flight")
		loop = suspend(r, ns, name)
		if c := apimeta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.PausedCondition); c != nil {
			Expect(c.Reason).NotTo(Equal(suspendedRefusedCondReason),
				"a FAILED delivery is terminal and must not read as an in-flight deliver Job: %q", c.Message)
		}
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"a terminal phase is not pausable: the suspend is honoured by stopping the sandbox, not by entering Paused")
	})

	It("still refuses spec.suspend while the deliver Job is in flight (control)", func() {
		ns := freshNS("f52b1-flight")
		name := "flight1"
		r := p5fToVerifyPassed(name, ns, true, false)
		Expect(loopHoldingForDelivery(p5fGet(ns, name))).To(BeTrue(), "setup: held at Verifying+Delivering")
		s6Reconcile(r, ns, name)

		loop := suspend(r, ns, name)
		c := apimeta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.PausedCondition)
		Expect(c).NotTo(BeNil(), "the refusal must be recorded on the Paused condition")
		Expect(c.Reason).To(Equal(suspendedRefusedCondReason))
		Expect(c.Message).NotTo(ContainSubstring("phase Succeeded"),
			"the hold is at phase Verifying, so the refusal's message must not claim otherwise")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "the Loop stays held, it does not pause")
	})

	It("persists the verify record as Verifying=False once the hold settles", func() {
		ns := freshNS("f52b2-settled")
		name := "settled1"
		r := p5fToVerifyPassed(name, ns, true, false)
		s6Reconcile(r, ns, name)
		loop := p5fGet(ns, name)
		Expect(loopHoldingForDelivery(loop)).To(BeTrue(), "setup: held at Verifying+Delivering")

		s6DeliverPod(ns, name, corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0, Reason: i65ContainerErrReason, Message: s6ValidTermination(loop),
			},
		})
		for range 3 {
			loop = s6Reconcile(r, ns, name)
		}
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))

		c := apimeta.FindStatusCondition(loop.Status.Conditions, string(coxv1alpha1.LoopPhaseVerifying))
		Expect(c).NotTo(BeNil(),
			"B2: settling the hold must keep the record that the Loop passed verify (it was deleted before)")
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(coxv1alpha1.ReasonDelivered),
			"the settled condition carries the delivery outcome that released the hold")
		Expect(loopHoldingForDelivery(loop)).To(BeFalse())
	})
})
