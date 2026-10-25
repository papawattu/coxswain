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

// A4 (PLAN-ALPHA.md A4): the deliver Job's self-explaining PR description —
// the FACTS half, generated operator-side.
//
// The PR description is built in two halves. The FACTS (this file) come from
// the Loop's spec/status ONLY and are rendered to a plain-text body by the
// operator (buildDeliverDescriptionBody); the plan (the AGENT-AUTHORED
// .coxswain/PLAN.md the deliver Job reads from the workspace PVC) is treated
// as UNTRUSTED DATA by the push script: it is fenced into a clearly labelled
// section, capped at deliverPlanMaxBytes with a truncation note, and never
// interleaved with the facts. The push container assembles the final JSON
// payload with python3 json (the push image has python3; the existing script
// already parses JSON with it — the facts body is carried as the script's
// $DESCRIPTION_BODY environment variable, never interpolated into the JSON
// by hand: the payload is JSON-escaped by the library, and a printf of
// untrusted text into JSON is impossible by construction).
//
// Facts provenance (A4 acceptance "the facts section is built from status
// fields only"): the inputs to buildDeliverDescription are
//
//   - spec (goal, the delivery config, the budget caps) — the desired state,
//     never a runner claim;
//   - status.iteration (the operator's own iteration count, B4 — the claim
//     only advances the phase machine, the count is the operator's);
//   - status.verify + status.currentVerify (the verify evidence: the pinned
//     verifiedCommit and the tamper exit code — kubelet-recorded, ADR-0005
//     D14, never a result.json claim);
//   - status.progress.lastResultStatus (the operator's own record of the LAST
//     verify outcome — written by the operator from the verify Job pod's exit
//     codes, OS1; a Loop that reaches Succeeded carries "" (the success path
//     never sets it) or the last failing check's "check-failed: <name>
//     (exit <n>)" record if it iterated);
//   - status.budget (the accumulated consumption the operator derives from
//     the metering proxy endpoint, P2c/P2d) + spec.budget (the caps).
//
// Nothing read from result.json (the runner's claim) feeds the body: the
// claim's observedPhase/status fields are not referenced here at all, and
// status.progress is the operator's OWN structured record (written by the
// operator from the verify evidence, not a gate input — ADR-0005), so the
// per-iteration record (status.progress.lastResultStatus) is operator-side
// data by construction. The mutation that sources a fact from result.json
// (A4 acceptance) fails the specs because the body would then name a value
// the operator's fields do not carry.

package controller

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// deliverDescriptionConfigName is the deliver description ConfigMap's name:
// <loop>-deliver-description (derivedName: '<loop>-deliver-description',
// truncated+hashed only for the near-max names that would exceed the 63-char
// DNS-1035 budget).
func deliverDescriptionConfigName(loopName string) string {
	return derivedName(loopName, "-deliver-description")
}

// deliverDescriptionDataKey is the ConfigMap data key the operator writes the
// facts body under. The push container mounts it at /deliver-description and
// reads the key with cat (the value is plain text; the JSON encoding of the
// payload happens in python3, never in the ConfigMap value).
const deliverDescriptionDataKey = "description.md"

// deliverPlanMaxBytes caps the agent-authored PLAN.md content the push script
// embeds (A4: the plan is untrusted DATA — fenced, labelled, capped). The
// cap is generous (16 KiB) so a realistic plan fits whole; a longer plan is
// truncated to a whole line at the cap with a truncation note appended. The
// cap is applied by the push script (the operator never sees the plan — the
// plan lives only on the agent's workspace PVC and in the deliver Job's
// import; the operator's facts body has no plan content at all).
const deliverPlanMaxBytes = 16 * 1024

// deliverPlanCopyMaxBytes caps the plan BYTES import-agent carries into the
// scratch volume (head -c, at the import step). It is the plan cap plus
// slack (one line's worth of headroom, and the cap itself if the plan has no
// newlines before it) so the copy always contains enough of the plan for the
// push python's whole-line truncation to land the same cap — the python's
// PLAN_MAX (deliverPlanMaxBytes) is the binding display cap, and the copy
// cap only bounds the bytes the scratch volume carries.
const deliverPlanCopyMaxBytes = deliverPlanMaxBytes + 4096

