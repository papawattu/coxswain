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

package stall

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The P2e normaliser (TDD-PLAN-PHASE2 P2e): a PURE function — the raw
// failing verify-check output in, the normalised output + SHA-256 hex out.
// The rules (normalisationVersion v1, seven, applied IN ORDER):
//
//  1. Strip timestamps (RFC3339, Go log format, leading epoch) — the
//     timestamp token is removed, the line is kept.
//  2. Strip hex addresses (0x + ≥4 hex digits) → 0xADDR.
//  3. Strip temp paths (a path containing /tmp/ or /var/tmp/) → the
//     volatile suffix is replaced by TMPDIR/PATH_PLACEHOLDER.
//  4. Strip line numbers (file.go:123:4 → file.go:LINE:COL; a test
//     duration (0.42s) → (DURATIONs)).
//  5. Strip 40-hex commit SHAs → COMMIT.
//  6. Strip verify-Job names (<name>-verify-<n>) → VERIFYJOB.
//  7. Collapse blank runs (3+ consecutive blanks → 1; trailing whitespace
//     trimmed per line, trailing blank lines dropped).
//
// Golden files (testdata/<name>.raw + .expected) pin each rule; the
// property specs (same-hash pairs, different-substance) pin the core
// property. The gate mutations (one per rule, plus the rule-1/rule-2
// order swap) are run in a scratch worktree and each makes the named
// spec FAIL — recorded in .samples/p2e/. The normaliser has NO imports
// of the controller (acceptance — the controller imports internal/stall).

// loadPair reads the golden pair <name>.raw (input) + <name>.expected
// (normalised output) from testdata/.
func loadPair(t *testing.T, name string) (raw, expected string) {
	t.Helper()
	rawB, err := os.ReadFile(filepath.Join("testdata", name+".raw"))
	if err != nil {
		t.Fatalf("read %s.raw: %v", name, err)
	}
	var expectedB []byte
	p := filepath.Join("testdata", name+".expected")
	if b, err := os.ReadFile(p); err == nil {
		expectedB = b
	} else if !os.IsNotExist(err) {
		t.Fatalf("read %s.expected: %v", name, err)
	}
	return string(rawB), string(expectedB)
}

// sha256hex is the operator's hash over the normalised output (the
// StallEntry.Hash field): SHA-256, hex-encoded lowercase.
func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestNormalisationVersionV1Value(t *testing.T) {
	// The version pin: the constant is exactly "v1" (the operator compares
	// consecutive hashes within the same version).
	if NormalisationVersionV1 != "v1" {
		t.Fatalf("NormalisationVersionV1 = %q, want \"v1\"", NormalisationVersionV1)
	}
}

func TestGoldenFiles(t *testing.T) {
	for _, n := range []string{"panic-go", "test-fail", "commit-sha", "verify-job-name", "blank-run", "no-noise", "rule-order", "empty"} {
		p := func() [2]string { raw, expected := loadPair(t, n); return [2]string{raw, expected} }()
		t.Run(n, func(t *testing.T) {
			got := Normalize(p[0])
			if got != p[1] {
				t.Fatalf("Normalize(%q) =\n%q\nwant\n%q", p[0], got, p[1])
			}
		})
	}
}

func TestHashIsSHA256OfNormalisedOutput(t *testing.T) {
	// The golden hash: the returned hash is the SHA-256 hex of the
	// normalised output (a deterministic re-computation, not the raw
	// input's hash — the raw input differs from the normalised output for
	// every noisy golden).
	raw, expected := loadPair(t, "panic-go")
	out, hash := NormalizeWithHash(raw)
	if out != expected {
		t.Fatalf("NormalizeWithHash output mismatch")
	}
	if want := sha256hex([]byte(expected)); hash != want {
		t.Fatalf("hash = %q, want sha256hex(normalised) = %q", hash, want)
	}
	// And it is NOT the raw input's hash (the noise was stripped).
	if sha256hex([]byte(raw)) == hash {
		t.Fatalf("hash equals the RAW input's hash — the normalisation stripped nothing")
	}
}

func TestTwoIdenticalAfterNormalise(t *testing.T) {
	// The core property, mutation-covered per rule (the plan's gate
	// mutations, recorded in .samples/p2e/):
	//   - pair A/B differs only in rule-1 + rule-2 + rule-3 noise
	//     (RFC3339 vs Go-log timestamps, a hex address, a /tmp vs
	//     /var/tmp path): removing ANY of rules 1-3 makes A != B.
	//   - pair C/D differs only in rule-4 noise (file:line:col + a temp
	//     dir that also keeps the rule-3 pin):
	//     - removing rule 4 makes C != D (the line numbers differ);
	//     - removing rule 3 makes A != C AND C != D (the /tmp dirs differ),
	//     so rule 3 is also covered by this pair.
	// Rules 5/6 are covered by the same-hash pairs in
	// TestCommitSHA* / TestVerifyJobName*; rule 7 by blank-run + empty.
	for _, pair := range []struct{ a, b string }{
		{"two-identical-after-normalise-a", "two-identical-after-normalise-b"},
		{"two-identical-after-normalise-c", "two-identical-after-normalise-d"},
	} {
		t.Run(pair.a+"+"+pair.b, func(t *testing.T) {
			rawA, _ := loadPair(t, pair.a)
			rawB, _ := loadPair(t, pair.b)
			outA, hashA := NormalizeWithHash(rawA)
			outB, hashB := NormalizeWithHash(rawB)
			if outA != outB {
				t.Fatalf("normalised outputs differ:\nA: %q\nB: %q", outA, outB)
			}
			if hashA != hashB {
				t.Fatalf("hashes differ: A=%s B=%s", hashA, hashB)
			}
		})
	}
}

