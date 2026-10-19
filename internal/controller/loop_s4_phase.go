/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on the "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the code for the distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// S4 (TDD-PLAN A. "Runner as phase driver", operator side; R19 OS1/OS5/OS8):
// the ADR-0004 claim reader + the phase machine advance.
//
// The channel (ADR-0004, precisely): the runner (the reference agent) is
// ONE-SHOT per desired phase. It reads .coxswain/desired-phase (written by
// the operator's phase-init container), does that phase's work, and exits —
// writing its claim to /dev/termination-log (the kubelet-capped 40-char
// termination message), as a strict JSON object:
//
//	{"observedPhase":"Implementing","status":"success","blockedReason":""}
//
// Exit code 0 = the phase completed the operator's ask; exit code 1 = the
// phase ended blocked (the message still carries status:"blocked"). The agent
// container runs with the sandbox pod's restartPolicy (the agent-sandbox CR
// default), so the kubelet leaves the agent container Terminated when it
// exits (no in-place restart of the one-shot runner; a restarted container's
// last claim is carried in LastTerminationState, which the live reader falls
// back to) and the message stays readable until the operator recreates the
// pod (the per-phase recycle).
//
// Why one container run per phase (the handoff question): the termination
// message is ONE-SHOT per container run (it is written at exit, and a
// restart would replace it). A long-lived runner daemon could not signal
// each phase through it. The simplest status-surfaced mechanism is therefore
// ONE CONTAINER RUN PER PHASE: the operator writes the desired phase, the
// runner executes exactly that phase and exits with the claim, and the
// operator (reading the claim via the APIReader path) advances status.phase
// by one step (the existing nextPhase table, unchanged) and recreates the
// sandbox pod with the next phase (a new emptyDir per phase run — the
// model-context continuity across phases is the S3/S4 runner's in-pod
// conversation file, which a phase-boundary pod recycle discards: the
// reference runner re-plans/re-implements from the repo, not from a warm
// conversation, and the plan survives as PLAN.md in the workspace). This is
// the same D38 pattern as the S3 baseCommit read (init container termination
// message via the APIReader), with no exec, no kubelet ReadFile, and no new
// RBAC (pods/status get is covered by the existing pod verbs).
//
// ADR-0005: the claim is NEVER a gate input. It is size-limited (the kubelet
// caps the message; the reader bounds the read anyway), strict-parsed (a
// malformed message is rejected — logged, not acted on), and the ONLY thing
// the operator does with it is the pure nextPhase match (reported phase ==
// the immediate-next phase after status.phase). No gate reads the claim's
// status or blockedReason; those ride into status.progress (OS1) for
// observability only.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// agentContainerNameS4 is the sandbox pod's agent container name (the claim
// reader finds it by this name; the pod builder sets the same name). Kept
// local to this file so the reader has one named reference (the builder and
// the envtest fixture share the literal; goconst allows it to appear in
// three files).
const agentContainerNameS4 = "agent"

// s4ClaimMaxBytes bounds the claim read (ADR-0005 size hygiene). The kubelet
// already caps /dev/termination-log (4096 bytes; the status termination
// message is capped similarly), but the reader bounds its own parse so a
// future kubelet cap change cannot blow the operator's memory or feed a
// pathological string into strict parsing. The cap is generous for the
// strict-JSON claim shape: the observedPhase value is a fixed enum word, the
// status is success/blocked, and the blockedReason is a one-line summary the
// runner itself bounds.
const s4ClaimMaxBytes = 4096

// PhaseClaim is the operator's parsed view of the runner's ADR-0004 claim
// (the agent container's termination message, strict JSON).
type PhaseClaim struct {
	// ObservedPhase is the phase the runner reports having executed (the
	// input to the nextPhase match, B1). Validated against the operator's
	// LoopPhase enum on the advance path (a claim naming an unknown phase is
	// a no-op, not an error).
	ObservedPhase coxv1alpha1.LoopPhase `json:"observedPhase"`
	// Status is the runner's result status for the phase (success/blocked).
	// OS1 only — never a gate input.
	Status string `json:"status"`
	// BlockedReason is the runner's one-line reason when Status is blocked.
	// OS1 only — never a gate input.
	BlockedReason string `json:"blockedReason,omitempty"`
	// Iteration is the .coxswain/iteration the runner read (the operator's
	// iteration marker file). OS1 only.
	Iteration int `json:"iteration,omitempty"`
	// S5a (B3): the runner's committed-head SHA (40-hex) for a completed
	// Implementing run. The operator strictly validates the shape on the
	// advance path and pins it to status.currentVerify.verifiedCommit on the
	// Implementing -> Verifying advance (the verify Job checks out exactly
	// this SHA, D11). A missing/malformed headCommit blocks the advance
	// (ADR-0005: no evidence, no advance).
	HeadCommit string `json:"headCommit,omitempty"`
	// B3 (PLAN-BETA / PLAN.md Phase 4): true when the runner's Planning run
	// detected a PLAN CHANGE (the fresh PLAN.md's SHA-256 does not match the
	// approved hash the operator wrote to .coxswain/plan-hash). It is a CLAIM
	// (ADR-0005): the operator uses it as a SIGNAL to clear the PlanApproved
	// condition (re-enter the hold), not as an authoritative gate input (the
	// operator's own read of the condition + phase is the gate input). An
	// absent field (pre-B3 runners) is false (no plan change detected).
	PlanChanged bool `json:"planChanged,omitempty"`
	// I93 (R28 P1): planMismatch, when present, must be a boolean (the
	// runner's plan-hash-mismatch signal at Implementing start). An absent
	// field (pre-I93 runners) is false (no mismatch detected).
	PlanMismatch bool `json:"planMismatch,omitempty"`
	// planAttempt is the .coxswain/plan-attempt the runner read (the operator's
	// plan-attempt marker file). OS1 only — never a gate input. The guard in
	// advancePhaseFromClaim uses it to reject a stale Planning success claim
	// from a prior attempt (I42b). An absent field (pre-I42b runners) is 0,
	// which matches a fresh Loop's status.planAttempt (also 0) — the guard is
	// a no-op for runners that do not echo the marker.
	PlanAttempt int `json:"planAttempt,omitempty"`
}

