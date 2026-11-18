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

package cni

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// resultHolder is the thread-safe holder for the latest probe result
// (D38 plan, design point 3a). It is mutex-guarded because
// MaxConcurrentReconciles > 1 is coming (R17 D42): the probe Runnable writes
// on result change while many reconciles read concurrently.
type resultHolder struct {
	mu     sync.RWMutex
	result CNIProbeResult
}

// probeHolder is the shared holder. Its initial state is Unknown
// (fail-closed: the gate holds new sandboxes Suspended until the first probe
// result; D142(a) lets an already-running, recorded-True sandbox keep running
// until then; design point 3).
var probeHolder = &resultHolder{result: CNIProbeResult{Reason: ReasonUnknown}}

// firstRegateRetry is the wait between list retries for the first result's
// re-gate (D142(a)). A var so specs can shorten it.
var firstRegateRetry = time.Second

// testMux guards test-only holder resets.
var testMux sync.Mutex

// Holder returns the shared result holder.
func Holder() *resultHolder { return probeHolder }

// ResetForTest resets the holder to its initial Unknown state. TEST-ONLY:
// specs reset it in BeforeEach/DeferCleanup so they are self-contained.
// (The eBPF gate has no equivalent: the fake enforcer is per-spec, but the
// probe result is shared operator state, so tests must own it explicitly.)
func ResetForTest() {
	testMux.Lock()
	defer testMux.Unlock()
	*probeHolder = resultHolder{result: CNIProbeResult{Reason: ReasonUnknown}}
}

// Result returns the latest probe result.
func (h *resultHolder) Result() CNIProbeResult {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.result
}

// Set stores the new result and reports whether it changed.
func (h *resultHolder) Set(new CNIProbeResult) (old CNIProbeResult, changed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	old = h.result
	changed = old.Reason != new.Reason
	if changed {
		h.result = new
	}
	return old, changed
}

// ProbeRunnable is the operator's CNI self-test runner (D38 plan, design
// points 2+3a): a leader-elected, NON-BLOCKING manager Runnable. It probes
// once immediately (in a goroutine, so manager start never waits on an image
// pull or a slow scheduler), then periodically every Interval. On every
// result change it re-gates every Loop: the new result goes into the
// thread-safe holder, a K8s Event is emitted on each Loop, and a reconcile
// request for each Loop is sent on the source.Channel that the Loop
// controller watches.
type ProbeRunnable struct {
	// Prober is the CNIProber seam (the real pod-based probe, or the fake in
	// tests). Must not be nil: a nil Prober means ProbeUnavailable forever.
	Prober CNIProber
	// Client lists Loops and writes the re-gate Events.
	Client client.Client
	// Recorder is the manager's EventRecorder for the re-gate Events (it
	// sets the Event's name via generateName, so the API accepts it; a
	// hand-built corev1.Event with no name is rejected by the API).
	// If nil, no Event is emitted (the channel re-gate still fires).
	Recorder record.EventRecorder
	// Interval is the probe period (default 10m; --cni-check-interval).
	Interval time.Duration
	// Timeout is the per-run probe timeout (default 120s; --cni-probe-timeout).
	// It covers BOTH probe passes (the no-policy control pod and the policy
	// pod — D103), not a single pod.
	Timeout time.Duration
	// Ch is the source channel the Loop controller watches; a nil channel
	// (tests) skips the re-gate send.
	Ch chan<- event.TypedGenericEvent[client.Object]
}

// Start implements manager.Runnable. It returns when ctx is done.
func (p *ProbeRunnable) Start(ctx context.Context) error {
	run := func() {
		if err := p.probeOnce(ctx); err != nil {
			ctrl.Log.WithName("cni-prober").Error(err, "CNI probe failed")
		}
	}
	go run() // first probe: never on the manager start path
	interval := p.Interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			run()
		}
	}
}

// probeOnce runs one probe with the per-run timeout and re-gates on change.
// It never returns an error that means "enforced": a probe that cannot
// complete (timeout, pod not Ready, pull failure, API error) is
// ProbeUnavailable, which the gate treats as not enforced (fail-closed).
func (p *ProbeRunnable) probeOnce(ctx context.Context) error {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	newResult, err := p.Prober.Probe(rctx)
	if err != nil {
		newResult = CNIProbeResult{Reason: ReasonProbeUnavailable, Detail: err.Error()}
	}
	old, changed := Holder().Set(newResult)
	if changed {
		SetNetworkEnforcedMetric(newResult)
		// D142(a): the FIRST result after startup (the holder was Unknown)
		// must reach every Loop: a new leader let running sandboxes keep
		// going until it. It is delivered with the operator's lifetime ctx
		// (not the spent probe ctx), retrying the list and blocking on a
		// full channel. A later change keeps the best-effort delivery: a
		// dropped event leaves the Loop gated by its last result, and the
		// next change re-gates it.
		if old.Reason == ReasonUnknown {
			p.retage(ctx, old, newResult, true)
		} else {
			p.retage(rctx, old, newResult, false)
		}
	}
	return nil
}

