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

// P2d (TDD-PLAN-PHASE2): the operator's budget decision. Each reconcile the
// operator reads the proxy's cumulative usage reading from the /coxswain/usage
// endpoint (the readProxyUsage seam — the readBaseCommit pattern: a func field
// with a non-cached-client default; no new RBAC), folds the reading into
// status.budget via the boot-ID delta rules (item 2 / P1-B), accumulates the
// active wall clock (item 10 / item E), and applies the spec.budget caps —
// onExceeded Pause (the P2f Paused phase, pausedReason Budget) or Fail
// (Failed, reason BudgetExceeded).
//
// The decision is inert in Paused (P2f: re-evaluated on resume) and loses the
// stall/budget precedence (item 8: the stall decision is evaluated first on
// the verify evidence).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"time"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/papawattu/coxswain/internal/proxy"
)

// modelPricesConfigMapName is the cluster-wide model prices ConfigMap (P2c
// item 11, the chosen shape): flat keys "prompt" / "completion", each a USD
// price per million tokens as a decimal string, in the operator's namespace.
const modelPricesConfigMapName = "coxswain-model-prices"

// Price ConfigMap keys (P2c item 11: flat keys in the operator namespace).
const (
	modelPricesPromptKey     = "prompt"
	modelPricesCompletionKey = "completion"
)

// Event reasons (P2d). Stable, documented strings (the OS5 pattern).
const (
	// meteringResetReason is the Warning Event reason when a proxy pod recreate
	// produced a new bootID (the counters were wiped; the loss is visible, not
	// silent).
	meteringResetReason = "MeteringReset"
	// meteringAnomalyReason is the Warning Event reason when a cumulative
	// counter dropped WITHOUT a bootID change (a corrupted/partial file or a
	// torn read — the operator rebases and adds nothing).
	meteringAnomalyReason = "MeteringAnomaly"
	// meteringDegradedReason is the Warning Event reason (and the condition
	// reason) when the proxy forwarded requests it could not meter (F44).
	meteringDegradedReason = "UnmeteredRequests"
	// budgetReadingStaleReason is the Warning Event reason (and the condition
	// reason) when the usage read has failed budgetReadFailureThreshold times
	// in a row (F44).
	budgetReadingStaleReason = "UsageReadFailed"
	// budgetReadFailureThreshold is the consecutive failed reads before
	// BudgetReadingStale is set (F44): a single miss (a pod mid-restart) is
	// not worth a condition.
	budgetReadFailureThreshold = 3
	// proxyUsageStartGrace is how long a proxy pod may exist without a usable
	// usage endpoint before a read failure is counted (F84): the pod is
	// created in the same reconcile that then reads it, and a restart replaces
	// it, so the first seconds are "not there yet", not a failure.
	proxyUsageStartGrace = 60 * time.Second
	// meteringEventQuiet is how long MeteringDegraded must have been False
	// before a re-degrade emits a second Warning Event (F44).
	meteringEventQuiet = 5 * time.Minute
	// budgetExceededEventReason is the Normal Event reason when a budget cap
	// fires (onExceeded=Fail).
	budgetExceededEventReason = "BudgetExceeded"
	// budgetFailedReason is the Failed-condition reason recorded when
	// onExceeded=Fail (distinct from the BudgetExceeded condition).
	budgetFailedReason = "BudgetExceeded"
)

// modelPrices is the resolved prompt/completion price pair (USD per million
// tokens) for one Loop's cost derivation. absent (nil prompt/completion) means
// no price source was found (the cost cap is inert — fail-closed, P2d).
type modelPrices struct {
	prompt     *big.Float
	completion *big.Float
}

// hasAny returns whether both prices are present (a complete price pair).
func (p *modelPrices) hasAny() bool {
	return p != nil && p.prompt != nil && p.completion != nil
}

