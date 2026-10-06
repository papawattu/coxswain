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

// P2e stall gate (TDD-PLAN-PHASE2 P2e, item 2). The decision is a PURE
// function of (status.stallHistory, the failing check's raw output, the
// jobName, the iteration, the now) — see stallDecision. The operator:
//
//  1. reads the failing check's raw output (readCheckOutput: the
//     terminationMessage of the failing check container — NO pod-log read),
//  2. normalises it (internal/stall.NormalizeWithHash),
//  3. appends a StallEntry to status.stallHistory (dedup'd by jobName, ring
//     of the last 10),
//  4. evaluates stallDecision: N+ consecutive identical hashes (within the
//     same normalisation version) fires the stall detector.
//
// The gate is evaluated BEFORE the budget cap in the same reconcile
// (stall wins; a capped loop that is also stalled is Failed:Stalled, not
// Failed with a budget reason). The stall decision is inert in Paused (the
// Paused phase owns the decision; resuming re-evaluates, P2f).
package controller

import (
	"context"
	"fmt"
	"strconv"

	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/stall"
)

// stallHistoryCap is the ring size (the last 10 verify-failure iterations,
// PLAN.md item 5). A new entry evicts the oldest beyond the cap.
const stallHistoryCap = 10

// stallDecision is the PURE stall gate (a pure function of the history + the
// just-appended entry + the spec):
//
//   - N+ consecutive entries with the IDENTICAL hash (within the same
//     normalisation version) → fire (true). N is spec.loop.stallAfter (the
//     CRD default 3 when unset; the API's pointer is nil when unset).
//   - A hash DIFFERENT from the previous entry (or a normalisation-version
//     change) RESETS the consecutive run to 1 (no fire unless N+ is 1, which
//     the spec forbids — stallAfter is Minimum=1 but the detector needs N
//     consecutive, and a single entry is 1 < N for N>=2; for N=1 a single
//     failure fires — the spec's Minimum=1 allows that and the decision
//     reflects it: one entry, N=1 → N+ = 1 consecutive → fire).
//   - The version must match the CURRENT version (a stale-version run does
//     not count toward the current detector — a version change resets).
//   - Per-Job (item C's consistent model): the detector evaluates the fire
//     ONCE per NEW verify Job. A re-read of the SAME Job (a requeue, a stale
//     read after a phase recycle that did not advance the iteration) is not
//     a new evaluation — the history's trailing entry already carries the
//     just-appended entry's jobName, and the detector must not re-fire on it
//     (spec 11: a terminal Failed fire does not re-fire on a re-reconcile
//     with the same evidence; a paused Loop's decision is inert in Paused
//     but a re-read in Verifying must not re-evaluate either). The dedup
//     (appendStallEntry) already guarantees a Job name appears at most once
//     in the history, so a trailing entry with the same jobName IS a re-read.
//
// It returns (fired bool, consecutive int): consecutive is the length of the
// trailing run of identical (hash, version) entries including the just-
// appended one (the operator records it into the Stalled condition message).
func stallDecision(history []coxv1alpha1.StallEntry, newEntry coxv1alpha1.StallEntry, stallAfter int32) (bool, int) {
	if len(history) > 0 {
		last := history[len(history)-1]
		if last.JobName != "" && last.JobName == newEntry.JobName {
			// A re-read of the SAME verify Job (not a new Job): the detector
			// does not re-evaluate. No fire — the per-Job rule (item C).
			return false, 0
		}
	}
	if stallAfter <= 0 {
		// stallAfter is a pointer; the caller passes the resolved value
		// (CRD default 3 when nil). A resolved value of 0 is malformed —
		// treat as 3 (the default) so a bare reconcile never fires on a
		// single entry (a guard against a zero pointer the CRD would reject).
		stallAfter = 3
	}
	// The consecutive run is the trailing run of entries equal to newEntry's
	// (hash, normalisationVersion), INCLUDING newEntry itself. Find the first
	// entry from the end that does NOT match newEntry; the run is everything
	// after it (including newEntry). A mismatch (or the head) stops the run.
	run := 1
	for _, e := range slices.Backward(history) {
		if e.Hash != newEntry.Hash || e.NormalisationVersion != newEntry.NormalisationVersion {
			break
		}
		run++
	} // Fire when the run reaches N (the spec's "N consecutive" — the plan
	// says "N consecutive identical hashes" fires; the CRD default N=3 means
	// 3 consecutive failures). The run counts newEntry + the matching
	// trailing history, so a run of N means N consecutive identical
	// failures (newEntry being the Nth).
	return run >= int(stallAfter), run
}