// parsePhaseClaim strict-parses a termination message into a PhaseClaim
// (ADR-0005). It rejects: a nil/oversized message, a non-JSON message, a
// JSON non-object, and a claim with no observedPhase. A malformed claim is
// an error the caller LOGS (it is never acted on and never gates — ADR-0005);
// the caller requeues so a transient/truncated write is retried.
func parsePhaseClaim(msg string) (*PhaseClaim, error) {
	s := strings.TrimSpace(msg)
	if s == "" {
		return nil, fmt.Errorf("claim is empty")
	}
	if len(s) > s4ClaimMaxBytes {
		return nil, fmt.Errorf("claim exceeds %d bytes (got %d)", s4ClaimMaxBytes, len(s))
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("claim is not a strict JSON object: %w", err)
	}
	var claim PhaseClaim
	var op string
	if v, ok := raw["observedPhase"]; ok {
		if err := json.Unmarshal(v, &op); err != nil {
			return nil, fmt.Errorf("claim.observedPhase is not a string: %w", err)
		}
	}
	if v, ok := raw["status"]; ok {
		if err := json.Unmarshal(v, &claim.Status); err != nil {
			return nil, fmt.Errorf("claim.status is not a string: %w", err)
		}
	}
	if v, ok := raw["blockedReason"]; ok {
		if err := json.Unmarshal(v, &claim.BlockedReason); err != nil {
			return nil, fmt.Errorf("claim.blockedReason is not a string: %w", err)
		}
	}
	if v, ok := raw["iteration"]; ok {
		var it int
		if err := json.Unmarshal(v, &it); err != nil {
			return nil, fmt.Errorf("claim.iteration is not an integer: %w", err)
		}
		claim.Iteration = it
	}
	// S5a (B3): headCommit, when present, must be a strict 40-hex commit SHA
	// (the operator pins it to status.currentVerify.verifiedCommit on the
	// Implementing -> Verifying advance; a malformed value is a malformed
	// claim, not a pin). An absent headCommit is valid (pre-S5a runners and
	// non-Implementing phases carry none).
	if v, ok := raw["headCommit"]; ok {
		if err := json.Unmarshal(v, &claim.HeadCommit); err != nil {
			return nil, fmt.Errorf("claim.headCommit is not a string: %w", err)
		}
		if claim.HeadCommit != "" && !isCommitSHA(claim.HeadCommit) {
			return nil, fmt.Errorf("claim.headCommit %q is not a 40-hex commit SHA", claim.HeadCommit)
		}
	}
	// B3 (PLAN-BETA / PLAN.md Phase 4): planChanged, when present, must be a
	// boolean (the runner's plan-change signal). An absent field (pre-B3
	// runners) is false (no plan change detected).
	if v, ok := raw["planChanged"]; ok {
		if err := json.Unmarshal(v, &claim.PlanChanged); err != nil {
			return nil, fmt.Errorf("claim.planChanged is not a boolean: %w", err)
		}
	}
	// I93 (R28 P1): planMismatch, when present, must be a boolean (the
	// runner's plan-hash-mismatch signal at Implementing start). An absent
	// field (pre-I93 runners) is false (no mismatch detected).
	if v, ok := raw["planMismatch"]; ok {
		if err := json.Unmarshal(v, &claim.PlanMismatch); err != nil {
			return nil, fmt.Errorf("claim.planMismatch is not a boolean: %w", err)
		}
	}
	// I42b: planAttempt, when present, must be a non-negative integer (the
	// runner's echo of the .coxswain/plan-attempt marker). An absent field
	// (pre-I42b runners) is 0, which matches a fresh Loop's status.planAttempt.
	if v, ok := raw["planAttempt"]; ok {
		var pa int
		if err := json.Unmarshal(v, &pa); err != nil {
			return nil, fmt.Errorf("claim.planAttempt is not an integer: %w", err)
		}
		if pa < 0 {
			return nil, fmt.Errorf("claim.planAttempt is negative: %d", pa)
		}
		claim.PlanAttempt = pa
	}
	claim.ObservedPhase = coxv1alpha1.LoopPhase(op)
	// Review P3: the claimed phase must be one the runner can claim
	// (Planning / Implementing / Verifying). nextPhase maps (phase, claim) ->
	// next for every in-set pair, so an out-of-set claim would otherwise fall
	// through the default case and be silently ignored (a typo'd phase would
	// never surface as an error). Reject it at the parse boundary instead.
	switch claim.ObservedPhase {
	case coxv1alpha1.LoopPhasePlanning, coxv1alpha1.LoopPhaseImplementing, coxv1alpha1.LoopPhaseVerifying:
	default:
		return nil, fmt.Errorf("claim names an invalid observedPhase %q (must be Planning, Implementing, or Verifying)", op)
	}
	return &claim, nil
}

// readPhaseClaim reads the runner's ADR-0004 claim from the sandbox pod's
// AGENT container termination message, via the operator's APIReader path (a
// real, non-cached client — the sandbox pod is NOT in the manager's Pod
// cache, so a cached Get would return NotFound and the claim would be
// silently skipped; the same D38 pattern as the S3 baseCommit read).
//
// It returns:
//   - (*PhaseClaim, nil) when the agent container has terminated with a
//     valid claim message (the caller advances / records progress).
//   - (nil, nil) when the agent has not terminated yet (still running, no
//     status, no pod): the caller requeues. This is the normal path while a
//     phase is executing.
//   - (nil, err) when the agent terminated with a MALFORMED claim (a
//     one-shot runner that crashes without a valid termination message):
//     the caller logs and requeues (bounded: the container stays Terminated,
//     so the retry is cheap and the message will not change until the pod is
//     recreated). A malformed claim is NEVER treated as a phase completion
//     (ADR-0005 fail-closed).
//
// No exec, no kubelet ReadFile, no new RBAC (pods/status get is covered by
// the existing pod verbs).
func (r *LoopReconciler) resolvePhaseClaim(ctx context.Context, loop *coxv1alpha1.Loop) (*PhaseClaim, error) {
	if r.readPhaseClaim != nil {
		return r.readPhaseClaim(ctx, loop)
	}
	return r.readPhaseClaimFromTerminationMessage(ctx, loop)
}

// readPhaseClaimFromTerminationMessage is the default (live) claim read: it
// gets the sandbox pod via the APIReader and reads the agent container's
// terminated status. See readPhaseClaim for the contract.
func (r *LoopReconciler) readPhaseClaimFromTerminationMessage(ctx context.Context, loop *coxv1alpha1.Loop) (*PhaseClaim, error) {
	reader := r.apiReader
	if reader == nil {
		// No APIReader wired (a bare test reconciler without
		// SetupWithManager): fall back to the cached client. In a live
		// deployment SetupWithManager always wires the APIReader (the
		// mutation-check spec asserts the cache path is not silently used
		// when the APIReader is available).
		reader = r
	}
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: sandboxName(loop.Name)}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil // the pod does not exist yet (or was deleted): no claim
		}
		return nil, fmt.Errorf("read sandbox pod for phase claim: %w", err)
	}
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name != agentContainerNameS4 {
			continue
		}
		// The claim lives in the agent's termination message. The live
		// state is the current one (the kubelet restarts the agent at most
		// N times under the RestartPolicy the agent-sandbox CR defaults to;
		// the reviewer's P2: fall back to the LAST termination state when the
		// current state is not Terminated — a container that was replaced by a
		// restart carries its last claim in LastTerminationState, and the
		// message is the stable read-back the S3 baseCommit pattern relies on).
		terminated := cs.State.Terminated
		if terminated == nil {
			terminated = cs.LastTerminationState.Terminated
		}
		if terminated == nil {
			// The agent is still running the phase (or has no status yet):
			// no claim yet. Requeue (the phase-init container has written the
			// desired phase and the runner is doing the work).
			return nil, nil
		}
		claim, err := parsePhaseClaim(terminated.Message)
		if err != nil {
			// The agent terminated but wrote a malformed/empty claim (it
			// crashed, or the termination log was truncated/overwritten).
			// ADR-0005 fail-closed: this is NOT a phase completion. Return the
			// error so the caller logs + requeues; the container stays
			// Terminated so the retry is cheap and the message is stable.
			return nil, fmt.Errorf("agent container terminated with a malformed claim: %w", err)
		}
		return claim, nil
	}
	// No agent container status yet (pod created, agent not registered).
	return nil, nil
}

// PhaseGate is the OS8 phase-gate seam: it decides whether the phase machine
// may advance from current to next. The S4 build ships exactly one
// implementation (autoApprovePhaseGate, option B — no approval gate); a
// future approval hold or observer-proposed hold plugs in here without
// rewriting nextPhase. A nil gate on the reconciler is treated as auto-
// approve (the advance proceeds), so a bare test reconciler without a wired
// gate keeps the existing B1 advance behaviour.
type PhaseGate interface {
	// Allow reports whether the transition current -> next may proceed.
	// allow=false holds the Loop in current (no Event, no advance); the
	// operator requeues and re-asks once the hold clears.
	Allow(loop *coxv1alpha1.Loop, current, next coxv1alpha1.LoopPhase) (allow bool, reason string)
}

// autoApprovePhaseGate is the S4 phase gate (option B: NO approval gate).
// The owner's bar is Planning -> Implementing -> Verifying with no Awaiting
// Approval hold, so every advance is allowed. It exists so (a) the gate
// seam is exercised and (b) a later hold (e.g. the Phase-4 approval gate,
// or an observer-proposed hold) replaces it without touching the advance
// code.
type autoApprovePhaseGate struct{}

// Allow always allows (option B). The reason is empty (an allow has no
// reason to record).
func (autoApprovePhaseGate) Allow(_ *coxv1alpha1.Loop, _, _ coxv1alpha1.LoopPhase) (bool, string) {
	return true, ""
}