// applyBudget is the P2d reconcile step: read the usage, accumulate the wall
// clock, update status.budget, and apply the caps. It runs on every reconcile
// (including Paused — the decision does not re-fire). The wall clock stops in
// Paused (item E) AND in terminal phases (Succeeded/Failed — a finished Loop's
// active time is fixed; re-reconciles of a terminal Loop must not keep
// accumulating and rewriting status). It returns the wall-clock RequeueAfter
// (0 when the cap is unset or already hit) and an error only for transient
// budget-status persistence failures (a read failure is NOT an error —
// status.budget is left unchanged and the decision waits for the next
// successful read).
func (r *LoopReconciler) applyBudget(ctx context.Context, loop *coxv1alpha1.Loop) time.Duration {
	now := r.operatorNow()

	// --- wall clock (item 10 / item E) ---
	// activeSeconds accumulates ACTIVE time: each reconcile adds
	// now - lastActiveStamp when the Loop is not Paused and not terminal
	// (the pause and the terminal phases stop the clock), and
	// lastActiveStamp is set to now on every such reconcile (reset on resume,
	// P2f). A fresh accumulation (no stamp yet) adds 0.
	var budgetRequeue time.Duration
	// The wall clock is accumulated on every non-paused, non-terminal
	// reconcile. The cap only decides enforcement (onExceeded), not recording
	// (I95): active time is useful without a cap — a Loop with no spec.budget
	// still carries its wall clock in status.budget.activeSeconds, and the
	// deliver description body prints it. A Paused Loop does not accumulate
	// (item E: the pause stops the clock), and neither does a terminal Loop
	// (Succeeded/Failed): a finished Loop's active time is fixed — every later
	// reconcile would add now - lastActiveStamp and rewrite the stamp, so
	// terminal Loops re-reconciled on any trigger (I96) would keep growing
	// activeSeconds without bound. Stopping the clock in terminal phases is
	// the same treatment as Paused.
	var added int64
	var stamp *metav1.Time
	// P5f: a Loop held for delivery (Verifying+Delivering) already passed
	// verify; its clock is stopped like a terminal Loop's, and no budget
	// decision may fail it (it would lose the delivery result).
	if !loopPaused(loop) && !isTerminalPhase(loop.Status.Phase) && !loopHoldingForDelivery(loop) {
		b := loop.Status.Budget
		if b != nil && b.LastActiveStamp != nil {
			added = max(0, int64(now.Sub(b.LastActiveStamp.Time).Seconds()))
		}
		// Set/refresh the stamp on every non-paused, non-terminal reconcile.
		// The stamp is set regardless of whether a cap is set (I95: the wall
		// clock is always accumulated; the cap only decides enforcement).
		// A Paused Loop does not set the stamp (item E: the pause stops the
		// clock). A terminal Loop does not set the stamp either (the clock is
		// stopped the same way: a finished Loop's active time is fixed).
		st := now
		stamp = &st
	}
	var current int64
	if loop.Status.Budget != nil {
		current = loop.Status.Budget.ActiveSeconds
	}
	current += added
	r.ensureBudgetStatus(loop, current, stamp)
	// The RequeueAfter for a quiet wall-clock Loop (item 10): only when the
	// cap is set and not yet hit, re-reconcile at the remaining time.
	if loop.Spec.Budget != nil && loop.Spec.Budget.MaxWallClock != "" {
		if d, err := coxv1alpha1.ParseMaxWallClock(loop.Spec.Budget.MaxWallClock); err == nil && d > 0 {
			remaining := d - time.Duration(current)*time.Second
			if remaining > 0 {
				budgetRequeue = remaining
			}
		}
	}

	// --- the read + delta (item 2, P1-B) ---
	// No model -> no proxy pod -> no read; the token/cost caps are inert
	// (the wall clock still applies, spec 9). A read failure (the proxy pod
	// not Ready, the dial refused, the pod absent) leaves status.budget
	// UNCHANGED (no reset, no delta — the operator does not guess; the
	// decision is on the last SUCCESSFUL read, never an estimate).
	if loop.Spec.Agent.EndpointSecretRef != "" {
		reading, err := r.resolveProxyUsageRead(ctx, loop)
		if err == nil && reading != nil {
			r.noteUsageReadOK(loop, now)
			r.applyUsageReading(ctx, loop, *reading)
		} else if err != nil && !errors.Is(err, proxy.ErrNotReady) {
			// F44: the caps stay on the last good reading (fail-open, by
			// design until the owner decides otherwise), but the staleness
			// is recorded and surfaced.
			r.noteUsageReadFailed(loop, err)
		}
		// F84: proxy.ErrNotReady (no pod, no IP, or still inside its start
		// grace) is NO READING YET, not a failure: it neither increments the
		// counter nor resets it, so a pod that flaps absent/up cannot erase a
		// genuine staleness either.
	} else {
		// F84 (P3-6): no model is configured, so the two read-related
		// conditions have no subject: clear them rather than leave them True.
		if conditionTruePresent(loop, coxv1alpha1.BudgetReadingStaleCondition) {
			setCondition(loop, coxv1alpha1.BudgetReadingStaleCondition, metav1.ConditionFalse,
				"NoEndpoint", "no model endpoint is configured, so there is no usage endpoint to read")
		}
		if conditionTruePresent(loop, coxv1alpha1.MeteringDegradedCondition) {
			setCondition(loop, coxv1alpha1.MeteringDegradedCondition, metav1.ConditionFalse,
				"NoEndpoint", "no model endpoint is configured, so there is no metering to degrade")
		}
	}

	// --- the decision (pure function of status.budget + spec.budget) ---
	// Gated on phase != Paused (P2f: the budget decision is inert in Paused —
	// a wall-clock hit while paused records exceeded but does not change the
	// phase/reason; re-evaluated on resume) and on every terminal phase
	// (item 8: a budget entry on a terminal phase would overwrite the
	// terminal record — a Succeeded Loop must not flip to Paused/Failed on a
	// cap hit, and a Failed Loop's stall record must not be clobbered by a
	// budget Fail; the reading is still folded into status.budget above, so
	// the BudgetExceeded CONDITION may still be recorded, but the onExceeded
	// phase action does not fire). The decision runs AFTER the verify/stall
	// decisions (item 8: a phase already changed by the stall decision is seen
	// here and the budget decision is inert).
	if !loopPaused(loop) && isPausablePhase(loop.Status.Phase) && !loopHoldingForDelivery(loop) {
		r.applyBudgetDecision(ctx, loop)
	}

	return budgetRequeue
}

