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

// D41c (ADR-0008, plan section D41c): the operator provisions, gates and
// drift-corrects one tool proxy pod + Service per tool in the effective
// policy union. ensureProxy/ensureEgressProxy with the tool loop.
//
// Labels are DISJOINT from the model proxy, egress proxy and agent pods: the
// tool proxy gets app.kubernetes.io/component=tool-proxy +
// coxswain.io/tool-proxy-for + coxswain.io/tool (never coxswain.io/loop, the
// KubeArmorPolicy selector; never the agent component label).
//
// Credential: when credentialSecretRef is set, the Secret is mounted
// read-only into the tool proxy container ONLY (/tool-cred/<name>, mode
// 0444, TOOL_CREDENTIAL_FILE=/tool-cred/<name>/<key>); the sandbox pod spec
// never carries a volume or env referencing it (the zero-credential
// property, the envtest-provable half).
//
// Gates: the owned+Ready gate (D35a pattern, order-independent like the
// I42b/I42c-review gates) holds the sandbox Suspended until every expected
// tool proxy pod is owned + Ready; a foreign <loop>-tool-<name> pod →
// ProxyConflict=True/ForeignToolProxy (NOT deleted, I2); the spec-hash
// drift annotation (coxswain.io/tool-proxy-spec-hash) drives
// delete-and-recreate.
//
// I43 norm: the same-Loop update/delete specs re-read objects from the API
// server between steps and assert the object is UPDATED (pod recreated via
// the hash) / DELETED (cleanup), not just created.

import (
	"context"
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/policy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// d41c test constants. In-cluster .svc repo (like i42b): the specs are
	// about the effective tool union; an external repo's workspace init
	// clone would (S6) add its own egress-proxy allowlist host and create the
	// egress proxy even with no network allows.
	d41cTestRepo         = "http://gitea.samples.svc:3000/samples/gocli.git"
	d41cPolicyName       = "d41c-pol"
	d41cPodCIDR          = "10.244.0.0/16"
	d41cServiceCIDR      = "10.96.0.0/12"
	d41cToolProxyImg     = "coxswain-tool-proxy:standin"
	d41cCredSecret       = "gh-cred"
	d41cCredKey          = "token"
	d41cToolName         = "gh"
	d41cUpstreamA        = "https://api.github.com"
	d41cUpstreamB        = "https://api.github.example"
	d41cConflict         = "ProxyConflict"
	d41cForeignReason    = "ForeignToolProxy"
	d41cForeignContainer = "foreign"
)

func newD41cReconciler() *LoopReconciler {
	return &LoopReconciler{
		Client:         k8sClient,
		Scheme:         k8sClient.Scheme(),
		PodCIDR:        d41cPodCIDR,
		ServiceCIDR:    d41cServiceCIDR,
		ToolProxyImage: d41cToolProxyImg,
		// AllowUnenforced: the tool-proxy gate is independent of the D30
		// enforcement gate. In envtest the Enforcer is nil, so D30 would
		// hold the sandbox Suspended. AllowUnenforced=true isolates the
		// tool-proxy gate (D35a pattern) from the D30 gate (same as I42b).
		AllowUnenforced: true,
	}
}

// d41cToolGH returns the plan's canonical tool definition.
func d41cToolGH(upstream string) coxv1alpha1.ToolSpec {
	return coxv1alpha1.ToolSpec{
		Name:     d41cToolName,
		Upstream: upstream,
		CredentialSecretRef: coxv1alpha1.CredentialSecretRef{
			Name: d41cCredSecret,
			Key:  d41cCredKey,
		},
		Rules: []coxv1alpha1.ToolRule{
			{Methods: []string{httpMethodGet}, Paths: []string{"/repos/acme/*"}},
		},
	}
}

