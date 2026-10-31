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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCanaryCheckScript (F60, D17.1) runs the operator's REAL canary script
// (canaryCheckScript — the SAME function the verify Job's canary init runs)
// against scratch Go trees in t.TempDir. Per AGENTS the execution test runs
// the real generated script with only the path constants substituted: the
// script's /verify (the verify pod's read-only checkout mount) and /tmp (its
// bounded writable scratch) are rewritten to scratch dirs. Everything else —
// the find, the cp -a of the WHOLE tree, the per-package injection, the
// per-package go test -json run, the token decision — is the real script.
//
// The scenarios cover the F60 fixes and the pre-fix defects the reviewer
// observed outside the cluster:
//
//   - healthy root package → 0 (the forced t.Fatal ran and was reported);
//   - healthy NESTED package (pkg/add, go.mod at the tree root) → 0 (the
//     pre-fix defect: the old script copied only the package dir without the
//     module root, go test failed with "go.mod file not found", and that
//     non-zero exit READ AS HEALTHY);
//   - subverted NESTED package (init() os.Exit(0) probe in pkg/add) → 43
//     (pre-fix: also read as healthy, for the same go.mod reason);
//   - DECOY: an honest aaa/ package plus a subverted root package → 43
//     (the pre-fix defect: one combined go test across packages, and the
//     honest package's canary failure made the exit non-zero → healthy);
//   - no-tests SPOOF: a probe that prints "testing: warning: no tests to
//     run" and exits 0 → 43, not 44 (the pre-fix defect: the script grepped
//     go test's output for that string and exited 44);
//   - build-broken package → 45 (a go test -json build-fail for THAT
//     package is infra, never subversion);
//   - no *_test.go anywhere → 44 (not applicable);
//   - a package clause after a leading comment → still detected (the
//     pre-fix head -1-of-the-test-file broke on leading comments).
//
// It needs a Go toolchain in PATH (go test on the copied tree).
func TestCanaryCheckScript(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain in PATH; canary script needs `go test`")
	}

	const goMod = "module demo\n\ngo 1.26\n"

	t.Run("healthy: root package, the forced-failure canary test is honestly reported", func(t *testing.T) {
		verify := t.TempDir()
		writeFile(t, verify, "go.mod", goMod)
		writeFile(t, verify, "main.go", "package demo\n\nfunc Add(a, b int) int { return a + b }\n")
		writeFile(t, verify, "main_test.go",
			"package demo\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1, 2) != 3 { t.Fatal(\"bad\") } }\n")

		out, code := runCanaryScript(t, verify)
		if code != 0 {
			t.Fatalf("canary exited %d for a healthy package; want 0 (the healthy negative control). output: %s", code, out)
		}
		if !strings.Contains(out, "OK — every tested package") {
			t.Fatalf("canary output lacks the healthy verdict. output: %s", out)
		}
	})

	t.Run("healthy nested: tests in pkg/add, go.mod at the tree root", func(t *testing.T) {
		// The reviewer's scenario A: the tests are NOT at the tree root. The
		// pre-fix script copied only pkg/add (no go.mod) and read the
		// resulting "go.mod file not found" non-zero exit as healthy — even
		// though nothing actually ran.
		verify := t.TempDir()
		writeFile(t, verify, "go.mod", goMod)
		writeFile(t, verify, "pkg/add/add.go", "package add\n\nfunc Add(a, b int) int { return a + b }\n")
		writeFile(t, verify, "pkg/add/add_test.go",
			"package add\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1, 2) != 3 { t.Fatal(\"x\") } }\n")

		out, code := runCanaryScript(t, verify)
		if code != 0 {
			t.Fatalf("canary exited %d for a healthy nested package; want 0 (the healthy negative control). output: %s", code, out)
		}
		if !strings.Contains(out, "OK — every tested package") {
			t.Fatalf("canary output lacks the healthy verdict. output: %s", out)
		}
	})

	t.Run("subverted nested: init os.Exit(0) probe in pkg/add", func(t *testing.T) {
		// The reviewer's scenario B: the round-8 probe in a NESTED package.
		// The init() os.Exit(0) runs when the canary's test binary is
		// compiled — it kills the process before the canary test fires, so
		// go test exits 0 despite the forced t.Fatal → 43.
		verify := t.TempDir()
		writeFile(t, verify, "go.mod", goMod)
		writeFile(t, verify, "pkg/add/add.go", "package add\n\nfunc Add(a, b int) int { return a + b }\n")
		writeFile(t, verify, "pkg/add/probe_test.go",
			"package add\n\nimport \"os\"\n\nfunc init() { os.Exit(0) }\n")

		out, code := runCanaryScript(t, verify)
		if code != 43 {
			t.Fatalf("canary exited %d for a subverted nested package; want 43 (subversion detected). output: %s", code, out)
		}
		if !strings.Contains(out, "SUBVERTED") {
			t.Fatalf("canary output lacks the subverted verdict. output: %s", out)
		}
	})

	t.Run("decoy: honest aaa/ plus a subverted root package", func(t *testing.T) {
		// The reviewer's scenario C: the canary tests EVERY package, so the
		// subverted root package cannot hide behind the honest aaa/ one.
		// Pre-fix, one combined go test across packages: aaa's honest canary
		// failure made the exit non-zero → healthy, and the subverted root
		// was never judged.
		verify := t.TempDir()
		writeFile(t, verify, "go.mod", goMod)
		writeFile(t, verify, "aaa/a.go", "package aaa\n\nfunc G() int { return 2 }\n")
		writeFile(t, verify, "aaa/a_test.go",
			"package aaa\n\nimport \"testing\"\n\nfunc TestG(t *testing.T) {}\n")
		writeFile(t, verify, "demo.go", "package demo\n\nfunc F() int { return 1 }\n")
		writeFile(t, verify, "probe_test.go",
			"package demo\n\nimport \"os\"\n\nfunc init() { os.Exit(0) }\n")

		out, code := runCanaryScript(t, verify)
		if code != 43 {
			t.Fatalf("canary exited %d for the decoy case; want 43 (the subverted root package must not hide behind honest aaa/). output: %s", code, out)
		}
		if !strings.Contains(out, "SUBVERTED") {
			t.Fatalf("canary output lacks the subverted verdict. output: %s", out)
		}
	})

	t.Run("no-tests spoof: a probe printing \"no tests to run\" and exiting 0 is 43, not 44", func(t *testing.T) {
		// The reviewer's scenario D: the pre-fix script grepped go test's
		// output for "no tests to run" and exited 44 — spoofable by a probe
		// that prints the line and os.Exit(0)s. The token-based decision
		// ignores the string: the package exited 0 despite the forced
		// t.Fatal → 43.
		verify := t.TempDir()
		writeFile(t, verify, "go.mod", goMod)
		writeFile(t, verify, "demo.go", "package demo\n\nfunc F() int { return 1 }\n")
		writeFile(t, verify, "probe_test.go",
			"package demo\n\nimport (\"fmt\";\"os\")\n\nfunc init() { fmt.Println(\"testing: warning: no tests to run\"); os.Exit(0) }\n")

		out, code := runCanaryScript(t, verify)
		if code != 43 {
			t.Fatalf("canary exited %d for the no-tests spoof; want 43 (the string is go test's output, not a decision). output: %s", code, out)
		}
		if !strings.Contains(out, "SUBVERTED") {
			t.Fatalf("canary output lacks the subverted verdict. output: %s", out)
		}
	})

	t.Run("build error: a package that does not compile", func(t *testing.T) {
		// A syntax error in a package's source: go test reports a build-fail
		// event for THAT package → 45 (infra, never subversion).
		verify := t.TempDir()
		writeFile(t, verify, "go.mod", goMod)
		writeFile(t, verify, "main.go", "package demo\n\nfunc Add(a, b int int { return a }\n")
		writeFile(t, verify, "main_test.go", "package demo\n\nimport \"testing\"\nfunc TestAdd(t *testing.T) {}\n")

		_, code := runCanaryScript(t, verify)
		if code != 45 {
			t.Fatalf("canary exited %d for a non-compiling package; want 45 (build error, not subversion).", code)
		}
	})

	t.Run("build-error string spoof: a probe printing the build-failed marker and exiting 0 is 43, not 45", func(t *testing.T) {
		// The pre-F60 build-error grep read a "[build failed]" string as
		// infra (45) — spoofable by a probe that prints the line and
		// os.Exit(0)s. The token-based decision ignores the string: the
		// package exited 0 despite the forced t.Fatal, so it is 43 (the
		// go toolchain's own -json build-fail event is the only 45
		// trigger now).
		verify := t.TempDir()
		writeFile(t, verify, "go.mod", goMod)
		writeFile(t, verify, "demo.go", "package demo\n\nfunc F() int { return 1 }\n")
		writeFile(t, verify, "probe_test.go",
			"package demo\n\nimport (\"fmt\";\"os\")\n\nfunc init() { fmt.Println(\"[build failed]\"); os.Exit(0) }\n")

		out, code := runCanaryScript(t, verify)
		if code != 43 {
			t.Fatalf("canary exited %d for the build-error-string spoof; want 43 (the string is not the go toolchain's build-fail event). output: %s", code, out)
		}
		if !strings.Contains(out, "SUBVERTED") {
			t.Fatalf("canary output lacks the subverted verdict. output: %s", out)
		}
	})

	t.Run("unavailable: no *_test.go anywhere", func(t *testing.T) {
		// The reviewer's scenario E (a Python repo) reduced to its Go
		// property: no *_test.go anywhere → 44, the operator records a
		// VerifyCanaryUnavailable event and takes no decision.
		verify := t.TempDir()
		writeFile(t, verify, "go.mod", goMod)
		writeFile(t, verify, "main.go", "package demo\n\nfunc Add(a, b int) int { return a + b }\n")
		writeFile(t, verify, "app.py", "print(1)\n")

		_, code := runCanaryScript(t, verify)
		if code != 44 {
			t.Fatalf("canary exited %d for a tree with no *_test.go; want 44 (canary unavailable).", code)
		}
	})

	t.Run("package clause after a leading comment", func(t *testing.T) {
		// The pre-fix script took head -1 of the first test file, which is a
		// comment here — no package clause found → 44. The clause detection
		// scans for the first ^package line instead.
		verify := t.TempDir()
		writeFile(t, verify, "go.mod", goMod)
		writeFile(t, verify, "main.go", "package demo\n\nfunc Add(a, b int) int { return a + b }\n")
		writeFile(t, verify, "main_test.go",
			"// demo tests: leading comment, then the package clause.\n// A second leading comment line.\npackage demo\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {}\n")

		out, code := runCanaryScript(t, verify)
		if code != 0 {
			t.Fatalf("canary exited %d for a package clause after a leading comment; want 0. output: %s", code, out)
		}
	})
}