// noteUsageReadOK records a successful usage read (F44): stamps
// lastUsageReadAt, resets the failure counter and clears BudgetReadingStale.
func (r *LoopReconciler) noteUsageReadOK(loop *coxv1alpha1.Loop, now metav1.Time) {
	if loop.Status.Budget == nil {
		loop.Status.Budget = &coxv1alpha1.BudgetStatus{}
	}
	b := loop.Status.Budget
	b.LastUsageReadAt = &now
	b.ConsecutiveReadFailures = 0
	if conditionTruePresent(loop, coxv1alpha1.BudgetReadingStaleCondition) {
		setCondition(loop, coxv1alpha1.BudgetReadingStaleCondition, metav1.ConditionFalse,
			"UsageReadRecovered", "the model proxy usage endpoint was read successfully")
	}
}

// noteUsageReadFailed records a failed usage read (F44). status.budget is
// otherwise untouched (no delta, no reset). At budgetReadFailureThreshold
// consecutive failures BudgetReadingStale is set True with ONE Warning Event
// (transition-gated: later failures do not re-emit).
func (r *LoopReconciler) noteUsageReadFailed(loop *coxv1alpha1.Loop, err error) {
	if loop.Status.Budget == nil {
		loop.Status.Budget = &coxv1alpha1.BudgetStatus{}
	}
	b := loop.Status.Budget
	if b.ConsecutiveReadFailures < math.MaxInt32 {
		b.ConsecutiveReadFailures++
	}
	if b.ConsecutiveReadFailures < budgetReadFailureThreshold {
		return
	}
	wasStale := conditionTruePresent(loop, coxv1alpha1.BudgetReadingStaleCondition)
	// F84: the message must not claim a "last successful reading" the Loop has
	// never had (the caps are inert, which is a different fact).
	effect := "the token and cost caps are evaluating the last successful reading"
	if b.LastUsageReadAt == nil {
		effect = "no reading has ever succeeded: the token and cost caps are inert ($0 spent is not evidence)"
	}
	setCondition(loop, coxv1alpha1.BudgetReadingStaleCondition, metav1.ConditionTrue,
		budgetReadingStaleReason,
		"the model proxy usage endpoint could not be read "+strconv.Itoa(int(b.ConsecutiveReadFailures))+" times in a row; "+effect+": "+err.Error())
	if !wasStale && r.Recorder != nil {
		r.Recorder.Eventf(loop, corev1.EventTypeWarning, budgetReadingStaleReason,
			"usage read failed %d times in a row (%v); %s", b.ConsecutiveReadFailures, err, effect)
	}
}

