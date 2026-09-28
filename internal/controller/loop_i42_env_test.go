// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License
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

// I42d (plan section I42d, ADR-0007): when the egress proxy is expected (the
// effective policy has network allows, or it cannot be read — the same
// fail-closed needsEgressProxy gate), the operator sets standard proxy env
// vars on the agent container so any HTTP client routes external egress
// through the egress proxy while model calls go directly to the model proxy:
//
//	HTTPS_PROXY=http://<loop>-egress-proxy.<ns>.svc:3128
//	HTTP_PROXY =http://<loop>-egress-proxy.<ns>.svc:3128
//	NO_PROXY   =<loop>-proxy.<ns>.svc,<loop>-proxy.<ns>.svc.cluster.local,localhost,127.0.0.1
//
// The vars are standard names (not COX_-prefixed) so clients pick them up;
// COX_MODEL_BASE_URL's host matches a NO_PROXY entry so model calls bypass
// the egress proxy. With no network allows the three vars are NOT set.
// A user spec.agent.env var with the same name is dropped: the operator
// value wins (the plan: operator values should win; an agent that could
// re-route its own egress away from the egress proxy would defeat the
// network allowlist).

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// i42d test constants.
	i42dPolicyName = "i42d-pol"
	i42dAllowHost  = i42eExternalHost
	// i42dUserProxy / i42dUserNOProxy are user spec.agent.env values that
	// collide with the operator-owned proxy names. The operator value must
	// win (they are dropped).
	i42dUserProxy   = "http://attacker.example:8080"
	i42dUserNOProxy = "attacker.example"
)

