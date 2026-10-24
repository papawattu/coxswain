// P5f (I98, D69 option i): Succeeded is held until Delivered is terminal.
// While delivery is on and the Delivered condition is not terminal, the Loop
// stays at phase Verifying (condition Verifying=True reason Delivering) with
// the sandbox Suspended; both phase and desiredPhase become Succeeded once
// Delivered is True or False (D64). Delivery off goes straight to Succeeded.
package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
)

// TestP5fDeadlineExceededMapsToDeliveryFailed: a deliver Job that hit its
// activeDeadlineSeconds carries a JobFailed condition (status.failed may be 0)
// and is Delivered=False/DeliveryFailed, which settles a held Loop.
func TestP5fDeadlineExceededMapsToDeliveryFailed(t *testing.T) {
	r := &LoopReconciler{}
	loop := &coxv1alpha1.Loop{}
	loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
	loop.Spec.Delivery = &coxv1alpha1.DeliveryConfig{Mode: coxv1alpha1.DeliveryModePullRequest}
	loop.Spec.Workspace.Repo = "http://x/y.git"
	loop.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: "abc"}
	loop.Status.Conditions = []metav1.Condition{{
		Type: string(coxv1alpha1.LoopPhaseVerifying), Status: metav1.ConditionTrue,
		Reason: coxv1alpha1.ReasonVerifyingDelivering,
	}}
	job := &batchv1.Job{}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded"}}
	if _, err := r.deliverJobOutcome(loop, job); err != nil {
		t.Fatal(err)
	}
	if !deliveryTerminal(loop) {
		t.Fatalf("a deadline-exceeded Job must be a terminal DeliveryFailed; conditions=%v", loop.Status.Conditions)
	}
	if !settleDeliveryHold(loop) || loop.Status.Phase != coxv1alpha1.LoopPhaseSucceeded {
		t.Fatalf("the held Loop must settle to Succeeded; phase=%s", loop.Status.Phase)
	}
}

// TestP5fSandboxGateStoppedOnDeliveryHold: the extended loopSandboxStopped
// gate stops the sandbox for Verifying+Delivering only (a Verifying Loop that
// is not holding keeps its sandbox Running).
func TestP5fSandboxGateStoppedOnDeliveryHold(t *testing.T) {
	r := &LoopReconciler{}
	held := &coxv1alpha1.Loop{}
	held.Status.Phase = coxv1alpha1.LoopPhaseVerifying
	held.Status.Conditions = []metav1.Condition{{
		Type: string(coxv1alpha1.LoopPhaseVerifying), Status: metav1.ConditionTrue,
		Reason: coxv1alpha1.ReasonVerifyingDelivering,
	}}
	if got := r.sandboxOperatingMode(held, false); got != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Fatalf("Verifying+Delivering: mode = %s, want Suspended", got)
	}
	plain := &coxv1alpha1.Loop{}
	plain.Status.Phase = coxv1alpha1.LoopPhaseVerifying
	if got := r.sandboxOperatingMode(plain, false); got != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Fatalf("plain Verifying: mode = %s, want Running", got)
	}
}

// p5fToVerifyPassed drives a fresh Loop through Planning/Implementing and a
// passing verify Job, WITHOUT forcing Succeeded. delivery=false clears
// spec.delivery; lessons sets lessonsAvailable on the Implementing claim.
func p5fToVerifyPassed(name, ns string, delivery, lessons bool, mutate ...func(*coxv1alpha1.Loop)) *LoopReconciler {
	ctx := context.Background()
	r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
	s5aEnsureSandbox(ns, name)
	loop := s6Loop(name, ns, "")
	if !delivery {
		loop.Spec.Delivery = nil
	}
	for _, m := range mutate {
		m(loop)
	}
	Expect(k8sClient.Create(ctx, loop)).To(Succeed())
	s6Reconcile(r, ns, name)
	loop = &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	loop.Status.BaseCommit = s5aBaseCommit
	Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
	s5aClaimPod(ns, name, "Planning", "")
	s6Reconcile(r, ns, name)
	s5aEnsureSandbox(ns, name)
	if lessons {
		s5aClaimPodWithLessonsAvailable(ns, name, "Implementing", s6HeadCommit, true)
	} else {
		s5aClaimPod(ns, name, "Implementing", s6HeadCommit)
	}
	s6Reconcile(r, ns, name)
	d61ReadyVerifyProxy(r, ns, name)
	s5aVerifyPod(ctx, ns, name, name+"-verify-1", 0)
	s6Reconcile(r, ns, name)
	return r
}