// noteUnmetered surfaces requests the proxy forwarded without a parseable
// usage object (F44). delta > 0 sets MeteringDegraded True with ONE Warning
// Event on the transition; delta == 0 clears a True condition.
func (r *LoopReconciler) noteUnmetered(loop *coxv1alpha1.Loop, delta int64) {
	b := loop.Status.Budget
	if delta <= 0 {
		if conditionTruePresent(loop, coxv1alpha1.MeteringDegradedCondition) {
			setCondition(loop, coxv1alpha1.MeteringDegradedCondition, metav1.ConditionFalse,
				"MeteringOK", "no new unmetered requests since the last read")
		}
		return
	}
	loopUnmeteredRequestsTotal.Add(float64(delta))
	// One Event per degradation episode: not while already True, and not for
	// a re-degrade within meteringEventQuiet of the clear (a stream that
	// alternates metered and unmetered reads would otherwise flap an Event
	// per reconcile).
	wasDegraded := conditionTruePresent(loop, coxv1alpha1.MeteringDegradedCondition)
	if c := apimeta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.MeteringDegradedCondition); c != nil &&
		c.Status == metav1.ConditionFalse && r.operatorNow().Sub(c.LastTransitionTime.Time) < meteringEventQuiet {
		wasDegraded = true
	}
	msg := fmt.Sprintf("the model proxy forwarded requests without a usage object: %d unmetered in total; they do not count toward the token or cost caps", b.UnmeteredRequests)
	setCondition(loop, coxv1alpha1.MeteringDegradedCondition, metav1.ConditionTrue, meteringDegradedReason, msg)
	if !wasDegraded && r.Recorder != nil {
		r.Recorder.Eventf(loop, corev1.EventTypeWarning, meteringDegradedReason,
			"%d request(s) forwarded without a usage object (%d in total): not counted toward the token or cost caps",
			delta, b.UnmeteredRequests)
	}
}

// resolveProxyUsageRead dispatches to the readProxyUsage test seam when it is
// set, otherwise to the default (the readBaseCommit pattern). It returns
// (nil, err) when no reading is available this reconcile — the caller leaves
// status.budget unchanged (a failed read is never "not exceeded").
func (r *LoopReconciler) resolveProxyUsageRead(ctx context.Context, loop *coxv1alpha1.Loop) (*proxy.Reading, error) {
	if r.readProxyUsage != nil {
		reading, err := r.readProxyUsage(ctx, loop)
		if err != nil {
			return nil, err
		}
		return &reading, nil
	}
	reading, err := r.readProxyUsageDefault(ctx, loop)
	if err != nil {
		return nil, err
	}
	return &reading, nil
}

// ensureBudgetStatus creates status.budget when absent and applies the
// wall-clock fields. Returns a persistence error only for an explicit
// status-write failure (the caller's Status().Update handles the actual write;
// this only mutates the in-memory object — so this method never returns an
// error except for programmer errors, kept for signature symmetry).
func (r *LoopReconciler) ensureBudgetStatus(loop *coxv1alpha1.Loop, activeSeconds int64, stamp *metav1.Time) {
	if loop.Status.Budget == nil {
		if stamp == nil {
			// Nothing to record: no existing budget, no stamp. Leave
			// status.budget absent (the P2c invariant: a Paused Loop with
			// no prior budget status stays absent).
			return
		}
		loop.Status.Budget = &coxv1alpha1.BudgetStatus{}
	}
	loop.Status.Budget.ActiveSeconds = activeSeconds
	if stamp != nil {
		loop.Status.Budget.LastActiveStamp = stamp
	}
}