// phaseAdvancedReason is the stable Event reason the operator emits on a
// FORWARD phase transition (OS5, R19: "stable reasons, e.g. PhaseAdvanced").
// The reason is a fixed string (documented as an API); the message carries
// the from/to phases. The verify ITERATE (Verifying -> Implementing) is
// distinct: it emits verifyIteratedReason (PhaseIterated, loop_verify_job.go)
// — an iterate is not a forward advance.
const phaseAdvancedReason = "PhaseAdvanced"

// claimPhaseForAdvance maps the runner's claim (ADR-0004) to the phase the
// B1 nextPhase match compares against. The runner reports the phase it
// EXECUTED (the current phase), never the next phase: a success claim for the
// current phase COMPLETES that phase, so the match runs against its
// successor (Planning -> Implementing, Implementing -> Verifying). A blocked
// claim (the phase did not complete) has NO successor mapping — it is the
// current phase, and nextPhase(current, current) == current (no advance).
// A success claim naming a phase that is not the current phase is returned
// unchanged (the pure B1 match: it names the immediate-next phase directly,
// or a no-op value).
func claimPhaseForAdvance(c *PhaseClaim) coxv1alpha1.LoopPhase {
	if c == nil || c.Status != claimSuccess {
		return c.ObservedPhase
	}
	switch c.ObservedPhase {
	case coxv1alpha1.LoopPhasePlanning:
		return coxv1alpha1.LoopPhaseImplementing
	case coxv1alpha1.LoopPhaseImplementing:
		return coxv1alpha1.LoopPhaseVerifying
	}
	return c.ObservedPhase
}

// claimSuccess is the runner's result status for a completed phase (the
// ADR-0004 claim's "status" field; the runner's statusSuccess constant —
// the runner cannot import the operator's types, so the string value is the
// contract, mirrored here).
const claimSuccess = "success"

// recordPhaseClaim runs the S4 advance: it advances status.phase/
// status.desiredPhase one step when the claim names the immediate-next phase
// (the existing nextPhase table, unchanged, gated by the OS8 PhaseGate), and
// it records the claim into status.progress (OS1). It returns changed
// (whether any status field was written) and advanced (whether the phase
// moved — the caller emits the OS5 Event when advanced).
//
// The advance is the ONLY thing the operator does with the claim's phase
// (B1, ADR-0004). A claim that does not name the immediate-next phase is a
// no-op (the runner reported a stale/skipped/unknown phase; the operator
// does not interpret runner output beyond this pure match). A blocked claim
// (status=blocked) does NOT advance: the pure match is on observedPhase
// only, and a runner that ends a phase blocked has not completed the
// operator's ask for that phase — it has reported the phase it was in, not
// the next one. (A blocked claim's observedPhase equals the CURRENT phase
// the runner was executing, never the next, so the nextPhase match
// naturally rejects it: nextPhase(current, current) == current.)
func (r *LoopReconciler) recordPhaseClaim(ctx context.Context, loop *coxv1alpha1.Loop, claim *PhaseClaim, now metav1.Time) (changed, advanced bool) {
	// B5a: bump the iteration metric on a NEW-phase transition (a re-claim of
	// the same phase does not bump). The claim's observedPhase is the phase
	// the runner reports having executed (the destination of this advance).
	if claim != nil && claim.ObservedPhase != "" && claim.ObservedPhase != loop.Status.Phase {
		loopPhaseTransitionsTotal.WithLabelValues(string(claim.ObservedPhase)).Inc()
	}
	// OS1: record the claim into status.progress. The pins (observed
	// generation / base commit) are the OPERATOR's, never the claim's (a
	// claim cannot set a pin for itself).
	// P2 (dogfood) + P1 (reviewer fix): include planAttempt in the progress
	// record so that a re-plan claim (same phase, iteration, status but a new
	// planAttempt) triggers a progress write, which refreshes lastActivityTime
	// on the claim-driven advance. This is the fix for the F4 gap: the
	// re-plan's advance did not refresh lastActivityTime because
	// progressEqual suppressed the write when the claim-derived fields were
	// the same (a re-plan claim has the same phase, iteration, and status as
	// the prior plan's claim, but a new planAttempt).
	p := &coxv1alpha1.ProgressStatus{
		Phase:            claim.ObservedPhase,
		LastActivityTime: &now,
		Iteration:        claim.Iteration,
		LastResultStatus: claim.Status,
		BlockedReason:    claim.BlockedReason,
		PlanAttempt:      claim.PlanAttempt,
	}
	if loop.Status.Progress == nil || !progressEqual(loop.Status.Progress, p) {
		// Copy the operator's pins onto the new progress record.
		p.ObservedGeneration = loop.Generation
		p.BaseCommit = loop.Status.BaseCommit
		loop.Status.Progress = p
		changed = true
	}
	// B1 + OS8: advance one step when the claim names the immediate-next
	// phase AND the gate allows it (option B: the gate always allows).
	// S5a hot-loop guard: the advance requires the claim's ObservedPhase to
	// match the CURRENT phase (loop.Status.Phase). A stale claim from a
	// previous phase (e.g., an Implementing claim left on the pod after a
	// verify-iterate back to Implementing) must NOT re-advance: the runner
	// has not re-run that phase, so the claim is evidence for a phase that
	// is no longer current. Without this guard, the one-shot claim is
	// re-consumed forever (the kind hot-loop: verify-2..verify-9 flooding).
	claimPhaseIsCurrent := claim.ObservedPhase == loop.Status.Phase
	next := nextPhase(loop.Status.Phase, claimPhaseForAdvance(claim))
	if next != loop.Status.Phase && claimPhaseIsCurrent {
		// S5a (B3, ADR-0005): the Implementing -> Verifying advance pins the
		// runner's committed head to status.currentVerify.verifiedCommit. The
		// pin requires a VALID headCommit in the claim (40-hex, strict-parsed
		// by parsePhaseClaim); a success claim without one is NOT a valid
		// Implementing completion (no evidence to verify) — the advance is
		// held (the claim is still recorded into progress, OS1) and the Loop
		// stays in Implementing. A blocked claim never advances (nextPhase
		// returns the current phase for a non-success status).
		if next == coxv1alpha1.LoopPhaseVerifying && claim.HeadCommit == "" {
			if loop.Status.ObservedPhase != claim.ObservedPhase {
				loop.Status.ObservedPhase = claim.ObservedPhase
				changed = true
			}
			return changed, false
		}
		gate := r.phaseGate
		if gate == nil {
			gate = autoApprovePhaseGate{}
		}
		allow, _ := gate.Allow(loop, loop.Status.Phase, next)
		if allow {
			loop.Status.Phase = next
			loop.Status.DesiredPhase = next
			if next == coxv1alpha1.LoopPhaseVerifying {
				// Pin the CURRENT iteration's verified commit (D11): the
				// verify Job (ensureVerifyJob) checks out exactly this SHA and
				// the tamper check diffs baseCommit..this SHA. The pin is the
				// operator's (written from the claim, never by the runner);
				// a verify Job naming a different commit is stale evidence
				// (D24 fail-closed).
				loop.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{
					VerifiedCommit: claim.HeadCommit,
				}
				// I65 P1-B: a new pin resets the per-verified-commit verify
				// status fields (the attempt count and the counted-Job-UID
				// guard). infraAttempts is a per-verified-commit attempt count
				// (exactly like tamperExitCode and lastCheckResults): a new
				// pin is a new verifiedCommit, and the old pin's infra flakes
				// must not accumulate into the new pin's bound (three transient
				// flakes spread over a long Loop, in different iterations, must
				// not fail it as VerifyInfraFailed). The reset is the operator's
				// (the pin advance is the operator's decision, not the agent's),
				// and it clears both the counter and the UID guard (a new pin
				// means a new Job, so the old UID is stale).
				if loop.Status.Verify != nil {
					loop.Status.Verify.InfraAttempts = 0
					loop.Status.Verify.InfraJobUID = ""
				}
			}
			advanced = true
			changed = true
		} else if next == coxv1alpha1.LoopPhaseImplementing && loop.Status.Phase == coxv1alpha1.LoopPhasePlanning && approvalMode(loop) == coxv1alpha1.ApprovalModeManual {
			// B3 (R1 P1, fourth round): the gate DENIED the Planning ->
			// Implementing advance (no approval — a Manual-mode Loop). The
			// runner COMPLETED the Planning phase (a success claim with a valid
			// headCommit), but the Loop must NOT advance to Implementing without
			// the user's approval. MOVE the phase to AwaitingApproval (the hold
			// re-enters; the user approves/rejects the plan, and the gate's
			// approved branch then advances to Implementing). The operator is the
			// sole writer of status (ADR-0004). This is the missing transition
			// that kept Manual Loops rerunning Planning forever (the gate denied
			// the advance, but nothing moved the Loop to the hold). The check is
			// on the Manual mode (the approval gate is active), so a generic
			// denying gate (e.g., the S4 mutation test's denyingPhaseGate) does NOT
			// trigger the move (it holds the advance at Planning, not AwaitingApproval).
			loop.Status.Phase = coxv1alpha1.LoopPhaseAwaitingApproval
			loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseAwaitingApproval
			advanced = true
			changed = true
		}
	}
	// I42b: Planning -> AwaitingApproval for Manual mode. When the runner
	// completes a Planning run (success claim, observedPhase=Planning, current
	// phase=Planning) in Manual mode, the Loop moves to AwaitingApproval (the
	// hold). This transition lives here (not in applyApprovalGate) so it only
	// happens when a FRESH claim is read (the agent actually ran). The gate's
	// Planning -> AwaitingApproval branch (applyApprovalGate) handles the case
	// where the Loop is ALREADY at AwaitingApproval (the hold) and the user
	// approves/rejects.
	if loop.Status.Phase == coxv1alpha1.LoopPhasePlanning &&
		claim.ObservedPhase == coxv1alpha1.LoopPhasePlanning &&
		claim.Status == claimSuccess &&
		approvalMode(loop) == coxv1alpha1.ApprovalModeManual {
		logf.FromContext(ctx).Info("advancing Planning -> AwaitingApproval on fresh Planning claim",
			"loop", loop.Name,
			"claimStatus", claim.Status,
			"observedPhase", claim.ObservedPhase,
			"planAttempt", claim.PlanAttempt,
			"currentPlanAttempt", loop.Status.PlanAttempt)
		loop.Status.Phase = coxv1alpha1.LoopPhaseAwaitingApproval
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseAwaitingApproval
		// I42b: refresh lastActivityTime on the advance (the agent just ran;
		// the claim is fresh). This is the evidence that the re-plan completed.
		// The no-churn spec (R17 P1) is preserved because this branch only
		// fires when the phase advances (a one-shot transition), not on every
		// reconcile where the same claim is re-read.
		if loop.Status.Progress == nil {
			loop.Status.Progress = &coxv1alpha1.ProgressStatus{}
		}
		loop.Status.Progress.LastActivityTime = &now
		if loop.Status.Progress.ObservedGeneration == 0 {
			loop.Status.Progress.ObservedGeneration = loop.Generation
		}
		if loop.Status.Progress.BaseCommit == "" {
			loop.Status.Progress.BaseCommit = loop.Status.BaseCommit
		}
		advanced = true
		changed = true
	}
	// Keep status.observedPhase in sync with the claim (the B1 field; the
	// progress block is the OS1 structured record).
	if loop.Status.ObservedPhase != claim.ObservedPhase {
		loop.Status.ObservedPhase = claim.ObservedPhase
		changed = true
	}
	return changed, advanced
}

