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

// D116 (owner decision, P6d): the operator's one-shot retry trigger. A retried
// Failed:SandboxFailed Loop resumes from its CURRENT checkpoint and branch
// state, in the phase it failed in, with the sandbox-attempt episode reset
// (clearSandboxInfraEpisode). The retry is UNBOUNDED and AUDITED (D100):
// every consumed retry emits a Normal Event with the retryEventReason reason,
// and the phase move is the PhaseAdvanced Event from the same trailing write
// pass. The annotation is a plain metadata write (the editor role's existing
// patch authorises it, like the D85 approval annotations) and is consumed
// exactly once: it is removed at the end of the pass that reads it, whether or
// not the Loop is eligible (an ineligible retry is a mis-aimed annotation,
// not a live request).

import (
	"context"
	"fmt"
	"strings"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// retryAnnotation is the operator's one-shot retry trigger (D116). The CLI's
// internal/cli/retry.go declares the SAME key ("coxswain.io/retry") in its own
// retryAnnotation constant — the two literals must stay in sync (the CLI sets
// it, the operator consumes it). Any value (including empty) is a request.
const retryAnnotation = "coxswain.io/retry"

// retryEventReason is the Normal Event's reason (the D100 audit).
const retryEventReason = "Retried"

// retryEpisodeSeparator splits the infraEpisode key into iteration and phase.
const retryEpisodeSeparator = "/"

// retryEligible reports whether the Loop is a valid retry target for the
// retryAnnotation (D116): phase and desiredPhase are Failed, the Failed
// condition's reason is SandboxFailed, and status.agent.infraEpisode parses
// as "<iteration>/<phase>" naming a real, non-Failed phase. EVERY other
// reason (TamperedVerify, Stalled, WorkspaceLost, ...) refuses — a retry of a
// verify-integrity failure would be a gate bypass, so the CLI refuses it too
// (internal/cli/retry.go).
func retryEligible(loop *coxv1alpha1.Loop) (bool, string) {
	if loop.Status.Phase != coxv1alpha1.LoopPhaseFailed ||
		loop.Status.DesiredPhase != coxv1alpha1.LoopPhaseFailed {
		return false, ""
	}
	failedReason := ""
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == string(coxv1alpha1.LoopPhaseFailed) {
			failedReason = loop.Status.Conditions[i].Reason
			break
		}
	}
	// The reason is the retry's actual gate: a Failed loop with a DIFFERENT
	// reason (TamperedVerify, Stalled, WorkspaceLost, ...) is a terminal
	// INTEGRITY failure, not a sandbox-infra failure — a retry of it would
	// bypass the verify gate, so the CLI refuses it too (internal/cli/retry.go).
	if failedReason != sandboxInfraFailedCondition {
		return false, ""
	}
	phase, ok := retryPhaseFromEpisode(loop)
	if !ok {
		return false, ""
	}
	return true, phase
}

// retryPhaseFromEpisode parses the infraEpisode key ("<iteration>/<phase>",
// F48g) into the phase to resume. The phase is everything AFTER the first
// separator: the iteration is numeric (strconv.Atoi would also parse it), but
// the phase is what a resume needs. It is accepted ONLY when it names a phase
// a sandbox run can fail in — Planning, Implementing or Verifying (R50 I230:
// any other value, e.g. Paused, would be written to Phase/DesiredPhase
// unchecked). A key with no separator, an empty phase, or any other phase
// name is not a retry target (the D116 guarantee: a retried Loop resumes in
// the phase it failed IN).
func retryPhaseFromEpisode(loop *coxv1alpha1.Loop) (string, bool) {
	if loop.Status.Agent == nil {
		return "", false
	}
	sep := strings.Index(loop.Status.Agent.InfraEpisode, retryEpisodeSeparator)
	if sep < 0 {
		return "", false
	}
	phase := loop.Status.Agent.InfraEpisode[sep+1:]
	switch phase {
	case string(coxv1alpha1.LoopPhasePlanning), string(coxv1alpha1.LoopPhaseImplementing), string(coxv1alpha1.LoopPhaseVerifying):
		return phase, true
	default:
		return "", false
	}
}