// readProxyUsageDefault is the DEFAULT readProxyUsage (the readBaseCommit
// pattern): it resolves the proxy pod's IP from the pod status the operator
// already reads (the non-cached apiReader path when wired, the cached client
// otherwise — no new RBAC: the operator already has pod get) and performs a
// plain HTTP GET to http://<pod-ip>:9090/coxswain/usage.
func (r *LoopReconciler) readProxyUsageDefault(ctx context.Context, loop *coxv1alpha1.Loop) (proxy.Reading, error) {
	reader := r.apiReader
	if reader == nil {
		reader = r
	}
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: proxyPodName(loop.Name)}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			// No proxy pod (absent, or not yet created): not a reading, and
			// not yet a FAILURE (F84): the pod is created in the same
			// reconcile that reads it.
			return proxy.Reading{}, fmt.Errorf("proxy pod %s/%s absent: %w", loop.Namespace, proxyPodName(loop.Name), proxy.ErrNotReady)
		}
		return proxy.Reading{}, fmt.Errorf("read proxy pod %s/%s: %w", loop.Namespace, proxyPodName(loop.Name), err)
	}
	if pod.Status.PodIP == "" {
		// The pod has not received an IP (not Ready / still scheduling): not a
		// reading.
		return proxy.Reading{}, fmt.Errorf("proxy pod %s/%s has no pod IP (not Ready): %w", loop.Namespace, pod.Name, proxy.ErrNotReady)
	}
	if started := pod.CreationTimestamp.Time; !started.IsZero() && r.operatorNow().Sub(started) < proxyUsageStartGrace {
		// The pod exists but is inside its start grace: the first seconds are
		// "not there yet" (the pod is created or replaced by this very
		// reconcile), so nothing here is a read failure (F84).
		return proxy.Reading{}, fmt.Errorf("proxy pod %s/%s is still starting (age %s): %w",
			loop.Namespace, pod.Name, r.operatorNow().Sub(started).Round(time.Second), proxy.ErrNotReady)
	}
	url := fmt.Sprintf("http://%s:%d/coxswain/usage", pod.Status.PodIP, proxyUsagePort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return proxy.Reading{}, fmt.Errorf("build usage request: %w", err)
	}
	hc := r.usageHTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Dial refused / timeout / DNS failure: not a reading (the operator
		// does not guess; the decision waits for the next successful read).
		return proxy.Reading{}, fmt.Errorf("usage read %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return proxy.Reading{}, fmt.Errorf("usage read %s: status %d", url, resp.StatusCode)
	}
	var reading proxy.Reading
	if err := json.NewDecoder(resp.Body).Decode(&reading); err != nil {
		return proxy.Reading{}, fmt.Errorf("usage read %s: decode: %w", url, err)
	}
	return reading, nil
}