// progressEqual reports whether two progress records carry the same
// CLAIM-DERIVED content (the advance path only writes progress when those
// fields changed, to avoid a status churn on every reconcile while a claim is
// stable). It compares ONLY the claim-derived fields (Phase,
// LastResultStatus, BlockedReason, Iteration). The operator-stamped fields
// (LastActivityTime, ObservedGeneration, BaseCommit) are EXCLUDED on purpose:
// the candidate record always gets a fresh LastActivityTime and does not yet
// carry the operator's pins, so comparing them would make the function report
// a change on every reconcile (the R17 P1 churn: ~20 status writes/min during
// a running phase on kind). The caller stamps those fields only when the
// claim-derived fields actually change.
func progressEqual(a, b *coxv1alpha1.ProgressStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Phase == b.Phase && a.LastResultStatus == b.LastResultStatus &&
		a.BlockedReason == b.BlockedReason && a.Iteration == b.Iteration &&
		a.PlanAttempt == b.PlanAttempt
}

// advancePhaseFromClaim (S4) reads the runner's ADR-0004 claim from the
// sandbox pod's termination message and advances the phase machine. The claim
// reader is the ONLY claim source (the legacy B1 seam that fell back to
// status.observedPhase was deleted in R17 — it clobbered progress while a
// phase was running). Returns (claimReadPending, changed, advancedFrom):
// claimReadPending is true when the operator must requeue (the claim reader
// found nothing or a malformed claim); changed is true when the Loop's status
// was mutated (progress record or phase advance); advancedFrom is the phase
// the machine advanced FROM ("" when it did not advance — the caller emits
// the PhaseAdvanced Event only after the status update persists the advance,
// I83: a Conflict on the status update must not double-emit the event).
func (r *LoopReconciler) advancePhaseFromClaim(ctx context.Context, loop *coxv1alpha1.Loop) (bool, bool, coxv1alpha1.LoopPhase) {
	claimReadPending := false
	changed := false
	var advancedFrom coxv1alpha1.LoopPhase
	// P2f: the claim reader is INERT while paused (the agent container is
	// stopped; a claim that does arrive while paused — e.g. from a pod mid-run
	// at pause time — is ignored: the nextPhase gate is phase != Paused).
	if loop.Status.Phase == coxv1alpha1.LoopPhasePaused {
		return false, false, ""
	}
	if loop.Status.Phase == coxv1alpha1.LoopPhaseSucceeded || loop.Status.Phase == coxv1alpha1.LoopPhaseFailed {
		return false, false, ""
	}
	// F6 (review follow-up): the AgentFailed condition is cleared to
	// False/Recovered on ANY valid fresh claim — BEFORE the other early
	// returns (the Verifying hold, the exhausted-failure terminal phase,
	// the F6 failure path). A fresh claim is evidence the agent recovered
	// from a prior sandbox failure (the pod that failed without a claim is
	// gone, its replacement is doing the work), so a healthy Loop must not
	// still show AgentFailed=True (the API doc promise on AgentFailedCondition).
	// A no-op when the condition is absent (the common path: no failure ever
	// happened); it does NOT change the phase machine (ADR-0005: the claim
	// channel is the only thing that advances).
	if changedByClear := clearRecoveredCondition(loop, sandboxFailedCondition); changedByClear {
		changed = true
	}
	// F6: a sandbox pod that FAILED without a phase claim (evicted, OOMKilled,
	// or the agent container terminated without a claim — the dogfood F6:
	// the EmptyDir /tmp sizeLimit eviction left the Loop silently stuck in
	// Planning) is a sandbox INFRASTRUCTURE failure, not the agent's work
	// (ADR-0005: no claim means no advance). It must be SURFACED (I90: a
	// condition + a Warning Event) and retried in a bounded, visible way, or
	// the Loop fails with the distinct SandboxFailed reason once the bound is
	// exhausted — the I65 VerifyInfraFailed pattern mirrored onto the sandbox.
	if f, serr := r.sandboxFailure(ctx, loop); serr != nil {
		// A transient read error (the sandbox pod read failed): no decision
		// (I49: a failed read is never a failure decision). Requeue.
		logf.FromContext(ctx).Error(serr, "sandbox pod read for the F6 failure check failed; no decision", "loop", loop.Name)
		claimReadPending = true
	} else if f != nil {
		changed = r.applySandboxFailure(ctx, loop, f)
		return false, changed, ""
	}
	if loop.Status.Phase == coxv1alpha1.LoopPhaseVerifying {
		// Verifying is evidence-gated (B3, S5): the runner does NOT execute
		// Verifying (ADR-0005 — verify evidence comes from the operator's
		// isolated Job, never the runner), so there is no runner claim to read
		// at this phase. Hold here until the verify Job's evidence drives the
		// transition (Verifying -> Succeeded | Implementing | Failed). Without
		// this hold the reader would see the pod's crash-loop churn: the runner
		// exits 1 with a blocked claim on every restart because desired-phase
		// is Verifying, and the OS1 progress record would flap on every 5s
		// requeue (a churn the kind run observed live).
		return false, false, ""
	}
	claim, cerr := r.resolvePhaseClaim(ctx, loop)
	if cerr != nil {
		// A malformed claim (the agent terminated without a valid
		// termination message): log and requeue. ADR-0005 fail-closed — a
		// malformed claim is NOT a phase completion and never gates.
		logf.FromContext(ctx).Error(cerr, "phase claim read failed; requeueing",
			"loop", loop.Name)
		claimReadPending = true
	} else if claim != nil {
		// S5a stale-iteration guard (ADR-0005 D11): a claim from an EARLIER
		// iteration is ignored — no advance, no progress write. The claim
		// carries the .coxswain/iteration the operator's phase-init wrote when
		// the pod was created (the runner reads it and echoes it into the
		// claim); loop.Status.Iteration is the operator's authoritative count.
		// After a verify iterate (a failed check -> Implementing,
		// iteration+1) the OLD pod — still holding the prior iteration's
		// success claim — is the one being reconciled until the desired-phase
		// recycle replaces it. Without this guard, the stale claim (observed
		// phase matches the current phase) would re-advance with the SAME old
		// headCommit, pinning a stale verify and hot-looping. A claim whose
		// iteration equals the current status iteration is CURRENT. An EMPTY
		// marker (claim.Iteration == 0, which phase-init never writes) is
		// current only while the Loop itself is on its first cycle
		// (status.iteration 0): the runner reads the missing marker as 0 and
		// echoes it into first-cycle claims. I48: once the first verify
		// iterate has bumped status.iteration (0 -> 2), an iteration-0 claim
		// left on the old pod is STALE and must be ignored like any other
		// prior-iteration claim. (The alternative fix — starting the phase-
		// init marker at 1 — was rejected: it would change the written
		// .coxswain/iteration value, the runner's read-back, and the
		// verifyJobName <=0 fallback for no behavioural gain.)
		// I63 (I48): the second clause is redundant — claim.Iteration != 0 ||
		// loop.Status.Iteration != 0 is true whenever claim.Iteration !=
		// loop.Status.Iteration (if they differ, at least one is non-zero). Dropped.
		if claim.Iteration != loop.Status.Iteration {
			logf.FromContext(ctx).Info("stale claim from a previous iteration ignored",
				"claimIteration", claim.Iteration,
				"statusIteration", loop.Status.Iteration)
			return false, false, ""
		}
		// I42b: a claim from a PREVIOUS plan attempt is ignored — no advance,
		// no progress write. The planAttempt counter is incremented on every
		// re-entry to Planning (a rejection re-plan or an onVerifyFail=Plan
		// re-plan); the phase-init writes it to .coxswain/plan-attempt and the
		// runner echoes it into its claim. A stale Planning success claim from
		// the prior attempt (same iteration, different planAttempt) would
		// otherwise pass the S5a consumed-guard (observedPhase matches the
		// current phase) and re-advance the new Planning phase at once (the
		// F4 dogfood finding: the reject -> Planning re-plan completed in 15s
		// with no new inference). An absent field (pre-I42b runners) is 0,
		// which matches a fresh Loop's status.planAttempt (also 0) — the guard
		// is a no-op for runners that do not echo the marker.
		// The guard applies to PLANNING claims only: the re-plan re-enters
		// Planning, so a stale Planning claim is the one that can re-advance
		// the new phase at once. An Implementing/Verifying claim's staleness
		// is the iteration guard's job (the planAttempt only changes on a
		// re-plan, which always re-enters Planning).
		// I42b kind finding: the planAttempt counter can be 0 even after a
		// rejection (an older operator that did not increment the field
		// processed the rejection; the field is new and defaults to 0). In
		// that case the planAttempt guard above is a no-op (0 == 0), and a
		// stale Planning claim with planAttempt 0 passes. The belt-and-
		// suspenders guard: a Planning claim with planAttempt 0 is STALE when
		// lastRejectionReason is non-empty (a rejection has occurred, so a
		// fresh claim with planAttempt > 0 is expected). This catches the
		// mixed-version case where the operator that processed the rejection
		// did not increment planAttempt.
		if claim.ObservedPhase == coxv1alpha1.LoopPhasePlanning && claim.PlanAttempt != loop.Status.PlanAttempt {
			logf.FromContext(ctx).Info("stale claim from a previous plan attempt ignored",
				"claimPlanAttempt", claim.PlanAttempt,
				"statusPlanAttempt", loop.Status.PlanAttempt)
			return false, false, ""
		}
		if claim.ObservedPhase == coxv1alpha1.LoopPhasePlanning && claim.PlanAttempt == 0 && loop.Status.Approval != nil && loop.Status.Approval.LastRejectionReason != "" {
			logf.FromContext(ctx).Info("stale claim from a previous plan attempt ignored (rejection reason present, planAttempt 0)",
				"claimPlanAttempt", claim.PlanAttempt)
			return false, false, ""
		}
		// S5a (OS5 P3): a CONSUMED claim is discarded, not re-consumed.
		// advancePhaseFromClaim runs every reconcile while the phase is not
		// terminal, and the operator never acknowledges a claim — so without
		// this guard a success claim is re-read on EVERY reconcile until the
		// pod is recycled. Two wrong behaviors follow: recordPhaseClaim
		// re-stamps progress with the same-second claim data (a 5s status
		// churn while a phase runs), and the advance path would RE-emit the
		// PhaseAdvanced Event with a WRONG from-phase (the kind event stream
		// showed a spurious 'Planning -> Implementing' at the verify iterate:
		// the prior cycle's Implementing success claim, still on the pod, was
		// re-consumed against the phase the Loop was in when that reconcile
		// ran, never the phase the claim had actually advanced). The guard:
		// a SUCCESS claim whose observedPhase no longer matches the CURRENT
		// phase has already been consumed (its advance happened in an earlier
		// reconcile and moved the phase away; the pod has not been recycled
		// yet) — log and discard: no progress write, no advance, no event.
		// A claim whose observedPhase matches the current phase is the one
		// this reconcile consumes; its advance (if any) moves the phase AWAY
		// from the claimed phase, so the same pod's claim is discarded on the
		// very next reconcile. A BLOCKED claim still rides into progress (OS1
		// observability of the runner's last word is unchanged) — a blocked
		// claim never advances, so re-stamping it is idempotent and it is the
		// runner's status, not a stale advance.
		if claim.Status == claimSuccess && claim.ObservedPhase != loop.Status.Phase {
			logf.FromContext(ctx).Info("claim already consumed (observedPhase no longer matches the current phase); discarding",
				"observedPhase", claim.ObservedPhase,
				"currentPhase", loop.Status.Phase)
			return false, false, ""
		}
		// S4 review P2 (R18): the clock is injected (r.now, defaulting to
		// metav1.Now) so a test can advance it more than 1s between reconciles
		// (metav1.Time marshals at 1-second precision: two same-second
		// reconciles are byte-identical and the no-churn spec cannot fail).
		var now metav1.Time
		if r.now != nil {
			now = r.now()
		} else {
			now = metav1.Now()
		}
		fromPhase := loop.Status.Phase
		// I93 (R29): the runner's plan-hash-mismatch claim. The check now
		// runs at the START of the Implementing run, BEFORE any agent work
		// (R29 P1): a mismatch blocks the run (no agent call, no commit) and
		// the claim carries status=blocked + PlanMismatch=true + a
		// blockedReason naming both hashes. The operator re-enters the
		// AwaitingApproval hold (clear the PlanApproved condition), SURFACES
		// the mismatch as the PlanMismatch condition (I90: visible via
		// `kubectl cox describe`; the hashes ride in the message) and emits
		// the PlanHashMismatch Warning Event (every state change has an Event).
		// The handler keys on the claim alone (not the phase): a blocked claim
		// never advances, so the phase is Implementing whenever the operator
		// first sees it, and the guarded phase move below is idempotent for a
		// re-queued claim (a re-hold to AwaitingApproval has already happened).
		// The signal is a CLAIM (ADR-0005): it is the runner's evidence that
		// the plan on disk differs from the approved hash; the re-hold itself
		// is the approval gate's input (the cleared PlanApproved condition),
		// so the PlanMismatch condition is a SURFACE, never a gate input.
		// This runs BEFORE recordPhaseClaim (which would advance the phase to
		// Verifying on a success claim); the re-hold must happen before the
		// advance, otherwise the Loop would advance past the hold.
		if claim.PlanMismatch && approvedPlanHash(loop) != "" {
			approved := approvedPlanHash(loop)
			clearCondition(loop, coxv1alpha1.PlanApprovedCondition)
			changed = true
			if loop.Status.Phase == coxv1alpha1.LoopPhaseImplementing {
				loop.Status.Phase = coxv1alpha1.LoopPhaseAwaitingApproval
				loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseAwaitingApproval
			}
			// I93 (R29 P2): surface the mismatch as a condition + Event. The
			// blockedReason (from the runner) names the current plan's hash and
			// the approved hash; the Event is emitted only on the transition
			// into True (the condition's setCondition is a no-op when
			// unchanged, but a re-queued claim would otherwise re-emit the
			// Event on every reconcile while the condition is already True).
			msg := fmt.Sprintf("plan-hash mismatch at Implementing start: approved hash %s; %s", approved, claim.BlockedReason)
			if !conditionTruePresent(loop, coxv1alpha1.PlanMismatchCondition) {
				if r.Recorder != nil {
					r.Recorder.Eventf(loop, corev1.EventTypeWarning, "PlanHashMismatch", "%s", msg)
				}
			}
			setCondition(loop, coxv1alpha1.PlanMismatchCondition, metav1.ConditionTrue, "PlanHashMismatch", msg)
			logf.FromContext(ctx).Info("runner blocked Implementing on a plan-hash mismatch before any agent work; clearing the PlanApproved condition, surfacing the PlanMismatch condition and re-entering the AwaitingApproval hold",
				"loop", loop.Name, "approvedHash", approved)
		}
		c, advanced := r.recordPhaseClaim(ctx, loop, claim, now)
		changed = changed || c
		// I89: map a ModelUnavailable claim to a Loop condition so
		// `kubectl cox describe` shows the model backend is down. The
		// condition is set to False (reason=ModelUnavailable) when the
		// claim carries ModelUnavailable in its blocked reason, and back
		// to True when a subsequent claim succeeds (the model recovered).
		// This is a diagnostic signal (infrastructure failure), not a gate
		// input (ADR-0005 anti-gaming is preserved: the condition does not
		// affect the phase machine or the verify decision).
		r.applyModelAvailableCondition(loop, claim)
		// B3 (PLAN-BETA / PLAN.md Phase 4): the runner's plan-change signal.
		// When the runner's Planning run detected a PLAN CHANGE (the fresh
		// PLAN.md's SHA-256 does not match the approved hash the operator
		// wrote to .coxswain/plan-hash), the operator clears the PlanApproved
		// condition (re-enter the hold). The signal is a CLAIM (ADR-0005):
		// it is the runner's evidence that the plan on disk differs from the
		// approved hash; the operator's own read of the condition + phase is
		// the gate input, and the applyApprovalGate function (level-based) is
		// the one that decides the hold. The operator uses the claim only as
		// a trigger to clear the condition (the re-hold) — the gate then
		// holds the advance on the next reconcile (the claim's observedPhase
		// is Planning, the gate's Planning -> AwaitingApproval hold applies).
		// The clear is idempotent (a no-op when the condition is absent).
		if claim.PlanChanged && approvedPlanHash(loop) != "" {
			clearCondition(loop, coxv1alpha1.PlanApprovedCondition)
			changed = true
			logf.FromContext(ctx).Info("runner detected a plan change; clearing the PlanApproved condition (re-entering the hold)",
				"loop", loop.Name)
		}
		if advanced {
			// I83: the PhaseAdvanced Event is NOT emitted here. It is queued
			// (advancedFrom) and emitted by the caller only after the status
			// update persists the advance. Emitting here (before the status
			// update) is the race that double-emits the event when the status
			// update returns a Conflict and the requeue re-processes the claim.
			advancedFrom = fromPhase
		}
	} else {
		// claim == nil && cerr == nil: the agent has not terminated yet.
		claimReadPending = true
	}
	// B1 seam (R17, 2026-10-03): DELETED. The seam re-applied recordPhaseClaim
	// with {ObservedPhase: status.observedPhase} whenever the claim reader
	// found nothing, so while a phase was RUNNING (no terminated claim yet) it
	// rewrote progress on every reconcile with the PREVIOUS phase and an empty
	// status — OS1's progress was wrong for the whole duration of the running
	// phase. The seam existed only so the legacy B1 envtests could drive the
	// machine by writing status.observedPhase directly; those specs are now
	// ported to inject claims through the readPhaseClaim test seam instead
	// (loop_phase_transition_envtest_test.go). The claim reader is the ONLY
	// claim source: if it found nothing, the operator records nothing.
	return claimReadPending, changed, advancedFrom
}

