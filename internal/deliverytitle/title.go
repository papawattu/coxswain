/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package deliverytitle derives the delivery PR title (and the matching
// runner implement-commit subject) from a Loop's spec.goal. It is a pure,
// operator-side helper: the operator computes it once and hands the result to
// both the runner (the COX_DELIVERY_TITLE env) and the deliver Job (the
// PR_TITLE script var) so the commit subject and the PR title are identical.
package deliverytitle

import (
	"strings"
	"unicode/utf8"
)

// titleMaxRunes is the first-line cap before the derived title is truncated.
const titleMaxRunes = 72

// Title derives the delivery PR title from goal. It takes the first line of
// goal (split on "\n"), trims surrounding whitespace, and returns:
//   - "coxswain: implement" when the trimmed first line is empty;
//   - otherwise "coxswain: " + that line, where an over-long line is cut to
//     its first titleMaxRunes-1 runes and suffixed with "…" (the "coxswain: "
//     prefix is still present).
//
// Special characters (quotes, <, >, &) pass through unchanged.
func Title(goal string) string {
	line := goal
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "coxswain: implement"
	}
	if utf8.RuneCountInString(line) > titleMaxRunes {
		runes := []rune(line)
		line = string(runes[:titleMaxRunes-1]) + "…"
	}
	return "coxswain: " + line
}