var _ = Describe("I42d: *_PROXY / NO_PROXY env on the agent container", func() {
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
			// AllowUnenforced: isolate the agent env assertions from the D30
			// enforcement gate (the egress proxy gate, D35a pattern, is
			// independent of D30). The env is set on the sandbox spec either
			// way.
			AllowUnenforced: true,
		}
	})

	buildLoop := func(name, ns string, env []coxv1alpha1.AgentEnvVar) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "i42d test",
				Workspace:  coxv1alpha1.Workspace{Repo: i42bTestRepo, Ref: loopRef},
				PolicyRefs: []string{i42dPolicyName},
				Agent:      coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel, Env: env},
			},
		}
	}

	// reconcileAgentEnv creates the namespace + AgentPolicy + Loop,
	// reconciles, and returns the agent container's env from the sandbox
	// spec as a name->value map (env may not contain duplicate names).
	reconcileAgentEnv := func(loopName, ns string, policyNetwork []string, env []coxv1alpha1.AgentEnvVar) map[string]string {
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42dPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: policyNetwork},
		})).To(Succeed())

		loop := buildLoop(loopName, ns, env)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: loopName}})
		Expect(err).NotTo(HaveOccurred())

		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: loopName + "-sandbox"}, sb)).To(Succeed(),
			"the sandbox must exist after reconcile")

		agentEnv := map[string]string{}
		for _, c := range sb.Spec.PodTemplate.Spec.Containers {
			if c.Name != "agent" {
				continue
			}
			for _, e := range c.Env {
				_, dup := agentEnv[e.Name]
				Expect(!dup).To(BeTrue(), "the agent container env must not contain duplicate name %q", e.Name)
				agentEnv[e.Name] = e.Value
			}
		}
		Expect(agentEnv).ToNot(BeEmpty(), "the agent container must exist with env")
		return agentEnv
	}

	wantEgressProxyURL := func(loopName, ns string) string {
		return "http://" + egressProxyServiceName(loopName) + "." + ns + ".svc:3128"
	}

	// spec 1: network allows present -> *_PROXY set with the expected values.
	It("sets HTTPS_PROXY, HTTP_PROXY and NO_PROXY when the effective policy has network allows", func() {
		ns := "i42d-allow-" + nowSuffix()
		env := reconcileAgentEnv("allow-loop", ns, []string{i42dAllowHost}, nil)

		wantURL := wantEgressProxyURL("allow-loop", ns)
		Expect(env["HTTPS_PROXY"]).To(Equal(wantURL),
			"HTTPS_PROXY must point at the egress proxy Service on port 3128")
		Expect(env["HTTP_PROXY"]).To(Equal(wantURL),
			"HTTP_PROXY must point at the egress proxy Service on port 3128")

		// NO_PROXY: the model proxy Service (short + FQDN form) + localhost.
		noProxy := env["NO_PROXY"]
		Expect(noProxy).To(ContainSubstring(proxyServiceName("allow-loop")+"."+ns+".svc"),
			"NO_PROXY must include the model proxy Service name")
		Expect(noProxy).To(ContainSubstring(proxyServiceName("allow-loop")+"."+ns+".svc.cluster.local"),
			"NO_PROXY must include the model proxy Service FQDN")
		Expect(noProxy).To(ContainSubstring("localhost"))
		Expect(noProxy).To(ContainSubstring("127.0.0.1"))
	})

	// spec 2: no network allows -> *_PROXY NOT set.
	It("does not set the proxy env vars when there are no network allows", func() {
		ns := "i42d-noallow-" + nowSuffix()
		env := reconcileAgentEnv("noallow-loop", ns, nil, nil)

		Expect(env).ToNot(HaveKey("HTTPS_PROXY"),
			"HTTPS_PROXY must not be set without network allows (the agent has no external egress)")
		Expect(env).ToNot(HaveKey("HTTP_PROXY"))
		Expect(env).ToNot(HaveKey("NO_PROXY"))
	})

	// spec 3: COX_MODEL_BASE_URL's host matches a NO_PROXY entry, so model
	// calls bypass the egress proxy.
	It("excludes the model proxy from the egress proxy via NO_PROXY", func() {
		ns := "i42d-noproxy-" + nowSuffix()
		env := reconcileAgentEnv("noproxy-loop", ns, []string{i42dAllowHost}, nil)

		baseURL := env[coxModelBaseURL]
		Expect(baseURL).ToNot(BeEmpty(), "COX_MODEL_BASE_URL must be set (a model endpoint is configured)")

		// NO_PROXY is comma-separated; the host part of COX_MODEL_BASE_URL
		// must be one of the entries (proxy clients match NO_PROXY by host),
		// so model calls bypass the egress proxy.
		entries := strings.Split(env["NO_PROXY"], ",")
		hostPart := strings.TrimPrefix(baseURL, "http://")
		if i := strings.IndexByte(hostPart, ':'); i >= 0 {
			hostPart = hostPart[:i]
		}
		Expect(entries).To(ContainElement(hostPart),
			"a NO_PROXY entry must be the model proxy host so model calls bypass the egress proxy")
	})

	// spec 4: standard names, not COX_-prefixed.
	It("uses the standard proxy env var names (not COX_-prefixed)", func() {
		ns := "i42d-names-" + nowSuffix()
		env := reconcileAgentEnv("names-loop", ns, []string{i42dAllowHost}, nil)

		Expect(env).To(HaveKey("HTTPS_PROXY"))
		Expect(env).To(HaveKey("HTTP_PROXY"))
		Expect(env).To(HaveKey("NO_PROXY"))
		for _, name := range []string{"COX_HTTPS_PROXY", "COX_HTTP_PROXY", "COX_NO_PROXY"} {
			Expect(env).ToNot(HaveKey(name), "the proxy env names must be the standard (unprefixed) names, not %s", name)
		}
	})

	// spec 5 (decision, handoff): a user spec.agent.env var that collides
	// with the operator-owned proxy names is dropped — the operator value
	// wins. The agent must not be able to re-route its egress away from the
	// egress proxy (the network allowlist is operator policy).
	It("drops user spec.agent.env vars that collide with the operator proxy names", func() {
		ns := "i42d-userconflict-" + nowSuffix()
		env := reconcileAgentEnv("userconflict-loop", ns, []string{i42dAllowHost}, []coxv1alpha1.AgentEnvVar{
			{Name: "HTTP_PROXY", Value: i42dUserProxy},
			{Name: "NO_PROXY", Value: i42dUserNOProxy},
			{Name: "KEEP_ME", Value: "survives"},
		})

		wantURL := wantEgressProxyURL("userconflict-loop", ns)
		Expect(env["HTTP_PROXY"]).To(Equal(wantURL),
			"the operator HTTP_PROXY must win over a user spec.agent.env value")
		Expect(env).ToNot(HaveKeyWithValue("NO_PROXY", i42dUserNOProxy),
			"the operator NO_PROXY must win over a user spec.agent.env value")
		Expect(env).To(HaveKeyWithValue("KEEP_ME", "survives"),
			"non-colliding user env vars must be preserved")
	})
})
