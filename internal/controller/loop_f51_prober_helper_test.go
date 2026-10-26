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

import (
	"github.com/papawattu/coxswain/internal/cni"
)

// testCNIProber is the D38 fake prober for specs that do not exercise the
// network gate themselves (F51 P2-3): a nil CNIProber now holds every sandbox
// Suspended (fail-closed), so a reconciler that leaves it nil would hold its
// sandbox for a reason the spec is not testing. The fake reports CNIEnforced —
// the network gate is satisfied — which is exactly the state the old nil
// short-circuit ("CNI probe not configured; gate not active") claimed.
//
// Specs that DO exercise the gate set their own prober result (CNIUnenforced,
// ProbeUnavailable, Unknown) and must not use this helper.
func testCNIProber() *cni.FakeProber {
	p := cni.NewFakeProber()
	p.SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced})
	return p
}
