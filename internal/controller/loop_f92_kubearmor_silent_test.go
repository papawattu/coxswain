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

// F92 (R41 I151): a missing KubeArmorPolicy CRD used to wedge every Loop with an
// EMPTY status. The enforcer's loud error ("KubeArmor CRD not installed (and
// --allow-unenforced is not set)") was returned from applyEffectivePolicyAndConditions
// before the condition half, and Reconcile returned it before any status write, so a
// fresh Loop kept status: {} — no phase, no condition, no Event — for ever. The
// failure was visible only in the operator log, which AGENTS R26 I90 forbids ("a
// fallback must surface its failure before it degrades").
//
// The fix tolerates exactly that shape (meta.IsNoMatchError) and lets the existing
// condition half run: PolicyEnforced=False/EngineUnavailable, the D30 gate holds the
// sandbox Suspended, and a transition-gated Warning Event names the cause. Every
// other error (Forbidden, an API blip) still returns and retries — that is what S2
// pins, so the tolerance cannot widen into "ignore every enforcement write failure".
package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/engine"
	"github.com/papawattu/coxswain/internal/policy"
)

var _ = Describe("F92: a missing KubeArmorPolicy CRD is surfaced on the Loop, not only in the log", func() {
	ctx := context.Background()

	// f92NoMatch is the error the enforcer returns when the CRD is absent
	// (internal/engine/kubearmor_enforcer.go: NoKindMatchError from the client's
	// RESTMapper, wrapped with %w). meta.IsNoMatchError must see through the wrap.
	f92NoMatch := func() error {
		return fmt.Errorf("get KubeArmorPolicy coxswain-l1: %w", &meta.NoKindMatchError{
			GroupKind: schema.GroupKind{Group: "security.kubearmor.com", Kind: "KubeArmorPolicy"},
		})
	}

	f92Ns := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	f92Loop := func(ns, name string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
			},
		}
	}

	f92Get := func(ns, name string) *coxv1alpha1.Loop {
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, got)).To(Succeed())
		return got
	}

	f92Drain := func(recorder *record.FakeRecorder) []string {
		out := []string{}
		for {
			select {
			case e := <-recorder.Events:
				out = append(out, e)
			default:
				return out
			}
		}
	}

	It("S1: surfaces PolicyEnforced=False/EngineUnavailable, one Warning Event, and holds the sandbox Suspended — without returning an error", func() {
		ns := f92Ns("f92-nomatch")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{
			CNIProber:       testCNIProber(),
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			apiReader:       k8sClient,
			Recorder:        recorder,
			Enforcer:        &fakeEnforcer{enforcing: false, reason: engine.ReasonEngineUnavailable},
			AllowUnenforced: false,
			ApplyEnforcementWritesFn: func(_ context.Context, _ *coxv1alpha1.Loop, _ policy.EffectivePolicy) error {
				return f92NoMatch()
			},
		}

		name := "l1"
		Expect(k8sClient.Create(ctx, f92Loop(ns, name))).To(Succeed())

		By("reconciling: the CRD-absent shape must NOT return an error, and must requeue")
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred(),
			"a missing KubeArmorPolicy CRD must not abort the reconcile (F92: that left status: {})")
		Expect(res.RequeueAfter).To(BeNumerically(">", 0),
			"a held Loop must be re-checked (F92 P2-2), not left waiting for an unrelated event")
		// This Loop is still clone-pending, so the window here is the 5s work
		// requeue; the exact F92 window (Equal(kaptCRDMissingRequeue)) is pinned
		// by S5, which seeds baseCommit to leave the F92 window as the only one.
		Expect(res.RequeueAfter).To(BeNumerically("<", kaptCRDMissingRequeue),
			"a clone-pending Loop takes the 5s work requeue, which is shorter than the F92 window")

		By("the Loop's status must say why (the R26 I90 norm)")
		loop := f92Get(ns, name)
		cond := policyEnforcedCondition(loop.Status.Conditions)
		Expect(cond).NotTo(BeNil(), "the Loop must carry a PolicyEnforced condition (F92: it used to carry none)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(engine.ReasonEngineUnavailable))
		Expect(cond.Message).To(ContainSubstring("KubeArmorPolicy CRD is not installed"))

		By("exactly one Warning Event naming the cause (other, unrelated Events may fire in the same pass)")
		unavailable := func(events []string) []string {
			out := []string{}
			for _, e := range events {
				if strings.Contains(e, PolicyEnforcementUnavailableEventReason) {
					out = append(out, e)
				}
			}
			return out
		}
		events := unavailable(f92Drain(recorder))
		Expect(events).To(HaveLen(1), "expected exactly one PolicyEnforcementUnavailable Event, got %v", events)
		Expect(events[0]).To(ContainSubstring(corev1.EventTypeWarning))
		Expect(events[0]).To(ContainSubstring("KubeArmorPolicy CRD is not installed"))

		By("a second reconcile does not re-announce (the transition gate)")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(unavailable(f92Drain(recorder))).To(BeEmpty(), "the Event is gated on the transition, not per reconcile")

		By("the D30 gate holds the sandbox Suspended (fail-closed)")
		Eventually(func() sandboxv1beta1.SandboxOperatingMode {
			sbx := &sandboxv1beta1.Sandbox{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name + "-sandbox"}, sbx); err != nil {
				return ""
			}
			return sbx.Spec.OperatingMode
		}, "10s").Should(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"the sandbox must be held Suspended while the engine is unavailable (D30)")
		Expect(f92Get(ns, name).Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"the Loop must carry a phase again (I151's 'no phase for ever' half), and D30 holds it rather than failing it")
	})

	It("S3: the CRD is named on the object even when Enforcing() reports a different reason (the kind-coxswain shape)", func() {
		// On a cluster whose nodes carry no BPF-LSM label, Enforcing() returns
		// NodeNotEnforcing — it never reaches the CRD read. The tolerated write
		// error is then the only place that knows the CRD is absent, so the
		// condition message must name it and the Event must still fire (that is
		// the shape the kind run hits; S1 alone would not cover it).
		ns := f92Ns("f92-nodelabel")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{
			CNIProber:       testCNIProber(),
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			apiReader:       k8sClient,
			Recorder:        recorder,
			Enforcer:        &fakeEnforcer{enforcing: false, reason: engine.ReasonNodeNotEnforcing},
			AllowUnenforced: false,
			ApplyEnforcementWritesFn: func(_ context.Context, _ *coxv1alpha1.Loop, _ policy.EffectivePolicy) error {
				return f92NoMatch()
			},
		}

		name := "l3"
		Expect(k8sClient.Create(ctx, f92Loop(ns, name))).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())

		cond := policyEnforcedCondition(f92Get(ns, name).Status.Conditions)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(engine.ReasonNodeNotEnforcing),
			"the observation reason is kept (a missing label is not rewritten as a missing CRD)")
		Expect(cond.Message).To(ContainSubstring("KubeArmorPolicy CRD is not installed"),
			"the message must name the real cause the write path saw")

		var events []string
		for _, e := range f92Drain(recorder) {
			if strings.Contains(e, PolicyEnforcementUnavailableEventReason) {
				events = append(events, e)
			}
		}
		Expect(events).To(HaveLen(1), "the CRD-absent shape is surfaced once, got %v", events)

		By("a second reconcile in THIS shape does not re-announce either (the gate is not reason-specific)")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())
		for _, e := range f92Drain(recorder) {
			Expect(strings.Contains(e, PolicyEnforcementUnavailableEventReason)).To(BeFalse(),
				"the Event is gated on the transition, not per reconcile (got %s)", e)
		}
	})

	It("S4: a Loop held for a missing CRD is re-checked on a bounded window, not left waiting (F92 P2-2)", func() {
		// The Loop is otherwise idle: its spec does not change and nothing it
		// references changes when KubeArmor is installed, so without a requeue
		// it would stay held until an unrelated event. The trailing requeue
		// decision carries the flag the tolerated write error set.
		r := &LoopReconciler{}
		Expect(r.reconcileRequeueAfter(true, false, false, false, false, time.Duration(0), time.Duration(0), true).RequeueAfter).
			To(Equal(kaptCRDMissingRequeue))
		// Positive control: the same call without the flag does not requeue.
		Expect(r.reconcileRequeueAfter(true, false, false, false, false, time.Duration(0), time.Duration(0), false).RequeueAfter).
			To(BeZero())
	})

	It("S5: Reconcile threads the tolerated no-match into the trailing requeue decision (F92 P2-2 wiring)", func() {
		// S4 pins the requeue RULE; this pins the WIRING: that the flag the write
		// path set actually reaches reconcileRequeueAfter. Without this a
		// `return false, nil` in applyEffectivePolicyAndConditions disables the
		// F92 P2-2 fix while S1 (its RequeueAfter only has to be > 0, satisfied by
		// the 5s clone-pending work requeue) and S4 (a direct unit call) both stay
		// green — a mutation that drops the return value was ONLY caught here.
		ns := f92Ns("f92-wiring")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		newReconciler := func(writeErr error) *LoopReconciler {
			r := &LoopReconciler{
				CNIProber:        testCNIProber(),
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				apiReader:        k8sClient,
				Recorder:         record.NewFakeRecorder(64),
				Enforcer:         &fakeEnforcer{enforcing: false, reason: engine.ReasonNodeNotEnforcing},
				AllowUnenforced:  false,
				baseCommitSeeded: true, // no clone-pending 5s work requeue: the only window left is the F92 one
			}
			r.ApplyEnforcementWritesFn = func(_ context.Context, _ *coxv1alpha1.Loop, _ policy.EffectivePolicy) error {
				return writeErr
			}
			return r
		}

		name := "l5"
		Expect(k8sClient.Create(ctx, f92Loop(ns, name))).To(Succeed())
		res, err := newReconciler(f92NoMatch()).Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(kaptCRDMissingRequeue),
			"the tolerated no-match must reach the trailing requeue decision (F92 P2-2)")

		By("positive control: the same shape with a SUCCESSFUL write does not take the F92 window")
		name = "l6"
		Expect(k8sClient.Create(ctx, f92Loop(ns, name))).To(Succeed())
		res, err = newReconciler(nil).Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).NotTo(Equal(kaptCRDMissingRequeue),
			"without a tolerated no-match the F92 window is not taken, so the assertion above is not vacuous (this shape requeues on another window, observed ~5m)")
	})

	It("S6: a terminal Loop in the CRD-absent shape gets no F92 window (the #299 round-2 rule)", func() {
		// The tolerated no-match is set on EVERY pass that hits it, whatever the
		// phase, so a Succeeded/Failed Loop on a cluster whose CRD later goes
		// missing would reconcile every minute for the life of the cluster. A
		// terminal Loop is never held by the engine gate (nothing runs without
		// enforcement), so the window must not apply to it. This is Reconcile-driven
		// on purpose: the S5 lesson is that a helper asserted only through its own
		// unit call does not prove the call site passes what it computes.
		ns := f92Ns("f92-terminal")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		newReconciler := func() *LoopReconciler {
			r := &LoopReconciler{
				CNIProber:        testCNIProber(),
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				apiReader:        k8sClient,
				Recorder:         record.NewFakeRecorder(64),
				Enforcer:         &fakeEnforcer{enforcing: false, reason: engine.ReasonNodeNotEnforcing},
				AllowUnenforced:  false,
				baseCommitSeeded: true, // the F92 window is otherwise the only one
			}
			r.ApplyEnforcementWritesFn = func(_ context.Context, _ *coxv1alpha1.Loop, _ policy.EffectivePolicy) error {
				return f92NoMatch()
			}
			return r
		}

		By("a terminal Loop: no F92 window")
		name := "l7"
		Expect(k8sClient.Create(ctx, f92Loop(ns, name))).To(Succeed())
		seeded := f92Get(ns, name)
		seeded.Status.Phase = coxv1alpha1.LoopPhaseSucceeded
		Expect(k8sClient.Status().Update(ctx, seeded)).To(Succeed())

		res, err := newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).NotTo(Equal(kaptCRDMissingRequeue),
			"a Succeeded Loop must not take the per-minute F92 window")

		By("positive control: the same setup at a working phase does take it")
		name = "l8"
		Expect(k8sClient.Create(ctx, f92Loop(ns, name))).To(Succeed())
		res, err = newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(kaptCRDMissingRequeue),
			"the same shape at Planning does take the F92 window, so the assertion above is not vacuous")

		By("the guard's truth table (a helper asserted only through itself would not pin the call site)")
		Expect(f92HeldRequeue(true, coxv1alpha1.LoopPhaseSucceeded)).To(BeFalse())
		Expect(f92HeldRequeue(true, coxv1alpha1.LoopPhaseFailed)).To(BeFalse())
		Expect(f92HeldRequeue(true, coxv1alpha1.LoopPhasePlanning)).To(BeTrue())
		Expect(f92HeldRequeue(true, coxv1alpha1.LoopPhasePaused)).To(BeTrue(), "Paused is not terminal")
		Expect(f92HeldRequeue(false, coxv1alpha1.LoopPhasePlanning)).To(BeFalse())
	})

	It("S2: every OTHER enforcement write error still returns and retries (the tolerance is exactly the CRD-absent shape)", func() {
		ns := f92Ns("f92-forbidden")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{
			CNIProber:       testCNIProber(),
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			apiReader:       k8sClient,
			Recorder:        recorder,
			Enforcer:        &fakeEnforcer{enforcing: false, reason: engine.ReasonEngineUnavailable},
			AllowUnenforced: false,
		}
		r.ApplyEnforcementWritesFn = func(_ context.Context, _ *coxv1alpha1.Loop, _ policy.EffectivePolicy) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "security.kubearmor.com", Resource: "kubearmorpolicies"}, "coxswain-l1", fmt.Errorf("denied"))
		}

		name := "l2"
		Expect(k8sClient.Create(ctx, f92Loop(ns, name))).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).To(HaveOccurred(),
			"a Forbidden enforcement write must still fail the reconcile (F92 tolerates only the CRD-absent shape)")
		Expect(meta.IsNoMatchError(err)).To(BeFalse())

		By("no EngineUnavailable condition was recorded (the reconcile never reached the condition half)")
		cond := policyEnforcedCondition(f92Get(ns, name).Status.Conditions)
		Expect(cond).To(BeNil(), "an unrelated write failure must not be reported as a missing engine")
		Expect(f92Drain(recorder)).To(BeEmpty())
	})
})
