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

package engine

import (
	"context"
	"testing"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// TestKubeArmorEnforcerEnforcingUnverifiedUntilI32 (I46): until the I32 relay
// consumes the KubeArmor alert stream, Enforcing must report (false,
// ReasonEnforcementUnverified) — "no probe", not "not enforcing" — so the
// PolicyEnforced condition is Unknown/EnforcementUnverified, never a claim
// that the engine is not enforcing while KubeArmor may be enforcing.
func TestKubeArmorEnforcerEnforcingUnverifiedUntilI32(t *testing.T) {
	e := &KubeArmorEnforcer{}
	enforcing, reason := e.Enforcing(context.Background(), &coxv1alpha1.Loop{})
	if enforcing {
		t.Fatalf("Enforcing = true; want false until the I32 relay is wired (fail-closed)")
	}
	if reason != ReasonEnforcementUnverified {
		t.Fatalf("Enforcing reason = %q; want %q (no probe, not NodeNotEnforcing)", reason, ReasonEnforcementUnverified)
	}
}
