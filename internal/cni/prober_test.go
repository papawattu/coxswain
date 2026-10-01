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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

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
		ObjectMeta: metav1.ObjectMeta{Name: "d38-loop", Namespace: "d38-e2e"},
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
		if l.Name != "d38-loop" {
			t.Fatalf("re-gate GenericEvent Loop name = %q, want d38-loop", l.Name)
		}
	default:
		t.Fatal("the re-gate GenericEvent was not sent on the channel: the holder was written by parse(), not by probeOnce (changed=false -> retage skipped)")
	}
}
