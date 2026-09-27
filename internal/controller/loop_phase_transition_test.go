/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// B1: the operator advances the phase machine when the runner reports that it
// finished the phase the operator asked it to do (observedPhase == desiredPhase).
// The happy-path forward table is:
//
//	Pending  -> Planning
//	Planning -> Implementing
//	Implementing -> Verifying
//	Verifying -> Succeeded
//
// The iterate (Verifying -> Implementing) and terminal (-> Failed) branches
// depend on the verify outcome (B3) and iteration count (B4); they are added in
// those slices, not here. nextPhase is a pure function of the two phases the
// operator tracks, so it is unit-testable without a cluster.
var _ = Describe("nextPhase (B1 transition table)", func() {
	It("advances Pending -> Planning when the runner reports Planning done", func() {
		got := nextPhase(coxv1alpha1.LoopPhasePending, coxv1alpha1.LoopPhasePlanning)
		Expect(got).To(Equal(coxv1alpha1.LoopPhasePlanning))
	})

	It("advances Planning -> Implementing when the runner reports Implementing done", func() {
		got := nextPhase(coxv1alpha1.LoopPhasePlanning, coxv1alpha1.LoopPhaseImplementing)
		Expect(got).To(Equal(coxv1alpha1.LoopPhaseImplementing))
	})

	It("advances Implementing -> Verifying when the runner reports Verifying done", func() {
		got := nextPhase(coxv1alpha1.LoopPhaseImplementing, coxv1alpha1.LoopPhaseVerifying)
		Expect(got).To(Equal(coxv1alpha1.LoopPhaseVerifying))
	})

	It("does NOT let the runner's report move Verifying -> Succeeded (D23: Succeeded is evidence-gated)", func() {
		// ADR-0004/0005: observedPhase is the runner's *claim*, and no gate
		// decision may be made on a claim. Succeeded is the gate — it must follow
		// only from verify-Job evidence (B2/B3/B3a: tamper exit 0, canary
		// behaving, every check exit 0), never from the runner reporting
		// Succeeded. So a report of Succeeded while in Verifying leaves the
		// operator in Verifying; the exit is decided by verifyOutcome(job) in B3.
		Expect(nextPhase(coxv1alpha1.LoopPhaseVerifying, coxv1alpha1.LoopPhaseSucceeded)).
			To(Equal(coxv1alpha1.LoopPhaseVerifying),
				"the runner's claim must never complete the Loop; only verify evidence can")
		// Likewise a report of Failed/Implementing from Verifying is a claim, not
		// an operator decision — nextPhase handles no exit from Verifying at all.
		Expect(nextPhase(coxv1alpha1.LoopPhaseVerifying, coxv1alpha1.LoopPhaseFailed)).
			To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(nextPhase(coxv1alpha1.LoopPhaseVerifying, coxv1alpha1.LoopPhaseImplementing)).
			To(Equal(coxv1alpha1.LoopPhaseVerifying))
	})

	It("does not advance when the runner's report is not the immediate next phase", func() {
		// The operator is at Planning; the only valid forward step from there is
		// Implementing. A report of Verifying (two steps ahead) or Succeeded
		// (skipping ahead) is not a valid single-step advance, so the operator
		// stays put.
		Expect(nextPhase(coxv1alpha1.LoopPhasePlanning, coxv1alpha1.LoopPhaseVerifying)).
			To(Equal(coxv1alpha1.LoopPhasePlanning))
		Expect(nextPhase(coxv1alpha1.LoopPhasePlanning, coxv1alpha1.LoopPhaseSucceeded)).
			To(Equal(coxv1alpha1.LoopPhasePlanning))
	})

	It("is a no-op for terminal phases", func() {
		Expect(nextPhase(coxv1alpha1.LoopPhaseSucceeded, coxv1alpha1.LoopPhaseSucceeded)).
			To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		Expect(nextPhase(coxv1alpha1.LoopPhaseFailed, coxv1alpha1.LoopPhaseFailed)).
			To(Equal(coxv1alpha1.LoopPhaseFailed))
	})
})