func p5fGet(ns, name string) *coxv1alpha1.Loop {
	l := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, l)).To(Succeed())
	return l
}

func p5fSandboxMode(ns, name string) sandboxv1beta1.SandboxOperatingMode {
	sbx := &sandboxv1beta1.Sandbox{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sbx)).To(Succeed())
	return sbx.Spec.OperatingMode
}

var _ = Describe("P5f: Succeeded is held until Delivered is terminal (I98)", func() {
	ctx := context.Background()
	freshNS := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })
		return ns
	}
	pushDone := func(ns, name string, exit int32, msg string) {
		s6DeliverPod(ns, name, corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: msg, Reason: i65ContainerErrReason},
		})
	}

	It("holds at Verifying/Delivering (not Succeeded) with the sandbox Suspended while Delivered is in progress", func() {
		ns := freshNS("p5f-hold")
		name := "hold1"
		r := p5fToVerifyPassed(name, ns, true, false)

		loop := p5fGet(ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "verify passed but delivery is not terminal")
		Expect(loop.Status.DesiredPhase).ToNot(Equal(coxv1alpha1.LoopPhaseSucceeded))
		Expect(loopHoldingForDelivery(loop)).To(BeTrue())

		By("the deliver Job runs during the hold; still in progress -> still held, across reconciles")
		s6Reconcile(r, ns, name)
		s6DeliverPod(ns, name, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
		for range 3 {
			loop = s6Reconcile(r, ns, name)
		}
		_ = s6GetJob(ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(loopHoldingForDelivery(loop)).To(BeTrue())
		ok, status, reason := s6Cond(loop)
		Expect(ok).To(BeTrue())
		Expect(status).To(Equal(metav1.ConditionFalse))
		Expect(reason).To(Equal(coxv1alpha1.ReasonDeliveryInProgress))

		By("the sandbox is Suspended during the hold (extended loopSandboxStopped gate)")
		Expect(p5fSandboxMode(ns, name)).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended))
	})

	It("sets Succeeded (phase AND desiredPhase) once Delivered=True", func() {
		ns := freshNS("p5f-ok")
		name := "ok1"
		r := p5fToVerifyPassed(name, ns, true, false)
		s6Reconcile(r, ns, name)
		loop := p5fGet(ns, name)
		pushDone(ns, name, 0, s6ValidTermination(loop))
		_ = s6Reconcile(r, ns, name)
		loop = s6Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		_, status, _ := s6Cond(loop)
		Expect(status).To(Equal(metav1.ConditionTrue))
		Expect(loopHoldingForDelivery(loop)).To(BeFalse())
		Expect(p5fSandboxMode(ns, name)).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended))
	})

	It("a user script waiting for Succeeded does not return until the PR exists", func() {
		ns := freshNS("p5f-wait")
		name := "wait1"
		r := p5fToVerifyPassed(name, ns, true, false)
		s6Reconcile(r, ns, name)
		s6DeliverPod(ns, name, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
		for range 4 {
			l := s6Reconcile(r, ns, name)
			Expect(l.Status.Phase).ToNot(Equal(coxv1alpha1.LoopPhaseSucceeded), "waiter must not see Succeeded before the PR exists")
			Expect(l.Status.Delivery).To(BeNil())
		}
		loop := p5fGet(ns, name)
		pushDone(ns, name, 0, s6ValidTermination(loop))
		for range 3 {
			loop = s6Reconcile(r, ns, name)
		}
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		Expect(loop.Status.Delivery).ToNot(BeNil(), "by the time the waiter sees Succeeded the PR is recorded")
		Expect(loop.Status.Delivery.PRNumber).To(BeEquivalentTo(7))
	})

	It("a FAILED delivery is still Succeeded with Delivered=False (D64)", func() {
		ns := freshNS("p5f-fail")
		name := "fail1"
		r := p5fToVerifyPassed(name, ns, true, false)
		s6Reconcile(r, ns, name)
		pushDone(ns, name, 1, "")
		var loop *coxv1alpha1.Loop
		for range 3 {
			loop = s6Reconcile(r, ns, name)
		}
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		_, status, reason := s6Cond(loop)
		Expect(status).To(Equal(metav1.ConditionFalse))
		Expect(reason).To(Equal(coxv1alpha1.ReasonDeliveryFailed))
	})

	It("delivery off: Succeeded immediately, no Delivered condition, no hold", func() {
		ns := freshNS("p5f-off")
		name := "off1"
		_ = p5fToVerifyPassed(name, ns, false, false)
		loop := p5fGet(ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		present, _, _ := s6Cond(loop)
		Expect(present).To(BeFalse())
		Expect(loopHoldingForDelivery(loop)).To(BeFalse())
	})

	It("keeps a REAL sandbox Suspended during the hold (gates satisfied: not masked by D30/D35a), Running at plain Verifying", func() {
		ns := freshNS("p5f-gate")
		name := "gate1"
		r := &LoopReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), AllowUnenforced: true,
			CNIProber: cni.NewFakeProber(), Recorder: record.NewFakeRecorder(64),
			readPhaseClaim: func(context.Context, *coxv1alpha1.Loop) (*PhaseClaim, error) { return nil, nil },
		}
		r.CNIProber.(*cni.FakeProber).SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced})
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: p2fModelSecret, Namespace: ns},
			StringData: map[string]string{modelAPIKey: "p5f-dummy", modelBaseURL: p2fModelEndpoint},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal: "P5f gate", Workspace: testWorkspace(),
				Agent: coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel, EndpointSecretRef: p2fModelSecret, ModelEndpoint: p2fModelEndpoint},
			},
		})).To(Succeed())
		Expect(r.ensureProxy(ctx, p5fGet(ns, name))).To(Succeed())
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: proxyPodName(name), Namespace: ns}, pod)).To(Succeed())
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		rec := func() {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
			Expect(err).NotTo(HaveOccurred())
		}
		rec()
		rec()
		rec()
		Expect(p5fSandboxMode(ns, name)).To(Equal(sandboxv1beta1.SandboxOperatingModeRunning), "baseline: Running")

		By("plain Verifying (no hold) keeps the sandbox Running")
		l := p5fGet(ns, name)
		l.Status.Phase, l.Status.DesiredPhase = coxv1alpha1.LoopPhaseVerifying, coxv1alpha1.LoopPhaseVerifying
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
		rec()
		rec()
		Expect(p5fSandboxMode(ns, name)).To(Equal(sandboxv1beta1.SandboxOperatingModeRunning))

		By("Verifying + reason Delivering stops the sandbox")
		l = p5fGet(ns, name)
		l.Spec.Delivery = &coxv1alpha1.DeliveryConfig{Mode: coxv1alpha1.DeliveryModePullRequest}
		Expect(k8sClient.Update(ctx, l)).To(Succeed())
		l = p5fGet(ns, name)
		l.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: s6HeadCommit}
		setCondition(l, string(coxv1alpha1.LoopPhaseVerifying), metav1.ConditionTrue, coxv1alpha1.ReasonVerifyingDelivering, "hold")
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
		rec()
		rec()
		Expect(p5fGet(ns, name).Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(p5fSandboxMode(ns, name)).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended))
	})

	It("P2-1: a held Loop past its wall-clock budget (onExceeded=Fail) stays held, accrues no time, and settles to Succeeded", func() {
		ns := freshNS("p5f-budget")
		name := "budget1"
		r := p5fToVerifyPassed(name, ns, true, false, func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxWallClock: "1m", OnExceeded: coxv1alpha1.BudgetExceededActionFail}
		})
		s6Reconcile(r, ns, name)
		Expect(loopHoldingForDelivery(p5fGet(ns, name))).To(BeTrue())
		By("the clock jumps 2h during the hold")
		r.now = func() metav1.Time { return metav1.NewTime(time.Now().Add(2 * time.Hour)) }
		s6DeliverPod(ns, name, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
		var loop *coxv1alpha1.Loop
		for range 3 {
			loop = s6Reconcile(r, ns, name)
		}
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "the budget must not fail a verified, delivering Loop")
		Expect(loopHoldingForDelivery(loop)).To(BeTrue())
		if loop.Status.Budget != nil {
			// A few real seconds may accrue before the hold starts; the 2h clock
			// jump must not be added.
			Expect(loop.Status.Budget.ActiveSeconds).To(BeNumerically("<", 600), "no wall-clock accrual during the hold")
		}
		pushDone(ns, name, 0, s6ValidTermination(loop))
		for range 3 {
			loop = s6Reconcile(r, ns, name)
		}
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		Expect(loop.Status.Delivery).ToNot(BeNil(), "the delivery result is recorded")
	})

	It("P2-1b: the budget DECISION is inert during the hold (already-exceeded wall clock does not fail a held Loop)", func() {
		ns := freshNS("p5f-budget2")
		name := "budget2"
		r := p5fToVerifyPassed(name, ns, true, false, func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxWallClock: "1m", OnExceeded: coxv1alpha1.BudgetExceededActionFail}
		})
		s6Reconcile(r, ns, name)
		l := p5fGet(ns, name)
		Expect(loopHoldingForDelivery(l)).To(BeTrue())
		if l.Status.Budget == nil {
			l.Status.Budget = &coxv1alpha1.BudgetStatus{}
		}
		l.Status.Budget.ActiveSeconds = 7200 // past the 1m cap, as if accrued before the hold
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
		var loop *coxv1alpha1.Loop
		for range 3 {
			loop = s6Reconcile(r, ns, name)
		}
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "the budget decision must not fail a held Loop")
		Expect(loopHoldingForDelivery(loop)).To(BeTrue())
	})

	It("P2-2: clearing spec.delivery mid-hold settles to Succeeded (no permanent hold)", func() {
		ns := freshNS("p5f-clear")
		name := "clear1"
		r := p5fToVerifyPassed(name, ns, true, false)
		Expect(loopHoldingForDelivery(p5fGet(ns, name))).To(BeTrue())
		l := p5fGet(ns, name)
		l.Spec.Delivery = nil
		Expect(k8sClient.Update(ctx, l)).To(Succeed())
		var loop *coxv1alpha1.Loop
		for range 3 {
			loop = s6Reconcile(r, ns, name)
		}
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		Expect(loopHoldingForDelivery(loop)).To(BeFalse())
	})

	It("the deliver Job carries an ActiveDeadlineSeconds", func() {
		ns := freshNS("p5f-deadline")
		name := "deadline1"
		r := p5fToVerifyPassed(name, ns, true, false)
		s6Reconcile(r, ns, name)
		job := s6GetJob(ns, name)
		Expect(job.Spec.ActiveDeadlineSeconds).ToNot(BeNil())
		Expect(*job.Spec.ActiveDeadlineSeconds).To(Equal(deliverJobActiveDeadlineSeconds))
	})

	Context("memory writer interaction (P5b/P5c)", func() {
		It("does NOT start the memory-writer Job during the Verifying/Delivering hold, and starts it once Succeeded after delivery", func() {
			ns := freshNS("p5f-mem")
			name := "mem1"
			r := p5fToVerifyPassed(name, ns, true, true)
			s6Reconcile(r, ns, name)
			s6DeliverPod(ns, name, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
			for range 3 {
				loop := s6Reconcile(r, ns, name)
				Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
			}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: memoryWriterJobName(p5bMustLoop(ns, name)), Namespace: ns}, &batchv1.Job{})
			Expect(err).To(HaveOccurred(), "no memory-writer Job while held")

			By("delivery terminal (False: D64) -> Succeeded -> the writer Job is created")
			pushDone(ns, name, 1, "")
			var loop *coxv1alpha1.Loop
			for range 4 {
				loop = s6Reconcile(r, ns, name)
			}
			Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
			Expect(p5bGetMemoryJob(ns, name)).ToNot(BeNil())
		})
	})
})