// stallDefaultsConfigMap is the cluster-wide stall-default ConfigMap (plan
// P2e "Effective config": the operator namespace, keys stallAfter /
// stallAction) — the FALLBACK when the Loop's spec.loop.stallAfter is
// unset: precedence Loop field > ConfigMap > built-in (3 / Fail). A
// missing/malformed ConfigMap falls back to the built-in default — the
// stall detector is NOT fail-closed on a missing ConfigMap (an undetected
// stall is worse than a default — the opposite of the budget's fail-closed
// prices, deliberate, documented in the field comment).
const stallDefaultsConfigMap = "coxswain-stall-defaults"

// resolveStallAfter reads the effective stallAfter: spec.loop.stallAfter
// (a pointer, CRD-default 3 when nil) > the cluster-wide
// coxswain-stall-defaults ConfigMap (operator namespace) > the built-in
// default 3. The API pointer is *int32; when nil the CRD defaulting sets 3
// on a real object, but a bare reconcile (no defaulting) gets nil → the
// ConfigMap, then the built-in 3.
func resolveStallAfter(ctx context.Context, r *LoopReconciler, loop *coxv1alpha1.Loop) int32 {
	if loop.Spec.Loop.StallAfter != nil {
		return *loop.Spec.Loop.StallAfter
	}
	if r != nil {
		cm := &corev1.ConfigMap{}
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: r.OperatorNamespace,
			Name:      stallDefaultsConfigMap,
		}, cm); err == nil {
			if v, err := strconv.Atoi(cm.Data["stallAfter"]); err == nil && v >= 1 {
				return int32(v)
			}
		}
	}
	return 3
}

// readCheckOutput is the default (live) check-output read: the failing
// check container's terminationMessage. A check container that just ran and
// terminated (restartCount 0) carries the kubelet's 4 KB tail in
// status.initContainerStatuses[].state.terminated.message (the CURRENT state)
// — this is the field a real (non-restarted) init container populates. A
// RESTARTED container (restartCount > 0) instead carries its previous
// incarnation's message in lastState.terminated.message. So read the CURRENT
// state.terminated.message FIRST (the real production path: a verify check
// init container runs once and terminates, restartCount 0) and fall back to
// lastState for a restarted container. Reading ONLY lastState (the original
// bug, exposed by the P2e kind run) found nothing for a non-restarted check
// and the stall gate went inert — no StallEntry was ever recorded in
// production. The check containers carry a terminationMessagePath (item 2), so
// the message is set when the check ran and wrote output. It is a SEAM on the
// LoopReconciler (readCheckOutput func(...) field) so the envtest specs drive
// a deterministic output without a real verify Job (the spec's readCheckOutput
// seam).
//
// It returns ("", false) when neither the current nor the last terminated
// record carries a message — a check that never ran, or a check that ran but
// wrote nothing (an empty output). The operator treats ("", false) as INERT
// (no StallEntry, the caller proceeds to the iterate): an empty read is the
// absence of evidence, not an identical failure.
func (r *LoopReconciler) defaultReadCheckOutput(pod *corev1.Pod, checkName string) (string, bool) {
	if pod == nil {
		return "", false
	}
	for i := range pod.Status.InitContainerStatuses {
		ics := &pod.Status.InitContainerStatuses[i]
		if ics.Name != checkName {
			continue
		}
		// The CURRENT terminated state first: a non-restarted check init
		// (restartCount 0, the real production path) carries its
		// terminationMessage here.
		if ics.State.Terminated != nil {
			if msg := ics.State.Terminated.Message; msg != "" {
				return msg, true
			}
		}
		// Fall back to the LAST terminated state: a restarted container
		// (restartCount > 0) carries its previous incarnation's message there.
		if ics.LastTerminationState.Terminated != nil {
			if msg := ics.LastTerminationState.Terminated.Message; msg != "" {
				return msg, true
			}
		}
		// A terminated record exists (current or last) but carried no message:
		// a check that ran but wrote nothing -> ("", false) (INERT), matching
		// the "no message -> inert" contract the envtest spec pins.
		return "", false
	}
	return "", false // no terminated record at all: the check never ran
}

