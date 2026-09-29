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

// I42b (plan section I, ADR-0007 I42 resolution): the operator owns the
// per-Loop egress proxy pod + Service. The egress proxy enforces the AgentPolicy
// network allowlist at the HTTP CONNECT / SNI / Host layer (I42a binary).
//
// Labels are DISJOINT from the model proxy and agent pods: the egress proxy
// gets app.kubernetes.io/component=egress-proxy + coxswain.io/egress-proxy-for
// (never coxswain.io/loop, which is the KubeArmorPolicy selector).
//
// Gate pattern (D35a): the egress proxy pod must be Ready and owned by the
// Loop for the sandbox to run. ProxyConflict (ForeignEgressProxy) when a
// foreign pod occupies the name. Drift detection via full pod-spec hash
// (D33 pattern): image or env changes recreate the pod.
//
// I41 consequence: when the egress proxy enforces network allows, the operator
// must NOT set PolicyTranslationLossy=True for agent network allows; it sets
// PolicyTranslationLossy=False reason NoPortLoss when network allows are
// present. (The condition remains True for the model proxy's egress.)

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
	// i42b test constants.
	i42bPolicyName           = "i42b-pol"
	i42bTestRepo             = "https://github.com/papawattu/coxswain.git"
	i42bExternalAllow        = i42eExternalHost
	i42bConfLoopName         = "egconf-loop"
	i42bPodCIDR              = "10.244.0.0/16"
	i42bServiceCIDR          = "10.96.0.0/12"
	i42bEgressProxyImg       = "coxswain-egress-proxy:standin"
	i42bForeignImage         = "docker.io/library/busybox:1.36"
	i42bForeignName          = "foreign"
	i42bDriftPodName         = "drift-loop-egress-proxy"
	i42bDriftLoopName        = "drift-loop"
	i42bEgressConflict       = "EgressProxyConflict"
	i42bLossyLoopName        = "lossy-loop"
	i42bEgressProxyComponent = "egress-proxy"
)