// applyUsageReading folds one proxy usage reading into status.budget via the
// boot-ID delta rules (item 2, P1-B). It mutates the in-memory status (the
// caller's trailing Status().Update persists it).
//
// Four rules:
//
//  1. Same bootID (the normal + container-restart case): accumulated +=
//     reading.cumulative - lastCumulative (floored at 0). A container restart
//     keeps the same bootID (the file persists it, P2b) — an ordinary
//     same-boot delta: no rebase, no double-count (the P1-B fix).
//  2. The first read (empty lastBootID): adopt the reading as the baseline —
//     set last* to the reading's values, add NOTHING (the pre-reading count is
//     unknown, not zero), and record NO MeteringReset warning (an adoption is
//     not an anomaly).
//  3. A different bootID (a pod recreate, the emptyDir wiped): ADD the
//     reading's counters in full to the accumulated totals (a fresh boot is
//     a delta from 0), then set last* to the reading's values, record
//     bootIDChanged=true (sticky) + a Warning Event MeteringReset. The loss
//     is visible, not silent.
//  4. A cumulative counter drops WITHOUT a bootID change (same bootID, a lower
//     cumulative value — a corrupted/partial file or a torn read): emit a
//     Warning Event MeteringAnomaly, rebase last* to the reading's values, and
//     add NOTHING (no negative tokens; the drop is not re-added on the next
//     read). This is NOT a bootIDChanged rebase (that is reserved for a
//     genuine new boot).
func (r *LoopReconciler) applyUsageReading(ctx context.Context, loop *coxv1alpha1.Loop, reading proxy.Reading) {
	if loop.Status.Budget == nil {
		loop.Status.Budget = &coxv1alpha1.BudgetStatus{}
	}
	b := loop.Status.Budget

	// Rule 2: the first read adopts the baseline (no delta, no warning).
	if b.LastBootID == "" {
		b.LastBootID = reading.BootID
		b.LastPromptTokens = reading.PromptTokens
		b.LastCompletionTokens = reading.CompletionTokens
		b.LastRequests = reading.Requests
		b.LastUnmeteredRequests = reading.UnmeteredRequests
		return
	}

	// Rule 3: a different bootID — a pod recreate (the emptyDir wiped).
	if b.LastBootID != reading.BootID {
		prior := b.LastBootID
		b.BootIDChanged = true // sticky
		// A fresh boot's counters started at 0: the reading IS the delta from
		// 0, so add it in full before rebasing last*.
		b.PromptTokens += reading.PromptTokens
		b.CompletionTokens += reading.CompletionTokens
		b.Requests += reading.Requests
		b.UnmeteredRequests += reading.UnmeteredRequests
		r.noteUnmetered(loop, reading.UnmeteredRequests)
		b.LastBootID = reading.BootID
		b.LastPromptTokens = reading.PromptTokens
		b.LastCompletionTokens = reading.CompletionTokens
		b.LastRequests = reading.Requests
		b.LastUnmeteredRequests = reading.UnmeteredRequests
		if r.Recorder != nil {
			r.Recorder.Eventf(loop, corev1.EventTypeWarning, meteringResetReason,
				"proxy boot ID changed %s -> %s: the pod's usage counters were wiped (a fresh boot); the accumulated count is a delta from 0",
				prior, reading.BootID)
		}
		return
	}

	// Rules 1 + 4: same bootID.
	// Rule 4 first: a counter that DROPS (same bootID, a lower cumulative
	// value) is an anomaly — rebase last* to the reading, add NOTHING.
	dropped := false
	for _, d := range []struct{ cur, last int64 }{
		{reading.PromptTokens, b.LastPromptTokens},
		{reading.CompletionTokens, b.LastCompletionTokens},
		{reading.Requests, b.LastRequests},
		{reading.UnmeteredRequests, b.LastUnmeteredRequests},
	} {
		if d.cur < d.last {
			dropped = true
			break
		}
	}
	if dropped {
		if r.Recorder != nil {
			r.Recorder.Eventf(loop, corev1.EventTypeWarning, meteringAnomalyReason,
				"proxy usage counter dropped without a boot ID change (boot %s): rebasing to the reading; the drop is not counted", reading.BootID)
		}
		// Rebase last* to the reading (rule 4); add nothing.
		b.LastPromptTokens = reading.PromptTokens
		b.LastCompletionTokens = reading.CompletionTokens
		b.LastRequests = reading.Requests
		b.LastUnmeteredRequests = reading.UnmeteredRequests
		return
	}

	// Rule 1: same bootID, no drop — add the deltas (floored at 0 per counter).
	promptDelta := max(0, reading.PromptTokens-b.LastPromptTokens)
	completionDelta := max(0, reading.CompletionTokens-b.LastCompletionTokens)
	b.PromptTokens += promptDelta
	b.CompletionTokens += completionDelta
	b.Requests += max(0, reading.Requests-b.LastRequests)
	unmeteredDelta := max(0, reading.UnmeteredRequests-b.LastUnmeteredRequests)
	b.UnmeteredRequests += unmeteredDelta
	r.noteUnmetered(loop, unmeteredDelta)
	// B5a: bump the token metric by the same deltas (the metric is the
	// process-wide, per-process sum of what the operator has folded into
	// status.budget; it is NOT per-Loop — the per-Loop view is status.budget).
	if promptDelta > 0 {
		loopTokensTotal.WithLabelValues("prompt").Add(float64(promptDelta))
	}
	if completionDelta > 0 {
		loopTokensTotal.WithLabelValues("completion").Add(float64(completionDelta))
	}
	b.LastPromptTokens = reading.PromptTokens
	b.LastCompletionTokens = reading.CompletionTokens
	b.LastRequests = reading.Requests
	b.LastUnmeteredRequests = reading.UnmeteredRequests

	// Cost (derived, P2a): promptTokens * promptPrice/1e6 +
	// completionTokens * completionPrice/1e6, from spec.budget.modelPrices or
	// the coxswain-model-prices ConfigMap. A missing/unreadable source with no
	// modelPrices leaves costUsd empty (the cost cap is inert — fail-closed:
	// an unpriceable cost is not a $0 cost).
	if p := r.modelPricesForLoop(ctx, loop); p.hasAny() {
		b.CostUsd = deriveCost(p, b.PromptTokens, b.CompletionTokens)
	} else {
		b.CostUsd = ""
	}
}

