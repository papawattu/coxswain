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

// I42e (plan section I, ADR-0007 I42 resolution): the AgentPolicy CRD's
// validation rejects any spec.network allow that names an in-cluster target.
// The CRD CEL rule handles the name-suffix cases (.svc / .svc.cluster.local,
// localhost, 127.0.0.1) at admission; the controller-side check handles the
// IP-in-pod/service-CIDR cases (which need the operator's CIDR config).
//
// An offending allow does NOT return a reconcile error that requeues forever.
// It sets the Loop's PolicyValid=False (reason InClusterAllow) and suspends
// the sandbox — the same fail-closed pattern C6a uses for an unresolvable
// policyRefs reference. This is the first layer of the SSRF defence; the
// egress proxy's resolved-IP check (I42a) is the backstop that catches
// rebinding/split-horizon even if a name passes validation.

import (
	"context"

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
	// i42e test constants (shared across the I42e specs in this file).
	i42ePolicyValidType    = "PolicyValid"
	i42eInClusterAllow     = "InClusterAllow"
	i42eTestRepo           = "https://github.com/papawattu/coxswain.git"
	i42ePodCIDR            = "10.244.0.0/16"
	i42eServiceCIDR        = "10.96.0.0/12"
	i42eExternalHost       = "proxy.golang.org:443"
	i42eSVCInClusterAllow  = "my-service.default.svc:443"
	i42eLocalhostAllow     = "localhost:8080"
	i42ePodCIDRIPAllow     = "10.244.0.5:8080"
	i42eServiceCIDRIPAllow = "10.96.0.1:443"
	// i42eGitHubHost is the GitHub API's host:port (the external network allow
	// the I72 egress-proxy gate tests use). Shared with the I72 specs so a
	// rename cannot desync them (and the goconst threshold is not hit).
	i42eGitHubHost = "api.github.com:443"
)

