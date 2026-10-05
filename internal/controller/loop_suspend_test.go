package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// S1: A Loop with spec.suspend:true must not run a Running sandbox. The
// sandbox it creates is Suspended (or the controller does not run it).
var _ = Describe("Loop suspend handling", func() {
	const (
		name = "suspend-loop"
		ns   = "suspend-test"
	)
	ctx := context.Background()
	nn := types.NamespacedName{Name: name, Namespace: ns}

	BeforeEach(func() {
		// Own namespace keeps this test decoupled from the S0 suite (both
		// list sandboxes; a shared namespace would let one test's sandbox leak
		// into the other's count).
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		if err := k8sClient.Create(ctx, nsObj); err != nil && !errors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	})

	AfterEach(func() {
		l := &coxv1alpha1.Loop{}
		if err := k8sClient.Get(ctx, nn, l); err == nil {
			_ = k8sClient.Delete(ctx, l)
		}
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		_ = k8sClient.Delete(ctx, nsObj)
	})

	It("creates a Suspended sandbox for a suspended Loop and sets phase=Paused (P2f, upgraded from S1)", func() {
		By("creating a Loop with spec.suspend=true")
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "anything",
				Suspend:   true,
				Workspace: testWorkspace(),
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		By("reconciling")
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		By("asserting the sandbox is Suspended, not Running")
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)).To(Succeed())
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"a suspended Loop must not run a Running sandbox")

		By("asserting the phase is Paused (P2f: spec.suspend=true on a non-terminal phase is a Paused transition). The S4 bootstrap advanced Pending->Planning in the same reconcile, so the pause sees Planning.)")
		l := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, l)).To(Succeed())
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))
		Expect(l.Status.PausedFrom).To(Equal(coxv1alpha1.LoopPhasePlanning))
		Expect(l.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonSuspend))
	})
})