// modelPricesForLoop resolves the price pair for a Loop's cost derivation:
// spec.budget.modelPrices (the per-Loop override) first, then the cluster-wide
// coxswain-model-prices ConfigMap (operator namespace, flat keys
// "prompt"/"completion"), then absent (the cost cap is inert).
func (r *LoopReconciler) modelPricesForLoop(ctx context.Context, loop *coxv1alpha1.Loop) *modelPrices {
	if spec := loop.Spec.Budget; spec != nil && spec.ModelPrices != nil {
		p, c, ok := parsePricePair(spec.ModelPrices.PromptUsdPerMtok, spec.ModelPrices.CompletionUsdPerMtok)
		if ok {
			return &modelPrices{prompt: p, completion: c}
		}
		// An override that is present but unparseable/incomplete: fall through
		// to the ConfigMap (it is the cluster-wide default the Loop did not
		// fully override).
	}
	ns := r.OperatorNamespace
	if ns == "" {
		ns = "coxswain-system"
	}
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: modelPricesConfigMapName}, cm); err != nil {
		// Missing/unreadable: absent (fail-closed — no $0).
		return &modelPrices{}
	}
	p, c, ok := parsePricePair(DecimalStringFrom(cm.Data[modelPricesPromptKey]), DecimalStringFrom(cm.Data[modelPricesCompletionKey]))
	if !ok {
		return &modelPrices{}
	}
	return &modelPrices{prompt: p, completion: c}
}

// DecimalStringFrom converts a raw ConfigMap data value to the API's
// DecimalString (a type alias over string — no conversion work).
func DecimalStringFrom(s string) coxv1alpha1.DecimalString {
	return coxv1alpha1.DecimalString(s)
}

// parsePricePair parses two USD-per-million-token prices. ok=false when either
// is empty or unparseable.
func parsePricePair(prompt, completion coxv1alpha1.DecimalString) (*big.Float, *big.Float, bool) {
	p, err := coxv1alpha1.ParseDecimalString(string(prompt))
	if err != nil || p < 0 {
		return nil, nil, false
	}
	c, err := coxv1alpha1.ParseDecimalString(string(completion))
	if err != nil || c < 0 {
		return nil, nil, false
	}
	if prompt == "" || completion == "" {
		return nil, nil, false
	}
	return big.NewFloat(p), big.NewFloat(c), true
}

// deriveCost computes promptTokens*promptPrice/1e6 +
// completionTokens*completionPrice/1e6 as a decimal-string USD cost (a
// derived, not measured, cost — P2a). It returns "" when a price is absent
// (the cost cap is inert, fail-closed).
func deriveCost(p *modelPrices, promptTokens, completionTokens int64) string {
	if !p.hasAny() {
		return ""
	}
	oneMillion := big.NewFloat(1_000_000)
	cost := new(big.Float)
	cost.Mul(cost, p.prompt) // cost = 0 * price = 0
	// We need promptTokens*price/1e6 + completionTokens*completionPrice/1e6.
	// Recompute cleanly.
	cost.SetInt64(promptTokens)
	cost.Mul(cost, p.prompt)
	cost.Quo(cost, oneMillion)
	comp := new(big.Float)
	comp.SetInt64(completionTokens)
	comp.Mul(comp, p.completion)
	comp.Quo(comp, oneMillion)
	cost.Add(cost, comp)
	return cost.Text('f', 10)
}

// applyBudgetDecision is the P2d decision: evaluate the spec.budget caps
// against status.budget and, when a cap is hit (exceeded false->true), apply
// onExceeded. It is inert in Paused (P2f) and after a stall decision (item 8:
// the phase is already Failed/Paused by then). It returns changed.
func (r *LoopReconciler) applyBudgetDecision(ctx context.Context, loop *coxv1alpha1.Loop) bool {
	spec := loop.Spec.Budget
	if spec == nil {
		return false
	}
	var (
		tokens  int64
		active  int64
		costUsd string
	)
	if b := loop.Status.Budget; b != nil {
		tokens = b.PromptTokens + b.CompletionTokens
		active = b.ActiveSeconds
		costUsd = b.CostUsd
	}
	hit, reason, value := r.capName(spec, tokens, active, costUsd)
	if hit == "" {
		// No cap hit. If the Loop was previously exceeded but the caps no
		// longer are (e.g. the wall clock was the cap and it was raised),
		// leave the sticky flag for the resume re-evaluation (P2f).
		return false
	}
	if loop.Status.Budget != nil && loop.Status.Budget.Exceeded {
		// Already fired (sticky): the decision is inert until the resume
		// re-evaluation (P2f item 5). No re-fire, no duplicate Event.
		return false
	}
	return r.fireBudgetExceeded(ctx, loop, spec, hit, reason, value)
}

