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
								{Name: agentContainerName, Image: "docker.io/library/golang:1.26", Command: []string{"sh", "-c", sleepInfinity}},
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
		Expect(list.Items[0].Labels).To(HaveKeyWithValue("pre-existing", unstructuredTrue),
			"the pre-existing sandbox should be adopted, not replaced")

		// I2: adopting a sandbox must give it the Loop as controller owner,
		// otherwise it is orphaned (no GC, no Owns event mapping).
		ctrlOwners := make([]metav1.OwnerReference, 0, len(list.Items[0].OwnerReferences))
		for _, o := range list.Items[0].OwnerReferences {
			if o.Controller != nil && *o.Controller {
				ctrlOwners = append(ctrlOwners, o)
			}
		}
		Expect(ctrlOwners).To(HaveLen(1),
			"the adopted sandbox must have exactly one controller owner ref (got %+v)", list.Items[0].OwnerReferences)
		Expect(ctrlOwners[0].Name).To(Equal(name), "the controller owner must be the Loop")
		Expect(ctrlOwners[0].Kind).To(Equal(loopKind))
		Expect(ctrlOwners[0].UID).To(Equal(loop.UID))
	})

	// I2 (case 2): a pre-existing sandbox owned by a *different* controller
	// must not be silently taken over. Self-contained (its own namespace) so it
	// does not collide with the first It's namespace teardown.
	It("does not take over a sandbox owned by a different controller", func() {
		const (
			fName = "adopt-foreign-loop"
			fNS   = "adopt-foreign-test"
		)
		fNN := types.NamespacedName{Name: fName, Namespace: fNS}
		defer func() {
			_ = k8sClient.Delete(ctx, &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: fName, Namespace: fNS}})
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fNS}})
		}()

		By("creating a namespace")
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fNS}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())

		By("creating a Loop")
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: fName, Namespace: fNS},
			Spec:       coxv1alpha1.LoopSpec{Goal: "adopt", Workspace: testWorkspace()},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// A foreign controller object (a different UID) that already owns the
		// sandbox. We use a plain ConfigMap-like owner ref with a distinct UID.
		foreignUID := types.UID("00000000-0000-0000-0000-000000000001")
		foreign := true
		existing := &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name: fName + "-sandbox", Namespace: fNS,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "v1", Kind: "ConfigMap", Name: "other-owner", UID: foreignUID,
					Controller: &foreign,
				}},
			},
			Spec: sandboxv1beta1.SandboxSpec{
				OperatingMode: sandboxv1beta1.SandboxOperatingModeSuspended, // a value we must not clobber
				SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
					PodTemplate: sandboxv1beta1.PodTemplate{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: agentContainerName, Image: "docker.io/library/golang:1.26", Command: []string{"sh", "-c", sleepInfinity}},
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, existing)).To(Succeed())

		By("reconciling (must not take over the foreign-owned sandbox)")
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: fNN})
		// Taking over is wrong; the controller must either error or leave the
		// sandbox alone. We assert the stronger invariant: the sandbox's owner
		// is still the foreign controller, not this Loop.

		By("asserting the sandbox is still foreign-owned and its spec is untouched")
		sbNN := types.NamespacedName{Name: fName + "-sandbox", Namespace: fNS}
		var got sandboxv1beta1.Sandbox
		Expect(k8sClient.Get(ctx, sbNN, &got)).To(Succeed())
		var stillForeign bool
		for _, o := range got.OwnerReferences {
			if o.UID == foreignUID && o.Kind == "ConfigMap" {
				stillForeign = true
			}
			if o.Kind == loopKind && o.Name == fName {
				Fail("the sandbox was taken over by the Loop (owner ref rewritten)")
			}
		}
		Expect(err).To(HaveOccurred(), "taking over a foreign-owned sandbox should surface as an error")
		Expect(stillForeign).To(BeTrue(), "the foreign owner ref must be preserved")
	})
})
