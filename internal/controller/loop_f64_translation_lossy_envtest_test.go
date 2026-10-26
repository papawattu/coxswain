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

// F64 (R14 I41 remainder): PolicyTranslationLossy is about a port NOTHING
// carries, not about the KubeArmor translation on its own.
//
// KubeArmor matches egress by hostname + protocol name, so a host:PORT allow
// becomes "host" + "tcp" — any port on that host. The condition must therefore
// report the allows whose port no other layer pins:
//
//   - the egress proxy pins the agent's allows at CONNECT time, but only once
//     its pod is owned by the Loop and Ready (the I42b gate); before that the
//     sandbox is Suspended and nothing carries the port;
//   - the model proxy's NetworkPolicy pins the model endpoint's port where
//     modelPeer can express it (ADR-0007 keeps the condition for the model
//     proxy's egress).
//
// The specs below pin the True state, the True->False transition on the same
// Loop (I43), the model endpoint half, and the decision helper directly.
package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// f64Allow is a host:PORT network allow: KubeArmor drops the port.
	f64Allow = "pypi.org:443"
	// f64Policy is the AgentPolicy name the specs reference.
	f64Policy = "f64-pol"
	// f64Repo is an in-cluster .svc repo (the shared fixture): the workspace-init
	// clone does not traverse the egress proxy, so the proxy exists only because
	// of the network allow (the F55/F70 shape).
	f64Repo = testRepoURL
	// f64ExternalEndpoint is a model endpoint whose host no NetworkPolicy rule
	// can express (modelPeer returns nil), so nothing carries the port.
	f64ExternalEndpoint = "api.example.com:443"
	// f64InClusterEndpoint is an in-cluster Service endpoint: modelPeer pins host
	// AND port on the model proxy's NetworkPolicy, so the port is carried.
	f64InClusterEndpoint = "vllm.llm.svc.cluster.local:8000"
	// f64TransitionSettle keeps the second reconcile in a later wall-clock second:
	// metav1.Time (LastTransitionTime) is second-granular, so a True->False
	// transition inside one second would not move it.
	f64TransitionSettle = 1100 * time.Millisecond
)

// f64Loop builds the Loop these specs reconcile: (optionally) a model endpoint
// and the given policyRefs.
func f64Loop(name, ns, endpoint string, policyRefs []string) *coxv1alpha1.Loop {
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(name + "-uid")},
		Spec: coxv1alpha1.LoopSpec{
			Goal:       "F64 translation lossy",
			Workspace:  coxv1alpha1.Workspace{Repo: f64Repo, Ref: loopRef},
			PolicyRefs: policyRefs,
			Agent:      coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
		},
	}
	if endpoint != "" {
		// The pair is required (D34): the endpoint and the Secret carrying its key.
		loop.Spec.Agent.ModelEndpoint = endpoint
		loop.Spec.Agent.EndpointSecretRef = "model-creds"
	}
	return loop
}

// f64OwnedPod returns a pod controller-owned by loop (the IsControlledBy
// precondition the carrier check reads) with PodReady set to ready.
func f64OwnedPod(loop *coxv1alpha1.Loop, name string, ready bool) *corev1.Pod {
	owned := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: loop.Namespace,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion:         coxv1alpha1.GroupVersion.String(),
			Kind:               "Loop",
			Name:               loop.Name,
			UID:                loop.UID,
			Controller:         &owned,
			BlockOwnerDeletion: &owned,
		}},
	}}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	return pod
}

// f64GetLoop reads the Loop back (the persisted status, not the in-memory copy).
func f64GetLoop(ns, name string) *coxv1alpha1.Loop {
	loop := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
	return loop
}