var _ = Describe("I42e: reject in-cluster network allows", func() {
	var (
		ctx context.Context
		r   *LoopReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		// The reconciler gets the pod/service CIDRs from the operator config
		// (in envtest, set directly on the struct; in a real cluster they come
		// from the POD_CIDR / SERVICE_CIDR env vars, read in SetupWithManager).
		r = &LoopReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			PodCIDR:     i42ePodCIDR,
			ServiceCIDR: i42eServiceCIDR,
		}
	})

	buildLoop := func(name, ns string, policyRefs []string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "i42e test",
				Workspace:  coxv1alpha1.Workspace{Repo: i42eTestRepo, Ref: loopRef},
				PolicyRefs: policyRefs,
				Agent:      coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
			},
		}
	}

	// spec 1: .svc suffix rejected by the CRD CEL rule at admission.
	It("rejects a .svc hostname in spec.network at admission (CEL rule)", func() {
		ns := "i42e-svc-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "svc-pol", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42eSVCInClusterAllow}},
		}
		err := k8sClient.Create(ctx, ap)
		Expect(err).To(HaveOccurred(),
			"the CRD CEL rule must reject a .svc hostname at admission (I42e)")
	})

	// spec 2: IP in pod CIDR rejected by the controller-side check.
	It("rejects an IP in the pod CIDR (controller-side, InClusterAllow)", func() {
		ns := "i42e-podcidr-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "podcidr-pol", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42ePodCIDRIPAllow}},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed(),
			"the CRD CEL rule cannot check CIDR membership (no cluster config)")

		loop := buildLoop("podcidr-loop", ns, []string{"podcidr-pol"})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "podcidr-loop"}})
		Expect(err).NotTo(HaveOccurred(), "an InClusterAllow must NOT be a reconcile error (no requeue loop)")

		gotLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "podcidr-loop"}, gotLoop)).To(Succeed())
		var policyValid *metav1.Condition
		for i := range gotLoop.Status.Conditions {
			if gotLoop.Status.Conditions[i].Type == i42ePolicyValidType {
				policyValid = &gotLoop.Status.Conditions[i]
			}
		}
		Expect(policyValid).ToNot(BeNil(), "the Loop must have a PolicyValid condition")
		Expect(policyValid.Status).To(Equal(metav1.ConditionFalse))
		Expect(policyValid.Reason).To(Equal(i42eInClusterAllow))
		Expect(policyValid.Message).To(ContainSubstring(i42ePodCIDRIPAllow),
			"the message must name the offending host:port")

		// No sandbox pod must be created.
		sandboxPod := &corev1.Pod{}
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "podcidr-loop-sandbox"}, sandboxPod)
		Expect(err).To(HaveOccurred(), "a Loop with an InClusterAllow must NOT get a sandbox pod")
	})

	// spec 3: IP in service CIDR rejected by the controller-side check.
	It("rejects an IP in the service CIDR (controller-side, InClusterAllow)", func() {
		ns := "i42e-svcid-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "svcid-pol", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42eServiceCIDRIPAllow}},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed())

		loop := buildLoop("svcid-loop", ns, []string{"svcid-pol"})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "svcid-loop"}})
		Expect(err).NotTo(HaveOccurred())

		gotLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "svcid-loop"}, gotLoop)).To(Succeed())
		var policyValid *metav1.Condition
		for i := range gotLoop.Status.Conditions {
			if gotLoop.Status.Conditions[i].Type == i42ePolicyValidType {
				policyValid = &gotLoop.Status.Conditions[i]
			}
		}
		Expect(policyValid).ToNot(BeNil())
		Expect(policyValid.Status).To(Equal(metav1.ConditionFalse))
		Expect(policyValid.Reason).To(Equal(i42eInClusterAllow))
	})

	// spec 4: localhost rejected by the CRD CEL rule (the controller-side check
	// is the backstop; tested directly via findInClusterNetworkAllow below).
	It("rejects localhost in spec.network at admission (CEL rule) and controller-side", func() {
		ns := "i42e-localhost-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "localhost-pol", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42eLocalhostAllow}},
		}
		err := k8sClient.Create(ctx, ap)
		Expect(err).To(HaveOccurred(),
			"the CRD CEL rule must reject localhost at admission (I42e)")

		// Controller-side check (defence in depth) also catches it.
		offending, ok := r.findInClusterNetworkAllow([]string{i42eLocalhostAllow})
		Expect(ok).To(BeTrue(), "the controller-side check must catch localhost")
		Expect(offending).To(Equal(i42eLocalhostAllow))
	})

	// spec 5: legitimate external host passes.
	It("allows a legitimate external host (no InClusterAllow)", func() {
		ns := "i42e-external-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "external-pol", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42eExternalHost}},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed(),
			"a legitimate external host must pass the CRD CEL rule")

		_, ok := r.findInClusterNetworkAllow([]string{i42eExternalHost})
		Expect(ok).To(BeFalse(), "a legitimate external host must NOT be flagged")
	})

	// spec 6: the sandbox is NOT created when InClusterAllow fires.
	It("does not create a sandbox when InClusterAllow fires", func() {
		ns := "i42e-nosandbox-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "nosandbox-pol", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42ePodCIDRIPAllow}},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed())

		loop := buildLoop("nosandbox-loop", ns, []string{"nosandbox-pol"})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "nosandbox-loop"}})
		Expect(err).NotTo(HaveOccurred())

		// The sandbox (agent-sandbox CR) must NOT have been created.
		sb := &sandboxv1beta1.Sandbox{}
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "nosandbox-loop-sandbox"}, sb)
		Expect(err).To(HaveOccurred(),
			"a Loop with InClusterAllow must NOT get a sandbox (fail-closed)")
	})
})

