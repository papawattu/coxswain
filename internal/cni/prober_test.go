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
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

const testLoopName = "d38-loop" // shared by both prober specs (goconst)

// D38s3 (Calico run): probeOnce must be the ONLY writer to the result holder,
// and it must re-gate (send a GenericEvent for every Loop + update the metric)
// whenever the probe result CHANGES. The regression this catches: if the
// PodProber also writes the holder itself (the old PodProber.parse() called
// Holder().Set), then probeOnce's Holder().Set reads changed=false and skips
// the re-gate entirely — no log, no metric, no GenericEvent, so Loops keep the
// stale Unknown condition forever.
//
// The test drives a probeOnce with the REAL PodProber (whose parse() is the
// path that would write the holder) and asserts the channel receives a
// GenericEvent for the Loop and the metric is set to 1.0. With the parse-side
// Holder().Set put back, the prober's Probe() pre-writes the holder to
// CNIEnforced, probeOnce's Set reads changed=false, and the channel stays
// empty — the test fails.
func TestProbeOnceReGatesOnResultChange(t *testing.T) {
	// Reset the holder and metric to their initial state so the test is
	// self-contained.
	ResetForTest()
	t.Cleanup(ResetForTest)
	SetNetworkEnforcedMetricForTest()
	t.Cleanup(SetNetworkEnforcedMetricForTest)

	// The holder starts at Unknown (ResetForTest).
	if got := Holder().Result().Reason; got != ReasonUnknown {
		t.Fatalf("precondition: holder should start at Unknown, got %v", got)
	}

	// One Loop to re-gate.
	scheme := runtime.NewScheme()
	_ = coxv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: testLoopName, Namespace: "d38-e2e"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loop).Build()

	// A small buffered channel for the re-gate GenericEvent.
	ch := make(chan event.TypedGenericEvent[client.Object], 8)

	// The REAL PodProber drives the probe: its parse() is the path that, if
	// buggy, writes the holder. A probe whose result differs from the holder's
	// (Unknown -> CNIEnforced) must cause probeOnce to re-gate.
	prober := NewPodProber(PodProberConfig{
		Namespace:  probePodName,
		ProbeImage: probeTestImage,
		Client:     cl,
		Reader:     cl, // the direct reader is the same fake (test)
	})

	// The Runnable drives the re-gate (no recorder — the unit only needs the
	// channel send + metric update).
	p := &ProbeRunnable{
		Prober:  prober,
		Client:  cl,
		Timeout: 2 * time.Second, // the per-run timeout (short for the unit)
		Ch:      ch,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.probeOnce(ctx); err != nil {
		t.Fatalf("probeOnce: %v", err)
	}

	// The holder must now hold the prober's result. On the real PodProber the
	// probe runs against the fake client (no probe pod object) -> ProbeUnavailable
	// (the fake client has no probe pod, so the probe fails -> ProbeUnavailable).
	// That is fine: the point is that the reason CHANGED from Unknown to
	// ProbeUnavailable and the re-gate fired. What the test must NOT tolerate is
	// the holder being pre-written by parse() (changed=false -> no re-gate).
	got := Holder().Result().Reason
	if got == ReasonUnknown {
		t.Fatal("the holder is still Unknown: probeOnce did not record the probe result at all")
	}

	// The metric must be set to a non-zero value for the enforced reason only;
	// for any non-enforcing reason it is 0.0. The re-gate fired only if the
	// holder changed, so the metric reflects the new reason.
	switch got {
	case ReasonCNIEnforced:
		if v := testutil.ToFloat64(networkEnforcedGauge.WithLabelValues("CNIEnforced")); v != 1 {
			t.Fatalf("coxswain_network_enforced{reason=CNIEnforced} should be 1 after a change, got %v", v)
		}
	default:
		if v := testutil.ToFloat64(networkEnforcedGauge.WithLabelValues(string(got))); v != 0 {
			t.Fatalf("coxswain_network_enforced{reason=%s} should be 0 for a non-enforcing reason, got %v", got, v)
		}
	}

	// The channel must receive a GenericEvent for the Loop (the re-gate signal
	// the Loop controller watches). This is the assertion that fails if
	// parse() writes the holder itself (changed=false -> no send).
	select {
	case evt := <-ch:
		if evt.Object == nil {
			t.Fatalf("re-gate GenericEvent has a nil Object")
		}
		l, ok := evt.Object.(*coxv1alpha1.Loop)
		if !ok {
			t.Fatalf("re-gate GenericEvent is not a *Loop, got %T", evt.Object)
		}
		if l.Name != testLoopName {
			t.Fatalf("re-gate GenericEvent Loop name = %q, want d38-loop", l.Name)
		}
	default:
		t.Fatal("the re-gate GenericEvent was not sent on the channel: the holder was written by parse(), not by probeOnce (changed=false -> retage skipped)")
	}
}