var _ = Describe("D41c: ensureToolProxies", func() {
	var (
		ctx context.Context
		r   *LoopReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		r = newD41cReconciler()
	})

	buildLoop := func(name, ns string, policyRefs []string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "d41c test",
				Workspace:  coxv1alpha1.Workspace{Repo: d41cTestRepo, Ref: loopRef},
				PolicyRefs: policyRefs,
				Agent:      coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
			},
		}
	}

	reconcileLoop := func(name, ns string) {
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
		})
		Expect(err).ToNot(HaveOccurred())
	}

	// createToolNS creates a fresh namespace with the credential Secret.
	createToolNS := func() string {
		ns := "d41c-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: d41cCredSecret, Namespace: ns},
			StringData: map[string]string{d41cCredKey: "test-token"},
		})).To(Succeed())
		return ns
	}

	getPod := func(ns, name string) *corev1.Pod {
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, pod)).To(Succeed())
		return pod
	}

	const (
		d41cContainersReady = "ContainersReady"
		d41cConfigMapKind   = "ConfigMap"
		d41cToolUpstreamEnv = "TOOL_UPSTREAM"
	)

	getSandbox := func(ns, loopName string) *sandboxv1beta1.Sandbox {
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: loopName + "-sandbox"}, sb)).To(Succeed())
		return sb
	}

	markPodReady := func(pod *corev1.Pod) {
		pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
			Reason: d41cContainersReady,
		})
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  policy.ComponentToolProxyLabel,
			Ready: true,
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	const d41cConflictLoop = "conflict-loop"
	toolProxyPod := func(loopName string) string { return loopName + "-tool-" + d41cToolName }

	// spec 1: no tools → nothing created.
	It("spec 1: no tools → no tool proxy pod or Service", func() {
		ns := createToolNS()
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d41cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42eExternalHost}},
		})).To(Succeed())

		loop := buildLoop("notools-loop", ns, []string{d41cPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop("notools-loop", ns)

		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "notools-loop-tool-" + d41cToolName}, pod)).
			To(HaveOccurred(), "no tool proxy pod when there are no tools")
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "notools-loop-tool-" + d41cToolName}, svc)).
			To(HaveOccurred(), "no tool proxy Service when there are no tools")
	})

	// spec 2: one tool → pod + Service; sandbox Suspended (not Ready yet).
	It("spec 2: one tool → pod + Service created, sandbox Suspended (not Ready yet)", func() {
		ns := createToolNS()
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d41cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41cUpstreamA)}},
		})).To(Succeed())

		loop := buildLoop("tool1-loop", ns, []string{d41cPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop("tool1-loop", ns)

		pod := getPod(ns, toolProxyPod("tool1-loop"))
		Expect(pod.Spec.SecurityContext).ToNot(BeNil())
		Expect(*pod.Spec.SecurityContext.RunAsUser).To(BeEquivalentTo(65535),
			"the tool proxy pod runs UID 65535 (distinct from agent 65532, model proxy 65533, egress proxy 65534)")
		Expect(metav1.IsControlledBy(pod, loop)).To(BeTrue(), "the tool proxy pod is owned by the Loop")

		// The TOOL_* env.
		var container corev1.Container
		for _, c := range pod.Spec.Containers {
			container = c
		}
		envs := map[string]string{}
		for _, e := range container.Env {
			envs[e.Name] = e.Value
		}
		Expect(envs["TOOL_NAME"]).To(Equal(d41cToolName))
		Expect(envs[d41cToolUpstreamEnv]).To(Equal(d41cUpstreamA))
		Expect(envs["TOOL_CREDENTIAL_FILE"]).To(Equal("/tool-cred/credential"))
		Expect(envs["LOOP_NAME"]).To(Equal("tool1-loop"))
		Expect(envs["LOOP_NAMESPACE"]).To(Equal(ns))
		Expect(envs["POD_CIDR"]).To(Equal(d41cPodCIDR))
		Expect(envs["SERVICE_CIDR"]).To(Equal(d41cServiceCIDR))
		var rules []coxv1alpha1.ToolRule
		Expect(json.Unmarshal([]byte(envs["TOOL_RULES_JSON"]), &rules)).To(Succeed())
		Expect(rules).To(HaveLen(1))
		Expect(rules[0].Methods).To(ConsistOf(httpMethodGet))

		// The TOOL_POLICY_HASH env is the effective policy hash (ADR-0008: a
		// record is attributable to the exact policy generation).
		Expect(envs["TOOL_POLICY_HASH"]).ToNot(BeEmpty(),
			"the TOOL_POLICY_HASH env must carry the effective policy hash")

		// Liveness/readiness TCP 8080.
		Expect(container.LivenessProbe).ToNot(BeNil())
		Expect(container.LivenessProbe.TCPSocket).ToNot(BeNil())
		Expect(container.LivenessProbe.TCPSocket.Port.IntValue()).To(Equal(8080))
		Expect(container.ReadinessProbe).ToNot(BeNil())
		Expect(container.ReadinessProbe.TCPSocket).ToNot(BeNil())
		Expect(container.ReadinessProbe.TCPSocket.Port.IntValue()).To(Equal(8080))

		// No SA token (the plan: automountServiceAccountToken: false).
		Expect(pod.Spec.AutomountServiceAccountToken).ToNot(BeNil())
		Expect(*pod.Spec.AutomountServiceAccountToken).To(BeFalse())

		// Service: port 8080, matching selector.
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: toolProxyPod("tool1-loop")}, svc)).To(Succeed())
		Expect(svc.Spec.Ports).To(HaveLen(1))
		Expect(int(svc.Spec.Ports[0].Port)).To(Equal(8080))
		Expect(svc.Spec.Selector["coxswain.io/tool"]).To(Equal(d41cToolName))
		Expect(metav1.IsControlledBy(svc, loop)).To(BeTrue())

		// Sandbox Suspended (the tool proxy pod is not Ready yet — the
		// owned+Ready gate, I49 in-progress state).
		sb := getSandbox(ns, "tool1-loop")
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"the sandbox must be Suspended until the tool proxy pod is Ready")
	})

	// spec 3: credential mount (proxy only, never the sandbox).
	It("spec 3: credential Secret mounted into the proxy only; the sandbox pod carries no tool credential", func() {
		ns := createToolNS()
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d41cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41cUpstreamA)}},
		})).To(Succeed())

		loop := buildLoop("cred-loop", ns, []string{d41cPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop("cred-loop", ns)

		pod := getPod(ns, toolProxyPod("cred-loop"))
		// The volume carries the Secret with mode 0444.
		var vol corev1.Volume
		found := false
		for _, v := range pod.Spec.Volumes {
			if v.Secret != nil && v.Secret.SecretName == d41cCredSecret {
				vol = v
				found = true
			}
		}
		Expect(found).To(BeTrue(), "the tool proxy pod must carry a Secret volume for "+d41cCredSecret)
		Expect(vol.Secret.DefaultMode).ToNot(BeNil())
		Expect(*vol.Secret.DefaultMode).To(BeEquivalentTo(0o444), "the credential Secret mounts mode 0444")
		// The volume projects only the referenced key (Items):
		// /tool-cred/credential, never the Secret's other keys.
		Expect(vol.Secret.Items).To(HaveLen(1), "the Secret volume must project exactly one key")
		Expect(vol.Secret.Items[0].Key).To(Equal(d41cCredKey),
			"the projected key must be the CredentialSecretRef.Key")
		Expect(vol.Secret.Items[0].Path).To(Equal("credential"),
			"the projected path must be 'credential'")
		// Mounted at /tool-cred, read-only, NO SubPath.
		var mount corev1.VolumeMount
		mountFound := false
		for _, c := range pod.Spec.Containers {
			for _, m := range c.VolumeMounts {
				if m.Name == vol.Name {
					mount = m
					mountFound = true
				}
			}
		}
		Expect(mountFound).To(BeTrue(), "the credential volume must be mounted into the tool proxy container")
		Expect(mount.MountPath).To(Equal("/tool-cred"),
			"the credential mounts at /tool-cred")
		Expect(mount.SubPath).To(BeEmpty(),
			"no SubPath: the projected key lands at /tool-cred/credential")
		Expect(mount.ReadOnly).To(BeTrue())
		// TOOL_CREDENTIAL_FILE must point to the actual mount path.
		var credEnv *corev1.EnvVar
		for i := range pod.Spec.Containers[0].Env {
			if pod.Spec.Containers[0].Env[i].Name == "TOOL_CREDENTIAL_FILE" {
				credEnv = &pod.Spec.Containers[0].Env[i]
			}
		}
		Expect(credEnv).ToNot(BeNil(), "TOOL_CREDENTIAL_FILE must be set")
		Expect(credEnv.Value).To(Equal(mount.MountPath+"/"+vol.Secret.Items[0].Path),
			"TOOL_CREDENTIAL_FILE must be mount.MountPath + '/' + items[0].Path")

		// The zero-credential property (the envtest-provable half): the
		// sandbox pod spec carries NO volume referencing the credential
		// Secret and NO env referencing it.
		sb := getSandbox(ns, "cred-loop")
		for _, v := range sb.Spec.PodTemplate.Spec.Volumes {
			if v.Secret != nil {
				Expect(v.Secret.SecretName).ToNot(Equal(d41cCredSecret),
					"the sandbox pod must NOT carry the tool credential Secret (zero credential)")
			}
		}
		for _, c := range sb.Spec.PodTemplate.Spec.Containers {
			for _, e := range c.Env {
				Expect(e.Value).ToNot(ContainSubstring(d41cCredSecret),
					"the sandbox pod env must NOT reference the tool credential Secret")
			}
		}
	})

	// spec 4: owned+Ready gate — not Ready → Suspended.
	It("spec 4: tool proxy pod present but not Ready → sandbox Suspended", func() {
		ns := createToolNS()
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d41cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41cUpstreamA)}},
		})).To(Succeed())

		loop := buildLoop("notready-loop", ns, []string{d41cPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop("notready-loop", ns)

		pod := getPod(ns, toolProxyPod("notready-loop"))
		// The pod exists but is not Ready (fresh pod, no Ready condition).
		Expect(isPodReady(pod)).To(BeFalse())

		sb := getSandbox(ns, "notready-loop")
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"the sandbox must be Suspended while the tool proxy pod is not Ready")

		// Reconcile AGAIN from the API server (the gate must read live
		// objects, order-independent like the I42b/I42c-review gates).
		reconcileLoop("notready-loop", ns)
		sb = getSandbox(ns, "notready-loop")
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"the sandbox must stay Suspended on re-reconcile (owned+Ready gate reads live objects)")
	})

	// spec 5: owned+Ready gate — Ready → Running.
	It("spec 5: tool proxy pod owned + Ready → sandbox Running", func() {
		ns := createToolNS()
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d41cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41cUpstreamA)}},
		})).To(Succeed())

		loop := buildLoop("ready-loop", ns, []string{d41cPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop("ready-loop", ns)

		pod := getPod(ns, toolProxyPod("ready-loop"))
		markPodReady(pod)

		// Reconcile from the API server: the gate now passes.
		reconcileLoop("ready-loop", ns)
		sb := getSandbox(ns, "ready-loop")
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeRunning),
			"the sandbox must be Running once the tool proxy pod is owned and Ready")
	})

	// spec 6: foreign pod → ProxyConflict + Suspended, foreign pod not deleted.
	It("spec 6: foreign tool proxy pod → ProxyConflict/ForeignToolProxy, sandbox Suspended, foreign pod kept", func() {
		ns := createToolNS()
		// A FOREIGN pod occupies the tool proxy name before the Loop exists.
		loaner := "foreign-ctl"
		Expect(k8sClient.Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      toolProxyPod("foreign-loop"),
				Namespace: ns,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "v1",
					Kind:       d41cConfigMapKind,
					Name:       loaner,
					UID:        "00000000-0000-0000-0000-000000000000",
					Controller: func() *bool { b := true; return &b }(),
				}},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: d41cForeignContainer, Image: i42bForeignImage}}},
		})).To(Succeed())

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d41cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41cUpstreamA)}},
		})).To(Succeed())

		loop := buildLoop("foreign-loop", ns, []string{d41cPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop("foreign-loop", ns)

		// The foreign pod must still exist (NOT deleted — I2 never-take-over).
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: toolProxyPod("foreign-loop")}, &corev1.Pod{})
		Expect(err).ToNot(HaveOccurred(), "the foreign tool proxy pod must NOT be deleted (I2)") // ProxyConflict=True/ForeignToolProxy.
		gotLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "foreign-loop"}, gotLoop)).To(Succeed())
		var conflict *metav1.Condition
		for i := range gotLoop.Status.Conditions {
			if gotLoop.Status.Conditions[i].Type == d41cConflict {
				conflict = &gotLoop.Status.Conditions[i]
			}
		}
		Expect(conflict).ToNot(BeNil(), "ProxyConflict must be set for a foreign tool proxy pod")
		Expect(conflict.Status).To(Equal(metav1.ConditionTrue))
		Expect(conflict.Reason).To(Equal(d41cForeignReason))

		// Sandbox Suspended (the owned+Ready gate holds on the foreign pod).
		sb := getSandbox(ns, "foreign-loop")
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"the sandbox must stay Suspended while a foreign pod occupies the tool proxy name")
	})

	// spec 7: drift — spec change → recreate (new hash, new env).
	It("spec 7: tool upstream change → pod recreated via the spec-hash, Service name stable", func() {
		ns := createToolNS()
		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d41cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41cUpstreamA)}},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed())

		loop := buildLoop("drift-loop", ns, []string{d41cPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop("drift-loop", ns)

		oldPod := getPod(ns, toolProxyPod("drift-loop"))
		oldUpstream := oldPod.Spec.Containers[0].Env
		var oldUp *corev1.EnvVar
		for i := range oldUpstream {
			if oldUpstream[i].Name == d41cToolUpstreamEnv {
				oldUp = &oldUpstream[i]
			}
		}
		Expect(oldUp).ToNot(BeNil())
		Expect(oldUp.Value).To(Equal(d41cUpstreamA))

		// Change the tool's upstream (same policy generation → new hash via a
		// new AgentPolicy update, as in I42b spec 6).
		ap.Spec.Tools = []coxv1alpha1.ToolSpec{d41cToolGH(d41cUpstreamB)}
		Expect(k8sClient.Update(ctx, ap)).To(Succeed())

		// Reconcile from the API server: the spec-hash mismatches → the old
		// pod is deleted. Recreation happens on the NEXT reconcile (the
		// delete+recreate pattern, I42b spec 6): the envtest has no kubelet,
		// so the Owns(Pod) watch that would fire the recreate on deletion is
		// not active; a third Reconcile creates the new pod.
		reconcileLoop("drift-loop", ns)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: toolProxyPod("drift-loop")}, &corev1.Pod{})).
			To(HaveOccurred(), "the drifted pod must be deleted after the first reconcile")
		reconcileLoop("drift-loop", ns)
		newPod := getPod(ns, toolProxyPod("drift-loop"))
		var newUp *corev1.EnvVar
		for i := range newPod.Spec.Containers[0].Env {
			if newPod.Spec.Containers[0].Env[i].Name == d41cToolUpstreamEnv {
				newUp = &newPod.Spec.Containers[0].Env[i]
			}
		}
		Expect(newUp).ToNot(BeNil())
		Expect(newUp.Value).To(Equal(d41cUpstreamB), "the recreated pod must carry the updated TOOL_UPSTREAM")

		// The Service name is stable across the recreate (the agent's env URL
		// depends on it).
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: toolProxyPod("drift-loop")}, svc)).To(Succeed(),
			"the tool proxy Service name must be stable across pod recreates")
	})

	// spec 8: label non-collision (regression guard, same as D33/I42b spec 7).
	It("spec 8: tool proxy labels do not match the agent KubeArmorPolicy selector", func() {
		ns := createToolNS()
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d41cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41cUpstreamA)}},
		})).To(Succeed())

		loop := buildLoop("label-loop", ns, []string{d41cPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop("label-loop", ns)

		pod := getPod(ns, toolProxyPod("label-loop"))
		Expect(pod.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "coxswain-tool-proxy"))
		Expect(pod.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "label-loop"))
		Expect(pod.Labels).To(HaveKeyWithValue(policy.ComponentLabelKey, policy.ComponentToolProxyLabel))
		Expect(pod.Labels).To(HaveKeyWithValue("app.kubernetes.io/part-of", "coxswain"))
		Expect(pod.Labels).To(HaveKeyWithValue("coxswain.io/tool-proxy-for", "label-loop"))
		Expect(pod.Labels).To(HaveKeyWithValue("coxswain.io/tool", d41cToolName))
		// The disjoint label set (the agent's fences must never bind to a tool
		// proxy pod):
		Expect(pod.Labels).ToNot(HaveKey("coxswain.io/loop"),
			"the tool proxy pod must NOT carry coxswain.io/loop (the agent KubeArmorPolicy selector)")
		Expect(pod.Labels).ToNot(HaveKeyWithValue(policy.ComponentLabelKey, "agent"),
			"the tool proxy pod must NOT carry the agent component label")
		Expect(pod.Labels).ToNot(HaveKeyWithValue(policy.ComponentLabelKey, policy.ComponentProxyLabel))
		Expect(pod.Labels).ToNot(HaveKeyWithValue(policy.ComponentLabelKey, policy.ComponentEgressProxyLabel))
	})

	// spec 9: union semantics + conflict fail-closed.
	It("spec 9: union conflict → PolicyValid=False/ToolConflict, no tool proxy; identical definitions → one proxy", func() {
		ns := createToolNS()

		// Two referenced policies both defining tool gh with different
		// upstreams → ToolConflict.
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "d41c-conflict-a", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41cUpstreamA)}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "d41c-conflict-b", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41cUpstreamB)}},
		})).To(Succeed())

		loop := buildLoop(d41cConflictLoop, ns, []string{"d41c-conflict-a", "d41c-conflict-b"})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop(d41cConflictLoop, ns)

		gotLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: d41cConflictLoop}, gotLoop)).To(Succeed())
		var policyValid *metav1.Condition
		for i := range gotLoop.Status.Conditions {
			if gotLoop.Status.Conditions[i].Type == "PolicyValid" {
				policyValid = &gotLoop.Status.Conditions[i]
			}
		}
		Expect(policyValid).ToNot(BeNil(), "the Loop must have a PolicyValid condition")
		Expect(policyValid.Status).To(Equal(metav1.ConditionFalse))
		Expect(policyValid.Reason).To(Equal("ToolConflict"),
			"a tool union conflict must fail closed with reason ToolConflict")
		Expect(policyValid.Message).To(ContainSubstring(d41cToolName))

		// No tool proxy pod created (validation fails before ensure*).
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: toolProxyPod(d41cConflictLoop)}, pod)).
			To(HaveOccurred(), "no tool proxy may be created for a ToolConflict Loop")

		// Identical definitions → one proxy (union dedup).
		ns2 := createToolNS()
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "d41c-ident-a", Namespace: ns2},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41cUpstreamA)}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "d41c-ident-b", Namespace: ns2},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41cUpstreamA)}},
		})).To(Succeed())
		loop2 := buildLoop("ident-loop", ns2, []string{"d41c-ident-a", "d41c-ident-b"})
		Expect(k8sClient.Create(ctx, loop2)).To(Succeed())
		reconcileLoop("ident-loop", ns2)
		pod2 := getPod(ns2, toolProxyPod("ident-loop"))
		Expect(pod2.Spec.Containers[0].Env).To(ContainElement(corev1.EnvVar{Name: d41cToolUpstreamEnv, Value: d41cUpstreamA}),
			"identical tool definitions across policies must dedup to one proxy")
	})

	// spec 10: same-Loop update/delete (I43 norm): re-read from the API
	// server; the pod is RECREATED (update) and DELETED (delete).
	It("spec 10: same-Loop update + delete (I43 norm)", func() {
		ns := createToolNS()
		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d41cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41cUpstreamA)}},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed())

		loop := buildLoop("updel-loop", ns, []string{d41cPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop("updel-loop", ns)

		// (a) Running transition: mark the tool proxy Ready and re-reconcile
		// from the API server.
		pod := getPod(ns, toolProxyPod("updel-loop"))
		markPodReady(pod)
		reconcileLoop("updel-loop", ns)
		sb := getSandbox(ns, "updel-loop")
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeRunning),
			"the sandbox must reach Running before the update step (I43: update, not just create)")

		// (b) Update the tool spec (new upstream) and re-reconcile from the
		// API server → the old pod is deleted (spec-hash mismatch). Recreation
		// happens on the NEXT reconcile (the delete+recreate pattern, I42b
		// spec 6): the envtest has no kubelet, so a third Reconcile creates
		// the new pod.
		ap.Spec.Tools = []coxv1alpha1.ToolSpec{d41cToolGH(d41cUpstreamB)}
		Expect(k8sClient.Update(ctx, ap)).To(Succeed())
		oldRV := getPod(ns, toolProxyPod("updel-loop")).ResourceVersion
		reconcileLoop("updel-loop", ns)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: toolProxyPod("updel-loop")}, &corev1.Pod{})).
			To(HaveOccurred(), "the drifted pod must be deleted after the first reconcile (I43: updated, not just created)")
		reconcileLoop("updel-loop", ns)
		newPod := getPod(ns, toolProxyPod("updel-loop"))
		Expect(newPod.ResourceVersion).ToNot(Equal(oldRV), "the pod must be RECREATED (new resourceVersion), not updated in place (I43)")
		var updatedUp *corev1.EnvVar
		for i := range newPod.Spec.Containers[0].Env {
			if newPod.Spec.Containers[0].Env[i].Name == d41cToolUpstreamEnv {
				updatedUp = &newPod.Spec.Containers[0].Env[i]
			}
		}
		Expect(updatedUp).ToNot(BeNil())
		Expect(updatedUp.Value).To(Equal(d41cUpstreamB), "the recreated pod must carry the new TOOL_UPSTREAM (I43)")

		// (c) Delete the tool from the policy and re-reconcile from the API
		// server → the pod + Service are deleted (cleanup) and the sandbox
		// can run with no tool proxy.
		ap.Spec.Tools = nil
		Expect(k8sClient.Update(ctx, ap)).To(Succeed())
		reconcileLoop("updel-loop", ns)
		podGone := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: toolProxyPod("updel-loop")}, podGone)).
			To(HaveOccurred(), "the tool proxy pod must be deleted when the tool leaves the effective policy (I43)")
		svcGone := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: toolProxyPod("updel-loop")}, svcGone)).
			To(HaveOccurred(), "the tool proxy Service must be deleted when the tool leaves the effective policy (I43)")

		// The sandbox gate no longer applies (no tools) → Running.
		reconcileLoop("updel-loop", ns)
		sb = getSandbox(ns, "updel-loop")
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeRunning),
			"with no tool proxies expected, the sandbox must run with no tool proxy (I43)")
	})
})

