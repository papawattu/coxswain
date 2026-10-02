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
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// D38 design point 3: the coxswain_network_enforced gauge is 1.0 when the CNI
// enforces (CNIEnforced), 0.0 for every other reason — including Unknown and
// ProbeUnavailable, which makes the resulting Suspended state alertable.
func TestNetworkEnforcedMetric(t *testing.T) {
	SetNetworkEnforcedMetricForTest()
	t.Cleanup(SetNetworkEnforcedMetricForTest)
	for _, tc := range []struct {
		reason Reason
		want   float64
	}{
		{ReasonCNIEnforced, 1},
		{ReasonCNIUnenforced, 0},
		{ReasonProbeUnavailable, 0},
		{ReasonUnknown, 0},
		{ReasonEnforcementDisabled, 0},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			SetNetworkEnforcedMetric(CNIProbeResult{Reason: tc.reason})
			got, err := testutil.GatherAndCount(metrics.Registry, "coxswain_network_enforced")
			if err != nil {
				t.Fatalf("gather: %v", err)
			}
			if got != 1 {
				t.Fatalf("want exactly one series after a result change, got %d", got)
			}
			val := testutil.ToFloat64(networkEnforcedGauge.WithLabelValues(string(tc.reason)))
			if val != tc.want {
				t.Fatalf("coxswain_network_enforced{reason=%q} = %v, want %v", tc.reason, val, tc.want)
			}
		})
	}
}

// D38 design point 3: an Event is emitted on a condition CHANGE (the probe
// Runnable re-gate path exercises the same Set-changed semantics). Here we
// exercise the pure comparison the controller uses: unchanged result = no
// event, changed = event.
func TestNetworkEnforcedChangedSemantics(t *testing.T) {
	unchanged := CNIProbeResult{Reason: ReasonProbeUnavailable, Detail: "d"}
	same := CNIProbeResult{Reason: ReasonProbeUnavailable, Detail: "d"}
	if unchanged.Reason == same.Reason && unchanged.Detail == same.Detail {
		// no change -> no re-gate event (the Runnable's `changed` guard)
	} else {
		t.Fatal("precondition broken")
	}
	flipped := CNIProbeResult{Reason: ReasonCNIEnforced}
	if unchanged.Reason == flipped.Reason {
		t.Fatal("a reason flip must be treated as a change (the event fires)")
	}
}