// deliverPlanSectionHeader is the push script's label for the untrusted
// plan section (A4: the plan is clearly labelled as written by the agent,
// and fenced — markdown fences, which survive the JSON body verbatim). The
// push script's python emits this label verbatim (the label is a fixed
// string, not user data — it lives in the operator's python source, and the
// Go constant here is the single source of truth the specs assert against).
const deliverPlanSectionHeader = "## Plan (written by the agent, untrusted)\n\n```markdown"

// deliverDescription is the facts body the operator builds for the deliver
// Job (the PR description's facts half). Rendered from the Loop's
// spec/status only (see the file header for the provenance).
type deliverDescription struct {
	// Namespace/Name identify the Loop (A4: the Loop's namespace/name is a
	// fact, not a guess — the operator writes it from metadata).
	Namespace string
	Name      string
	// Goal is spec.goal, verbatim (size-bounded to a display line; the
	// operator does not truncate user spec content — the goal is the
	// Loop author's own text, and a huge goal is the author's problem).
	Goal string
	// Iteration is status.iteration (the operator's 0-based run count: 0 on a
	// first run, then 2, 3 … — 1 is skipped; F90).
	Iteration int
	// MaxIterations is spec.loop.maxIterations (0 when unset; the body
	// prints "unset").
	MaxIterations int
	// VerifiedCommit is status.currentVerify.verifiedCommit (the pin).
	VerifiedCommit string
	// TamperClean reports the last tamper-check outcome: 0 = clean
	// (status.verify.tamperExitCode 0), -1 = no evidence
	// (status.verify.tamperExitCode nil — fail-closed reading, printed as
	// "no tamper evidence"), >0 = tampered (a protected path changed; the
	// body names the exit code).
	TamperClean int32
	// LastVerifyOutcome is status.progress.lastResultStatus ("" for the
	// success path — printed as "the acceptance checks passed"; the failing
	// record when the Loop iterated).
	LastVerifyOutcome string
	// VerifyHistory is the per-iteration verify record (PLAN-ALPHA A4: "the
	// verify results per iteration"): the operator's status.stallHistory
	// verify-failure entries (one per failing iteration, in the ring's order
	// — oldest first) PLUS the final pass (the pinned verifiedCommit the
	// delivery names). Empty when the Loop succeeded on its first iteration
	// with no recorded stall (the body prints a single "iteration 1 passed"
	// line).
	VerifyHistory []verifyHistoryEntry
	// Budget carries the used-vs-limit facts (nil when status.budget is
	// nil: the body prints "budget not yet read").
	Budget *deliverBudgetFacts
}

// verifyHistoryEntry is one line of the facts body's per-iteration verify
// record. Either a FAILING iteration (from a status.stallHistory entry: the
// iteration number, the failing check, and when the verify Job finished —
// all operator-recorded, never a runner claim) or the FINAL PASS (the
// iteration the delivery names, with the pinned verifiedCommit).
type verifyHistoryEntry struct {
	// Iteration is the iteration number (0 for a first run, then 2, 3 … — 1 is
	// skipped; F90): the stallHistory entry's iteration for a failure, the
	// operator's status.iteration for the final pass).
	Iteration int
	// Check is the failing check's name (empty for the final pass).
	Check string
	// Pass is true for the final-pass entry (the verify that pinned
	// status.currentVerify.verifiedCommit) and false for a stallHistory
	// failure.
	Pass bool
	// VerifiedCommit is the pinned commit (the final pass only; empty for a
	// stallHistory failure — a failing verify never pins a commit).
	VerifiedCommit string
	// At is the verify Job pod's finish time (the stallHistory entry's
	// kubelet-recorded time for a failure; zero for the final pass, which
	// the body does not print a time for).
	At metav1.Time
}

