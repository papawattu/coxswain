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

// Package engine is the seam between Coxswain and the eBPF enforcement engine
// (ADR-0007 Q3). The operator is engine-agnostic: it emits a policy and reads
// enforcement evidence through this interface, so the KubeArmor implementation
// (the default) is swappable. The operator never imports the engine's types
// directly; the KubeArmorPolicy is represented here by a minimal typed struct
// (KubeArmorPolicy) so the emitter is a pure, testable function without a
// dependency on the KubeArmor Go module.
package engine

import (
	"context"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/policy"
)

// Enforcer is the engine-agnostic seam the operator uses to apply a policy and
// read enforcement evidence. The KubeArmor implementation (the default)
// translates Coxswain's EnginePolicy into a KubeArmorPolicy and reports whether
// the engine is actually enforcing it (D30). A fake implementation is used in
// envtest to drive the fail-closed gate without a real engine.
type Enforcer interface {
	// Apply emits the effective engine policy for the Loop (idempotent). It is
	// called whenever the Loop's effective policy changes. It must not block on
	// the engine; a failure to apply surfaces as the caller's reconcile error.
	Apply(ctx context.Context, loop *coxv1alpha1.Loop, ep policy.EffectivePolicy) error

	// Enforcing reports (D30) whether the engine is ACTUALLY enforcing the
	// Loop's policy on the node the sandbox lands on. It returns a boolean and,
	// when false, a reason (EngineUnavailable | NodeNotEnforcing | PolicyRejected
	// | AuditOnly). The operator uses this to fail closed: a sandbox only runs
	// (OperatingMode Running) when Enforcing is true; otherwise it is held
	// Suspended with PolicyEnforced=False and requeued (never fails the Loop).
	Enforcing(ctx context.Context, loop *coxv1alpha1.Loop) (bool, string)
}

// EngineEnforcementReasons are the D30 reasons for PolicyEnforced=False.
const (
	ReasonEngineUnavailable = "EngineUnavailable"
	ReasonNodeNotEnforcing  = "NodeNotEnforcing"
	ReasonPolicyRejected    = "PolicyRejected"
	ReasonAuditOnly         = "AuditOnly"
)