// retage emits a re-gate Event on every Loop and sends a GenericEvent for
// each on the channel (design point 3a). The reconcile itself reads the new
// result from the holder — it never runs the probe. A nil Client (unit tests)
// skips the Event and the channel send: the re-gate is a no-op and the result
// is still stored in the holder (the metric is updated by the caller).
//
// first is true for the first result after startup (D142(a), I273): the list
// is retried until it succeeds and each send blocks until the controller takes
// it, both bounded only by ctx (the operator's lifetime). Choice: blocking,
// retried delivery rather than a requeue of Loops let run while the holder was
// Unknown, because it needs no extra state and the first result is a single
// bounded burst (one event per Loop) to a consumer that is always draining.
func (p *ProbeRunnable) retage(ctx context.Context, old, new CNIProbeResult, first bool) {
	log := ctrl.Log.WithName("cni-prober")
	log.Info("CNI probe result changed", "old", string(old.Reason), "new", string(new.Reason))
	if p.Client == nil {
		return
	}
	var loops coxv1alpha1.LoopList
	for {
		err := p.Client.List(ctx, &loops)
		if err == nil {
			break
		}
		log.Error(err, "Could not list Loops for the CNI re-gate", "first", first)
		if !first {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(firstRegateRetry):
		}
	}
	for i := range loops.Items {
		loop := &loops.Items[i]
		if p.Recorder != nil {
			// The Event type tracks the RESULT, not the fact of a change: a
			// transition to an enforcing state (CNIEnforced) or an intentional
			// escape hatch (EnforcementDisabled) is Normal (the healthy
			// outcome), while a non-enforcing result (CNIUnenforced,
			// ProbeUnavailable, Unknown) stays Warning. (PR #42 P3 note: the
			// healthy CNIEnforced outcome previously showed up as a Warning on
			// every Loop.)
			et := corev1.EventTypeWarning
			if new.Reason == ReasonCNIEnforced || new.Reason == ReasonEnforcementDisabled {
				et = corev1.EventTypeNormal
			}
			p.Recorder.Eventf(loop, et, string(new.Reason),
				"NetworkEnforced: %s (%s)", new.Reason, new.Describe())
		}
		if p.Ch != nil {
			ev := event.GenericEvent{Object: loop}
			if first {
				select {
				case p.Ch <- ev:
				case <-ctx.Done():
					return
				}
				continue
			}
			select {
			case p.Ch <- ev:
			default:
				// Drop on a full channel. This is NOT recovered by the next
				// periodic probe: an unchanged result never re-gates (only a
				// change does), so a dropped Loop keeps its previous gate
				// until the result changes or the Loop reconciles for
				// another reason. The first result after startup does not
				// take this path (first above).
			}
		}
	}
}

// Channel wraps a re-gate channel in a source.Channel with the given handler.
// It exists so the regression spec can characterize the buggy handler (see
// RegateSource) without importing controller-runtime's source package here.
func Channel(ch <-chan event.TypedGenericEvent[client.Object], hdl handler.TypedEventHandler[client.Object, reconcile.Request]) source.Source {
	return source.Channel(ch, hdl)
}

// RegateSource builds the source the Loop controller must watch for re-gate
// requests (design points 2+3a): a source.Channel over ch that the probe
// writes GenericEvents (one per Loop) to; the controller's Watches maps each
// to its own reconcile request.
//
// The handler is EnqueueRequestForObject, NOT EnqueueRequestForOwner:
// retage sends one GenericEvent per Loop whose Object is the Loop itself
// (not an object that owns/is owned by a Loop). EnqueueRequestForOwner(
// OnlyControllerOwner) looks for a controller owner reference ON the event
// object and filters to the Loop kind — a Loop has no Loop controller owner,
// so no request is ever enqueued and the re-gate never reconciles any Loop
// (the live failure: the operator logged "CNI probe result changed" and
// emitted the per-Loop Events, but no reconcile followed and the Loops kept
// their stale NetworkEnforced conditions). loop_d38_cni_regate_source_test.go
// drives this function directly and FAILS while it uses
// EnqueueRequestForOwner (R16 I43 gate norm: the spec must fail when the
// gate is disabled — a spec that builds the handler itself would still pass
// if this function regressed).
func RegateSource(ch <-chan event.TypedGenericEvent[client.Object]) source.Source {
	var enqueueObject handler.EnqueueRequestForObject
	return Channel(ch, &enqueueObject)
}

// AddProbeRunnable registers the leader-elected, non-blocking probe Runnable
// with the manager and returns the re-gate source (RegateSource) the Loop
// controller must watch (design points 2+3a).
func AddProbeRunnable(mgr manager.Manager, prober CNIProber, interval, timeout time.Duration) (source.Source, error) {
	ch := make(chan event.TypedGenericEvent[client.Object], 128)
	r := &ProbeRunnable{
		Prober: prober,
		Client: mgr.GetClient(),
		//nolint:staticcheck // SA1019: GetEventRecorderFor (old events API) is deprecated; the new GetEventRecorder returns a different interface (events.EventRecorder) whose method set doesn't match record.EventRecorder. Port to the new API in a follow-on; the old API is still supported.
		Recorder: mgr.GetEventRecorderFor("coxswain-cni-prober"),
		Interval: interval,
		Timeout:  timeout,
		Ch:       ch,
	}
	if err := mgr.Add(r); err != nil {
		return nil, err
	}
	return RegateSource(ch), nil
}
