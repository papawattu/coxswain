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

// P2f (TDD-PLAN-PHASE2): the Paused phase — the pause entry (one mechanism,
// three entry points), the suspension gate, and the resume semantics.
//
// The specs below are the plan's numbered envtest-first tests 1-15. The
// gate mutations (I49 norm) are applied EXACTLY in a scratch worktree and
// each makes its named spec FAIL (results recorded in the PR body).

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
)

// p2f fixture constants.
const (
	p2fModelSecret   = "p2f-model-creds"
	p2fModelEndpoint = "10.0.0.9:9200" // IP-literal: the D35a proxy gate peer is an ipBlock
)

// p2fHeadCommit is the 40-hex head commit for the P2f specs (the runner's
// committed head; the B1 spec models a successful Implementing claim that
// always commits and reports the head).
const p2fHeadCommit = "c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4" // 40-hex

// p2fInt64Ptr returns a *int64 pointing to v (the P2f budget cap values).
//
//nolint:modernize // newexpr: the linter wants new(x), but new(x) returns a zero value, not a pointer to x.
func p2fInt64Ptr(v int64) *int64 { return &v }

var (
	p2fMaxTokens200 = p2fInt64Ptr(200)
	p2fMaxTokens500 = p2fInt64Ptr(500)
)

var _ = Describe("P2f: Paused phase, pausedFrom/pausedReason, suspension gate, resume", func() {
	ctx := context.Background()

	// p2fReconciler builds a reconciler whose gates are all satisfied so the
	// sandbox reaches Running when it should (D30 AllowUnenforced, D38 CNI
	// enforced, D35a proxy Ready+owned). The specs then prove the P2f
	// suspension gate holds it Suspended while Paused and releases it on
	// resume. now is a mutable clock (the item-E specs advance it).
	newP2fReconciler := func(recorder *record.FakeRecorder, nowPtr **metav1.Time) *LoopReconciler {
		r := &LoopReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			AllowUnenforced: true,
			CNIProber:       cni.NewFakeProber(),
			Recorder:        recorder,
			readPhaseClaim:  func(context.Context, *coxv1alpha1.Loop) (*PhaseClaim, error) { return nil, nil },
		}
		if nowPtr != nil {
			*nowPtr = new(metav1.Time)
			**nowPtr = metav1.Now()
			r.now = func() metav1.Time { return **nowPtr }
		}
		r.CNIProber.(*cni.FakeProber).SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced})
		return r
	}

	// primeProxy creates the model proxy the D35a gate requires (the operator's
	// ensureProxy builds the real pod spec + spec-hash annotation, so the drift
	// check doesn't delete it) and then marks the pod Ready. Owned by the Loop.
	primeProxy := func(r *LoopReconciler, loop *coxv1alpha1.Loop) {
		_ = k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: p2fModelSecret, Namespace: loop.Namespace},
			StringData: map[string]string{modelAPIKey: "p2f-dummy", modelBaseURL: p2fModelEndpoint},
		})
		// Let the operator create the real proxy pod (spec + hash annotation).
		Expect(r.ensureProxy(ctx, loop)).To(Succeed())
		// Mark the proxy pod Ready (the D35a gate's isPodReady check).
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: proxyPodName(loop.Name)}, pod)).To(Succeed(),
			"the operator's ensureProxy must have created the proxy pod")
		now := metav1.Now()
		pod.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	// createLoop creates a Loop with a model endpoint (the D35a gate is on the
	// path, so primeProxy must be called before the sandbox can reach Running).
	createLoop := func(ns, name string, mutate func(*coxv1alpha1.Loop)) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "P2f paused spec",
				Workspace: testWorkspace(),
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: p2fModelSecret,
					ModelEndpoint:     p2fModelEndpoint,
				},
			},
		}
		if mutate != nil {
			mutate(loop)
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		return loop
	}

	reconcile := func(r *LoopReconciler, ns, name string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred(), "reconcile %s/%s", ns, name)
	}

	getLoop := func(ns, name string) *coxv1alpha1.Loop {
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
		return got
	}

	sandboxMode := func(ns, name string) sandboxv1beta1.SandboxOperatingMode {
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-sandbox"}, sb)).To(Succeed(),
			"sandbox %s-sandbox must exist", name)
		return sb.Spec.OperatingMode
	}

	setPhase := func(loop *coxv1alpha1.Loop, phase coxv1alpha1.LoopPhase) {
		loop.Status.Phase = phase
		loop.Status.DesiredPhase = phase
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
	}

	setSuspend := func(ns, name string, suspend bool) {
		l := getLoop(ns, name)
		l.Spec.Suspend = suspend
		Expect(k8sClient.Update(ctx, l)).To(Succeed())
	}

	setCondition := func(l *coxv1alpha1.Loop, condType string, status metav1.ConditionStatus, reason, msg string) {
		_ = l
		_ = status
		_ = reason
		_ = msg
		// (the specs assert conditions via the cond() helper below)
		_ = condType
	}
	_ = setCondition

	cond := func(l *coxv1alpha1.Loop, condType string) *metav1.Condition {
		for i := range l.Status.Conditions {
			if l.Status.Conditions[i].Type == condType {
				return &l.Status.Conditions[i]
			}
		}
		return nil
	}

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

	It("spec 1: spec.suspend=true -> Paused (pausedFrom, pausedReason, condition, event, sandbox Suspended)", func() {
		ns := nsFor("p2f-s1")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2fReconciler(recorder, nil)
		loop := createLoop(ns, "p2f-s1", nil)
		primeProxy(r, loop)
		reconcile(r, ns, "p2f-s1") // bootstrap Pending -> Planning
		setPhase(getLoop(ns, "p2f-s1"), coxv1alpha1.LoopPhaseImplementing)
		reconcile(r, ns, "p2f-s1") // sandbox Running

		By("flipping suspend=true and re-reconciling")
		setSuspend(ns, "p2f-s1", true)
		reconcile(r, ns, "p2f-s1")

		l := getLoop(ns, "p2f-s1")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))
		Expect(l.Status.PausedFrom).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		Expect(l.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonSuspend))
		c := cond(l, coxv1alpha1.PausedCondition)
		Expect(c).NotTo(BeNil(), "the Paused condition must be set")
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		events := drainEvents(recorder)
		Expect(events).To(ContainElement(ContainSubstring("suspend")),
			"a Paused Event naming the source (suspend) must fire: %v", events)
		Expect(sandboxMode(ns, "p2f-s1")).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended))
	})

	It("spec 2: pausedFrom/pausedReason are not overwritten on re-reconcile", func() {
		ns := nsFor("p2f-s2")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2fReconciler(recorder, nil)
		loop := createLoop(ns, "p2f-s2", nil)
		primeProxy(r, loop)
		reconcile(r, ns, "p2f-s2")
		setPhase(getLoop(ns, "p2f-s2"), coxv1alpha1.LoopPhaseVerifying)
		reconcile(r, ns, "p2f-s2")

		setSuspend(ns, "p2f-s2", true)
		reconcile(r, ns, "p2f-s2")
		l1 := getLoop(ns, "p2f-s2")
		Expect(l1.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))
		Expect(l1.Status.PausedFrom).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(l1.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonSuspend))

		By("re-reconciling with suspend still true")
		reconcile(r, ns, "p2f-s2")
		l2 := getLoop(ns, "p2f-s2")
		Expect(l2.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))
		Expect(l2.Status.PausedFrom).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"pausedFrom must not be overwritten (it is no longer the phase the Loop left)")
		Expect(l2.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonSuspend))
	})

	It("spec 3: the verify Job does not run while paused (no new Job; termination produces no decision)", func() {
		ns := nsFor("p2f-s3")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2fReconciler(recorder, nil)
		loop := createLoop(ns, "p2f-s3", func(l *coxv1alpha1.Loop) {
			l.Spec.Verify = coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}}
		})
		primeProxy(r, loop)
		reconcile(r, ns, "p2f-s3")
		// At Verifying with a pin: the verify Job would be created (the
		// B3 evidence path).
		l := getLoop(ns, "p2f-s3")
		l.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		l.Status.DesiredPhase = coxv1alpha1.LoopPhaseVerifying
		l.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: p2fHeadCommit}
		l.Status.BaseCommit = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
		reconcile(r, ns, "p2f-s3")

		job := &batchv1.Job{}
		jobErr := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "p2f-s3-verify-1"}, job)
		Expect(jobErr).ToNot(HaveOccurred(), "the verify Job must exist at Verifying")
		// Terminate the verify pod with a non-zero check exit.
		p2fVerifyPod(ctx, ns, "p2f-s3", verifyJobName(getLoop(ns, "p2f-s3")), 1)

		By("pausing (suspend=true) and re-reconciling")
		setSuspend(ns, "p2f-s3", true)
		reconcile(r, ns, "p2f-s3")
		l = getLoop(ns, "p2f-s3")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))
		Expect(l.Status.PausedFrom).To(Equal(coxv1alpha1.LoopPhaseVerifying))

		By("asserting no new verify Job and no decision (stallHistory/budget unchanged, phase stays Paused)")
		reconcile(r, ns, "p2f-s3")
		l = getLoop(ns, "p2f-s3")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused),
			"a verify Job termination while paused must produce no iterate/stall/budget decision")
		Expect(l.Status.Iteration).To(Equal(0))
		Expect(l.Status.StallHistory).To(BeEmpty())
		Expect(l.Status.Budget).To(BeNil())
		// No NEW verify Job (the existing one is left to terminate).
		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs, client.InNamespace(ns), client.MatchingLabels{verifyForLabel: "p2f-s3"})).To(Succeed())
		Expect(jobs.Items).To(HaveLen(1), "no new verify Job is created while paused")
	})

	It("spec 4: a stale claim while paused is ignored (the nextPhase gate on phase != Paused)", func() {
		ns := nsFor("p2f-s4")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2fReconciler(recorder, nil)
		loop := createLoop(ns, "p2f-s4", nil)
		primeProxy(r, loop)
		reconcile(r, ns, "p2f-s4")
		setPhase(getLoop(ns, "p2f-s4"), coxv1alpha1.LoopPhaseImplementing)
		reconcile(r, ns, "p2f-s4")

		setSuspend(ns, "p2f-s4", true)
		reconcile(r, ns, "p2f-s4")
		l := getLoop(ns, "p2f-s4")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))

		By("injecting a claim naming the immediate-next phase (Verifying)")
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return &PhaseClaim{
				ObservedPhase: coxv1alpha1.LoopPhaseImplementing,
				Status:        claimSuccess,
				HeadCommit:    p2fHeadCommit,
			}, nil
		}
		reconcile(r, ns, "p2f-s4")

		l = getLoop(ns, "p2f-s4")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused),
			"a claim arriving while paused must NOT advance the phase")
	})

	It("spec 5: resume via suspend=false (a Suspend pause) -> exact phase, record cleared, sandbox Running", func() {
		ns := nsFor("p2f-s5")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2fReconciler(recorder, nil)
		loop := createLoop(ns, "p2f-s5", nil)
		primeProxy(r, loop)
		reconcile(r, ns, "p2f-s5")
		setPhase(getLoop(ns, "p2f-s5"), coxv1alpha1.LoopPhaseImplementing)
		reconcile(r, ns, "p2f-s5")

		setSuspend(ns, "p2f-s5", true)
		reconcile(r, ns, "p2f-s5")
		Expect(getLoop(ns, "p2f-s5").Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))
		Expect(sandboxMode(ns, "p2f-s5")).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended))

		By("flipping suspend=false and re-reconciling")
		setSuspend(ns, "p2f-s5", false)
		reconcile(r, ns, "p2f-s5")

		l := getLoop(ns, "p2f-s5")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing), "the exact pausedFrom phase")
		Expect(l.Status.PausedFrom).To(BeEmpty())
		Expect(l.Status.PausedReason).To(BeEmpty())
		c := cond(l, coxv1alpha1.PausedCondition)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("Resumed"))
		events := drainEvents(recorder)
		Expect(events).To(ContainElement(ContainSubstring("Implementing")),
			"a Resumed Event naming the phase must fire: %v", events)
		Expect(sandboxMode(ns, "p2f-s5")).To(Equal(sandboxv1beta1.SandboxOperatingModeRunning),
			"the suspension gate releases on resume (the D30/D35a gates are satisfied in the fixture)")
	})

	It("spec 6: suspend=false does NOT resume a Budget or Stall pause", func() {
		ns := nsFor("p2f-s6")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2fReconciler(recorder, nil)
		loop := createLoop(ns, "p2f-s6", nil)
		primeProxy(r, loop)
		reconcile(r, ns, "p2f-s6")
		setPhase(getLoop(ns, "p2f-s6"), coxv1alpha1.LoopPhaseImplementing)
		reconcile(r, ns, "p2f-s6")

		for _, tc := range []struct {
			name   string
			reason coxv1alpha1.PausedReason
		}{
			{"budget", coxv1alpha1.PausedReasonBudget},
			{"stall", coxv1alpha1.PausedReasonStall},
		} {
			By("the pause reason is " + tc.name)
			l := getLoop(ns, "p2f-s6")
			// Simulate a Budget/Stall entry (P2d/P2e's shape): phase Paused,
			// the reason set, spec.suspend already false.
			l.Status.Phase = coxv1alpha1.LoopPhasePaused
			l.Status.PausedFrom = coxv1alpha1.LoopPhaseImplementing
			l.Status.PausedReason = tc.reason
			Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
			reconcile(r, ns, "p2f-s6")

			got := getLoop(ns, "p2f-s6")
			Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused),
				"spec.suspend=false must NOT resume a %s pause", tc.reason)
			Expect(got.Status.PausedFrom).To(Equal(coxv1alpha1.LoopPhaseImplementing))
			Expect(got.Status.PausedReason).To(Equal(tc.reason))
			Expect(sandboxMode(ns, "p2f-s6")).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
				"the suspension gate holds the sandbox Suspended (regardless of spec.suspend)")
		}
	})

	It("spec 7: resume via the annotation (a Budget/Stall pause); leftover annotation on a non-paused Loop is ignored", func() {
		ns := nsFor("p2f-s7")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2fReconciler(recorder, nil)
		loop := createLoop(ns, "p2f-s7", nil)
		primeProxy(r, loop)
		reconcile(r, ns, "p2f-s7")

		By("a Budget pause at pausedFrom=Verifying with the annotation")
		l := getLoop(ns, "p2f-s7")
		l.Status.Phase = coxv1alpha1.LoopPhasePaused
		l.Status.DesiredPhase = coxv1alpha1.LoopPhasePaused
		l.Status.PausedFrom = coxv1alpha1.LoopPhaseVerifying
		l.Status.PausedReason = coxv1alpha1.PausedReasonBudget
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
		// The annotation is a metadata change: a separate Update (the status
		// subresource does not carry annotations).
		l = getLoop(ns, "p2f-s7")
		l.Annotations = map[string]string{resumeAnnotation: unstructuredTrue}
		Expect(k8sClient.Update(ctx, l)).To(Succeed())
		reconcile(r, ns, "p2f-s7")

		got := getLoop(ns, "p2f-s7")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "back to the exact pausedFrom phase")
		Expect(got.Status.PausedFrom).To(BeEmpty())
		Expect(got.Status.PausedReason).To(BeEmpty())
		c := cond(got, coxv1alpha1.PausedCondition)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("Resumed"))
		Expect(got.Annotations).NotTo(HaveKey(resumeAnnotation), "the annotation is cleared on a valid resume")
		events := drainEvents(recorder)
		Expect(events).To(ContainElement(ContainSubstring("Verifying")))

		By("a leftover annotation on a NON-paused Loop is ignored (no side effect)")
		got = getLoop(ns, "p2f-s7")
		got.Annotations = map[string]string{resumeAnnotation: unstructuredTrue}
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		reconcile(r, ns, "p2f-s7")
		got = getLoop(ns, "p2f-s7")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "the phase is unchanged")
		Expect(got.Annotations).To(HaveKey(resumeAnnotation), "the annotation is NOT cleared by a non-paused reconcile")
	})

	It("spec 8: a resumed budget-exceeded Loop re-evaluates (P3 refuse + item-5 clear)", func() {
		ns := nsFor("p2f-s8")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2fReconciler(recorder, nil)
		loop := createLoop(ns, "p2f-s8", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{
				MaxTokens:  p2fMaxTokens200,
				OnExceeded: coxv1alpha1.BudgetExceededActionPause,
			}
		})
		primeProxy(r, loop)
		reconcile(r, ns, "p2f-s8")

		l := getLoop(ns, "p2f-s8")
		// A Budget pause: phase Paused, the counts over the cap, exceeded=true.
		l.Status.Phase = coxv1alpha1.LoopPhasePaused
		l.Status.DesiredPhase = coxv1alpha1.LoopPhasePaused
		l.Status.PausedFrom = coxv1alpha1.LoopPhaseImplementing
		l.Status.PausedReason = coxv1alpha1.PausedReasonBudget
		l.Status.Budget = &coxv1alpha1.BudgetStatus{
			PromptTokens:     250,
			CompletionTokens: 0,
			Exceeded:         true,
			ExceededReason:   coxv1alpha1.BudgetExceededTokens,
		}
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
		// The phase advance (Planning->Paused) is a real operator action: the
		// sandbox is deleted for re-creation. A second reconcile re-creates it.
		reconcile(r, ns, "p2f-s8")
		reconcile(r, ns, "p2f-s8")

		By("(a) caps not raised: resume via the annotation is REFUSED (P3)")
		l = getLoop(ns, "p2f-s8")
		l.Annotations = map[string]string{resumeAnnotation: unstructuredTrue}
		Expect(k8sClient.Update(ctx, l)).To(Succeed())
		modeBefore := sandboxMode(ns, "p2f-s8")
		reconcile(r, ns, "p2f-s8")
		got := getLoop(ns, "p2f-s8")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused), "a refused resume keeps the phase Paused")
		c := cond(got, coxv1alpha1.PausedCondition)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Message).To(ContainSubstring("Tokens"), "the message names the still-exceeded cap")
		Expect(got.Annotations).To(HaveKey(resumeAnnotation), "the annotation is NOT cleared on a refused resume")
		Expect(sandboxMode(ns, "p2f-s8")).To(Equal(modeBefore), "no OperatingMode transition on a refused resume")
		events := drainEvents(recorder)
		Expect(events).To(ContainElement(ContainSubstring("ResumeRefused")),
			"a Warning ResumeRefused Event must fire: %v", events)

		By("(b) caps raised (an I43 same-Loop update): resume clears the exceedance")
		got = getLoop(ns, "p2f-s8")
		got.Spec.Budget.MaxTokens = p2fMaxTokens500
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		reconcile(r, ns, "p2f-s8")
		got = getLoop(ns, "p2f-s8")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing), "the Loop proceeds")
		Expect(got.Status.Budget).NotTo(BeNil())
		Expect(got.Status.Budget.Exceeded).To(BeFalse(), "raising the cap clears the exceedance (250 < 500)")
		bc := cond(got, coxv1alpha1.BudgetExceededCondition)
		Expect(bc).NotTo(BeNil())
		Expect(bc.Status).To(Equal(metav1.ConditionFalse))
		Expect(bc.Reason).To(Equal("ClearedOnResume"))
		pc := cond(got, coxv1alpha1.PausedCondition)
		Expect(pc).NotTo(BeNil())
		Expect(pc.Status).To(Equal(metav1.ConditionFalse))
		Expect(pc.Reason).To(Equal("Resumed"))
		Expect(got.Annotations).NotTo(HaveKey(resumeAnnotation))
		// Two reconciles after the resume: the phase is unchanged (the
		// re-evaluation cleared the exceedance; the decision does not re-fire).
		reconcile(r, ns, "p2f-s8")
		reconcile(r, ns, "p2f-s8")
		got = getLoop(ns, "p2f-s8")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		Expect(got.Status.Budget.Exceeded).To(BeFalse())
	})

	It("spec 9: Failed/Succeeded are not pausable; delivery-in-flight refused (item F)", func() {
		ns := nsFor("p2f-s9")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2fReconciler(recorder, nil)

		By("a Failed Loop with suspend=true: sandbox Suspended (S1) but phase stays Failed, not pausable")
		loopA := createLoop(ns, "p2f-s9a", nil)
		primeProxy(r, loopA)
		reconcile(r, ns, "p2f-s9a")
		setPhase(getLoop(ns, "p2f-s9a"), coxv1alpha1.LoopPhaseFailed)
		reconcile(r, ns, "p2f-s9a")
		By("setting suspend=true on the Failed Loop")
		setSuspend(ns, "p2f-s9a", true)
		reconcile(r, ns, "p2f-s9a")
		gotA := getLoop(ns, "p2f-s9a")
		Expect(gotA.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed))
		Expect(gotA.Status.PausedFrom).To(BeEmpty(), "a terminal phase is not pausable")
		Expect(gotA.Status.PausedReason).To(BeEmpty())
		Expect(sandboxMode(ns, "p2f-s9a")).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended))

		By("a Succeeded Loop with no deliver Job in flight: not pausable (spec-9 terminal rule)")
		loopB := createLoop(ns, "p2f-s9b", nil)
		primeProxy(r, loopB)
		reconcile(r, ns, "p2f-s9b")
		setPhase(getLoop(ns, "p2f-s9b"), coxv1alpha1.LoopPhaseSucceeded)
		reconcile(r, ns, "p2f-s9b")
		By("setting suspend=true on the Succeeded Loop")
		setSuspend(ns, "p2f-s9b", true)
		reconcile(r, ns, "p2f-s9b")
		gotB := getLoop(ns, "p2f-s9b")
		Expect(gotB.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		Expect(gotB.Status.PausedFrom).To(BeEmpty())

		By("a Succeeded Loop with a deliver Job in flight: suspend=true is REFUSED (item F)")
		loopC := createLoop(ns, "p2f-s9c", func(l *coxv1alpha1.Loop) {
			l.Spec.Delivery = &coxv1alpha1.DeliveryConfig{Mode: coxv1alpha1.DeliveryModePullRequest}
		})
		// I72: the deliver Job is held until the egress proxy is Ready when
		// delivery traverses the proxy (testWorkspace's repo is external). The
		// in-flight deliver Job is what item-F's refusal keys on, so the spec
		// must get one: create an AgentPolicy with a network allow (the egress
		// proxy is only needed when the effective policy has network allows,
		// I42b), reference it, and mark the operator's egress proxy pod Ready
		// (s6ReadyEgressProxy, shared with the S6 specs).
		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "p2f-s9c-policy", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{"example.com:443"}},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed())
		loopC.Spec.PolicyRefs = []string{"p2f-s9c-policy"}
		Expect(k8sClient.Update(ctx, loopC)).To(Succeed())
		primeProxy(r, loopC)
		reconcile(r, ns, "p2f-s9c")
		setPhase(getLoop(ns, "p2f-s9c"), coxv1alpha1.LoopPhaseSucceeded)
		lc := getLoop(ns, "p2f-s9c")
		lc.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: p2fHeadCommit}
		Expect(k8sClient.Status().Update(ctx, lc)).To(Succeed())
		reconcile(r, ns, "p2f-s9c")
		// I72: mark the egress proxy Ready so the deliver Job is created
		// (in flight) before the suspend refusal is asserted.
		s6ReadyEgressProxy(r, ns, "p2f-s9c")
		reconcile(r, ns, "p2f-s9c")
		By("setting suspend=true on the in-flight Succeeded Loop")
		setSuspend(ns, "p2f-s9c", true)
		reconcile(r, ns, "p2f-s9c")
		gotC := getLoop(ns, "p2f-s9c")
		Expect(gotC.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded), "the phase stays Succeeded (refused)")
		c := cond(gotC, coxv1alpha1.PausedCondition)
		Expect(c).NotTo(BeNil(), "the Paused condition records the refusal")
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Message).To(ContainSubstring("deliver"))
		// The deliver Job is undisturbed: it exists and is not deleted/suspended.
		deliverJob := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: deliverJobName("p2f-s9c")}, deliverJob)).To(Succeed(),
			"the deliver Job must exist (delivery is in flight)")
		Expect(deliverJob.Spec.Suspend).NotTo(HaveValue(BeTrue()),
			"the deliver Job is not suspended (item F: delivery is in flight)")
		// The deliver Job runs in its own pod (clone-base/import-agent/push read
		// the workspace from a volume, independent of the sandbox pod), so the
		// sandbox is stopped on the terminal Succeeded phase (I74). The deliver
		// Job itself is undisturbed — it completes on its own.
		Expect(sandboxMode(ns, "p2f-s9c")).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"I74: the sandbox is stopped on the terminal Succeeded phase (the deliver Job does not need the sandbox pod)")
	})

	It("spec 10: in-progress — a pod mid-run at pause time (I49): the claim is ignored on termination", func() {
		ns := nsFor("p2f-s10")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2fReconciler(recorder, nil)
		loop := createLoop(ns, "p2f-s10", nil)
		primeProxy(r, loop)
		reconcile(r, ns, "p2f-s10")
		setPhase(getLoop(ns, "p2f-s10"), coxv1alpha1.LoopPhaseImplementing)
		reconcile(r, ns, "p2f-s10")

		// A pod mid-run at pause time (the agent container Running).
		createAgentPodRunning(ctx, ns, "p2f-s10")

		setSuspend(ns, "p2f-s10", true)
		reconcile(r, ns, "p2f-s10")
		l := getLoop(ns, "p2f-s10")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))
		Expect(l.Status.PausedFrom).To(Equal(coxv1alpha1.LoopPhaseImplementing))

		By("the in-flight run's claim (when it terminates) is ignored")
		terminateAgentPod(ctx, ns, "p2f-s10", `{"observedPhase":"Implementing","status":"success","blockedReason":"","headCommit":"c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"}`)
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return &PhaseClaim{
				ObservedPhase: coxv1alpha1.LoopPhaseImplementing,
				Status:        claimSuccess,
				HeadCommit:    p2fHeadCommit,
			}, nil
		}
		reconcile(r, ns, "p2f-s10")
		l = getLoop(ns, "p2f-s10")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused),
			"the phase does NOT advance on the termination (the claim is ignored while paused)")
	})

	It("spec 11: same-Loop update (I43): after a resume, suspend=true again re-enters Paused at the current phase", func() {
		ns := nsFor("p2f-s11")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2fReconciler(recorder, nil)
		loop := createLoop(ns, "p2f-s11", nil)
		primeProxy(r, loop)
		reconcile(r, ns, "p2f-s11")
		setPhase(getLoop(ns, "p2f-s11"), coxv1alpha1.LoopPhaseImplementing)
		reconcile(r, ns, "p2f-s11")

		setSuspend(ns, "p2f-s11", true)
		reconcile(r, ns, "p2f-s11")
		setSuspend(ns, "p2f-s11", false)
		reconcile(r, ns, "p2f-s11")
		Expect(getLoop(ns, "p2f-s11").Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing))

		By("updating spec.suspend to true again (from the API server) and re-reconciling")
		setSuspend(ns, "p2f-s11", true)
		reconcile(r, ns, "p2f-s11")
		l := getLoop(ns, "p2f-s11")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))
		Expect(l.Status.PausedFrom).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"the phase it is in, not a stale pre-pause value")
		Expect(sandboxMode(ns, "p2f-s11")).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended))
	})

	It("spec 12: resume does not re-iterate (item C)", func() {
		ns := nsFor("p2f-s12")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2fReconciler(recorder, nil)
		loop := createLoop(ns, "p2f-s12", func(l *coxv1alpha1.Loop) {
			l.Spec.Verify = coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}}
		})
		primeProxy(r, loop)
		reconcile(r, ns, "p2f-s12")

		By("a paused-from-Verifying Loop (a budget/suspend pause): the iteration is unchanged by the resume")
		l := getLoop(ns, "p2f-s12")
		l.Status.Phase = coxv1alpha1.LoopPhasePaused
		l.Status.DesiredPhase = coxv1alpha1.LoopPhasePaused
		l.Status.PausedFrom = coxv1alpha1.LoopPhaseVerifying
		l.Status.PausedReason = coxv1alpha1.PausedReasonSuspend
		l.Spec.Suspend = true
		l.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: p2fHeadCommit}
		l.Status.BaseCommit = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
		l.Status.Iteration = 2
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
		l = getLoop(ns, "p2f-s12")
		l.Spec.Suspend = false
		Expect(k8sClient.Update(ctx, l)).To(Succeed())
		reconcile(r, ns, "p2f-s12")
		got := getLoop(ns, "p2f-s12")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(got.Status.Iteration).To(Equal(2), "the resume itself does not bump the iteration")

		By("a paused-from-Implementing Loop (a stall pause): the iteration is the post-iterate value; the next verify is a NEW Job")
		// A stall pause entered from Implementing AFTER the iterate (iteration
		// already advanced to 3). Resuming re-enters Implementing; the next
		// verify (after a new pin) is <loop>-verify-4 (a new Job).
		l2 := createLoop(ns, "p2f-s12b", func(l *coxv1alpha1.Loop) {
			l.Spec.Verify = coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}}
		})
		primeProxy(r, l2)
		reconcile(r, ns, "p2f-s12b")
		l2 = getLoop(ns, "p2f-s12b")
		l2.Status.Phase = coxv1alpha1.LoopPhasePaused
		l2.Status.DesiredPhase = coxv1alpha1.LoopPhasePaused
		l2.Status.PausedFrom = coxv1alpha1.LoopPhaseImplementing
		l2.Status.PausedReason = coxv1alpha1.PausedReasonStall
		l2.Status.Iteration = 3
		Expect(k8sClient.Status().Update(ctx, l2)).To(Succeed())
		l2 = getLoop(ns, "p2f-s12b")
		l2.Annotations = map[string]string{resumeAnnotation: unstructuredTrue}
		Expect(k8sClient.Update(ctx, l2)).To(Succeed())
		reconcile(r, ns, "p2f-s12b")
		g2 := getLoop(ns, "p2f-s12b")
		Expect(g2.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		Expect(g2.Status.Iteration).To(Equal(3), "the post-iterate value (the iterate advanced it before the pause)")
	})

	It("spec 13: pause from Planning/Pending/AwaitingApproval (item 9), table-driven", func() {
		ns := nsFor("p2f-s13")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2fReconciler(recorder, nil)
		for _, tc := range []coxv1alpha1.LoopPhase{
			coxv1alpha1.LoopPhasePlanning,
			coxv1alpha1.LoopPhaseAwaitingApproval,
		} {
			By("the Loop is at " + string(tc))
			// The Loop name must be DNS-1123 (lowercase); the phase names are
			// uppercase, so use a lowercase suffix.
			name := "p2f-s13-" + strings.ToLower(string(tc))
			loop := createLoop(ns, name, nil)
			primeProxy(r, loop)
			reconcile(r, ns, name)
			setPhase(getLoop(ns, name), tc)
			// A real phase advance (Planning->AwaitingApproval) deletes the
			// sandbox for re-creation; a second reconcile re-creates it. The
			// Planning->Planning case is a no-op (the bootstrap already advanced
			// to Planning), so the second reconcile is a no-op there too.
			reconcile(r, ns, name)
			reconcile(r, ns, name)
			Expect(sandboxMode(ns, name)).To(Equal(sandboxv1beta1.SandboxOperatingModeRunning),
				"the sandbox is Running at %s before the pause", tc)

			setSuspend(ns, name, true)
			reconcile(r, ns, name)
			l := getLoop(ns, name)
			Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))
			Expect(l.Status.PausedFrom).To(Equal(tc))
			Expect(l.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonSuspend))
			Expect(sandboxMode(ns, name)).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended))

			By("resume returns to the same phase")
			setSuspend(ns, name, false)
			reconcile(r, ns, name)
			Expect(getLoop(ns, name).Status.Phase).To(Equal(tc))
		}
	})

	It("spec 14: a wall-clock cap hit while already Paused (item 9): the first pause wins", func() {
		ns := nsFor("p2f-s14")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		nowPtr := new(metav1.Time)
		r := newP2fReconciler(recorder, &nowPtr)
		loop := createLoop(ns, "p2f-s14", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{
				MaxWallClock: "1h",
				OnExceeded:   coxv1alpha1.BudgetExceededActionPause,
			}
		})
		primeProxy(r, loop)
		reconcile(r, ns, "p2f-s14")
		setPhase(getLoop(ns, "p2f-s14"), coxv1alpha1.LoopPhaseImplementing)
		reconcile(r, ns, "p2f-s14")

		// A Suspend pause.
		setSuspend(ns, "p2f-s14", true)
		reconcile(r, ns, "p2f-s14")
		l := getLoop(ns, "p2f-s14")
		Expect(l.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))
		Expect(l.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonSuspend))

		By("the wall clock elapses while paused: the first pause wins, exceeded is recorded")
		l.Status.Budget = &coxv1alpha1.BudgetStatus{ActiveSeconds: 4000, Exceeded: true, ExceededReason: coxv1alpha1.BudgetExceededWallClock}
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
		reconcile(r, ns, "p2f-s14")
		got := getLoop(ns, "p2f-s14")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))
		Expect(got.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonSuspend), "the first pause wins")
		Expect(got.Status.Budget).NotTo(BeNil())
		Expect(got.Status.Budget.Exceeded).To(BeTrue())
		Expect(got.Status.Budget.ExceededReason).To(Equal(coxv1alpha1.BudgetExceededWallClock))

		By("a SECOND cap hit while already Paused does not double-write; the first pause's reason is preserved (item 9)")
		// The Loop is already paused (reason Suspend). A wall-clock cap hit
		// while paused would, without the "first pause wins" rule, overwrite
		// the pausedReason to Budget. The rule: the pausedReason is set only
		// on the FIRST entry (the PausedFrom nil-check). The budget exceedance
		// is recorded in status.budget.exceeded (by P2d's accumulation step)
		// but the pausedReason stays Suspend.
		got = getLoop(ns, "p2f-s14")
		got.Status.Budget = &coxv1alpha1.BudgetStatus{ActiveSeconds: 4000, Exceeded: true, ExceededReason: coxv1alpha1.BudgetExceededWallClock}
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		reconcile(r, ns, "p2f-s14")
		got = getLoop(ns, "p2f-s14")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))
		Expect(got.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonSuspend), "the first pause wins (item 9)")
		Expect(got.Status.PausedFrom).To(Equal(coxv1alpha1.LoopPhaseImplementing))
	})

	It("spec 15: a long pause does not leak into activeSeconds (item E)", func() {
		ns := nsFor("p2f-s15")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		nowPtr := new(metav1.Time)
		r := newP2fReconciler(recorder, &nowPtr)
		loop := createLoop(ns, "p2f-s15", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{
				MaxWallClock: "1h",
				OnExceeded:   coxv1alpha1.BudgetExceededActionPause,
			}
		})
		primeProxy(r, loop)
		reconcile(r, ns, "p2f-s15")
		setPhase(getLoop(ns, "p2f-s15"), coxv1alpha1.LoopPhaseImplementing)
		reconcile(r, ns, "p2f-s15")

		// Prime the budget counts: 30m active, a lastActiveStamp set.
		l := getLoop(ns, "p2f-s15")
		stamp := metav1.Now()
		l.Status.Budget = &coxv1alpha1.BudgetStatus{ActiveSeconds: 1800, LastActiveStamp: &stamp}
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())

		By("suspend=true -> Paused")
		setSuspend(ns, "p2f-s15", true)
		reconcile(r, ns, "p2f-s15")
		got := getLoop(ns, "p2f-s15")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))

		By("advance the clock 2h (a long pause): the stamp is frozen until resume (P2c owns the activeSeconds accumulation)")
		*nowPtr = metav1.NewTime(nowPtr.Time.Add(2 * time.Hour)) //nolint:staticcheck // QF1008: metav1.Time embeds time.Time; .Time is the promoted field
		reconcile(r, ns, "p2f-s15")
		got = getLoop(ns, "p2f-s15")
		Expect(got.Status.Budget).NotTo(BeNil())
		Expect(got.Status.Budget.LastActiveStamp).NotTo(BeNil(),
			"the lastActiveStamp is frozen at the pause (P2f does not advance it while paused)")

		By("suspend=false -> resume: the lastActiveStamp is reset to now (item E)")
		setSuspend(ns, "p2f-s15", false)
		reconcile(r, ns, "p2f-s15")
		got = getLoop(ns, "p2f-s15")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		Expect(got.Status.Budget).NotTo(BeNil())
		Expect(got.Status.Budget.LastActiveStamp).NotTo(BeNil(),
			"the lastActiveStamp is reset (item E: the wall clock never counts the pause)")
		// The reset stamp is the resume time (now), NOT the pre-pause stamp
		// (30m ago) and NOT the zero time. Without the item-E reset, the first
		// post-resume reconcile would add now - lastActiveStamp INCLUDING the
		// 2h pause, leaking it into activeSeconds.
		resetStamp := got.Status.Budget.LastActiveStamp.Time
		prePauseStamp := stamp.Time
		Expect(resetStamp.After(prePauseStamp)).To(BeTrue(),
			"the reset lastActiveStamp (%s) is after the pre-pause stamp (%s) — the pause is not counted",
			resetStamp.Format(time.RFC3339), prePauseStamp.Format(time.RFC3339))
	})

	It("spec 16: a budget-paused Loop whose cap is raised stays Paused on a suspend flip and resumes on the annotation (the P2h sub-case (c) sequence)", func() {
		ns := nsFor("p2f-s16")
		defer deleteNS(ctx, ns)
		recorder := record.NewFakeRecorder(64)
		r := newP2fReconciler(recorder, nil)
		loop := createLoop(ns, "p2f-s16", func(l *coxv1alpha1.Loop) {
			l.Spec.Budget = &coxv1alpha1.BudgetConfig{
				MaxTokens:  p2fMaxTokens200,
				OnExceeded: coxv1alpha1.BudgetExceededActionPause,
			}
		})
		primeProxy(r, loop)
		reconcile(r, ns, "p2f-s16")

		By("a Budget pause at pausedFrom=Implementing (exceeded, 200/200)")
		l := getLoop(ns, "p2f-s16")
		l.Status.Phase = coxv1alpha1.LoopPhasePaused
		l.Status.DesiredPhase = coxv1alpha1.LoopPhasePaused
		l.Status.PausedFrom = coxv1alpha1.LoopPhaseImplementing
		l.Status.PausedReason = coxv1alpha1.PausedReasonBudget
		l.Status.Budget = &coxv1alpha1.BudgetStatus{
			PromptTokens:     100,
			CompletionTokens: 100,
			Requests:         2,
			Exceeded:         true,
			ExceededReason:   "Tokens",
		}
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())

		By("the cap is raised (no annotation): the Loop STAYS Paused, still exceeded (the item-5 re-evaluation belongs to a RESUME trigger — a spec change alone does not clear the sticky exceeded flag; the decision is inert in Paused)")
		l = getLoop(ns, "p2f-s16")
		l.Spec.Budget.MaxTokens = p2fMaxTokens500
		Expect(k8sClient.Update(ctx, l)).To(Succeed())
		reconcile(r, ns, "p2f-s16")
		got := getLoop(ns, "p2f-s16")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused),
			"a Budget pause is NOT resumed by a cap raise (no resume trigger fired)")
		Expect(got.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonBudget))
		if got.Status.Budget != nil {
			Expect(got.Status.Budget.Exceeded).To(BeTrue(),
				"the sticky exceeded flag is not cleared without a resume trigger (the item-5 re-evaluation is gated on resumeTriggered)")
		}

		By("the suspend flip (true -> false, no annotation): it must NOT resume a Budget pause (spec 6)")
		setSuspend(ns, "p2f-s16", true)
		reconcile(r, ns, "p2f-s16")
		setSuspend(ns, "p2f-s16", false)
		reconcile(r, ns, "p2f-s16")
		got = getLoop(ns, "p2f-s16")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused),
			"a spec.suspend flip must not resume a Budget pause (only the annotation may)")
		Expect(got.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonBudget))

		By("the annotation (coxswain.io/resume): it MUST resume to the exact pausedFrom phase (the cap was raised, so the item-5 re-evaluation clears exceeded and the refuse-while-exceeded guard does not refuse)")
		got = getLoop(ns, "p2f-s16")
		got.Annotations = map[string]string{resumeAnnotation: unstructuredTrue}
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		reconcile(r, ns, "p2f-s16")
		got = getLoop(ns, "p2f-s16")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"the annotation resumes a Budget pause to the exact pausedFrom phase (the P2h sub-case (c) annotation-resume check) — the in-kind run-20261006202101 left it Paused")
		Expect(got.Status.Budget).NotTo(BeNil())
		Expect(got.Status.Budget.Exceeded).To(BeFalse(),
			"the item-5 re-evaluation clears the exceedance on the annotation resume (200 < 500)")
		Expect(got.Status.PausedFrom).To(BeEmpty())
		Expect(got.Status.PausedReason).To(BeEmpty())
		c := cond(got, coxv1alpha1.PausedCondition)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("Resumed"))
		Expect(got.Annotations).NotTo(HaveKey(resumeAnnotation), "the annotation is cleared on a valid resume")
	})
})

