// C2 + D101 (Phase 6, PLAN-PHASE6.md C2, owner decision 2026-11-01): the
// operator's Loop node-placement flags (--loop-node-selector /
// --loop-tolerations) pin EVERY operator-owned Loop workload to the node
// pool — the sandbox, the model/egress/tool proxy pods and the verify,
// deliver and memory-writer Jobs. The specs here were written RED first
// (against main, where none of the objects carry any placement field) and
// must pass once the flags are applied.
//
// F93b (defect 2 in .samples/f93/F93.md): the Sandbox template's placement
// must be reconciled from the CURRENT flags every pass — including CLEARING
// it when the flags are empty (the F93 defect: a stale BPF node affinity
// survived every helm revision that dropped --allow-unenforced, because the
// mutate only ever SET the affinity, never cleared it). The same-Loop specs
// below flip the reconciler's flags between reconciles of the SAME Loop and
// assert the template follows (the I42c rule).
//
// Jobs are immutable and short-lived: a flag change re-places NEW Jobs only
// (documented in OPERATOR.md). Proxy pods are recreated on placement drift
// by the existing spec-hash delete+recreate (the hash covers the full
// desired spec, so a placement change changes the hash).

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/engine"
	"github.com/papawattu/coxswain/internal/policy"
)

var _ = Describe("C2: operator Loop node placement (flags pin every Loop workload)", func() {
	var (
		ctx = context.Background()
		ns  string
	)

	const (
		c2NSKey    = "pool"
		c2NSValA   = "loops"
		c2NSValB   = "loops2"
		c2TaintKey = "coxswain.io/pool"
		c2TaintVal = "loops"
	)

	c2SelectorA := map[string]string{c2NSKey: c2NSValA}
	c2SelectorB := map[string]string{c2NSKey: c2NSValB}
	c2Toleration := corev1.Toleration{
		Key: c2TaintKey, Operator: corev1.TolerationOpEqual,
		Value: c2TaintVal, Effect: corev1.TaintEffectNoSchedule,
	}

	BeforeEach(func() {
		ns = "c2-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	AfterEach(func() {
		_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	// c2Reconciler builds the spec's reconciler: a wired CNI prober (CNI gate
	// passes — testCNIProber's cached CNIEnforced result, loop_f51_prober_
	// helper_test.go), no Enforcer + AllowUnenforced (the D30 gate passes on
	// the flag) and the C2 placement fields.
	c2Reconciler := func(selector map[string]string, tols []corev1.Toleration) *LoopReconciler {
		return &LoopReconciler{
			CNIProber:        testCNIProber(),
			Client:           k8sClient,
			Scheme:           k8sClient.Scheme(),
			AllowUnenforced:  true,
			AllowUnenforcedNetwork: true,
			LoopNodeSelector: selector,
			LoopTolerations:  tols,
		}
	}

	reconcile := func(r *LoopReconciler, name string) {
		_, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	// c2Loop creates a sandbox-only Loop (in-cluster repo — F55: no egress
	// proxy for the workspace init clone; no endpointSecretRef — no model
	// proxy hold; no policy refs — no tool proxies), the shape the F48 specs
	// use.
	c2Loop := func(name string) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		return loop
	}

	c2GetSandbox := func(name string) *sandboxv1beta1.Sandbox {
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)).To(Succeed())
		return sb
	}

	// carryPlacement asserts the given spec carries EXACTLY the given
	// placement: the nodeSelector keys and tolerations present, and nothing
	// else (an unexpected key or toleration is a fail — the flags are the
	// only source; an empty flag set means empty fields).
	carryPlacement := func(spec corev1.PodSpec, wantSel map[string]string, wantTols []corev1.Toleration) {
		Expect(spec.NodeSelector).To(Equal(wantSel),
			"the pod template's nodeSelector must be exactly the flag's (empty flags = empty selector)")
		Expect(spec.Tolerations).To(Equal(wantTols),
			"the pod template's tolerations must be exactly the flag's (empty flags = none)")
	}

	// c2ModelLoop creates a Loop with an endpointSecretRef (the model proxy
	// exists: the operator-owned bare proxy pod with the spec-hash
	// delete+recreate contract).
	c2ModelLoop := func(name string) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: d33ModelCredsSecret,
					ModelEndpoint:     d34ModelEndpoint,
				},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		return loop
	}

	// c2GetLoop re-reads the Loop (fresh resourceVersion for the status
	// writes).
	c2GetLoop := func(name string) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		return loop
	}

	It("flags set: the sandbox, model proxy pod and the verify, deliver and memory-writer Jobs carry the placement; with the flags unset, none carry any", func() {
		name := "c2p"
		r := c2Reconciler(c2SelectorA, []corev1.Toleration{c2Toleration})
		c2ModelLoop(name)
		reconcile(r, name)

		By("the sandbox template")
		carryPlacement(c2GetSandbox(name).Spec.PodTemplate.Spec, c2SelectorA, []corev1.Toleration{c2Toleration})

		By("the model proxy pod")
		p := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-proxy", Namespace: ns}, p)).To(Succeed())
		carryPlacement(p.Spec, c2SelectorA, []corev1.Toleration{c2Toleration})

		By("the verify Job (phase Verifying, pin + baseCommit set)")
		loop := c2GetLoop(name)
		loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		loop.Status.BaseCommit = "a1b2c3"
		loop.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: "c3d4e5"}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		reconcile(r, name)
		vj := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-verify-1", Namespace: ns}, vj)).To(Succeed())
		carryPlacement(vj.Spec.Template.Spec, c2SelectorA, []corev1.Toleration{c2Toleration})

		By("the deliver Job (phase Succeeded, PullRequest mode, pin unchanged so the idempotency skip does not apply)")
		loop = c2GetLoop(name)
		loop.Spec.Delivery = &coxv1alpha1.DeliveryConfig{Mode: coxv1alpha1.DeliveryModePullRequest}
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())
		loop = c2GetLoop(name)
		loop.Status.Phase = coxv1alpha1.LoopPhaseSucceeded
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		reconcile(r, name)
		dj := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-deliver", Namespace: ns}, dj)).To(Succeed())
		carryPlacement(dj.Spec.Template.Spec, c2SelectorA, []corev1.Toleration{c2Toleration})

		By("the memory-writer Job (a qualifying terminal outcome: Succeeded + lessonsAvailable)")
		loop = c2GetLoop(name)
		loop.Status.Progress = &coxv1alpha1.ProgressStatus{LessonsAvailable: true}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		reconcile(r, name)
		mj := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-memory-write-1", Namespace: ns}, mj)).To(Succeed())
		carryPlacement(mj.Spec.Template.Spec, c2SelectorA, []corev1.Toleration{c2Toleration})

		By("flags unset: a fresh Loop's objects carry NO placement (today's behaviour)")
		rOff := c2Reconciler(nil, nil)
		c2ModelLoop("c2poff")
		reconcile(rOff, "c2poff")
		carryPlacement(c2GetSandbox("c2poff").Spec.PodTemplate.Spec, map[string]string{}, []corev1.Toleration{})
		p2 := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c2poff-proxy", Namespace: ns}, p2)).To(Succeed())
		carryPlacement(p2.Spec, map[string]string{}, []corev1.Toleration{})
		loop2 := c2GetLoop("c2poff")
		loop2.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		loop2.Status.BaseCommit = "a1b2c3"
		loop2.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: "c3d4e5"}
		Expect(k8sClient.Status().Update(ctx, loop2)).To(Succeed())
		reconcile(rOff, "c2poff")
		vj2 := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c2poff-verify-1", Namespace: ns}, vj2)).To(Succeed())
		carryPlacement(vj2.Spec.Template.Spec, map[string]string{}, []corev1.Toleration{})
	})

	It("F93b: changing the flags re-places the SAME Loop's sandbox template; clearing the flags clears the placement", func() {
		name := "c2f93"
		r := c2Reconciler(c2SelectorA, []corev1.Toleration{c2Toleration})
		c2Loop(name)
		reconcile(r, name)
		carryPlacement(c2GetSandbox(name).Spec.PodTemplate.Spec, c2SelectorA, []corev1.Toleration{c2Toleration})

		By("the flags change (the operator restarted with new flags): the SAME Loop's template follows")
		r2 := c2Reconciler(c2SelectorB, nil)
		reconcile(r2, name)
		carryPlacement(c2GetSandbox(name).Spec.PodTemplate.Spec, c2SelectorB, []corev1.Toleration{})

		By("the flags are cleared: the placement is CLEARED (the F93 defect: the mutate froze the template at creation — it only ever SET fields, never cleared them)")
		r3 := c2Reconciler(nil, nil)
		reconcile(r3, name)
		carryPlacement(c2GetSandbox(name).Spec.PodTemplate.Spec, map[string]string{}, []corev1.Toleration{})
	})

	It("F93b (the BPF affinity itself): an enforcing enforcer SETS the affinity; the allow-unenforced re-reconcile REMOVES it (the same-Loop F93 defect)", func() {
		name := "c2bpf"
		// c2BPFEnforcer returns the D52 BPF affinity (the enforcing
		// KubeArmorEnforcer's shape, kubearmor_enforcer.go:481) or nil (the
		// --allow-unenforced behaviour: BPFNodeAffinity returns nil). The
		// spec drives the seam applyBPFNodeAffinity consumes, so the CLEAR
		// contract (nil removes the stale affinity) is pinned without a real
		// KubeArmor install.
		aff := &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{Key: "kubearmor.io/enforcer", Operator: corev1.NodeSelectorOpIn, Values: []string{"bpf"}},
						},
					}},
				},
			},
		}
		enf := c2BPFEnforcer{aff: aff}
		unenf := c2BPFEnforcer{aff: nil}

		c2Loop(name)

		By("enforcing: the affinity lands on the sandbox template")
		rEnf := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient,
			Scheme: k8sClient.Scheme(), Enforcer: enf}
		reconcile(rEnf, name)
		sb := c2GetSandbox(name)
		Expect(sb.Spec.PodTemplate.Spec.Affinity).To(Equal(aff),
			"the enforcing BPF affinity must be on the sandbox template")

		By("the operator restarts with --allow-unenforced (BPFNodeAffinity nil): the SAME Loop's template LOSES the affinity — F93 defect 2 (the stale affinity survived every rev-31-onward reconcile)")
		rUnenf := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient,
			Scheme: k8sClient.Scheme(), AllowUnenforced: true, Enforcer: unenf}
		reconcile(rUnenf, name)
		sb = c2GetSandbox(name)
		Expect(sb.Spec.PodTemplate.Spec.Affinity).To(BeNil(),
			"the BPF affinity must be CLEARED when the enforcer stops returning one (the mutate sets the placement from the CURRENT state every pass, including clearing)")
	})

	It("proxy pods are recreated on a placement flag change (bare pods are immutable; the spec hash covers the placement)", func() {
		name := "c2drift"
		r := c2Reconciler(c2SelectorA, []corev1.Toleration{c2Toleration})
		c2ModelLoop(name)
		reconcile(r, name)
		p1 := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-proxy", Namespace: ns}, p1)).To(Succeed())
		carryPlacement(p1.Spec, c2SelectorA, []corev1.Toleration{c2Toleration})
		uid1 := p1.UID

		By("the operator restarts with different flags: the spec-hash drift runs the delete+recreate")
		r2 := c2Reconciler(c2SelectorB, nil)
		reconcile(r2, name)
		// The stale-hash pass DELETES the old pod (its spec is immutable —
		// a bare pod cannot be Updated). The recreation needs the pod gone:
		// the real cluster's Owns(Pod) watch requeues the Loop after the
		// delete lands; envtest drives the next pass explicitly.
		pg := &corev1.Pod{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: name + "-proxy", Namespace: ns}, pg)).To(
			MatchError(apierrors.IsNotFound))
		reconcile(r2, name)
		p2 := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-proxy", Namespace: ns}, p2)).To(Succeed())
		Expect(p2.UID).NotTo(Equal(uid1),
			"the recreated pod is a NEW pod (the bare pod's spec is immutable — the flag change could only be applied by recreation)")
		carryPlacement(p2.Spec, c2SelectorB, []corev1.Toleration{})
	})

	It("Jobs are NOT deleted+recreated on a flag change (documented in OPERATOR.md: new Jobs only)", func() {
		// The verify Job's immutability contract (B3b): an existing Job is
		// left untouched on re-reconcile. A flag change between reconciles
		// must not delete+recreate it (Jobs are short-lived; OPERATOR.md
		// documents that a flag change re-places NEW Jobs only).
		name := "c2job"
		r := c2Reconciler(c2SelectorA, []corev1.Toleration{c2Toleration})
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{Goal: loopGoal, Workspace: testWorkspace()},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		loop = c2GetLoop(name)
		loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		loop.Status.BaseCommit = "a1b2c3"
		loop.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: "c3d4e5"}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		reconcile(r, name)
		j1 := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-verify-1", Namespace: ns}, j1)).To(Succeed())

		By("the flags change and the SAME Loop re-reconciles: the existing Job is untouched (still the old placement)")
		r2 := c2Reconciler(c2SelectorB, nil)
		reconcile(r2, name)
		j2 := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-verify-1", Namespace: ns}, j2)).To(Succeed())
		Expect(j2.UID).To(Equal(j1.UID),
			"the running verify Job must not be deleted+recreated on a flag change (it is short-lived; new Jobs get the new placement)")
		carryPlacement(j2.Spec.Template.Spec, c2SelectorA, []corev1.Toleration{c2Toleration})
	})
})

// c2BPFEnforcer is the F93b spec's Enforcer seam: it returns the configured
// BPF affinity (the enforcing enforcer's shape) or nil (the
// --allow-unenforced behaviour — KubeArmorEnforcer.BPFNodeAffinity returns
// nil under the hatch). applyBPFNodeAffinity consumes this seam, so the spec
// pins the CLEAR contract (a nil affinity removes the stale one) without a
// real KubeArmor install.
type c2BPFEnforcer struct {
	aff *corev1.Affinity
}

func (c c2BPFEnforcer) Apply(_ context.Context, _ *coxv1alpha1.Loop, _ policy.EffectivePolicy) error {
	return nil
}
func (c c2BPFEnforcer) Enforcing(_ context.Context, _ *coxv1alpha1.Loop) (bool, string) {
	return true, ""
}
func (c c2BPFEnforcer) KaptSpecHash(_ *coxv1alpha1.Loop, _ policy.EffectivePolicy) string {
	return ""
}
func (c c2BPFEnforcer) BPFNodeAffinity() *corev1.Affinity {
	return c.aff
}

var _ engine.Enforcer = c2BPFEnforcer{}
