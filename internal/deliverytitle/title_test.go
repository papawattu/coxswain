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

package deliverytitle

import (
	"strings"
	"testing"
)

// TestTitle is a table test over the derivation rules: first-line selection,
// the 72-rune cap (71 + "…"), the empty fallback, and special-character pass-
// through. The over-72 case asserts the "coxswain: " prefix is still present.
func TestTitle(t *testing.T) {
	seventyOne := strings.Repeat("a", 71)
	seventyTwo := strings.Repeat("b", 72)
	seventyThree := strings.Repeat("c", 73)
	// unicodeSeventyThree is 73 RUNES (not bytes): each é is a single rune.
	unicodeSeventyThree := strings.Repeat("\u00e9", 73)
	unicodeSeventyTwo := strings.Repeat("\u00e9", 72)

	cases := []struct {
		name string
		goal string
		want string
	}{
		{
			name: "multi-line uses only the first line",
			goal: "Add a login page\n   (and a logout one too)\n",
			want: "coxswain: Add a login page",
		},
		{
			name: "first line trimmed of surrounding whitespace",
			goal: "   Fix the race in cache   \nsecond line",
			want: "coxswain: Fix the race in cache",
		},
		{
			name: "over 72 runes cut to 71 + ellipsis with prefix",
			goal: seventyThree + "\nrest",
			want: "coxswain: " + strings.Repeat("c", 71) + "…",
		},
		{
			name: "unicode over 72 runes cut to 71 runes + ellipsis",
			goal: unicodeSeventyThree,
			want: "coxswain: " + strings.Repeat("\u00e9", 71) + "…",
		},
		{
			name: "exactly 72 runes unchanged",
			goal: seventyTwo,
			want: "coxswain: " + seventyTwo,
		},
		{
			name: "exactly 72 unicode runes unchanged",
			goal: unicodeSeventyTwo,
			want: "coxswain: " + unicodeSeventyTwo,
		},
		{
			name: "71 runes unchanged (just under the cap)",
			goal: seventyOne,
			want: "coxswain: " + seventyOne,
		},
		{
			name: "empty goal falls back",
			goal: "",
			want: "coxswain: implement",
		},
		{
			name: "blank first line falls back",
			goal: "   \nsecond line",
			want: "coxswain: implement",
		},
		{
			name: "whitespace-only first line with tab falls back",
			goal: "\t \t\n",
			want: "coxswain: implement",
		},
		{
			name: "quotes and special characters pass through",
			goal: `Fix <tag> with "quotes" & ampersand's`,
			want: `coxswain: Fix <tag> with "quotes" & ampersand's`,
		},
		{
			name: "leading newline then first real line",
			goal: "\nFirst real line\nMore",
			want: "coxswain: implement",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Title(tc.goal); got != tc.want {
				t.Fatalf("Title(%q) = %q, want %q", tc.goal, got, tc.want)
			}
		})
	}
}