// deliverBudgetFacts is the operator's used-vs-limit budget fact (A4: budget
// used vs limit). The limits come from spec.budget (nil fields = no cap,
// printed as "no cap"); the used values from status.budget.
type deliverBudgetFacts struct {
	// Tokens is the accumulated prompt+completion token total.
	Tokens int64
	// Requests is the accumulated metered-request count.
	Requests int64
	// Requests is the accumulated metered-request count.
	// CostUsd is the derived cost in USD ("" when unknown).
	CostUsd string
	// ActiveSeconds is the accumulated active time.
	ActiveSeconds int64
	// MaxTokens / MaxCostUsd / MaxWallClockSeconds are the caps (nil = no
	// cap; "" wall clock = no cap).
	MaxTokens           *int64
	MaxCostUsd          string
	MaxWallClockSeconds int64
	// Exceeded / ExceededReason are status.budget.exceeded* (sticky).
	Exceeded       bool
	ExceededReason coxv1alpha1.BudgetExceededReason
}

// deliverDescriptionForLoop builds the facts from the Loop's spec/status only.
// It reads NOTHING else: the body is a pure function of spec + status (the
// A4 spec asserts exactly this — the body changes when and only when the
// facts change, and never carries a runner claim).
func deliverDescriptionForLoop(loop *coxv1alpha1.Loop) deliverDescription {
	// I95: the PR body is rendered only when the deliver Job is (re)created
	// (the deliver description ConfigMap is ensured from ensureDeliver, before
	// the Job exists), while status.budget is accumulated on EVERY reconcile
	// (applyBudgetStep runs the wall-clock accumulation before the deliver
	// step). A Loop that takes minutes from Planning to Delivered therefore
	// rendered its body from a status.budget that stopped at the last
	// reconciliation BEFORE delivery was first expected (the S5 -> S6 phase
	// flip) — #166 printed "Budget (active time): 0s" because status.budget
	// was nil until that first Succeeded reconcile, whose wall-clock
	// accumulation adds 0 (no lastActiveStamp yet).
	//
	// A4 flake (fix/a4-flake): the wall-clock refresh (now - lastActiveStamp)
	// is the OPERATOR's job (ensureDeliverDescriptionConfig calls
	// refreshDeliverDescriptionBudget on the loop before rendering, and the
	// caller's status write persists the refreshed value). This function is
	// a PURE READER of the persisted status — it does NOT refresh. The
	// earlier implementation called refreshDeliverDescriptionBudget(loop)
	// here, which MUTATED the caller's loop IN PLACE; the caller's status
	// write then persisted the mutation, and a spec that read the loop back
	// and called this function again (to compute wantBody) measured
	// time.Since(mutatedStamp) — a SECOND mutation — and wantBody's budget
	// section diverged from the ConfigMap body (the A4 flake). Making the
	// reader pure + having the operator refresh before rendering (so the
	// refresh is persisted) makes the body deterministic: the spec reads the
	// persisted (already-refreshed) value and computes the same body as the
	// operator.
	// F83e: the pin is resolved ONCE, with the nil check the verify-history
	// append below needs anyway. A Loop with no pin has no verified commit to
	// name (the body prints "(none pinned)") and no verify record that can
	// speak for one: status.currentVerify is nil on every Loop that has not
	// reached Verifying, which is most of them.
	pin := ""
	if loop.Status.CurrentVerify != nil {
		pin = loop.Status.CurrentVerify.VerifiedCommit
	}
	// F83e: the tamper code counts only when the record belongs to THIS pin.
	// status.verify.verifiedCommit is the pin the operator copied when it
	// recorded the code (F83c), so a record whose commit differs — a forged or
	// legacy status: loops/status is user-writable, D85 — is not evidence about
	// this delivery and reads as "no evidence" (-1, the fail-closed reading).
	// `pin != ""` matters too: otherwise an empty record would match a Loop
	// with no pin.
	tamper := int32(-1)
	if v := loop.Status.Verify; v != nil && v.TamperExitCode != nil &&
		pin != "" && v.VerifiedCommit == pin {
		tamper = *v.TamperExitCode
	}
	last := ""
	if loop.Status.Progress != nil {
		last = loop.Status.Progress.LastResultStatus
	}
	budget := &deliverBudgetFacts{}
	if loop.Status.Budget != nil {
		b := loop.Status.Budget
		budget.Tokens = b.PromptTokens + b.CompletionTokens
		budget.Requests = b.Requests
		budget.CostUsd = b.CostUsd
		budget.ActiveSeconds = b.ActiveSeconds
		budget.Exceeded = b.Exceeded
		budget.ExceededReason = b.ExceededReason
	}
	if loop.Spec.Budget != nil {
		budget.MaxTokens = loop.Spec.Budget.MaxTokens
		budget.MaxCostUsd = string(loop.Spec.Budget.MaxCostUsd)
		if loop.Spec.Budget.MaxWallClock != "" {
			if d, err := durationToSeconds(loop.Spec.Budget.MaxWallClock); err == nil {
				budget.MaxWallClockSeconds = d
			}
		}
	}
	// The per-iteration verify record (A4 "the verify results per
	// iteration"): the operator's status.stallHistory verify-failure
	// entries (in the ring's order, oldest first) plus the final pass (the
	// iteration the delivery names, with the pinned verifiedCommit). A
	// stallHistory failure and the final pass are distinct: a failing
	// verify never pins a commit, and the final pass is the verify that did
	// pin one (status.currentVerify). Both are operator-side (the ring is
	// the operator's record of the verify Job pods' exit codes, and
	// status.currentVerify is the operator's pin) — never a runner claim.
	var verifyHistory []verifyHistoryEntry
	for i := range loop.Status.StallHistory {
		verifyHistory = append(verifyHistory, verifyHistoryEntry{
			Iteration: loop.Status.StallHistory[i].Iteration,
			Check:     loop.Status.StallHistory[i].Check,
			Pass:      false,
			At:        loop.Status.StallHistory[i].At,
		})
	}
	if loop.Status.CurrentVerify != nil && loop.Status.CurrentVerify.VerifiedCommit != "" {
		verifyHistory = append(verifyHistory, verifyHistoryEntry{
			Iteration:      loop.Status.Iteration,
			Pass:           true,
			VerifiedCommit: loop.Status.CurrentVerify.VerifiedCommit,
		})
	}
	return deliverDescription{
		Namespace:         loop.Namespace,
		Name:              loop.Name,
		Goal:              loop.Spec.Goal,
		Iteration:         loop.Status.Iteration,
		MaxIterations:     loop.Spec.Loop.MaxIterations,
		VerifiedCommit:    pin,
		TamperClean:       tamper,
		LastVerifyOutcome: last,
		VerifyHistory:     verifyHistory,
		Budget:            budget,
	}
}

