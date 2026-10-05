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
	"fmt"
	"math/big"
	"net/http"
	"time"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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
// (including Paused — the wall clock keeps recording, the decision does not
// re-fire). It returns the wall-clock RequeueAfter (0 when the cap is unset
// or already hit) and an error only for transient budget-status persistence
// failures (a read failure is NOT an error — status.budget is left unchanged
// and the decision waits for the next successful read).
func (r *LoopReconciler) applyBudget(ctx context.Context, loop *coxv1alpha1.Loop) time.Duration {
	now := r.operatorNow()

	// --- wall clock (item 10 / item E) ---
	// activeSeconds accumulates ACTIVE time: each reconcile adds
	// now - lastActiveStamp when the Loop is not Paused (the pause stops the
	// clock), and lastActiveStamp is set to now on every non-paused reconcile
	// (reset on resume, P2f). A fresh accumulation (no stamp yet) adds 0.
	var budgetRequeue time.Duration
	if loop.Spec.Budget != nil && loop.Spec.Budget.MaxWallClock != "" {
		var added int64
		var stamp *metav1.Time
		if !loopPaused(loop) {
			b := loop.Status.Budget
			if b != nil && b.LastActiveStamp != nil {
				added = max(0, int64(now.Sub(b.LastActiveStamp.Time)/time.Second))
			}
			// Set/refresh the stamp on every non-paused reconcile (item E).
			st := now
			stamp = &st
		}
		var current int64
		if loop.Status.Budget != nil {
			current = loop.Status.Budget.ActiveSeconds
		}
		current += added
		r.ensureBudgetStatus(loop, current, stamp)
		// The RequeueAfter for a quiet wall-clock Loop (item 10): when the cap
		// is set and not yet hit, re-reconcile at the remaining time.
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
		if reading, err := r.resolveProxyUsageRead(ctx, loop); err == nil && reading != nil {
			r.applyUsageReading(ctx, loop, *reading)
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
	if !loopPaused(loop) && isPausablePhase(loop.Status.Phase) {
		r.applyBudgetDecision(ctx, loop)
	}

	return budgetRequeue
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
		if errors.IsNotFound(err) {
			// No proxy pod (absent, or not yet created): not a reading.
			return proxy.Reading{}, fmt.Errorf("proxy pod %s/%s absent: %w", loop.Namespace, proxyPodName(loop.Name), err)
		}
		return proxy.Reading{}, fmt.Errorf("read proxy pod %s/%s: %w", loop.Namespace, proxyPodName(loop.Name), err)
	}
	if pod.Status.PodIP == "" {
		// The pod has not received an IP (not Ready / still scheduling): not a
		// reading.
		return proxy.Reading{}, fmt.Errorf("proxy pod %s/%s has no pod IP (not Ready)", loop.Namespace, pod.Name)
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
//  3. A different bootID (a pod recreate, the emptyDir wiped): reset last* to
//     the reading's values, record bootIDChanged=true (sticky) + a Warning
//     Event MeteringReset (a fresh boot, a delta from 0). The loss is
//     visible, not silent.
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
	b.PromptTokens += max(0, reading.PromptTokens-b.LastPromptTokens)
	b.CompletionTokens += max(0, reading.CompletionTokens-b.LastCompletionTokens)
	b.Requests += max(0, reading.Requests-b.LastRequests)
	b.UnmeteredRequests += max(0, reading.UnmeteredRequests-b.LastUnmeteredRequests)
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
