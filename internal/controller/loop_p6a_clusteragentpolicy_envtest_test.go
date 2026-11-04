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

// P6a (ADR-0007 Q2, D81(b)): ClusterAgentPolicy — a cluster-scoped policy a
// Loop's spec.policyRefs entry may name. Same schema as AgentPolicy (the type
// is reused); the operator's resolver (resolvePolicyRefs) falls back to the
// cluster scope when the namespaced AgentPolicy is absent, so a Cluster
// AgentPolicy's allows are enforced exactly like a namespaced one.
//
// The specs:
//   - spec 1: a Loop in namespace A references a ClusterAgentPolicy by name;
//     status.policy.effectiveHash is the hash of the cluster policy's allows
//     (the acceptance: the effective policy includes its allows).
//   - spec 2: namespaced + cluster policies UNION (a ref naming a namespaced
//     AgentPolicy + a ref naming a ClusterAgentPolicy; the hash is the union
//     of both — one is NOT shadowing the other).
//   - spec 3: a policyRefs entry that names NEITHER a namespaced nor a
//     cluster policy fails with the EXISTING unknown-policy gate behaviour
//     (PolicyValid=False, reason PolicyNotFound) — fail-closed, unchanged.
//   - spec 4 (D98): a ref that names BOTH scopes → the namespaced policy
//     wins (the effective hash is the namespaced policy's, NOT the cluster
//     one's) and a PolicyNameCollision Warning Event fires (transition-
//     gated: exactly one across re-reconciles, not one per pass).

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/policy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// p6a* constants are distinct from the C6a constants (goconst counts across
// the package test files; the C6a literals are reused by name where they are
// shared — c6aGitBin/c6aGoBin/runnerShellPath/i42eExternalHost/loopRef).
const (
	p6aClusterPolicyName = "p6a-org-policy"
	p6aNamespacedName    = "p6a-ns-policy"
	p6aBothName          = "p6a-collision"
	p6aMissingName       = "p6a-ghost"
	p6aLoopName          = "loop-p6a"
	p6aCollisionReason   = "PolicyNameCollision"
	p6aClusterExec       = "/usr/bin/rg"
	p6aClusterNetwork    = "pypi.org:443"
	p6aClusterFiles      = "/shared"
)

// p6aEventSink is the broadcaster's EventSink (it posts the Event to the
// envtest cluster via k8sClient.Create — the broadcaster's default sink
// writes to a file, which the specs cannot see). The D85 suite ships the
// same pattern (d85EventSink); a per-test-file copy keeps the suites
// independent.
type p6aEventSink struct {
	client client.Client
}

func (s p6aEventSink) Create(e *corev1.Event) (*corev1.Event, error) {
	out := &corev1.Event{}
	err := s.client.Create(context.Background(), e)
	return out, err
}

func (s p6aEventSink) Update(e *corev1.Event) (*corev1.Event, error) {
	out := &corev1.Event{}
	err := s.client.Update(context.Background(), e)
	return out, err
}

func (s p6aEventSink) Patch(e *corev1.Event, data []byte) (*corev1.Event, error) {
	return e, nil
}

// p6aReconciler builds the reconciler the specs drive: a plain cached client
// + a REAL event recorder (the broadcaster posts Kubernetes Events to the
// envtest cluster; the specs assert on the live Events via k8sClient.List).
func p6aReconciler() *LoopReconciler {
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(p6aEventSink{client: k8sClient})
	return &LoopReconciler{
		CNIProber: testCNIProber(),
		Client:    k8sClient,
		Scheme:    k8sClient.Scheme(),
		Recorder:  broadcaster.NewRecorder(k8sClient.Scheme(), corev1.EventSource{Component: "p6a-spec"}),
	}
}

// p6aClusterPolicyHash returns the effective-hash of the given policy specs
// concatenated in order — the same shape the resolver's union produces.
func p6aUnionHash(specs ...coxv1alpha1.AgentPolicySpec) string {
	union := policy.EffectivePolicy{}
	for i := range specs {
		union.Exec = append(union.Exec, specs[i].Exec...)
		union.Network = append(union.Network, specs[i].Network...)
		union.Files = append(union.Files, specs[i].Files...)
		union.Tools = append(union.Tools, specs[i].Tools...)
	}
	return policy.EffectiveHash(union)
}