// capName returns the name of the first spec.budget cap the current counts
// hit (>= — the boundary rule: a cap hit AT the value fires), and the
// exceeded value (a string for the Event/condition message). "" when no cap
// is hit. costUsd is the derived cost string ("" when unpriceable — the cost
// cap is inert, fail-closed).
func (r *LoopReconciler) capName(spec *coxv1alpha1.BudgetConfig, tokens, activeSeconds int64, costUsd string) (string, coxv1alpha1.BudgetExceededReason, string) {
	if spec.MaxTokens != nil && tokens >= *spec.MaxTokens {
		return "Tokens", coxv1alpha1.BudgetExceededTokens, fmt.Sprintf("%d", tokens)
	}
	if wc := r.parseMaxWallClockSeconds(spec.MaxWallClock); wc > 0 && activeSeconds >= wc {
		return "WallClock", coxv1alpha1.BudgetExceededWallClock, fmt.Sprintf("%ds", activeSeconds)
	}
	if spec.MaxCostUsd != "" && costUsd != "" {
		if v, cerr := coxv1alpha1.ParseDecimalString(string(spec.MaxCostUsd)); cerr == nil {
			if cur, perr := coxv1alpha1.ParseDecimalString(costUsd); perr == nil && cur >= v {
				return "Cost", coxv1alpha1.BudgetExceededCost, costUsd
			}
		}
	}
	return "", "", ""
}

// fireBudgetExceeded sets status.budget.exceeded + the BudgetExceeded
// condition and applies onExceeded (Pause -> the P2f pause entry with
// pausedReason=Budget; Fail -> Failed with reason BudgetExceeded). It returns
// changed.
func (r *LoopReconciler) fireBudgetExceeded(ctx context.Context, loop *coxv1alpha1.Loop, spec *coxv1alpha1.BudgetConfig, capName string, reason coxv1alpha1.BudgetExceededReason, value string) bool {
	_ = ctx
	b := loop.Status.Budget
	b.Exceeded = true
	b.ExceededReason = reason
	setCondition(loop, coxv1alpha1.BudgetExceededCondition, metav1.ConditionTrue, "Exceeded",
		fmt.Sprintf("budget cap %s hit at %s", capName, value))

	switch spec.OnExceeded {
	case coxv1alpha1.BudgetExceededActionFail:
		// P2g (the auditability sweep): the BudgetExceeded Event fires for the
		// Fail path too (the condition is set above; the Event is the audit
		// record of the exceedance itself, the Failed-phase outcome is the
		// onExceeded action).
		if r.Recorder != nil {
			r.Recorder.Eventf(loop, corev1.EventTypeNormal, budgetExceededEventReason,
				"budget cap %s exceeded: %s (onExceeded=Fail)", capName, value)
		}
		// Fail: terminal Failed, reason BudgetExceeded. The Failed-phase
		// cleanup (sandbox suspension via the spec.suspend/S1 path is not
		// automatic; the Loop is terminal and the next reconcile's gates hold
		// the sandbox). A Normal Event.
		from := loop.Status.Phase
		loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
		setCondition(loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue, budgetFailedReason,
			fmt.Sprintf("budget cap %s hit at %s (onExceeded=Fail)", capName, value))
		if r.Recorder != nil {
			r.Recorder.Eventf(loop, corev1.EventTypeNormal, budgetExceededEventReason,
				"budget cap %s hit at %s; the Loop is Failed (onExceeded=Fail, from phase %s)", capName, value, from)
		}
		return true
	case coxv1alpha1.BudgetExceededActionPause, "":
		// Pause (the default): the P2f pause entry with pausedReason=Budget.
		// The phase the Loop left must be captured BEFORE overwriting
		// status.phase.
		if !isPausablePhase(loop.Status.Phase) {
			// A terminal phase is not pausable: the exceedance is recorded (the
			// condition above), the phase is unchanged (the Pause action is a
			// no-op on a terminal phase).
			return true
		}
		enteredFrom := loop.Status.Phase
		loop.Status.Phase = coxv1alpha1.LoopPhasePaused
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhasePaused
		if loop.Status.PausedFrom == "" {
			loop.Status.PausedFrom = enteredFrom
			loop.Status.PausedReason = coxv1alpha1.PausedReasonBudget
		}
		setCondition(loop, coxv1alpha1.PausedCondition, metav1.ConditionTrue, "Paused",
			fmt.Sprintf("paused: budget cap %s hit at %s", capName, value))
		if r.Recorder != nil {
			r.Recorder.Eventf(loop, corev1.EventTypeNormal, "Paused",
				"paused: budget cap %s hit at %s, from phase %s", capName, value, enteredFrom)
		}
		return true
	}
	return false
}