// applyModelAvailableCondition (I89) maps the runner claim's model
// availability to a Loop condition. When the claim is blocked and its
// BlockedReason carries ModelUnavailable, the condition is set to False
// (reason=ModelUnavailable). When the claim succeeds, the condition is
// set back to True (reason=Recovered). The condition is purely
// diagnostic (visible via `kubectl cox describe`); it does not gate the
// phase machine or the verify decision (ADR-0005 anti-gaming preserved).
func (r *LoopReconciler) applyModelAvailableCondition(loop *coxv1alpha1.Loop, claim *PhaseClaim) {
	if claim.Status == "blocked" && strings.Contains(claim.BlockedReason, "ModelUnavailable") {
		setCondition(loop, coxv1alpha1.ModelAvailableCondition, metav1.ConditionFalse,
			"ModelUnavailable", "model backend unavailable: "+claim.BlockedReason)
	} else if claim.Status == "success" {
		setCondition(loop, coxv1alpha1.ModelAvailableCondition, metav1.ConditionTrue,
			"Recovered", "model backend reachable")
	}
}

// F6 (dogfood finding): the sandbox pod's infrastructure failure. An evicted
// or otherwise-terminated agent pod that left NO phase claim (eviction, a
// non-claim exit, a crash with no termination message) leaves the phase
// machine stuck: the claim reader re-queues forever (no claim, no decision)
// and nothing is surfaced. The operator must NOTICE it, SURFACE it (I90:
// AgentFailed condition + Warning Event) and retry in a bounded, visible way
// (recreate the Sandbox; count attempts) — or fail the Loop after N, mirroring
// the I65 VerifyInfraFailed infra-retry pattern.
const (
	// sandboxInfraRetries is the bounded retry count for a sandbox pod that
	// failed without a claim: 1 original pod + 2 recreations = 3 total before
	// the Loop is Failed:SandboxFailed. The bound keeps a persistently broken
	// node (a node whose memory pressure evicts every sandbox pod) from
	// recreating the Sandbox forever, and the recreation does not burn a
	// maxIterations iteration (a pod failure says nothing about the agent's
	// work — the ADR-0005 claim channel is exactly why a failure without a
	// claim is infra, not work).
	sandboxInfraRetries = 2
	// sandboxFailedCondition is the Loop condition type that SURFACES a
	// sandbox pod failure without a claim (F6, I90): True with the pod's
	// termination reason (Evicted, Error, OOMKilled, …) and the kubelet
	// message ("Usage of EmptyDir volume tmp exceeds the limit 100Mi") while
	// the operator is deciding what to do. Cleared (to False/Recovered) once
	// the Loop advances past the failure (a fresh claim arrives) or the bound
	// is exhausted (the Failed condition carries the durable record).
	sandboxFailedCondition = "AgentFailed"
	// sandboxInfraRetryEventReason / sandboxInfraFinalEventReason are the
	// stable Warning Event reasons for the sandbox infra retry and the final
	// failure (every state change has an Event — I90 observability).
	sandboxInfraRetryEventReason = "SandboxRetry"
	sandboxInfraFinalEventReason = "SandboxFailed"
	sandboxInfraFailedCondition  = "SandboxFailed"
)