// durationToSeconds converts a Go-duration string to whole seconds for the
// description (a display fact, not a gate input; a parse failure yields an
// error the caller treats as "no cap"). Distinct from the reconciler's
// parseMaxWallClockSeconds (the budget DECISION's parser, which returns 0 on
// error): the display path wants the error (a malformed cap prints "no cap",
// never a wrong number).
func durationToSeconds(s string) (int64, error) {
	var total int64
	i := 0
	for i < len(s) {
		digits := 0
		for i+digits < len(s) && s[i+digits] >= '0' && s[i+digits] <= '9' {
			digits++
		}
		if digits == 0 {
			return 0, fmt.Errorf("malformed duration part at %q", s[i:])
		}
		n, err := strconv.ParseInt(s[i:i+digits], 10, 64)
		if err != nil {
			return 0, err
		}
		i += digits
		unitStart := i
		for i < len(s) && (s[i] >= 'a' && s[i] <= 'z' || s[i] == 'µ') {
			i++
		}
		unit := s[unitStart:i]
		if unit == "" {
			return 0, fmt.Errorf("duration number without a unit: %q", s)
		}
		switch unit {
		case "ns":
			// Sub-second: display as 0s (a description fact, not a gate).
		case "us", "µs":
			// Sub-second.
		case "ms":
			if n >= 1000 {
				total += n / 1000
			}
		case "s":
			total += n
		case "m":
			total += n * 60
		case "h":
			total += n * 3600
		default:
			return 0, fmt.Errorf("unknown duration unit %q", unit)
		}
	}
	return total, nil
}

