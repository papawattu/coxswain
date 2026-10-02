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
// phase ended blocked (the message still carries status:"blocked"). The
// sandbox pod runs restartPolicy Never, so the kubelet leaves the agent
// container Terminated (no restart, no message churn) and the message stays
// readable until the operator recreates the pod.
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
	"strings"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	claim.ObservedPhase = coxv1alpha1.LoopPhase(op)
	if claim.ObservedPhase == "" {
		return nil, fmt.Errorf("claim has no observedPhase")
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
		if cs.State.Terminated == nil {
			// The agent is still running the phase (or has no status yet):
			// no claim yet. Requeue (the phase-init container has written the
			// desired phase and the runner is doing the work).
			return nil, nil
		}
		claim, err := parsePhaseClaim(cs.State.Terminated.Message)
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

// phaseAdvancedReason is the stable Event reason the operator emits on every
// phase transition (OS5, R19: "stable reasons, e.g. PhaseAdvanced"). The
// reason is a fixed string (documented as an API); the message carries the
// from/to phases.
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
func (r *LoopReconciler) recordPhaseClaim(loop *coxv1alpha1.Loop, claim *PhaseClaim, now metav1.Time) (changed, advanced bool) {
	// OS1: record the claim into status.progress. The pins (observed
	// generation / base commit) are the OPERATOR's, never the claim's (a
	// claim cannot set a pin for itself).
	p := &coxv1alpha1.ProgressStatus{
		Phase:            claim.ObservedPhase,
		LastActivityTime: &now,
		Iteration:        claim.Iteration,
		LastResultStatus: claim.Status,
		BlockedReason:    claim.BlockedReason,
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
	next := nextPhase(loop.Status.Phase, claimPhaseForAdvance(claim))
	if next != loop.Status.Phase {
		gate := r.phaseGate
		if gate == nil {
			gate = autoApprovePhaseGate{}
		}
		allow, _ := gate.Allow(loop, loop.Status.Phase, next)
		if allow {
			loop.Status.Phase = next
			loop.Status.DesiredPhase = next
			advanced = true
			changed = true
		}
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
		a.BlockedReason == b.BlockedReason && a.Iteration == b.Iteration
}

// advancePhaseFromClaim (S4) reads the runner's ADR-0004 claim from the
// sandbox pod's termination message and advances the phase machine. The claim
// reader is the ONLY claim source (the legacy B1 seam that fell back to
// status.observedPhase was deleted in R17 — it clobbered progress while a
// phase was running). Returns (claimReadPending, changed):
// claimReadPending is true when the operator must requeue (the claim reader
// found nothing or a malformed claim); changed is true when the Loop's status
// was mutated (progress record or phase advance).
func (r *LoopReconciler) advancePhaseFromClaim(ctx context.Context, loop *coxv1alpha1.Loop) (bool, bool) {
	claimReadPending := false
	changed := false
	if loop.Status.Phase == coxv1alpha1.LoopPhaseSucceeded || loop.Status.Phase == coxv1alpha1.LoopPhaseFailed {
		return false, false
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
		return false, false
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
		now := metav1.Now()
		fromPhase := loop.Status.Phase
		c, advanced := r.recordPhaseClaim(loop, claim, now)
		changed = changed || c
		if advanced {
			r.emitPhaseAdvancedEvent(loop, fromPhase)
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
	return claimReadPending, changed
}
