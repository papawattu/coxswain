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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo creates a temp git repo with a base commit containing a non-protected
// source file and the files the fixtures will modify. It returns the repo dir
// and the base commit SHA (HEAD after the first commit). git is configured
// locally so the test never touches the user's global config.
func newRepo(t *testing.T) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	// A non-protected source file (the agent's real work) + an existing
	// protected root-level test file (fixture a will edit it) + a nested one.
	writeFile(t, dir, "pkg/add.go", "package pkg\n\nfunc Add(a, b int) int { return a + b }\n")
	writeFile(t, dir, "pkg/add_test.go", "package pkg\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1, 2) != 3 { t.Fatal(\"no\") } }\n")
	writeFile(t, dir, "add_test.go", "package main\n\nimport \"testing\"\n\nfunc TestRoot(t *testing.T) { if Add(1, 2) != 3 { t.Fatal(\"no\") } }\n")
	writeFile(t, dir, "README.md", "# demo\n")
	writeFile(t, dir, "go.mod", "module demo\n\ngo 1.26\n")
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	sha, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	return dir, strings.TrimSpace(string(sha))
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func removeFile(t *testing.T, dir, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, rel)); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "--allow-empty", "-m", "change")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func headOf(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// Shared literal for the Makefile protected-path fixture (named to keep
// goconst quiet; the value is arbitrary).
const makefileGlob = "Makefile"

// changedGlobs returns the Go-preset globs (no extras) for the fixtures.
func changedGlobs() []string { return ExpandPreset(PresetGo, nil, false) }

// TestChangedGlobsAgainstRealRepo is the D25 acceptance: run the tamper check
// against a real temp git repo. (a)-(g) change a protected path and MUST be
// reported non-empty; (h) changes only a non-protected file and MUST be empty.
// The plain (non-glob) pathspec behavior fails (a), (b), (e) — this test
// requires the `:(glob)` magic.
func TestChangedGlobsAgainstRealRepo(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(t *testing.T, dir string)
		wantEmpty bool
	}{
		{
			name: "(a) edit an existing ROOT-level *_test.go",
			mutate: func(t *testing.T, dir string) {
				// The ROOT-level add_test.go is the one the plain pathspec misses
				// (D25): `**/*_test.go` matches nested but not root under git's
				// default pathspec; `:(glob)**/*_test.go` matches both.
				writeFile(t, dir, "add_test.go", "package main\n\nimport \"testing\"\n\nfunc TestRoot(t *testing.T) { if Add(1, 2) != 999 { t.Fatal(\"doctored\") } }\n")
			},
			wantEmpty: false,
		},
		{
			name: "(b) add a ROOT-level zz_test.go with TestMain",
			mutate: func(t *testing.T, dir string) {
				// A single-package repo keeps tests at the root; this is the
				// root-level case the plain pathspec misses.
				writeFile(t, dir, "zz_test.go",
					"package main\n\nimport \"os\"\nimport \"testing\"\n\nfunc TestMain(m *testing.M) { os.Exit(0) }\n")
			},
			wantEmpty: false,
		},
		{
			name: "(c) add a replace directive to go.mod",
			mutate: func(t *testing.T, dir string) {
				b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, dir, "go.mod", string(b)+"\nreplace demo => ./fake\n")
			},
			wantEmpty: false,
		},
		{
			name: "(d) the same edit under pkg/ (nested *_test.go)",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "pkg/add_test.go", "package pkg\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1, 2) != 42 { t.Fatal(\"doctored\") } }\n")
			},
			wantEmpty: false,
		},
		{
			name: "(e) add testdata/ at the ROOT",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "testdata/secret.json", `{"x":1}`)
			},
			wantEmpty: false,
		},
		{
			name: "(f) rename a protected file",
			mutate: func(t *testing.T, dir string) {
				runGit(t, dir, "mv", "go.mod", "go.mod.bak")
			},
			wantEmpty: false,
		},
		{
			name: "(g) delete a protected file",
			mutate: func(t *testing.T, dir string) {
				removeFile(t, dir, "pkg/add_test.go")
			},
			wantEmpty: false,
		},
		{
			name: "(h) clean change to a non-protected file only",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "pkg/add.go", "package pkg\n\nfunc Add(a, b int) int { return a + b }\n\n// comment\n")
			},
			wantEmpty: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, base := newRepo(t)
			tc.mutate(t, dir)
			commitAll(t, dir)
			verified := headOf(t, dir)
			got, err := Changed(dir, base, verified, changedGlobs())
			if err != nil {
				t.Fatalf("Changed: %v", err)
			}
			if tc.wantEmpty {
				if len(got) != 0 {
					t.Fatalf("expected no protected-path changes, got %v", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("expected a protected-path change to be reported, got empty (the preset missed this path — check the :(glob) magic)")
			}
		})
	}
}

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
