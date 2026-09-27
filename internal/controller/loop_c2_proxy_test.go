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

// C2 (ADR-0006 item 2): the model proxy sidecar. The sandbox pod gains a
// second container (the proxy) that holds the model key (mounted ONLY into
// the proxy, never the agent), injects auth, and forwards to the configured
// endpoint. The agent talks to COX_MODEL_BASE_URL=http://localhost:<port> and
// holds no key.
//
// Seam (envtest): assert the built Sandbox pod has a proxy container; the
// model-creds secret volume is mounted into the proxy AND not into the agent;
// and the agent's env has COX_MODEL_BASE_URL set to the localhost proxy.
//
// Each It is self-contained (its own namespace, created and deleted inline)
// because an earlier Its' sandbox would leak into a shared namespace and break
// a "secret mounted into the proxy and not the agent" assertion.

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
	// modelCredsSecret is the Secret name the test's Loop's spec.agent.endpointSecretRef
	// points at (it is mounted only into the proxy container, per C2).
	modelCredsSecret = "cox-model-creds"
)

var _ = Describe("C2: model proxy sidecar (ADR-0006)", func() {
	ctx := context.Background()

	buildLoop := func(name, ns string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Agent: coxv1alpha1.AgentConfig{
					Image:             "example.com/coxswain/runner:v1",
					Model:             "local-model",
					EndpointSecretRef: modelCredsSecret,
				},
			},
		}
	}

	// buildLoopNoAgent is a Loop with NO spec.agent at all (the README sample):
	// the agent runs with no model, so there is no proxy / no key.
	buildLoopNoAgent := func(name, ns string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
			},
		}
	}

	reconcileToSandbox := func(r *LoopReconciler, name, ns string) *sandboxv1beta1.Sandbox {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)).To(Succeed())
		return sb
	}

	containerByName := func(sb *sandboxv1beta1.Sandbox, name string) *corev1.Container {
		for i := range sb.Spec.PodTemplate.Spec.Containers {
			if sb.Spec.PodTemplate.Spec.Containers[i].Name == name {
				return &sb.Spec.PodTemplate.Spec.Containers[i]
			}
		}
		return nil
	}

	mountedVolumeNames := func(c *corev1.Container) []string {
		names := make([]string, 0, len(c.VolumeMounts))
		for _, vm := range c.VolumeMounts {
			names = append(names, vm.Name)
		}
		return names
	}

	envByName := func(c *corev1.Container) map[string]corev1.EnvVar {
		m := map[string]corev1.EnvVar{}
		for _, e := range c.Env {
			m[e.Name] = e
		}
		return m
	}

	It("mounts the model key into the proxy and not the agent, and points the agent at the proxy (C2a: ref set)", func() {
		ns := "c2-proxy-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "c2proxy"
		Expect(k8sClient.Create(ctx, buildLoop(name, ns))).To(Succeed())
		sb := reconcileToSandbox(&LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}, name, ns)

		By("having a proxy container on the sandbox pod")
		proxy := containerByName(sb, proxyContainerName)
		Expect(proxy).NotTo(BeNil(), "the sandbox pod must have a proxy sidecar container (C2)")

		By("mounting the model-creds secret volume into the proxy")
		Expect(mountedVolumeNames(proxy)).To(ContainElement(modelCredsVolume),
			"the proxy must mount the model-creds secret volume (it holds the key)")

		By("NOT mounting the model-creds secret volume into the agent")
		agent := containerByName(sb, agentContainerName)
		Expect(agent).NotTo(BeNil(), "the sandbox pod must still have the agent container")
		Expect(mountedVolumeNames(agent)).NotTo(ContainElement(modelCredsVolume),
			"the agent must not mount the model-creds secret volume (C2: the key lives only in the proxy)")

		By("setting the agent's COX_MODEL_BASE_URL to the localhost proxy")
		agentEnv := envByName(agent)
		Expect(agentEnv).To(HaveKey(coxModelBaseURL),
			"the agent must be pointed at the local proxy via COX_MODEL_BASE_URL (C2)")
		Expect(agentEnv[coxModelBaseURL].Value).To(Equal(localhostProxyBaseURL),
			"COX_MODEL_BASE_URL must be the localhost proxy URL (the agent holds no key and talks to localhost)")

		By("carrying the model-creds volume as a Secret in the pod spec")
		pod := &sb.Spec.PodTemplate.Spec
		var credsVol *corev1.Volume
		for i := range pod.Volumes {
			if pod.Volumes[i].Name == modelCredsVolume {
				credsVol = &pod.Volumes[i]
			}
		}
		Expect(credsVol).NotTo(BeNil(), "the pod must declare the model-creds volume")
		Expect(credsVol.Secret).NotTo(BeNil(), "the model-creds volume must be a Secret volume")
		Expect(credsVol.Secret.SecretName).To(Equal(modelCredsSecret),
			"the model-creds volume must reference the Loop's endpointSecretRef Secret")

		By("delivering the key only as a file, not env (P2)")
		// The key must NOT be delivered via env (it leaks to child processes,
		// crash dumps, /proc/<pid>/environ). Only the read-only file mount.
		proxyEnv := envByName(proxy)
		Expect(proxyEnv).NotTo(HaveKey("MODEL_API_KEY"),
			"the model key must not be delivered to the proxy via env (P2: env leaks)")
		Expect(proxyEnv).NotTo(HaveKey("MODEL_BASE_URL"),
			"the model base URL must not be delivered to the proxy via env (P2)")

		By("giving the proxy its own UID, not the agent's (P2)")
		proxySC := proxy.SecurityContext
		Expect(proxySC).NotTo(BeNil())
		Expect(proxySC.RunAsUser).NotTo(BeNil(), "the proxy must pin its own UID (P2)")
		Expect(*proxySC.RunAsUser).NotTo(Equal(int64(65532)),
			"the proxy must NOT run as the agent's UID (a shared PID ns / volume could expose the key)")

		By("not sharing the process namespace (P2)")
		Expect(pod.ShareProcessNamespace).NotTo(BeNil(),
			"shareProcessNamespace must be explicit (P2)")
		Expect(*pod.ShareProcessNamespace).To(BeFalse(),
			"the pod must not share the process namespace (the key would be readable in /proc/<pid>/environ)")
	})

	It("builds a VALID pod with no proxy when endpointSecretRef is unset (P1: no half-configured proxy)", func() {
		ns := "c2-noref-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "c2noref"
		// A Loop with NO spec.agent (like the README sample): the agent runs with
		// no model. There must be NO proxy container, no model-creds volume, and
		// no COX_MODEL_BASE_URL — never a half-configured proxy (P1: the pod must
		// be valid, not InvalidConfiguration on secretKeyRef.name "").
		Expect(k8sClient.Create(ctx, buildLoopNoAgent(name, ns))).To(Succeed())
		sb := reconcileToSandbox(&LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}, name, ns)

		pod := &sb.Spec.PodTemplate.Spec
		By("NOT adding a proxy container when there is no model endpoint")
		Expect(containerByName(sb, proxyContainerName)).To(BeNil(),
			"no proxy container when endpointSecretRef is unset (P1)")

		By("NOT adding the model-creds volume")
		for _, v := range pod.Volumes {
			Expect(v.Name).NotTo(Equal(modelCredsVolume),
				"no model-creds volume when endpointSecretRef is unset (P1)")
		}

		By("NOT setting COX_MODEL_BASE_URL on the agent")
		agent := containerByName(sb, agentContainerName)
		Expect(agent).NotTo(BeNil(), "the agent container must still be present")
		agentEnv := envByName(agent)
		Expect(agentEnv).NotTo(HaveKey(coxModelBaseURL),
			"COX_MODEL_BASE_URL must not be set when there is no proxy (P1)")

		By("leaving every container with a valid env (no empty secretKeyRef)")
		for i := range pod.Containers {
			for _, e := range pod.Containers[i].Env {
				if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
					Expect(e.ValueFrom.SecretKeyRef.Name).NotTo(BeEmpty(),
						"container %q must not reference an empty secret name (P1: pod was InvalidConfiguration on kind)",
						pod.Containers[i].Name)
				}
			}
		}
	})
})