// D38 P3 note (PR #42): the re-gate Event's type must track the RESULT, not
// the fact of a change. A transition to an enforcing state (CNIEnforced) or
// an intentional escape hatch (EnforcementDisabled) emits a Normal Event (the
// healthy outcome); any non-enforcing result (CNIUnenforced, ProbeUnavailable,
// Unknown) emits a Warning. The FakeRecorder renders each event as
// "<type> <reason> <message> ", so the prefix identifies the type.
func TestRetageEventTracksResult(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = coxv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: testLoopName, Namespace: "d38-e2e"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loop).Build()

	cases := []struct {
		reason Reason
		want   string
	}{
		{ReasonCNIEnforced, corev1.EventTypeNormal + " "},
		{ReasonEnforcementDisabled, corev1.EventTypeNormal + " "},
		{ReasonCNIUnenforced, corev1.EventTypeWarning + " "},
		{ReasonProbeUnavailable, corev1.EventTypeWarning + " "},
		{ReasonUnknown, corev1.EventTypeWarning + " "},
	}
	for _, tc := range cases {
		t.Run(string(tc.reason), func(t *testing.T) {
			recorder := record.NewFakeRecorder(16)
			p := &ProbeRunnable{Prober: nil, Client: cl, Recorder: recorder}
			p.retage(context.Background(),
				CNIProbeResult{Reason: ReasonUnknown},
				CNIProbeResult{Reason: tc.reason}, false)
			got, ok := <-recorder.Events
			if !ok {
				t.Fatalf("no Event emitted for reason %s", tc.reason)
			}
			if len(got) < len(tc.want) || got[:len(tc.want)] != tc.want {
				t.Fatalf("Event type for %s = %q, want prefix %q", tc.reason, got, tc.want)
			}
			// The reason must ride on the Event (operator grep-ability).
			if !strings.Contains(got, " "+string(tc.reason)+" ") {
				t.Fatalf("Event does not carry the reason: %q", got)
			}
		})
	}
}

// D142(a) (I273): the FIRST result after startup reaches every Loop even when
// the re-gate channel is full: delivery blocks instead of dropping. A later
// change keeps the best-effort drop.
func TestFirstResultRegatesEveryLoopThroughAFullChannel(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)

	scheme := runtime.NewScheme()
	_ = coxv1alpha1.AddToScheme(scheme)
	objs := make([]client.Object, 0, 3)
	for _, n := range []string{"a", "b", "c"} {
		objs = append(objs, &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: "ns"}})
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	ch := make(chan event.TypedGenericEvent[client.Object], 1)
	ch <- event.GenericEvent{Object: objs[0]} // full before the probe runs
	f := NewFakeProber()
	f.SetResult(CNIProbeResult{Reason: ReasonCNIUnenforced, Detail: "X"})
	p := &ProbeRunnable{Prober: f, Client: cl, Timeout: time.Second, Ch: ch}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = p.probeOnce(ctx); close(done) }()

	time.Sleep(200 * time.Millisecond) // the channel stays full: nobody drains yet
	got := 0
	for got < 4 { // the filler + the three Loops
		select {
		case <-ch:
			got++
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 4 events arrived: the first result was dropped on a full channel", got)
		}
	}
	<-done
}

// A failed Loop list must not lose the first result either: it is retried.
func TestFirstResultRetriesTheLoopList(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	old := firstRegateRetry
	firstRegateRetry = 10 * time.Millisecond
	t.Cleanup(func() { firstRegateRetry = old })

	scheme := runtime.NewScheme()
	_ = coxv1alpha1.AddToScheme(scheme)
	fails := 2
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"}}).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, o ...client.ListOption) error {
			if fails > 0 {
				fails--
				return errors.New("transient")
			}
			return c.List(ctx, l, o...)
		}}).Build()
	ch := make(chan event.TypedGenericEvent[client.Object], 4)
	f := NewFakeProber()
	f.SetResult(CNIProbeResult{Reason: ReasonCNIUnenforced, Detail: "X"})
	p := &ProbeRunnable{Prober: f, Client: cl, Timeout: time.Second, Ch: ch}
	if err := p.probeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	default:
		t.Fatal("the first result was lost after a transient list failure")
	}
}

// I281 (S3): a ProbeUnavailable result is retried on a backoff. The fake
// Prober returns unavailable on call 1 and enforced on call 2. With
// Interval=1h and RetryInitial=50ms, the holder must be CNIEnforced within 2s
// (not waiting 1h for the next tick).
func TestProbeUnavailableRetriesOnBackoff(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)

	scheme := runtime.NewScheme()
	_ = coxv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: testLoopName, Namespace: "d38-e2e"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loop).Build()

	// A prober that returns unavailable first, then enforced.
	seqProber := &sequenceProber{results: []CNIProbeResult{
		{Reason: ReasonProbeUnavailable, Detail: "simulated"},
		{Reason: ReasonCNIEnforced, Detail: "ok"},
	}}

	p := &ProbeRunnable{
		Prober:       seqProber,
		Client:       cl,
		Interval:     time.Hour,
		RetryInitial: 50 * time.Millisecond,
		Timeout:      time.Second,
		Ch:           make(chan event.TypedGenericEvent[client.Object], 8),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = p.Start(ctx) }()

	// Poll the holder: it must reach CNIEnforced well before 1h.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if Holder().Result().Reason == ReasonCNIEnforced {
			cancel()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("holder is %v after 2s; want CNIEnforced (the unavailable result must be retried on a backoff, not wait Interval)", Holder().Result().Reason)
}

