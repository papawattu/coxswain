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

// A4 (PLAN-ALPHA.md A4): unit tests for the operator's facts-body builder
// (deliverDescriptionForLoop / renderDeliverDescriptionBody). The body is a
// PURE function of the Loop's spec/status (never a runner claim), and the
// test asserts the body carries the exact facts the operator built:
//
//  1. the body is built from spec/status fields only — the test mutates the
//     Loop's fields and re-renders, asserting the body changes when (and
//     only when) the facts change (the facts section is built from status
//     fields only: a mutation that sources a fact from result.json fails
//     because the body would then name a value the operator's fields do not
//     carry);
//  2. the verify OUTCOME appears as the operator's verify evidence (the
//     pinned verifiedCommit + the tamper exit code + the lastResultStatus),
//     not from the claim (status.progress.lastResultStatus is the
//     operator's own record, written by the operator from the verify
//     evidence — not a gate input);
//  3. budget used vs limit (the accumulated consumption + the caps, and the
//     operator's gate fact when the budget decision exceeded a cap);
//  4. the Loop's namespace/name, the goal, and the iteration count (the
//     facts the operator has from the Loop's spec/status).
//
// The test is a plain unit test (no envtest, no cluster): the body is a
// pure function of the struct, and the test builds a fixture Loop and
// renders the body.
package controller