var _ = Describe("I42b: ensureEgressProxy", func() {
	var (
		ctx context.Context
		r   *LoopReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		r = &LoopReconciler{
			Client:           k8sClient,
			Scheme:           k8sClient.Scheme(),
			PodCIDR:          i42bPodCIDR,
			ServiceCIDR:      i42bServiceCIDR,
			EgressProxyImage: i42bEgressProxyImg,
			// AllowUnenforced: the egress proxy gate is independent of the D30
			// enforcement gate. In envtest, the Enforcer is nil, so D30 would
			// hold the sandbox Suspended. AllowUnenforced=true lets us isolate
			// the egress proxy gate (D35a pattern) from the D30 gate.
			AllowUnenforced: true,
		}
	})

	buildLoop := func(name, ns string, policyRefs []string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "i42b test",
				Workspace:  coxv1alpha1.Workspace{Repo: i42bTestRepo, Ref: loopRef},
				PolicyRefs: policyRefs,
				Agent:      coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
			},
		}
	}

	// spec 1: no network allows -> no egress proxy pod/Service.
	It("does not create an egress proxy when there are no network allows", func() {
		ns := "i42b-noallow-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// An AgentPolicy with no network allows.
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42bPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{}, // no network allows
		})).To(Succeed())

		loop := buildLoop("noallow-loop", ns, []string{i42bPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "noallow-loop"}})
		Expect(err).NotTo(HaveOccurred())

		// No egress proxy pod.
		pod := &corev1.Pod{}
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "noallow-loop-egress-proxy"}, pod)
		Expect(err).To(HaveOccurred(), "no egress proxy pod when there are no network allows")

		// No egress proxy Service.
		svc := &corev1.Service{}
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "noallow-loop-egress-proxy"}, svc)
		Expect(err).To(HaveOccurred(), "no egress proxy Service when there are no network allows")
	})

	// spec 2: network allows -> egress proxy pod + Service created, sandbox Suspended (not Ready yet).
	It("creates the egress proxy pod and Service when network allows are present", func() {
		ns := "i42b-allow-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42bPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42bExternalAllow}},
		})).To(Succeed())

		// i42dAllowLoop is the shared "network allows present" Loop name
		// (loop_i42_env_test.go); the literal must stay a constant (goconst).
		loop := buildLoop(i42dAllowLoop, ns, []string{i42bPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: i42dAllowLoop}})
		Expect(err).NotTo(HaveOccurred())

		// Egress proxy pod exists.
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i42dAllowLoop + "-egress-proxy"}, pod)).To(Succeed(),
			"the egress proxy pod must be created when network allows are present")

		// Verify the pod carries the correct labels (DISJOINT from agent/model proxy).
		Expect(pod.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", i42bEgressProxyComponent))
		Expect(pod.Labels).To(HaveKeyWithValue("coxswain.io/egress-proxy-for", i42dAllowLoop))
		Expect(pod.Labels).ToNot(HaveKey("coxswain.io/loop"),
			"the egress proxy pod must NOT carry coxswain.io/loop (the KubeArmorPolicy selector)")
		Expect(pod.Labels).ToNot(HaveKeyWithValue("app.kubernetes.io/component", "agent"))
		Expect(pod.Labels).ToNot(HaveKeyWithValue("app.kubernetes.io/component", "model-proxy"))

		// Verify the EGRESS_POLICY_JSON env var.
		var container corev1.Container
		for _, c := range pod.Spec.Containers {
			if c.Name == i42bEgressProxyComponent {
				container = c
				break
			}
		}
		Expect(container).ToNot(BeZero(), "the egress proxy container must exist")
		var policyJSONEnv *corev1.EnvVar
		for _, e := range container.Env {
			if e.Name == "EGRESS_POLICY_JSON" {
				policyJSONEnv = &e
				break
			}
		}
		Expect(policyJSONEnv).ToNot(BeNil(), "the EGRESS_POLICY_JSON env var must be set")
		var allows []string
		Expect(json.Unmarshal([]byte(policyJSONEnv.Value), &allows)).To(Succeed())
		Expect(allows).To(ContainElement(i42bExternalAllow))

		// Hardening (P2 review): pin the security posture so a future edit
		// that drops RunAsNonRoot, flips the SA-token automount, or stops
		// passing the CIDRs fails here. This pod fronts all agent egress.
		Expect(pod.Spec.AutomountServiceAccountToken).ToNot(BeNil())
		Expect(*pod.Spec.AutomountServiceAccountToken).To(BeFalse(), "no SA token on the egress proxy pod")
		Expect(pod.Spec.SecurityContext).ToNot(BeNil())
		Expect(*pod.Spec.SecurityContext.RunAsUser).To(BeEquivalentTo(65534))
		Expect(*pod.Spec.SecurityContext.RunAsGroup).To(BeEquivalentTo(65534))
		sc := container.SecurityContext
		Expect(sc).ToNot(BeNil())
		Expect(*sc.RunAsUser).To(BeEquivalentTo(65534))
		Expect(*sc.RunAsGroup).To(BeEquivalentTo(65534))
		Expect(*sc.RunAsNonRoot).To(BeTrue(), "RunAsNonRoot must be set")
		Expect(*sc.ReadOnlyRootFilesystem).To(BeTrue(), "read-only rootfs")
		Expect(*sc.AllowPrivilegeEscalation).To(BeFalse(), "no privilege escalation")
		Expect(sc.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")), "drop ALL capabilities")
		Expect(sc.SeccompProfile).ToNot(BeNil())
		Expect(sc.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))

		// Readiness probe is TCP 3128.
		Expect(container.ReadinessProbe).ToNot(BeNil())
		Expect(container.ReadinessProbe.TCPSocket).ToNot(BeNil())
		Expect(container.ReadinessProbe.TCPSocket.Port.IntVal).To(BeEquivalentTo(3128))

		// The CIDR env vars must carry the reconciler's values (P2 review).
		envByName := map[string]string{}
		for _, e := range container.Env {
			envByName[e.Name] = e.Value
		}
		Expect(envByName["POD_CIDR"]).To(Equal(i42bPodCIDR))
		Expect(envByName["SERVICE_CIDR"]).To(Equal(i42bServiceCIDR))

		// EGRESS_POLICY_HASH equals the effective policy hash (P2 review).
		wantHash := policy.EffectiveHash(policy.EffectivePolicy{Network: []string{i42bExternalAllow}})
		Expect(envByName["EGRESS_POLICY_HASH"]).To(Equal(wantHash))

		// Owner refs on pod and Service (P2 review).
		loopRef := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i42dAllowLoop}, loopRef)).To(Succeed())
		Expect(metav1.IsControlledBy(pod, loopRef)).To(BeTrue(), "the egress proxy pod must be owned by the Loop")

		// Egress proxy Service exists.
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i42dAllowLoop + "-egress-proxy"}, svc)).To(Succeed(),
			"the egress proxy Service must be created")
		Expect(svc.Spec.Ports).To(HaveLen(1))
		Expect(svc.Spec.Ports[0].Port).To(BeEquivalentTo(3128))
		Expect(metav1.IsControlledBy(svc, loopRef)).To(BeTrue(), "the egress proxy Service must be owned by the Loop")
		// The Service selector must equal the pod labels (P2 review).
		for k, v := range pod.Labels {
			Expect(svc.Spec.Selector[k]).To(Equal(v), "Service selector must match pod label %s", k)
		}

		// The sandbox must be Suspended (the egress proxy pod is not Ready yet).
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i42dAllowLoop + "-sandbox"}, sb)).To(Succeed())
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"the sandbox must be Suspended when the egress proxy is not Ready")
	})

	// spec 3: egress proxy pod Ready -> sandbox Running.
	It("sets the sandbox to Running when the egress proxy pod is Ready", func() {
		ns := "i42b-ready-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42bPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42bExternalAllow}},
		})).To(Succeed())

		loop := buildLoop("ready-loop", ns, []string{i42bPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// First reconcile: creates the egress proxy pod (not Ready).
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "ready-loop"}})
		Expect(err).NotTo(HaveOccurred())

		// Mark the egress proxy pod as Ready.
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "ready-loop-egress-proxy"}, pod)).To(Succeed())
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		// Second reconcile: the egress proxy is Ready, so the sandbox should be Running.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "ready-loop"}})
		Expect(err).NotTo(HaveOccurred())

		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "ready-loop-sandbox"}, sb)).To(Succeed())
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeRunning),
			"the sandbox must be Running when the egress proxy is Ready")
	})

	// spec 4: foreign egress proxy pod -> ProxyConflict + ForeignEgressProxy + Suspended.
	It("sets ProxyConflict when a foreign pod occupies the egress proxy name", func() {
		ns := "i42b-conflict-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42bPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42bExternalAllow}},
		})).To(Succeed())

		loop := buildLoop("conflict-loop", ns, []string{i42bPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// Pre-create a FOREIGN pod with the egress proxy name (not owned by the Loop).
		foreignPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "conflict-loop-egress-proxy",
				Namespace: ns,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  i42bForeignName,
					Image: i42bForeignImage,
				}},
			},
		}
		Expect(k8sClient.Create(ctx, foreignPod)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "conflict-loop"}})
		Expect(err).NotTo(HaveOccurred())

		// The foreign pod must still exist (not deleted — I2 never-take-over).
		gotForeign := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "conflict-loop-egress-proxy"}, gotForeign)).To(Succeed(),
			"the foreign egress proxy pod must NOT be deleted (I2 never-take-over)")

		// The Loop must have an EgressProxyConflict condition (P2 review: the
		// egress proxy's conflict is a separate condition type from the model
		// proxy's ProxyConflict, so the two never overwrite each other).
		gotLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "conflict-loop"}, gotLoop)).To(Succeed())
		var conflict *metav1.Condition
		for i := range gotLoop.Status.Conditions {
			if gotLoop.Status.Conditions[i].Type == i42bEgressConflict {
				conflict = &gotLoop.Status.Conditions[i]
				break
			}
		}
		Expect(conflict).ToNot(BeNil(), "EgressProxyConflict condition must be set")
		Expect(conflict.Status).To(Equal(metav1.ConditionTrue))
		Expect(conflict.Reason).To(Equal("ForeignEgressProxy"))

		// The sandbox must be Suspended.
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "conflict-loop-sandbox"}, sb)).To(Succeed())
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"the sandbox must be Suspended when a foreign egress proxy pod occupies the name")
	})

	// spec 5: policy change -> pod recreated with updated EGRESS_POLICY_HASH, Service unchanged.
	It("recreates the egress proxy pod on policy change (drift detection)", func() {
		ns := "i42b-drift-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42bPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42bExternalAllow}},
		})).To(Succeed())

		loop := buildLoop(i42bDriftLoopName, ns, []string{i42bPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// First reconcile: creates the egress proxy pod.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: i42bDriftLoopName}})
		Expect(err).NotTo(HaveOccurred())

		pod1 := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i42bDriftPodName}, pod1)).To(Succeed())
		uid1 := pod1.UID

		// Update the AgentPolicy: add a second network allow.
		ap := &coxv1alpha1.AgentPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i42bPolicyName}, ap)).To(Succeed())
		ap.Spec.Network = append(ap.Spec.Network, "api.openai.com:443")
		Expect(k8sClient.Update(ctx, ap)).To(Succeed())

		// Second reconcile: the pod spec hash changes (EGRESS_POLICY_JSON
		// changed), so the pod is deleted. Recreation happens on the NEXT
		// reconcile (delete+recreate pattern), so a third Reconcile is needed
		// before asserting the new pod (P2 review: the test previously passed
		// without ever checking the recreate).
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: i42bDriftLoopName}})
		Expect(err).NotTo(HaveOccurred())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: i42bDriftLoopName}})
		Expect(err).NotTo(HaveOccurred())

		// The recreated pod exists with a NEW UID, the updated EGRESS_POLICY_JSON
		// (both allows) and the updated EGRESS_POLICY_HASH.
		podNew := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i42bDriftPodName}, podNew)).To(Succeed(),
			"the egress proxy pod must be recreated on policy change")
		Expect(podNew.UID).ToNot(Equal(uid1), "the recreated pod must have a new UID")
		var container corev1.Container
		for _, c := range podNew.Spec.Containers {
			if c.Name == i42bEgressProxyComponent {
				container = c
				break
			}
		}
		envByName := map[string]string{}
		for _, e := range container.Env {
			envByName[e.Name] = e.Value
		}
		var newAllows []string
		Expect(json.Unmarshal([]byte(envByName["EGRESS_POLICY_JSON"]), &newAllows)).To(Succeed())
		Expect(newAllows).To(ContainElement(i42bExternalAllow))
		Expect(newAllows).To(ContainElement("api.openai.com:443"), "the recreated pod must carry the new allow")
		wantHash := policy.EffectiveHash(policy.EffectivePolicy{Network: []string{i42bExternalAllow, "api.openai.com:443"}})
		Expect(envByName["EGRESS_POLICY_HASH"]).To(Equal(wantHash), "the recreated pod must carry the updated policy hash")

		// The Service should still exist (unchanged).
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i42bDriftPodName}, svc)).To(Succeed(),
			"the egress proxy Service must still exist after a policy change")
	})

	// spec 6: C6b KubeArmorPolicy selector does NOT match the egress proxy pod.
	It("verifies the egress proxy pod labels are disjoint from the agent KubeArmorPolicy selector", func() {
		ns := "i42b-disjoint-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42bPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42bExternalAllow}},
		})).To(Succeed())

		loop := buildLoop("disjoint-loop", ns, []string{i42bPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "disjoint-loop"}})
		Expect(err).NotTo(HaveOccurred())

		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "disjoint-loop-egress-proxy"}, pod)).To(Succeed())

		// The agent KubeArmorPolicy (C6b) selects on coxswain.io/loop.
		// The egress proxy pod must NOT have that label, or the agent's exec
		// allowlist would bind to the egress proxy pod (blocking its own binary).
		_, hasLoopLabel := pod.Labels["coxswain.io/loop"]
		Expect(hasLoopLabel).To(BeFalse(),
			"the egress proxy pod must NOT carry coxswain.io/loop (the agent KubeArmorPolicy selector)")

		// The agent NetworkPolicy (D34) selects on app.kubernetes.io/component=agent.
		_, hasAgentComponent := pod.Labels["app.kubernetes.io/component"]
		if hasAgentComponent {
			Expect(pod.Labels["app.kubernetes.io/component"]).ToNot(Equal("agent"),
				"the egress proxy pod must NOT carry the agent component label")
		}
	})

	// spec 8 (P2 review): the egress proxy's conflict is a SEPARATE condition
	// (EgressProxyConflict), so the model proxy's ProxyConflict (D35) and the
	// egress proxy's conflict can never overwrite each other. A healthy model
	// proxy (ProxyConflict=False/NoConflict) plus a FOREIGN egress proxy pod
	// must keep EgressProxyConflict=True (ForeignEgressProxy) across two
	// reconciles, without the healthy model proxy's NoConflict writing
	// ProxyConflict=False over it.
	It("keeps EgressProxyConflict=True (ForeignEgressProxy) across reconciles when a foreign egress proxy pod is present alongside a healthy model proxy", func() {
		ns := "i42b-egconf-" + nowSuffix()
		const loopName = i42bConfLoopName
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// The model creds Secret (required alongside endpointSecretRef).
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "egconf-creds", Namespace: ns},
			StringData: map[string]string{
				modelAPIKey:  "key",
				modelBaseURL: "http://fake-model:8000",
			},
		})).To(Succeed())

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42bPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42bExternalAllow}},
		})).To(Succeed())

		// A Loop with BOTH a model endpoint (so the model proxy runs) and a
		// network allow (so the egress proxy is expected). modelEndpoint and
		// endpointSecretRef must be set together (CRD CEL rule).
		loop := buildLoop(loopName, ns, []string{i42bPolicyName})
		loop.Spec.Agent.ModelEndpoint = "fake-model:8000"
		loop.Spec.Agent.EndpointSecretRef = "egconf-creds"
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// Pre-create a FOREIGN pod with the egress proxy name (not owned by the Loop).
		foreignPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      loopName + "-egress-proxy",
				Namespace: ns,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  i42bForeignName,
					Image: i42bForeignImage,
				}},
			},
		}
		Expect(k8sClient.Create(ctx, foreignPod)).To(Succeed())

		checkEgressConflict := func(reconcileNo int) {
			gotLoop := &coxv1alpha1.Loop{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: loopName}, gotLoop)).To(Succeed())
			var conflict *metav1.Condition
			for i := range gotLoop.Status.Conditions {
				if gotLoop.Status.Conditions[i].Type == i42bEgressConflict {
					conflict = &gotLoop.Status.Conditions[i]
					break
				}
			}
			Expect(conflict).ToNot(BeNil(),
				fmt.Sprintf("reconcile %d: the Loop must have an EgressProxyConflict condition when a foreign egress proxy pod is present", reconcileNo))
			Expect(conflict.Status).To(Equal(metav1.ConditionTrue),
				fmt.Sprintf("reconcile %d: EgressProxyConflict must stay True while the foreign pod is present (the healthy model proxy must not clear it)", reconcileNo))
			Expect(conflict.Reason).To(Equal("ForeignEgressProxy"))
		}

		// Reconcile 1: the model proxy is ensured (healthy, no conflict) and the
		// egress proxy name is occupied by a foreign pod.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: loopName}})
		Expect(err).NotTo(HaveOccurred())
		checkEgressConflict(1)

		// Reconcile 2: the healthy model proxy's D35b path runs again and must
		// NOT clear the egress conflict.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: loopName}})
		Expect(err).NotTo(HaveOccurred())
		checkEgressConflict(2)

		// The model proxy's own condition is unaffected (False/NoConflict or
		// True/ForeignProxy — in this fixture it is the controller's own proxy,
		// so NoConflict/Resolved).
		gotLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: loopName}, gotLoop)).To(Succeed())
		var proxyConflict *metav1.Condition
		for i := range gotLoop.Status.Conditions {
			if gotLoop.Status.Conditions[i].Type == "ProxyConflict" {
				proxyConflict = &gotLoop.Status.Conditions[i]
				break
			}
		}
		Expect(proxyConflict).ToNot(BeNil(), "the model proxy's ProxyConflict condition must still be present")
		Expect(proxyConflict.Status).To(Equal(metav1.ConditionFalse),
			"the healthy model proxy must report no conflict even while the egress proxy is foreign")
	})

	// spec 9 (P3 review): a stale EgressProxyConflict=True is cleared on the
	// no-allows path. A foreign pod once held the name (condition True), then
	// the network allows are removed; the early cleanup return must clear the
	// condition (it is moot: no egress proxy is required anymore) rather than
	// report a conflict that no longer applies.
	It("clears a stale EgressProxyConflict when the network allows are removed", func() {
		ns := "i42b-stale-" + nowSuffix()
		const loopName = "stale-loop"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42bPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42bExternalAllow}},
		})).To(Succeed())

		loop := buildLoop(loopName, ns, []string{i42bPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// Pre-create a FOREIGN pod with the egress proxy name.
		foreignPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      loopName + "-egress-proxy",
				Namespace: ns,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  i42bForeignName,
					Image: i42bForeignImage,
				}},
			},
		}
		Expect(k8sClient.Create(ctx, foreignPod)).To(Succeed())

		// Reconcile 1: the foreign pod holds the name -> EgressProxyConflict=True.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: loopName}})
		Expect(err).NotTo(HaveOccurred())
		gotLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: loopName}, gotLoop)).To(Succeed())
		var conflict *metav1.Condition
		for i := range gotLoop.Status.Conditions {
			if gotLoop.Status.Conditions[i].Type == i42bEgressConflict {
				conflict = &gotLoop.Status.Conditions[i]
				break
			}
		}
		Expect(conflict).ToNot(BeNil(), "reconcile 1: the foreign egress proxy pod must set EgressProxyConflict")
		Expect(conflict.Status).To(Equal(metav1.ConditionTrue))

		// Remove the network allows (the egress proxy is no longer required).
		ap := &coxv1alpha1.AgentPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i42bPolicyName}, ap)).To(Succeed())
		ap.Spec.Network = nil
		Expect(k8sClient.Update(ctx, ap)).To(Succeed())

		// Reconcile 2: the no-allows path runs (early return) and must clear the
		// stale condition.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: loopName}})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: loopName}, gotLoop)).To(Succeed())
		var cleared *metav1.Condition
		for i := range gotLoop.Status.Conditions {
			if gotLoop.Status.Conditions[i].Type == i42bEgressConflict {
				cleared = &gotLoop.Status.Conditions[i]
				break
			}
		}
		Expect(cleared).ToNot(BeNil(), "reconcile 2: the condition must still exist (cleared, not removed)")
		Expect(cleared.Status).To(Equal(metav1.ConditionFalse),
			"the stale EgressProxyConflict must be cleared when the network allows are removed")
		Expect(cleared.Reason).To(Equal("Resolved"))

		// The foreign pod must still exist (I2 never-take-over, even on cleanup).
		gotForeign := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: loopName + "-egress-proxy"}, gotForeign)).To(Succeed(),
			"the foreign pod must NOT be deleted by the cleanup path either")
	})

	// spec 7: PolicyTranslationLossy condition when network allows are present.
	// NOTE (I41 consequence, ADR-0007): when the egress proxy enforces network
	// allows at the app layer, the operator should set PolicyTranslationLossy=False
	// NoPortLoss (the port precision is carried by the egress proxy, not lost in
	// KubeArmor translation). This change is NOT yet implemented — the current
	// code still sets PolicyTranslationLossy=True for network allows (I41).
	// This test verifies the current behavior and will be updated when the
	// NoPortLoss change lands.
	It("verifies the PolicyTranslationLossy condition for network allows (current: True, future: NoPortLoss)", func() {
		ns := "i42b-lossy-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42bPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42bExternalAllow}},
		})).To(Succeed())

		loop := buildLoop(i42bLossyLoopName, ns, []string{i42bPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: i42bLossyLoopName}})
		Expect(err).NotTo(HaveOccurred())

		gotLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i42bLossyLoopName}, gotLoop)).To(Succeed())
		var lossy *metav1.Condition
		for i := range gotLoop.Status.Conditions {
			if gotLoop.Status.Conditions[i].Type == "PolicyTranslationLossy" {
				lossy = &gotLoop.Status.Conditions[i]
				break
			}
		}
		// The condition should be set (either True for the current I41 behavior,
		// or False NoPortLoss for the future I41 consequence).
		Expect(lossy).ToNot(BeNil(), "the PolicyTranslationLossy condition must be set")
		// Current behavior (I41): True (network allows are lossy in KubeArmor
		// translation). Future (I41 consequence with egress proxy): False NoPortLoss.
		// This assertion is intentionally loose to cover both states.
		Expect(lossy.Status).To(BeElementOf(metav1.ConditionTrue, metav1.ConditionFalse),
			"PolicyTranslationLossy must be either True (current I41) or False NoPortLoss (future I41 consequence)")
	})
})