// renderDeliverDescriptionBody renders the facts body as the PR description
// (markdown). It is a PURE function of the struct (no clock, no I/O) so a
// spec can call it with a fixed fixture and assert the exact body — the
// facts section is the operator's text, and the body is deterministic.
func renderDeliverDescriptionBody(d deliverDescription) string {
	var b strings.Builder
	b.WriteString("# coxswain delivery\n\n")
	b.WriteString("Delivered by the coxswain operator from the verified commit\n\n")
	fmt.Fprintf(&b, "- **Loop:** %s/%s\n", d.Namespace, d.Name)
	fmt.Fprintf(&b, "- **Goal:** %s\n", oneLine(d.Goal))
	if d.MaxIterations > 0 {
		fmt.Fprintf(&b, "- **Iterations:** %d of %d max\n", d.Iteration, d.MaxIterations)
	} else {
		fmt.Fprintf(&b, "- **Iterations:** %d (maxIterations unset)\n", d.Iteration)
	}
	if d.VerifiedCommit == "" {
		b.WriteString("- **Verified commit:** (none pinned)\n")
	} else {
		fmt.Fprintf(&b, "- **Verified commit:** `%s`\n", d.VerifiedCommit)
	}
	switch {
	case d.TamperClean < 0:
		b.WriteString("- **Tamper check:** no tamper evidence (fail-closed reading)\n")
	case d.TamperClean == 0:
		b.WriteString("- **Tamper check:** clean (no protected path changed)\n")
	default:
		fmt.Fprintf(&b, "- **Tamper check:** exit %d (a protected path changed)\n", d.TamperClean)
	}
	switch d.LastVerifyOutcome {
	case "":
		b.WriteString("- **Last verify:** the acceptance checks passed\n")
	default:
		fmt.Fprintf(&b, "- **Last verify:** %s\n", oneLine(d.LastVerifyOutcome))
	}
	b.WriteString(verifyHistoryLines(d.VerifyHistory))
	b.WriteString(budgetLines(d.Budget))
	if d.Budget != nil && d.Budget.Exceeded {
		fmt.Fprintf(&b, "- **Budget gate:** the operator gated delivery (budget %s)\n", d.Budget.ExceededReason)
	}
	return b.String()
}

// budgetLines renders the budget used-vs-limit facts under their own
// "## Budget" heading (the budget lines were previously printed directly
// after the per-iteration verify record and rendered under that section —
// R26 P3). nil budget -> one line. The body itself no longer carries a
// trailing "---": the push script's plan section supplies the single
// separator before the (labelled, fenced) plan, so there is exactly one
// "---" in the final description (R26 P3: the doubled "---").
func budgetLines(bf *deliverBudgetFacts) string {
	if bf == nil {
		return "\n## Budget\n- **Budget:** not yet read\n"
	}
	var s strings.Builder
	s.WriteString("\n## Budget\n")
	fmt.Fprintf(&s, "- **Budget (tokens):** %d", bf.Tokens)
	if bf.MaxTokens != nil {
		fmt.Fprintf(&s, " of %d cap", *bf.MaxTokens)
	} else {
		s.WriteString(" (no cap)")
	}
	s.WriteString("\n")
	fmt.Fprintf(&s, "- **Budget (requests):** %d\n", bf.Requests)
	if bf.CostUsd != "" {
		fmt.Fprintf(&s, "- **Budget (cost):** $%s", bf.CostUsd)
		if bf.MaxCostUsd != "" {
			fmt.Fprintf(&s, " of $%s cap", bf.MaxCostUsd)
		} else {
			s.WriteString(" (no cap)")
		}
		s.WriteString("\n")
	}
	fmt.Fprintf(&s, "- **Budget (active time):** %s", humanDurationSeconds(bf.ActiveSeconds))
	if bf.MaxWallClockSeconds > 0 {
		fmt.Fprintf(&s, " of %s cap", humanDurationSeconds(bf.MaxWallClockSeconds))
	} else {
		s.WriteString(" (no cap)")
	}
	s.WriteString("\n")
	if bf.Exceeded {
		fmt.Fprintf(&s, "- **Budget exceeded:** yes (%s)\n", bf.ExceededReason)
	}
	return s.String()
}

