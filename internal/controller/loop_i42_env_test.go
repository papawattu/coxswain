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
//	HTTPS_PROXY=http://<loop>-egress-proxy.<ns>.svc.cluster.local:3128
//	HTTP_PROXY =http://<loop>-egress-proxy.<ns>.svc.cluster.local:3128
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
	"github.com/papawattu/coxswain/internal/engine"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// i42d test constants.
	i42dPolicyName = "i42d-pol"
	i42dAllowHost  = i42eExternalHost
	// i42dAllowLoop is the Loop name for the "network allows present" specs.
	// Shared with loop_i42b_egress_proxy_test.go so the repeated literal is a
	// constant (goconst).
	i42dAllowLoop = "allow-loop"
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
		// The real KubeArmor enforcer: spec 7 asserts the agent
		// KubeArmorPolicy's DNS allowlist, which the fakeEnforcer-based
		// specs in this suite never exercise.
		r = &LoopReconciler{
			Client:           k8sClient,
			Scheme:           k8sClient.Scheme(),
			Enforcer:         &engine.KubeArmorEnforcer{Client: k8sClient, ProxyFQDN: ProxyServiceFQDN, EgressProxyFQDN: EgressProxyServiceFQDN},
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

	// makeSecret creates a model-creds Secret so the Loop has an
	// EndpointSecretRef (like a real Loop): with a model endpoint the agent
	// gets COX_MODEL_BASE_URL (the model proxy .svc URL), which is what spec 3
	// asserts NO_PROXY excludes.
	makeSecret := func(name, ns string) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			StringData: map[string]string{
				modelAPIKey:  d34TestKey,
				modelBaseURL: "http://model-endpoint:8000",
			},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	}

	buildLoop := func(name, ns string, env []coxv1alpha1.AgentEnvVar) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "i42d test",
				Workspace:  coxv1alpha1.Workspace{Repo: i42bTestRepo, Ref: loopRef},
				PolicyRefs: []string{i42dPolicyName},
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					Env:               env,
					EndpointSecretRef: name + "-model",
					ModelEndpoint:     "model-endpoint:8000",
				},
			},
		}
	}

	// setPolicyNetwork (re)writes the test AgentPolicy's network allows in ns.
	// A new allows value makes needsEgressProxy flip, so a re-reconcile must
	// update the existing sandbox's agent env.
	setPolicyNetwork := func(ns string, network []string) {
		ap := &coxv1alpha1.AgentPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i42dPolicyName}, ap)).To(Succeed())
		ap.Spec.Network = network
		Expect(k8sClient.Update(ctx, ap)).To(Succeed())
	}

	// reconcileLoop reconciles the Loop once in ns.
	reconcileLoop := func(loopName, ns string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: loopName}})
		Expect(err).NotTo(HaveOccurred())
	}

	// agentEnvOf returns the agent container's env from the sandbox spec as a
	// name->value map (env may not contain duplicate names).
	agentEnvOf := func(loopName, ns string) map[string]string {
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: loopName + "-sandbox"}, sb)).To(Succeed(),
			"the sandbox must exist after reconcile")

		agentEnv := map[string]string{}
		for _, c := range sb.Spec.PodTemplate.Spec.Containers {
			if c.Name != agentContainerName {
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
		return "http://" + egressProxyServiceName(loopName) + "." + ns + ".svc.cluster.local:3128"
	}

	// setup creates the namespace + model Secret + AgentPolicy + Loop and
	// reconciles once; it returns the namespace (cleaned up on test exit).
	setup := func(loopName string, policyNetwork []string, env []coxv1alpha1.AgentEnvVar) string {
		ns := "i42d-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })

		makeSecret(loopName+"-model", ns)

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42dPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: policyNetwork},
		})).To(Succeed())

		loop := buildLoop(loopName, ns, env)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		reconcileLoop(loopName, ns)
		return ns
	}

	// spec 1: network allows present -> *_PROXY set with the expected values.
	It("sets HTTPS_PROXY, HTTP_PROXY and NO_PROXY when the effective policy has network allows", func() {
		loopName := i42dAllowLoop
		ns := setup(loopName, []string{i42dAllowHost}, nil)
		env := agentEnvOf(loopName, ns)

		wantURL := wantEgressProxyURL(i42dAllowLoop, ns)
		// Both cases of each proxy var are set: clients are inconsistent about
		// case (curl reads only lowercase http_proxy; Go reads either; wget/pip
		// /npm/git/apt mostly lowercase), so the operator emits both.
		for _, name := range operatorProxyNames("HTTPS_PROXY", "HTTP_PROXY") {
			Expect(env[name]).To(Equal(wantURL),
				"%s must point at the egress proxy Service on port 3128", name)
		}

		// NO_PROXY (both cases): the model proxy Service (short + FQDN form)
		// + localhost.
		for _, name := range operatorProxyNames("NO_PROXY") {
			noProxy := env[name]
			Expect(noProxy).To(ContainSubstring(proxyServiceName(i42dAllowLoop)+"."+ns+".svc"),
				"%s must include the model proxy Service name", name)
			Expect(noProxy).To(ContainSubstring(proxyServiceName(i42dAllowLoop)+"."+ns+".svc.cluster.local"),
				"%s must include the model proxy Service FQDN", name)
			Expect(noProxy).To(ContainSubstring("localhost"))
			Expect(noProxy).To(ContainSubstring("127.0.0.1"))
		}

		// ALL_PROXY is NOT set (reserved): it would catch model traffic, which
		// must bypass the egress proxy via NO_PROXY.
		Expect(env).ToNot(HaveKey("ALL_PROXY"))
		Expect(env).ToNot(HaveKey("all_proxy"))

		// Regression guard: the proxy vars must be APPENDED to the agent env,
		// not replace it — the pre-existing vars (COX_MODEL_BASE_URL, HOME)
		// must survive alongside them.
		Expect(env[coxModelBaseURL]).To(Equal(r.proxyServiceURL(i42dAllowLoop, ns)),
			"COX_MODEL_BASE_URL must survive alongside the proxy env vars")
		Expect(env).To(HaveKeyWithValue("HOME", "/scratch"))
	})

	// spec 2: no network allows -> *_PROXY NOT set.
	It("does not set the proxy env vars when there are no network allows", func() {
		loopName := "noallow-loop"
		ns := setup(loopName, nil, nil)
		env := agentEnvOf(loopName, ns)

		Expect(env).ToNot(HaveKey("HTTPS_PROXY"),
			"HTTPS_PROXY must not be set without network allows (the agent has no external egress)")
		Expect(env).ToNot(HaveKey("HTTP_PROXY"))
		Expect(env).ToNot(HaveKey("NO_PROXY"))
	})

	// spec 3: COX_MODEL_BASE_URL's host matches a NO_PROXY entry, so model
	// calls bypass the egress proxy.
	It("excludes the model proxy from the egress proxy via NO_PROXY", func() {
		loopName := "noproxy-loop"
		ns := setup(loopName, []string{i42dAllowHost}, nil)
		env := agentEnvOf(loopName, ns)

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
		loopName := "names-loop"
		ns := setup(loopName, []string{i42dAllowHost}, nil)
		env := agentEnvOf(loopName, ns)

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
		loopName := "userconflict-loop"
		ns := setup(loopName, []string{i42dAllowHost}, []coxv1alpha1.AgentEnvVar{
			{Name: envHTTPProxy, Value: i42dUserProxy},
			{Name: envHttpProxy, Value: i42dUserProxy},
			{Name: envNoProxy, Value: i42dUserNOProxy},
			{Name: envNoProxyLower, Value: i42dUserNOProxy},
			// all_proxy is reserved (not emitted): a user setting it must be
			// dropped too, or it would route traffic the egress proxy is not
			// meant to handle.
			{Name: "all_proxy", Value: i42dUserProxy},
			{Name: "KEEP_ME", Value: "survives"},
		})
		env := agentEnvOf(loopName, ns)

		wantURL := wantEgressProxyURL("userconflict-loop", ns)
		for _, name := range operatorProxyNames("HTTP_PROXY") {
			Expect(env[name]).To(Equal(wantURL),
				"the operator %s must win over a user spec.agent.env value", name)
		}
		for _, name := range operatorProxyNames("NO_PROXY") {
			Expect(env).ToNot(HaveKeyWithValue(name, i42dUserNOProxy),
				"the operator %s must win over a user spec.agent.env value", name)
		}
		Expect(env).ToNot(HaveKey("all_proxy"),
			"a user all_proxy must be dropped (the operator reserves it)")
		Expect(env).To(HaveKeyWithValue("KEEP_ME", "survives"),
			"non-colliding user env vars must be preserved")
	})

	// spec 7 (I42d, kind acceptance finding): the agent's KubeArmorPolicy
	// matchDNSQueries must contain <loop>-egress-proxy.<ns>.svc IFF the effective
	// policy has network allows. Without it, KubeArmor's DNS allowlist blocks the
	// agent from resolving the egress proxy Service name and every proxied
	// request fails (allowed hosts time out in DNS); with no allows the egress
	// proxy does not exist and the name must not appear in the allowlist.
	It("includes the egress proxy FQDN in the agent KubeArmorPolicy DNS allowlist iff network allows are present", func() {
		getDNSDomains := func(ns string) []string {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(engine.KubeArmorGVK)
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "coxswain-" + i42dAllowLoop}, obj)).To(Succeed(),
				"the agent KubeArmorPolicy must exist")
			network, _, _ := unstructured.NestedMap(obj.Object, "spec", "network")
			items, _ := network["matchDNSQueries"].([]any)
			domains := make([]string, 0, len(items))
			for _, it := range items {
				domains = append(domains, it.(map[string]any)["domain"].(string))
			}
			return domains
		}

		// With allows: the egress proxy FQDN is in the DNS allowlist (and the
		// model proxy FQDN is still there too, D33).
		withAllowsNS := "i42d-kapt-allow-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: withAllowsNS}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: withAllowsNS}})
		})
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42dPolicyName, Namespace: withAllowsNS},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42dAllowHost}},
		})).To(Succeed())
		loop := buildLoop(i42dAllowLoop, withAllowsNS, nil)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop(i42dAllowLoop, withAllowsNS)

		egFQDN := i42dAllowLoop + "-egress-proxy." + withAllowsNS + ".svc"
		withAllowsDomains := getDNSDomains(withAllowsNS)
		Expect(withAllowsDomains).To(ContainElement(egFQDN),
			"with network allows the agent DNS allowlist must include the egress proxy FQDN so the agent can resolve it")
		Expect(withAllowsDomains).To(ContainElement(i42dAllowLoop+"-proxy."+withAllowsNS+".svc"),
			"the model proxy FQDN must still be in the DNS allowlist")

		// No allows: the egress proxy does not exist; its FQDN must NOT appear.
		noAllowsNS := "i42d-kapt-noallow-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: noAllowsNS}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: noAllowsNS}}) })
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42dPolicyName, Namespace: noAllowsNS},
			Spec:       coxv1alpha1.AgentPolicySpec{},
		})).To(Succeed())
		noAllowLoop := buildLoop(i42dAllowLoop, noAllowsNS, nil)
		Expect(k8sClient.Create(ctx, noAllowLoop)).To(Succeed())
		reconcileLoop(i42dAllowLoop, noAllowsNS)

		noAllowsDomains := getDNSDomains(noAllowsNS)
		Expect(noAllowsDomains).ToNot(ContainElement(egFQDN),
			"with no network allows the egress proxy FQDN must not be in the DNS allowlist")
	})

	// spec 8 (I42d, kind acceptance finding): the agent's env var URL hosts
	// (COX_MODEL_BASE_URL, HTTPS_PROXY) must be resolvable by the agent, which
	// means the KubeArmorPolicy DNS allowlist must contain the full
	// .svc.cluster.local FQDNs that the resolver sends (ndots:1, absolute
	// names). The FQDNs are built from proxyServiceName / egressProxyServiceName
	// (not literals) so a rename in the controller breaks this test.
	It("pins that the agent env URL hosts are in the KubeArmorPolicy DNS allowlist (full FQDN form)", func() {
		loopName := "dnsallow-loop"
		ns := setup(loopName, []string{i42dAllowHost}, nil)

		// Build the FQDNs from the same helpers the controller uses.
		modelFQDN := proxyServiceName(loopName) + "." + ns + ".svc.cluster.local"
		egressFQDN := egressProxyServiceName(loopName) + "." + ns + ".svc.cluster.local"

		// The env vars carry the full-form URLs.
		env := agentEnvOf(loopName, ns)
		Expect(env[coxModelBaseURL]).To(HavePrefix("http://"+modelFQDN+":"),
			"COX_MODEL_BASE_URL must use the full .svc.cluster.local FQDN")
		Expect(env["HTTPS_PROXY"]).To(HavePrefix("http://"+egressFQDN+":"),
			"HTTPS_PROXY must use the full .svc.cluster.local FQDN")

		// The KubeArmorPolicy DNS allowlist must contain both FQDNs so the
		// agent can resolve them (ndots:1 means the resolver sends the
		// absolute name, which must match an allowlist entry exactly or as a
		// prefix, depending on KubeArmor's matching). The allowlist carries
		// the .svc form (not .svc.cluster.local) because KubeArmor matches
		// on the name the policy author wrote; with ndots:1 the resolver
		// sends the .svc.cluster.local form, so the allowlist must cover it.
		// The egress FQDN is added by the enforcer (I42d); the model FQDN is
		// added by policy.Translate (D33). Both are the .svc form.
		getDNSDomains := func() []string {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(engine.KubeArmorGVK)
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "coxswain-" + loopName}, obj)).To(Succeed())
			network, _, _ := unstructured.NestedMap(obj.Object, "spec", "network")
			items, _ := network["matchDNSQueries"].([]any)
			domains := make([]string, 0, len(items))
			for _, it := range items {
				domains = append(domains, it.(map[string]any)["domain"].(string))
			}
			return domains
		}
		domains := getDNSDomains()
		// The allowlist has the .svc form; the resolver (ndots:1) sends the
		// .svc.cluster.local form. KubeArmor's prefix matching handles the
		// suffix difference: <name>.<ns>.svc is a prefix of
		// <name>.<ns>.svc.cluster.local. Pin both: the .svc entry must be
		// present (it's what the enforcer writes) and the .svc.cluster.local
		// form must be the one the resolver sends (it's what the env URL
		// host is).
		Expect(domains).To(ContainElement(modelFQDN[:len(modelFQDN)-len(".cluster.local")]),
			"the model proxy .svc FQDN must be in the DNS allowlist (enforcer-written form)")
		Expect(domains).To(ContainElement(egressFQDN[:len(egressFQDN)-len(".cluster.local")]),
			"the egress proxy .svc FQDN must be in the DNS allowlist (enforcer-written form)")
	})

	// spec 9 (I42d, kind acceptance finding): the agent pod must carry
	// ndots=1 so the resolver sends absolute names for in-cluster Service
	// URLs without search-suffix expansion. Without it, the resolver first
	// queries <name>.<search-suffix> which is not on the KubeArmor DNS
	// allowlist, so KubeArmor denies the lookup and the agent cannot resolve
	// the proxy or egress proxy Service names.
	It("sets ndots=1 on the agent pod so in-cluster Service URLs resolve without search expansion", func() {
		loopName := "ndots-loop"
		ns := setup(loopName, []string{i42dAllowHost}, nil)

		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sandboxName(loopName), Namespace: ns}, sb)).To(Succeed())
		dnsCfg := sb.Spec.PodTemplate.Spec.DNSConfig
		Expect(dnsCfg).ToNot(BeNil(),
			"the agent pod must carry a dnsConfig (ndots:1) so in-cluster URLs resolve")
		var found bool
		for _, opt := range dnsCfg.Options {
			if opt.Name == "ndots" && opt.Value != nil && *opt.Value == "1" {
				found = true
			}
		}
		Expect(found).To(BeTrue(),
			"the agent pod dnsConfig must set ndots=1 (search-suffix expansion breaks KubeArmor DNS allowlist matching)")

		// The env URLs use the full .svc.cluster.local form so the resolver
		// sends an absolute name that matches the KubeArmor allowlist.
		env := agentEnvOf(loopName, ns)
		Expect(env["HTTPS_PROXY"]).To(ContainSubstring(".svc.cluster.local:"),
			"HTTPS_PROXY must use the full .svc.cluster.local form (ndots:1 sends absolute names)")
		Expect(env[coxModelBaseURL]).To(ContainSubstring(".svc.cluster.local:"),
			"COX_MODEL_BASE_URL must use the full .svc.cluster.local form")
	})

	// spec 6 (handoff): the env must UPDATE on an existing Sandbox when the
	// effective policy's network allows are added or removed — the operator
	// re-asserts the agent container spec on every reconcile (CreateOrUpdate
	// mutates the live sandbox), so a policy edit propagates to the sandbox
	// spec (and, via the sandbox controller's template-hash reconcile, to the
	// pod). Same Loop: add the allows, re-reconcile -> env appears; remove
	// them, re-reconcile -> env is gone again.
	It("updates the existing Sandbox's agent env when network allows are added or removed", func() {
		loopName := "upd-loop"
		// Start with NO allows: the sandbox exists without the proxy env.
		ns := setup(loopName, nil, nil)

		env := agentEnvOf(loopName, ns)
		Expect(env).ToNot(HaveKey("HTTPS_PROXY"), "setup: no allows -> no proxy env on the new sandbox")

		// Add a network allow and re-reconcile the SAME Loop: the existing
		// sandbox's agent env must gain the proxy vars.
		setPolicyNetwork(ns, []string{i42dAllowHost})
		reconcileLoop(loopName, ns)

		env = agentEnvOf(loopName, ns)
		wantURL := wantEgressProxyURL(loopName, ns)
		Expect(env["HTTPS_PROXY"]).To(Equal(wantURL),
			"adding a network allow must set HTTPS_PROXY on the existing sandbox")
		Expect(env["HTTP_PROXY"]).To(Equal(wantURL))
		Expect(env).To(HaveKey("NO_PROXY"))

		// Remove the allow and re-reconcile: the proxy vars must be gone.
		setPolicyNetwork(ns, nil)
		reconcileLoop(loopName, ns)

		env = agentEnvOf(loopName, ns)
		Expect(env).ToNot(HaveKey("HTTPS_PROXY"), "removing the allows must unset HTTPS_PROXY on the existing sandbox")
		Expect(env).ToNot(HaveKey("HTTP_PROXY"))
		Expect(env).ToNot(HaveKey("NO_PROXY"))
	})
})