// sequenceProber is a CNIProber that returns results in order, then the last
// one forever.
type sequenceProber struct {
	results []CNIProbeResult
	calls   int
}

func (s *sequenceProber) Probe(_ context.Context) (CNIProbeResult, error) {
	idx := s.calls
	if idx >= len(s.results) {
		idx = len(s.results) - 1
	}
	s.calls++
	return s.results[idx], nil
}

func (s *sequenceProber) LatestResult() CNIProbeResult {
	return s.results[0]
}

// timedProber wraps a CNIProber and records the time of each Probe call
// (S5: the backoff-reset spec asserts on the gap between calls).
type timedProber struct {
	inner  CNIProber
	record func()
}

func (t *timedProber) Probe(ctx context.Context) (CNIProbeResult, error) {
	if t.record != nil {
		t.record()
	}
	return t.inner.Probe(ctx)
}

func (t *timedProber) LatestResult() CNIProbeResult {
	return t.inner.LatestResult()
}

// I281 (S4): nextProbeDelay doubles on consecutive unavailable results and
// caps at Interval; a non-unavailable result resets to Interval.
func TestNextProbeDelay(t *testing.T) {
	interval := 10 * time.Minute
	base := 15 * time.Second
	cases := []struct {
		name     string
		prev     time.Duration
		interval time.Duration
		base     time.Duration
		unavail  bool
		want     time.Duration
	}{
		{"first unavailable (prev=0) → base", 0, interval, base, true, base},
		{"second unavailable (prev=base) → 2×base", base, interval, base, true, 2 * base},
		{"third unavailable (prev=2×base) → 4×base", 2 * base, interval, base, true, 4 * base},
		{"unavailable past cap → interval", interval, interval, base, true, interval},
		{"not unavailable → interval (reset)", base, interval, base, false, interval},
		{"not unavailable (prev=0) → interval", 0, interval, base, false, interval},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextProbeDelay(tc.prev, tc.interval, tc.base, tc.unavail)
			if got != tc.want {
				t.Fatalf("nextProbeDelay(prev=%v, interval=%v, base=%v, unavail=%v) = %v, want %v",
					tc.prev, tc.interval, tc.base, tc.unavail, got, tc.want)
			}
		})
	}
}

// I281 P1 (S5): after a good result the backoff resets to 0, so the next
// probe is at the full interval (not the leftover backoff). Prober results:
// unavailable, enforced, enforced. Interval=400ms, RetryInitial=20ms.
//
// With the reset: call 1 (unavail) sets backoff=20ms; call 2 (enforced) resets
// backoff=0, timer=interval=400ms; call 3 is 400ms after call 2.
// Without the reset (M3): call 1 sets backoff=20ms; call 2 does not reset,
// timer=20ms; call 3 is 20ms after call 2.
//
// Assertion: the gap between call 2 and call 3 is >= 300ms (the full interval
// after the reset). Fails under M3 where the gap is ~20ms.
func TestBackoffResetsAfterGoodResult(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)

	scheme := runtime.NewScheme()
	_ = coxv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()

	var mu sync.Mutex
	var callTimes []time.Time
	seqProber := &sequenceProber{results: []CNIProbeResult{
		{Reason: ReasonProbeUnavailable, Detail: "simulated"},
		{Reason: ReasonCNIEnforced, Detail: "ok"},
		{Reason: ReasonCNIEnforced, Detail: "still ok"},
	}}
	// Wrap the prober to record call times.
	tp := &timedProber{inner: seqProber}
	tp.record = func() {
		mu.Lock()
		callTimes = append(callTimes, time.Now())
		mu.Unlock()
	}

	p := &ProbeRunnable{
		Prober:       tp,
		Client:       cl,
		Interval:     400 * time.Millisecond,
		RetryInitial: 20 * time.Millisecond,
		Timeout:      100 * time.Millisecond,
		Ch:           make(chan event.TypedGenericEvent[client.Object], 8),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = p.Start(ctx) }()

	// Wait until all three calls have been made.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(callTimes)
		mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			mu.Lock()
			defer mu.Unlock()
			t.Fatalf("only %d probe calls before deadline; want 3 (unavailable, enforced, enforced)", n)
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	// Gap between call 2 (enforced) and call 3: with the reset, the timer is
	// the full interval (400ms); without the reset (M3), the timer is the
	// leftover backoff (20ms).
	gap := callTimes[2].Sub(callTimes[1])
	if gap < 300*time.Millisecond {
		t.Fatalf("gap between enforced (call 2) and next probe (call 3) = %v; want >= 300ms (the full interval after a backoff reset, not the leftover backoff)",
			gap.Round(time.Millisecond))
	}
}