var _ = Describe("F64: PolicyTranslationLossy tracks the carrier, not the translation", func() {
	var (
		ctx context.Context
		r   *LoopReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		r = &LoopReconciler{
			CNIProber: testCNIProber(),
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			// AllowUnenforced: the D30 enforcement gate is not what these specs
			// are about (envtest has no Enforcer); this isolates the condition.
			AllowUnenforced: true,
		}
	})

	// Design spec 2: a host:PORT allow with no carrier is True/KubeArmorDroppedPorts.
	It("reports a host:PORT allow as lossy while the egress proxy is not Ready", func() {
		ns := "f64-noproxy-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: f64Policy, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{f64Allow}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, f64Loop("f64-noproxy", ns, "", []string{f64Policy}))).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "f64-noproxy"}})
		Expect(err).NotTo(HaveOccurred())

		c := translationLossyCondition(f64GetLoop(ns, "f64-noproxy").Status.Conditions)
		Expect(c).NotTo(BeNil(), "the condition must be set on the first pass")
		Expect(c.Status).To(Equal(metav1.ConditionTrue),
			"the egress proxy pod exists but is not Ready, so nothing carries the port")
		Expect(c.Reason).To(Equal("KubeArmorDroppedPorts"))
		Expect(c.Message).To(ContainSubstring(f64Allow))
	})

	// Design specs 1 + 3: proxy owned and Ready -> False/NoPortLoss, on the SAME
	// Loop whose earlier pass said True (I43: the condition is updated).
	It("clears the condition when the egress proxy becomes Ready on the same Loop", func() {
		ns := "f64-ready-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: f64Policy, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{f64Allow}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, f64Loop("f64-ready", ns, "", []string{f64Policy}))).To(Succeed())

		// Pass 1: the egress proxy pod is created, not Ready.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "f64-ready"}})
		Expect(err).NotTo(HaveOccurred())
		before := translationLossyCondition(f64GetLoop(ns, "f64-ready").Status.Conditions)
		Expect(before).NotTo(BeNil())
		Expect(before.Status).To(Equal(metav1.ConditionTrue), "nothing carries the port yet")

		// The proxy comes up: owned by the Loop and Ready.
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: egressProxyPodName("f64-ready")}, pod)).To(Succeed())
		Expect(metav1.IsControlledBy(pod, f64GetLoop(ns, "f64-ready"))).To(BeTrue(),
			"the operator creates the egress proxy pod owned by the Loop")
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		// LastTransitionTime is second-granular; keep the transition in a later second
		// so the freshness assertion below is not a flake.
		time.Sleep(f64TransitionSettle)

		// Pass 2: the carrier is up, so the port is pinned at CONNECT time.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "f64-ready"}})
		Expect(err).NotTo(HaveOccurred())
		after := translationLossyCondition(f64GetLoop(ns, "f64-ready").Status.Conditions)
		Expect(after).NotTo(BeNil())
		Expect(after.Status).To(Equal(metav1.ConditionFalse),
			"a Ready, owned egress proxy carries the port, so the condition must clear")
		Expect(after.Reason).To(Equal("NoPortLoss"))
		Expect(after.Message).NotTo(ContainSubstring(f64Allow),
			"the cleared message must not still name the allow as unpinned")
		Expect(after.LastTransitionTime.Equal(&before.LastTransitionTime)).To(BeFalse(),
			"I43: a True->False transition must move LastTransitionTime")
		Expect(after.LastTransitionTime.After(before.LastTransitionTime.Time)).To(BeTrue(),
			"the transition time is the later pass, not the creation value")
	})

	// ADR-0007's remaining half: the model proxy's egress. An endpoint no
	// NetworkPolicy rule can express has no carrier, so its port IS reported.
	It("reports a model endpoint no NetworkPolicy pins, and not one it does", func() {
		ns := "f64-endpoint-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// An external FQDN: modelPeer cannot express it, so no rule carries the port.
		Expect(k8sClient.Create(ctx, f64Loop("f64-ext", ns, f64ExternalEndpoint, nil))).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "f64-ext"}})
		Expect(err).NotTo(HaveOccurred())
		ext := translationLossyCondition(f64GetLoop(ns, "f64-ext").Status.Conditions)
		Expect(ext).NotTo(BeNil())
		Expect(ext.Status).To(Equal(metav1.ConditionTrue),
			"an external model endpoint's port is carried by nothing")
		Expect(ext.Reason).To(Equal("KubeArmorDroppedPorts"))
		Expect(ext.Message).To(ContainSubstring(f64ExternalEndpoint))

		// An in-cluster Service endpoint: the model proxy's NetworkPolicy pins
		// host AND port (modelPeer -> NamespaceSelector + port), so no loss.
		Expect(k8sClient.Create(ctx, f64Loop("f64-int", ns, f64InClusterEndpoint, nil))).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "f64-int"}})
		Expect(err).NotTo(HaveOccurred())
		in := translationLossyCondition(f64GetLoop(ns, "f64-int").Status.Conditions)
		Expect(in).NotTo(BeNil())
		Expect(in.Status).To(Equal(metav1.ConditionFalse),
			"an in-cluster endpoint's port is pinned by the model proxy NetworkPolicy")
		Expect(in.Reason).To(Equal("NoPortLoss"))
	})
})