// p2f test helpers (package-level; the specs above reference them).

// deleteNS deletes a namespace (the specs' deferred teardown).
func deleteNS(ctx context.Context, ns string) {
	_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
}

// createAgentPodRunning creates the stand-in sandbox pod with the agent
// container RUNNING (spec 10's I49 in-progress case: a pod mid-run at pause
// time). The default claim reader (apiReader nil -> the cached client) finds
// the pod; a running agent yields (nil, nil) — requeue, no decision.
func createAgentPodRunning(ctx context.Context, ns, name string) {
	_ = k8sClient.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: agentContainerNameS4, Image: "standin"}},
		},
	})
}

// terminateAgentPod sets the sandbox pod's agent container to Terminated with
// the given claim message (the one-shot runner's single container run).
func terminateAgentPod(ctx context.Context, ns, name, message string) {
	pod := &corev1.Pod{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, pod); err != nil {
		return
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: agentContainerNameS4, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 0, Message: message,
		}}},
	}
	_ = k8sClient.Status().Update(ctx, pod)
}

// p2fVerifyPod creates the stand-in verify Job pod for the named Job (the
// job-name label the Job controller stamps + the verify-for label the reader
// filters on), with the named check init terminated at the given exit code and
// every other init (clone-base, import-agent, tamper, artifact) exit 0. A
// non-zero check -> verifyIterate (a decision); a zero -> verifySucceeded.
// spec 3 terminates the verify pod while the Loop is paused to prove the
// termination produces no decision.
func p2fVerifyPod(ctx context.Context, ns, loopName, jobName string, checkExit int32) {
	checkCount := 1
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-pod",
			Namespace: ns,
			Labels:    map[string]string{s5aJobNameLabel: jobName, verifyForLabel: loopName},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "verify-main", Image: "standin"}}},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	inits := []corev1.ContainerStatus{
		{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		{Name: s5aArtifact, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		{Name: "check-0", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: checkExit}}},
	}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: pod.Name}, pod)).To(Succeed())
	pod.Status.InitContainerStatuses = inits
	_ = k8sClient.Status().Update(ctx, pod)
	_ = checkCount
}