// D41c: the owned+Ready sandbox gate (D35a pattern) and the fail-closed
// union read. A read error on the effective policy must hold the sandbox
// Suspended (like the I42b needsEgressProxy contract); a fresh tool proxy
// pod that is not Ready must hold the sandbox Suspended.
var _ = Describe("D41c: toolProxyGatesSuspended fail-closed", func() {
	It("a tool proxy pod not Ready holds the sandbox gate (owned+Ready, fail-closed)", func() {
		r := newD41cReconciler()
		ctx := context.Background()
		ns := "d41c-fc-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })

		// A Loop with a tool; the tool proxy pod exists (Loop-owned) but is
		// NOT Ready → the gate holds.
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "fc-loop", Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "d41c fail-closed test",
				Workspace:  coxv1alpha1.Workspace{Repo: d41cTestRepo, Ref: loopRef},
				PolicyRefs: []string{d41cPolicyName},
				Agent:      coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
			},
		}
		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d41cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41cUpstreamA)}},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		Expect(k8sClient.Create(ctx, ap)).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "fc-loop-tool-" + d41cToolName, Namespace: ns},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "t", Image: verifyBusybox}}},
		})).To(Succeed())

		Expect(r.toolProxyGatesSuspended(ctx, loop)).To(BeTrue(),
			"an unready tool proxy pod must hold the sandbox gate (fail-closed)")

		// A Loop with NO tools → no gate.
		ap.Spec.Tools = nil
		Expect(k8sClient.Update(ctx, ap)).To(Succeed())
		Expect(r.toolProxyGatesSuspended(ctx, loop)).To(BeFalse(),
			"no tools in the effective policy → no tool-proxy gate")
	})

	It("needsToolProxy fails closed on a policy read error (like the I42b needsEgressProxy contract)", func() {
		// A fake-client unit (like the I42b gate test): a Get error on a
		// referenced AgentPolicy must make needsToolProxy return true, so the
		// sandbox gate requires the tool proxy (fail-closed) rather than
		// skipping the gate.
		r := newD41cReconciler()
		ctx := context.Background()
		const loopName = "fc2-loop"
		const policyName = "fc2-pol"
		const ns = "default"

		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: loopName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "d41c fail-closed unit",
				Workspace:  coxv1alpha1.Workspace{Repo: d41cTestRepo, Ref: loopRef},
				PolicyRefs: []string{policyName},
				Agent:      coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
			},
		}
		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41cUpstreamA)}},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		Expect(k8sClient.Create(ctx, ap)).To(Succeed())

		Expect(needsToolProxy(ctx, r, loop)).To(BeTrue(),
			"the tool is in the effective policy → the gate applies")

		// Delete the referenced policy → the union read fails (NotFound) →
		// the gate still applies (fail-closed).
		Expect(k8sClient.Delete(ctx, ap)).To(Succeed())
		Expect(needsToolProxy(ctx, r, loop)).To(BeTrue(),
			"a read error (missing referenced policy) must keep the gate (fail-closed, never skip)")
	})
})

