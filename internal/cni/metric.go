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

// Package cni — the coxswain_network_enforced metric (D38 design point 3).
// 1.0 when the CNI enforces (reason CNIEnforced), 0.0 for every other reason —
// including Unknown and ProbeUnavailable, which makes the resulting Suspended
// state ALERTABLE: a cluster whose probe never ran (or cannot run) reads 0.0
// and pages, instead of a silent "no metric".
package cni

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// networkEnforcedGauge is the D38 gauge (design point 3). It is a single
// value per operator process (the probe result is cluster-wide operator
// state, not per-Loop) — the per-Loop condition is the Loop-facing surface.
var networkEnforcedGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "coxswain_network_enforced",
		Help: "1.0 when the CNI enforces pod -> host-network egress (NetworkEnforced condition True), 0.0 otherwise — including Unknown (no probe result yet) and ProbeUnavailable, which holds Loops Suspended and is alertable.",
	},
	[]string{"reason"},
)

func init() {
	metrics.Registry.MustRegister(networkEnforcedGauge)
}

// SetNetworkEnforcedMetric records the current probe result in the gauge.
// Called by the probe Runnable on every result change.
func SetNetworkEnforcedMetric(r CNIProbeResult) {
	networkEnforcedGauge.Reset()
	if r.Reason == ReasonCNIEnforced {
		networkEnforcedGauge.WithLabelValues(string(r.Reason)).Set(1)
	} else {
		networkEnforcedGauge.WithLabelValues(string(r.Reason)).Set(0)
	}
}

// SetNetworkEnforcedMetricForTest resets the gauge to its zero state. TEST-ONLY.
func SetNetworkEnforcedMetricForTest() { networkEnforcedGauge.Reset() }
