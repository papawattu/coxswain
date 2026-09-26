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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// S2: A Sandbox that already exists (e.g. left by a previous controller
// instance before a crash) is adopted, not re-created. Reconciling must not
// produce a second sandbox.
var _ = Describe("Loop existing-sandbox adoption", func() {
	const (
		name = "adopt-loop"
		ns   = "adopt-test"
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

	It("adopts a pre-existing sandbox instead of creating a second", func() {
		By("creating a Loop")
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "adopt",
				Workspace: testWorkspace(),
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		By("pre-creating the sandbox (simulating a prior controller instance)")
		existing := &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name: name + "-sandbox", Namespace: ns,
				Labels: map[string]string{"pre-existing": "true"},
			},
			Spec: sandboxv1beta1.SandboxSpec{
				OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
					PodTemplate: sandboxv1beta1.PodTemplate{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "agent", Image: "docker.io/library/golang:1.26", Command: []string{"sh", "-c", "sleep infinity"}},
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, existing)).To(Succeed())

		By("reconciling")
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		By("asserting exactly one sandbox, still the adopted one")
		var list sandboxv1beta1.SandboxList
		Expect(k8sClient.List(ctx, &list, client.InNamespace(ns))).To(Succeed())
		Expect(list.Items).To(HaveLen(1), "must not create a second sandbox")
		Expect(list.Items[0].Labels).To(HaveKeyWithValue("pre-existing", "true"),
			"the pre-existing sandbox should be adopted, not replaced")
	})
})
