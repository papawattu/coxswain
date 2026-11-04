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
	"strings"
	"testing"
)

// P6a follow-ups (reviewer's P3-3, finding K): the CRD's CEL XValidation
// rejects a non-canonical exec on every write form (Create and Patch both
// re-evaluate it), so no spec-pinned envtest can seed one through the API
// server. The NonCanonicalExecPath gate's MESSAGE therefore gets unit
// coverage here: it must name the RESOLVED kind, and the kind is a
// function of the ref, not a hard-coded "AgentPolicy".
func TestNonCanonicalExecMessageNamesKind(t *testing.T) {
	msg := nonCanonicalExecMessage("ClusterAgentPolicy", "p", p6aClusterNonCanonical)
	for _, want := range []string{"ClusterAgentPolicy p:", p6aClusterNonCanonical} {
		if !strings.Contains(msg, want) {
			t.Errorf("nonCanonicalExecMessage(%q) = %q; want it to contain %q", "ClusterAgentPolicy", msg, want)
		}
	}
}
