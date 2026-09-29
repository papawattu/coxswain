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

package controller

// I43 (R3): the proxy NetworkPolicy's update path is DRIFT CORRECTION — its
// inputs (modelEndpoint, the model endpoint port) are CEL-validated immutable
// (self == oldSelf || self == ''), and the referenced AgentPolicy's network
// allows feed the AGENT netpol (the egress-proxy peer), not the proxy netpol.
// So like the proxy Service and the model-proxy KubeArmorPolicy, the proxy
// netpol's same-Loop spec must verify the controller RESTORES a tampered
// netpol (createOrUpdateNP re-asserts the desired spec against the live
// object since I42c) and RECREATES a deleted one. A create-only controller
// would pass a creation-only spec but fail these.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	cxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// I43 R3 fixture constants (drift-correction spec).
const (
	i43ProxyNPName  = "i43-proxy-np"
	i43ProxyNPEndpt = "10.0.0.7:9100" // IP-literal endpoint -> exact ipBlock /32 peer
	i43ProxyNPKey   = "i43-proxy-np-creds"
)

var _ = Describe("I43: proxy NetworkPolicy drift correction", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	buildLoop := func(loopName, ns, secretName, modelEndpoint string) *cxv1alpha1.Loop {
		return &cxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: loopName, Namespace: ns},
			Spec: cxv1alpha1.LoopSpec{
				Goal: "I43 proxy-netpol drift correction",
				Workspace: cxv1alpha1.Workspace{
					Repo: "https://github.com/example/repo",
					Ref:  "main",
				},
				Agent: cxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: secretName,
					ModelEndpoint:     modelEndpoint,
				},
				Loop: cxv1alpha1.LoopSettings{MaxIterations: 1},
			},
		}
	}

	reconcileLoop := func(name, ns string) {
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).ToNot(HaveOccurred(), "reconcile %s/%s: %v", ns, name, err)
	}

	It("restores a tampered proxy NetworkPolicy and recreates a deleted one on the same Loop (I43 same-Loop, R3)", func() {
		ns := "i43-proxy-np" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// Model-creds secret (required together with modelEndpoint).
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: i43ProxyNPKey, Namespace: ns},
			StringData: map[string]string{
				"MODEL_API_KEY":  "i43-dummy-key",
				"MODEL_BASE_URL": "http://10.0.0.7:9100",
			},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())

		const loopName = i43ProxyNPName
		loop := buildLoop(loopName, ns, i43ProxyNPKey, i43ProxyNPEndpt)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop(loopName, ns)

		npKey := types.NamespacedName{Namespace: ns, Name: loopName + "-proxy-netpol"}
		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, npKey, np)).To(Succeed(), "the proxy netpol must exist after the first reconcile")
		originalUID := np.UID
		Expect(np.OwnerReferences).To(HaveLen(1))
		Expect(np.OwnerReferences[0].Kind).To(Equal("Loop"))
		Expect(np.OwnerReferences[0].Name).To(Equal(loopName))

		// The model egress rule: the IP-literal endpoint (10.0.0.7:9100)
		// resolves to an exact ipBlock 10.0.0.7/32 peer, port 9100/TCP.
		modelRule := func(np *networkingv1.NetworkPolicy) *networkingv1.NetworkPolicyEgressRule {
			for i := range np.Spec.Egress {
				for _, peer := range np.Spec.Egress[i].To {
					if peer.IPBlock != nil {
						return &np.Spec.Egress[i]
					}
				}
			}
			return nil
		}
		rule := modelRule(np)
		Expect(rule).ToNot(BeNil(), "the initial proxy netpol must carry the model egress rule")
		Expect(rule.To[0].IPBlock.CIDR).To(Equal("10.0.0.7/32"))
		Expect(rule.Ports).To(ContainElement(networkingv1.NetworkPolicyPort{
			Protocol: new(corev1.ProtocolTCP),
			Port:     intstrPtr(9100),
		}))

		// TAMPER the live proxy netpol out-of-band: drop the model egress rule
		// (leaving only the DNS rule). A create-only controller would leave it
		// as-tampered; the controller's createOrUpdateNP (since I42c) must
		// re-assert the desired spec and restore the model rule.
		tampered := np.DeepCopy()
		// Keep only the DNS rule (egress[0]); drop the model rule (egress[1]).
		tampered.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{tampered.Spec.Egress[0]}
		Expect(k8sClient.Update(ctx, tampered)).To(Succeed())

		// Reconcile the SAME Loop: the controller must restore the model egress
		// rule on the EXISTING netpol (same UID — an in-place update, not a
		// recreation).
		reconcileLoop(loopName, ns)
		np = &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, npKey, np)).To(Succeed(),
			"the proxy netpol must still exist after the same-Loop reconcile")
		Expect(np.UID).To(Equal(originalUID),
			"the tampered netpol is RESTORED in place (same UID), not deleted+recreated")
		rule = modelRule(np)
		Expect(rule).ToNot(BeNil(),
			"the tampered proxy netpol must GAIN BACK the model egress rule on the same-Loop reconcile (drift correction)")
		Expect(rule.To[0].IPBlock.CIDR).To(Equal("10.0.0.7/32"),
			"the restored model egress peer must be the ipBlock 10.0.0.7/32")
		Expect(rule.Ports).To(ContainElement(networkingv1.NetworkPolicyPort{
			Protocol: new(corev1.ProtocolTCP),
			Port:     intstrPtr(9100),
		}), "the restored model egress rule must carry port 9100/TCP")
		Expect(np.OwnerReferences).To(HaveLen(1))
		Expect(np.OwnerReferences[0].Kind).To(Equal("Loop"))
		Expect(np.OwnerReferences[0].Name).To(Equal(loopName))

		// DELETE the proxy netpol and re-reconcile: the controller must
		// recreate it (new UID, desired spec, controller ref to the Loop).
		Expect(k8sClient.Delete(ctx, np)).To(Succeed())
		reconcileLoop(loopName, ns)
		np = &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, npKey, np)).To(Succeed(),
			"the deleted proxy netpol must be recreated on the same-Loop reconcile")
		Expect(np.UID).ToNot(Equal(originalUID),
			"the recreated netpol must be a new object (not the deleted one)")
		Expect(np.OwnerReferences).To(HaveLen(1))
		Expect(np.OwnerReferences[0].Kind).To(Equal("Loop"))
		Expect(np.OwnerReferences[0].Name).To(Equal(loopName),
			"the recreated netpol must be owned by the Loop (controller ref)")
		rule = modelRule(np)
		Expect(rule).ToNot(BeNil(),
			"the recreated proxy netpol must carry the model egress rule")
		Expect(rule.To[0].IPBlock.CIDR).To(Equal("10.0.0.7/32"))
		Expect(rule.Ports).To(ContainElement(networkingv1.NetworkPolicyPort{
			Protocol: new(corev1.ProtocolTCP),
			Port:     intstrPtr(9100),
		}))
	})
})
