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

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/alerts"
)

// PolicyBlockedCondition is the non-phase condition type recording the
// operator's count of the actions the enforcement engine BLOCKED for the Loop
// (F62, ADR-0007 Q5/C8).
//
// The three states mean three different things, and none of them is silent:
//
//   - True (reason BlocksObserved): the audit stream was healthy; the message
//     names the count, the latest target and how many alerts the operator could
//     not attribute to any Loop. status.policy.blockedCount carries the number.
//   - Unknown (reason AuditStreamUnavailable / AuditStreamGap /
//     AuditStreamUndecodable): there is NO trustworthy number — the stream never
//     attached, the engine is absent, a rotation window cannot be ruled out, or
//     the lines stopped parsing. status.policy.blockedCount is cleared (nil),
//     never 0: a relay outage must not read as "nothing was blocked" (I32).
//   - Absent: the relay has no news for this Loop yet (no sandbox pod, no
//     stream). That is "none yet", not "zero" and not "unknown".
//
// It is a SURFACE, not a gate input (Q5): blocks never fail or pause the Loop.
const PolicyBlockedCondition = "PolicyBlocked"

// policyBlocksObservedReason is the PolicyBlocked=True reason. The Unknown
// reasons are the relay's own (alerts.Reason*), so the condition and the log
// name the same cause.
const policyBlocksObservedReason = "BlocksObserved"

// AuditReader is the alert relay's read seam (F62): the reconcile reads the
// relay's in-memory view of one Loop. The relay never writes status and the
// reconcile never writes the relay's state — the holder is the whole seam.
// A nil seam means the relay is not installed (a bare test reconciler, or a
// deliberately silent install): NOTHING is written, so blockedCount stays nil
// (never 0).
type AuditReader interface {
	Snapshot(uid types.UID) (alerts.Snapshot, bool)
}

// recordBlockedActions writes status.policy.blockedCount and the PolicyBlocked
// condition from the relay's snapshot. It reports the change through *changed so
// Reconcile gains no branch of its own (it is at its gocyclo budget: an `if` or
// a new `||` there fails `make lint`).
func (r *LoopReconciler) recordBlockedActions(loop *coxv1alpha1.Loop, changed *bool) {
	if r.Audit == nil {
		return
	}
	snap, ok := r.Audit.Snapshot(loop.UID)
	if !ok {
		// No news for this Loop: write nothing. blockedCount stays nil and the
		// condition stays absent — "none yet" is not a number (I32).
		return
	}
	var before metav1.Condition
	hadCond := false
	if c := loopCondition(loop, PolicyBlockedCondition); c != nil {
		before = *c
		hadCond = true
	}
	var countBefore int64
	hadCount := false
	if loop.Status.Policy != nil && loop.Status.Policy.BlockedCount != nil {
		countBefore = *loop.Status.Policy.BlockedCount
		hadCount = true
	}

	switch snap.Health {
	case alerts.HealthHealthy:
		if loop.Status.Policy == nil {
			loop.Status.Policy = &coxv1alpha1.PolicyStatus{}
		}
		count := snap.Count
		loop.Status.Policy.BlockedCount = &count
		setCondition(loop, PolicyBlockedCondition, metav1.ConditionTrue, policyBlocksObservedReason,
			blockedActionsMessage(snap))
	case alerts.HealthGap, alerts.HealthUnavailable:
		if loop.Status.Policy != nil {
			// Unknown: the count is CLEARED, never left at the last number (a
			// stale number reads as a current fact) and never 0 (I32).
			loop.Status.Policy.BlockedCount = nil
		}
		reason := snap.Reason
		if reason == "" {
			reason = alerts.ReasonStreamUnavailable
		}
		setCondition(loop, PolicyBlockedCondition, metav1.ConditionUnknown, reason,
			blockedUnknownMessage(snap, reason))
	default:
		// HealthUnknown with a snapshot (the relay has seen the Loop's pod but
		// not decided): do not guess a number.
		return
	}

	conditionChanged := hadCond != (loopCondition(loop, PolicyBlockedCondition) != nil)
	if !conditionChanged {
		if after := loopCondition(loop, PolicyBlockedCondition); after != nil {
			conditionChanged = !equality.Semantic.DeepEqual(before, *after)
		}
	}
	now := loop.Status.Policy != nil && loop.Status.Policy.BlockedCount != nil
	countChanged := now != hadCount
	if now && hadCount {
		countChanged = *loop.Status.Policy.BlockedCount != countBefore
	}
	if conditionChanged || countChanged {
		*changed = true
	}
}

// blockedActionsMessage renders the PolicyBlocked=True message: the number, the
// latest target (what was bounced, not only how often) and the process-wide
// unattributed count when the relay has one.
func blockedActionsMessage(snap alerts.Snapshot) string {
	msg := fmt.Sprintf("the engine blocked %d action(s) for this Loop", snap.Count)
	if snap.LatestTarget != "" && !snap.LatestAt.IsZero() {
		msg += fmt.Sprintf("; latest target %s at %s", snap.LatestTarget, snap.LatestAt.UTC().Format(time.RFC3339))
	}
	if snap.Unattributed > 0 {
		msg += fmt.Sprintf("; %d audit alert(s) in this operator could not be attributed to any Loop", snap.Unattributed)
	}
	return msg
}

// blockedUnknownMessage renders the PolicyBlocked=Unknown message: why the count
// cannot be trusted, and what the operator does about it (nothing — Q5: a block
// never fails or pauses the Loop).
func blockedUnknownMessage(snap alerts.Snapshot, reason string) string {
	msg := "the engine's blocked-action count is UNKNOWN, not zero: "
	switch reason {
	case alerts.ReasonStreamGap:
		msg += "the audit stream re-attached and cannot prove it saw every alert in between (a rotation window)"
	case alerts.ReasonUndecodable:
		msg += "the audit stream stopped producing lines this operator can decode"
	default:
		msg += "the audit stream is unavailable (the engine or its pod log cannot be read)"
	}
	if snap.Count > 0 {
		msg += fmt.Sprintf("; the last number seen was %d", snap.Count)
	}
	if snap.Unattributed > 0 {
		msg += fmt.Sprintf("; %d audit alert(s) in this operator could not be attributed to any Loop", snap.Unattributed)
	}
	msg += ". No action is taken: a blocked action never fails or pauses the Loop"
	return msg
}

// loopCondition returns the named condition of the Loop (nil when absent).
func loopCondition(loop *coxv1alpha1.Loop, condType string) *metav1.Condition {
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == condType {
			return &loop.Status.Conditions[i]
		}
	}
	return nil
}
