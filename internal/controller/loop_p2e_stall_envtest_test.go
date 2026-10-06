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

// P2e stall gate envtest specs (item 2). The gate is exercised through the
// REAL reconciler (r.applyStallGate) with the readCheckOutput seam driving a
// deterministic failing-check output (no real verify Job / pod-log read):
// N consecutive identical verify failures fire the stall detector (Fail →
// Failed:Stalled; Pause → Paused:Stall; Continue → the Stalled condition is
// set and the loop keeps iterating); a differing output does NOT fire; the
// history is dedup'd by jobName and ring-capped; the default read is the
// terminationMessage (no pod-log read); and the stall gate wins over the
// budget cap (a capped loop that is also stalled is Failed:Stalled, not
// budget-capped). These are the gate specs; the gate-mutations (stall
// disabled, stall loses to budget, readCheckOutput removed) are run in the
// scratch worktree and recorded in .samples/p2e/mutations.md.
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

// p2eOutputA / p2eOutputB are two raw check outputs that normalise to
// DIFFERENT hashes (different failing assertion → different substance) — the
// "differing output" specs drive the run to RESET. p2eOutputRepeated is a
// single raw output repeated across iterations (the "N consecutive identical"
// specs): its normalised hash is stable (the internal/stall normaliser).
const (
	p2eOutputA        = "FAIL: TestBuild\nmain.go:10: assertion failed (got 1, want 2)\n"
	p2eOutputB        = "FAIL: TestDeploy\nkube.go:99: connection refused\n"
	p2eOutputRepeated = "FAIL: TestBuild\nmain.go:10: assertion failed (got 1, want 2)\n"
)

