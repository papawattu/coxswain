// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License
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

// I42c review P2: the egress-proxy NetworkPolicy's except list is derived
// from internal/egress (the egress binary's resolved-IP carve-outs), so the
// two layers cannot drift. A second hand-written v4 list had already lost
// 100.64.0.0/10 (CGNAT). This unit test pins the superset: every v4 carve-out
// the egress binary applies must appear in the netpol's except list.

import (
	"testing"

	"github.com/papawattu/coxswain/internal/egress"
)

func TestEgressCarveOutCIDRsSupersetOfEgressV4(t *testing.T) {
	// With no operator-supplied CIDRs the except list is the egress binary's
	// v4 carve-outs verbatim; with them it must be a strict superset.
	for _, tc := range []struct {
		name        string
		podCIDR     string
		serviceCIDR string
	}{
		{name: "no operator CIDRs", podCIDR: "", serviceCIDR: ""},
		{name: "operator CIDRs", podCIDR: "10.244.0.0/16", serviceCIDR: "10.96.0.0/12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			except := egressCarveOutCIDRs(tc.podCIDR, tc.serviceCIDR)
			exceptSet := map[string]bool{}
			for _, c := range except {
				exceptSet[c] = true
			}
			for _, c := range egress.CarveOutCIDRsV4() {
				if !exceptSet[c] {
					t.Errorf("egressCarveOutCIDRs(%q, %q) is missing egress v4 carve-out %q: the netpol and egress binary layers must agree", tc.podCIDR, tc.serviceCIDR, c)
				}
			}
			if tc.podCIDR != "" && !exceptSet[tc.podCIDR] {
				t.Errorf("egressCarveOutCIDRs must include the operator POD_CIDR %q", tc.podCIDR)
			}
			if tc.serviceCIDR != "" && !exceptSet[tc.serviceCIDR] {
				t.Errorf("egressCarveOutCIDRs must include the operator SERVICE_CIDR %q", tc.serviceCIDR)
			}
		})
	}
}

// TestEgressCarveOutCIDRsV6 pins the v6 mirror's carve-outs: the plan's v6
// except list (fc00::/7, fe80::/10, ::1/128, 64:ff9b::/96, ::/128) is exactly
// the egress binary's v6 list, so asserting membership pins both the mirror
// and the derivation.
func TestEgressCarveOutCIDRsV6(t *testing.T) {
	v6 := egress.CarveOutCIDRsV6()
	v6Set := map[string]bool{}
	for _, c := range v6 {
		v6Set[c] = true
	}
	for _, want := range []string{"fc00::/7", "fe80::/10", "::1/128", "64:ff9b::/96", "::/128"} {
		if !v6Set[want] {
			t.Errorf("egress.CarveOutCIDRsV6 is missing %q (the plan's v6 carve-out list)", want)
		}
	}
}
