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

package alerts

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// TestHolderForgetPrunesSummaryAndSnapshot asserts R52 I249's acceptance at the
// Holder itself: a deleted Loop's learn summary AND F62 snapshot are both gone,
// and Forget prunes only its own UID.
//
// Mutations: removing `delete(h.learn, uid)` fails the "learn summary pruned"
// assertion (m1); removing `delete(h.loops, uid)` fails the "snapshot pruned"
// assertion (m2).
func TestHolderForgetPrunesSummaryAndSnapshot(t *testing.T) {
	h := NewHolder()
	u1 := types.UID("uid-prune-1")
	u2 := types.UID("uid-prune-2")
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)

	base := Record{Loop: "l1", Namespace: "ns", Source: SourceKubeArmor, Action: "Audit", Target: "/tmp/x", Detail: "operation=File, pod=l1-sandbox"}
	h.observe(u1, Alert{Blocked: true, Target: "/etc/passwd", Time: now})
	h.addLearn(u1, withTime(base, "2026-11-08T10:00:00Z"))
	h.observe(u2, Alert{Blocked: true, Target: "/etc/shadow", Time: now})
	h.addLearn(u2, withTime(base, "2026-11-08T10:01:00Z"))

	if _, ok := h.Snapshot(u1); !ok {
		t.Fatal("Snapshot(u1) must exist before Forget")
	}
	if _, _, ok := h.LearnSummary(u1); !ok {
		t.Fatal("LearnSummary(u1) must exist before Forget")
	}

	h.Forget(u1)

	if _, ok := h.Snapshot(u1); ok {
		t.Error("Snapshot still returns ok=true after Forget; the snapshot must be pruned")
	}
	if _, _, ok := h.LearnSummary(u1); ok {
		t.Error("LearnSummary still returns ok=true after Forget; the learn summary must be pruned")
	}
	if _, ok := h.Snapshot(u2); !ok {
		t.Error("Snapshot(u2) must survive Forget(u1); Forget prunes only its own UID")
	}
	if _, _, ok := h.LearnSummary(u2); !ok {
		t.Error("LearnSummary(u2) must survive Forget(u1); Forget prunes only its own UID")
	}

	// Degenerate calls must not panic.
	h.Forget("")
	var nilH *Holder
	nilH.Forget(u1)
}