// applyRetry consumes the coxswain.io/retry annotation (D116). It returns
// true when the retry was APPLIED (the status change is in place and needs
// the trailing status write) and false otherwise — both when the annotation
// is absent and when the Loop is ineligible (the annotation is still removed
// in the ineligible case, at the same end-of-pass place as the redeliver
// annotation; an ineligible retry is discarded, not re-queued).
//
// On an eligible Loop it: (1) parses the phase P from the infraEpisode key,
// (2) resets the sandbox-attempt episode (clearSandboxInfraEpisode — the new
// run gets its full retry bound again), (3) clears the Failed condition,
// (4) sets phase and desiredPhase to P (the resume — the checkpoint and
// branch state are untouched, D116), (5) emits the D100 audit Event, and
// (6) deletes the failed Sandbox (best-effort: a failed delete is logged, not
// a controller error — the next pass re-issues it through the sandbox ensure
// path).
func (r *LoopReconciler) applyRetry(ctx context.Context, loop *coxv1alpha1.Loop) bool {
	if _, present := loop.Annotations[retryAnnotation]; !present {
		return false
	}
	ok, phase := retryEligible(loop)
	if !ok {
		// Discarded (not Failed:SandboxFailed, or the episode key is not a
		// phase): change nothing, but the annotation is still removed below
		// so a stale request cannot re-fire after the Loop's next transition.
		logf.FromContext(ctx).Info("Ignored retry annotation (not Failed:SandboxFailed)",
			"loop", loop.Name, "phase", loop.Status.Phase)
		return false
	}
	// The resume (D116): the episode reset, the Failed-condition clear, and
	// the phase move. The checkpoint and the branch (status.currentVerify's
	// pinned commit + the workspace PVC) are untouched — the Loop resumes in
	// the phase it failed in.
	clearSandboxInfraEpisode(loop)
	clearCondition(loop, string(coxv1alpha1.LoopPhaseFailed))
	loop.Status.Phase = coxv1alpha1.LoopPhase(phase)
	loop.Status.DesiredPhase = coxv1alpha1.LoopPhase(phase)
	if r.Recorder != nil {
		r.Recorder.Eventf(loop, corev1.EventTypeNormal, retryEventReason,
			"retry requested (coxswain.io/retry): resuming %s from the current checkpoint; sandbox attempts reset", phase)
	}
	// Delete the failed Sandbox so a fresh one is created from the workspace
	// PVC on the next ensureSandbox (the D116 "sandbox-attempt episode reset"
	// half). A failed delete is logged, not a controller error: the attempt
	// count is already reset and persisted by the trailing status write, and
	// the ensure path re-issues the delete (it recreates a stale Sandbox that
	// cannot serve the resumed phase).
	if r.DeleteSandboxForRetry(ctx, loop) {
		logf.FromContext(ctx).Info("Retry: failed to delete the failed sandbox; the ensure path will re-issue the delete (no wedge)",
			"loop", loop.Name)
	}
	return true
}

// removeRetryAnnotation clears the retry annotation. Modelled on
// removeDeliverAnnotation (the I52 pattern): a merge patch from a fresh read,
// run AFTER the trailing status write so the two Loop writes in one reconcile
// never race. The annotation is considered PRESENT when the key exists (even
// with an empty value), and it is removed for eligible AND ineligible Loops
// alike (applyRetry decided; this is pure cleanup). Already-absent is a no-op.
// The retry-on-409 (re-read, re-check presence, re-patch) covers a concurrent
// writer, not the reconcile's own ordered writes. It returns only an error
// (no changed flag: the caller — the end-of-pass cleanup — never branches on
// whether a patch ran).
func (r *LoopReconciler) removeRetryAnnotation(ctx context.Context, loop *coxv1alpha1.Loop) error {
	if _, ok := loop.Annotations[retryAnnotation]; !ok {
		return nil
	}
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		fresh := &coxv1alpha1.Loop{}
		if err := r.Get(ctx, types.NamespacedName{Name: loop.Name, Namespace: loop.Namespace}, fresh); err != nil {
			return err
		}
		if _, ok := fresh.Annotations[retryAnnotation]; !ok {
			return nil // already removed (another reconcile or the test)
		}
		updated := fresh.DeepCopy()
		delete(updated.Annotations, retryAnnotation)
		if err := r.Patch(ctx, updated, client.MergeFrom(fresh)); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("remove %s annotation from %s: %w", retryAnnotation, loop.Name, err)
	}
	return nil
}