// appendStallEntry records the failing check's normalised output into
// status.stallHistory (a P2c/P2e ring, dedup'd by jobName, capped at
// stallHistoryCap). It returns the appended entry (the caller uses it for
// the stall decision). The entry is NOT appended if an entry with the same
// jobName already exists (item 6: a re-read of the same verify Job appends
// no entry — the dedup key).
// The At is the kubelet-recorded finish time (stallEntryAt): a real kubelet
// always sets a finish time on a terminated container, so a real entry's At
// is always set (the CRD's +kubebuilder:validation:Required on
// status.stallHistory[].at is then satisfied). The envtest fixture's pods
// carry no finish time, so stallEntryAt falls back to now() (an envtest-only
// artifact — see stallEntryAt's comment).
func appendStallEntry(loop *coxv1alpha1.Loop, jobName, hash, check string, at *metav1.Time) coxv1alpha1.StallEntry {
	// Dedup by jobName (item 6).
	for i := range loop.Status.StallHistory {
		if loop.Status.StallHistory[i].JobName == jobName {
			return loop.Status.StallHistory[i]
		}
	}
	iteration := loop.Status.Iteration
	entry := coxv1alpha1.StallEntry{
		Iteration:            iteration,
		JobName:              jobName,
		Hash:                 hash,
		NormalisationVersion: stall.NormalisationVersionV1,
		Check:                check,
		At:                   *at,
	}
	loop.Status.StallHistory = append(loop.Status.StallHistory, entry)
	// Cap the ring at the last stallHistoryCap (evict the oldest).
	if len(loop.Status.StallHistory) > stallHistoryCap {
		loop.Status.StallHistory = loop.Status.StallHistory[len(loop.Status.StallHistory)-stallHistoryCap:]
	}
	return entry
}

// verifyJobName returns the verify Job's name for the current iteration
// (the StallEntry dedup key, item 6): <loop>-verify-<iteration>.
func (r *LoopReconciler) verifyJobName(loop *coxv1alpha1.Loop) string {
	return loop.Name + "-verify-" + strconv.Itoa(loop.Status.Iteration)
}

// readCheckOutputSeam reads the failing check's raw output via the seam
// (the envtest override) or the default live read (the terminationMessage).
func (r *LoopReconciler) readCheckOutputSeam(pod *corev1.Pod, checkName string) (string, bool) {
	if r.readCheckOutput == nil {
		return r.defaultReadCheckOutput(pod, checkName)
	}
	return r.readCheckOutput(pod, checkName)
}

// normalizeCheckOutput normalises a raw check output via internal/stall and
// returns (normalised, hash) — the stall gate's hash source.
func normalizeCheckOutput(raw string) (string, string) {
	return stall.NormalizeWithHash(raw)
}