// runCanaryScript runs the REAL canaryCheckScript against a scratch Go tree:
// it rewrites the script's path constants — /verify (the verify pod's
// read-only checkout mount), /tmp (its bounded writable scratch), and the
// TMPDIR cache hint — to dirs under t.TempDir() (whose own /tmp prefix is
// Go's, not the script's), writes the rewritten script to a file, and runs
// it with bash. It returns the combined output and the exit code.
func runCanaryScript(t *testing.T, verifyDir string) (string, int) {
	t.Helper()
	if err := os.MkdirAll(verifyDir, 0o755); err != nil {
		t.Fatalf("mkdir verify: %v", err)
	}
	// A per-run scratch /tmp (the script mkdir's canary-<seed> under it and
	// trap-deletes it; pointing /tmp at a root subdir keeps it contained).
	root := t.TempDir()
	scratch := filepath.Join(root, "tmp")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	script := canaryCheckScript()
	// Only the path constants are substituted (AGENTS: the execution test
	// runs the real generated script with only path or host constants
	// substituted). TMPDIR is the go toolchain's cache hint (its build
	// cache lands there), so it is a path constant too.
	script = strings.ReplaceAll(script, `export TMPDIR="/tmp"`, `export TMPDIR="`+scratch+`"`)
	// The script's `cp -a /verify/. "$SCRATCH/"` is the only line that needs
	// its source path rewritten to the test tree (verifyDir). Substituting
	// `cp -a /verify/.` with `cp -a "DIR/."` quotes the source (verifyDir may
	// carry spaces or parens from the t.TempDir() run name) and keeps the
	// same `/.` contents-copy semantics: the tree's contents land at the
	// TOP of $SCRATCH (an unquoted `cp -a DIR "$SCRATCH/"` would copy DIR as
	// a subdirectory, nesting the tree one level deep). Only the path
	// constant is substituted; the script body is unchanged (AGENTS).
	script = strings.ReplaceAll(script, `cp -a /verify/.`, `cp -a "`+verifyDir+`/."`)

	// Verify no UNQUOTED /verify or /tmp mention remains (the two quoted
	// substitutions above now wrap their values in double quotes, so the
	// only remaining mentions are inside quoted strings). A mention outside
	// quotes would be a script-body path constant the test did not rewrite.
	for line := range strings.SplitSeq(script, "\n") {
		if strings.Contains(line, "/verify") &&
			!strings.Contains(line, verifyDir) &&
			!strings.Contains(line, `"/`) {
			t.Fatalf("unrewritten /verify left in the script: %s", line)
		}
		if strings.Contains(line, "/tmp") &&
			!strings.Contains(line, scratch) &&
			!strings.HasPrefix(root, "/tmp/") &&
			!strings.Contains(line, `"/`) {
			t.Fatalf("unrewritten /tmp left in the script: %s", line)
		}
	}
	// The script must be written to a path the EXECUTOR can open: the run
	// name under t.TempDir() can carry spaces, parens and dots, and the
	// script's own shell (dash/bash) mangles a command line at a space and
	// ends a line at some of the rest. Derive a STABLE, ABSOLUTE, ASCII-safe
	// dir under os.TempDir() (NOT a sanitized relative path: replacing ""
	// with "" drops the leading "/", which made the old script path
	// relative and landed in the test's CWD, leaking a junk dir per run).
	// t.Cleanup removes it; the substituted scratch paths above keep their
	// real characters.
	safe := func(s string) string {
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			c := s[i]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
				b.WriteByte(c)
				continue
			}
			fmt.Fprintf(&b, "_%02x", c)
		}
		return b.String()
	}
	sanRoot := filepath.Join(os.TempDir(), "coxs-f60-canary", safe(root)+"_"+t.Name())
	if err := os.MkdirAll(sanRoot, 0o755); err != nil {
		t.Fatalf("mkdir sanRoot: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(sanRoot); err != nil {
			t.Errorf("remove sanRoot: %v", err)
		}
	})
	scriptPath := filepath.Join(sanRoot, "canary.sh")
	mustWriteFile(t, scriptPath, []byte(script))
	// Run the script with a FIXED argv: the substituted paths (which may
	// carry spaces or parens from the t.TempDir() run name) land in the FILE,
	// and a command like `bash -c 'sh /path' "$SCRIPT"` would put them in the
	// argv, which the shell re-parses as words (an `export TMPDIR=/tmp/...(
	// ...' line with parens is a syntax error). Instead: the script file is
	// written with an ABSOLUTE safe path (sanRoot above) and is fed to bash
	// through stdin — the argv is `bash -s`, and bash reads the whole file
	// from stdin without re-parsing path words. (Equivalently the script
	// could be a symlink with a safe name; stdin redirect is simplest and
	// keeps the real path constants intact in the file.)
	in, err := os.Open(scriptPath)
	if err != nil {
		t.Fatalf("open script: %v", err)
	}
	cmd := exec.Command("bash", "-s")
	cmd.Stdin = in
	cmd.Stdout = nil
	outb, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if okExitError(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("running canary script: %v (output: %s)", err, string(outb))
		}
	}
	return string(outb), code
}
