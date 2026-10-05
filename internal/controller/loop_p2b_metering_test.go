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

// P2b (ADR-0009, TDD-PHASE2 P2b): the metering model proxy's operator wiring.
// The proxy pod carries the usage-port env (PROXY_PORT_USAGE / PROXY_USAGE_FILE
// / LOOP_NAME / LOOP_NAMESPACE), the model-creds file env (MODEL_CRED_FILE),
// the proxy-usage emptyDir volume, and the metering image; the <loop>-proxy
// NetworkPolicy gains an ingress rule for the operator namespace /
// controller-manager on the usage port (9090) — NEVER the agent (which has no
// SA token). The I43 same-Loop norm (R21 I49) applies to the proxy netpol's
// update path: a same-Loop reconcile must actually apply a change to the
// EXISTING netpol (not just create it) — a create-only controller would pass a
// creation-only spec but fail the drift-correction here.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// P2b fixture constants (goconst: they recur across the specs).
const (
	p2bModelCredsSecret = "p2b-model-creds"
	p2bModelEndpoint    = "10.0.0.9:9200" // IP-literal -> exact ipBlock /32 peer
	p2bOperatorNS       = "p2b-operator-ns"
)

var _ = Describe("P2b: metering model proxy operator wiring", func() {
	ctx := context.Background()

	buildLoop := func(name, ns, secretName, modelEndpoint string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "P2b metering model proxy",
				Workspace: testWorkspace(),
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: secretName,
					ModelEndpoint:     modelEndpoint,
				},
				Loop: coxv1alpha1.LoopSettings{MaxIterations: 1},
			},
		}
	}

	makeSecret := func(name, ns string) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			StringData: map[string]string{
				modelAPIKey:  "p2b-dummy-key",
				modelBaseURL: p2bModelEndpoint,
			},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	}

	// reconcileWith re-reconciles the Loop with a custom reconciler (the
	// OperatorNamespace is settable so the specs can toggle the usage ingress
	// rule on/off to prove the fail-closed behavior).
	reconcileWith := func(r *LoopReconciler, name, ns string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).ToNot(HaveOccurred(), "reconcile %s/%s: %v", ns, name, err)
	}

	proxyPod := func(name, ns string) *corev1.Pod {
		p := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-proxy"}, p)).To(Succeed(),
			"the proxy pod <loop>-proxy must exist")
		return p
	}

	proxyNP := func(name, ns string) *networkingv1.NetworkPolicy {
		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-proxy-netpol"}, np)).To(Succeed(),
			"the proxy netpol <loop>-proxy-netpol must exist")
		return np
	}

	// usageIngressRule finds the proxy netpol's ingress rule that allows the
	// operator namespace on the usage port (9090), or nil if absent.
	usageIngressRule := func(np *networkingv1.NetworkPolicy) *networkingv1.NetworkPolicyIngressRule {
		for i := range np.Spec.Ingress {
			for _, peer := range np.Spec.Ingress[i].From {
				if peer.NamespaceSelector != nil {
					if nsLabel, ok := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; ok &&
						nsLabel == p2bOperatorNS {
						for _, port := range np.Spec.Ingress[i].Ports {
							if port.Port.IntVal == proxyUsagePort {
								return &np.Spec.Ingress[i]
							}
						}
					}
				}
			}
		}
		return nil
	}

	It("builds the metering proxy pod (usage env, emptyDir, metering image) with a usage-port netpol ingress", func() {
		ns := "p2b-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		makeSecret(p2bModelCredsSecret, ns)

		name := "p2bproxy"
		Expect(k8sClient.Create(ctx, buildLoop(name, ns, p2bModelCredsSecret, p2bModelEndpoint))).To(Succeed())
		reconcileWith(&LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), OperatorNamespace: p2bOperatorNS}, name, ns)

		pod := proxyPod(name, ns)
		c := pod.Spec.Containers[0]
		env := map[string]corev1.EnvVar{}
		for _, e := range c.Env {
			env[e.Name] = e
		}

		By("running the metering image (the reconciler default, not the stand-in)")
		Expect(c.Image).To(Equal("coxswain-proxy:metering"),
			"P2b: the proxy image default flips from the stand-in to the metering image")

		By("setting the usage-port env (PROXY_PORT_USAGE, PROXY_USAGE_FILE, LOOP_NAME, LOOP_NAMESPACE)")
		Expect(env).To(HaveKey("PROXY_PORT_USAGE"))
		Expect(env["PROXY_PORT_USAGE"].Value).To(Equal("9090"))
		Expect(env).To(HaveKey("PROXY_USAGE_FILE"))
		Expect(env["PROXY_USAGE_FILE"].Value).To(Equal("/var/lib/proxy-usage/usage.json"))
		Expect(env).To(HaveKey("LOOP_NAME"))
		Expect(env["LOOP_NAME"].Value).To(Equal(name))
		Expect(env).To(HaveKey("LOOP_NAMESPACE"))
		Expect(env["LOOP_NAMESPACE"].Value).To(Equal(ns))

		By("setting the model-creds file env (MODEL_CRED_FILE) and keeping MODEL_ENDPOINT")
		Expect(env).To(HaveKey("MODEL_CRED_FILE"))
		Expect(env).To(HaveKey("MODEL_ENDPOINT"))

		By("mounting the proxy-usage emptyDir volume")
		var foundEmptyDir *corev1.Volume
		for i := range pod.Spec.Volumes {
			if pod.Spec.Volumes[i].Name == "proxy-usage" {
				foundEmptyDir = &pod.Spec.Volumes[i]
			}
		}
		Expect(foundEmptyDir).ToNot(BeNil(), "P2b: the proxy-usage emptyDir volume must be declared")
		Expect(foundEmptyDir.EmptyDir).ToNot(BeNil(), "the proxy-usage volume must be an emptyDir")
		mounted := false
		for _, vm := range c.VolumeMounts {
			if vm.Name == "proxy-usage" {
				mounted = true
			}
		}
		Expect(mounted).To(BeTrue(), "the proxy-usage volume must be mounted into the proxy container")

		By("creating the <loop>-proxy netpol with a usage-port ingress for the operator namespace / controller-manager")
		np := proxyNP(name, ns)
		rule := usageIngressRule(np)
		Expect(rule).ToNot(BeNil(),
			"P2b: the proxy netpol must carry a usage-port (9090) ingress rule for the operator namespace")
		// The operator peer: a namespaceSelector (kubernetes.io/metadata.name =
		// operator NS) AND a podSelector (control-plane=controller-manager).
		var operatorPeer *networkingv1.NetworkPolicyPeer
		for _, peer := range rule.From {
			if peer.NamespaceSelector != nil && peer.PodSelector != nil {
				operatorPeer = &peer
			}
		}
		Expect(operatorPeer).ToNot(BeNil(),
			"the usage-port ingress must be an operator-namespace + controller-manager peer (namespace + pod selector)")
		Expect(operatorPeer.NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
			"kubernetes.io/metadata.name", p2bOperatorNS,
		))
		Expect(operatorPeer.PodSelector.MatchLabels).To(HaveKeyWithValue(
			"control-plane", "controller-manager",
		))
		// The usage ingress must NOT be open to the agent (no SA token): the
		// only 9090 ingress peer is the operator. The agent peer (8080) is a
		// DIFFERENT rule (the model proxy port), not the usage port.
		for _, ingressRule := range np.Spec.Ingress {
			for _, port := range ingressRule.Ports {
				if port.Port.IntVal == proxyUsagePort {
					for _, peer := range ingressRule.From {
						Expect(peer.PodSelector.MatchLabels["coxswain.io/loop"]).To(BeEmpty(),
							"the usage-port (9090) ingress must NOT allow the agent (it has no SA token)")
					}
				}
			}
		}
	})

	It("leaves the usage ingress ABSENT when the operator namespace is unset (fail-closed)", func() {
		ns := "p2b-failclosed-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		makeSecret(p2bModelCredsSecret, ns)

		name := "p2bfc"
		Expect(k8sClient.Create(ctx, buildLoop(name, ns, p2bModelCredsSecret, p2bModelEndpoint))).To(Succeed())
		// No OperatorNamespace set: the usage ingress rule must be omitted (the
		// usage endpoint stays unreachable rather than wide-open).
		reconcileWith(&LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}, name, ns)

		np := proxyNP(name, ns)
		Expect(usageIngressRule(np)).To(BeNil(),
			"with no operator namespace the usage-port ingress rule must be absent (fail-closed, not wide-open)")
	})

	It("restores a tampered <loop>-proxy netpol on the same-Loop reconcile (I43 same-Loop norm)", func() {
		ns := "p2b-i43-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		makeSecret(p2bModelCredsSecret, ns)

		name := "p2bi43"
		Expect(k8sClient.Create(ctx, buildLoop(name, ns, p2bModelCredsSecret, p2bModelEndpoint))).To(Succeed())
		reconcileWith(&LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), OperatorNamespace: p2bOperatorNS}, name, ns)

		npKey := types.NamespacedName{Namespace: ns, Name: name + "-proxy-netpol"}
		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, npKey, np)).To(Succeed())
		originalUID := np.UID
		Expect(usageIngressRule(np)).ToNot(BeNil(), "the initial proxy netpol must carry the usage-port ingress rule")

		// TAMPER the live netpol out-of-band: drop the usage-port ingress rule
		// (leaving only the agent 8080 rule). A create-only controller would
		// leave it as-tampered; the controller's createOrUpdateNP must
		// re-assert the desired spec and restore the usage rule.
		tampered := np.DeepCopy()
		tampered.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{tampered.Spec.Ingress[0]}
		Expect(k8sClient.Update(ctx, tampered)).To(Succeed())

		// Reconcile the SAME Loop: the controller must restore the usage-port
		// ingress rule on the EXISTING netpol (same UID — an in-place update,
		// not a recreation).
		reconcileWith(&LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), OperatorNamespace: p2bOperatorNS}, name, ns)
		np = &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, npKey, np)).To(Succeed())
		Expect(np.UID).To(Equal(originalUID),
			"the tampered netpol is RESTORED in place (same UID), not deleted+recreated")
		Expect(usageIngressRule(np)).ToNot(BeNil(),
			"the tampered proxy netpol must GAIN BACK the usage-port ingress rule on the same-Loop reconcile (drift correction)")
	})
})