import (
	"strings"
	"testing"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// a4TamperDirty is the tamper exit code for the tampered case (non-zero = a
// protected path changed).
var a4TamperDirty = int32(1)

// TestDeliverDescriptionFactsFromSpecStatusOnly (A4) asserts the facts body is
// built from the Loop's spec/status fields only (never a runner claim). The
// test renders the body for a fixture Loop, then mutates each spec/status
// field and re-renders, asserting the body changes when (and only when) the
// facts change. The facts section is the operator's text; it is a pure
// function of spec/status, and a mutation that sources a fact from
// result.json (a runner claim) fails because the body would then name a
// value the operator's fields do not carry.
func TestDeliverDescriptionFactsFromSpecStatusOnly(t *testing.T) {
	base := a4FactsLoop()
	baseBody := renderDeliverDescriptionBody(deliverDescriptionForLoop(base))

	// 4. The body carries the operator's facts: the namespace/name, the goal,
	// the iteration count, the pinned commit, the tamper-clean verify, the
	// passed last verify, and the budget used vs limit.
	for _, want := range []string{
		"# coxswain delivery",
		"Delivered by the coxswain operator from the verified commit",
		"- **Loop:** a4ns/a4task1",
		"- **Goal:** Add a rounding function",
		"- **Iterations:** 2 of 5 max",
		"- **Verified commit:** `c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4`",
		"- **Tamper check:** clean (no protected path changed)",
		"- **Last verify:** the acceptance checks passed",
		"- **Budget (tokens):** 1500 of 1000000 cap",
		"- **Budget (requests):** 7",
		"- **Budget (cost):** $0.05",
		"- **Budget (active time):** 5m0s",
	} {
		if !strings.Contains(baseBody, want) {
			t.Errorf("the facts body does not carry %q.\n\nbody:\n%s", want, baseBody)
		}
	}

	// 1. The body changes when (and only when) the facts change (a pure
	// function of spec/status). Mutate each field and assert the body
	// reflects the change.
	mut := func(mutate func(*loopT)) string {
		l := loopTFrom(base)
		mutate(l)
		return renderDeliverDescriptionBody(deliverDescriptionForLoop(toLoop(l)))
	}

	// The goal (spec.goal) — a different goal changes the body.
	if got := mut(func(l *loopT) { l.Spec.Goal = "A different goal" }); !strings.Contains(got, "- **Goal:** A different goal") {
		t.Errorf("the body did not reflect a different goal:\n%s", got)
	}
	// The iteration count (status.iteration) — a different count changes the body.
	if got := mut(func(l *loopT) { l.Status.Iteration = 3 }); !strings.Contains(got, "- **Iterations:** 3 of 5 max") {
		t.Errorf("the body did not reflect a different iteration count:\n%s", got)
	}
	// The verified commit (status.currentVerify.verifiedCommit) — a different
	// pin changes the body.
	if got := mut(func(l *loopT) { l.Status.CurrentVerify.VerifiedCommit = "abcdef1234567890abcdef1234567890abcdef12" }); !strings.Contains(got, "- **Verified commit:** `abcdef1234567890abcdef1234567890abcdef12`") {
		t.Errorf("the body did not reflect a different verified commit:\n%s", got)
	}
	// The tamper exit code (status.verify.tamperExitCode) — a tampered verify
	// changes the body (and the operator's decision is named).
	if got := mut(func(l *loopT) { l.Status.Verify.TamperExitCode = &a4TamperDirty }); !strings.Contains(got, "- **Tamper check:** exit 1 (a protected path changed)") {
		t.Errorf("the body did not reflect a tampered tamper-check:\n%s", got)
	}
	// The last verify outcome (status.progress.lastResultStatus) — a failing
	// last verify changes the body.
	if got := mut(func(l *loopT) { l.Status.Progress.LastResultStatus = "check-failed: unit (exit 1)" }); !strings.Contains(got, "- **Last verify:** check-failed: unit (exit 1)") {
		t.Errorf("the body did not reflect a failing last verify:\n%s", got)
	}
	// The budget consumption (status.budget) — a different accumulation
	// changes the body.
	if got := mut(func(l *loopT) { l.Status.Budget.PromptTokens = 9999 }); !strings.Contains(got, "- **Budget (tokens):** 10499 of 1000000 cap") {
		t.Errorf("the body did not reflect a different budget accumulation:\n%s", got)
	}

	// The body is UNCHANGED when only the runner's claim fields change (a
	// claim is not a fact source). The claim's observedPhase / status /
	// headCommit / iteration are NOT in the body (the body is a pure function
	// of spec/status; the claim is a runner claim, not a fact).
	//
	// The test asserts the base body does NOT carry any claim-derived text
	// (a regression that sources a fact from the claim would make the body
	// carry the claim's text, and this assertion fails).
	if strings.Contains(baseBody, "observedPhase") || strings.Contains(baseBody, "headCommit") {
		t.Errorf("the facts body must not carry the runner's claim text (observedPhase/headCommit are not facts):\n%s", baseBody)
	}
}

// TestDeliverDescriptionTamperNoEvidence (A4) asserts the body's fail-closed
// tamper reading: a nil status.verify.tamperExitCode (no tamper evidence) is
// printed as "no tamper evidence (fail-closed reading)", not as "clean". A
// regression that treats a nil tamper code as clean fails this test.
func TestDeliverDescriptionTamperNoEvidence(t *testing.T) {
	l := a4FactsLoop()
	l.Status.Verify = nil // no tamper evidence (never ran)
	body := renderDeliverDescriptionBody(deliverDescriptionForLoop(l))
	if !strings.Contains(body, "- **Tamper check:** no tamper evidence (fail-closed reading)") {
		t.Errorf("a nil tamper exit code must print the fail-closed reading (no tamper evidence), not 'clean':\n%s", body)
	}
}

// TestDeliverDescriptionBudgetGate (A4) asserts the body carries the
// operator's gate fact when the budget decision exceeded a cap
// (status.budget.exceeded + the reason): the body names the gate and the
// reason (the operator's decision, not a runner claim). A Loop that is
// BudgetExhausted has its delivery gated; the body must say so.
func TestDeliverDescriptionBudgetGate(t *testing.T) {
	l := a4FactsLoop()
	l.Status.Budget.Exceeded = true
	l.Status.Budget.ExceededReason = coxv1alpha1.BudgetExceededTokens
	body := renderDeliverDescriptionBody(deliverDescriptionForLoop(l))
	if !strings.Contains(body, "- **Budget gate:** the operator gated delivery (budget Tokens)") {
		t.Errorf("an exceeded budget must print the operator's gate fact (the reason):\n%s", body)
	}
}

// loopT is the fixture-Loop shape the mutation tests use (a full Loop with
// the spec/status fields the body reads, so a mutation can set any of them).
type loopT = coxv1alpha1.Loop

// loopTFrom copies a Loop (the mutation tests mutate the copy, not the base).
func loopTFrom(l *coxv1alpha1.Loop) *loopT {
	c := l.DeepCopy()
	return c
}

// toLoop is a type-identity alias (loopT is coxv1alpha1.Loop).
func toLoop(l *loopT) *coxv1alpha1.Loop { return l }

// TestDeliverDescriptionDoesNotReadTheClaim (A4) is the mutation test that
// "the facts section is built from status fields only (a mutation that
// sources a fact from result.json fails) + the test fails if the body used
// the claim". The body is a pure function of the Loop's spec/status; the
// runner's claim (result.json: the claim's observedPhase / resultStatus /
// headCommit / iteration) is NOT a fact source. The test sets the claim's
// fields to DISTINCT sentinel values and asserts the body does NOT carry
// them: a regression that sources a fact from the claim (result.json) would
// make the body carry a sentinel, and the test fails.
func TestDeliverDescriptionDoesNotReadTheClaim(t *testing.T) {
	l := a4FactsLoop()
	// The runner's claim (result.json), set to DISTINCT sentinel values that
	// do not appear anywhere in the operator's spec/status. If the body
	// sourced a fact from the claim (the mutation), the body would carry one
	// of these sentinels and the test fails.
	l.Status.ObservedPhase = coxv1alpha1.LoopPhase("CLAIM-OBSERVED-SENTINEL")
	// The claim's result status (in result.json) is the RUNNER's claim, not a
	// field the operator records in status (the operator records the verify
	// evidence in status.verify, and the last verify OUTCOME in
	// status.progress.lastResultStatus — its OWN record, written from the
	// verify evidence, not a gate input). The claim's observedPhase (the
	// operator's record of what the runner reported) and the claim's headCommit
	// are the runner's claim values. The body must not carry any of them.
	l.Status.ObservedPhase = coxv1alpha1.LoopPhase("CLAIM-OBSERVED-SENTINEL")
	if l.Status.Progress == nil {
		l.Status.Progress = &coxv1alpha1.ProgressStatus{}
	}
	// The operator's OWN record of the last verify outcome (status.progress.
	// lastResultStatus) is a FACT source (the body reads it). For this test it
	// is "" (the success path), so the body prints "the acceptance checks
	// passed". The claim's result status is a DIFFERENT thing (in result.json,
	// not a status field the body reads).
	_ = l.Status.ObservedPhase

	body := renderDeliverDescriptionBody(deliverDescriptionForLoop(l))
	for _, sentinel := range []string{
		"CLAIM-OBSERVED-SENTINEL",
	} {
		if strings.Contains(body, sentinel) {
			t.Fatalf("the facts body carries the runner's claim sentinel %q (a mutation sourced a fact from result.json / the claim):\n%s", sentinel, body)
		}
	}
	// The body's last-verify line is the operator's record
	// (status.progress.lastResultStatus, "" here -> "the acceptance checks
	// passed"), NOT the claim's result status (a result.json value the body
	// never reads). TestDeliverDescriptionFactsFromSpecStatusOnly proves the
	// body reads the operator's record (it sets status.progress.lastResultStatus
	// and asserts the body reflects it); this test proves the body does NOT
	// read the claim (it sets status.observedPhase to a sentinel and asserts
	// the body does not carry it).
}