// sandboxFailure is the operator's read of the sandbox pod for the F6
// failure check: it reports the pod's failed-without-a-claim state (nil when
// there is no such failure, the error when the pod read failed — the caller
// makes NO decision on a read error, I49). It is reached only when the
// phase machine has no current claim (advancePhaseFromClaim's claim reader
// found nothing or a malformed claim), so it never mis-fires on a pod that is
// progressing (the I49 norm: an in-progress agent — Waiting or Running — is
// never a failure).
type sandboxFailure struct {
	sandboxUID types.UID
	container  string
	reason     string
	exitCode   int32
	message    string
	podFailed  bool
}

// The F6 claim-wins check: the agent's own claim (its termination message)
// is the ADR-0004 channel, and a VALID claim is evidence the phase advanced
// even when the pod also ended up Failed. The envtest race (reviewer,
// 13:04): the agent exited 0 with a valid claim at about the same moment the
// kubelet evicted the pod for the EmptyDir overrun — the pod is Failed
// (Evicted) AND the claim is valid. The claim WINS: this is not a sandbox
// infrastructure failure, the advance path proceeds. A malformed/absent
// claim does not win (the failure surfaces, ADR-0005 fail-closed).
func (r *LoopReconciler) hasValidSandboxClaim(ctx context.Context, loop *coxv1alpha1.Loop) bool {
	claim, cerr := r.resolvePhaseClaim(ctx, loop)
	return cerr == nil && claim != nil
}