// f64FakeClient is the unit-test client (core + the CRD group only).
func f64FakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	if err := coxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add coxv1alpha1 to scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// TestTranslationLossyAllows is the decision table for the single implementation
// (design spec 5): which shape reports a port and which does not. It drives the
// real helper, not a re-implementation.
func TestTranslationLossyAllows(t *testing.T) {
	const ns = "f64-unit"

	cases := []struct {
		name     string
		network  []string
		endpoint string
		// underProxy is what sits under the egress proxy pod's name: "" (no pod),
		// "notready", "ready" (owned by the Loop) or "foreign".
		underProxy string
		want       []string
	}{
		{
			name:    "bare host allow: nothing to lose",
			network: []string{"pypi.org"},
			want:    nil,
		},
		{
			name:    "host:PORT allow, no egress proxy pod yet: lossy",
			network: []string{f64Allow},
			want:    []string{f64Allow},
		},
		{
			name:       "host:PORT allow, egress proxy owned but not Ready: lossy",
			network:    []string{f64Allow},
			underProxy: "notready",
			want:       []string{f64Allow},
		},
		{
			name:       "host:PORT allow, foreign Ready pod: lossy (not a carrier)",
			network:    []string{f64Allow},
			underProxy: "foreign",
			want:       []string{f64Allow},
		},
		{
			name:       "host:PORT allow, egress proxy owned and Ready: carried",
			network:    []string{f64Allow},
			underProxy: "ready",
			want:       nil,
		},
		{
			name:     "external model endpoint with a port: lossy",
			endpoint: f64ExternalEndpoint,
			want:     []string{f64ExternalEndpoint},
		},
		{
			name:     "in-cluster model endpoint: the NetworkPolicy pins host and port",
			endpoint: f64InClusterEndpoint,
			want:     nil,
		},
		{
			name:     "model endpoint without a port: nothing to lose",
			endpoint: "api.example.com",
			want:     nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			loop := f64Loop("f64-unit", ns, tc.endpoint, nil)
			if len(tc.network) > 0 {
				loop.Spec.PolicyRefs = []string{f64Policy}
			}
			objs := []client.Object{}
			if len(tc.network) > 0 {
				objs = append(objs, &coxv1alpha1.AgentPolicy{
					ObjectMeta: metav1.ObjectMeta{Name: f64Policy, Namespace: ns},
					Spec:       coxv1alpha1.AgentPolicySpec{Network: tc.network},
				})
			}
			switch tc.underProxy {
			case "notready":
				objs = append(objs, f64OwnedPod(loop, egressProxyPodName(loop.Name), false))
			case "ready":
				objs = append(objs, f64OwnedPod(loop, egressProxyPodName(loop.Name), true))
			case "foreign":
				pod := f64OwnedPod(loop, egressProxyPodName(loop.Name), true)
				pod.OwnerReferences = nil // a foreign pod is not a carrier
				objs = append(objs, pod)
			}
			c := f64FakeClient(t, objs...)
			r := &LoopReconciler{Client: c, Scheme: c.Scheme(), AllowUnenforced: true}

			effective, err := r.effectivePolicy(ctx, loop)
			if err != nil {
				t.Fatalf("resolve effective policy: %v", err)
			}
			got := r.translationLossyAllows(ctx, loop, effective)
			if len(got) != len(tc.want) {
				t.Fatalf("translationLossyAllows() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("translationLossyAllows() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
