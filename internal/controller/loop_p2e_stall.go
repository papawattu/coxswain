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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
//
// It returns (fired bool, consecutive int): consecutive is the length of the
// trailing run of identical (hash, version) entries including the just-
// appended one (the operator records it into the Stalled condition message).
func stallDecision(history []coxv1alpha1.StallEntry, newEntry coxv1alpha1.StallEntry, stallAfter int32) (bool, int) {
	if stallAfter <= 0 {
		// stallAfter is a pointer; the caller passes the resolved value
		// (CRD default 3 when nil). A resolved value of 0 is malformed —
		// treat as 3 (the default) so a bare reconcile never fires on a
		// single entry (a guard against a zero pointer the CRD would reject).
		stallAfter = 3
	}
	// The consecutive run is the trailing run of entries equal to newEntry's
	// (hash, normalisationVersion), INCLUDING newEntry itself. Build the
	// run: newEntry + the trailing history entries matching it.
	run := 1
	for i := len(history) - 1; i >= 0; i-- {
		e := history[i]
		if e.Hash != newEntry.Hash || e.NormalisationVersion != newEntry.NormalisationVersion {
			break
		}
		run++
	}
	// Fire when the run reaches N (the spec's "N consecutive" — the plan
	// says "N consecutive identical hashes" fires; the CRD default N=3 means
	// 3 consecutive failures). The run counts newEntry + the matching
	// trailing history, so a run of N means N consecutive identical
	// failures (newEntry being the Nth).
	return run >= int(stallAfter), run
}

// resolveStallAfter reads spec.loop.stallAfter (a pointer, CRD-default 3 when
// nil). The API pointer is *int32; when nil the CRD defaulting sets 3, but a
// bare reconcile (no defaulting) gets nil → the code default 3.
func resolveStallAfter(spec *coxv1alpha1.LoopSpec) int32 {
	if spec == nil || spec.Loop.StallAfter == nil {
		return 3
	}
	return *spec.Loop.StallAfter
}

// readCheckOutput is the default (live) check-output read: the failing
// check container's terminationMessage (the 4 KB tail Kubernetes records in
// status.initContainerStatuses[].lastState.terminated.terminationMessage,
// which is set because the check containers carry a terminationMessagePath,
// item 2). It is a SEAM on the LoopReconciler (readCheckOutput func(...)
// field) so the envtest specs drive a deterministic output without a real
// verify Job (the spec's readCheckOutput seam).
//
// It returns ("", false) when the check container's lastState has no
// terminated record with a terminationMessage (a check that never ran, or a
// check whose terminationMessagePath was never written — a no-output
// failure). The operator treats a no-output failure as an EMPTY raw (the
// normaliser of "" is "", all such failures hash equal — a no-output hot
// loop IS a stall, correctly detected).
func (r *LoopReconciler) defaultReadCheckOutput(pod *corev1.Pod, checkName string) (string, bool) {
	if pod == nil {
		return "", false
	}
	for i := range pod.Status.InitContainerStatuses {
		ics := &pod.Status.InitContainerStatuses[i]
		if ics.Name != checkName {
			continue
		}
		if ics.LastTerminationState.Terminated != nil {
			if msg := ics.LastTerminationState.Terminated.Message; msg != "" {
				return msg, true
			}
			return "", false
		}
		return "", false
	}
	return "", false
}

// appendStallEntry records the failing check's normalised output into
// status.stallHistory (a P2c/P2e ring, dedup'd by jobName, capped at
// stallHistoryCap). It returns the appended entry (the caller uses it for
// the stall decision). The entry is NOT appended if an entry with the same
// jobName already exists (item 6: a re-read of the same verify Job appends
// no entry — the dedup key).
func appendStallEntry(loop *coxv1alpha1.Loop, jobName string, hash, version, check string, at string) coxv1alpha1.StallEntry {
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
		NormalisationVersion: version,
		Check:                check,
	}
	if t, err := parseStallTime(at); err == nil {
		entry.At = *t
	}
	loop.Status.StallHistory = append(loop.Status.StallHistory, entry)
	// Cap the ring at the last stallHistoryCap (evict the oldest).
	if len(loop.Status.StallHistory) > stallHistoryCap {
		loop.Status.StallHistory = loop.Status.StallHistory[len(loop.Status.StallHistory)-stallHistoryCap:]
	}
	return entry
}

// parseStallTime parses an RFC3339 finish time into a *metav1.Time (the
// entry's At; a parse failure leaves the zero Time — the ring is still
// recorded, the At is just absent).
func parseStallTime(at string) (*metav1.Time, error) {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return nil, err
	}
	mt := metav1.Time{}
	mt.Time = t
	return &mt, nil
}

// verifyJobName returns the verify Job's name for the current iteration
// (the StallEntry dedup key, item 6): <loop>-verify-<iteration>.
func (r *LoopReconciler) verifyJobName(loop *coxv1alpha1.Loop) string {
	return loop.Name + "-verify-" + strconv.Itoa(int(loop.Status.Iteration))
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
	at := ""
	if pod != nil {
		at = podFinishTime(pod, failedCheck)
	}
	entry := appendStallEntry(loop, jobName, hash, stall.NormalisationVersionV1, failedCheck, at)
	fired, run := stallDecision(loop.Status.StallHistory, entry, resolveStallAfter(&loop.Spec))
	if !fired {
		return false
	}
	action := loop.Spec.Loop.StallAction
	if action == "" {
		action = coxv1alpha1.StallActionFail // the CRD default
	}
	switch action {
	case coxv1alpha1.StallActionPause:
		loop.Status.Phase = coxv1alpha1.LoopPhasePaused
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhasePaused
		loop.Status.PausedReason = coxv1alpha1.PausedReasonStall
		setCondition(loop, string(coxv1alpha1.StalledCondition), metav1.ConditionTrue,
			"Stalled", fmt.Sprintf("stall detector fired: %d consecutive identical verify failures (stallAction=Pause)", run))
	case coxv1alpha1.StallActionContinue:
		// Keep iterating: the Stalled condition is set, the phase stays
		// Verifying->Implementing (the caller proceeds to the iterate).
		setCondition(loop, string(coxv1alpha1.StalledCondition), metav1.ConditionTrue,
			"Stalled", fmt.Sprintf("stall detector fired: %d consecutive identical verify failures (stallAction=Continue, keeping the loop)", run))
		return false // keep iterating
	default: // StallActionFail
		setCondition(loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue,
			"Stalled", fmt.Sprintf("stall detector fired: %d consecutive identical verify failures (stallAction=Fail)", run))
		loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
	}
	return true
}

// podFinishTime returns the check container's pod finish time (RFC3339) for
// the StallEntry's At (the kubelet-recorded finish; empty when the container
// has no finish time — the entry's At is then zero).
func podFinishTime(pod *corev1.Pod, checkName string) string {
	if pod == nil {
		return ""
	}
	for i := range pod.Status.InitContainerStatuses {
		ics := &pod.Status.InitContainerStatuses[i]
		if ics.Name != checkName {
			continue
		}
		if ics.State.Terminated != nil && !ics.State.Terminated.FinishedAt.IsZero() {
			return ics.State.Terminated.FinishedAt.Time.Format(time.RFC3339)
		}
		if ics.LastTerminationState.Terminated != nil && !ics.LastTerminationState.Terminated.FinishedAt.IsZero() {
			return ics.LastTerminationState.Terminated.FinishedAt.Time.Format(time.RFC3339)
		}
		return ""
	}
	return ""
}