func (r *LoopReconciler) sandboxFailure(ctx context.Context, loop *coxv1alpha1.Loop) (*sandboxFailure, error) {
	reader := r.apiReader
	if reader == nil {
		// No APIReader wired (a bare test reconciler without
		// SetupWithManager): fall back to the cached client. If THAT is also
		// nil (the S5a discard spec builds the reconciler with no client at
		// all — only the readPhaseClaim seam), there is no reader: no
		// decision (the F6 check is a no-op for a bare reconciler).
		if r.Client == nil {
			return nil, nil
		}
		reader = r
	}
	sb := &sandboxv1beta1.Sandbox{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: sandboxName(loop.Name)}, sb); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil // no Sandbox yet (or it is being deleted): no decision
		}
		return nil, err
	}
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: sandboxName(loop.Name)}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil // pod not created yet (or deleted): no decision
		}
		return nil, err
	}
	// A pod that has not FAILED (Running/Pending): no decision (I49 — an
	// in-progress agent is never a failure).
	if pod.Status.Phase != corev1.PodFailed {
		return nil, nil
	}
	agent := agentContainerStatus(pod)
	if agent == nil {
		// No agent container status yet (the pod failed before the agent
		// registered — e.g. a workspace init failure): still a failed pod with
		// no claim; the decision below is the same (surface + retry) but the
		// condition names the pod-level reason instead of a container's.
		podReason, podMsg := podStatusMessage(pod)
		return &sandboxFailure{
			sandboxUID: sb.UID,
			container:  "<pod>",
			reason:     podReason,
			message:    podMsg,
			podFailed:  true,
		}, nil
	}
	// The agent container has a live (non-terminated) state: the pod is in
	// progress (Running/Waiting), not a failure without a claim (I49: no
	// decision).
	if agent.State.Terminated == nil && agent.LastTerminationState.Terminated == nil {
		return nil, nil
	}
	// The agent container terminated: if the termination carried a VALID
	// claim, the claim wins (ADR-0004: the claim channel is the only thing
	// that advances) and this is NOT a sandbox infrastructure failure —
	// the evict-and-exit race the reviewer observed (the agent exited 0 at
	// about the same moment as the kubelet eviction) advances the Loop
	// instead of retrying the sandbox. A malformed or absent claim is no
	// claim: the F6 failure stands.
	if r.hasValidSandboxClaim(ctx, loop) {
		return nil, nil
	}
	// The agent container terminated WITHOUT a valid claim: this is the
	// failure the F6 check is for (the claim reader already returned nothing
	// or a malformed claim). Surface the container's termination evidence.
	term := agent.State.Terminated
	if term == nil {
		term = agent.LastTerminationState.Terminated
	}
	// The pod-level status carries the KUBELET's word for the failure — on
	// an eviction the kubelet sets pod.status.reason="Evicted" and
	// pod.status.message ("Usage of EmptyDir volume tmp exceeds the limit
	// 100Mi") while the container's termination reason is the coarser
	// "Error" (exit 137). Prefer the pod status when set; fall back to the
	// container's (the doc promise on AgentFailedCondition — its reason is
	// the termination reason, its message the kubelet message). A
	// DisruptionTarget/TerminationByKubelet condition message is a secondary
	// source for the kubelet text (its Status=True marks the target, not a
	// failure condition, so it is never scanned as one).
	reason, message := term.Reason, term.Message
	if podReason, podMsg := podStatusMessage(pod); podReason != podFailedReason {
		reason = podReason
		if podMsg != "" {
			message = podMsg
		}
	}
	return &sandboxFailure{
		sandboxUID: sb.UID,
		container:  agent.Name,
		reason:     reason,
		exitCode:   term.ExitCode,
		message:    message,
		podFailed:  true,
	}, nil
}

// applySandboxFailure is the F6 bounded-retry decision for a sandbox pod that
// failed without a claim. It mirrors applyVerifyInfraFailure (I65):
//
//  1. it SURFACES the failure (I90): the AgentFailed condition (True, the
//     container's reason + the kubelet message) and a Warning Event per retry
//     and on the final failure — the wedge F6 fixes (a silent Planning) is
//     made visible;
//  2. it counts the failure into status.agent.infraAttempts (the
//     per-Sandbox-UID guard, infraSandboxUID — a re-read of the same failing
//     Sandbox does not double-count, the I65 P1-A pattern);
//  3. while within the bound (sandboxInfraRetries recreations) it DELETES the
//     Sandbox so the next reconcile recreates a fresh one (a NEW Sandbox —
//     the operator never mutates a running one; the workspace survives on
//     the per-Loop PVC) and requeues; and
//  4. once exhausted it fails the Loop with the distinct SandboxFailed
//     reason (the Failed condition, phase Failed, the Warning Event).
//
// It returns changed (whether the Loop's status was mutated this call — the
// caller ORs it in and persists in the shared Status().Update). A failed
// delete is returned as an error (the reconcile retries; the attempt count is
// already persisted, so a re-reconcile does not double-count — the same
// failed-delete handling as applyVerifyInfraFailure).
func (r *LoopReconciler) applySandboxFailure(ctx context.Context, loop *coxv1alpha1.Loop, f *sandboxFailure) bool {
	log := logf.FromContext(ctx)
	msg := sandboxFailureMessage(f)
	// I90: surface the failure. The condition carries the reason + the kubelet
	// message ("Usage of EmptyDir volume tmp exceeds the limit 100Mi"); the
	// message is kubelet-recorded (not the runner's clock, not the agent's
	// claim — there is no claim, which is the whole point).
	setCondition(loop, sandboxFailedCondition, metav1.ConditionTrue, f.reason, msg)
	if loop.Status.Agent == nil {
		loop.Status.Agent = &coxv1alpha1.AgentStatus{}
	}
	// I65 P1-A (mirrored): count at most ONE infra attempt per Sandbox UID. A
	// re-read of the same failing Sandbox (the reconcile re-ran before the
	// delete completed, or the stale pod still owned by the old Sandbox) must
	// not burn another attempt.
	if loop.Status.Agent.InfraSandboxUID != "" && string(f.sandboxUID) == loop.Status.Agent.InfraSandboxUID {
		// Already counted: re-issue the delete if the Sandbox is still present
		// and not Terminating (a transient delete failure left it in place),
		// and keep the condition + Event visible. Never a controller error,
		// never a double-count.
		if r.ReissueSandboxDelete(ctx, loop) {
			return true
		}
		log.Info("sandbox failure re-read (same UID); skipping (already counted)", "loop", loop.Name, "attempt", loop.Status.Agent.InfraAttempts)
		return false
	}
	loop.Status.Agent.InfraAttempts++
	loop.Status.Agent.InfraSandboxUID = string(f.sandboxUID)
	attempt := loop.Status.Agent.InfraAttempts
	if attempt > sandboxInfraRetries {
		// Exhausted: fail the Loop with the distinct reason. The Failed
		// condition carries the durable record; the AgentFailed condition is
		// cleared (the Failed condition supersedes it).
		setCondition(loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue,
			sandboxInfraFailedCondition,
			fmt.Sprintf("sandbox failed without a phase claim after %d attempts (%s)", attempt, msg))
		clearCondition(loop, sandboxFailedCondition)
		loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
		bumpLoopCompletion("failed", "sandbox_infra")
		if r.Recorder != nil {
			r.Recorder.Eventf(loop, corev1.EventTypeWarning, sandboxInfraFinalEventReason,
				"sandbox failed without a phase claim after %d attempts: %s", attempt, msg)
		}
		return true
	}
	// Within the bound: surface + recreate the Sandbox and requeue.
	if r.Recorder != nil {
		r.Recorder.Eventf(loop, corev1.EventTypeWarning, sandboxInfraRetryEventReason,
			"sandbox pod failed without a phase claim (attempt %d of %d); recreating the sandbox: %s",
			attempt, sandboxInfraRetries+1, msg)
	}
	if r.DeleteSandboxForRetry(ctx, loop) {
		// A failed delete: the attempt count is already persisted, so the next
		// reconcile sees the same failing Sandbox and the same count and
		// retries the delete (never a controller error, never a wedge).
		log.Error(nil, "failed to delete sandbox for recreate; requeueing (will retry)", "loop", loop.Name, "attempt", attempt)
		return true
	}
	log.Info("sandbox pod failed without a claim; recreating the sandbox", "loop", loop.Name, "attempt", attempt)
	return true
}

