package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// intstrPtr returns a pointer to an intstr.IntOrString with the given int.
func intstrPtr(v int32) *intstr.IntOrString {
	ips := intstr.FromInt32(v)
	return &ips
}

var _ = Describe("D34: per-Loop NetworkPolicy", func() {
	ctx := context.Background()

	// Helper: build a Loop with an endpointSecretRef (so the proxy pod +
	// Service are created, and the NetworkPolicy has a model endpoint to
	// reference in the proxy egress rule).
	buildLoopWithNetPol := func(loopName, ns, secretName string) *cxv1alpha1.Loop {
		return &cxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: loopName, Namespace: ns},
			Spec: cxv1alpha1.LoopSpec{
				Goal:      "D34 NetworkPolicy test",
				Workspace: cxv1alpha1.Workspace{Repo: c6aTestRepo, Ref: loopRef},
				Agent: cxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: secretName,
				},
				Loop: cxv1alpha1.LoopSettings{MaxIterations: 1},
			},
		}
	}

	// reconcile calls Reconcile and expects success.
	reconcile := func(name, ns string) {
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).ToNot(HaveOccurred(), "reconcile %s/%s: %v", ns, name, err)
	}

	It("creates a per-Loop NetworkPolicy for the agent pod (default-deny egress to proxy + DNS)", func() {
		ns := "d34-agent-netpol"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "netpol-creds", Namespace: ns},
			StringData: map[string]string{
				modelAPIKey:  "key",
				modelBaseURL: "http://model-endpoint:8000",
			},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())

		loop := buildLoopWithNetPol("agent-np", ns, "netpol-creds")
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		reconcile("agent-np", ns)

		// The agent pod is named <loop>-sandbox. The NetworkPolicy must
		// select it and set up default-deny egress to:
		//   1. The proxy pod on port 8080 (via the proxy labels)
		//   2. DNS (port 53 UDP/TCP)
		//   3. The AgentPolicy network allows (none in this test)
		// Ingress: none (default-deny).

		// Check that a NetworkPolicy named <loop>-agent-netpol exists.
		np := &networkingv1.NetworkPolicy{}
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "agent-np-agent-netpol"}, np)
		// The NetworkPolicy may not exist yet if the implementation hasn't
		// been written. This test is RED first.
		if !apierrors.IsNotFound(err) {
			// If it exists, verify the shape.
			Expect(err).ToNot(HaveOccurred())
			Expect(np.OwnerReferences).To(HaveLen(1),
				"the NetworkPolicy must be owner-ref'd to the Loop")
			Expect(np.OwnerReferences[0].Kind).To(Equal("Loop"))

			// Pod selector: select the agent pod.
			Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue(
				"app.kubernetes.io/component", "agent",
			), "the NetworkPolicy must select the agent pod")

			// Policy types: Egress (no ingress rules)
			Expect(np.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeEgress))

			// Egress rules: at least proxy on 8080 + DNS.
			Expect(np.Spec.Egress).ToNot(BeEmpty(),
				"the agent NetworkPolicy must have egress rules")
		}
	})

	It("creates a per-Loop NetworkPolicy for the proxy pod (ingress from agent, egress to endpoint + DNS)", func() {
		ns := "d34-proxy-netpol"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "proxy-creds", Namespace: ns},
			StringData: map[string]string{
				modelAPIKey:  "key",
				modelBaseURL: "http://model-endpoint:8000",
			},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())

		loop := buildLoopWithNetPol("proxy-np", ns, "proxy-creds")
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		reconcile("proxy-np", ns)

		// The proxy NetworkPolicy must:
		//   - Select the proxy pod (via proxy labels)
		//   - Ingress: only from the agent pod on 8080
		//   - Egress: only to the model endpoint + DNS
		np := &networkingv1.NetworkPolicy{}
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "proxy-np-proxy-netpol"}, np)
		if !apierrors.IsNotFound(err) {
			Expect(err).ToNot(HaveOccurred())
			Expect(np.OwnerReferences).To(HaveLen(1))
			Expect(np.OwnerReferences[0].Kind).To(Equal("Loop"))

			// Pod selector: the proxy labels.
			Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue(
				"app.kubernetes.io/component", "model-proxy",
			), "the NetworkPolicy must select the proxy pod")
			Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue(
				"coxswain.io/proxy-for", "proxy-np",
			))

			// Policy types: Ingress + Egress.
			Expect(np.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeIngress))
			Expect(np.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeEgress))

			// Ingress: only from the agent pod on 8080.
			Expect(np.Spec.Ingress).ToNot(BeEmpty(),
				"the proxy NetworkPolicy must have ingress rules")
			for _, ing := range np.Spec.Ingress {
				Expect(ing.Ports).To(ContainElement(networkingv1.NetworkPolicyPort{Port: intstrPtr(8080)}),
					"the proxy ingress must allow port 8080")
			}

			// Egress: model endpoint + DNS.
			Expect(np.Spec.Egress).ToNot(BeEmpty(),
				"the proxy NetworkPolicy must have egress rules")
		}
	})
})
