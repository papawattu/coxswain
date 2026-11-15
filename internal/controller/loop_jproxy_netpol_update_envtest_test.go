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

// J5b review fix 1 (PR #491 review P2, R16 I43 norm): an envtest spec that
// calls r.ensureJudgeProxyNetworkPolicy with spec.verify.judge.modelEndpoint = A,
// switches it to B, calls it again, and asserts the live egress names B not A.
// This pins the same-Loop update path: a no-op mutate (the I42c lesson) would
// freeze the netpol at creation with endpoint A and leave B out of the live
// object, and this spec would fail.

import (
	"context"
	"net"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	jproxyNetpolUpdateLoop = "jproxy-np-update"
	jproxyEndpointA        = "10.0.0.7:9100"
	jproxyEndpointB        = "10.0.0.8:9200"
)

// cidrFor returns the /32 ipBlock CIDR for the IP part of an endpoint string.
func cidrFor(endpoint string) string {
	host, _, _ := net.SplitHostPort(endpoint)
	return host + "/32"
}

var _ = Describe("J5b: the Judge proxy NetworkPolicy same-Loop update (PR #491 P2)", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("updates the live jproxy netpol egress when the Judge endpoint changes (I43 norm)", func() {
		ns := "jproxy-npu-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })

		r := &LoopReconciler{
			CNIProber:       testCNIProber(),
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			PodCIDR:         d41cPodCIDR,
			ServiceCIDR:     d41cServiceCIDR,
			AllowUnenforced: true,
		}

		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: jproxyNetpolUpdateLoop, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				// No agent modelEndpoint (the spec is about the Judge's netpol,
				// not the agent's; setting one without EndpointSecretRef
				// triggers the CRD XValidation rule).
				Goal:      "jproxy netpol update test",
				Workspace: coxv1alpha1.Workspace{Repo: loopRepo, Ref: loopRef},
				Verify: coxv1alpha1.VerifyConfig{
					Judge: &coxv1alpha1.JudgeConfig{
						Enabled:           true,
						MaxTokens:         512,
						ModelEndpoint:     jproxyEndpointA,
						EndpointSecretRef: "judge-secret",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// Ensure with endpoint A.
		Expect(r.ensureJudgeProxyNetworkPolicy(ctx, loop)).To(Succeed())

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Namespace: ns, Name: judgeProxyNetPolName(jproxyNetpolUpdateLoop)},
			np,
		)).To(Succeed())

		// The live egress must name A (10.0.0.7/32), not B.
		cidrA := cidrFor(jproxyEndpointA)
		cidrB := cidrFor(jproxyEndpointB)
		hasA := false
		hasB := false
		for _, rule := range np.Spec.Egress {
			for _, peer := range rule.To {
				if peer.IPBlock != nil && peer.IPBlock.CIDR == cidrA {
					hasA = true
				}
				if peer.IPBlock != nil && peer.IPBlock.CIDR == cidrB {
					hasB = true
				}
			}
		}
		Expect(hasA).To(BeTrue(), "after ensure with endpoint A the live netpol egress must name A (%s)", cidrA)
		Expect(hasB).To(BeFalse(), "after ensure with endpoint A the live netpol egress must NOT name B (%s)", cidrB)

		// Switch the Judge endpoint to B in the in-memory Loop and ensure again.
		// The CRD makes judge.modelEndpoint immutable, so we cannot Update the
		// Loop in the cluster — but ensureJudgeProxyNetworkPolicy reads the
		// endpoint from the in-memory Loop object (buildJudgeProxyNetPol reads
		// loop.Spec.Verify.Judge.ModelEndpoint), so mutating the in-memory copy
		// and calling ensure again exercises the same-Loop update path exactly
		// as a real reconcile would (reconcile always re-gets the Loop; the
		// point of this spec is that the live NetPol tracks the endpoint).
		loop.Spec.Verify.Judge.ModelEndpoint = jproxyEndpointB
		Expect(r.ensureJudgeProxyNetworkPolicy(ctx, loop)).To(Succeed())

		np2 := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Namespace: ns, Name: judgeProxyNetPolName(jproxyNetpolUpdateLoop)},
			np2,
		)).To(Succeed())

		// The live egress must now name B (10.0.0.8/32), not A.
		hasA2 := false
		hasB2 := false
		for _, rule := range np2.Spec.Egress {
			for _, peer := range rule.To {
				if peer.IPBlock != nil && peer.IPBlock.CIDR == cidrA {
					hasA2 = true
				}
				if peer.IPBlock != nil && peer.IPBlock.CIDR == cidrB {
					hasB2 = true
				}
			}
		}
		Expect(hasB2).To(BeTrue(), "after re-ensure with endpoint B the live netpol egress must name B (%s)", cidrB)
		Expect(hasA2).To(BeFalse(), "after re-ensure with endpoint B the live netpol egress must NOT name A (%s) — the I42c no-op mutate would freeze it at A", cidrA)
	})
})
