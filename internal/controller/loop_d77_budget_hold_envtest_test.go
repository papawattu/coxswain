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

// D77 (R30, owner 2026-11-01: (b) for maxCost, (a) for maxTokens): a Loop
// with a maxCost cap holds when its usage reading is STALE
// (BudgetReadingStale == True, F44) — the cost cap cannot be enforced
// without a fresh reading, and holding is the owner's choice for maxCost
// (maxTokens-only Loops keep the fail-open + surface-only behaviour, so a
// stale reading only SURFACES there). The gate sits AFTER the proxy ensure
// and after applyBudgetStep's read (NOT in validateStep — a hold there would
// deadlock a new maxCost Loop that has no reading yet: it would never get a
// proxy, so never a reading, so be held forever). While held: the phase
// advance and any new sandbox/iteration are blocked, the running sandbox is
// suspended (the agent spends model money), and the proxy + usage read keep
// running so a recovered reading releases the hold.

import (
	"context"
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
	"github.com/papawattu/coxswain/internal/proxy"
)

var _ = Describe("D77: budget-hold gate (maxCost + stale reading)", func() {
	ctx := context.Background()

	const (
		d77CostCap = "0.01"
	)

	getLoop := func(ns, name string) *coxv1alpha1.Loop {
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
		return got
	}

	newReconciler := func(recorder *record.FakeRecorder) *LoopReconciler {
		r := &LoopReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			AllowUnenforced: true,
			CNIProber:       cni.NewFakeProber(),
			Recorder:        recorder,
			readPhaseClaim: func(context.Context, *coxv1alpha1.Loop) (*PhaseClaim, error) {
				return nil, nil
			},
			readBaseCommit: func(context.Context, *coxv1alpha1.Loop) (string, bool, error) {
				return p2dSandboxSHA, true, nil
			},
		}
		r.baseCommitSeeded = true
		r.CNIProber.(*cni.FakeProber).SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced})
		return r
	}

	// createLoop creates a maxCost Loop (modelPrices inline, so the cost is
	// derived without the cluster ConfigMap) and seeds status.baseCommit.
	createLoop := func(ns, name string, mutate func(*coxv1alpha1.Loop)) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "D77 budget-hold spec",
				Workspace: testWorkspace(),
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: p2dModelSecret,
					ModelEndpoint:     p2dModelEndpoint,
				},
			},
		}
		if mutate == nil {
			mutate = func(l *coxv1alpha1.Loop) {
				l.Spec.Budget = &coxv1alpha1.BudgetConfig{
					MaxCostUsd:  coxv1alpha1.DecimalString(d77CostCap),
					ModelPrices: &coxv1alpha1.ModelPrices{PromptUsdPerMtok: "1", CompletionUsdPerMtok: "1"},
				}
			}
		}
		mutate(loop)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		l := getLoop(ns, name)
		l.Status.BaseCommit = p2dSandboxSHA
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed(), "seed status.baseCommit")
		return l
	}

	// primeProxy marks the model proxy pod Ready (D35a) via the operator's
	// own ensureProxy + a headless egress Service (the P2d pattern: the
	// readProxyUsage seam stands in for the pod's :9090 endpoint).
	primeProxy := func(r *LoopReconciler, loop *coxv1alpha1.Loop) {
		_ = k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: p2dModelSecret, Namespace: loop.Namespace},
			StringData: map[string]string{modelAPIKey: "d77-dummy", modelBaseURL: p2dModelEndpoint},
		})
		Expect(r.ensureProxy(ctx, loop)).To(Succeed())
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: proxyPodName(loop.Name)}, pod)).To(Succeed(),
			"ensureProxy must have created the proxy pod")
		now := metav1.Now()
		pod.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
		pod.Status.PodIP = "127.0.0.1"
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		_ = k8sClient.Create(ctx, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: egressProxyServiceName(loop.Name), Namespace: loop.Namespace},
			Spec: corev1.ServiceSpec{ClusterIP: "None"},
		})
	}

	// reconcile drives the Loop through the full Reconcile (including the
	// trailing status write) and re-reads it from the API. The budget read
	// (applyBudget, every non-hold pass) runs inside Reconcile, so one
	// reconcile is one failed read — driving N failed reads takes N
	// reconciles (budgetReadFailureThreshold trips after 3, one per pass).
	reconcile := func(r *LoopReconciler, ns, name string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred(), "reconcile %s/%s", ns, name)
		if l := getLoop(ns, name); l.Status.Phase == coxv1alpha1.LoopPhaseVerifying {
			// A claimless Planning Loop advances to Verifying (the verifyJob
			// step) where the verify reader early-returns; park it back at
			// Planning (the D77 specs test the budget read, not the verify
			// outcome).
			l.Status.Phase = coxv1alpha1.LoopPhasePlanning
			Expect(k8sClient.Status().Update(ctx, l)).To(Succeed(), "park %s/%s at Planning", ns, name)
		}
	}

	// failReads drives the seam into consecutive failed reads (the
	// BudgetReadingStale trigger: budgetReadFailureThreshold, one per
	// reconcile).
	failReads := func(r *LoopReconciler) {
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{}, errors.New("usage endpoint refused")
		}
	}

	getSandbox := func(ns, name string) *sandboxv1beta1.Sandbox {
		sb := &sandboxv1beta1.Sandbox{}
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-sandbox"}, sb)
		if client.IgnoreNotFound(err) != nil {
			Fail(fmt.Sprintf("read sandbox %s/%s: %v", ns, name+"-sandbox", err))
		}
		return sb
	}

	// cond returns the condition by type (nil when absent).
	cond := func(l *coxv1alpha1.Loop, condType string) *metav1.Condition {
		for i := range l.Status.Conditions {
			if l.Status.Conditions[i].Type == condType {
				return &l.Status.Conditions[i]
			}
		}
		return nil
	}

	// drainEvents pulls the FakeRecorder's event lines.
	drainEvents := func(recorder *record.FakeRecorder) []string {
		var out []string
	Loop:
		for {
			select {
			case e, ok := <-recorder.Events:
				if !ok {
					break Loop
				}
				out = append(out, e)
			default:
				break Loop
			}
		}
		return out
	}

	nsFor := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	It("spec 1: maxCost + stale reading -> BudgetHold=True (held across reconciles; sandbox suspended; proxy kept up)", func() {
		ns := nsFor("d77-s1")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newReconciler(recorder)
		loop := createLoop(ns, "d77-s1", nil)
		primeProxy(r, loop)
		// The bootstrap reconcile may advance the Loop to Verifying (the
		// claimless-Planning advance); parking at Planning is harmless — the
			// D77 specs test the budget read + hold gate, not the verify outcome.
		reconcile(r, ns, "d77-s1") // bootstrap to Planning
		Expect(getLoop(ns, "d77-s1").Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning))

		By("a fresh read: the first read adopts the baseline (not stale, not held)")
		fails := 0
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			if fails > 0 {
				fails--
				return proxy.Reading{}, errors.New("usage endpoint refused")
			}
			return proxy.Reading{BootID: "B1", PromptTokens: 0, CompletionTokens: 0, Requests: 0}, nil
		}
		reconcile(r, ns, "d77-s1")
		l := getLoop(ns, "d77-s1")
		Expect(cond(l, coxv1alpha1.BudgetReadingStaleCondition)).NotTo(BeTrue(), "a successful read is not stale (absent or False)")
		Expect(cond(l, coxv1alpha1.BudgetHoldCondition)).NotTo(BeTrue(), "a fresh reading is not a hold (absent or False)")

		By("3 consecutive failed reads (one per reconcile) -> BudgetReadingStale=True (F44) and BudgetHold=True")
		failReads(r)
		for i := 0; i < budgetReadFailureThreshold; i++ {
			reconcile(r, ns, "d77-s1")
		}
		l = getLoop(ns, "d77-s1")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning), "the hold is at Planning")
		Expect(cond(l, coxv1alpha1.BudgetReadingStaleCondition)).To(BeTrue(), "F44: 3 failed reads set BudgetReadingStale")
		hold := cond(l, coxv1alpha1.BudgetHoldCondition)
		Expect(hold).NotTo(BeNil(), "BudgetHold condition must be set")
		Expect(hold.Status).To(Equal(metav1.ConditionTrue), "a maxCost Loop with a stale reading is held")
		Expect(hold.Reason).To(Equal("StaleReading"), "the hold's reason is stable (StaleReading)")
		events := drainEvents(recorder)
		Expect(events).To(ContainElement(ContainSubstring("BudgetHold")),
			"a Warning BudgetHold Event fires on the False->True transition: %v", events)

		By("the sandbox (Running while at Planning) is suspended — the agent spends model money")
		Expect(getSandbox(ns, "d77-s1").Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"a held Loop's running sandbox is suspended")

		By("the proxy is kept up so the reading can recover (reads keep running while held)")
		proxyPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: proxyPodName("d77-s1")}, proxyPod)).To(Succeed(),
			"the proxy pod must survive the hold (its read is the release)")

		By("two further stale reconciles: still held, phase unchanged (no advance, no claim)")
		reconcile(r, ns, "d77-s1")
		reconcile(r, ns, "d77-s1")
		l = getLoop(ns, "d77-s1")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning), "a hold is not a phase change")
		hold = cond(l, coxv1alpha1.BudgetHoldCondition)
		Expect(hold).NotTo(BeNil())
		Expect(hold.Status).To(Equal(metav1.ConditionTrue), "the hold persists while the reading stays stale")
	})

	It("spec 2: the same Loop releases on a fresh reading (BudgetHold=False, reason ReadingFresh; no restart, no manual resume)", func() {
		ns := nsFor("d77-s2")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newReconciler(recorder)
		loop := createLoop(ns, "d77-s2", nil)
		primeProxy(r, loop)
		reconcile(r, ns, "d77-s2")
		failReads(r)
		for i := 0; i < budgetReadFailureThreshold; i++ {
			reconcile(r, ns, "d77-s2")
		}
		l := getLoop(ns, "d77-s2")
		held := cond(l, coxv1alpha1.BudgetHoldCondition)
		Expect(held).NotTo(BeNil(), "the hold condition is persisted on the held pass (re-read from the API)")
		Expect(held.Status).To(Equal(metav1.ConditionTrue), "held first")

		By("the seam flips to a fresh reading: the release is automatic")
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{BootID: "B1", PromptTokens: 10, CompletionTokens: 5, Requests: 1}, nil
		}
		reconcile(r, ns, "d77-s2")
		l = getLoop(ns, "d77-s2")
		Expect(cond(l, coxv1alpha1.BudgetReadingStaleCondition).Status).To(Equal(metav1.ConditionFalse),
			"a fresh read clears BudgetReadingStale (noteUsageReadOK)")
		hold := cond(l, coxv1alpha1.BudgetHoldCondition)
		Expect(hold).NotTo(BeNil(), "the condition is cleared, not dropped (it is the surface of the release)")
		Expect(hold.Status).To(Equal(metav1.ConditionFalse), "a fresh reading releases the hold")
		Expect(hold.Reason).To(Equal("ReadingFresh"), "the release's reason is stable (ReadingFresh)")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning), "release is not a phase change (no restart)")
		By("the budget keeps working after the release (the read folded in: the cost was derived)")
		Expect(l.Status.Budget).NotTo(BeNil())
		Expect(l.Status.Budget.CostUsd).NotTo(BeEmpty(), "a post-release fresh read derives the cost (the read kept running)")
	})

	It("spec 3: maxTokens only + stale reading -> NOT held (the owner chose surface-only for maxTokens)", func() {
		ns := nsFor("d77-s3")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newReconciler(recorder)
		loop := createLoop(ns, "d77-s3", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxTokens: new(int64)}
			*l.Spec.Budget.MaxTokens = 200
		})
		primeProxy(r, loop)
		reconcile(r, ns, "d77-s3")
		failReads(r)
		for i := 0; i < budgetReadFailureThreshold; i++ {
			reconcile(r, ns, "d77-s3")
		}
		l := getLoop(ns, "d77-s3")
		Expect(cond(l, coxv1alpha1.BudgetReadingStaleCondition).Status).To(Equal(metav1.ConditionTrue),
			"F44 still SURFACES the staleness for a maxTokens-only Loop")
		Expect(cond(l, coxv1alpha1.BudgetHoldCondition)).NotTo(BeTrue(),
			"maxTokens-only: the stale reading surfaces, it does not hold (owner choice (a); absent or False)")
	})

	It("spec 4: a never-Ready proxy -> held only AFTER the F44 threshold, NOT before (the startup case is not a hold)", func() {
		ns := nsFor("d77-s4")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newReconciler(recorder)
		loop := createLoop(ns, "d77-s4", nil)
		primeProxy(r, loop)
		// The read proxy pod is DELETED (the proxy never comes up; the operator
		// re-ensures it every reconcile, as it does in envtest with the real
		// ensureProxy, and the pod comes back NOT Ready -> proxy.ErrNotReady
		// while in grace, then a counted failure once past it).
		_ = k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: proxyPodName("d77-s4"), Namespace: ns}})
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{}, fmt.Errorf("dial usage endpoint: connection refused")
		}

		By("failures UNDER the threshold: not held (a starting proxy is not a stale reading)")
		for i := 0; i < budgetReadFailureThreshold-1; i++ {
			reconcile(r, ns, "d77-s4")
			l := getLoop(ns, "d77-s4")
			Expect(cond(l, coxv1alpha1.BudgetHoldCondition)).NotTo(BeTrue(),
				"failures below the F44 threshold (%d) must NOT hold the Loop (absent or False)", budgetReadFailureThreshold)
		}

		By("at the threshold: BudgetReadingStale trips (the never-had-a-reading case) and the hold follows")
		reconcile(r, ns, "d77-s4")
		l := getLoop(ns, "d77-s4")
		stale := cond(l, coxv1alpha1.BudgetReadingStaleCondition)
		Expect(stale).NotTo(BeNil())
		Expect(stale.Status).To(Equal(metav1.ConditionTrue), "F44: a never-had-a-reading proxy past the threshold is stale")
		hold := cond(l, coxv1alpha1.BudgetHoldCondition)
		Expect(hold).NotTo(BeNil())
		Expect(hold.Status).To(Equal(metav1.ConditionTrue), "a maxCost Loop is held once its reading is stale")
	})

	// reconcileStale reconciles past the F44 threshold (threshold+1 failed
	// reads) on the named Loop with the given reconciler.
	reconcileStale := func(r *LoopReconciler, ns, name string) {
		failReads(r)
		for i := 0; i < budgetReadFailureThreshold+1; i++ {
			reconcile(r, ns, name)
		}
	}

	It("spec 5: inert in Paused, the P5f delivery hold and terminal phases (the BudgetHold condition is not set)", func() {
		By("Paused: the gate is inert (the P2f decision is re-evaluated on resume)")
		ns := nsFor("d77-s5")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newReconciler(recorder)
		loop := createLoop(ns, "d77-s5", nil)
		primeProxy(r, loop)
		reconcile(r, ns, "d77-s5")
		l := getLoop(ns, "d77-s5")
		l.Spec.Suspend = true
		Expect(k8sClient.Update(ctx, l)).To(Succeed())
		reconcileStale(r, ns, "d77-s5")
		l = getLoop(ns, "d77-s5")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused), "suspend=true -> Paused")
		Expect(cond(l, coxv1alpha1.BudgetReadingStaleCondition).Status).To(Equal(metav1.ConditionTrue),
			"the stale reading is still SURFACED while paused (the surface is not inert)")
		Expect(cond(l, coxv1alpha1.BudgetHoldCondition)).NotTo(BeTrue(),
			"the gate is inert in Paused: the decision is re-evaluated on resume (P2f; absent or False)")

		By("P5f delivery hold: the gate is inert (it would lose the delivery result)")
		ns2 := nsFor("d77-s5b")
		defer deleteNS(ctx, ns2)
		recorder2 := record.NewFakeRecorder(64)
		r2 := newReconciler(recorder2)
		loop2 := createLoop(ns2, "d77-s5b", nil)
		primeProxy(r2, loop2)
		reconcile(r2, ns2, "d77-s5b")
		l = getLoop(ns2, "d77-s5b")
		l.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		l.Status.DesiredPhase = coxv1alpha1.LoopPhaseVerifying
		l.Status.Conditions = append(l.Status.Conditions, metav1.Condition{
			Type: string(coxv1alpha1.LoopPhaseVerifying), Status: metav1.ConditionTrue,
			Reason: coxv1alpha1.ReasonVerifyingDelivering, Message: "delivering", LastTransitionTime: metav1.Now(),
		})
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
		reconcileStale(r2, ns2, "d77-s5b")
		l = getLoop(ns2, "d77-s5b")
		Expect(cond(l, coxv1alpha1.BudgetReadingStaleCondition).Status).To(Equal(metav1.ConditionTrue),
			"the stale reading is still surfaced in the delivery hold")
		Expect(loopHoldingForDelivery(l)).To(BeTrue(), "fixture: the P5f delivery hold is active")
		Expect(cond(l, coxv1alpha1.BudgetHoldCondition)).NotTo(BeTrue(),
			"the gate is inert in the P5f delivery hold: it would lose the delivery result (absent or False)")

		By("terminal phases (Succeeded): the gate is inert (a finished Loop is never held)")
		ns3 := nsFor("d77-s5c")
		defer deleteNS(ctx, ns3)
		recorder3 := record.NewFakeRecorder(64)
		r3 := newReconciler(recorder3)
		loop3 := createLoop(ns3, "d77-s5c", nil)
		primeProxy(r3, loop3)
		reconcile(r3, ns3, "d77-s5c")
		l = getLoop(ns3, "d77-s5c")
		l.Status.Phase = coxv1alpha1.LoopPhaseSucceeded
		l.Status.DesiredPhase = coxv1alpha1.LoopPhaseSucceeded
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
		reconcileStale(r3, ns3, "d77-s5c")
		l = getLoop(ns3, "d77-s5c")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded), "terminal is not touched")
		Expect(cond(l, coxv1alpha1.BudgetHoldCondition)).NotTo(BeTrue(), "the gate is inert in a terminal phase (absent or False)")
	})
})
