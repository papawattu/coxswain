package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/papawattu/coxswain/internal/tamper"
)

// f39Repo builds a scratch repo with a base commit and returns its dir and
// the base SHA. mutate edits the working tree; the second return is the
// verified commit made after it.
func f39Repo(t *testing.T, mutate func(dir string)) (dir, base, verified string) {
	t.Helper()
	dir = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	write("pkg/x.go", "package pkg\n")
	write("pkg/x_test.go", "package pkg\n")
	write("go.mod", "module m\n")
	write("README.md", "# r\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	base = git("rev-parse", "HEAD")
	mutate(dir)
	git("add", "-A")
	git("commit", "-q", "-m", "agent")
	return dir, base, git("rev-parse", "HEAD")
}

// f39Run executes the REAL generated tamper script with only the scratch path
// substituted, and returns its exit code and combined output.
func f39Run(t *testing.T, dir, base, verified string, globs []string) (int, string) {
	t.Helper()
	script := strings.ReplaceAll(tamperCheckScript(base, verified, globs), "cd "+verifyScratchPath, "cd "+dir)
	cmd := exec.Command("sh", "-c", script)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	err := cmd.Run()
	if err == nil {
		return 0, out.String()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), out.String()
	}
	t.Fatalf("run: %v", err)
	return -1, ""
}

func f39Edit(rel, content string) func(string) {
	return func(dir string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			panic(err)
		}
	}
}

// TestTamperScriptExecutes (F39): the default go preset (four globs) must
// detect a changed protected path. With the globs newline-joined into one
// pathspec nothing matched and the gate printed "tamper: clean". Mutation:
// re-join the globs with "\n" into one quoted argument and the first two
// cases FAIL (exit 0).
func TestTamperScriptExecutes(t *testing.T) {
	globs := tamper.ExpandPreset(tamper.PresetGo, nil, false)
	if len(globs) < 2 {
		t.Fatalf("the default preset must have several globs, got %v", globs)
	}
	cases := []struct {
		name     string
		mutate   func(string)
		wantExit int
		wantOut  string
	}{
		{"changed _test.go", f39Edit("pkg/x_test.go", "package pkg\n// doctored\n"), 1, "pkg/x_test.go"},
		{"changed go.mod", f39Edit("go.mod", "module m2\n"), 1, "go.mod"},
		{"unrelated file", f39Edit("README.md", "# changed\n"), 0, "tamper: clean"},
		// The D25 cases (previously tested through the now-removed
		// tamper.Changed), now run through the real script.
		{"add root zz_test.go", f39Edit("zz_test.go", "package main\n"), 1, "zz_test.go"},
		{"replace directive in go.mod", f39Edit("go.mod", "module m\n\nreplace m => ./fake\n"), 1, "go.mod"},
		{"add root testdata", f39Edit("testdata/secret.json", "{}"), 1, "testdata/secret.json"},
		{"rename go.mod", func(d string) {
			if err := os.Rename(filepath.Join(d, "go.mod"), filepath.Join(d, "go.mod.bak")); err != nil {
				panic(err)
			}
		}, 1, "go.mod"},
		{"delete pkg/x_test.go", func(d string) {
			if err := os.Remove(filepath.Join(d, "pkg/x_test.go")); err != nil {
				panic(err)
			}
		}, 1, "pkg/x_test.go"},
		{"edit non-protected source", f39Edit("pkg/x.go", "package pkg\n// work\n"), 0, "tamper: clean"},
	}
	for _, tc := range cases {
		dir, base, verified := f39Repo(t, tc.mutate)
		code, out := f39Run(t, dir, base, verified, globs)
		if code != tc.wantExit || !strings.Contains(out, tc.wantOut) {
			t.Errorf("%s: exit=%d output=%q, want exit=%d containing %q", tc.name, code, out, tc.wantExit, tc.wantOut)
		}
	}
}

// TestTamperScriptFailsClosedOnGitError (F39): a failing git diff (a revision
// that does not exist) must not read as clean. The old script piped the diff
// into sed, so sed's exit status hid the failure.
func TestTamperScriptFailsClosedOnGitError(t *testing.T) {
	dir, base, _ := f39Repo(t, f39Edit("README.md", "# c\n"))
	code, out := f39Run(t, dir, base, strings.Repeat("0", 40), tamper.ExpandPreset(tamper.PresetGo, nil, false))
	if code == 0 || strings.Contains(out, "tamper: clean") {
		t.Fatalf("a bad revision must fail closed, got exit=%d output=%q", code, out)
	}
}

// TestTamperScriptNoGlobsIsClean (F39): with nothing protected the script must
// not run a pathspec-less git diff (which would flag every change).
func TestTamperScriptNoGlobsIsClean(t *testing.T) {
	dir, base, verified := f39Repo(t, f39Edit("pkg/x.go", "package pkg\n// agent work\n"))
	// The real expansion paths that yield zero globs (T-1): preset none with
	// no protectedPaths, and an override with none; plus a nil list.
	for name, globs := range map[string][]string{
		"nil":               nil,
		"preset none":       tamper.ExpandPreset(tamper.PresetNone, nil, false),
		"override, no list": tamper.ExpandPreset(tamper.PresetGo, nil, true),
	} {
		code, out := f39Run(t, dir, base, verified, globs)
		if code != 0 || !strings.Contains(out, "tamper: clean (no protected paths)") {
			t.Fatalf("%s: exit=%d output=%q, want explicit clean", name, code, out)
		}
	}
}

