package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("D79: the wall clock pauses while a Loop waits at AwaitingApproval", func() {
	newLoop := func(phase coxv1alpha1.LoopPhase) (*LoopReconciler, *coxv1alpha1.Loop, *time.Time) {
		ns := "d79-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "d79"},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "D79 wall clock",
				Workspace: testWorkspace(),
				Agent:     coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		t0 := time.Now().Truncate(time.Second)
		clock := t0
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient,
			now: func() metav1.Time { return metav1.NewTime(clock) }}
		loop.Status.Phase = phase
		loop.Status.DesiredPhase = phase
		stamp := metav1.NewTime(t0)
		loop.Status.Budget = &coxv1alpha1.BudgetStatus{ActiveSeconds: 10, LastActiveStamp: &stamp}
		return r, loop, &clock
	}

	It("(a) 600s at AwaitingApproval adds nothing, and the stamp is refreshed so the wait never counts later", func() {
		r, loop, clock := newLoop(coxv1alpha1.LoopPhaseAwaitingApproval)
		*clock = clock.Add(600 * time.Second)
		r.applyBudget(ctx, loop)
		Expect(loop.Status.Budget.ActiveSeconds).To(Equal(int64(10)), "the approval wait must not count")
		By("after approval the Loop is Implementing; 60s later only those 60s are added")
		loop.Status.Phase = coxv1alpha1.LoopPhaseImplementing
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseImplementing
		*clock = clock.Add(60 * time.Second)
		r.applyBudget(ctx, loop)
		Expect(loop.Status.Budget.ActiveSeconds).To(Equal(int64(70)), "only the post-approval time counts")
	})

	It("(b) control: 600s while Implementing IS counted", func() {
		r, loop, clock := newLoop(coxv1alpha1.LoopPhaseImplementing)
		*clock = clock.Add(600 * time.Second)
		r.applyBudget(ctx, loop)
		Expect(loop.Status.Budget.ActiveSeconds).To(Equal(int64(610)))
	})
})