// D41c: the tool union helpers — dedupTools collapses identical definitions;
// conflictingTool names the first conflicting tool (deterministic order).
var _ = Describe("D41c: tool union helpers", func() {
	It("dedupTools keeps the first definition of each name and preserves order", func() {
		tools := []coxv1alpha1.ToolSpec{
			d41cToolGH(d41cUpstreamA),
			{Name: "g2", Upstream: d41cUpstreamB, Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
			d41cToolGH(d41cUpstreamA), // identical → deduped
		}
		out := dedupTools(tools)
		Expect(out).To(HaveLen(2))
		Expect(out[0].Name).To(Equal(d41cToolName))
		Expect(out[1].Name).To(Equal("g2"))
	})

	It("conflictingTool names the first (alphabetical) conflicting tool", func() {
		// Two tools, only one conflicts → the conflicting one is named.
		tools := []coxv1alpha1.ToolSpec{
			{Name: "a", Upstream: d41cUpstreamA, Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
			{Name: "a", Upstream: d41cUpstreamB, Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
			{Name: "b", Upstream: d41cUpstreamA, Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
		}
		name, ok := conflictingTool(tools)
		Expect(ok).To(BeTrue())
		Expect(name).To(Equal("a"))

		// No conflict (single definitions) → not found.
		_, ok = conflictingTool([]coxv1alpha1.ToolSpec{d41cToolGH(d41cUpstreamA)})
		Expect(ok).To(BeFalse())

		// Identical definitions → not a conflict (the dedup case).
		_, ok = conflictingTool([]coxv1alpha1.ToolSpec{
			d41cToolGH(d41cUpstreamA), d41cToolGH(d41cUpstreamA),
		})
		Expect(ok).To(BeFalse())
	})

	It("a userinfo upstream is rejected by the controller-side check (D41b review)", func() {
		r := newD41cReconciler()
		tools := []coxv1alpha1.ToolSpec{
			{Name: "u", Upstream: "https://user:pass@api.github.com", Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
		}
		offending, ok := r.findInClusterToolUpstream(tools)
		Expect(ok).To(BeTrue(), "a userinfo-carrying upstream must be rejected (embedded credential)")
		Expect(offending).To(Equal("https://user:pass@api.github.com"))

		// And toolUpstreamHost returns "" for it (the credential is NOT parsed
		// around).
		Expect(toolUpstreamHost("https://user:pass@api.github.com")).To(Equal(""),
			"toolUpstreamHost must not parse a userinfo-carrying URL past the credential")
	})

	It("toolUpstreamHost parses hosts via url.Parse (no string slicing)", func() {
		Expect(toolUpstreamHost("https://api.github.com")).To(Equal("api.github.com"))
		Expect(toolUpstreamHost("https://api.github.com:8443")).To(Equal("api.github.com"))
		Expect(toolUpstreamHost("https://API.GITHUB.COM")).To(Equal("api.github.com"))
		Expect(toolUpstreamHost("https://api.github.com/repos")).To(Equal("api.github.com"))
		Expect(toolUpstreamHost("http://my-svc.default.svc:8080/api/v1")).To(Equal("my-svc.default.svc"))
		Expect(toolUpstreamHost("http://[::1]:8080")).To(Equal("::1"))
		Expect(toolUpstreamHost("")).To(Equal(""))
		Expect(toolUpstreamHost("not-a-url")).To(Equal(""))
	})

	It("formatting the derived tool proxy name stays within the DNS budget", func() {
		// A long loop name + a long tool name must still fit the 63-char
		// budget (derivedName truncates + hashes near-max names).
		longLoop := fmt.Sprintf("l%050d", 1)
		name := toolProxyPodName(longLoop, d41cToolName)
		Expect(len(name)).To(BeNumerically("<=", 63))
	})
})

var _ = Describe("D41 tool proxy image", func() {
	It("defaults to the Go dev stand-in when ToolProxyImage is unset", func() {
		// Same pattern as proxyImage / egressProxyImage: a dev stand-in when
		// unset; a real tool proxy image (rule engine, credential injection,
		// audit) is settable via the reconciler's ToolProxyImage field / the
		// manager's --tool-proxy-image flag (D41c).
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		Expect(r.toolProxyImage()).To(Equal("coxswain-tool-proxy:standin"))
	})

	It("honours the ToolProxyImage override", func() {
		// A DISTINCT value from the default (coxswain-tool-proxy:standin) so
		// the spec actually proves the override is honoured (not just that the
		// default is returned regardless of the field).
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ToolProxyImage: "example.invalid/tool-proxy:override"}
		Expect(r.toolProxyImage()).To(Equal("example.invalid/tool-proxy:override"))
	})
})
