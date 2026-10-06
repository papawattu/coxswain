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

// Package stall is the P2e stall detector's normaliser (TDD-PLAN-PHASE2
// P2e). It is PURE: the raw failing verify-check output in, the normalised
// output + its SHA-256 hex out. It imports nothing from the controller (the
// controller imports this package, never the reverse).
//
// The input is the operator-collected check output: the failing check-*
// container's termination message (the 4 KB tail Kubernetes records in
// status.initContainerStatuses[].lastState.terminated.terminationMessage
// when the container carries a terminationMessagePath) — no pod-log read.
package stall

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// NormalisationVersionV1 is the version of the rule set below (PLAN.md's
// normalisationVersion, item 7). A future v2 is a NEW version: the operator
// compares consecutive hashes for equality WITHIN the same version, and a
// version change resets the consecutive run.
const NormalisationVersionV1 = "v1"

// rule1 applies to every line in two passes (rule 1 is FIRST overall, before
// rule 2 — the rule-order golden pins this):
//
//	rule1DecimalTimestamps: an RFC3339 timestamp (date + T + HH:MM:SS +
//	optional .frac + Z/offset), the Go log format (SLASH-separated date +
//	space + HH:MM:SS), or a LEADING epoch integer.
//	rule1HexHourTimestamps: an RFC3339-form token whose HOUR field is a
//	0x-prefixed hex quad (the rule-order golden: 2026-07-03T0x1e00:00:00Z —
//	a real timestamp parser would reject the 0x1e00 hour; the normaliser's
//	permissive swallow is what makes the rule-1/rule-2 ORDER observable:
//	rule 1 (first) consumes the whole date+T+0x1e00 token, so rule 2 never
//	sees it as a hex address. The 4 hex digits are deliberate: they are
//	EXACTLY at rule 2's minimum (≥4), so under a rule-2-FIRST swap rule 2
//	reaches in and replaces the 0x1e00 mid-token (leaving the date), while
//	the ordered pass (rule 1 first) strips the whole token — the two orders
//	then differ and the rule-order golden pins the order. The decimal pass
//	runs first in the line loop so the decimal timestamp (date + T +
//	HH:MM:SS) is stripped whole, and the hex-hour pass is a no-op there).
//
// Go's RE2 is not backtracking, so the decimal form CANNOT carry an
// optional tail after a class that could match the hex-hour digits (the
// leftmost match wins and the tail is dropped from the match) — hence the
// two-pass split and the hex-hour form matching ONLY date+T+0xff.
var (
	rule1DecimalTimestamps = regexp.MustCompile(
		`\d{4}-\d{2}-\d{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:?[0-9]{2})?|` +
			`\d{4}/\d{2}/\d{2} [0-9]{2}:[0-9]{2}:[0-9]{2}|^[0-9]{9,12}`)
	rule1HexHourTimestamps = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T0x[0-9a-fA-F]{4}`)
)

// rule2HexAddresses strips a 0x-prefixed hex of ≥4 digits (pointers, PC
// addresses) → a fixed 0xADDR placeholder. The regex is a BARE 0x + ≥4 hex
// (no \b word boundary): a hex token embedded in a longer alphanumeric run —
// the rule-order golden's 0x1e00 HOUR inside the date+T+0x1e00 timestamp —
// IS replaced. That is exactly what makes the rule-1/rule-2 ORDER
// observable (the rule-order golden pins it): the ordered pass (rule 1
// first) strips the whole date+T+0x1e00 token, so rule 2 never sees the
// 0x1e00; the swap (rule 2 first) reaches in and replaces the embedded
// 0x1e00 mid-token, leaving the date — the two orders differ, so the swap
// mutation makes the golden FAIL. (A \b boundary would make the swap a no-op
// on the embedded token and hide the order — hence it is deliberately
// absent; a standalone 0xhex≥4 pointer/PC still matches identically.)
var rule2HexAddresses = regexp.MustCompile(`0x[0-9a-fA-F]{4,}`)

// rule3TempPaths strips a temp path: a (backslash-free) path containing
// /tmp/ or /var/tmp/ → the fixed TMPDIR/PATH_PLACEHOLDER. The trailing
// character class excludes whitespace, a backtick, a quote, a closing paren
// and a closing bracket so the match does not swallow the following word.
var rule3TempPaths = regexp.MustCompile(`\S*(?:/tmp/|/var/tmp/)[^\s` + "`'\")" + `\]]*`)

// rule4LineNumbers strips line-number noise: a Go panic/test format
// file.go:123(:4) → file.go:LINE(:COL) (a filename with a DOT, a colon,
// digits, and optionally another colon+digits — the dot requirement keeps
// the bare numbers of a timestamp from matching), and a test-framework
// duration (1.23s) → (DURATIONs).
var rule4LineNumbers = regexp.MustCompile(
	`\b[\w./+-]+(\.\w+)+:\d+(:\d+)?|\(\d+([.,]\d+)?s\)`)

// rule5CommitSHAs strips a 40-hex commit SHA (the per-iteration commit the
// check output names — it DIFFERS every iteration and would otherwise
// defeat the hash, item 7) → a fixed COMMIT placeholder.
var rule5CommitSHAs = regexp.MustCompile(`\b[0-9a-f]{40}\b`)

// rule6VerifyJobNames strips the verify-Job name <loop>-verify-<n> (the
// Job carries the iteration — it differs every re-run, item 7) → a fixed
// VERIFYJOB placeholder.
var rule6VerifyJobNames = regexp.MustCompile(`[\w.-]+-verify-\d+`)

// Normalize applies the seven rules IN ORDER (rule 1 first, then rules
// 2–6; the rule-order golden pins that rule 1 runs before rule 2) and
// returns the normalised output.
func Normalize(raw string) string {
	// Rule 1 (two passes per line; the decimal pass first so a full
	// decimal timestamp is stripped whole, then the hex-hour pass for the
	// 0xff-hour token).
	lines := strings.Split(raw, "\n")
	for i, ln := range lines {
		ln = rule1DecimalTimestamps.ReplaceAllString(ln, "")
		ln = rule1HexHourTimestamps.ReplaceAllString(ln, "")
		lines[i] = ln
	}
	out := strings.Join(lines, "\n")
	// Rules 2-6 (global).
	out = rule2HexAddresses.ReplaceAllString(out, "0xADDR")
	out = rule3TempPaths.ReplaceAllString(out, "TMPDIR/PATH_PLACEHOLDER")
	out = rule4LineNumbers.ReplaceAllStringFunc(out, func(m string) string {
		if m[0] == '(' {
			return "(DURATIONs)"
		}
		// file.go:123:4 → file.go:LINE:COL; file.go:123 → file.go:LINE.
		// The filename ends at the first colon (a Go filename has no colons);
		// everything from that colon onward is line(:col) noise.
		filename, _, found := strings.Cut(m, ":")
		if !found {
			return m
		}
		if strings.Count(m, ":") > 1 {
			return filename + ":LINE:COL"
		}
		return filename + ":LINE"
	})
	out = rule5CommitSHAs.ReplaceAllString(out, "COMMIT")
	out = rule6VerifyJobNames.ReplaceAllString(out, "VERIFYJOB")
	// Rule 7: trim trailing whitespace per line, collapse 3+ consecutive
	// blank lines → 1. The input's trailing-newline is PRESERVED (the
	// byte-identical no-noise property: a newline-terminated input that
	// carries no noise must come out byte-identical, terminator included)
	// — only INTERIOR trailing blank lines collapse.
	hadTrailingNewline := strings.HasSuffix(raw, "\n")
	lines = strings.Split(out, "\n")
	if hadTrailingNewline {
		// the final empty element after the last \n is the terminator,
		// not a blank line: drop it before the collapse, re-add after.
		lines = lines[:len(lines)-1]
	}
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " \t")
	}
	out = strings.Join(lines, "\n")
	out = regexp.MustCompile(`\n{3,}`).ReplaceAllString(out, "\n\n")
	if hadTrailingNewline {
		out += "\n"
	}
	return out
}

// NormalizeWithHash normalises raw and returns the output + its SHA-256 hex
// (the StallEntry hash pair; the hash is over the normalised bytes).
func NormalizeWithHash(raw string) (string, string) {
	out := Normalize(raw)
	sum := sha256.Sum256([]byte(out))
	return out, hex.EncodeToString(sum[:])
}

// StallEntry is the normaliser-side view of one verify-failure iteration's
// record (the controller's api/v1alpha1.StallEntry carries the same fields
// + the iteration/jobName/at the operator stamps; this local type keeps the
// normaliser's test surface self-contained).
type StallEntry struct {
	Hash                 string
	NormalisationVersion string
}
