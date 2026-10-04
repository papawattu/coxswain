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

// S5a (OS5 P3): the consumed-claim discard — a SUCCESS claim whose
// observedPhase no longer matches the CURRENT phase has already been
// consumed (its advance happened in an earlier reconcile and moved the
// phase away; the pod has not been recycled yet) and must NOT be
// re-consumed on the next reconcile: no progress write, no advance, no
// event. The kind event stream showed the failure mode: a spurious
// 'Planning -> Implementing' PhaseAdvanced at the verify iterate, where the
// prior cycle's PLANNING success claim (still on the pod, iteration marker
// not yet re-written) was re-consumed — nextPhase(Implementing,
// Implementing) advanced to Verifying and the event's from-phase was
// whatever the reconciler's phase was at that moment, never the claim's
// actual advance (Planning -> Implementing).
package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
)

var _ = Describe("S5a: consumed-claim discard (OS5 P3)", func() {
	var (
		ctx   context.Context
		r     *LoopReconciler
		claim *PhaseClaim
	)

	BeforeEach(func() {
		ctx = context.Background()
		recorder := record.NewFakeRecorder(64)
		r = &LoopReconciler{
			Recorder: recorder,
			now:      func() metav1.Time { return metav1.NewTime(time.Now().Truncate(time.Second)) },
			// The claim is swapped per-case via the closure below (the seam
			// reads the outer variable at call time).
			readPhaseClaim: func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
				return claim, nil
			},
		}
	})

	// swapClaim replaces the served claim.
	swapClaim := func(c *PhaseClaim) {
		claim = c
	}

	It("discards a consumed success claim (observedPhase != current phase): no progress, no advance, no event", func() {
		// The prior cycle has JUST iterated back: status is Implementing
		// (iteration 2, pin cleared), and the pod still holds the prior
		// PLANNING success claim. This spec isolates the CONSUMED-claim
		// discard guard (S5a OS5 P3): the claim's iteration matches the
		// current status iteration (2 == 2 — post-S5a, phase-init writes the
		// CURRENT iteration into .coxswain/iteration on the recycle, so the
		// new-cycle claim carries iteration 2), so the stale-iteration guard
		// does NOT fire and the discard guard is the only line that catches
		// the re-consumption. (The exact kind shape — an iteration-0 claim at
		// status iteration 2 — is caught by the stale-iteration guard
		// instead; I48 extended it to cover iteration-0 claims once
		// status.iteration > 0, and the iteration-0 spec lives in
		// loop_verify_job_envtest_test.go.)
		// Without the discard guard, the claim is re-consumed:
		// claimPhaseForAdvance(Planning success) == Implementing, so the B1
		// match nextPhase(Implementing, Implementing) == Verifying ADVANCES
		// the machine and emits PhaseAdvanced with from-phase Implementing —
		// a spurious advance of a claim whose real advance (Planning ->
		// Implementing) happened one cycle ago.
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "cloop", Namespace: "discardns", Generation: 1},
			Status: coxv1alpha1.LoopStatus{
				Phase:        coxv1alpha1.LoopPhaseImplementing,
				DesiredPhase: coxv1alpha1.LoopPhaseImplementing,
				Iteration:    2,
				BaseCommit:   s5aBaseCommit,
			},
		}
		swapClaim(&PhaseClaim{ObservedPhase: coxv1alpha1.LoopPhasePlanning, Status: claimSuccess, Iteration: 2})

		// Reconcile 1: the consumed claim is discarded (observedPhase
		// Planning != current phase Implementing). No progress write, no
		// advance, no event. Mutation M5 (drop the discard guard): the claim
		// is re-consumed — the machine spuriously advances to Verifying (a
		// verify Job would then be built against the WRONG commit) and
		// progress re-stamps with the stale claim.
		pending, changed := r.advancePhaseFromClaim(ctx, loop)
		Expect(pending).To(BeFalse())
		Expect(changed).To(BeFalse(), "a consumed success claim must not re-stamp progress or re-advance (M5: drop the discard guard -> the stale Planning claim spuriously advances to Verifying)")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"no advance on a consumed claim (M5: the spurious advance lands on Verifying)")
		Expect(loop.Status.CurrentVerify).To(BeNil(), "no re-pin on a consumed claim")

		// The discard is STATELESS: the SAME claim is re-discarded on the
		// next reconcile (until the pod is recycled).
		pending, changed = r.advancePhaseFromClaim(ctx, loop)
		Expect(pending).To(BeFalse())
		Expect(changed).To(BeFalse(), "the discard is stateless: the SAME claim is re-discarded on the next reconcile")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing))

		// A blocked claim naming the CURRENT phase is STILL consumed and
		// recorded (OS1: the runner's last word is observability; a blocked
		// claim never advances, so re-stamping it is idempotent and correct).
		// The discard guard applies to SUCCESS claims only. Iteration 2
		// matches the current status iteration (the same-cycle marker).
		swapClaim(&PhaseClaim{ObservedPhase: coxv1alpha1.LoopPhaseImplementing, Status: "blocked", BlockedReason: "model timeout", Iteration: 2})
		pending, changed = r.advancePhaseFromClaim(ctx, loop)
		Expect(pending).To(BeFalse())
		Expect(changed).To(BeTrue(), "a BLOCKED claim on the current phase is still recorded into progress (OS1)")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing), "a blocked claim never advances")
		Expect(loop.Status.Progress).NotTo(BeNil())
		Expect(loop.Status.Progress.LastResultStatus).To(Equal("blocked"))
		Expect(loop.Status.Progress.BlockedReason).To(Equal("model timeout"))
	})
})