// I42e: the controller-side check handles the CIDR cases (which need the
// operator's CIDR config) and localhost. The CRD CEL rule handles the .svc
// suffix cases at admission. Together they cover all in-cluster target types.
var _ = Describe("I42e: controller-side findInClusterNetworkAllow", func() {
	var r *LoopReconciler

	BeforeEach(func() {
		r = &LoopReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			PodCIDR:     i42ePodCIDR,
			ServiceCIDR: i42eServiceCIDR,
		}
	})

	It("rejects IPs in the pod CIDR", func() {
		allows := []string{"10.244.0.5:8080", "10.244.99.99:443"}
		for _, a := range allows {
			offending, ok := r.findInClusterNetworkAllow([]string{a})
			Expect(ok).To(BeTrue(), "IP %s in pod CIDR %s must be rejected", a, i42ePodCIDR)
			Expect(offending).To(Equal(a))
		}
	})

	It("rejects IPs in the service CIDR", func() {
		allows := []string{"10.96.0.1:443", "10.96.255.255:80"}
		for _, a := range allows {
			offending, ok := r.findInClusterNetworkAllow([]string{a})
			Expect(ok).To(BeTrue(), "IP %s in service CIDR %s must be rejected", a, i42eServiceCIDR)
			Expect(offending).To(Equal(a))
		}
	})

	It("rejects loopback / unspecified / link-local / CGNAT / multicast IP forms (review P2/P3)", func() {
		// P3 + P2(range): the first layer rejects the non-allowlisted IP ranges
		// by range (egress.IPInCarveOuts OR the Go range predicates), not a fixed
		// string list — so 127.0.0.3, 169.254.1.1, [fe80::1], [fd12::1], CGNAT
		// 100.64.1.1, and IPv6 site-local multicast [ff02::1] are all caught.
		// The egress proxy's resolved-IP check (I42a) is the backstop for
		// rebinding / split-horizon, but these forms are caught here too.
		for _, a := range []string{
			"localhost:8080", "127.0.0.1:443", "127.0.0.0:443", "127.0.0.2:80",
			"127.0.0.3:80", "127.0.0.255:80", "127.255.255.255:80", "0.0.0.0:80",
			"169.254.0.0:80", "169.254.1.1:80", "169.254.169.254:80",
			"[::1]:443", "[::]:443", "[fe80::1]:443", "[fd12::1]:443",
			"[ff02::1]:443", "100.64.1.1:443", "192.168.1.10:443",
			"255.255.255.255:80", "224.0.0.1:443",
		} {
			offending, ok := r.findInClusterNetworkAllow([]string{a})
			Expect(ok).To(BeTrue(), "%s must be rejected as an in-cluster non-allowlisted IP range", a)
			Expect(offending).To(Equal(a))
		}
	})

	It("rejects in-cluster name suffixes case-insensitively and trailing-dot-aware (review P2)", func() {
		// P2: the controller mirrors the CRD CEL rule's name check, but
		// case-insensitively and trailing-dot-aware, so objects created before
		// the rule and future CRD drift are caught too.
		for _, a := range []string{
			"my-service.default.svc:443", "api.default.SVC:443",
			"Kubernetes.Default.Svc:443", "my-svc.default.svc.:443",
			"kubernetes.default.svc.cluster.local.:443",
			"KUBERNETES.DEFAULT.SVC.:443", "my-svc.default.cluster.local:443",
		} {
			offending, ok := r.findInClusterNetworkAllow([]string{a})
			Expect(ok).To(BeTrue(), "%s must be rejected as an in-cluster name suffix", a)
			Expect(offending).To(Equal(a))
		}
	})

	It("allows legitimate external hosts and IPs", func() {
		for _, a := range []string{i42eExternalHost, "8.8.8.8:443", "1.1.1.1:53", "servicewarehouse.example.com:443", i42eGitHubHost} {
			_, ok := r.findInClusterNetworkAllow([]string{a})
			Expect(ok).To(BeFalse(), "%s must NOT be flagged as in-cluster", a)
		}
	})

	It("ignores malformed entries (no port)", func() {
		// A bare host (no :port) is not a valid network allow; the
		// hostPartOfAllow returns "" and the check skips it. The egress
		// proxy rejects it at dial time.
		_, ok := r.findInClusterNetworkAllow([]string{"proxy.golang.org"})
		Expect(ok).To(BeFalse(), "a malformed entry (no port) is skipped")
	})

	It("skips only the operator-CIDR case when the reconciler has no CIDRs configured", func() {
		// When PodCIDR / ServiceCIDR are empty (operator config not set), the
		// operator-CIDR case is skipped. Use an IP in a NON-standard range
		// (TEST-NET-3) so it is flagged only via the operator's explicit CIDR,
		// not the standard range check (which now always runs, correctly
		// fail-closed, for 127/8 / RFC1918 / link-local / CGNAT / multicast).
		rNoCIDR := &LoopReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			// PodCIDR / ServiceCIDR are empty.
		}
		_, ok := rNoCIDR.findInClusterNetworkAllow([]string{"203.0.113.5:8080"})
		Expect(ok).To(BeFalse(),
			"with no operator CIDRs configured, an IP outside the standard ranges is not flagged by the operator-CIDR case")
		// ...and it IS flagged once the operator carves out that range.
		rWithCIDR := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), PodCIDR: "203.0.113.0/24"}
		_, ok = rWithCIDR.findInClusterNetworkAllow([]string{"203.0.113.5:8080"})
		Expect(ok).To(BeTrue(),
			"with the operator's pod CIDR configured, an IP in that CIDR is flagged")
	})

	It("still catches standard-range IPs even with no operator CIDRs (fail-closed)", func() {
		// The standard-range check (loopback / RFC1918 / link-local / CGNAT /…
		// multicast) runs regardless of the operator CIDR config — a 10.x pod
		// IP is in-cluster whether or not the operator told us the pod CIDR.
		rNoCIDR := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, ok := rNoCIDR.findInClusterNetworkAllow([]string{"10.244.0.5:8080"})
		Expect(ok).To(BeTrue(),
			"an IP in a standard private range is in-cluster regardless of the operator CIDR config")
	})
})