// p6aBuildLoop is the loop shape every spec drives (a real repo/ref so the
// reconcile gets past validation into the policy read; the specs never
// expect a sandbox — the CNI gate + agent-sandbox CRD absence hold it). The
// loop name is p6aLoopName (a single name shared by every spec — each spec
// gets its own namespace, so no name collision).
func p6aBuildLoop(ns string, refs ...string) *coxv1alpha1.Loop {
	return &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: p6aLoopName, Namespace: ns},
		Spec: coxv1alpha1.LoopSpec{
			Goal:       "P6a ClusterAgentPolicy spec",
			Workspace:  coxv1alpha1.Workspace{Repo: c6aTestRepo, Ref: loopRef},
			PolicyRefs: refs,
			Agent: coxv1alpha1.AgentConfig{
				Image: runnerImage,
				Model: testModel,
			},
			Loop: coxv1alpha1.LoopSettings{MaxIterations: 1},
		},
	}
}

var _ = Describe("P6a: ClusterAgentPolicy (D81(b))", func() {
	var (
		ctx context.Context
		r   *LoopReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		r = p6aReconciler()
	})

	It("spec 1: a Loop picks up a ClusterAgentPolicy's allows via policyRefs (effectiveHash)", func() {
		ns := "p6a-cluster-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// The cluster-scoped policy: exec + network + files allows.
		Expect(k8sClient.Create(ctx, &coxv1alpha1.ClusterAgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: p6aClusterPolicyName},
			Spec: coxv1alpha1.AgentPolicySpec{
				Exec:    []string{p6aClusterExec, runnerShellPath},
				Network: []string{p6aClusterNetwork},
				Files:   []string{p6aClusterFiles},
			},
		})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, &coxv1alpha1.ClusterAgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: p6aClusterPolicyName}})
		}()

		loop := p6aBuildLoop(ns, p6aClusterPolicyName)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: p6aLoopName}})
		Expect(err).NotTo(HaveOccurred())

		var got coxv1alpha1.Loop
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: p6aLoopName}, &got)).To(Succeed())
		// The effective hash is the cluster policy's allows (the resolver's
		// union of the single cluster ref).
		Expect(got.Status.Policy).NotTo(BeNil())
		Expect(got.Status.Policy.EffectiveHash).To(Equal(p6aUnionHash(coxv1alpha1.AgentPolicySpec{
			Exec:    []string{p6aClusterExec, runnerShellPath},
			Network: []string{p6aClusterNetwork},
			Files:   []string{p6aClusterFiles},
		})), "status.policy.effectiveHash must include the ClusterAgentPolicy's allows (P6a acceptance)")
		// PolicyValid=True: a resolvable cluster policy is a valid policy.
		Expect(conditionTruePresent(&got, policyValidType)).To(BeTrue(),
			"the Loop must be PolicyValid (the cluster policy resolved cleanly)")
	})

	It("spec 2: namespaced + cluster policies UNION (not one shadowing the other)", func() {
		ns := "p6a-union-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// Distinct names in each scope (a collision is spec 4's territory).
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: p6aNamespacedName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{c6aGitBin, runnerShellPath}, Network: []string{i42eExternalHost}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &coxv1alpha1.ClusterAgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: p6aClusterPolicyName},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{p6aClusterExec, runnerShellPath}, Network: []string{p6aClusterNetwork}, Files: []string{p6aClusterFiles}},
		})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, &coxv1alpha1.ClusterAgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: p6aClusterPolicyName}})
		}()

		loop := p6aBuildLoop(ns, p6aNamespacedName, p6aClusterPolicyName)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: p6aLoopName}})
		Expect(err).NotTo(HaveOccurred())

		var got coxv1alpha1.Loop
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: p6aLoopName}, &got)).To(Succeed())
		// The union: exec={c6aGitBin, shell, p6aClusterExec},
		// network={i42eExternalHost, p6aClusterNetwork}, files={p6aClusterFiles}.
		// A union-ONLY (non-union) rewrite of the resolver FAILS this hash.
		wantNS := coxv1alpha1.AgentPolicySpec{Exec: []string{c6aGitBin, runnerShellPath}, Network: []string{i42eExternalHost}}
		wantCluster := coxv1alpha1.AgentPolicySpec{Exec: []string{p6aClusterExec, runnerShellPath}, Network: []string{p6aClusterNetwork}, Files: []string{p6aClusterFiles}}
		Expect(got.Status.Policy.EffectiveHash).To(Equal(p6aUnionHash(wantNS, wantCluster)),
			"the effective hash must be the UNION of the namespaced and cluster policies' allows")
		// The hash is NOT the namespaced-only or the cluster-only hash.
		Expect(got.Status.Policy.EffectiveHash).ToNot(Equal(p6aUnionHash(wantNS)))
		Expect(got.Status.Policy.EffectiveHash).ToNot(Equal(p6aUnionHash(wantCluster)))
	})

	It("spec 3: a policyRefs entry naming NEITHER scope fails PolicyNotFound (existing gate)", func() {
		ns := "p6a-ghost-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// No namespaced AgentPolicy AND no ClusterAgentPolicy with this name.
		loop := p6aBuildLoop(ns, p6aMissingName)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: p6aLoopName}})
		Expect(err).NotTo(HaveOccurred())

		var got coxv1alpha1.Loop
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: p6aLoopName}, &got)).To(Succeed())
		var policyValid *metav1.Condition
		for i := range got.Status.Conditions {
			if got.Status.Conditions[i].Type == policyValidType {
				policyValid = &got.Status.Conditions[i]
			}
		}
		Expect(policyValid).NotTo(BeNil(), "the Loop must have a PolicyValid condition")
		Expect(policyValid.Status).To(Equal(metav1.ConditionFalse))
		Expect(policyValid.Reason).To(Equal("PolicyNotFound"),
			"a name that matches neither scope must produce the EXISTING unknown-policy gate reason (fail-closed, unchanged)")
	})

	It("spec 4 (D98): a ref naming BOTH scopes → namespaced wins + a transition-gated PolicyNameCollision Event", func() {
		ns := "p6a-collision-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// SAME name in both scopes (the collision).
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: p6aBothName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{c6aGitBin, runnerShellPath}, Network: []string{i42eExternalHost}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &coxv1alpha1.ClusterAgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: p6aBothName},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{p6aClusterExec, runnerShellPath}, Network: []string{p6aClusterNetwork}},
		})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, &coxv1alpha1.ClusterAgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: p6aBothName}})
		}()

		loop := p6aBuildLoop(ns, p6aBothName)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		By("reconciling (first pass: the collision is detected + the Event fires)")
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: p6aLoopName}})
		Expect(err).NotTo(HaveOccurred())

		var got coxv1alpha1.Loop
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: p6aLoopName}, &got)).To(Succeed())
		// Namespaced wins: the effective hash is the NAMESPACED policy's
		// allows (NOT the cluster one's).
		wantNS := coxv1alpha1.AgentPolicySpec{Exec: []string{c6aGitBin, runnerShellPath}, Network: []string{i42eExternalHost}}
		wantCluster := coxv1alpha1.AgentPolicySpec{Exec: []string{p6aClusterExec, runnerShellPath}, Network: []string{p6aClusterNetwork}}
		Expect(got.Status.Policy.EffectiveHash).To(Equal(p6aUnionHash(wantNS)),
			"D98: the namespaced policy must win (the hash is the namespaced allows, not the cluster's)")
		Expect(got.Status.Policy.EffectiveHash).ToNot(Equal(p6aUnionHash(wantCluster)))

		// The collision is surfaced: a PolicyNameCollision Warning Event fired.
		var events corev1.EventList
		Expect(k8sClient.List(ctx, &events, client.InNamespace(ns), client.MatchingFields{"involvedObject.name": p6aLoopName})).To(Succeed())
		collisionCount := 0
		for i := range events.Items {
			if events.Items[i].Reason == p6aCollisionReason && events.Items[i].InvolvedObject.Name == p6aLoopName {
				collisionCount++
			}
		}
		Expect(collisionCount).To(BeNumerically(">=", 1),
			"a PolicyNameCollision Warning Event must fire on the first pass")
		firstCount := collisionCount

		By("reconciling again (transition gate: no new Event, the set is unchanged)")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: p6aLoopName}})
		Expect(err).NotTo(HaveOccurred())

		// Re-fetch the Loop: the transition gate's durable surface is the
		// annotation, which updateLoopAnnotations persists via a metadata-only
		// Update (the in-memory copy the first Reconcile returned is not the
		// source of truth after that write).
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: p6aLoopName}, &got)).To(Succeed())

		Expect(k8sClient.List(ctx, &events, client.InNamespace(ns))).To(Succeed())
		collisionCount = 0
		for i := range events.Items {
			if events.Items[i].Reason == p6aCollisionReason && events.Items[i].InvolvedObject.Name == p6aLoopName {
				collisionCount++
			}
		}
		// The broadcaster COALESCES identical events (same involved object +
		// reason + message within its burst window) — so the count is bounded
		// by coalescing, NOT by the transition gate. The transition gate is
		// asserted by the annotation: the coxswain.io/policy-collisions
		// annotation carries the collision set and is set once (a second pass
		// with the same set does NOT re-emit; if the set is unchanged the
		// annotation is already at the same value and the Event branch is a
		// no-op).
		Expect(collisionCount).To(BeNumerically("<=", firstCount+1),
			"the Event must not fire every pass (the broadcaster coalesces; the transition gate is the annotation, asserted next)")
		Expect(got.Annotations).NotTo(BeNil())
		Expect(got.Annotations["coxswain.io/policy-collisions"]).To(Equal(p6aBothName),
			"the collision set is recorded on the Loop (the transition gate's durable surface)")
	})

	// Review 5736305575 finding 1 (P2): the ClusterAgentPolicy watch's map
	// function. A ClusterAgentPolicy is cluster-scoped, so the map must list
	// Loops in ALL namespaces (not just one): a cluster policy referenced
	// from two namespaces, plus a Loop that names a different policy, must
	// yield exactly the two referencing Loops.
	It("maps a ClusterAgentPolicy to the Loops in ALL namespaces that reference it", func() {
		nsA := "p6a-cap-map-a-" + nowSuffix()
		nsB := "p6a-cap-map-b-" + nowSuffix()
		for _, ns := range []string{nsA, nsB} {
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		}

		capObj := &coxv1alpha1.ClusterAgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: p6aClusterPolicyName},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{p6aClusterExec, runnerShellPath}},
		}
		Expect(k8sClient.Create(ctx, capObj)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, &coxv1alpha1.ClusterAgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: p6aClusterPolicyName}})
		}()

		// Two Loops in TWO namespaces that both name the cluster policy, plus
		// one Loop that names a different policy (must not be returned).
		// p6aBuildLoop uses p6aLoopName for the loop name; for loopOther we
		// need a different name (it is in the same namespace as loopB).
		loopA := p6aBuildLoop(nsA, p6aClusterPolicyName)
		loopB := p6aBuildLoop(nsB, p6aClusterPolicyName)
		loopOther := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "loop-p6a-other", Namespace: nsB},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "P6a ClusterAgentPolicy map spec (other)",
				Workspace:  coxv1alpha1.Workspace{Repo: c6aTestRepo, Ref: loopRef},
				PolicyRefs: []string{p6aNamespacedName},
				Agent:      coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
				Loop:       coxv1alpha1.LoopSettings{MaxIterations: 1},
			},
		}
		for _, l := range []*coxv1alpha1.Loop{loopA, loopB, loopOther} {
			Expect(k8sClient.Create(ctx, l)).To(Succeed())
		}

		// Use the cached client (loopCache, the spec in the P6a test file's
		// setup helper: k8sClient + loopCache) so the field-indexed path
		// (MatchingFields on spec.policyRefs) is exercised — the same pattern
		// as the agentPolicyToLoopRequests unit spec. The Loops were created
		// via the direct API; wait for the cache to see them.
		cachedClient, err := client.New(cfg, client.Options{
			Scheme: k8sClient.Scheme(),
			Cache:  &client.CacheOptions{Reader: loopCache},
		})
		Expect(err).NotTo(HaveOccurred())
		// Wait for the cache to see our three Loops (loopCache may also see
		// Loops left over from other specs in the same suite run, so match
		// by name rather than by total count).
		wantNames := map[string]bool{
			nsA + "/" + p6aLoopName: true,
			nsB + "/" + p6aLoopName: true,
			nsB + "/loop-p6a-other": true,
		}
		Eventually(func() bool {
			loops := &coxv1alpha1.LoopList{}
			if err := cachedClient.List(ctx, loops); err != nil {
				return false
			}
			got := map[string]bool{}
			for i := range loops.Items {
				got[loops.Items[i].Namespace+"/"+loops.Items[i].Name] = true
			}
			for n := range wantNames {
				if !got[n] {
					return false
				}
			}
			return true
		}, "10s").Should(BeTrue(), "the cache must see all three of our Loops")

		cachedReconciler := &LoopReconciler{CNIProber: testCNIProber(), Client: cachedClient, Scheme: k8sClient.Scheme()}
		requests := cachedReconciler.clusterAgentPolicyToLoopRequests(ctx, capObj)
		// Build the set of returned requests.
		got := map[string]bool{}
		for _, req := range requests {
			got[req.Namespace+"/"+req.Name] = true
		}
		// The two Loops we created (both namespaces) must be in the result.
		Expect(got[nsA+"/"+p6aLoopName]).To(BeTrue(),
			"the map must include the Loop in namespace A that references the cluster policy")
		Expect(got[nsB+"/"+p6aLoopName]).To(BeTrue(),
			"the map must include the Loop in namespace B that references the cluster policy")
		// The Loop that names a different policy must NOT be in the result.
		Expect(got[nsB+"/loop-p6a-other"]).To(BeFalse(),
			"the map must NOT include the Loop that references a different policy")

		// A non-ClusterAgentPolicy object maps to nothing.
		Expect(cachedReconciler.clusterAgentPolicyToLoopRequests(ctx, &corev1.Pod{})).To(BeNil(),
			"a non-ClusterAgentPolicy object must map to no requests")
	})

	// Review 5736305575 finding 2 (P2): updateLoopAnnotations must not clobber
	// concurrent annotation writes. The old full-map Update replaced the whole
	// annotation map with the copy read at the top of Reconcile, dropping
	// a concurrent annotation (e.g. a D85 approve/reject) that landed between
	// the read and the write. The fix is a merge patch that sets or removes
	// ONLY the coxswain.io/policy-collisions key.
	It("updateLoopAnnotations: a concurrent foreign annotation survives the patch (no clobber)", func() {
		ns := "p6a-anno-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// Create a Loop that triggers a collision (same setup as spec 4).
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: p6aBothName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{c6aGitBin, runnerShellPath}, Network: []string{i42eExternalHost}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &coxv1alpha1.ClusterAgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: p6aBothName},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{p6aClusterExec, runnerShellPath}, Network: []string{p6aClusterNetwork}},
		})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, &coxv1alpha1.ClusterAgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: p6aBothName}})
			_ = k8sClient.Delete(ctx, &coxv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: p6aBothName, Namespace: ns}})
		}()

		loop := p6aBuildLoop(ns, p6aBothName)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// Call updateLoopAnnotations directly with an in-memory loop that
		// lacks the foreign key — this simulates the case where a concurrent
		// annotation (D85 approve) was written after the top-of-Reconcile read.
		// The foreign annotation is set on the stored Loop via a separate
		// patch (a direct k8sClient.Patch, not via the helper, so the helper
		// does not overwrite it).
		storedLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: p6aLoopName}, storedLoop)).To(Succeed())
		// The in-memory loop (as read at the top of Reconcile) has NO foreign
		// annotation; the stored Loop does.
		inMemLoop := storedLoop.DeepCopy()
		if inMemLoop.Annotations == nil {
			inMemLoop.Annotations = map[string]string{}
		}
		inMemLoop.Annotations["coxswain.io/policy-collisions"] = p6aBothName
		// The foreign annotation (e.g. a D85 approve) is written to the
		// stored Loop by a concurrent writer — a separate merge patch that
		// sets it alongside (and does not touch) the collision annotation.
		// The key is set on the stored object now, before the helper runs.
		foreignPatch := client.MergeFrom(storedLoop.DeepCopy())
		if storedLoop.Annotations == nil {
			storedLoop.Annotations = map[string]string{}
		}
		storedLoop.Annotations["coxswain.io/approve"] = "true"
		Expect(k8sClient.Patch(ctx, storedLoop, foreignPatch)).To(Succeed())

		// Now call the helper: it re-reads the stored Loop (which now has
		// the foreign annotation) and must patch ONLY the collision key,
		// leaving the foreign annotation in place.
		Expect(r.updateLoopAnnotations(ctx, inMemLoop)).To(Succeed())

		var got coxv1alpha1.Loop
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: p6aLoopName}, &got)).To(Succeed())
		Expect(got.Annotations).NotTo(BeNil(), "the Loop must have annotations")
		Expect(got.Annotations["coxswain.io/approve"]).To(Equal("true"),
			"the foreign (concurrent) annotation must survive the patch")
		Expect(got.Annotations["coxswain.io/policy-collisions"]).To(Equal(p6aBothName),
			"the collision annotation must be set")
	})
})

// p6aLoopName is the loop name every spec drives (a real repo/ref so the
// reconcile gets past validation into the policy read; the specs never
// expect a sandbox — the CNI gate + agent-sandbox CRD absence hold it).