// TestTamperScriptCustomGlob (F39): a spec.verify.protectedPaths entry is a
// glob alongside the preset's.
func TestTamperScriptCustomGlob(t *testing.T) {
	globs := tamper.ExpandPreset(tamper.PresetGo, []string{"README.md"}, false)
	dir, base, verified := f39Repo(t, f39Edit("README.md", "# changed\n"))
	code, out := f39Run(t, dir, base, verified, globs)
	if code != 1 || !strings.Contains(out, "README.md") {
		t.Fatalf("custom glob: exit=%d output=%q", code, out)
	}
}

// D79(b): a clean tamper run prints "diffhash: <64 hex>". The same agent diff
// gives the same hash in two repos; a different diff gives a different hash;
// a tampered run prints none.
func TestTamperScriptDiffHash(t *testing.T) {
	globs := tamper.ExpandPreset(tamper.PresetGo, nil, false)
	hashOf := func(out string) string {
		for l := range strings.SplitSeq(out, "\n") {
			if h, ok := strings.CutPrefix(l, "diffhash: "); ok {
				return h
			}
		}
		return ""
	}
	d1, b1, v1 := f39Repo(t, f39Edit("pkg/x.go", "package pkg\n// work A\n"))
	c1, o1 := f39Run(t, d1, b1, v1, globs)
	d2, b2, v2 := f39Repo(t, f39Edit("pkg/x.go", "package pkg\n// work A\n"))
	c2, o2 := f39Run(t, d2, b2, v2, globs)
	d3, b3, v3 := f39Repo(t, f39Edit("pkg/x.go", "package pkg\n// work B\n"))
	c3, o3 := f39Run(t, d3, b3, v3, globs)
	if c1 != 0 || c2 != 0 || c3 != 0 {
		t.Fatalf("clean runs must exit 0: %d %d %d\n%s", c1, c2, c3, o1)
	}
	h1, h2, h3 := hashOf(o1), hashOf(o2), hashOf(o3)
	if len(h1) != 64 {
		t.Fatalf("no 64-hex diffhash line in %q", o1)
	}
	if h1 != h2 {
		t.Errorf("the same agent diff gave different hashes: %s vs %s", h1, h2)
	}
	if h1 == h3 {
		t.Errorf("a different agent diff gave the same hash %s", h1)
	}
	dn, bn, vn := f39Repo(t, f39Edit("pkg/x.go", "package pkg\n// work A\n"))
	if _, on := f39Run(t, dn, bn, vn, nil); hashOf(on) != h1 {
		t.Errorf("the no-globs path must print the same diffhash: %q", on)
	}
	dt, bt, vt := f39Repo(t, f39Edit("pkg/x_test.go", "package pkg\n// doctored\n"))
	if ct, ot := f39Run(t, dt, bt, vt, globs); ct != 1 || hashOf(ot) != "" {
		t.Errorf("a tampered run must exit 1 with no diffhash: exit=%d %q", ct, ot)
	}
}

// D79(b) part 2 step 2: the printed hash is the SHA-256 of the agent diff's
// CONTENT (base..verified, binary), recomputed here from the two commits. If
// the script hashed anything else, this independent recomputation would
// disagree.
func TestTamperScriptDiffHashIsContentOnly(t *testing.T) {
	globs := tamper.ExpandPreset(tamper.PresetGo, nil, false)
	hashOf := func(out string) string {
		for l := range strings.SplitSeq(out, "\n") {
			if h, ok := strings.CutPrefix(l, "diffhash: "); ok {
				return h
			}
		}
		return ""
	}
	d1, b1, v1 := f39Repo(t, f39Edit("pkg/x.go", "package pkg\n// work A\n"))
	c1, o1 := f39Run(t, d1, b1, v1, globs)
	if c1 != 0 {
		t.Fatalf("clean run must exit 0: %d\n%s", c1, o1)
	}
	h1 := hashOf(o1)
	if len(h1) != 64 {
		t.Fatalf("no 64-hex diffhash line in %q", o1)
	}
	var sb bytes.Buffer
	cmd := exec.Command("git", "-C", d1, "diff", "--no-ext-diff", "--no-color", "--binary", b1, v1)
	cmd.Stdout = &sb
	if err := cmd.Run(); err != nil {
		t.Fatalf("recomputing the agent diff: %v", err)
	}
	sum := sha256.Sum256(sb.Bytes())
	want := hex.EncodeToString(sum[:])
	if h1 != want {
		t.Errorf("the printed hash %s is not the sha256 of the diff's content %s", h1, want)
	}
}
