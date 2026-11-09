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

// D119 step 2: the controller persists a learn-mode Loop's summary (held
// in-memory by the relay since step 1) into an owned ConfigMap, and forgets
// the deleted Loop's relay state on the delete path. These envtest specs
// cover the RECONCILE's half of the seam (the ConfigMap's create/update and
// the delete-path Forget) — the relay's own in-memory summary is specified
// in internal/alerts (D119 step 1).
//
// Mutation m1: remove the learn-mode check in recordLearnSummary → the
// "non-learn-mode Loop gets no ConfigMap" spec fails (the ConfigMap is
// written for an enforcing Loop).
// Mutation m2: skip the Update when the ConfigMap exists → the "the SAME
// ConfigMap is UPDATED to 3 lines" spec fails (the ConfigMap stays at 2
// lines after the second reconcile).
// Mutation m3: count = new instead of max in the merge → the merge unit
// spec (TestLearnSummaryMergeIsUnionByMaxCount) fails.

package controller

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/alerts"
)

var _ = Describe("D119 step 2: the learn summary is persisted to an owned ConfigMap", func() {
	ctx := context.Background()

	l2Ns := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	l2Reconciler := func(audit AuditReader) *LoopReconciler {
		return &LoopReconciler{
			CNIProber: testCNIProber(),
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			apiReader: k8sClient,
			Recorder:  record.NewFakeRecorder(64),
			Audit:     audit,
		}
	}

	l2LearnLoop := func(ns, name string) types.UID {
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())
		// Set the PolicyEnforced condition to False / reason AuditOnly (the
		// learn-mode signal). The suite does not render this condition
		// itself (the real reconciler sets it via
		// applyEffectivePolicyAndConditions, which needs an Enforcer); the
		// spec sets it directly so the gate (isLearnMode) sees learn mode.
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Status.Conditions = []metav1.Condition{
			{
				Type:               string(PolicyEnforcedCondition),
				Status:             metav1.ConditionFalse,
				Reason:             "AuditOnly",
				Message:            "learn mode (P6b)",
				LastTransitionTime: metav1.Now(),
			},
		}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		return loop.UID
	}

	l2GetCM := func(ns, name string) *corev1.ConfigMap {
		cm := &corev1.ConfigMap{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cm); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			Fail("get configmap: " + err.Error())
		}
		return cm
	}

	l2Record := func(i int, ts string) alerts.Record {
		return alerts.Record{
			Time:    ts,
			Loop:    "l2learn",
			Source:  alerts.SourceKubeArmor,
			Action:  "Audit",
			Target:  "/data/file" + strings.Repeat("x", 200) + string(rune('a'+i)),
			Detail:  "operation=File, pod=l2learn-sandbox",
			Count:   1,
			Verdict: "allowed",
		}
	}

	It("creates the ConfigMap on the first learn-mode reconcile and updates it on the second (I43)", func() {
		ns := l2Ns("l2-learn")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "l2learn"
		audit := newF62Audit()
		uid := l2LearnLoop(ns, name)
		r := l2Reconciler(audit)

		// Two learns-relevant records at first.
		audit.SetLearnSummary(uid, []alerts.Record{l2Record(1, "2026-11-08T10:00:00Z"), l2Record(2, "2026-11-08T10:05:00Z")}, 0)
		s5aReconcile(r, ns, name)

		cmName := learnSummaryCMName(name)
		cm := l2GetCM(ns, cmName)
		Expect(cm).NotTo(BeNil(), "the ConfigMap must be created on the first learn-mode reconcile")
		Expect(cm.OwnerReferences).To(HaveLen(1), "the ConfigMap must carry a controller owner ref")
		Expect(cm.OwnerReferences[0].Kind).To(Equal("Loop"))
		Expect(cm.OwnerReferences[0].UID).To(Equal(uid), "the owner ref must name the Loop's UID")
		Expect(cm.Labels).To(HaveKeyWithValue(managedByLabel, partOfCoxswain), "the ConfigMap must carry the operator's managed-by label")
		Expect(cm.Labels).To(HaveKeyWithValue(loopLabelKey, name), "the ConfigMap must carry the per-Loop label")

		body := cm.Data[learnSummaryRecordsKey]
		lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
		Expect(lines).To(HaveLen(2), "the ConfigMap must carry one line per distinct record")
		Expect(cm.Data[learnSummaryDroppedKey]).To(Equal("0"))

		// The SAME Loop, a new record: an update, not a create-only write (I43).
		// Re-set the learn-mode condition (the first reconcile's
		// applyEffectivePolicyAndConditions overwrote it to
		// False/EngineUnavailable — no Enforcer is wired in envtest).
		loop2 := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop2)).To(Succeed())
		loop2.Status.Conditions = []metav1.Condition{{
			Type: string(PolicyEnforcedCondition), Status: metav1.ConditionFalse,
			Reason: "AuditOnly", Message: "learn mode (P6b)", LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, loop2)).To(Succeed())
		audit.SetLearnSummary(uid, []alerts.Record{
			l2Record(1, "2026-11-08T10:00:00Z"),
			l2Record(2, "2026-11-08T10:05:00Z"),
			l2Record(3, "2026-11-08T10:10:00Z"),
		}, 0)
		second := s5aReconcile(r, ns, name)
		Expect(second.UID).To(Equal(uid), "the Loop is unchanged across reconciles")
		cm2 := l2GetCM(ns, cmName)
		Expect(cm2).NotTo(BeNil(), "the ConfigMap must still exist")
		Expect(cm2.UID).To(Equal(cm.UID), "the SAME ConfigMap must be UPDATED (not a new object created) on the second reconcile")
		body2 := cm2.Data[learnSummaryRecordsKey]
		lines2 := strings.Split(strings.TrimRight(body2, "\n"), "\n")
		Expect(lines2).To(HaveLen(3),
			"the second reconcile must UPDATE the ConfigMap to 3 lines (mutation m2: skipping the Update leaves 2)")
	})

	It("does NOT create a ConfigMap for a non-learn-mode Loop (mutation m1: drop the gate)", func() {
		ns := l2Ns("l2-nolearn")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "l2nolearn"
		audit := newF62Audit()
		uid := l2LearnLoop(ns, name)
		// Flip the condition to an ENFORCING Loop (not learn mode).
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Status.Conditions = []metav1.Condition{
			{
				Type:               string(PolicyEnforcedCondition),
				Status:             metav1.ConditionTrue,
				Reason:             "Enforcing",
				Message:            "enforcing",
				LastTransitionTime: metav1.Now(),
			},
		}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		r := l2Reconciler(audit)

		audit.SetLearnSummary(uid, []alerts.Record{l2Record(1, "2026-11-08T10:00:00Z")}, 0)
		s5aReconcile(r, ns, name)

		// The ConfigMap must NOT exist: the gate (isLearnMode) is False.
		Expect(l2GetCM(ns, learnSummaryCMName(name))).To(BeNil(),
			"a non-learn-mode Loop must not get a learn-summary ConfigMap (mutation m1: removing the gate writes one)")
	})

	It("calls Audit.Forget on the deleting path (R52 I249: neither summary nor snapshot was ever pruned)", func() {
		ns := l2Ns("l2-forget")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "l2forget"
		audit := newF62Audit()
		uid := l2LearnLoop(ns, name)
		audit.SetLearnSummary(uid, []alerts.Record{l2Record(1, "2026-11-08T10:00:00Z")}, 0)
		r := l2Reconciler(audit)
		s5aReconcile(r, ns, name)

		// Delete the Loop (the operator's finalizer is added on the first
		// reconcile; the delete path is finalizeB2DeleteStep, which calls
		// r.forgetAuditState).
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(k8sClient.Delete(ctx, loop)).To(Succeed())
		// The delete path reconciles the deleting Loop and calls Forget.
		// The Loop may be gone by the time the second reconcile runs (the
		// finalizer is removed and the Loop is garbage-collected), so use a
		// direct Reconcile that tolerates NotFound.
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})

		uids := audit.ForgetUIDs()
		Expect(uids).To(ContainElement(uid),
			"the deleting path must call Audit.Forget on the Loop's UID (the learn summary and the F62 snapshot are both pruned — R52 I249)")
	})

	It("does not overwrite a foreign ConfigMap of the same name (the operator leaves foreign objects alone)", func() {
		ns := l2Ns("l2-foreign")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "l2foreign"
		audit := newF62Audit()
		uid := l2LearnLoop(ns, name)
		r := l2Reconciler(audit)
		audit.SetLearnSummary(uid, []alerts.Record{l2Record(1, "2026-11-08T10:00:00Z")}, 0)

		// Pre-create a ConfigMap with the same name, owned by a DIFFERENT
		// controller (a foreign Loop). The operator's upsertLearnSummaryCM
		// must leave it alone (no write, no error, no Event).
		foreignLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, foreignLoop)).To(Succeed())
		// A foreign object: same name, not owned by the Loop. The owner ref
		// is a different UID (a Loop that is NOT the reconciled Loop).
		foreignCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      learnSummaryCMName(name),
				Namespace: ns,
				OwnerReferences: []metav1.OwnerReference{
					{APIVersion: "coxswain.wattu.com/v1alpha1", Kind: "Loop", Name: "foreign-" + name, UID: types.UID("foreign-uid")},
				},
			},
			Data: map[string]string{learnSummaryRecordsKey: "foreign-body", learnSummaryDroppedKey: "99"},
		}
		Expect(k8sClient.Create(ctx, foreignCM)).To(Succeed())
		_ = foreignLoop

		s5aReconcile(r, ns, name)

		cm := l2GetCM(ns, learnSummaryCMName(name))
		Expect(cm).NotTo(BeNil())
		Expect(cm.Data[learnSummaryRecordsKey]).To(Equal("foreign-body"),
			"the operator must NOT overwrite a foreign ConfigMap of the same name (the foreign object is left alone)")
		Expect(cm.Data[learnSummaryDroppedKey]).To(Equal("99"))
	})
})