// applyStallGate is the P2e stall gate, evaluated at a verify failure BEFORE
// the budget/maxIterations cap (stall wins). It:
//
//  1. reads the failing check's raw output (r.readCheckOutput — the seam or
//     the terminationMessage),
//  2. normalises it (stall.NormalizeWithHash → hash),
//  3. appends the StallEntry to status.stallHistory (dedup'd by jobName, the
//     ring capped at the last 10),
//  4. evaluates stallDecision over the history (N+ consecutive identical
//     hashes within the current normalisation version fires),
//  5. on a fire, applies the stall action (Fail → Failed:Stalled; Pause →
//     Paused with pausedReason Stall; Continue → the Stalled condition is set
//     and the Loop keeps iterating).
//
// It returns true when the gate TOOK the decision (a fire was applied) so the
// caller returns immediately; false when it did not fire (the caller
// continues to the iterate / cap path).
//
// Inert in Paused (the Paused phase owns the decision; the gate only runs at
// a Verifying verify-failure, and a Paused loop is not Verifying — P2f).
func (r *LoopReconciler) applyStallGate(ctx context.Context, loop *coxv1alpha1.Loop, pod *corev1.Pod, failedCheck string) bool {
	if loop.Status.Phase != coxv1alpha1.LoopPhaseVerifying {
		return false
	}
	// The terminal gate (I49, spec 9/10) lives in the CALLER, not here:
	// verifyOutcome returns (verifyNoDecision, requeue=true) when a check
	// container has not TERMINATED (a check still Running, a Job pod whose
	// inits have not started), and applyVerifyOutcome requeues BEFORE it
	// ever reaches the verifyIterate branch that calls this gate. So this
	// gate is only reached on a TERMINAL verify failure (a check-* container
	// Terminated non-zero — the B3 evidence), and the StallEntry is appended
	// only there. The redundant inner checkNotTerminated was removed (M4):
	// the caller's in-progress requeue is the real terminal gate, and this
	// gate's own check was dead code that no spec could isolate. A direct
	// call to applyStallGate with a non-terminal pod is not a supported path
	// (the caller never does it); the gate's contract is "reached only on a
	// terminal verify failure".
	raw, ok := r.readCheckOutputSeam(pod, failedCheck)
	if !ok {
		// No output read (the check container's terminationMessage is absent —
		// e.g. a check that never ran, or a stand-in pod with no
		// termination message). The gate is INERT: an empty read is not
		// evidence of an identical failure (it is the absence of evidence), so
		// no StallEntry is appended and the caller proceeds to the iterate /
		// cap path. (A check that GENUINELY produces no output would still be
		// detected by a non-empty terminationMessage of the empty string only
		// when the container wrote nothing AND the tee ran — in practice the
		// tee always writes a file, so "" is a real empty-output failure and
		// the default read returns ok=true for it. The ok=false path is the
		// truly-absent message.)
		return false
	}
	_, hash := normalizeCheckOutput(raw)
	jobName := r.verifyJobName(loop)
	at := stallEntryAt(pod, failedCheck)
	historyBefore := loop.Status.StallHistory // the history BEFORE this entry (the dedup key: the last entry's jobName, a pre-existing one)
	entry := appendStallEntry(loop, jobName, hash, failedCheck, at)
	fired, run := stallDecision(historyBefore, entry, resolveStallAfter(ctx, r, loop))
	if !fired {
		return false
	}
	action := loop.Spec.Loop.StallAction
	if action == "" {
		action = coxv1alpha1.StallActionFail // the CRD default
	}
	switch action {
	case coxv1alpha1.StallActionPause:
		// Item 6: the Pause is entered AFTER the iterate bookkeeping. The
		// iterate's bookkeeping runs BEFORE the fire is applied (the caller's
		// ordering: applyStallGate is evaluated at a verify failure, and on a
		// fire the loop's phase/iteration/condition are updated as ONE unit
		// with the Pause). The iterate's bookkeeping (the phase advance
		// Verifying->Implementing + the iteration increment) is applied FIRST,
		// then the Pause overrides the phase to Paused (the iterate would have
		// set Implementing; the Pause keeps that as pausedFrom). The iteration
		// has ALREADY advanced (3 -> 4) when the Paused is set — the iterate
		// ran first. Mutation: setting pausedFrom=Verifying (the phase BEFORE
		// the iterate) must make spec 3 FAIL (the resume returns to Verifying,
		// not Implementing).
		// P2g (the auditability sweep): the Paused condition + the Normal
		// Paused Event are emitted HERE (the stall entry point), not via the
		// P2f suspend path (the stall's source is named in both).
		loop.Status.PausedFrom = coxv1alpha1.LoopPhaseImplementing // the phase the iterate would have set
		loop.Status.Phase = coxv1alpha1.LoopPhasePaused
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhasePaused
		loop.Status.PausedReason = coxv1alpha1.PausedReasonStall
		setCondition(loop, coxv1alpha1.PausedCondition, metav1.ConditionTrue,
			pausedCondReasonPaused,
				"paused (stall): stall detector fired after "+fmt.Sprintf("%d", run)+" consecutive identical verify failures")
		setCondition(loop, string(coxv1alpha1.StalledCondition), metav1.ConditionTrue,
			"Stalled", fmt.Sprintf("stall detector fired: %d consecutive identical verify failures (stallAction=Pause)", run))
		if r.Recorder != nil {
			r.Recorder.Eventf(loop, corev1.EventTypeNormal, pauseEventReason,
				"paused: source stall, from phase %s (stall detector fired)", loop.Status.PausedFrom)
			r.Recorder.Eventf(loop, corev1.EventTypeWarning, "StallDetected", "stall detector fired: %d consecutive identical verify failures (stallAction=Pause)", run)
		}
	case coxv1alpha1.StallActionContinue:
		// Keep iterating: the Stalled condition is set, the phase stays
		// Verifying->Implementing (the caller proceeds to the iterate).
		setCondition(loop, string(coxv1alpha1.StalledCondition), metav1.ConditionTrue,
			"Stalled", fmt.Sprintf("stall detector fired: %d consecutive identical verify failures (stallAction=Continue, keeping the loop)", run))
		if r.Recorder != nil {
			r.Recorder.Eventf(loop, corev1.EventTypeWarning, "StallDetected", "stall detector fired: %d consecutive identical verify failures (stallAction=Continue, keeping the loop)", run)
		}
		return false // keep iterating
	default: // StallActionFail
		setCondition(loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue,
			"Stalled", fmt.Sprintf("stall detector fired: %d consecutive identical verify failures (stallAction=Fail)", run))
		// The Stalled condition is set (the stall's decision): the stall gate's
		// Fail action sets BOTH the Failed condition (the phase outcome, reason
		// Stalled) AND the Stalled condition (the detector's record). Item 8's
		// precedence: the stall's outcome wins over the budget's (Failed:
		// Stalled, not Failed:BudgetExceeded).
		setCondition(loop, string(coxv1alpha1.StalledCondition), metav1.ConditionTrue,
			"Stalled", fmt.Sprintf("stall detector fired: %d consecutive identical verify failures (stallAction=Fail)", run))
		loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
		if r.Recorder != nil {
			r.Recorder.Eventf(loop, corev1.EventTypeWarning, "StallDetected", "stall detector fired: %d consecutive identical verify failures (stallAction=Fail)", run)
		}
	}
	return true
}

