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

// D41b (ADR-0008, plan section D41b): AgentPolicy.spec.tools[] + validation.
// The CRD CEL rules reject in-cluster tool upstreams (.svc / .cluster.local /
// localhost / 127.0.0.1) at admission; the controller-side check rejects IP
// literals in the operator's pod/service CIDR (which need cluster config).
//
// An offending tool upstream does NOT return a reconcile error. It sets the
// Loop's PolicyValid=False (reason ToolUpstreamInCluster) and suspends the
// sandbox — the same fail-closed pattern I42e uses for in-cluster network
// allows.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	d41PolicyValidType       = "PolicyValid"
	d41ToolUpstreamInCluster = "ToolUpstreamInCluster"
	d41TestRepo              = "https://github.com/papawattu/coxswain.git"
	d41PodCIDR               = "10.244.0.0/16"
	d41ServiceCIDR           = "10.96.0.0/12"
	d41SVCUpstream           = "http://my-svc.default.svc:8080"
	d41LocalhostUpstream     = "http://localhost:8080"
	d41ClusterLocalUpstream  = "http://my-svc.default.cluster.local:8080"
	d41PodCIDRUpstream       = "http://10.244.0.5:8080"
	d41ServiceCIDRUpstream   = "http://10.96.0.1:443"
	d41ExternalUpstream      = "https://api.github.com"
	d41PodCIDRPolicyName     = "podcidr-pol"
	d41PodCIDRLoopName       = "podcidr-loop"
	d41SvcIDPolicyName       = "svcid-pol"
	d41SvcIDLoopName         = "svcid-loop"
)

