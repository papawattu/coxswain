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
	"fmt"
	"testing"
)

func int32Ptr(v int32) *int32 {
	p := new(int32)
	*p = v
	return p
}

// Shared commit SHAs for the tamper-evidence table (named to keep goconst
// quiet; they are arbitrary distinct values, not real SHAs).
const (
	commitSame = "aaaa1111"
	commitDiff = "bbbb2222"
)

// TestTamperVerdict is the B2/D24 tri-state seam. The decision is a pure
// function of the operator's OWN evidence (a *int32 tamper exit code + the
// verifiedCommit it names) and returns a tri-state. It never reads the
// runner's result.json claim (I28). The real-git-diff fixtures that produce a
// non-zero tamper code are exercised by the internal/tamper package tests
// (D25); this table covers the decision rule itself.
func TestTamperVerdict(t *testing.T) {
	cases := []struct {
		name        string
		exitCode    *int32
		evidenceCmt string
		verifiedCmt string
		want        TamperVerdict
	}{
		// nil = no evidence (never ran / Job crashed / status lost) -> Unknown,
		// NEVER clean (D24 fail-closed).
		{"nil tamper evidence -> Unknown (not clean)", nil, commitSame, commitSame, TamperUnknown},
		// explicit 0 from a terminated container, bound to the current commit
		// -> Clean (D24; D27 requires a non-empty matching binding).
		{"explicit 0 -> Clean", int32Ptr(0), commitSame, commitSame, TamperClean},
		// non-zero -> Tampered (terminal), the anti-gaming property.
		{"non-zero exit -> Tampered", int32Ptr(1), commitSame, commitSame, TamperTampered},
		{"non-zero exit (code 2) -> Tampered", int32Ptr(2), commitSame, commitSame, TamperTampered},
		// Stale evidence: the evidence names a different verifiedCommit than the
		// current one -> treated as nil (Unknown), even if it is a clean 0
		// (D24.3: evidence from a previous iteration's Job can't be reused).
		{"stale clean evidence (commit mismatch) -> Unknown", int32Ptr(0), commitSame, commitDiff, TamperUnknown},
		{"stale tampered evidence (commit mismatch) -> Unknown", int32Ptr(1), commitSame, commitDiff, TamperUnknown},
		// Same commit -> the evidence is current; the code decides.
		{"current commit, 0 -> Clean", int32Ptr(0), commitSame, commitSame, TamperClean},
		{"current commit, non-zero -> Tampered", int32Ptr(1), commitSame, commitSame, TamperTampered},
		// D27 fail-closed on a missing binding: an empty commit on EITHER side
		// means the evidence isn't bound to a known current commit -> Unknown
		// (never Clean, never Tampered-advancing).
		{"empty evidence commit -> Unknown (D27)", int32Ptr(1), "", commitSame, TamperUnknown},
		{"empty current commit -> Unknown (D27)", int32Ptr(1), commitSame, "", TamperUnknown},
		{"both commits empty -> Unknown (D27)", int32Ptr(0), "", "", TamperUnknown},
		// Clean 0 with an empty binding is NOT clean (D27: fail-closed, not the
		// old skip-when-empty behavior).
		{"empty evidence commit, code 0 -> Unknown (D27)", int32Ptr(0), "", commitSame, TamperUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tamperVerdict(tc.exitCode, tc.evidenceCmt, tc.verifiedCmt)
			if got != tc.want {
				t.Errorf("tamperVerdict(%v, %q, %q) = %v, want %v",
					desc(tc.exitCode), tc.evidenceCmt, tc.verifiedCmt, got, tc.want)
			}
		})
	}
}

// TestTamperVerdictNonZeroIsTerminalRegardlessOfEvidenceCommit pins the D24
// acceptance: non-zero tamper evidence is terminal for the SAME verifiedCommit,
// and nil/unknown never advances. The "regardless of claim" property is proven
// at the envtest seam (the runner claims done, the evidence says tampered ->
// Failed), so this test no longer takes a claim (I28).
func TestTamperVerdictNonZeroIsTerminal(t *testing.T) {
	if got := tamperVerdict(int32Ptr(1), commitSame, commitSame); got != TamperTampered {
		t.Fatalf("non-zero current-commit evidence must be Tampered, got %v", got)
	}
	if got := tamperVerdict(nil, commitSame, commitSame); got != TamperUnknown {
		t.Fatalf("nil evidence must be Unknown (not clean), got %v", got)
	}
	if got := tamperVerdict(int32Ptr(0), commitSame, commitSame); got != TamperClean {
		t.Fatalf("explicit 0 current-commit evidence must be Clean, got %v", got)
	}
}

func desc(p *int32) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprint(*p)
}