func TestTwoDifferentSubstance(t *testing.T) {
	// The negative property: two inputs whose SUBSTANCE differs (a
	// different file, a different error message) → different hashes (the
	// normaliser does not over-collapse).
	rawA, _ := loadPair(t, "two-different-substance-a")
	rawB, _ := loadPair(t, "two-different-substance-b")
	_, hashA := NormalizeWithHash(rawA)
	_, hashB := NormalizeWithHash(rawB)
	if hashA == hashB {
		t.Fatalf("different substance produced the SAME hash %s (over-collapse)", hashA)
	}
}

func TestCommitSHASameHashAcrossIterations(t *testing.T) {
	// Item 7 (run-specific): a check output naming a 40-hex commit SHA →
	// COMMIT; a second input with the same substance and a DIFFERENT 40-hex
	// SHA → the same normalised output and the same hash (two iterations on
	// different commits hash equal — the stall fires).
	rawA, _ := loadPair(t, "commit-sha-a")
	rawB, _ := loadPair(t, "commit-sha-b")
	outA, hashA := NormalizeWithHash(rawA)
	outB, hashB := NormalizeWithHash(rawB)
	if outA != outB {
		t.Fatalf("normalised outputs differ:\nA: %q\nB: %q", outA, outB)
	}
	if hashA != hashB {
		t.Fatalf("different commit SHAs produced different hashes: A=%s B=%s", hashA, hashB)
	}
	// And the normalised output carries the COMMIT placeholder.
	if !contains(outA, "COMMIT") {
		t.Fatalf("normalised output does not name COMMIT: %q", outA)
	}
}

func TestVerifyJobNameSameHashAcrossIterations(t *testing.T) {
	// Item 7 (run-specific): a check output naming <loop>-verify-3 →
	// VERIFYJOB; a second input with the same substance and
	// <loop>-verify-4 → the same normalised output and the same hash.
	rawA, _ := loadPair(t, "verify-job-name-a")
	rawB, _ := loadPair(t, "verify-job-name-b")
	outA, hashA := NormalizeWithHash(rawA)
	outB, hashB := NormalizeWithHash(rawB)
	if outA != outB {
		t.Fatalf("normalised outputs differ:\nA: %q\nB: %q", outA, outB)
	}
	if hashA != hashB {
		t.Fatalf("different Job names produced different hashes: A=%s B=%s", hashA, hashB)
	}
	if !contains(outA, "VERIFYJOB") {
		t.Fatalf("normalised output does not name VERIFYJOB: %q", outA)
	}
}

func TestStallEntryCarriesVersion(t *testing.T) {
	// Version pin: a v1 hash is recorded with NormalisationVersion "v1"
	// (the operator compares consecutive hashes within the same version).
	out, hash := NormalizeWithHash("2026-07-03T12:00:00Z panic at 0xdeadbeef\n")
	entry := StallEntry{Hash: hash, NormalisationVersion: NormalisationVersionV1}
	if entry.NormalisationVersion != "v1" {
		t.Fatalf("StallEntry.NormalisationVersion = %q, want v1", entry.NormalisationVersion)
	}
	if out == "" {
		t.Fatalf("normalised output is empty")
	}
}

func TestEmptyInput(t *testing.T) {
	// empty.txt: the defined hash of the empty normalised output.
	out, hash := NormalizeWithHash("")
	if out != "" {
		t.Fatalf("Normalize(\"\") = %q, want empty", out)
	}
	if want := sha256hex([]byte("")); hash != want {
		t.Fatalf("empty hash = %s, want %s", hash, want)
	}
}

func TestNoNoiseIsNoOp(t *testing.T) {
	// no-noise.txt: input with NONE of the noise types → byte-identical
	// output (guards against over-eager stripping).
	raw, expected := loadPair(t, "no-noise")
	if out, _ := NormalizeWithHash(raw); out != expected {
		t.Fatalf("no-noise input was mutated")
	}
	if raw != expected {
		t.Fatalf("the no-noise golden must be byte-identical (raw != expected)")
	}
}

func TestRuleOrderMatters(t *testing.T) {
	// rule-order.txt pins rule 1's permissive timestamp swallow: the raw line
	// 2026-07-03T0x1e:00:00Z carries a 2-digit hex HOUR inside the timestamp
	// token. Rule 1 (first) swallows the whole token (the golden is just
	// 'crash'), and rule 2 (\b + ≥4 hex) never fires on the embedded 0x1e —
	// so the ORDER is pinned by the golden itself: a rule-1 regex that
	// required a decimal hour would leave 0x1e:00:00Z behind, and a rule-2
	// without the \b boundary would replace the 0x1e mid-token in BOTH
	// orders (the swap mutation would then produce the same output and
	// could not fail — the boundary is what makes the swap observable).
	raw, expected := loadPair(t, "rule-order")
	got := Normalize(raw)
	if got != expected {
		t.Fatalf("rule-order golden mismatch (the order pin): got %q want %q", got, expected)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
