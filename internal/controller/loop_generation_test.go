package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// S3: status.observedGeneration tracks spec changes. After a spec update (new
// generation), a reconcile must record the new generation.
var _ = Describe("Loop observedGeneration tracking", func() {
	const (
		name = "gen-loop"
		ns   = "gen-test"
	)
	ctx := context.Background()
	nn := types.NamespacedName{Name: name, Namespace: ns}

	BeforeEach(func() {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		if err := k8sClient.Create(ctx, nsObj); err != nil && !errors.IsAlreadyExists(err) {
			_ = err
		}
	})

	AfterEach(func() {
		l := &coxv1alpha1.Loop{}
		if err := k8sClient.Get(ctx, nn, l); err == nil {
			_ = k8sClient.Delete(ctx, l)
		}
		_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	It("records the new generation after a spec change", func() {
		By("creating the Loop")
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "v1",
				Workspace: testWorkspace(),
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		By("mutating the spec (bumps generation)")
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		firstGen := loop.Generation
		loop.Spec.Goal = "v2"
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Generation).To(BeNumerically(">", firstGen), "spec change should bump generation")

		By("reconciling again")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		By("asserting observedGeneration caught up")
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.ObservedGeneration).To(Equal(loop.Generation),
			"observedGeneration must track the latest spec generation")
	})
})
