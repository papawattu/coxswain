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

package tamper

import (
	"testing"
)

// Shared literal for the Makefile protected-path fixture (named to keep
// goconst quiet; the value is arbitrary).
const makefileGlob = "Makefile"

// TestExpandPreset pins the preset expansion (D25.1) in one place.
func TestExpandPreset(t *testing.T) {
	t.Run("go preset emits the four go globs with :(glob) magic", func(t *testing.T) {
		g := ExpandPreset(PresetGo, nil, false)
		if len(g) != 4 {
			t.Fatalf("go preset = %v, want 4 globs", g)
		}
		for _, want := range []string{":(glob)**/*_test.go", ":(glob)**/testdata/**", ":(glob)go.mod", ":(glob)go.sum"} {
			found := false
			for _, got := range g {
				if got == want {
					found = true
				}
			}
			if !found {
				t.Errorf("go preset missing %q; got %v", want, g)
			}
		}
	})
	t.Run("extra Makefile is magic-ized to match the root", func(t *testing.T) {
		g := ExpandPreset(PresetGo, []string{makefileGlob}, false)
		found := false
		for _, got := range g {
			if got == ":(glob)"+makefileGlob {
				found = true
			}
		}
		if !found {
			t.Errorf("Makefile extra should become :(glob)Makefile; got %v", g)
		}
	})
	t.Run("an already-magic extra is passed through", func(t *testing.T) {
		g := ExpandPreset(PresetGo, []string{":(top)" + makefileGlob}, false)
		found := false
		for _, got := range g {
			if got == ":(top)"+makefileGlob {
				found = true
			}
		}
		if !found {
			t.Errorf(":(top)Makefile should be passed through; got %v", g)
		}
	})
	t.Run("none preset uses only the extras", func(t *testing.T) {
		g := ExpandPreset(PresetNone, []string{makefileGlob}, false)
		if len(g) != 1 || g[0] != ":(glob)"+makefileGlob {
			t.Errorf("none preset = %v, want [:(glob)Makefile]", g)
		}
	})
	t.Run("override replaces the preset with the extras", func(t *testing.T) {
		g := ExpandPreset(PresetGo, []string{makefileGlob}, true)
		if len(g) != 1 || g[0] != ":(glob)"+makefileGlob {
			t.Errorf("override = %v, want [:(glob)Makefile]", g)
		}
	})
}
