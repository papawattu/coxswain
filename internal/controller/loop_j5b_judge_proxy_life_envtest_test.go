// Phase 8 J5b step 3a (D141): the Judge proxy's LIFECYCLE (<loop>-jproxy
// exists only while the Judge Job runs) and METERING (its usage folds into
// the SAME status.budget totals with its own boot-ID delta state).
package controller

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/papawattu/coxswain/internal/proxy"
)

func j5bOwnEndpoint(l *coxv1alpha1.Loop) {
	l.Spec.Verify.Judge.ModelEndpoint = "judge-vllm.judge-ns.svc:8001"
	l.Spec.Verify.Judge.EndpointSecretRef = "judge-key"
}

var _ = Describe("J5b 3a the Judge proxy lifecycle (D141)", func() {
	var ctx context.Context
	BeforeEach(func() { ctx = context.Background() })

	freshNS := func() string {
		ns := "j5bl-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}
	jproxyExists := func(ns, name string) (podFound, svcFound bool) {
		pod := &corev1.Pod{}
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: judgeProxyName(name)}, pod)
		if err != nil {
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%v", err)
		}
		svc := &corev1.Service{}
		serr := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: judgeProxyName(name)}, svc)
		if serr != nil {
			Expect(apierrors.IsNotFound(serr)).To(BeTrue(), "%v", serr)
		}
		return err == nil, serr == nil
	}
	finishJudge := func(ns, name, jobName string) {
		job := judgeGetJob(ns, jobName)
		judgePod(ctx, ns, name, job,
			corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0,
				Message: `{"verdict":true,"score":87,"reason":"meets the goal"}`}}, corev1.PodSucceeded)
		judgeCompleteJob(ctx, job)
	}

	It("own endpoint: the jproxy pod + Service exist while Judging and are GONE after the verdict, usage folded into status.budget", func() {
		ns := freshNS()
		jobName := judgeDriveToJudging(ns, "j5bl-a", false, j5bOwnEndpoint)
		pod, svc := jproxyExists(ns, "j5bl-a")
		Expect(pod).To(BeTrue(), "the jproxy pod exists while the Judge Job runs")
		Expect(svc).To(BeTrue(), "the jproxy Service exists while the Judge Job runs")
		p := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: judgeProxyName("j5bl-a")}, p)).To(Succeed())
		Expect(p.Labels).To(Equal(judgeProxyLabels("j5bl-a")))
		Expect(metav1.IsControlledBy(p, judgeGet(ns, "j5bl-a"))).To(BeTrue())
		s := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: judgeProxyName("j5bl-a")}, s)).To(Succeed())
		Expect(s.Spec.Selector).To(Equal(judgeProxyLabels("j5bl-a")))
		Expect(s.Spec.Ports[0].Port).To(Equal(int32(proxyPort)))
		Expect(metav1.IsControlledBy(s, judgeGet(ns, "j5bl-a"))).To(BeTrue())

		// A still-running Judge keeps the proxy.
		r := judgeFreshReconciler()
		judgeReconcile(r, ns, "j5bl-a")
		pod, svc = jproxyExists(ns, "j5bl-a")
		Expect(pod && svc).To(BeTrue(), "no teardown while the Judge is in progress")

		finishJudge(ns, "j5bl-a", jobName)
		reads := 0
		r.readJudgeProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			reads++
			return proxy.Reading{BootID: "jb1", PromptTokens: 100, CompletionTokens: 20, Requests: 1}, nil
		}
		loop := judgeReconcile(r, ns, "j5bl-a")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		pod, svc = jproxyExists(ns, "j5bl-a")
		Expect(pod).To(BeFalse(), "the jproxy pod is deleted once the verdict is recorded")
		Expect(svc).To(BeFalse(), "the jproxy Service is deleted once the verdict is recorded")
		Expect(reads).To(Equal(1), "exactly one FINAL read, before the delete")
		Expect(loop.Status.Budget).NotTo(BeNil())
		Expect(loop.Status.Budget.PromptTokens).To(Equal(int64(100)))
		Expect(loop.Status.Budget.CompletionTokens).To(Equal(int64(20)))
		Expect(loop.Status.Budget.JudgeProxy).NotTo(BeNil())
		Expect(loop.Status.Budget.JudgeProxy.LastBootID).To(Equal("jb1"))
	})

	It("no own endpoint: no jproxy is ever created", func() {
		ns := freshNS()
		jobName := judgeDriveToJudging(ns, "j5bl-b", false)
		pod, svc := jproxyExists(ns, "j5bl-b")
		Expect(pod || svc).To(BeFalse())
		finishJudge(ns, "j5bl-b", jobName)
		r := judgeFreshReconciler()
		r.readJudgeProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			Fail("no jproxy, so no Judge usage read")
			return proxy.Reading{}, nil
		}
		loop := judgeReconcile(r, ns, "j5bl-b")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		pod, svc = jproxyExists(ns, "j5bl-b")
		Expect(pod || svc).To(BeFalse())
		Expect(loop.Status.Budget == nil || loop.Status.Budget.JudgeProxy == nil).To(BeTrue())
	})

	It("a failed FINAL read still deletes the jproxy, with a Warning Event JudgeUsageUnread and a note on JudgeVerdict (I90)", func() {
		ns := freshNS()
		jobName := judgeDriveToJudging(ns, "j5bl-c", false, j5bOwnEndpoint)
		finishJudge(ns, "j5bl-c", jobName)
		r := judgeFreshReconciler()
		r.readJudgeProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{}, fmt.Errorf("jproxy gone: %w", proxy.ErrNotReady)
		}
		loop := judgeReconcile(r, ns, "j5bl-c")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded), "a read failure never blocks the Loop")
		pod, svc := jproxyExists(ns, "j5bl-c")
		Expect(pod || svc).To(BeFalse(), "deleted anyway")
		Expect(strings.Join(judgeDrainedEvents(r), "\n")).To(And(
			ContainSubstring("Warning"), ContainSubstring("JudgeUsageUnread")))
		vc := judgeVerdictCond(loop)
		Expect(vc).NotTo(BeNil())
		Expect(vc.Message).To(ContainSubstring("usage unread"))
		Expect(loop.Status.Budget == nil || loop.Status.Budget.PromptTokens == 0).To(BeTrue(), "no silent zero is invented as a reading")
	})

	It("a Judge job that failed also takes the jproxy down", func() {
		ns := freshNS()
		jobName := judgeDriveToJudging(ns, "j5bl-d", false, j5bOwnEndpoint)
		judgeFailedJob(ctx, judgeGetJob(ns, jobName))
		r := judgeFreshReconciler()
		r.readJudgeProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{BootID: "jb9", PromptTokens: 7, CompletionTokens: 3, Requests: 1}, nil
		}
		loop := judgeReconcile(r, ns, "j5bl-d")
		pod, svc := jproxyExists(ns, "j5bl-d")
		Expect(pod || svc).To(BeFalse())
		Expect(loop.Status.Budget.PromptTokens).To(Equal(int64(7)))
	})
})

