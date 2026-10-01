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

// Package cni is the operator-side CNI self-test seam (D38). The operator
// probes whether the cluster's CNI polices pod -> host-network egress (the
// D38 property) by running a short-lived probe pod under the agent-shaped
// NetworkPolicy and reading the RESULT lines it writes to its termination
// message. The result gates Loop execution (NetworkEnforced condition,
// D30-style) exactly like the eBPF gate (internal/engine) does.
package cni

import (
	"context"
	"fmt"
)

// Reason is the NetworkEnforced condition reason (D38 plan, design point 3).
// All five reasons are listed here; the Suspended semantics live in
// NetworkEnforcedStatus.
type Reason string

const (
	// ReasonCNIEnforced: probe ran, every target BLOCKED (status True).
	ReasonCNIEnforced Reason = "CNIEnforced"
	// ReasonCNIUnenforced: probe ran, some target REACHABLE (status False).
	ReasonCNIUnenforced Reason = "CNIUnenforced"
	// ReasonProbeUnavailable: the probe could not run — pod not Ready in the
	// timeout, no output, unknown/wrong-count RESULT lines, or a non-zero pod
	// exit (status False, fail-closed).
	ReasonProbeUnavailable Reason = "ProbeUnavailable"
	// ReasonEnforcementDisabled: not enforced, but the operator runs with
	// --allow-unenforced-network, so Loops run anyway (status False). The
	// only non-enforcing reason that does NOT hold Suspended.
	ReasonEnforcementDisabled Reason = "EnforcementDisabled"
	// ReasonUnknown: initial state before the first probe result (status
	// Unknown). The gate treats it as not enforced (Suspended), so the
	// operator is fail-closed from startup, not just from the first probe
	// failure.
	ReasonUnknown Reason = "Unknown"
)

// CNIProbeResult is one probe run's outcome.
type CNIProbeResult struct {
	// Reason is one of CNIEnforced / CNIUnenforced / ProbeUnavailable.
	Reason Reason
	// Rows are the parsed target rows (label -> REACHABLE|BLOCKED), for the
	// Event message and the condition message. Empty on ProbeUnavailable.
	Rows []TargetRow
	// Detail carries the short human-readable detail for the condition
	// message (e.g. the reachable labels, or the validation error).
	Detail string
}

// TargetRow is one probe target's row.
type TargetRow struct {
	Label   string
	Verdict string // REACHABLE or BLOCKED(...)
}

// NetworkEnforcedCondition is the Loop condition type (D38), analogous to
// PolicyEnforced (D30) but for the network layer.
const NetworkEnforcedCondition = "NetworkEnforced"

// HoldsSuspended reports whether a NetworkEnforced result holds Loops
// Suspended. Every reason holds Suspended EXCEPT CNIEnforced (it's True) and
// EnforcementDisabled (the escape hatch was set on purpose, so Loops run
// anyway). Unknown and ProbeUnavailable hold Suspended: the operator is
// fail-closed, including before the first probe result.
func (r CNIProbeResult) HoldsSuspended() bool {
	switch r.Reason {
	case ReasonCNIEnforced, ReasonEnforcementDisabled:
		return false
	default:
		return true
	}
}

// CNIProber is the network-layer seam the operator uses (D38), analogous to
// engine.Enforcer (D30). The real implementation creates the probe pod +
// NetworkPolicy in the fixed coxswain-cni-probe namespace, waits for it with
// a per-run timeout, reads the termination message, validates the RESULT
// lines strictly, and deletes the pod. A fake drives the envtests.
//
// Probe must not block indefinitely: the caller passes a bounded context, and
// a probe that cannot complete (timeout, pod not Ready, pull failure) must
// return ReasonProbeUnavailable — never an error that is treated as
// "enforced".
//
// The reconcile loop reads the CURRENT result through LatestResult() — the
// real implementation's LatestResult returns the holder's cached result (the
// probe itself runs in the leader-elected Runnable); the fake returns its
// configured result, which is what the envtest specs drive. This keeps the
// reconcile loop free of any probe execution (the probe runs in the
// Runnable, never in the reconcile loop, design point 3a).
type CNIProber interface {
	// Probe runs one probe and reports the result. It is called by the
	// leader-elected probe Runnable, not by the reconcile loop.
	Probe(ctx context.Context) (CNIProbeResult, error)
	// LatestResult returns the current result the reconcile loop should read
	// (the cached result, or the fake's configured result). It must never
	// run a probe.
	LatestResult() CNIProbeResult
}

// Describe renders a short message for the NetworkEnforced condition
// and the K8s Event.
func (r CNIProbeResult) Describe() string {
	if r.Reason == ReasonProbeUnavailable && r.Detail != "" {
		return fmt.Sprintf("probe unavailable: %s", r.Detail)
	}
	if r.Reason == ReasonCNIUnenforced {
		return fmt.Sprintf("REACHABLE targets: %s", r.Detail)
	}
	if r.Reason == ReasonCNIEnforced {
		return "every target was BLOCKED by the NetworkPolicy (pod -> host-network egress is policed)"
	}
	return r.Detail
}
