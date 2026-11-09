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

// J1d (#467 kind proof): the Judge Job's verdict call goes to the Loop's
// model proxy pod (the judge pod's egress netpol allows the proxy pod,
// loop_judge_job.go). The proxy's INGRESS must therefore admit the judge
// pod too: the judge pod carries coxswain.io/judge-for=<loop>
// (judgeJobLabels), which matches neither the agent peer
// (app.kubernetes.io/component=agent) nor the memory-writer peer
// (coxswain.io/memory-write-for). Without the peer every judge call times
// out at the proxy and every verdict is recorded JudgeUnavailable (the kind
// evidence: the judge termmsg "call failed: Post … context deadline
// exceeded").
//
// This spec is the same-Loop norm (I43/I49): a proxy netpol created WITHOUT
// the judge peer (the state every pre-fix Loop is in) must GAIN the peer on
// the next reconcile — the update path, not a create-only controller.

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

// j1dJudgeIngressPeer returns the judge peer in the proxy netpol's 8080
// ingress rule, or nil if no peer carries coxswain.io/judge-for = loopName.
func j1dJudgeIngressPeer(np *networkingv1.NetworkPolicy, loopName string) *networkingv1.NetworkPolicyPeer {
	for i := range np.Spec.Ingress {
		for _, port := range np.Spec.Ingress[i].Ports {
			if port.Port.IntValue() != 8080 {
				continue
			}
			for j := range np.Spec.Ingress[i].From {
				peer := np.Spec.Ingress[i].From[j]
				if peer.PodSelector != nil &&
					peer.PodSelector.MatchLabels[judgeForLabel] == loopName {
					return &np.Spec.Ingress[i].From[j]
				}
			}
		}
	}
	return nil
}

var _ = Describe("J1d: the proxy NetworkPolicy admits the Judge pod (verdict call)", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	buildLoop := func(loopName, ns, secretName string) *cxv1alpha1.Loop {
		return &cxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: loopName, Namespace: ns},
			Spec: cxv1alpha1.LoopSpec{
				Goal: "J1d proxy netpol judge peer",
				Workspace: cxv1alpha1.Workspace{
					Repo: "https://github.com/example/repo",
					Ref:  "main",
				},
				Agent: cxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: secretName,
					ModelEndpoint:     "vllm:8000",
				},
				Loop: cxv1alpha1.LoopSettings{MaxIterations: 1},
			},
		}
	}

	reconcileLoop := func(name, ns string) {
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).ToNot(HaveOccurred(), "reconcile %s/%s: %v", ns, name, err)
	}

	It("carries the judge peer on the 8080 ingress at create, and a drifted netpol without it GAINS the peer on the same-Loop reconcile (same-Loop)", func() {
		ns := "j1d-judge-peer" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "j1d-judge-creds", Namespace: ns},
			StringData: map[string]string{
				"MODEL_API_KEY":  "j1d-dummy-key",
				"MODEL_BASE_URL": "http://vllm:8000",
			},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())

		const loopName = "j1djudge"
		loop := buildLoop(loopName, ns, "j1d-judge-creds")
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop(loopName, ns)

		npKey := types.NamespacedName{Namespace: ns, Name: loopName + "-proxy-netpol"}
		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, npKey, np)).To(Succeed(), "the proxy netpol must exist after the first reconcile")
		originalUID := np.UID

		// The 8080 ingress admits the judge pod (the judge's verdict POST
		// rides this port; the judge pod's only identifying label is
		// coxswain.io/judge-for=<loop>).
		peer := j1dJudgeIngressPeer(np, loopName)
		Expect(peer).ToNot(BeNil(),
			"the proxy netpol's 8080 ingress must admit the judge pod (coxswain.io/judge-for=%s)", loopName)

		// TAMPER the live proxy netpol out-of-band: drop the judge peer from
		// the 8080 rule (the state every pre-fix Loop is in). A create-only
		// controller would leave the peer out; the controller's createOrUpdateNP
		// must re-assert the desired spec and restore the peer.
		tampered := np.DeepCopy()
		kept := make([]networkingv1.NetworkPolicyIngressRule, 0, len(tampered.Spec.Ingress))
		for i := range tampered.Spec.Ingress {
			from := []networkingv1.NetworkPolicyPeer{}
			dropped := false
			for j := range tampered.Spec.Ingress[i].From {
				p := tampered.Spec.Ingress[i].From[j]
				if p.PodSelector != nil && p.PodSelector.MatchLabels[judgeForLabel] == loopName {
					dropped = true
					continue
				}
				from = append(from, p)
			}
			if dropped {
				Expect(from).ToNot(BeEmpty(), "the 8080 rule keeps the agent peer after the tamper")
				tampered.Spec.Ingress[i].From = from
			}
			kept = append(kept, tampered.Spec.Ingress[i])
		}
		tampered.Spec.Ingress = kept
		Expect(j1dJudgeIngressPeer(tampered, loopName)).To(BeNil(), "the tamper dropped the judge peer")
		Expect(k8sClient.Update(ctx, tampered)).To(Succeed())

		// Reconcile the SAME Loop: the controller must restore the judge peer
		// on the EXISTING netpol (same UID — an in-place update, not a
		// recreation).
		reconcileLoop(loopName, ns)
		np = &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, npKey, np)).To(Succeed())
		Expect(np.UID).To(Equal(originalUID),
			"the tampered netpol is RESTORED in place (same UID), not deleted+recreated")
		Expect(j1dJudgeIngressPeer(np, loopName)).ToNot(BeNil(),
			"the tampered proxy netpol must GAIN BACK the judge peer on the same-Loop reconcile (drift correction)")
	})
})
