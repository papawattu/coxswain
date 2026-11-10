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
// limitations under this License.

package controller

// I265 (R55): TWICE now, a new pod that calls the Loop's model proxy got an
// egress rule on its own side but no ingress peer on the proxy's side (P5c for
// the memory writer, J1d for the Judge). Both were found only on kind. This
// is the STRUCTURAL spec that catches any recurrence for every caller at once:
// it lists every NetworkPolicy in the Loop's namespace, takes each one whose
// egress admits a pod carrying coxswain.io/proxy-for=<loop> (netpolProxyForLabel),
// and asserts the proxy's 8080 ingress admits THAT pod's selector (its own
// podSelector's MatchLabels must be a superset of an ingress peer's
// MatchLabels). A future caller that adds an egress rule to the proxy without
// an ingress peer here fails the spec — without a kind run.
//
// Approach note (per the I265 brief): one Loop is reconciled far enough that
// the proxy NetworkPolicy and the agent's NetworkPolicy exist (the proxy +
// agent netpols are created by ensureNetworkPolicy in the sandbox phase); the
// two Job callers' netpols are created by calling the builder functions
// DIRECTLY (ensureMemoryWriterNetworkPolicy / ensureJudgeNetworkPolicy),
// because the memory writer is only created on a terminal phase and the judge
// netpol is created on the Judging hold — two more reconcile steps than the
// spec needs, and the builders are the production code path. All three netpols
// come from the real builders, not hand-written specs.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	cxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// i265CallerPod is one caller admitted to reach the model proxy: its netpol's
// own podSelector (the caller pod's labels) as collected by the spec.
type i265CallerPod struct {
	netpolName  string
	podSelector map[string]string
}

// i265ProxyIngressAdmits reports whether the proxy netpol's 8080 ingress
// carries a peer whose podSelector's MatchLabels are a SUBSET of the caller
// pod's labels (the ingress peer must select the caller pod: every label the
// peer requires, the caller pod must carry).
func i265ProxyIngressAdmits(proxyNP *networkingv1.NetworkPolicy, caller i265CallerPod) bool {
	for i := range proxyNP.Spec.Ingress {
		for _, port := range proxyNP.Spec.Ingress[i].Ports {
			if port.Port.IntValue() != 8080 {
				continue
			}
			for j := range proxyNP.Spec.Ingress[i].From {
				peer := proxyNP.Spec.Ingress[i].From[j]
				if peer.PodSelector == nil || peer.PodSelector.MatchLabels == nil {
					continue
				}
				peerL := peer.PodSelector.MatchLabels
				// The peer's labels must all be present (with the same value)
				// in the caller pod's labels — a subset.
				subset := true
				for k, v := range peerL {
					if cv, ok := caller.podSelector[k]; !ok || cv != v {
						subset = false
						break
					}
				}
				if subset {
					return true
				}
			}
		}
	}
	return false
}

var _ = Describe("I265: the proxy NetworkPolicy admits every caller that reaches it", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("lists every netpol whose egress reaches the model proxy and asserts the proxy's 8080 ingress admits each caller (3+ callers)", func() {
		ns := "i265-proxy-callers" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "i265-creds", Namespace: ns},
			StringData: map[string]string{
				"MODEL_API_KEY":  "i265-dummy-key",
				"MODEL_BASE_URL": "http://vllm:8000",
			},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())

		const loopName = "i265callers"
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme()}
		loop := &cxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: loopName, Namespace: ns},
			Spec: cxv1alpha1.LoopSpec{
				Goal: "I265 proxy ingress caller admission",
				Workspace: cxv1alpha1.Workspace{
					Repo: "https://github.com/example/repo",
					Ref:  "main",
				},
				Agent: cxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: "i265-creds",
					ModelEndpoint:     "vllm:8000",
				},
				Verify: cxv1alpha1.VerifyConfig{
					AcceptanceChecks: []string{"true"},
					Judge:            &cxv1alpha1.JudgeConfig{Enabled: true, MaxTokens: 512},
				},
				Loop: cxv1alpha1.LoopSettings{MaxIterations: 1},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		// Reconcile far enough that ensureNetworkPolicy creates the proxy
		// netpol + the agent's netpol (the sandbox phase).
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: loopName}})
		Expect(err).ToNot(HaveOccurred(), "reconcile %s/%s: %v", ns, loopName, err)

		// The two Job callers' netpols: call the builder functions directly
		// (the production code path — ensureMemoryWriterNetworkPolicy /
		// ensureJudgeNetworkPolicy) so the spec sees the REAL netpols the
		// controller would create, not hand-written copies.
		Expect(r.ensureMemoryWriterNetworkPolicy(ctx, loop)).To(Succeed())
		Expect(r.ensureJudgeNetworkPolicy(ctx, loop)).To(Succeed())

		// Collect the caller netpols: every netpol in the namespace whose
		// egress has a peer selecting coxswain.io/proxy-for=<loop>.
		npList := &networkingv1.NetworkPolicyList{}
		Expect(k8sClient.List(ctx, npList, client.InNamespace(ns))).To(Succeed(), "list the namespace's NetworkPolicies")

		callers := []i265CallerPod{}
		for i := range npList.Items {
			np := &npList.Items[i]
			if np.Name == loopName+"-proxy-netpol" {
				continue // the proxy's own netpol is not a caller
			}
			for ri := range np.Spec.Egress {
				rule := np.Spec.Egress[ri]
				for pi := range rule.To {
					peer := rule.To[pi]
					if peer.PodSelector != nil &&
						peer.PodSelector.MatchLabels[netpolProxyForLabel] == loopName {
						callers = append(callers, i265CallerPod{
							netpolName:  np.Name,
							podSelector: np.Spec.PodSelector.MatchLabels,
						})
						break
					}
				}
			}
		}

		// Positive control: the three known callers (agent, memory writer,
		// judge) were ALL found by the LIST — that is the property the spec
		// must prove (a new caller is caught because it is listed, not
		// because the spec knows its name).
		Expect(callers).To(HaveLen(3),
			"the agent, memory-writer, and judge caller netpols must each be found by the LIST (a netpol whose egress reaches the proxy); found: %+v", callers)
		agentFound, writerFound, judgeFound := false, false, false
		for _, c := range callers {
			if c.podSelector["coxswain.io/loop"] == loopName &&
				c.podSelector["app.kubernetes.io/component"] == "agent" {
				agentFound = true
			}
			if c.podSelector[memoryWriteForLabelKey] == loopName {
				writerFound = true
			}
			if c.podSelector[judgeForLabel] == loopName {
				judgeFound = true
			}
		}
		Expect(agentFound).To(BeTrue(), "the agent caller (coxswain.io/loop=<loop>, component=agent) was found")
		Expect(writerFound).To(BeTrue(), "the memory-writer caller (memory-write-for=<loop>) was found")
		Expect(judgeFound).To(BeTrue(), "the judge caller (judge-for=<loop>) was found")

		// The structural assertion: the proxy's 8080 ingress admits EACH
		// caller (a peer whose MatchLabels are a subset of the caller pod's
		// labels).
		proxyNP := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: loopName + "-proxy-netpol"}, proxyNP)).To(Succeed())
		for _, caller := range callers {
			Expect(i265ProxyIngressAdmits(proxyNP, caller)).To(BeTrue(),
				"the proxy's 8080 ingress must admit caller %s (pod selector %v) — I265: an egress rule without an ingress peer was the P5c and J1d defect",
				caller.netpolName, caller.podSelector)
		}
	})
})