// verifyHistoryLines renders the per-iteration verify record (A4 "the verify
// results per iteration"): one line per failing iteration (from the
// operator's status.stallHistory ring, oldest first) plus the final pass
// (the iteration the delivery names, with the pinned verifiedCommit). It is a
// pure function of the entries (no clock, no I/O), so a spec can call it with
// a fixed fixture and assert the exact section.
//
// A failing line names the iteration, the failing check, and when the verify
// Job finished (the stallHistory entry's kubelet-recorded time — operator-
// side, never a runner claim). The final pass names the iteration and the
// pinned verifiedCommit. An empty history (a first-iteration success with no
// recorded stall) prints a single "iteration N passed" line (the pass is the
// delivery's own pinned commit, so it is always present for a delivered
// Loop).
func verifyHistoryLines(vh []verifyHistoryEntry) string {
	if len(vh) == 0 {
		// No stallHistory and no pinned commit (a fixture that never verified)
		// — the section is empty (the body has no per-iteration record to show).
		return ""
	}
	var s strings.Builder
	s.WriteString("\n## Verify results per iteration\n")
	for _, e := range vh {
		if e.Pass {
			if e.VerifiedCommit == "" {
				fmt.Fprintf(&s, "- iteration %d: passed\n", e.Iteration)
			} else {
				fmt.Fprintf(&s, "- iteration %d: passed (verified commit `%s`)\n", e.Iteration, e.VerifiedCommit)
			}
		} else {
			check := oneLine(e.Check)
			if check == "" {
				check = "(check unnamed)"
			}
			if e.At.IsZero() {
				fmt.Fprintf(&s, "- iteration %d: FAILED (%s)\n", e.Iteration, check)
			} else {
				fmt.Fprintf(&s, "- iteration %d: FAILED (%s) at %s\n", e.Iteration, check, e.At.UTC().Format("2006-01-02 15:04:05Z"))
			}
		}
	}
	return s.String()
}

// humanDurationSeconds renders whole seconds as a compact Go-style duration.
func humanDurationSeconds(total int64) string {
	h := total / 3600
	m := (total % 3600) / 60
	sec := total % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm%ds", h, m, sec)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, sec)
	}
	return fmt.Sprintf("%ds", sec)
}

// oneLine collapses a (possibly multi-line) user string to a single line for
// the body (newlines -> " / "; surrounding whitespace trimmed). The goal and
// lastResultStatus are user/verify text and must not break the bullet
// layout; the raw values stay in the Loop's spec/status — this is display
// shaping only.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", " / ")
	return strings.TrimSpace(s)
}

// refreshDeliverDescriptionBudget accumulates the wall clock into
// status.budget the way applyBudgetStep does (loop_p2d_budget.go: add
// now - lastActiveStamp, floor at 0, set the stamp to now), but ONLY the
// wall-clock half (no proxy read, no cap re-application). I95: the facts
// body is rendered before the deliver Job is created, when the last
// budget accumulation may be minutes old (the applyBudgetStep call in the
// same reconcile runs on the loop object and is not yet visible here when
// the body is rendered — the body is a snapshot; see the comment on
// deliverDescriptionForLoop). It mutates the caller's loop IN PLACE so the
// caller's subsequent status write (finalizeLoopStatus persists the loop
// after the deliver step) carries the refresh — the body and the persisted
// status agree on the value, and a later reconcile does not re-measure the
// same interval (the stamp is already current).
//
// It is a no-op only when the Loop is Paused (applyBudgetStep stops the
// clock in Paused, and a Paused Loop cannot be Succeeded — deliveryExpected
// requires Phase == Succeeded — so this path never sees one; the guard is
// here to mirror applyBudgetStep's shape). It is NOT gated on
// spec.budget.maxWallClock: the wall clock is always accumulated (I95, the
// cap only decides enforcement, not recording).
func refreshDeliverDescriptionBudget(loop *coxv1alpha1.Loop) {
	if loopPaused(loop) {
		return
	}
	if loop.Status.Budget == nil || loop.Status.Budget.LastActiveStamp == nil {
		// Nothing to accumulate: no existing budget or no stamp yet. The
		// deliver description body prints 0s (the zero value) — that is
		// correct for a Loop that has never been reconciled with a stamp.
		return
	}
	now := time.Now()
	loop.Status.Budget.ActiveSeconds += max(0, int64(now.Sub(loop.Status.Budget.LastActiveStamp.Time)/time.Second))
	st := metav1.NewTime(now)
	loop.Status.Budget.LastActiveStamp = &st
}
