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
// (fail-closed: the gate holds Loops Suspended until the first probe result
// lands, design point 3).
var probeHolder = &resultHolder{result: CNIProbeResult{Reason: ReasonUnknown}}

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
	// Timeout is the per-run probe timeout (default 60s; --cni-probe-timeout).
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
		p.retage(rctx, old, newResult)
	}
	return nil
}

// retage emits a re-gate Event on every Loop and sends a GenericEvent for
// each on the channel (design point 3a). The reconcile itself reads the new
// result from the holder — it never runs the probe. A nil Client (unit tests)
// skips the Event and the channel send: the re-gate is a no-op and the result
// is still stored in the holder (the metric is updated by the caller).
func (p *ProbeRunnable) retage(ctx context.Context, old, new CNIProbeResult) {
	log := ctrl.Log.WithName("cni-prober")
	log.Info("CNI probe result changed", "old", string(old.Reason), "new", string(new.Reason))
	if p.Client == nil {
		return
	}
	var loops coxv1alpha1.LoopList
	if err := p.Client.List(ctx, &loops); err != nil {
		log.Error(err, "Could not list Loops for the CNI re-gate")
		return
	}
	for i := range loops.Items {
		loop := &loops.Items[i]
		if p.Recorder != nil {
			p.Recorder.Eventf(loop, corev1.EventTypeWarning, string(new.Reason),
				"NetworkEnforced: %s (%s)", new.Reason, new.Describe())
		}
		if p.Ch != nil {
			select {
			case p.Ch <- event.GenericEvent{Object: loop}:
			default:
				// Drop on a full channel: the next periodic probe re-gates.
				// Never block the Runnable.
			}
		}
	}
}

// AddProbeRunnable registers the leader-elected, non-blocking probe Runnable
// with the manager and returns the source the Loop controller must watch for
// re-gate requests (design points 2+3a). The source is a source.Channel that
// the probe writes GenericEvents (one per Loop) to; the controller's
// Watches maps each to its own reconcile request.
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
// drives the real source.Channel with both handlers and FAILS while the
// source uses EnqueueRequestForOwner.
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
	var enqueueObject handler.EnqueueRequestForObject
	return source.Channel(ch, &enqueueObject), nil
}