// DeleteSandboxForRetry deletes the Loop's Sandbox so the next reconcile
// recreates it fresh (the F6 sandbox infra-retry, the I65 verify-Job-recreate
// pattern). It returns true when the delete FAILED (the caller requeues and
// retries on the next reconcile) and false when it succeeded or the Sandbox
// is already gone (nothing to delete — the recreate is the next reconcile's
// ensureSandbox work).
func (r *LoopReconciler) DeleteSandboxForRetry(ctx context.Context, loop *coxv1alpha1.Loop) bool {
	sb := &sandboxv1beta1.Sandbox{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: sandboxName(loop.Name)}, sb); err != nil {
		if apierrors.IsNotFound(err) {
			return false // already gone
		}
		// A failed read is treated as "delete unknown" — the caller requeues
		// and retries; it is NOT a controller error (never a wedge).
		return true
	}
	if sb.DeletionTimestamp != nil {
		return false // already Terminating: the GC is handling it
	}
	// Background propagation deletes the pod with the Sandbox (the D27
	// pattern: the apiserver's default ORPHAN propagation leaves the stale
	// pod in place without a GC, and the pod would carry the failed agent
	// statuses into the new Sandbox's read-back). The fresh Sandbox is
	// created on the next reconcile (the name is still taken until the
	// apiserver deletes async — the same requeue dance the D27 stale-guard
	// uses).
	if err := r.Delete(ctx, sb, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return true
	}
	return false
}

// ReissueSandboxDelete re-issues the background delete of the Loop's Sandbox
// when it is still present and NOT yet Terminating (a transient delete
// failure left it in place — the I65 P1-A re-read path). It returns true
// when it re-issued (the caller requeues) and false when the Sandbox is gone
// or already Terminating (the GC is handling it).
func (r *LoopReconciler) ReissueSandboxDelete(ctx context.Context, loop *coxv1alpha1.Loop) bool {
	sb := &sandboxv1beta1.Sandbox{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: sandboxName(loop.Name)}, sb); err != nil {
		if apierrors.IsNotFound(err) {
			return false // gone: nothing to re-issue
		}
		return false // a read failure: no decision (I49)
	}
	if sb.DeletionTimestamp != nil {
		return false // already Terminating
	}
	if err := r.Delete(ctx, sb, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return true
	}
	return true
}

// sandboxFailureMessage builds the human-readable failure message for the
// AgentFailed condition / Events / the SandboxFailed Failed-condition:
// it names the failed container and its termination reason + exit code, plus
// the kubelet message (the eviction reason, "Usage of EmptyDir volume tmp
// exceeds the limit 100Mi") when the status carries one. A failed pod with
// no agent container status names the pod-level condition instead.
func sandboxFailureMessage(f *sandboxFailure) string {
	if f == nil {
		return "sandbox pod failed without a phase claim"
	}
	s := "the sandbox pod's " + f.container + " container terminated without a phase claim"
	if f.reason != "" {
		s += " (reason " + f.reason
		if f.exitCode != 0 {
			s += ", exit " + strconv.FormatInt(int64(f.exitCode), 10)
		}
		s += ")"
	} else if f.exitCode != 0 {
		s += fmt.Sprintf(" (exit %d)", f.exitCode)
	}
	if f.message != "" {
		s += ": " + truncate(s4ClaimMaxBytes, f.message)
	}
	return s
}

// clearRecoveredCondition is the operator's "recovered" clear for a SURFACE
// condition the operator itself sets (AgentFailed — F6): it upserts the
// condition to False / Recovered, and returns false (a no-op) when the
// condition is absent or already False. Unlike clearCondition (which removes
// a user-set condition), the recovered state is left in place as the audit
// record that the Loop recovered (the I90 visibility: the condition's
// LastTransitionTime is when the recovery was observed).
func clearRecoveredCondition(loop *coxv1alpha1.Loop, condType string) bool {
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == condType {
			if loop.Status.Conditions[i].Status != metav1.ConditionTrue {
				return false // absent or already False: no change
			}
			loop.Status.Conditions[i].Status = metav1.ConditionFalse
			loop.Status.Conditions[i].Reason = "Recovered"
			loop.Status.Conditions[i].Message = "the agent recovered from the sandbox failure (a fresh phase claim arrived)"
			loop.Status.Conditions[i].LastTransitionTime = metav1.Now()
			return true
		}
	}
	return false // absent: a no-op
}

// agentContainerStatus returns the agent container's status (nil when the
// pod has no agent container status yet).
func agentContainerStatus(pod *corev1.Pod) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == agentContainerNameS4 {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

const podFailedReason = "PodFailed"

// podStatusMessage returns the pod-level failure evidence from the POD
// STATUS (the kubelet's own words): pod.status.reason ("Evicted", "Low
// on ephemeral-storage", ...) and pod.status.message ("Usage of EmptyDir
// volume tmp exceeds the limit 100Mi") when set. A DisruptionTarget
// condition set to TerminationByKubelet (Status=True — it marks the pod as
// a disruption target, not a failure condition) is a SECONDARY source for
// the message only; the generic failure conditions (Ready=False/PodFailed,
// ContainersReady=False) carry no message and are deliberately NOT scanned
// (a real failed pod's Ready condition is reason PodFailed with an empty
// message — scanning it would hide the kubelet text). The caller still
// treats the pod as failed — pod.Status.Phase is the primary signal.
func podStatusMessage(pod *corev1.Pod) (string, string) {
	reason := podFailedReason
	if pod.Status.Reason != "" {
		reason = pod.Status.Reason
	}
	message := pod.Status.Message
	if message == "" {
		for i := range pod.Status.Conditions {
			c := &pod.Status.Conditions[i]
			if c.Type == corev1.DisruptionTarget && c.Status == corev1.ConditionTrue && c.Reason == "TerminationByKubelet" {
				message = c.Message
				if reason == podFailedReason {
					reason = "Evicted"
				}
			}
		}
	}
	return reason, message
}

// truncate caps s to n bytes (the I90 condition/Event messages must stay
// readable; a kubelet message that grows past the cap is truncated, not
// silently dropped).
func truncate(n int, s string) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