var _ = Describe("P2e: stall gate (N consecutive identical verify failures)", func() {
	ctx := context.Background()

	// p2eNs creates a fresh namespace for a spec (the specs defer
	// deleteNS(ctx, ns)).
	p2eNs := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	// newP2eReconciler builds a bare reconciler (no full reconcile drive —
	// the gate is exercised directly) with the readCheckOutput seam set to a
	// fixed output for the named check.
	newP2eReconciler := func(checkOutput string) *LoopReconciler {
		return &LoopReconciler{
			readCheckOutput: func(pod *corev1.Pod, checkName string) (string, bool) {
				return checkOutput, true
			},
		}
	}

	// aVerifyingLoop returns a Loop at Verifying with the given iteration and
	// stallAfter / stallAction.
	aVerifyingLoop := func(stallAfter int32, action coxv1alpha1.StallAction) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "stalllp", Namespace: ""},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "P2e stall spec",
				Workspace: testWorkspace(),
				Loop: coxv1alpha1.LoopSettings{
					StallAfter:  &stallAfter,
					StallAction: action,
				},
			},
			Status: coxv1alpha1.LoopStatus{
				Phase:     coxv1alpha1.LoopPhaseVerifying,
				Iteration: 1,
			},
		}
	}

	// aFailingPod returns a verify pod with check-0 failed (exit 1) and a
	// finish time (the entry's At).
	aFailingPod := func() *corev1.Pod {
		fin := metav1.NewTime(time.Unix(1700000000, 0))
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "stalllp-verify-1", Namespace: ""},
			Status: corev1.PodStatus{
				InitContainerStatuses: []corev1.ContainerStatus{
					{Name: verifyTamperInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
					{Name: verifyArtifactInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
					{Name: s5aCheck0, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, FinishedAt: fin}}},
				},
			},
		}
	}

	getLoop := func(ns, name string) *coxv1alpha1.Loop {
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
		return got
	}

	// createStallLoop creates a Loop in ns with the given spec (status set to
	// Verifying/iteration 1) and returns it.
	createStallLoop := func(ns string, spec coxv1alpha1.LoopSpec) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "stalllp", Namespace: ns},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		l := getLoop(ns, "stalllp")
		l.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		l.Status.DesiredPhase = coxv1alpha1.LoopPhaseVerifying
		l.Status.Iteration = 1
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
		return l
	}

	// conditionTrue returns the condition of the given type with status True
	// (absent = nil).
	conditionTrue := func(loop *coxv1alpha1.Loop, condType string) *metav1.Condition {
		for i := range loop.Status.Conditions {
			c := &loop.Status.Conditions[i]
			if c.Type == condType && c.Status == metav1.ConditionTrue {
				return c
			}
		}
		return nil
	}

	// runFailures drives applyStallGate for iterations [1..n] with the fixed
	// reconciler (same output each iteration), returning the last fired bool.
	runFailures := func(r *LoopReconciler, ns string, n int) bool {
		var fired bool
		for iter := 1; iter <= n; iter++ {
			loop := getLoop(ns, "stalllp")
			loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
			loop.Status.Iteration = iter
			Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
			fired = r.applyStallGate(ctx, loop, aFailingPod(), s5aCheck0)
			Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		}
		return fired
	}

	It("fails the loop after N consecutive identical verify failures (stallAction=Fail)", func() {
		ns := p2eNs("p2e-s1")
		defer deleteNS(ctx, ns)
		r := newP2eReconciler(p2eOutputRepeated)
		spec := aVerifyingLoop(3, coxv1alpha1.StallActionFail).Spec
		createStallLoop(ns, spec)

		fired := runFailures(r, ns, 3)
		Expect(fired).To(BeTrue(), "the 3rd consecutive identical failure must fire (N=3)")

		loop := getLoop(ns, "stalllp")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed))
		cond := conditionTrue(loop, string(coxv1alpha1.LoopPhaseFailed))
		Expect(cond).ToNot(BeNil())
		Expect(cond.Reason).To(Equal("Stalled"))
	})

	It("pauses the loop (Paused:Stall) after N consecutive identical failures (stallAction=Pause)", func() {
		ns := p2eNs("p2e-s2")
		defer deleteNS(ctx, ns)
		r := newP2eReconciler(p2eOutputRepeated)
		spec := aVerifyingLoop(3, coxv1alpha1.StallActionPause).Spec
		createStallLoop(ns, spec)

		fired := runFailures(r, ns, 3)
		Expect(fired).To(BeTrue())
		loop := getLoop(ns, "stalllp")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePaused))
		Expect(loop.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonStall))
		cond := conditionTrue(loop, string(coxv1alpha1.StalledCondition))
		Expect(cond).ToNot(BeNil())
	})

	It("keeps the loop iterating (the Stalled condition is set) when stallAction=Continue", func() {
		ns := p2eNs("p2e-s3")
		defer deleteNS(ctx, ns)
		r := newP2eReconciler(p2eOutputRepeated)
		spec := aVerifyingLoop(3, coxv1alpha1.StallActionContinue).Spec
		createStallLoop(ns, spec)

		fired := runFailures(r, ns, 3)
		Expect(fired).To(BeFalse(), "stallAction=Continue keeps the loop iterating (does not take the decision)")
		loop := getLoop(ns, "stalllp")
		Expect(loop.Status.Phase).ToNot(Equal(coxv1alpha1.LoopPhaseFailed))
		cond := conditionTrue(loop, string(coxv1alpha1.StalledCondition))
		Expect(cond).ToNot(BeNil(), "the Stalled condition is set even when the loop keeps going")
	})

	It("does NOT fire on a differing verify-failure output (the run resets)", func() {
		ns := p2eNs("p2e-s4")
		defer deleteNS(ctx, ns)
		// Alternate A, B, A, B, A: the run never reaches N=3 (each output
		// resets the consecutive run to 1).
		outputs := []string{p2eOutputA, p2eOutputB, p2eOutputA, p2eOutputB, p2eOutputA}
		spec := aVerifyingLoop(3, coxv1alpha1.StallActionFail).Spec
		createStallLoop(ns, spec)

		for i, out := range outputs {
			r := newP2eReconciler(out)
			loop := getLoop(ns, "stalllp")
			loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
			loop.Status.Iteration = i + 1
			Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
			fired := r.applyStallGate(ctx, loop, aFailingPod(), s5aCheck0)
			Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
			Expect(fired).To(BeFalse(), "output %d (A/B alternating) must not fire", i)
		}
		loop := getLoop(ns, "stalllp")
		Expect(loop.Status.Phase).ToNot(Equal(coxv1alpha1.LoopPhaseFailed))
	})

	It("ring-caps status.stallHistory at the last 10 (the older entries evict)", func() {
		ns := p2eNs("p2e-s5")
		defer deleteNS(ctx, ns)
		r := newP2eReconciler(p2eOutputRepeated)
		spec := aVerifyingLoop(999, coxv1alpha1.StallActionFail).Spec // a high N so it never fires
		createStallLoop(ns, spec)

		var fired bool
		for iter := 1; iter <= 12; iter++ {
			loop := getLoop(ns, "stalllp")
			loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
			loop.Status.Iteration = iter
			Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
			fired = r.applyStallGate(ctx, loop, aFailingPod(), s5aCheck0)
			Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		}
		Expect(fired).To(BeFalse(), "N=999 never fires")
		loop := getLoop(ns, "stalllp")
		Expect(loop.Status.StallHistory).To(HaveLen(10), "the ring is capped at the last 10")
		Expect(loop.Status.StallHistory[0].JobName).To(Equal("stalllp-verify-3"))
		Expect(loop.Status.StallHistory[9].JobName).To(Equal("stalllp-verify-12"))
	})

	It("dedups by jobName (a re-read of the same verify Job appends no entry)", func() {
		ns := p2eNs("p2e-s6")
		defer deleteNS(ctx, ns)
		r := newP2eReconciler(p2eOutputRepeated)
		spec := aVerifyingLoop(999, coxv1alpha1.StallActionFail).Spec
		createStallLoop(ns, spec)

		loop := getLoop(ns, "stalllp")
		loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		loop.Status.Iteration = 1
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		// Two reads of the SAME verify Job (same jobName) → one entry.
		r.applyStallGate(ctx, loop, aFailingPod(), s5aCheck0)
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		loop = getLoop(ns, "stalllp")
		r.applyStallGate(ctx, loop, aFailingPod(), s5aCheck0)
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		loop = getLoop(ns, "stalllp")
		Expect(loop.Status.StallHistory).To(HaveLen(1), "a re-read of the same jobName appends no entry")
	})

	It("wins over the budget cap (a capped loop that is also stalled is Failed:Stalled)", func() {
		// The stall gate is evaluated BEFORE the maxIterations cap in
		// applyVerifyOutcome (stall wins). This spec pins the precedence by
		// driving the gate at the SAME iteration the cap would fail, and
		// asserting the Stalled reason (not MaxIterationsExceeded).
		ns := p2eNs("p2e-s7")
		defer deleteNS(ctx, ns)
		r := newP2eReconciler(p2eOutputRepeated)
		stallAfter := int32(1) // N=1: a single failure fires the stall gate
		maxIter := int(1)      // the cap is ALSO reached at iteration 1
		base := aVerifyingLoop(stallAfter, coxv1alpha1.StallActionFail).Spec
		base.Loop.MaxIterations = maxIter
		createStallLoop(ns, base)

		loop := getLoop(ns, "stalllp")
		loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		loop.Status.Iteration = 1
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		fired := r.applyStallGate(ctx, loop, aFailingPod(), s5aCheck0)
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		Expect(fired).To(BeTrue(), "the stall gate fires before the budget cap (stall wins)")
		loop = getLoop(ns, "stalllp")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed))
		cond := conditionTrue(loop, string(coxv1alpha1.LoopPhaseFailed))
		Expect(cond.Reason).To(Equal("Stalled"), "stall wins: Failed:Stalled, not the budget reason")
	})

	It("reads the check output from the terminationMessage (defaultReadCheckOutput, no pod-log read)", func() {
		// A bare reconciler (no seam) uses defaultReadCheckOutput: the check
		// container's LastTerminationState.Terminated.Message (the
		// terminationMessage). The spec drives a pod whose check-0 carries a
		// terminationMessage.
		r := &LoopReconciler{}
		pod := aFailingPod()
		for i := range pod.Status.InitContainerStatuses {
			ics := &pod.Status.InitContainerStatuses[i]
			if ics.Name == s5aCheck0 {
				ics.LastTerminationState = corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1,
						Message:  p2eOutputA,
					},
				}
			}
		}
		raw, ok := r.defaultReadCheckOutput(pod, s5aCheck0)
		Expect(ok).To(BeTrue())
		Expect(raw).To(Equal(p2eOutputA))

		// A pod with no termination message returns ("", false) — an empty
		// raw (the normaliser of "" is ""; a no-output hot loop still stalls).
		podNoMsg := aFailingPod()
		raw2, ok2 := r.defaultReadCheckOutput(podNoMsg, s5aCheck0)
		Expect(ok2).To(BeFalse())
		Expect(raw2).To(BeEmpty())
	})
})