var _ = Describe("J5b 3a the Judge proxy metering (D141)", func() {
	newR := func() *LoopReconciler { return judgeFreshReconciler() }
	loopWith := func() *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
	}

	It("a Judge reading of 100+20 adds exactly that; a second read of the same counters adds 0", func() {
		r, loop := newR(), loopWith()
		loop.Status.Budget = &coxv1alpha1.BudgetStatus{PromptTokens: 50, CompletionTokens: 10, Requests: 2}
		rd := proxy.Reading{BootID: "jb1", PromptTokens: 100, CompletionTokens: 20, Requests: 1}
		r.applyJudgeUsageReading(context.Background(), loop, rd)
		b := loop.Status.Budget
		Expect([]int64{b.PromptTokens, b.CompletionTokens, b.Requests}).To(Equal([]int64{150, 30, 3}))
		r.applyJudgeUsageReading(context.Background(), loop, rd)
		Expect([]int64{b.PromptTokens, b.CompletionTokens, b.Requests}).To(Equal([]int64{150, 30, 3}))
		Expect(b.JudgeProxy.LastBootID).To(Equal("jb1"))
		Expect(b.LastBootID).To(BeEmpty(), "the Loop proxy's own delta state is untouched")
		Expect(b.BootIDChanged).To(BeFalse(), "a new jproxy is normal, not a reset of the Loop proxy")
	})

	It("a same-boot read with higher counters adds only the delta", func() {
		r, loop := newR(), loopWith()
		r.applyJudgeUsageReading(context.Background(), loop, proxy.Reading{BootID: "jb1", PromptTokens: 100, CompletionTokens: 20, Requests: 1})
		r.applyJudgeUsageReading(context.Background(), loop, proxy.Reading{BootID: "jb1", PromptTokens: 130, CompletionTokens: 25, Requests: 2})
		b := loop.Status.Budget
		Expect([]int64{b.PromptTokens, b.CompletionTokens, b.Requests}).To(Equal([]int64{130, 25, 2}))
	})

	It("a jproxy boot-ID change (restart or a NEW jproxy for a later verify) adds the new counters without subtracting", func() {
		r, loop := newR(), loopWith()
		r.applyJudgeUsageReading(context.Background(), loop, proxy.Reading{BootID: "jb1", PromptTokens: 100, CompletionTokens: 20, Requests: 1})
		r.applyJudgeUsageReading(context.Background(), loop, proxy.Reading{BootID: "jb2", PromptTokens: 40, CompletionTokens: 8, Requests: 1})
		b := loop.Status.Budget
		Expect([]int64{b.PromptTokens, b.CompletionTokens, b.Requests}).To(Equal([]int64{140, 28, 2}))
		Expect(b.JudgeProxy.LastBootID).To(Equal("jb2"))
		Expect(b.JudgeProxy.LastPromptTokens).To(Equal(int64(40)))
		// And the new boot's later growth is a plain delta again.
		r.applyJudgeUsageReading(context.Background(), loop, proxy.Reading{BootID: "jb2", PromptTokens: 45, CompletionTokens: 9, Requests: 2})
		Expect(b.PromptTokens).To(Equal(int64(145)))
	})

	It("a token cap below Loop+Judge but above the Loop alone fires the budget decision", func() {
		r, loop := newR(), loopWith()
		cap := int64(150)
		loop.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxTokens: &cap, OnExceeded: coxv1alpha1.BudgetExceededActionFail}
		loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		loop.Status.Budget = &coxv1alpha1.BudgetStatus{PromptTokens: 90, CompletionTokens: 10}
		Expect(r.applyBudgetDecision(context.Background(), loop)).To(BeFalse(), "the Loop alone (100) is under the cap (150)")
		Expect(loop.Status.Budget.Exceeded).To(BeFalse())
		r.applyJudgeUsageReading(context.Background(), loop, proxy.Reading{BootID: "jb1", PromptTokens: 100, CompletionTokens: 20, Requests: 1})
		Expect(r.applyBudgetDecision(context.Background(), loop)).To(BeTrue(), "Loop+Judge (220) hits the cap")
		Expect(loop.Status.Budget.Exceeded).To(BeTrue())
		Expect(loop.Status.Budget.ExceededReason).To(Equal(coxv1alpha1.BudgetExceededTokens))
	})
})

