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

// C1 (ADR-0006): the operator builds the Sandbox pod hardened and credential-
// free. The agent container must carry the full pod-hardening set, have no
// ServiceAccount token automounted, and hold no secret volume — the model key
// lives only in the proxy sidecar (C2). This is an envtest seam: it reads the
// Sandbox object the operator actually built, not a mock.
//
// Each It is self-contained (its own namespace, created and deleted inline)
// because earlier Its' sandboxes would leak into a shared BeforeEach namespace
// and break an "exactly one container / no secret volume" assertion.
var _ = Describe("C1: sandbox pod hardening + spec.agent (ADR-0006)", func() {
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
					EndpointSecretRef: "cox-model-creds",
				},
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

	agentContainer := func(sb *sandboxv1beta1.Sandbox) *corev1.Container {
		for i := range sb.Spec.PodTemplate.Spec.Containers {
			if sb.Spec.PodTemplate.Spec.Containers[i].Name == agentContainerName {
				return &sb.Spec.PodTemplate.Spec.Containers[i]
			}
		}
		Fail("no agent container on the built sandbox")
		return nil
	}

	It("hardens the agent container and automounts no token", func() {
		ns := "c1-hardening-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "hardening"
		Expect(k8sClient.Create(ctx, buildLoop(name, ns))).To(Succeed())
		sb := reconcileToSandbox(&LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}, name, ns)

		pod := &sb.Spec.PodTemplate.Spec
		By("automounting no ServiceAccount token on the pod")
		f := false
		Expect(pod.AutomountServiceAccountToken).NotTo(BeNil(), "the sandbox pod must not automount an SA token")
		Expect(*pod.AutomountServiceAccountToken).To(Equal(f), "automountServiceAccountToken must be explicitly false")

		agent := agentContainer(sb)
		sc := agent.SecurityContext
		By("setting runAsNonRoot, dropping all caps, and denying privilege escalation")
		Expect(sc).NotTo(BeNil())
		Expect(sc.AllowPrivilegeEscalation).NotTo(BeNil())
		Expect(*sc.AllowPrivilegeEscalation).To(BeFalse(), "allowPrivilegeEscalation must be false")
		Expect(sc.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")),
			"the agent must drop all capabilities")
		Expect(sc.RunAsNonRoot).NotTo(BeNil())
		Expect(*sc.RunAsNonRoot).To(BeTrue(), "runAsNonRoot must be true")
		Expect(sc.ReadOnlyRootFilesystem).NotTo(BeNil())
		Expect(*sc.ReadOnlyRootFilesystem).To(BeTrue(),
			"read-only rootfs must be on, with /workspace + scratch as emptyDir volumes")

		// I35 (R10): runAsNonRoot without a UID breaks the default golang image on
		// kind (CreateContainerConfigError). The agent must pin a UID/GID and the
		// pod must set fsGroup so /workspace + /scratch are writable by it; HOME and
		// TMPDIR point at the writable scratch (a read-only rootfs otherwise breaks
		// every tool that writes ~/.cache or /tmp).
		By("pinning a non-root UID/GID and a fsGroup (I35)")
		Expect(sc.RunAsUser).NotTo(BeNil())
		Expect(*sc.RunAsUser).To(BeNumerically("==", 65532), "runAsUser must be the platform non-root UID (65532)")
		Expect(sc.RunAsGroup).NotTo(BeNil())
		Expect(*sc.RunAsGroup).To(BeNumerically("==", 65532), "runAsGroup must be the platform non-root GID (65532)")
		Expect(pod.SecurityContext).NotTo(BeNil())
		Expect(pod.SecurityContext.FSGroup).NotTo(BeNil())
		Expect(*pod.SecurityContext.FSGroup).To(BeNumerically("==", 65532), "fsGroup must match the agent UID so emptyDir volumes are writable")

		By("pointing HOME and TMPDIR at the writable scratch (I35)")
		envByName := map[string]string{}
		for _, e := range agent.Env {
			envByName[e.Name] = e.Value
		}
		Expect(envByName["HOME"]).To(Equal("/scratch"), "HOME must be on the writable scratch volume")
		Expect(envByName["TMPDIR"]).NotTo(BeEmpty(), "TMPDIR must be set")
		Expect(envByName["TMPDIR"]).To(HavePrefix("/scratch"), "TMPDIR must be on the writable scratch volume")

		// I36 (R10): ADR-0006 item 4 requires CPU/memory limits; C1 set none, so one
		// agent could starve the node. Also an ephemeral-storage limit — /workspace
		// and /scratch are emptyDir and an agent can fill the node's disk.
		By("setting CPU, memory, and ephemeral-storage limits (I36)")
		Expect(agent.Resources.Limits).NotTo(BeEmpty(), "the agent must have resource limits (I36)")
		Expect(agent.Resources.Limits).To(HaveKey(corev1.ResourceCPU), "a CPU limit is required (I36)")
		Expect(agent.Resources.Limits).To(HaveKey(corev1.ResourceMemory), "a memory limit is required (I36)")
		Expect(agent.Resources.Limits).To(HaveKey(corev1.ResourceEphemeralStorage), "an ephemeral-storage limit is required (I36)")

		Expect(sc.SeccompProfile).NotTo(BeNil())
		Expect(sc.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault),
			"seccomp must be RuntimeDefault")

		By("mounting no secret volume into the agent container (the key lives in the proxy)")
		for _, vm := range agent.VolumeMounts {
			Expect(vm.Name).NotTo(Equal("cox-model-creds"),
				"the agent must not mount the model-credential secret")
		}
	})

	It("uses spec.agent.image for the agent container when set", func() {
		ns := "c1-agentimage-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "agentimage"
		Expect(k8sClient.Create(ctx, buildLoop(name, ns))).To(Succeed())
		sb := reconcileToSandbox(&LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}, name, ns)

		agent := agentContainer(sb)
		Expect(agent.Image).To(Equal("example.com/coxswain/runner:v1"),
			"the agent container must use spec.agent.image when set")
	})
})
