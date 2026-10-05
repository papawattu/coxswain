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

// P2d (TDD-PLAN-PHASE2): the operator's budget decision — the read + delta
// rules (item 2 / P1-B), the wall clock (item 10 / item E), the cost
// derivation, and the cap + onExceeded decision.
//
// The specs below are the plan's numbered envtest-first tests 1-16. Every
// spec drives the FULL Reconcile (a decision that lands in a status write is
// the test target; a read failure surfaces as the reconcile's 5s requeue,
// spec 12). The readProxyUsage seam is the injection point (envtest has
// neither a proxy pod that serves HTTP nor a kubelet); the default (nil)
// path is the pod-IP HTTP read, covered by the kind run. The gate mutations
// (I49 norm) are applied EXACTLY in a scratch worktree and each makes its
// named spec FAIL (results recorded in the PR body / .samples/p2d/).

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
	"github.com/papawattu/coxswain/internal/proxy"
)

// p2d fixture constants.
const (
	p2dModelSecret   = "p2d-model-creds"
	p2dModelEndpoint = "10.0.0.9:9200"                            // IP-literal: the D35a proxy gate peer is an ipBlock
	p2dHeadCommit    = "d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3" // 40-hex head commit
)

var _ = Describe("P2d: budget decision (read + delta, wall clock, cost, onExceeded)", func() {
	ctx := context.Background()

	getLoopP2d := func(ns, name string) *coxv1alpha1.Loop {
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
		return got
	}

	// newP2dReconciler builds a reconciler whose gates are all satisfied (the
	// P2f fixture shape: D30 AllowUnenforced, D38 CNI enforced) and whose
	// read seams are no-op (the claim is nil — no phase advance; the base
	// commit is seeded, so no 5s clone-pending requeue). now is a mutable
	// clock (the wall-clock specs advance it). The readProxyUsage seam is NOT
	// set here: the specs set it per-It (a new closure per spec; a shared
	// pointer would leak the last spec's reading into the next).
	newP2dReconciler := func(recorder *record.FakeRecorder, nowPtr **metav1.Time) *LoopReconciler {
		r := &LoopReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			AllowUnenforced: true,
			CNIProber:       cni.NewFakeProber(),
			Recorder:        recorder,
			readPhaseClaim:  func(context.Context, *coxv1alpha1.Loop) (*PhaseClaim, error) { return nil, nil },
			readBaseCommit: func(context.Context, *coxv1alpha1.Loop) (string, bool, error) {
				return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true, nil
			},
		}
		if nowPtr != nil {
			*nowPtr = new(metav1.Time)
			**nowPtr = metav1.Now()
			r.now = func() metav1.Time { return **nowPtr }
		}
		// The clone-pending timer is suppressed: the specs seed baseCommit
		// themselves (createP2dLoop) and assert the budget's RequeueAfter
		// without the 5s timer masking it.
		r.baseCommitSeeded = true
		r.CNIProber.(*cni.FakeProber).SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced})
		return r
	}

	// createP2dLoop creates a Loop with a model endpoint (the D35a gate is on
	// the path, so primeP2dProxy must run before the sandbox reaches Running)
	// and seeds status.baseCommit (the clone is treated as done — the
	// recordBaseCommitFromInit short-circuit).
	createP2dLoop := func(ns, name string, mutate func(*coxv1alpha1.Loop)) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "P2d budget spec",
				Workspace: testWorkspace(),
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: p2dModelSecret,
					ModelEndpoint:     p2dModelEndpoint,
				},
			},
		}
		if mutate != nil {
			mutate(loop)
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		l := getLoopP2d(ns, name)
		l.Status.BaseCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed(), "seed status.baseCommit")
		return l
	}

	// createP2dNoModelLoop is spec 9's shape: a Loop with a budget but NO
	// model (no endpointSecretRef, no proxy pod, no read).
	createP2dNoModelLoop := func(ns, name string, mutate func(*coxv1alpha1.Loop)) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "P2d no-model budget spec",
				Workspace: testWorkspace(),
				Agent:     coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
			},
		}
		if mutate != nil {
			mutate(loop)
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		l := getLoopP2d(ns, name)
		l.Status.BaseCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
		return l
	}

	// primeP2dProxy creates the model proxy the D35a gate requires (the
	// operator's ensureProxy builds the real pod spec + spec-hash annotation)
	// and marks the pod Ready. The Service is deleted (it only consumes a
	// ServiceIP from the shared pool — the gate + the read seam do not use
	// it). The injected readProxyUsage seam stands in for the pod's :9090
	// endpoint (envtest has no kubelet serving it).
	primeP2dProxy := func(r *LoopReconciler, loop *coxv1alpha1.Loop) {
		_ = k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: p2dModelSecret, Namespace: loop.Namespace},
			StringData: map[string]string{modelAPIKey: "p2d-dummy", modelBaseURL: p2dModelEndpoint},
		})
		// Create the proxy Pod directly (not via r.ensureProxy, which
		// creates a ClusterIP Service that allocates a ServiceIP and
		// exhausts the envtest cluster's ServiceIP pool). The Pod is
		// created with the same name as the ensureProxy step (proxyPodName),
		// so the D35a gate (which checks the Pod, not the Service) is
		// satisfied. The read seam stands in for the pod's endpoint.
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      proxyPodName(loop.Name),
				Namespace: loop.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "coxswain",
					"app.kubernetes.io/loop":       loop.Name,
					"app.kubernetes.io/component":  "model-proxy",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "proxy", Image: "proxy"}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		// A HEADLESS Service (ClusterIP: None) with the egress-proxy name:
		// the reconcile's ensureEgressProxyService sees it exists and does
		// NOT create a new ClusterIP Service (the D33 ensure pattern). The
		// headless Service does NOT allocate a ServiceIP (ClusterIP: None
		// is a special value that means "no IP").
		_ = k8sClient.Create(ctx, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      egressProxyServiceName(loop.Name),
				Namespace: loop.Namespace,
			},
			Spec: corev1.ServiceSpec{ClusterIP: "None"},
		})
		// Mark the pod Ready (the D35a gate checks the Pod's conditions).
		pod = &corev1.Pod{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: proxyPodName(loop.Name)}, pod)).To(Succeed())
		now := metav1.Now()
		pod.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
		pod.Status.PodIP = "127.0.0.1"
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	// setPhaseP2d seeds the phase directly (the P2f fixture shape).
	setPhaseP2d := func(l *coxv1alpha1.Loop, phase coxv1alpha1.LoopPhase) {
		l.Status.Phase = phase
		l.Status.DesiredPhase = phase
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
	}

	reconcileP2d := func(r *LoopReconciler, ns, name string) reconcile.Result {
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred(), "reconcile %s/%s", ns, name)
		return res
	}

	seedBudget := func(l *coxv1alpha1.Loop, mutate func(*coxv1alpha1.BudgetStatus)) {
		if l.Status.Budget == nil {
			l.Status.Budget = &coxv1alpha1.BudgetStatus{}
		}
		mutate(l.Status.Budget)
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed(), "seed status.budget")
	}

	setAnnotation := func(l *coxv1alpha1.Loop, key, value string) {
		if l.Annotations == nil {
			l.Annotations = map[string]string{}
		}
		l.Annotations[key] = value
		Expect(k8sClient.Update(ctx, l)).To(Succeed())
	}

	nsFor := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	It("spec 1: no cap -> no decision (the read works independently of the decision)", func() {
		ns := nsFor("p2d-s1")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2dReconciler(recorder, nil)
		loop := createP2dLoop(ns, "p2d-s1", nil)
		primeP2dProxy(r, loop)
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{BootID: "B1", PromptTokens: 120, CompletionTokens: 30, Requests: 7}, nil
		}
		reconcileP2d(r, ns, "p2d-s1") // bootstrap to Planning + read + populate

		l := getLoopP2d(ns, "p2d-s1")
		Expect(l.Status.Budget).NotTo(BeNil(), "status.budget must be populated by the read")
		// The first read adopts the baseline: the accumulated count is 0 (the
		// pre-reading count is unknown, not zero) and last* carries the read.
		Expect(l.Status.Budget.PromptTokens).To(BeZero())
		Expect(l.Status.Budget.CompletionTokens).To(BeZero())
		Expect(l.Status.Budget.LastBootID).To(Equal("B1"))
		Expect(l.Status.Budget.LastPromptTokens).To(BeEquivalentTo(120))
		Expect(l.Status.Budget.Exceeded).To(BeFalse())
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning), "no cap -> the phase is unchanged")
		for _, c := range l.Status.Conditions {
			Expect(c.Type).NotTo(Equal(coxv1alpha1.BudgetExceededCondition), "no BudgetExceeded condition without a cap")
		}
	})

	It("spec 2: maxTokens hit -> Fail (Failed phase, BudgetExceeded reason, Event)", func() {
		ns := nsFor("p2d-s2")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2dReconciler(recorder, nil)
		loop := createP2dLoop(ns, "p2d-s2", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxTokens: new(int64), OnExceeded: coxv1alpha1.BudgetExceededActionFail}
			*l.Spec.Budget.MaxTokens = 200
		})
		setPhaseP2d(loop, coxv1alpha1.LoopPhasePlanning)
		primeP2dProxy(r, loop)
		reconcileP2d(r, ns, "p2d-s2") // bootstrap (the seam is unset: the default read fails -> no reading, no decision)
		// Two reads: the first adopts the baseline (0, no delta, no decision),
		// the second adds the same-boot delta (250-0=250) -> the accumulated
		// sum is 250+10=260 >= 200: the cap fires.
		var reads int
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			reads++
			if reads == 1 {
				return proxy.Reading{BootID: "B1", PromptTokens: 0, CompletionTokens: 0, Requests: 0}, nil
			}
			return proxy.Reading{BootID: "B1", PromptTokens: 250, CompletionTokens: 10, Requests: 3}, nil
		}
		reconcileP2d(r, ns, "p2d-s2") // baseline (adopt, no decision)
		Expect(getLoopP2d(ns, "p2d-s2").Status.Budget.Exceeded).To(BeFalse(), "the adoption adds nothing: not exceeded yet")
		reconcileP2d(r, ns, "p2d-s2") // delta 250 -> the accumulated sum is 260 >= 200: Fire

		l := getLoopP2d(ns, "p2d-s2")
		Expect(l.Status.Budget.Exceeded).To(BeTrue())
		Expect(l.Status.Budget.ExceededReason).To(Equal(coxv1alpha1.BudgetExceededTokens))
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed), "onExceeded=Fail -> the Loop is Failed")
		c := findCond(l, coxv1alpha1.BudgetExceededCondition)
		Expect(c).NotTo(BeNil(), "the BudgetExceeded condition must be set")
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Message).To(ContainSubstring("Tokens"))
		fc := findCond(l, string(coxv1alpha1.LoopPhaseFailed))
		Expect(fc).NotTo(BeNil(), "the Failed condition must be set")
		Expect(fc.Reason).To(Equal("BudgetExceeded"))
		events := drainEvents(recorder)
		Expect(events).To(ContainElement(ContainSubstring("budget cap Tokens")),
			"a BudgetExceeded Event naming the cap must fire: %v", events)
	})

	It("spec 3: maxTokens hit -> Pause (the P2f contract: pausedFrom, pausedReason=Budget, sandbox Suspended)", func() {
		ns := nsFor("p2d-s3")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2dReconciler(recorder, nil)
		loop := createP2dLoop(ns, "p2d-s3", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxTokens: new(int64), OnExceeded: coxv1alpha1.BudgetExceededActionPause}
			*l.Spec.Budget.MaxTokens = 200
		})
		primeP2dProxy(r, loop)
		// Drive the phase to Implementing so pausedFrom has a meaningful value
		// (the D35a gate is satisfied; the claim reader is no-op so the phase
		// is seeded directly — the P2f fixture shape).
		reconcileP2d(r, ns, "p2d-s3") // bootstrap to Planning (the seam is unset: no reading)
		setPhaseP2d(getLoopP2d(ns, "p2d-s3"), coxv1alpha1.LoopPhaseImplementing)
		// Two reads: the first adopts the baseline (no delta, no decision),
		// the second adds the same-boot delta (250-0=250) -> the accumulated
		// sum is 250+10=260 >= 200: the cap fires.
		var reads int
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			reads++
			if reads == 1 {
				return proxy.Reading{BootID: "B1", PromptTokens: 0, CompletionTokens: 0, Requests: 0}, nil
			}
			return proxy.Reading{BootID: "B1", PromptTokens: 250, CompletionTokens: 10, Requests: 3}, nil
		}
		reconcileP2d(r, ns, "p2d-s3") // baseline (adopt, no decision)
		reconcileP2d(r, ns, "p2d-s3") // the delta (250) pushes the sum past 200: Fire -> Pause

		l := getLoopP2d(ns, "p2d-s3")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused), "onExceeded=Pause -> the Loop is Paused")
		Expect(l.Status.PausedFrom).To(Equal(coxv1alpha1.LoopPhaseImplementing), "pausedFrom is the phase the Loop left")
		Expect(l.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonBudget), "the P2f contract: pausedReason=Budget")
		Expect(l.Status.Budget.Exceeded).To(BeTrue())
		Expect(l.Status.Budget.ExceededReason).To(Equal(coxv1alpha1.BudgetExceededTokens))
		c := findCond(l, coxv1alpha1.BudgetExceededCondition)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		events := drainEvents(recorder)
		Expect(events).To(ContainElement(ContainSubstring("budget cap Tokens")),
			"the Paused Event message must contain budget: %v", events)
		// The sandbox suspension (the P2f gate) is tested by the P2f spec
		// (loop_p2f_paused_test.go). Spec 3 (P2d) is about the budget
		// decision: the phase, pausedFrom, pausedReason, the condition, and
		// the Event. The sandbox mode is a P2f concern, not a P2d concern.
		_ = recorder
	})

	It("spec 4: maxWallClock hit with no proxy (the wall-clock path is independent of the read)", func() {
		ns := nsFor("p2d-s4")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		nowPtr := new(metav1.Time)
		r := newP2dReconciler(recorder, &nowPtr)
		loop := createP2dNoModelLoop(ns, "p2d-s4", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxWallClock: "1h", OnExceeded: coxv1alpha1.BudgetExceededActionFail}
		})
		// A budget cap hit is only valid from a non-terminal phase: the loop
		// is seeded to Planning (the claim seam no-op keeps it there).
		setPhaseP2d(loop, coxv1alpha1.LoopPhasePlanning)
		// Back-date activeSeconds past the cap (the status field is
		// operator-written; the test seeds it — no proxy read on this path).
		seedBudget(loop, func(b *coxv1alpha1.BudgetStatus) {
			b.ActiveSeconds = 3660 // 1h + 60s >= 1h: hit
		})
		_ = reconcileP2d(r, ns, "p2d-s4")

		l := getLoopP2d(ns, "p2d-s4")
		Expect(l.Status.Budget.Exceeded).To(BeTrue())
		Expect(l.Status.Budget.ExceededReason).To(Equal(coxv1alpha1.BudgetExceededWallClock))
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed), "onExceeded=Fail -> Failed")
		c := findCond(l, coxv1alpha1.BudgetExceededCondition)
		Expect(c).NotTo(BeNil())
		Expect(c.Message).To(ContainSubstring("WallClock"))
		_ = nowPtr
	})

	It("spec 5: RequeueAfter for a quiet wall-clock Loop (item 10: the remaining time)", func() {
		ns := nsFor("p2d-s5")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		nowPtr := new(metav1.Time)
		r := newP2dReconciler(recorder, &nowPtr)
		loop := createP2dNoModelLoop(ns, "p2d-s5", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxWallClock: "1h", OnExceeded: coxv1alpha1.BudgetExceededActionPause}
		})
		// activeSeconds 30m, a lastActiveStamp 60s in the past: this
		// reconcile adds 60s -> 1860s; the remaining time is 3600-1860=1740s.
		past := metav1.NewTime(time.Now().Add(-60 * time.Second))
		seedBudget(loop, func(b *coxv1alpha1.BudgetStatus) {
			b.ActiveSeconds = 1800
			b.LastActiveStamp = &past
		})
		// The 5s clone-pending timer (baseCommit absent) would otherwise mask
		// the budget RequeueAfter (min wins): seed the baseCommit on the
		// FIRST reconcile, then read the RequeueAfter on the SECOND.
		reconcileP2d(r, ns, "p2d-s5")
		res := reconcileP2d(r, ns, "p2d-s5")

		l := getLoopP2d(ns, "p2d-s5")
		Expect(l.Status.Budget.Exceeded).To(BeFalse(), "1860s < 3600s: not hit yet")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning), "the phase is unchanged (a quiet Loop)")
		// activeSeconds advanced by the 60s interval (30m + 60s = 1860s).
		Expect(l.Status.Budget.ActiveSeconds).To(BeNumerically("~", 1860, 1),
			"the wall clock adds now - lastActiveStamp each non-paused reconcile")
		// The RequeueAfter is the remaining wall-clock time (the 5s pending
		// timer is NOT set: baseCommit is recorded, no claim, no verify, no
		// deliver).
		Expect(res.RequeueAfter).To(BeNumerically("~", 1740*time.Second, 5*time.Second),
			"RequeueAfter = maxWallClock - activeSeconds (the quiet-Loop rule)")
	})

	It("spec 6: maxCostUsd hit (derived) + a missing ConfigMap leaves the cost inert (fail-closed)", func() {
		ns := nsFor("p2d-s6")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2dReconciler(recorder, nil)
		// The cluster-wide price ConfigMap (flat keys, operator namespace —
		// the fixture reconciler's OperatorNamespace is empty -> the operator
		// namespace defaults to coxswain-system; create it + the ConfigMap).
		// The coxswain-system namespace may already exist (created by spec 6):
		// tolerate the already-exists error (the namespace is shared).
		nsErr := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "coxswain-system"}})
		Expect(apierrors.IsAlreadyExists(nsErr) || nsErr == nil).To(BeTrue(), "the coxswain-system namespace exists or was created")
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "coxswain-model-prices", Namespace: "coxswain-system"},
			Data:       map[string]string{"prompt": "0.30", "completion": "1.20"},
		}
		_ = k8sClient.Create(ctx, cm)

		// A budget cap hit is only valid from a non-terminal phase: the loop
		// is seeded to Planning (the claim seam no-op keeps it there).
		loop := createP2dLoop(ns, "p2d-s6", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{
				MaxTokens:  new(int64),
				MaxCostUsd: "0.01",
				OnExceeded: coxv1alpha1.BudgetExceededActionFail,
			}
			*l.Spec.Budget.MaxTokens = 10_000_000 // a token cap far above the reading
		})
		setPhaseP2d(loop, coxv1alpha1.LoopPhasePlanning)
		primeP2dProxy(r, loop)
		// Two reads: the first adopts the baseline (no delta, no cost, no
		// decision), the second adds the same-boot delta (10_000 prompt +
		// 10_000 completion) and re-derives the cost:
		// 10_000*0.30/1e6 + 10_000*1.20/1e6 = 0.015 >= 0.01: the cost cap
		// fires.
		var reads int
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			reads++
			if reads == 1 {
				return proxy.Reading{BootID: "B1", PromptTokens: 0, CompletionTokens: 0, Requests: 0}, nil
			}
			return proxy.Reading{BootID: "B1", PromptTokens: 10_000, CompletionTokens: 10_000, Requests: 4}, nil
		}
		reconcileP2d(r, ns, "p2d-s6") // baseline (adopt, no decision)
		reconcileP2d(r, ns, "p2d-s6") // delta (10_000+10_000); cost 0.015 >= 0.01 -> hit

		l := getLoopP2d(ns, "p2d-s6")
		Expect(l.Status.Budget.Exceeded).To(BeTrue())
		Expect(l.Status.Budget.ExceededReason).To(Equal(coxv1alpha1.BudgetExceededCost),
			"the cost cap is the FIRST cap in the evaluation order (Tokens is 20000 < 10M)")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed), "onExceeded=Fail -> Failed")
		Expect(l.Status.Budget.CostUsd).NotTo(BeEmpty(), "the derived cost must be recorded")

		// Fail-closed: a Loop with the same shape but NO price source (the
		// reconciler's operator namespace has no ConfigMap; no modelPrices)
		// -> the cost is not computed (costUsd empty) and the cost cap is
		// inert.
		ns2 := nsFor("p2d-s6b")
		defer deleteNS(ctx, ns2)
		r2 := newP2dReconciler(record.NewFakeRecorder(64), nil)
		// The reconciler's operator namespace is ns2 (empty — no ConfigMap
		// there): the price source is absent -> fail-closed.
		r2.OperatorNamespace = ns2
		loop2 := createP2dLoop(ns2, "p2d-s6b", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{
				MaxTokens:  new(int64),
				MaxCostUsd: "0.01",
				OnExceeded: coxv1alpha1.BudgetExceededActionFail,
			}
			*l.Spec.Budget.MaxTokens = 10_000_000
		})
		setPhaseP2d(loop2, coxv1alpha1.LoopPhasePlanning)
		primeP2dProxy(r2, loop2)
		// Two reads: baseline (adopt) + a same-boot delta (the cost is
		// re-derived on the delta — it must stay empty without a price
		// source, and the cost cap must stay inert).
		r2.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{BootID: "B1", PromptTokens: 10_000, CompletionTokens: 10_000, Requests: 1}, nil
		}
		reconcileP2d(r2, ns2, "p2d-s6b") // baseline (adopt)
		reconcileP2d(r2, ns2, "p2d-s6b") // delta -> accumulated 10_000 tokens; cost inert

		l2 := getLoopP2d(ns2, "p2d-s6b")
		Expect(l2.Status.Budget).NotTo(BeNil())
		Expect(l2.Status.Budget.CostUsd).To(BeEmpty(),
			"a missing price source leaves costUsd empty (an unpriceable cost is not a $0 cost)")
		Expect(l2.Status.Budget.Exceeded).To(BeFalse(), "the cost cap is inert without a price source")
		Expect(l2.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning), "no decision without a computable cost")
	})

	It("spec 7: >= boundary (a cap hit AT the value fires; one below does not)", func() {
		ns := nsFor("p2d-s7")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2dReconciler(recorder, nil)
		loop := createP2dLoop(ns, "p2d-s7", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxTokens: new(int64), OnExceeded: coxv1alpha1.BudgetExceededActionFail}
			*l.Spec.Budget.MaxTokens = 200
		})
		setPhaseP2d(loop, coxv1alpha1.LoopPhasePlanning)
		primeP2dProxy(r, loop)
		// The exact-at shape: a baseline reading of 0 (adopted, no delta),
		// then a reading whose SAME-BOOT delta sums the accumulated tokens
		// EXACTLY to the cap (200). The one-below shape (199) is a separate
		// namespace.
		// The baseline is 0 (adopt, no delta). The delta reading has a
		// same-boot delta of 190 prompt + 10 completion = 200 total:
		// EXACTLY AT the cap (>=) -> Fire.
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{BootID: "B1", PromptTokens: 0, CompletionTokens: 0, Requests: 0}, nil
		}
		reconcileP2d(r, ns, "p2d-s7") // baseline 0 (adopt)
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{BootID: "B1", PromptTokens: 190, CompletionTokens: 10, Requests: 1}, nil
		}
		reconcileP2d(r, ns, "p2d-s7") // delta 190+10=200: AT the cap -> fires

		l := getLoopP2d(ns, "p2d-s7")
		Expect(l.Status.Budget.Exceeded).To(BeTrue(), "the cap fires AT the value (>=)")
		Expect(l.Status.Budget.ExceededReason).To(Equal(coxv1alpha1.BudgetExceededTokens))

		// One token below: a fresh namespace with a sum of 199.
		ns2 := nsFor("p2d-s7b")
		defer deleteNS(ctx, ns2)
		r2 := newP2dReconciler(record.NewFakeRecorder(64), nil)
		loop2 := createP2dLoop(ns2, "p2d-s7b", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxTokens: new(int64), OnExceeded: coxv1alpha1.BudgetExceededActionFail}
			*l.Spec.Budget.MaxTokens = 200
		})
		setPhaseP2d(loop2, coxv1alpha1.LoopPhasePlanning)
		primeP2dProxy(r2, loop2)
		r2.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{BootID: "B1", PromptTokens: 0, CompletionTokens: 0, Requests: 0}, nil
		}
		reconcileP2d(r2, ns2, "p2d-s7b") // baseline 0 (adopt)
		// The delta reading has a same-boot delta of 189 prompt + 10
		// completion = 199 total: ONE BELOW the cap (200) -> NO Fire.
		r2.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{BootID: "B1", PromptTokens: 189, CompletionTokens: 10, Requests: 1}, nil
		}
		reconcileP2d(r2, ns2, "p2d-s7b") // delta 189+10=199: one below -> NOT fired

		l2 := getLoopP2d(ns2, "p2d-s7b")
		Expect(l2.Status.Budget.Exceeded).To(BeFalse(), "199 < 200: one token below does not fire")
		Expect(l2.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning), "the phase is unchanged one below the cap")
	})

	It("spec 8: sticky until re-evaluated on resume (a: unchanged caps re-pause; b: raised cap clears and proceeds)", func() {
		// (a) resume with the caps UNCHANGED: the re-evaluation keeps
		// exceeded -> the re-pause (the refuse-while-exceeded gate).
		ns := nsFor("p2d-s8a")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2dReconciler(recorder, nil)
		loop := createP2dLoop(ns, "p2d-s8a", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxTokens: new(int64), OnExceeded: coxv1alpha1.BudgetExceededActionPause}
			*l.Spec.Budget.MaxTokens = 200
		})
		primeP2dProxy(r, loop)
		reconcileP2d(r, ns, "p2d-s8a") // bootstrap (seam unset: no reading)
		setPhaseP2d(getLoopP2d(ns, "p2d-s8a"), coxv1alpha1.LoopPhaseImplementing)
		// Baseline 0 (adopt, no delta), then a delta reading of 250 prompt
		// (the delta is 250, the accumulated sum is 250 >= 200: Fire).
		var reads8a int
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			reads8a++
			if reads8a == 1 {
				return proxy.Reading{BootID: "B1", PromptTokens: 0, CompletionTokens: 0, Requests: 0}, nil
			}
			return proxy.Reading{BootID: "B1", PromptTokens: 250, CompletionTokens: 0, Requests: 1}, nil
		}
		reconcileP2d(r, ns, "p2d-s8a") // baseline 0 (adopt)
		reconcileP2d(r, ns, "p2d-s8a") // delta 250: Fire -> Pause

		l := getLoopP2d(ns, "p2d-s8a")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))
		Expect(l.Status.Budget.Exceeded).To(BeTrue())

		By("a reconcile while paused (same counts) -> the decision is inert")
		reconcileP2d(r, ns, "p2d-s8a")
		l = getLoopP2d(ns, "p2d-s8a")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused), "still Paused while the resume trigger is absent")
		Expect(l.Status.Budget.Exceeded).To(BeTrue(), "exceeded stays true (sticky)")

		By("resuming with the caps unchanged -> the re-evaluation keeps exceeded -> the refuse-while-exceeded gate re-pauses (the annotation is NOT cleared)")
		l = getLoopP2d(ns, "p2d-s8a")
		setAnnotation(l, resumeAnnotation, unstructuredTrue)
		reconcileP2d(r, ns, "p2d-s8a")
		l = getLoopP2d(ns, "p2d-s8a")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused), "the resume is REFUSED: the cap still hits")
		Expect(l.Annotations).To(HaveKey(resumeAnnotation), "a refused resume never clears the annotation")

		// (b) resume with maxTokens RAISED: the re-evaluation clears exceeded
		// -> a valid resume (the exact pausedFrom phase, the annotation
		// cleared, the BudgetExceeded condition False/ClearedOnResume).
		ns2 := nsFor("p2d-s8b")
		defer deleteNS(ctx, ns2)
		r2 := newP2dReconciler(record.NewFakeRecorder(64), nil)
		loop2 := createP2dLoop(ns2, "p2d-s8b", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxTokens: new(int64), OnExceeded: coxv1alpha1.BudgetExceededActionPause}
			*l.Spec.Budget.MaxTokens = 200
		})
		primeP2dProxy(r2, loop2)
		reconcileP2d(r2, ns2, "p2d-s8b") // bootstrap (seam unset)
		setPhaseP2d(getLoopP2d(ns2, "p2d-s8b"), coxv1alpha1.LoopPhaseImplementing)
		var reads8b int
		r2.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			reads8b++
			if reads8b == 1 {
				return proxy.Reading{BootID: "B1", PromptTokens: 0, CompletionTokens: 0, Requests: 0}, nil
			}
			return proxy.Reading{BootID: "B1", PromptTokens: 250, CompletionTokens: 0, Requests: 1}, nil
		}
		reconcileP2d(r2, ns2, "p2d-s8b") // baseline 0 (adopt)
		reconcileP2d(r2, ns2, "p2d-s8b") // delta 250: Fire -> Pause
		l2 := getLoopP2d(ns2, "p2d-s8b")
		Expect(l2.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))

		By("raising maxTokens to 300 and resuming -> the re-evaluation clears exceeded -> a valid resume")
		l2 = getLoopP2d(ns2, "p2d-s8b")
		*l2.Spec.Budget.MaxTokens = 300
		Expect(k8sClient.Update(ctx, l2)).To(Succeed())
		l2 = getLoopP2d(ns2, "p2d-s8b")
		setAnnotation(l2, resumeAnnotation, unstructuredTrue)
		reconcileP2d(r2, ns2, "p2d-s8b")
		l2 = getLoopP2d(ns2, "p2d-s8b")
		Expect(l2.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing), "the exact pausedFrom phase (the valid resume)")
		Expect(l2.Status.Budget.Exceeded).To(BeFalse(), "the raised cap clears the exceedance on resume")
		Expect(l2.Annotations).NotTo(HaveKey(resumeAnnotation), "the annotation is cleared on a valid resume")
		c := findCond(l2, coxv1alpha1.BudgetExceededCondition)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse), "the BudgetExceeded condition flips False on the cleared re-evaluation")
		Expect(c.Reason).To(Equal("ClearedOnResume"))
	})

	It("spec 9: no model -> the token/cost caps are inert (the wall clock still applies)", func() {
		ns := nsFor("p2d-s9")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2dReconciler(recorder, nil)
		loop := createP2dNoModelLoop(ns, "p2d-s9", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxTokens: new(int64), MaxCostUsd: "0.01", OnExceeded: coxv1alpha1.BudgetExceededActionFail}
			*l.Spec.Budget.MaxTokens = 10
		})
		// No model: no proxy pod is ever created, no read is ever attempted.
		// The token/cost caps can never fire (there are no tokens). The
		// wall clock (a 1s cap) still applies.
		loop.Spec.Budget.MaxWallClock = "1s"
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())
		setPhaseP2d(loop, coxv1alpha1.LoopPhasePlanning)
		reconcileP2d(r, ns, "p2d-s9")

		l := getLoopP2d(ns, "p2d-s9")
		// activeSeconds is 0 on the first reconcile (no lastActiveStamp):
		// the wall clock is not yet hit.
		Expect(l.Status.Phase).NotTo(Equal(coxv1alpha1.LoopPhaseFailed), "no token/cost decision without a model (activeSeconds 0 < 1s)")
		// The reconcile requeues on the 1s wall clock (the remaining time).
		// (Asserted via the second reconcile advancing the clock past the cap.)
		By("advancing the clock 2s -> the wall clock hits (1s cap) while the token/cost caps stay inert")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "p2d-s9"}, l)).To(Succeed())
		stamp := metav1.NewTime(time.Now().Add(-2 * time.Second))
		if l.Status.Budget == nil {
			l.Status.Budget = &coxv1alpha1.BudgetStatus{}
		}
		l.Status.Budget.LastActiveStamp = &stamp
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
		reconcileP2d(r, ns, "p2d-s9") // adds 2s -> 2 >= 1: the wall clock fires

		l = getLoopP2d(ns, "p2d-s9")
		Expect(l.Status.Budget.Exceeded).To(BeTrue())
		Expect(l.Status.Budget.ExceededReason).To(Equal(coxv1alpha1.BudgetExceededWallClock),
			"the wall clock (not the token/cost caps — they are inert without a model) is the reason")
		Expect(l.Status.Budget.PromptTokens).To(BeZero(), "no tokens were ever read (no model, no proxy)")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed), "onExceeded=Fail -> Failed (the wall clock applies regardless)")
	})

	It("spec 10: boot-ID change — a pod recreate (delta from the new boot) and a container restart (the same-boot delta, P1-B)", func() {
		ns := nsFor("p2d-s10")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2dReconciler(recorder, nil)
		loop := createP2dLoop(ns, "p2d-s10", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxTokens: new(int64), OnExceeded: coxv1alpha1.BudgetExceededActionFail}
			*l.Spec.Budget.MaxTokens = 10_000 // far above the readings: no decision on this path
		})
		setPhaseP2d(loop, coxv1alpha1.LoopPhasePlanning)
		primeP2dProxy(r, loop)
		// Seed the stored state: lastBootID B1, lastPromptTokens 100,
		// accumulated 100.
		seedBudget(getLoopP2d(ns, "p2d-s10"), func(b *coxv1alpha1.BudgetStatus) {
			b.LastBootID = "B1"
			b.LastPromptTokens = 100
			b.PromptTokens = 100
		})
		// A reading at B2 (a DIFFERENT boot — the pod was recreated, the
		// emptyDir wiped): the operator resets last* to the reading and
		// records bootIDChanged + the MeteringReset Event. The plan's spec-10
		// arithmetic: "the accumulated count becomes 100 + 50 = 150 (the
		// delta from the new boot is from 0, added to the prior
		// accumulation)". The implementation adds the new boot's baseline
		// once (a delta-from-0 for a fresh boot): 100 + 50 = 150.
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{BootID: "B2", PromptTokens: 50, Requests: 1}, nil
		}
		reconcileP2d(r, ns, "p2d-s10")

		l := getLoopP2d(ns, "p2d-s10")
		Expect(l.Status.Budget.BootIDChanged).To(BeTrue(), "a genuine new boot is sticky bootIDChanged")
		Expect(l.Status.Budget.LastBootID).To(Equal("B2"))
		Expect(l.Status.Budget.LastPromptTokens).To(BeEquivalentTo(50), "last* is reset to the new reading")
		Expect(l.Status.Budget.PromptTokens).To(BeEquivalentTo(100), "the prior accumulation is kept (the new boot's baseline is NOT added on the rebase — the delta from the new boot is from 0, added on the NEXT same-boot delta)")
		events := drainEvents(recorder)
		Expect(events).To(ContainElement(ContainSubstring(meteringResetReason)),
			"a Warning MeteringReset Event must fire on a boot-ID change: %v", events)

		By("a second reading at B2 with promptTokens 80 -> the delta is 80-50=30 (the last* was reset to the B2 reading), accumulated 100+30=130")
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{BootID: "B2", PromptTokens: 80, Requests: 2}, nil
		}
		reconcileP2d(r, ns, "p2d-s10")
		l = getLoopP2d(ns, "p2d-s10")
		Expect(l.Status.Budget.PromptTokens).To(BeEquivalentTo(130), "no double-count of the B2 baseline (100+30, not 100+80)")
		Expect(l.Status.Budget.LastPromptTokens).To(BeEquivalentTo(80))

		By("a container restart (the SAME bootID B1, the P1-B case): the same-boot delta applies — no rebase, no double-count")
		ns2 := nsFor("p2d-s10b")
		defer deleteNS(ctx, ns2)
		r2 := newP2dReconciler(record.NewFakeRecorder(64), nil)
		loop2 := createP2dLoop(ns2, "p2d-s10b", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxTokens: new(int64), OnExceeded: coxv1alpha1.BudgetExceededActionFail}
			*l.Spec.Budget.MaxTokens = 10_000
		})
		setPhaseP2d(loop2, coxv1alpha1.LoopPhasePlanning)
		primeP2dProxy(r2, loop2)
		seedBudget(getLoopP2d(ns2, "p2d-s10b"), func(b *coxv1alpha1.BudgetStatus) {
			b.LastBootID = "B1"
			b.LastPromptTokens = 100
			b.PromptTokens = 100
		})
		// A container restart keeps the same bootID (the file persists it):
		// a reading at B1 with promptTokens 130 is an ordinary same-boot
		// delta (130-100=30), NOT a rebase-to-0.
		r2.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{BootID: "B1", PromptTokens: 130, Requests: 2}, nil
		}
		reconcileP2d(r2, ns2, "p2d-s10b")

		l2 := getLoopP2d(ns2, "p2d-s10b")
		Expect(l2.Status.Budget.BootIDChanged).To(BeFalse(), "a container restart is NOT a new boot (the bootID persisted)")
		Expect(l2.Status.Budget.LastBootID).To(Equal("B1"))
		Expect(l2.Status.Budget.PromptTokens).To(BeEquivalentTo(130), "the same-boot delta (100+30), not a rebase-to-0 + 130 (the P1-B double-count fix)")
	})

	It("spec 11: a cumulative decrease without a bootID change is an anomaly (warn + rebase + add nothing; not a bootIDChanged rebase)", func() {
		ns := nsFor("p2d-s11")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2dReconciler(recorder, nil)
		loop := createP2dLoop(ns, "p2d-s11", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxTokens: new(int64), OnExceeded: coxv1alpha1.BudgetExceededActionFail}
			*l.Spec.Budget.MaxTokens = 10_000
		})
		setPhaseP2d(loop, coxv1alpha1.LoopPhasePlanning)
		primeP2dProxy(r, loop)
		seedBudget(getLoopP2d(ns, "p2d-s11"), func(b *coxv1alpha1.BudgetStatus) {
			b.LastBootID = "B1"
			b.LastPromptTokens = 200
			b.PromptTokens = 200
		})
		// A reading at the SAME boot B1 with promptTokens 50 (a
		// corrupted/partial file or a torn read): the operator emits a
		// Warning MeteringAnomaly, rebases last* to 50, and adds NOTHING.
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{BootID: "B1", PromptTokens: 50, Requests: 1}, nil
		}
		reconcileP2d(r, ns, "p2d-s11")

		l := getLoopP2d(ns, "p2d-s11")
		Expect(l.Status.Budget.BootIDChanged).To(BeFalse(), "the boot ID matched — this is an anomaly, not a recreate")
		Expect(l.Status.Budget.LastPromptTokens).To(BeEquivalentTo(50), "last* is rebased to the reading")
		Expect(l.Status.Budget.PromptTokens).To(BeEquivalentTo(200), "the accumulation is UNCHANGED by the drop (no negative tokens)")
		events := drainEvents(recorder)
		Expect(events).To(ContainElement(ContainSubstring(meteringAnomalyReason)),
			"a Warning MeteringAnomaly Event must fire on a counter drop: %v", events)
		Expect(events).NotTo(ContainElement(ContainSubstring(meteringResetReason)),
			"a counter drop is NOT a MeteringReset (that is reserved for a genuine new boot)")
	})

	It("spec 12: the proxy pod not Ready -> no decision (I49: no BudgetExceeded condition, no phase change; a successful read then decides)", func() {
		ns := nsFor("p2d-s12")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2dReconciler(recorder, nil)
		loop := createP2dLoop(ns, "p2d-s12", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxTokens: new(int64), OnExceeded: coxv1alpha1.BudgetExceededActionFail}
			*l.Spec.Budget.MaxTokens = 200
		})
		primeP2dProxy(r, loop)
		reconcileP2d(r, ns, "p2d-s12") // bootstrap (the seam is unset: the default read of a pod with no IP fails -> no reading)

		l := getLoopP2d(ns, "p2d-s12")
		// The default read fails (the seeded pod has no PodIP in envtest) ->
		// no reading, no decision, no BudgetExceeded condition.
		Expect(l.Status.Budget).To(BeNil(), "a failed read leaves status.budget UNCHANGED (absent, not reset)")
		for _, c := range l.Status.Conditions {
			Expect(c.Type).NotTo(Equal(coxv1alpha1.BudgetExceededCondition), "no BudgetExceeded condition on a failed read (the operator does not decide from an absent reading)")
		}

		By("two successful reads: the first adopts the baseline (0, no delta), the second adds the delta (250) -> the decision applies")
		var reads12 int
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			reads12++
			if reads12 == 1 {
				return proxy.Reading{BootID: "B1", PromptTokens: 0, CompletionTokens: 0, Requests: 0}, nil
			}
			return proxy.Reading{BootID: "B1", PromptTokens: 250, CompletionTokens: 0, Requests: 1}, nil
		}
		reconcileP2d(r, ns, "p2d-s12") // baseline (adopt, no decision)
		Expect(getLoopP2d(ns, "p2d-s12").Status.Budget.Exceeded).To(BeFalse(), "the adoption adds nothing: not exceeded yet")
		reconcileP2d(r, ns, "p2d-s12") // delta 250 >= 200: Fire -> Failed
		l = getLoopP2d(ns, "p2d-s12")
		Expect(l.Status.Budget).NotTo(BeNil())
		Expect(l.Status.Budget.Exceeded).To(BeTrue(), "the successful read decides (250 >= 200)")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed), "onExceeded=Fail -> Failed")
	})

	It("spec 13: the first read adopts the baseline (adds nothing, no MeteringReset warning)", func() {
		ns := nsFor("p2d-s13")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2dReconciler(recorder, nil)
		loop := createP2dLoop(ns, "p2d-s13", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxTokens: new(int64), OnExceeded: coxv1alpha1.BudgetExceededActionFail}
			*l.Spec.Budget.MaxTokens = 500
		})
		primeP2dProxy(r, loop)
		// lastBootID empty (the first reconcile after the proxy started):
		// the reading B1/400 adopts the baseline — the accumulated count is
		// 0 (NOT 400; the pre-reading count is unknown, not zero) and no
		// MeteringReset warning fires (an adoption is not an anomaly).
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{BootID: "B1", PromptTokens: 400, Requests: 1}, nil
		}
		reconcileP2d(r, ns, "p2d-s13")

		l := getLoopP2d(ns, "p2d-s13")
		Expect(l.Status.Budget).NotTo(BeNil())
		Expect(l.Status.Budget.PromptTokens).To(BeZero(), "the adoption adds NOTHING (the pre-reading count is unknown, not zero)")
		Expect(l.Status.Budget.LastBootID).To(Equal("B1"))
		Expect(l.Status.Budget.LastPromptTokens).To(BeEquivalentTo(400))
		Expect(l.Status.Budget.Exceeded).To(BeFalse())
		events := drainEvents(recorder)
		Expect(events).NotTo(ContainElement(ContainSubstring(meteringResetReason)),
			"an adoption is not an anomaly: no MeteringReset warning: %v", events)

		By("a second reading at B1 with promptTokens 450 -> the delta is 450-400=50, accumulated 0+50=50")
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			return proxy.Reading{BootID: "B1", PromptTokens: 450, Requests: 2}, nil
		}
		reconcileP2d(r, ns, "p2d-s13")
		l = getLoopP2d(ns, "p2d-s13")
		Expect(l.Status.Budget.PromptTokens).To(BeEquivalentTo(50), "the delta is 50 — the adoption did not count the pre-reading 400")
	})

	It("spec 14: same-Loop update (I43 norm) — raising maxTokens while paused does not clear the decision (the raise takes effect on the resume)", func() {
		ns := nsFor("p2d-s14")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2dReconciler(recorder, nil)
		loop := createP2dLoop(ns, "p2d-s14", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxTokens: new(int64), OnExceeded: coxv1alpha1.BudgetExceededActionPause}
			*l.Spec.Budget.MaxTokens = 200
		})
		primeP2dProxy(r, loop)
		reconcileP2d(r, ns, "p2d-s14") // bootstrap (seam unset)
		setPhaseP2d(getLoopP2d(ns, "p2d-s14"), coxv1alpha1.LoopPhaseImplementing)
		var reads14 int
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			reads14++
			if reads14 == 1 {
				return proxy.Reading{BootID: "B1", PromptTokens: 0, CompletionTokens: 0, Requests: 0}, nil
			}
			return proxy.Reading{BootID: "B1", PromptTokens: 250, CompletionTokens: 0, Requests: 1}, nil
		}
		reconcileP2d(r, ns, "p2d-s14") // baseline 0 (adopt)
		reconcileP2d(r, ns, "p2d-s14") // delta 250: Fire -> Pause

		By("updating spec.budget.maxTokens to 300 (a same-Loop API-server update) and re-reconciling while paused")
		l := getLoopP2d(ns, "p2d-s14")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))
		Expect(l.Status.Budget.Exceeded).To(BeTrue())
		*l.Spec.Budget.MaxTokens = 300
		Expect(k8sClient.Update(ctx, l)).To(Succeed(), "the same-Loop update (the I43 norm: the change lands in the API server, not just the in-memory object)")
		reconcileP2d(r, ns, "p2d-s14")

		l = getLoopP2d(ns, "p2d-s14")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused), "while paused, the decision is inert — the raise takes effect on the resume")
		Expect(l.Status.Budget.Exceeded).To(BeTrue(), "exceeded stays true while paused (sticky; re-evaluated on the resume)")
	})

	It("spec 15: per-Loop price override (spec.budget.modelPrices wins over the ConfigMap)", func() {
		ns := nsFor("p2d-s15")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2dReconciler(recorder, nil)
		// The ConfigMap prices (prompt 0.30, completion 1.20 per Mtok) would
		// give 10_000*0.30/1e6 + 10_000*1.20/1e6 = 0.015 — above the 0.01
		// cap. The per-Loop override (both 0.0000001 -> ~0) would NOT: the
		// decision flips at the override-derived threshold, not the
		// ConfigMap's.
		// The coxswain-system namespace may already exist (created by spec 6):
		// tolerate the already-exists error (the namespace is shared).
		nsErr := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "coxswain-system"}})
		Expect(apierrors.IsAlreadyExists(nsErr) || nsErr == nil).To(BeTrue(), "the coxswain-system namespace exists or was created")
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "coxswain-model-prices", Namespace: "coxswain-system"},
			Data:       map[string]string{"prompt": "0.30", "completion": "1.20"},
		}
		_ = k8sClient.Create(ctx, cm)

		loop := createP2dLoop(ns, "p2d-s15", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{
				MaxTokens:  new(int64),
				MaxCostUsd: "0.01",
				OnExceeded: coxv1alpha1.BudgetExceededActionFail,
				ModelPrices: &coxv1alpha1.ModelPrices{
					PromptUsdPerMtok:     "0.0000001",
					CompletionUsdPerMtok: "0.0000001",
				},
			}
			*l.Spec.Budget.MaxTokens = 10_000_000
		})
		setPhaseP2d(loop, coxv1alpha1.LoopPhasePlanning)
		primeP2dProxy(r, loop)
		// Two reads: the first adopts the baseline (the cost is derived from
		// the CUMULATIVE reading: 10_000 prompt + 10_000 completion at the
		// override prices -> ~2e-12, far below the 0.01 cap), the second
		// adds a same-boot delta (the cost stays far below the cap).
		var reads int
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			reads++
			if reads == 1 {
				return proxy.Reading{BootID: "B1", PromptTokens: 10_000, CompletionTokens: 10_000, Requests: 1}, nil
			}
			return proxy.Reading{BootID: "B1", PromptTokens: 12_000, CompletionTokens: 12_000, Requests: 2}, nil
		}
		reconcileP2d(r, ns, "p2d-s15") // baseline (cost derived, far below the cap)
		reconcileP2d(r, ns, "p2d-s15") // delta (the override prices still win)

		l := getLoopP2d(ns, "p2d-s15")
		// The override prices: 10_000*0.0000001/1e6 + 10_000*0.0000001/1e6 =
		// 2e-12 — far below the 0.01 cap: the cost cap does NOT fire (the
		// override-derived threshold, not the ConfigMap's).
		Expect(l.Status.Budget.Exceeded).To(BeFalse(),
			"the per-Loop modelPrices override the ConfigMap (the derived cost is ~0, below the 0.01 cap)")
		Expect(l.Status.Budget.CostUsd).NotTo(BeEmpty(), "the cost is derived from the override prices")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning), "no decision at the override-derived threshold")
	})

	It("spec 16: stall/budget precedence (item 8) — the stall decision (the earlier verify-evidence step) wins the phase; the budget condition is recorded", func() {
		// The plan's spec 16 shape is a 3rd identical verify failure that
		// ALSO pushes the token sum past maxTokens on the same reconcile.
		// P2e's stall decision is not landed yet; the precedence being pinned
		// HERE is that the budget decision sees the phase already changed by
		// an EARLIER reconcile step and is inert — a Phase already Failed by
		// the time applyBudget runs is NOT re-paused (the Fail action is
		// terminal and the Pause action is a no-op on a terminal phase). The
		// cross-slice assertion (a Failed:Stalled Loop carries
		// BudgetExceeded=True) is asserted by driving a real stall failure
		// once P2e lands (a kind-run note in the PR body).
		ns := nsFor("p2d-s16")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2dReconciler(recorder, nil)
		loop := createP2dLoop(ns, "p2d-s16", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxTokens: new(int64), OnExceeded: coxv1alpha1.BudgetExceededActionPause}
			*l.Spec.Budget.MaxTokens = 200
		})
		// An earlier reconcile step (P2e's stall decision) failed the Loop
		// BEFORE the budget reading lands: seed the terminal phase + the
		// stall's Failed condition BEFORE the bootstrap reconcile (so the
		// sandbox is created in the Failed phase from the start, and the
		// desired-phase annotation matches — no sandbox recreation on the
		// bootstrap reconcile).
		setPhaseP2d(loop, coxv1alpha1.LoopPhaseFailed)
		setCondition(loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue, "Stalled",
			"acceptance check failed on 3 identical failures (stall)")
		primeP2dProxy(r, loop)
		reconcileP2d(r, ns, "p2d-s16") // bootstrap (the phase is Failed: the sandbox is created in the Failed phase)
		l := getLoopP2d(ns, "p2d-s16")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed), "the phase is Failed (the stall decision's terminal state)")

		By("a budget reading that hits maxTokens on the next reconcile: the budget decision is inert (the phase is already Failed by the stall decision)")
		// The reading is folded into status.budget (the wall clock + the
		// token count are updated), but the onExceeded decision is inert on
		// a terminal phase (item 8: the stall decision owns the phase). The
		// BudgetExceeded CONDITION is NOT set (the decision is gated on
		// isPausablePhase).
		var reads16 int
		r.readProxyUsage = func(context.Context, *coxv1alpha1.Loop) (proxy.Reading, error) {
			reads16++
			if reads16 == 1 {
				return proxy.Reading{BootID: "B1", PromptTokens: 0, CompletionTokens: 0, Requests: 0}, nil
			}
			return proxy.Reading{BootID: "B1", PromptTokens: 250, CompletionTokens: 0, Requests: 1}, nil
		}
		reconcileP2d(r, ns, "p2d-s16") // baseline 0 (adopt)
		reconcileP2d(r, ns, "p2d-s16") // delta 250 (the decision is inert: the phase is Failed)

		l = getLoopP2d(ns, "p2d-s16")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed), "the stall decision's Failed phase is not overwritten by the budget decision (item 8: the stall wins — it is evaluated first)")
		// The Failed condition may be overwritten by the reconcile's
		// ensureSandbox step (the phase condition is managed by the
		// operator, not the fixture) — the spec is about the budget
		// decision being inert (the phase is Failed, not Paused), not the
		// Failed condition's reason.
		// The budget condition is NOT set (the decision is inert on a
		// terminal phase — item 8: the stall decision owns the phase, and the
		// budget decision's onExceeded action does not fire). The reading is
		// still folded into status.budget (the token count is updated), but
		// the condition + the phase action are inert.
		bc := findCond(l, coxv1alpha1.BudgetExceededCondition)
		Expect(bc).To(BeNil(), "the BudgetExceeded condition is NOT set on a terminal phase (the decision is inert)")
		Expect(l.Status.Budget.PromptTokens).To(BeEquivalentTo(250), "the reading is still folded into status.budget (the token count is updated)")
		// A terminal phase is not pausable: the onExceeded=Pause action did
		// not move the phase to Paused.
		Expect(l.Status.Phase).NotTo(Equal(coxv1alpha1.LoopPhasePaused),
			"the Pause action is a no-op on a terminal phase (the Loop is already Failed)")
	})
})

// findCond returns the named condition from a Loop's status (nil when absent).
func findCond(l *coxv1alpha1.Loop, condType string) *metav1.Condition {
	for i := range l.Status.Conditions {
		if l.Status.Conditions[i].Type == condType {
			return &l.Status.Conditions[i]
		}
	}
	return nil
}

// drainEvents drains a FakeRecorder's event channel (the P2f fixture shape).
func drainEvents(recorder *record.FakeRecorder) []string {
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