var _ = Describe("D41b: AgentPolicy.spec.tools[] validation", func() {
	var (
		ctx context.Context
		r   *LoopReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		r = &LoopReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			PodCIDR:     d41PodCIDR,
			ServiceCIDR: d41ServiceCIDR,
		}
	})

	// buildToolSpec returns a valid ToolSpec with the given upstream and name.
	buildToolSpec := func(name, upstream string) coxv1alpha1.ToolSpec {
		return coxv1alpha1.ToolSpec{
			Name:     name,
			Upstream: upstream,
			CredentialSecretRef: coxv1alpha1.CredentialSecretRef{
				Name: testCredSecretName,
				Key:  "token",
			},
			Rules: []coxv1alpha1.ToolRule{
				{
					Methods: []string{httpMethodGet},
					Paths:   []string{"/repos/acme/*"},
				},
			},
		}
	}

	// buildLoop returns a Loop referencing the given policy names.
	buildLoop := func(name, ns string, policyRefs []string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "d41b test",
				Workspace:  coxv1alpha1.Workspace{Repo: d41TestRepo, Ref: loopRef},
				PolicyRefs: policyRefs,
				Agent:      coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
			},
		}
	}

	// spec 1: .svc upstream rejected at admission (CEL rule).
	It("spec 1: rejects a .svc upstream at admission (CEL rule)", func() {
		ns := "d41-svc-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "svc-pol", Namespace: ns},
			Spec: coxv1alpha1.AgentPolicySpec{
				Tools: []coxv1alpha1.ToolSpec{
					buildToolSpec("github", d41SVCUpstream),
				},
			},
		}
		err := k8sClient.Create(ctx, ap)
		Expect(err).To(HaveOccurred(),
			"the CRD CEL rule must reject a .svc upstream at admission (D41b)")
	})

	// spec 2: localhost / .cluster.local upstreams rejected at admission (CEL rule).
	It("spec 2: rejects localhost and .cluster.local upstreams at admission (CEL rule)", func() {
		ns := "d41-localhost-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// localhost
		ap1 := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "localhost-pol", Namespace: ns},
			Spec: coxv1alpha1.AgentPolicySpec{
				Tools: []coxv1alpha1.ToolSpec{
					buildToolSpec("tool1", d41LocalhostUpstream),
				},
			},
		}
		err := k8sClient.Create(ctx, ap1)
		Expect(err).To(HaveOccurred(),
			"the CRD CEL rule must reject localhost at admission (D41b)")

		// .cluster.local
		ap2 := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "clusterlocal-pol", Namespace: ns},
			Spec: coxv1alpha1.AgentPolicySpec{
				Tools: []coxv1alpha1.ToolSpec{
					buildToolSpec("tool2", d41ClusterLocalUpstream),
				},
			},
		}
		err = k8sClient.Create(ctx, ap2)
		Expect(err).To(HaveOccurred(),
			"the CRD CEL rule must reject .cluster.local at admission (D41b)")
	})

	// spec 3: IP in pod CIDR rejected controller-side.
	It("spec 3: rejects an IP in the pod CIDR controller-side (ToolUpstreamInCluster)", func() {
		ns := "d41-podcidr-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d41PodCIDRPolicyName, Namespace: ns},
			Spec: coxv1alpha1.AgentPolicySpec{
				Tools: []coxv1alpha1.ToolSpec{
					buildToolSpec("github", d41PodCIDRUpstream),
				},
			},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed(),
			"the CRD CEL rule cannot check CIDR membership (no cluster config)")

		loop := buildLoop(d41PodCIDRLoopName, ns, []string{d41PodCIDRPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: d41PodCIDRLoopName}})
		Expect(err).NotTo(HaveOccurred(), "a ToolUpstreamInCluster must NOT be a reconcile error (no requeue loop)")

		gotLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: d41PodCIDRLoopName}, gotLoop)).To(Succeed())
		var policyValid *metav1.Condition
		for i := range gotLoop.Status.Conditions {
			if gotLoop.Status.Conditions[i].Type == d41PolicyValidType {
				policyValid = &gotLoop.Status.Conditions[i]
			}
		}
		Expect(policyValid).ToNot(BeNil(), "the Loop must have a PolicyValid condition")
		Expect(policyValid.Status).To(Equal(metav1.ConditionFalse))
		Expect(policyValid.Reason).To(Equal(d41ToolUpstreamInCluster))
		Expect(policyValid.Message).To(ContainSubstring(d41PodCIDRUpstream),
			"the message must name the offending upstream")

		// No sandbox pod must be created.
		sandboxPod := &corev1.Pod{}
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: d41PodCIDRLoopName + "-sandbox"}, sandboxPod)
		Expect(err).To(HaveOccurred(), "a Loop with a ToolUpstreamInCluster must NOT get a sandbox pod")
	})

	// spec 4: IP in service CIDR rejected controller-side.
	It("spec 4: rejects an IP in the service CIDR controller-side (ToolUpstreamInCluster)", func() {
		ns := "d41-svcid-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d41SvcIDPolicyName, Namespace: ns},
			Spec: coxv1alpha1.AgentPolicySpec{
				Tools: []coxv1alpha1.ToolSpec{
					buildToolSpec("github", d41ServiceCIDRUpstream),
				},
			},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed())

		loop := buildLoop(d41SvcIDLoopName, ns, []string{d41SvcIDPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: d41SvcIDLoopName}})
		Expect(err).NotTo(HaveOccurred())

		gotLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: d41SvcIDLoopName}, gotLoop)).To(Succeed())
		var policyValid *metav1.Condition
		for i := range gotLoop.Status.Conditions {
			if gotLoop.Status.Conditions[i].Type == d41PolicyValidType {
				policyValid = &gotLoop.Status.Conditions[i]
			}
		}
		Expect(policyValid).ToNot(BeNil())
		Expect(policyValid.Status).To(Equal(metav1.ConditionFalse))
		Expect(policyValid.Reason).To(Equal(d41ToolUpstreamInCluster))
	})

	// spec 5: legitimate upstream passes.
	It("spec 5: allows a legitimate external upstream (no rejection)", func() {
		ns := "d41-external-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "external-pol", Namespace: ns},
			Spec: coxv1alpha1.AgentPolicySpec{
				Tools: []coxv1alpha1.ToolSpec{
					buildToolSpec("github", d41ExternalUpstream),
				},
			},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed(),
			"a legitimate external upstream must pass the CRD CEL rule")

		// The controller-side check must not flag it.
		offending, ok := r.findInClusterToolUpstream(ap.Spec.Tools)
		Expect(ok).To(BeFalse(), "a legitimate external upstream must NOT be flagged")
		Expect(offending).To(Equal(""))
	})

	// spec 6: bad method / duplicate name / non-http scheme rejected (CEL, API server rejection).
	It("spec 6: rejects bad method, duplicate name, and non-http scheme (CEL rule)", func() {
		ns := "d41-cel-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// Bad method (lowercase "get" instead of "GET").
		ap1 := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "badmethod-pol", Namespace: ns},
			Spec: coxv1alpha1.AgentPolicySpec{
				Tools: []coxv1alpha1.ToolSpec{
					{
						Name:     "tool1",
						Upstream: d41ExternalUpstream,
						Rules: []coxv1alpha1.ToolRule{
							{Methods: []string{"get"}, Paths: []string{"/"}},
						},
					},
				},
			},
		}
		err := k8sClient.Create(ctx, ap1)
		Expect(err).To(HaveOccurred(),
			"the CRD CEL rule must reject a bad method (lowercase) at admission (D41b)")

		// Duplicate name.
		ap2 := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "dupname-pol", Namespace: ns},
			Spec: coxv1alpha1.AgentPolicySpec{
				Tools: []coxv1alpha1.ToolSpec{
					buildToolSpec("github", d41ExternalUpstream),
					buildToolSpec("github", "https://api.github.com"),
				},
			},
		}
		err = k8sClient.Create(ctx, ap2)
		Expect(err).To(HaveOccurred(),
			"the CRD CEL rule must reject duplicate tool names at admission (D41b)")

		// Non-http scheme (ftp://).
		ap3 := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "badscheme-pol", Namespace: ns},
			Spec: coxv1alpha1.AgentPolicySpec{
				Tools: []coxv1alpha1.ToolSpec{
					{
						Name:     "tool3",
						Upstream: "ftp://api.github.com",
						Rules:    []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}, Paths: []string{"/"}}},
					},
				},
			},
		}
		err = k8sClient.Create(ctx, ap3)
		Expect(err).To(HaveOccurred(),
			"the CRD CEL rule must reject a non-http scheme at admission (D41b)")
	})

	// spec 7: no tools → no change in behaviour.
	It("spec 7: no tools → no change in behaviour (existing policy with only network allows)", func() {
		ns := "d41-notools-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// A policy with only network allows (no tools).
		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "netonly-pol", Namespace: ns},
			Spec: coxv1alpha1.AgentPolicySpec{
				Network: []string{"proxy.golang.org:443"},
			},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed(),
			"an existing policy with only network allows must still work")

		// The controller-side tool upstream check must not flag anything (no tools).
		offending, ok := r.findInClusterToolUpstream(ap.Spec.Tools)
		Expect(ok).To(BeFalse(), "a policy with no tools must not be flagged by the tool upstream check")
		Expect(offending).To(Equal(""))
	})
})

