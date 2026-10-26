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

		By("reconciling: the CRD-absent shape must NOT return an error")
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred(),
			"a missing KubeArmorPolicy CRD must not abort the reconcile (F92: that left status: {})")

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
		Expect(f92Get(ns, name).Status.Phase).NotTo(Equal(coxv1alpha1.LoopPhaseFailed),
			"D30 holds the Loop, it never fails it")
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
