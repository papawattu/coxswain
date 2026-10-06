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

// Unit tests for the PURE stall decision (P2e item 2). The envtest specs
// (loop_p2e_stall_envtest_test.go) drive the gate through the reconcile; these
// pin the decision arithmetic in isolation (the gate-mutation coverage the
// plan calls for, in the scratch worktree).
package controller

import (
	"testing"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// TestStallDecisionFiresAfterNConsecutive: N consecutive identical hashes
// (same version) fire; N-1 do not. The run counts the just-appended entry +
// the matching trailing history.
func TestStallDecisionFiresAfterNConsecutive(t *testing.T) {
	mkEntry := func(h, v string) coxv1alpha1.StallEntry {
		return coxv1alpha1.StallEntry{Hash: h, NormalisationVersion: v}
	}
	cases := []struct {
		name       string
		history    []coxv1alpha1.StallEntry
		newEntry   coxv1alpha1.StallEntry
		stallAfter int32
		wantFire   bool
		wantRun    int
	}{
		{"one of N=3", nil, mkEntry("h", "v1"), 3, false, 1},
		{"two of N=3", []coxv1alpha1.StallEntry{mkEntry("h", "v1")}, mkEntry("h", "v1"), 3, false, 2},
		{"three of N=3 fires", []coxv1alpha1.StallEntry{mkEntry("h", "v1"), mkEntry("h", "v1")}, mkEntry("h", "v1"), 3, true, 3},
		{"N=2 fires at two", []coxv1alpha1.StallEntry{mkEntry("h", "v1")}, mkEntry("h", "v1"), 2, true, 2},
		{"N=1 fires at one", nil, mkEntry("h", "v1"), 1, true, 1},
		{"different hash resets", []coxv1alpha1.StallEntry{mkEntry("h", "v1"), mkEntry("g", "v1")}, mkEntry("f", "v1"), 3, false, 1},
		{"version change resets", []coxv1alpha1.StallEntry{mkEntry("h", "v1")}, mkEntry("h", "v2"), 3, false, 1},
		{"run does not cross a different hash", []coxv1alpha1.StallEntry{mkEntry("g", "v1"), mkEntry("h", "v1")}, mkEntry("h", "v1"), 2, true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fired, run := stallDecision(tc.history, tc.newEntry, tc.stallAfter)
			if fired != tc.wantFire {
				t.Fatalf("fire = %v, want %v", fired, tc.wantFire)
			}
			if run != tc.wantRun {
				t.Fatalf("run = %d, want %d", run, tc.wantRun)
			}
		})
	}
}

// TestStallDecisionZeroStallAfterDefaultsToThree: a resolved stallAfter of 0
// (malformed — the CRD rejects <1) is treated as 3 (the code default) so a
// single entry never fires.
func TestStallDecisionZeroStallAfterDefaultsToThree(t *testing.T) {
	e := coxv1alpha1.StallEntry{Hash: "h", NormalisationVersion: "v1"}
	if fired, _ := stallDecision(nil, e, 0); fired {
		t.Fatalf("a single entry fired with stallAfter=0 (should default to 3)")
	}
}

// TestAppendStallEntryDedupsByJobName: a re-read of the same verify Job
// (same jobName) appends NO entry (item 6).
func TestAppendStallEntryDedupsByJobName(t *testing.T) {
	loop := &coxv1alpha1.Loop{}
	loop.Status.Iteration = 1
	appendStallEntry(loop, "lp-verify-1", "h1", "check-0", "")
	appendStallEntry(loop, "lp-verify-1", "h1", "check-0", "") // dedup
	if len(loop.Status.StallHistory) != 1 {
		t.Fatalf("history len = %d, want 1 (dedup by jobName)", len(loop.Status.StallHistory))
	}
	// A NEW iteration (new jobName) appends.
	loop.Status.Iteration = 2
	appendStallEntry(loop, "lp-verify-2", "h1", "check-0", "")
	if len(loop.Status.StallHistory) != 2 {
		t.Fatalf("history len = %d, want 2", len(loop.Status.StallHistory))
	}
}

// TestAppendStallEntryCapsRing: the ring holds the last 10 entries (the older
// ones evict).
func TestAppendStallEntryCapsRing(t *testing.T) {
	loop := &coxv1alpha1.Loop{}
	for i := 1; i <= 12; i++ {
		loop.Status.Iteration = i
		appendStallEntry(loop, "lp-verify-"+itoa(i), "h"+itoa(i), "check-0", "")
	}
	if len(loop.Status.StallHistory) != 10 {
		t.Fatalf("history len = %d, want 10 (ring cap)", len(loop.Status.StallHistory))
	}
	// The oldest (verify-1, verify-2) evicted; the newest (verify-12) present.
	if loop.Status.StallHistory[0].JobName != "lp-verify-3" {
		t.Fatalf("oldest = %s, want lp-verify-3", loop.Status.StallHistory[0].JobName)
	}
	if loop.Status.StallHistory[9].JobName != "lp-verify-12" {
		t.Fatalf("newest = %s, want lp-verify-12", loop.Status.StallHistory[9].JobName)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