// D41b: the controller-side tool upstream check handles the CIDR cases (which
// need the operator's CIDR config) and the in-cluster name cases (mirrored, so
// pre-rule objects and future CRD drift are caught).
var _ = Describe("D41b: controller-side findInClusterToolUpstream", func() {
	var r *LoopReconciler

	BeforeEach(func() {
		r = &LoopReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			PodCIDR:     d41PodCIDR,
			ServiceCIDR: d41ServiceCIDR,
		}
	})

	It("rejects IPs in the pod CIDR", func() {
		tools := []coxv1alpha1.ToolSpec{
			{Name: "t1", Upstream: "http://10.244.0.5:8080", Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
			{Name: "t2", Upstream: "http://10.244.99.99:443", Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
		}
		for _, t := range tools {
			offending, ok := r.findInClusterToolUpstream([]coxv1alpha1.ToolSpec{t})
			Expect(ok).To(BeTrue(), "upstream %s in pod CIDR %s must be rejected", t.Upstream, d41PodCIDR)
			Expect(offending).To(Equal(t.Upstream))
		}
	})

	It("rejects IPs in the service CIDR", func() {
		tools := []coxv1alpha1.ToolSpec{
			{Name: "t1", Upstream: "http://10.96.0.1:443", Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
			{Name: "t2", Upstream: "http://10.96.255.255:80", Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
		}
		for _, t := range tools {
			offending, ok := r.findInClusterToolUpstream([]coxv1alpha1.ToolSpec{t})
			Expect(ok).To(BeTrue(), "upstream %s in service CIDR %s must be rejected", t.Upstream, d41ServiceCIDR)
			Expect(offending).To(Equal(t.Upstream))
		}
	})

	It("rejects in-cluster name suffixes", func() {
		tools := []coxv1alpha1.ToolSpec{
			{Name: "t1", Upstream: "http://my-svc.default.svc:443", Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
			{Name: "t2", Upstream: "http://api.default.SVC:443", Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
			{Name: "t3", Upstream: "http://my-svc.default.cluster.local:443", Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
		}
		for _, t := range tools {
			offending, ok := r.findInClusterToolUpstream([]coxv1alpha1.ToolSpec{t})
			Expect(ok).To(BeTrue(), "upstream %s must be rejected as an in-cluster name suffix", t.Upstream)
			Expect(offending).To(Equal(t.Upstream))
		}
	})

	It("rejects localhost and loopback IPs", func() {
		tools := []coxv1alpha1.ToolSpec{
			{Name: "t1", Upstream: "http://localhost:8080", Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
			{Name: "t2", Upstream: "http://127.0.0.1:443", Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
			{Name: "t3", Upstream: "http://127.0.0.2:80", Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
			{Name: "t4", Upstream: "http://0.0.0.0:80", Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
		}
		for _, t := range tools {
			offending, ok := r.findInClusterToolUpstream([]coxv1alpha1.ToolSpec{t})
			Expect(ok).To(BeTrue(), "upstream %s must be rejected (localhost/loopback)", t.Upstream)
			Expect(offending).To(Equal(t.Upstream))
		}
	})

	It("allows legitimate external upstreams", func() {
		tools := []coxv1alpha1.ToolSpec{
			{Name: "t1", Upstream: "https://api.github.com", Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
			{Name: "t2", Upstream: "https://8.8.8.8:443", Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
			{Name: "t3", Upstream: "https://proxy.golang.org", Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
		}
		for _, t := range tools {
			_, ok := r.findInClusterToolUpstream([]coxv1alpha1.ToolSpec{t})
			Expect(ok).To(BeFalse(), "upstream %s must NOT be flagged as in-cluster", t.Upstream)
		}
	})

	It("skips the operator-CIDR case when the reconciler has no CIDRs configured", func() {
		rNoCIDR := &LoopReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			// PodCIDR / ServiceCIDR are empty.
		}
		// An IP in a NON-standard range (TEST-NET-3) is flagged only via the
		// operator's explicit CIDR, not the standard range check.
		tools := []coxv1alpha1.ToolSpec{
			{Name: "t1", Upstream: "http://203.0.113.5:8080", Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
		}
		_, ok := rNoCIDR.findInClusterToolUpstream(tools)
		Expect(ok).To(BeFalse(),
			"with no operator CIDRs configured, an IP outside the standard ranges is not flagged by the operator-CIDR case")

		// ...and it IS flagged once the operator carves out that range.
		rWithCIDR := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), PodCIDR: "203.0.113.0/24"}
		_, ok = rWithCIDR.findInClusterToolUpstream(tools)
		Expect(ok).To(BeTrue(),
			"with the operator's pod CIDR configured, an IP in that CIDR is flagged")
	})

	It("still catches standard-range IPs even with no operator CIDRs (fail-closed)", func() {
		rNoCIDR := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		tools := []coxv1alpha1.ToolSpec{
			{Name: "t1", Upstream: "http://10.244.0.5:8080", Rules: []coxv1alpha1.ToolRule{{Methods: []string{httpMethodGet}}}},
		}
		_, ok := rNoCIDR.findInClusterToolUpstream(tools)
		Expect(ok).To(BeTrue(),
			"an IP in a standard private range is in-cluster regardless of the operator CIDR config")
	})
})

// D41b: toolUpstreamHost extracts the host from a tool upstream URL.
var _ = Describe("D41b: toolUpstreamHost", func() {
	It("extracts the host from a simple URL", func() {
		Expect(toolUpstreamHost("https://api.github.com")).To(Equal("api.github.com"))
		Expect(toolUpstreamHost("http://api.github.com")).To(Equal("api.github.com"))
	})

	It("extracts the host from a URL with a port", func() {
		Expect(toolUpstreamHost("https://api.github.com:8443")).To(Equal("api.github.com"))
		Expect(toolUpstreamHost("http://my-svc.default.svc:8080")).To(Equal("my-svc.default.svc"))
	})

	It("extracts the host from a URL with a path", func() {
		Expect(toolUpstreamHost("https://api.github.com/repos")).To(Equal("api.github.com"))
		Expect(toolUpstreamHost("http://my-svc.default.svc:8080/api/v1")).To(Equal("my-svc.default.svc"))
	})

	It("lowercases the host", func() {
		Expect(toolUpstreamHost("https://API.GITHUB.COM")).To(Equal("api.github.com"))
		Expect(toolUpstreamHost("http://MY-SVC.DEFAULT.SVC:8080")).To(Equal("my-svc.default.svc"))
	})

	It("trims a trailing dot", func() {
		Expect(toolUpstreamHost("http://my-svc.default.svc.")).To(Equal("my-svc.default.svc"))
	})

	It("handles IPv6 hosts (bracketed)", func() {
		Expect(toolUpstreamHost("http://[::1]:8080")).To(Equal("::1"))
		Expect(toolUpstreamHost("http://[fe80::1]:443")).To(Equal("fe80::1"))
	})

	It("returns empty for malformed URLs", func() {
		Expect(toolUpstreamHost("")).To(Equal(""))
		Expect(toolUpstreamHost("not-a-url")).To(Equal(""))
		Expect(toolUpstreamHost("https://")).To(Equal(""))
	})
})
