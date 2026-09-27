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
	"testing"
)

// TestTamperVerdict is the B2 (D10) anti-gaming seam. The tamper decision must
// be driven ONLY by the tamper-check container's exit code (the operator's
// own evidence from the two pinned SHAs), never by the runner's result.json
// claim. A naive implementation that lets resultClaimsSuccess override the
// exit code fails the anti-gaming cases below.
func TestTamperVerdict(t *testing.T) {
	cases := []struct {
		name             string
		tamperExitCode   int
		resultClaimsOK   bool
		wantTampered     bool
		wantTerminalReas string
	}{
		// A protected path changed (tamper container non-zero) — the three D10
		// fixtures all surface this way in a real Job — and the runner CLAIMS
		// success. Anti-gaming: it must still be TamperedVerify.
		{"edit existing *_test.go, runner claims success", 1, true, true, TamperedVerifyReason},
		{"add new *_test.go with TestMain->os.Exit(0), runner claims success", 1, true, true, TamperedVerifyReason},
		{"add replace directive to go.mod, runner claims success", 2, true, true, TamperedVerifyReason},
		// Tamper container clean (0) — no protected path changed. Not tampered,
		// even though the runner also claims success (the verify proceeds to the
		// check containers, B3).
		{"clean diff, runner claims success", 0, true, false, ""},
		// Clean diff but the runner reports a BLOCKED/failed run — still not a
		// tamper verdict; the decision never reads the claim either way.
		{"clean diff, runner claims blocked", 0, false, false, ""},
		// Non-zero exit code is tampered regardless of the claim.
		{"non-zero exit, runner claims blocked", 1, false, true, TamperedVerifyReason},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTampered, gotReason := tamperVerdict(tc.tamperExitCode, tc.resultClaimsOK)
			if gotTampered != tc.wantTampered {
				t.Errorf("tamperVerdict(%d, %v) tampered = %v, want %v",
					tc.tamperExitCode, tc.resultClaimsOK, gotTampered, tc.wantTampered)
			}
			if gotReason != tc.wantTerminalReas {
				t.Errorf("tamperVerdict(%d, %v) terminalReason = %q, want %q",
					tc.tamperExitCode, tc.resultClaimsOK, gotReason, tc.wantTerminalReas)
			}
		})
	}
}

// TestTamperVerdictIgnoresClaim is the core anti-gaming assertion, stated
// directly: for a non-zero tamper exit code the verdict is TamperedVerify no
// matter what the claim is. This pins the D10 property that result.json (which
// the agent can forge, sharing the runner's shell) is never a gate.
func TestTamperVerdictIgnoresClaim(t *testing.T) {
	claimSuccess := true
	claimBlocked := false
	// Same exit code, opposite claims — the verdict must be identical.
	a, reasonA := tamperVerdict(1, claimSuccess)
	b, reasonB := tamperVerdict(1, claimBlocked)
	if a != true || b != true {
		t.Fatalf("a non-zero tamper exit must always be tampered, got %v / %v", a, b)
	}
	if reasonA != TamperedVerifyReason || reasonB != TamperedVerifyReason {
		t.Fatalf("a non-zero tamper exit must always be %s, got %q / %q",
			TamperedVerifyReason, reasonA, reasonB)
	}
}
