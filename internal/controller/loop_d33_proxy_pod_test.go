/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

// D33 (REVIEW-PHASE1-R13, owner option c): the model proxy runs in its OWN
// operator-owned pod + Service per Loop, not as a sidecar in the agent's
// sandbox pod. NetworkPolicy and KubeArmorPolicy are pod-scoped and
// agent-sandbox allows exactly one pod per Sandbox, so a sidecar can never be
// split from the agent for egress; a separate pod is the only way D29 (the
// agent holds no key AND cannot reach the endpoint directly) becomes
// enforceable. This file replaces loop_c2_proxy_test.go's sidecar contract —
// the sidecar code and tests are removed, not kept alongside.
//
// Seam (envtest): with an endpointSecretRef, the sandbox pod has ONE container
// (agent) and no model-creds volume; a proxy Pod + Service exist, owner-referenced
// to the Loop (GC'd with it), the Secret is mounted read-only into the proxy
// pod only; the agent's COX_MODEL_BASE_URL points at the Service.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

const (
	d33ModelCredsSecret = "cox-model-creds"
	modelAPIKey         = "MODEL_API_KEY"
	modelBaseURL        = "MODEL_BASE_URL"
)

var _ = Describe("D33: proxy pod + Service per Loop (replaces the C2a sidecar)", func() {
	ctx := context.Background()

	buildLoop := func(name, ns string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: d33ModelCredsSecret,
				},
			},
		}
	}

	buildLoopNoAgent := func(name, ns string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
			},
		}
	}

	buildLoopWithSecret := func(name, ns, secretName string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: secretName,
				},
			},
		}
	}

	reconcile := func(name, ns string) {
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
	}

	getSandbox := func(name, ns string) *sandboxv1beta1.Sandbox {
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)).To(Succeed())
		return sb
	}

	containerByName := func(pod *corev1.PodSpec, name string) *corev1.Container {
		for i := range pod.Containers {
			if pod.Containers[i].Name == name {
				return &pod.Containers[i]
			}
		}
		return nil
	}

	envByName := func(c *corev1.Container) map[string]corev1.EnvVar {
		m := map[string]corev1.EnvVar{}
		for _, e := range c.Env {
			m[e.Name] = e
		}
		return m
	}

	It("replaces the C2a sidecar: sandbox has one container, proxy pod + Service exist", func() {
		ns := "d33-proxy-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "d33proxy"
		Expect(k8sClient.Create(ctx, buildLoop(name, ns))).To(Succeed())
		reconcile(name, ns)

		sb := getSandbox(name, ns)
		pod := &sb.Spec.PodTemplate.Spec

		By("having exactly ONE container (agent) on the sandbox pod — no proxy sidecar")
		Expect(pod.Containers).To(HaveLen(1), "D33: the proxy is its own pod, not a sidecar container")
		agent := containerByName(pod, agentContainerName)
		Expect(agent).NotTo(BeNil(), "the single container must be the agent")

		By("mounting NO model-creds volume on the sandbox pod")
		for _, v := range pod.Volumes {
			Expect(v.Name).NotTo(Equal(modelCredsVolume),
				"D33: the model-creds Secret belongs to the proxy pod, not the sandbox pod")
		}

		By("pointing the agent's COX_MODEL_BASE_URL at the per-Loop proxy Service")
		agentEnv := envByName(agent)
		Expect(agentEnv).To(HaveKey(coxModelBaseURL),
			"the agent must be pointed at the proxy Service via COX_MODEL_BASE_URL (D33)")
		Expect(agentEnv[coxModelBaseURL].Value).To(Equal(proxyServiceURL(name, ns)),
			"COX_MODEL_BASE_URL must be http://<loop>-proxy.<ns>.svc:8080")

		By("creating the proxy Pod, owner-referenced to the Loop")
		p := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-proxy", Namespace: ns}, p)).To(Succeed(),
			"D33: the proxy pod <loop>-proxy must exist")
		Expect(isOwnedByLoop(p, name)).To(BeTrue(),
			"D33: the proxy pod must carry the Loop controller owner ref (GC'd with the Loop)")

		By("mounting the model-creds Secret read-only into the proxy pod")
		proxyC := p.Spec.Containers[0]
		Expect(proxyC.VolumeMounts).NotTo(BeEmpty(), "the proxy pod must mount the model-creds volume")
		var foundVol *corev1.Volume
		for i := range p.Spec.Volumes {
			if p.Spec.Volumes[i].Name == modelCredsVolume {
				foundVol = &p.Spec.Volumes[i]
			}
		}
		Expect(foundVol).NotTo(BeNil(), "the proxy pod must declare the model-creds volume")
		Expect(foundVol.Secret).NotTo(BeNil(), "the model-creds volume must be a Secret volume")
		Expect(foundVol.Secret.SecretName).To(Equal(d33ModelCredsSecret),
			"the model-creds volume must reference the Loop's endpointSecretRef Secret")
		for _, vm := range proxyC.VolumeMounts {
			if vm.Name == modelCredsVolume {
				Expect(vm.ReadOnly).To(BeTrue(), "D33: the Secret mount must be read-only")
			}
		}
		By("delivering the key only as a file, not env (P2, unchanged from C2)")
		proxyEnv := envByName(&proxyC)
		Expect(proxyEnv).NotTo(HaveKey("MODEL_API_KEY"),
			"the model key must not be delivered to the proxy via env (P2: env leaks)")
		Expect(proxyEnv).NotTo(HaveKey("MODEL_BASE_URL"),
			"the model base URL must not be delivered to the proxy via env (P2)")

		By("hardening the proxy pod like the agent (D33)")
		sc := proxyC.SecurityContext
		Expect(sc).NotTo(BeNil())
		Expect(sc.AllowPrivilegeEscalation).NotTo(BeNil())
		Expect(*sc.AllowPrivilegeEscalation).To(BeFalse())
		Expect(sc.RunAsNonRoot).NotTo(BeNil())
		Expect(*sc.RunAsNonRoot).To(BeTrue())
		Expect(sc.RunAsUser).NotTo(BeNil(), "the proxy must pin its own non-root UID")
		Expect(*sc.RunAsUser).NotTo(Equal(int64(0)))
		Expect(sc.ReadOnlyRootFilesystem).NotTo(BeNil())
		Expect(*sc.ReadOnlyRootFilesystem).To(BeTrue())
		Expect(sc.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))
		Expect(sc.SeccompProfile).NotTo(BeNil())
		Expect(p.Spec.AutomountServiceAccountToken).NotTo(BeNil())
		Expect(*p.Spec.AutomountServiceAccountToken).To(BeFalse(),
			"D33: the proxy pod must not automount the SA token")
		Expect(proxyC.Resources.Limits).NotTo(BeEmpty(), "D33: the proxy must carry limits (I36 parity)")

		By("creating the proxy Service <loop>-proxy on port 8080, owner-referenced to the Loop")
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-proxy", Namespace: ns}, svc)).To(Succeed(),
			"D33: the Service <loop>-proxy must exist")
		Expect(isOwnedByLoop(svc, name)).To(BeTrue(),
			"D33: the proxy Service must be GC'd with the Loop")
		Expect(svc.Spec.Ports).To(HaveLen(1))
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))
		Expect(svc.Spec.Selector).NotTo(BeEmpty(),
			"the Service must select the proxy pod (not headless/no-selector)")
	})

	It("runs with no model: no proxy pod, no Service, no COX_MODEL_BASE_URL (P1 parity)", func() {
		ns := "d33-noref-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "d33noref"
		Expect(k8sClient.Create(ctx, buildLoopNoAgent(name, ns))).To(Succeed())
		reconcile(name, ns)

		sb := getSandbox(name, ns)
		pod := &sb.Spec.PodTemplate.Spec
		agent := containerByName(pod, agentContainerName)
		Expect(agent).NotTo(BeNil())
		agentEnv := envByName(agent)
		Expect(agentEnv).NotTo(HaveKey(coxModelBaseURL),
			"P1 parity: no COX_MODEL_BASE_URL when there is no model endpoint")

		By("not creating a proxy pod or Service")
		p := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-proxy", Namespace: ns}, p)).NotTo(Succeed())
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-proxy", Namespace: ns}, svc)).NotTo(Succeed())
	})

	It("P1 (R15): the proxy pod does NOT carry coxswain.io/loop (disjoint from the agent selector)", func() {
		ns := "d33-p1-label"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		secretName := "p1-creds"
		Expect(k8sClient.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			StringData: map[string]string{modelAPIKey: "k", modelBaseURL: "http://m.example:8000"}})).To(Succeed())
		name := "d33p1"
		Expect(k8sClient.Create(ctx, buildLoopWithSecret(name, ns, secretName))).To(Succeed())
		reconcile(name, ns)

		// The proxy pod must NOT carry coxswain.io/loop.
		p := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-proxy", Namespace: ns}, p)).To(Succeed())
		Expect(p.Labels).NotTo(HaveKey("coxswain.io/loop"),
			"P1: the proxy pod must not carry the agent's coxswain.io/loop label")
		Expect(p.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", "model-proxy"))
		Expect(p.Labels).To(HaveKeyWithValue("coxswain.io/proxy-for", name))

		// The Service selects ONLY the proxy labels.
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-proxy", Namespace: ns}, svc)).To(Succeed())
		Expect(svc.Spec.Selector).To(HaveKeyWithValue("app.kubernetes.io/component", "model-proxy"))
		Expect(svc.Spec.Selector).To(HaveKeyWithValue("coxswain.io/proxy-for", name))
		Expect(svc.Spec.Selector).NotTo(HaveKey("coxswain.io/loop"))

		// Note: the sandbox pod's coxswain.io/loop label is set by the
		// agent-sandbox controller, not by our controller, so it's not
		// checkable from envtest. The P1 guarantee is that the proxy pod
		// does NOT carry the label, which is asserted above. The KubeArmorPolicy
		// selector (C6b) uses coxswain.io/loop and will therefore match only
		// the sandbox pod, not the proxy pod.
	})

	It("P2 (R15): deleting the proxy pod triggers reconciliation and recreation", func() {
		ns := "d33-p2-recreate"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		secretName := "p2-creds"
		Expect(k8sClient.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			StringData: map[string]string{modelAPIKey: "k", modelBaseURL: "http://m.example:8000"}})).To(Succeed())
		name := "d33p2"
		Expect(k8sClient.Create(ctx, buildLoopWithSecret(name, ns, secretName))).To(Succeed())
		reconcile(name, ns)

		// The proxy pod exists.
		p := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-proxy", Namespace: ns}, p)).To(Succeed())
		originalUID := p.UID

		// Delete the proxy pod.
		Expect(k8sClient.Delete(ctx, p)).To(Succeed())

		// Reconcile the Loop: the controller must recreate the proxy pod.
		// Note: the local `reconcile` closure shadows the `reconcile` package,
		// so we create a separate reconciler and call Reconcile directly.
		// Use the local reconcile closure (which shadows the reconcile package
		// name but does exactly what we need: Reconcile the Loop and assert
		// no error).
		reconcile(name, ns)

		// The proxy pod is back (with a new UID).
		By("recreating the proxy pod after deletion")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-proxy", Namespace: ns}, p)).To(Succeed(),
			"P2: the proxy pod must be recreated after deletion")
		Expect(p.UID).NotTo(Equal(originalUID))
		Expect(p.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", "model-proxy"))
	})

	It("recreates the proxy pod when the spec hash annotation is stale (R15 round 3)", func() {
		ns := "d34-p2-drift" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "drift-creds", Namespace: ns},
			StringData: map[string]string{
				modelAPIKey:      "dummy-key",
				"MODEL_BASE_URL": "http://fake-endpoint:8000",
			},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		Expect(k8sClient.Create(ctx, buildLoopWithSecret("drift-loop", ns, "drift-creds"))).To(Succeed())

		// Reconcile: creates the sandbox + proxy pod.
		reconcile("drift-loop", ns)

		// Get the proxy pod's current hash and UID.
		proxyPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "drift-loop-proxy"}, proxyPod)).To(Succeed())
		originalUID := proxyPod.UID
		originalHash := proxyPod.Annotations[proxySpecHashAnnotation]
		Expect(originalHash).ToNot(BeEmpty())

		// Tamper with the hash annotation to simulate a stale spec.
		proxyPod.Annotations[proxySpecHashAnnotation] = "stale-hash-000000"
		Expect(k8sClient.Update(ctx, proxyPod)).To(Succeed())

		// Reconcile: the hash mismatch should trigger a delete + recreate.
		reconcile("drift-loop", ns)
		reconcile("drift-loop", ns)

		// Get the new proxy pod: it should have a new UID and the current hash.
		newPod := &corev1.Pod{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "drift-loop-proxy"}, newPod)
		}, "5s", "250ms").Should(Succeed())

		Expect(newPod.UID).ToNot(Equal(originalUID),
			"the proxy pod must be recreated (new UID) after a hash mismatch")
		Expect(newPod.Annotations[proxySpecHashAnnotation]).To(Equal(originalHash),
			"the recreated pod must carry the current hash")
	})
})

// isOwnedByLoop reports whether obj carries a controller owner reference to a
// Loop named loopName (D33: the proxy pod + Service are GC'd with the Loop).
func isOwnedByLoop(obj metav1.Object, loopName string) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller && ref.Kind == loopKind && ref.Name == loopName {
			return true
		}
	}
	return false
}

var _ = Describe("D33 proxy image", func() {
	It("defaults to the working stand-in when ProxyImage is unset", func() {
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		Expect(r.proxyImage()).To(Equal("docker.io/library/golang:1.26"))
	})

	It("honours the ProxyImage override", func() {
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ProxyImage: "my-registry/proxy:v9"}
		Expect(r.proxyImage()).To(Equal("my-registry/proxy:v9"))
	})
})