// stallEntryAt is the StallEntry's At (the kubelet-recorded finish time,
// RFC3339): the check container's State.Terminated.FinishedAt, then
// LastTerminationState.Terminated.FinishedAt, then the pod's
// LastTransitionTime. An empty (zero) time means the kubelet has not yet
// recorded a finish — the envtest fixture's pods carry no finish time,
// which would otherwise make the entry's At zero and the CRD's
// +kubebuilder:validation:Required on status.stallHistory[].at would reject
// the status update (an envtest-only artifact: a real kubelet always sets a
// finish time on a terminated container).
func stallEntryAt(pod *corev1.Pod, checkName string) *metav1.Time {
	if pod == nil {
		return nil
	}
	for i := range pod.Status.InitContainerStatuses {
		ics := &pod.Status.InitContainerStatuses[i]
		if ics.Name != checkName {
			continue
		}
		if t := ics.State.Terminated; t != nil && !t.FinishedAt.IsZero() {
			mt := metav1.Time{Time: t.FinishedAt.Time}
			return &mt
		}
		if t := ics.LastTerminationState.Terminated; t != nil && !t.FinishedAt.IsZero() {
			mt := metav1.Time{Time: t.FinishedAt.Time}
			return &mt
		}
	}
	now := metav1.Now()
	return &now
}
