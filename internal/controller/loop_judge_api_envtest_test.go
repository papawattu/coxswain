// Judge API (ADR-0011, J1a): the API shape of the opt-in advisory Judge —
// spec.verify.judge, status.judge, and the JudgeVerdict condition constants.
// No behaviour: the CRD schema is what these specs exercise (accept, reject,
// no defaulting), plus the status subresource's validation of the recorded
// verdict's fixed shape.
package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("Judge API", func() {
	ctx := context.Background()
	freshNS := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })
		return ns
	}
	newLoop := func(ns, name string, judge *coxv1alpha1.JudgeConfig) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "Judge API",
				Workspace: testWorkspace(),
				Agent:     coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
				Verify: coxv1alpha1.VerifyConfig{
					AcceptanceChecks: []string{"go build ./..."},
					Judge:            judge,
				},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
		return got
	}

	It("accepts a Loop with verify.judge {enabled: true, maxTokens: 512} and reads it back", func() {
		ns := freshNS("j1a-accept")
		got := newLoop(ns, "judge-on", &coxv1alpha1.JudgeConfig{Enabled: true, MaxTokens: 512})
		Expect(got.Spec.Verify.Judge).ToNot(BeNil())
		Expect(got.Spec.Verify.Judge.Enabled).To(BeTrue())
		Expect(got.Spec.Verify.Judge.MaxTokens).To(Equal(int64(512)))
	})

	It("rejects a Loop with a negative verify.judge.maxTokens (minimum is 1; a plain 0 is the zero value and drops off)", func() {
		ns := freshNS("j1a-zero")
		// 0 is the int64 zero value, so the omitempty tag drops it from the
		// serialized body and the field is simply ABSENT (accepted, no
		// default — the no-defaulting spec below relies on exactly this). The
		// Minimum=1 boundary therefore cannot fire on a plain 0: it fires on
		// any non-zero negative, which must be rejected.
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "judge-zero"},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "Judge API",
				Workspace: testWorkspace(),
				Agent:     coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
				Verify: coxv1alpha1.VerifyConfig{
					AcceptanceChecks: []string{"go build ./..."},
					Judge:            &coxv1alpha1.JudgeConfig{Enabled: true, MaxTokens: -1},
				},
			},
		}
		if err := k8sClient.Create(ctx, loop); err == nil {
			Fail("the API server must reject a negative maxTokens (Minimum=1)")
		} else {
			Expect(err.Error()).To(MatchRegexp(`maxTokens`), "the API server must reject a negative maxTokens (Minimum=1)")
		}
	})

	It("rejects an enabled Judge without a maxTokens cap (XValidation)", func() {
		ns := freshNS("j1a-nocap")
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "judge-nocap"},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "Judge API",
				Workspace: testWorkspace(),
				Agent:     coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
				Verify: coxv1alpha1.VerifyConfig{
					AcceptanceChecks: []string{"go build ./..."},
					Judge:            &coxv1alpha1.JudgeConfig{Enabled: true},
				},
			},
		}
		if err := k8sClient.Create(ctx, loop); err == nil {
			Fail("the API server must reject an enabled Judge without maxTokens")
		} else {
			Expect(err.Error()).To(MatchRegexp(`maxTokens`), "the API server must reject an enabled Judge without maxTokens (the XValidation message names it)")
		}
	})

	It("accepts a disabled Judge without a maxTokens cap (XValidation is opt-in only)", func() {
		ns := freshNS("j1a-disabled")
		got := newLoop(ns, "judge-disabled", &coxv1alpha1.JudgeConfig{Enabled: false})
		Expect(got.Spec.Verify.Judge).ToNot(BeNil())
		Expect(got.Spec.Verify.Judge.Enabled).To(BeFalse())
		Expect(got.Spec.Verify.Judge.MaxTokens).To(BeZero())
	})

	It("rejects a Loop with verify.judge.maxTokens = 40000 (maximum is 32768)", func() {
		ns := freshNS("j1a-big")
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "judge-big"},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "Judge API",
				Workspace: testWorkspace(),
				Agent:     coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
				Verify: coxv1alpha1.VerifyConfig{
					AcceptanceChecks: []string{"go build ./..."},
					Judge:            &coxv1alpha1.JudgeConfig{Enabled: true, MaxTokens: 40000},
				},
			},
		}
		if err := k8sClient.Create(ctx, loop); err == nil {
			Fail("the API server must reject maxTokens: 40000 (Maximum=32768)")
		} else {
			Expect(err.Error()).To(MatchRegexp(`maxTokens`), "the API server must reject maxTokens: 40000 (Maximum=32768)")
		}
	})

	It("never defaults judge: a Loop without it reads back with spec.verify.judge == nil", func() {
		ns := freshNS("j1a-none")
		got := newLoop(ns, "judge-off", nil)
		Expect(got.Spec.Verify.Judge).To(BeNil(), "the Judge is opt-in and never defaulted (ADR-0011)")
	})

	It("rejects a status update with judge.score = 101 (maximum is 100)", func() {
		ns := freshNS("j1a-score")
		newLoop(ns, "judge-score", nil)
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "judge-score"}, loop)).To(Succeed())
		v := true
		loop.Status.Judge = &coxv1alpha1.JudgeStatus{
			Verdict:   &v,
			Score:     101,
			Reason:    "looks fine",
			Truncated: false,
			At:        &metav1.Time{Time: time.Now()},
		}
		err := k8sClient.Status().Update(ctx, loop)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(MatchRegexp(`score`), "the API server must reject judge.score: 101 (Maximum=100)")
	})

	It("rejects a status update with a judge.reason of 201 chars (maximum is 200)", func() {
		ns := freshNS("j1a-reason")
		newLoop(ns, "judge-reason", nil)
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "judge-reason"}, loop)).To(Succeed())
		v := true
		loop.Status.Judge = &coxv1alpha1.JudgeStatus{
			Verdict:   &v,
			Score:     90,
			Reason:    string(make([]byte, 201)),
			Truncated: false,
			At:        &metav1.Time{Time: time.Now()},
		}
		err := k8sClient.Status().Update(ctx, loop)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(MatchRegexp(`reason`), "the API server must reject a 201-char judge.reason (MaxLength=200)")
	})

	It("records a well-formed status.judge and exposes the JudgeVerdict condition constants", func() {
		ns := freshNS("j1a-record")
		newLoop(ns, "judge-record", &coxv1alpha1.JudgeConfig{Enabled: true, MaxTokens: 1024})
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "judge-record"}, loop)).To(Succeed())
		v := false
		loop.Status.Judge = &coxv1alpha1.JudgeStatus{
			Verdict:   &v,
			Score:     42,
			Reason:    "games the tests",
			Truncated: true,
			At:        &metav1.Time{Time: time.Now()},
		}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "judge-record"}, got)).To(Succeed())
		Expect(got.Status.Judge).ToNot(BeNil())
		Expect(got.Status.Judge.Verdict).ToNot(BeNil())
		Expect(*got.Status.Judge.Verdict).To(BeFalse())
		Expect(got.Status.Judge.Score).To(Equal(int32(42)))
		Expect(got.Status.Judge.Reason).To(Equal("games the tests"))
		Expect(got.Status.Judge.Truncated).To(BeTrue())
		// The condition constants the J1 behaviour will write (ADR-0011).
		Expect(coxv1alpha1.JudgeVerdictCondition).To(Equal("JudgeVerdict"))
		Expect(coxv1alpha1.ReasonJudgeUnavailable).To(Equal("JudgeUnavailable"))
	})
})
