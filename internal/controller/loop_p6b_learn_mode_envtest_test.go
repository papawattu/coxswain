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

// P6b (learn mode / auditOnly): an enforcing engine with an effective policy
// where every referenced policy is auditOnly records (spec.action Audit) but
// does not block. The D30 gate is UNCHANGED (with a working engine the sandbox
// still runs); what changes is the condition: PolicyEnforced=False, reason
// AuditOnly. A mixed Loop (D108) enforces the union and records the ignored
// auditOnly names in status.policy.auditOnlyIgnored with one Warning Event per
// set change.
package controller

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/engine"
	"github.com/papawattu/coxswain/internal/policy"
)

var _ = Describe("P6b: learn mode (auditOnly) sets PolicyEnforced=False AuditOnly", func() {
	ctx := context.Background()

	p6bNs := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	p6bPolicy := func(ns, name string, auditOnly bool) {
		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.AgentPolicySpec{
				Exec:      []string{"/usr/bin/git"},
				AuditOnly: auditOnly,
			},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed())
	}

	p6bLoop := func(ns string, refs []string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "l1", Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       loopGoal,
				Workspace:  testWorkspace(),
				Verify:     coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
				PolicyRefs: refs,
			},
		}
	}

	p6bReconciler := func(enforcing bool, reason string, recorder *record.FakeRecorder) *LoopReconciler {
		return &LoopReconciler{
			CNIProber: testCNIProber(),
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  recorder,
			Enforcer:  &fakeEnforcer{enforcing: enforcing, reason: reason},
			ApplyEnforcementWritesFn: func(_ context.Context, _ *coxv1alpha1.Loop, _ policy.EffectivePolicy) error {
				return nil
			},
		}
	}

	p6bDrain := func(recorder *record.FakeRecorder) []string {
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

	It("a1: every policy auditOnly with an enforcing engine => PolicyEnforced=False/AuditOnly", func() {
		ns := p6bNs("p6b-learn")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		p6bPolicy(ns, "learn", true)
		loop := p6bLoop(ns, []string{"learn"})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		recorder := record.NewFakeRecorder(64)
		r := p6bReconciler(true, "", recorder)
		_, err := r.applyEffectivePolicyAndConditions(ctx, loop)
		Expect(err).NotTo(HaveOccurred())

		cond := policyEnforcedCondition(loop.Status.Conditions)
		Expect(cond).NotTo(BeNil(), "the Loop must carry a PolicyEnforced condition")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("AuditOnly"))
		Expect(cond.Message).To(ContainSubstring("learn mode"))
	})

	It("a2: the same fixture with auditOnly=false => PolicyEnforced=True/Enforcing (the flag alone flips it)", func() {
		ns := p6bNs("p6b-enf")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		p6bPolicy(ns, "learn", false)
		loop := p6bLoop(ns, []string{"learn"})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		recorder := record.NewFakeRecorder(64)
		r := p6bReconciler(true, "", recorder)
		_, err := r.applyEffectivePolicyAndConditions(ctx, loop)
		Expect(err).NotTo(HaveOccurred())

		cond := policyEnforcedCondition(loop.Status.Conditions)
		Expect(cond).NotTo(BeNil(), "the Loop must carry a PolicyEnforced condition")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("Enforcing"))
	})

	It("a3: a mixed Loop (D108) enforces the union and records the ignored auditOnly name once per set change", func() {
		ns := p6bNs("p6b-mixed")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		p6bPolicy(ns, "enf", false)
		p6bPolicy(ns, "learn", true)
		loop := p6bLoop(ns, []string{"enf", "learn"})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		recorder := record.NewFakeRecorder(64)
		r := p6bReconciler(true, "", recorder)
		_, err := r.applyEffectivePolicyAndConditions(ctx, loop)
		Expect(err).NotTo(HaveOccurred())

		cond := policyEnforcedCondition(loop.Status.Conditions)
		Expect(cond).NotTo(BeNil(), "the Loop must carry a PolicyEnforced condition")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("Enforcing"))

		Expect(loop.Status.Policy).NotTo(BeNil(), "status.policy must carry the D108 record")
		Expect(loop.Status.Policy.AuditOnlyIgnored).To(Equal([]string{"learn"}))

		By("exactly one AuditOnlyIgnored Warning Event")
		ignored := func(events []string) []string {
			out := []string{}
			for _, e := range events {
				if strings.Contains(e, "AuditOnlyIgnored") {
					out = append(out, e)
				}
			}
			return out
		}
		events := ignored(p6bDrain(recorder))
		Expect(events).To(HaveLen(1), "expected exactly one AuditOnlyIgnored Event, got %v", events)
		Expect(events[0]).To(ContainSubstring(corev1.EventTypeWarning))

		By("a second pass on the same in-memory Loop emits no further Event (set changed, not every pass)")
		_, err = r.applyEffectivePolicyAndConditions(ctx, loop)
		Expect(err).NotTo(HaveOccurred())
		Expect(ignored(p6bDrain(recorder))).To(BeEmpty(), "the Event is gated on the set change, not per pass")
	})

	It("a4: learn mode needs a working engine — an unenforcing engine reports its own reason, not AuditOnly", func() {
		ns := p6bNs("p6b-noengine")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		p6bPolicy(ns, "learn", true)
		loop := p6bLoop(ns, []string{"learn"})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		recorder := record.NewFakeRecorder(64)
		r := p6bReconciler(false, engine.ReasonNodeNotEnforcing, recorder)
		_, err := r.applyEffectivePolicyAndConditions(ctx, loop)
		Expect(err).NotTo(HaveOccurred())

		cond := policyEnforcedCondition(loop.Status.Conditions)
		Expect(cond).NotTo(BeNil(), "the Loop must carry a PolicyEnforced condition")
		Expect(cond.Reason).To(Equal(engine.ReasonNodeNotEnforcing))
		Expect(cond.Reason).NotTo(Equal("AuditOnly"), "learn mode is not a substitute for a working engine")
	})
})
