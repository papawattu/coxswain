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

// TestCanaryCheckScriptF94 (F94, owner decision 2026-11-01 (a)+(b)) runs the
// operator's REAL canary script (canaryCheckScript — the SAME function the
// verify Job's canary init runs) against scratch git repos whose tree plays
// the /verify checkout holding BOTH the base and the verified commit
// (clone-base fetched the base; import-agent grafted the verified commit on
// top — the shape the canary runs in, so `ls-tree <BASE_COMMIT>` reads the
// base's tree). It covers the two new branches of the "no *_test.go at HEAD"
// path plus the env contract:
//
//   - (a) tests-removed: the base commit HAD a *_test.go and HEAD deleted it
//     → exit 46 (verifyCanaryTestsRemovedExit). The agent removed every
//     test, so `go test` would pass vacuously — the 44 hold that kept this
//     case in Verifying forever is what stops that subversion; 46 is the
//     terminal signal (the gate mapping lands in session 2).
//   - (b) never-had-tests: neither base nor HEAD has a *_test.go → exit 0
//     (NOT a new non-zero code: a non-zero init exit fails the pod and the
//     acceptance checks would never run) and the termination-message file
//     contains the skip marker `canary-skipped: no-tests-in-repo` (the
//     operator reads the marker ONLY on exit 0 — session 2).
//   - env contract: any of the three F94 env vars unset → the branch falls
//     back to the pre-F94 44 (no 46, no skip marker — the operator cannot
//     tell tests-removed from never-had-tests, and a marker file that is
//     never written would mislead the session-2 reader). The operator
//     always sets all three (the Job builder), so this is the fail-closed
//     path for a hand-edited or pre-F94 container.
//
// The pre-F94 healthy / subverted / 44 / 45 cases are the F60 execution
// test's (TestCanaryCheckScript, loop_f60_canary_script_test.go) and are
// unchanged by F94 (their trees have no .git, so the F94 branches are inert
// and the legacy 44 no-tests case keeps reading 44 — the F60 "unavailable"
// spec is the positive control for that). The (a)/(b) cases never reach `go
// test`, so only git is needed here.
func TestCanaryCheckScriptF94(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git in PATH; the canary script's base ls-tree needs git")
	}

	t.Run("(a) tests-removed: base had a_test.go, HEAD deleted it → 46", func(t *testing.T) {
		verify, baseSHA := f94SetupRepo(t,
			map[string]string{
				"go.mod":    "module demo\n\ngo 1.26\n",
				"main.go":   "package demo\n\nfunc Add(a, b int) int { return a + b }\n",
				"a_test.go": "package demo\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {}\n",
			},
			"base: with tests")
		// The agent's commit DELETES the only test file (git rm), keeping
		// the tree otherwise identical.
		if out, err := exec.Command("git", "-C", verify, "rm", "a_test.go").CombinedOutput(); err != nil {
			t.Fatalf("git rm a_test.go: %v: %s", err, string(out))
		}
		if out, err := exec.Command("git", "-C", verify, "commit", "-m", "head: tests removed").CombinedOutput(); err != nil {
			t.Fatalf("git commit: %v: %s", err, string(out))
		}

		root := t.TempDir()
		term := filepath.Join(root, "termination.log")
		out, code := f94RunCanary(t, verify, baseSHA, term, verifyCanarySkipMarker)
		if code != verifyCanaryTestsRemovedExit {
			t.Fatalf("canary exited %d for a repo whose base had tests and HEAD deleted them; want %d (tests removed, terminal). output: %s", code, verifyCanaryTestsRemovedExit, out)
		}
		if !strings.Contains(out, "tests removed") {
			t.Fatalf("canary output lacks the tests-removed verdict. output: %s", out)
		}
		// 46 must NOT write the skip marker (that is the (b) path only):
		// the session-2 operator reads the marker only on exit 0, but the
		// two exits must be unambiguous on their own.
		if _, err := os.Stat(term); !os.IsNotExist(err) {
			t.Fatalf("the 46 path must not write the termination file (stat err: %v)", err)
		}
	})

	t.Run("(b) never-had-tests: base and HEAD have no *_test.go → 0 + skip marker", func(t *testing.T) {
		verify, baseSHA := f94SetupRepo(t,
			map[string]string{
				"go.mod":  "module demo\n\ngo 1.26\n",
				"main.go": "package demo\n\nfunc Add(a, b int) int { return a + b }\n",
				"app.py":  "print(1)\n",
			},
			"base: no tests")
		// A second commit so base != HEAD (the tamper/import shape: the
		// agent did work, just never with tests).
		f94AddCommit(t, verify, "notes.txt", "some agent work\n", "head: agent work, still no tests")

		root := t.TempDir()
		term := filepath.Join(root, "termination.log")
		out, code := f94RunCanary(t, verify, baseSHA, term, verifyCanarySkipMarker)
		if code != 0 {
			t.Fatalf("canary exited %d for a repo with no tests at HEAD or base; want 0 (skip — a non-zero exit fails the pod and the checks would never run). output: %s", code, out)
		}
		data, err := os.ReadFile(term)
		if err != nil {
			t.Fatalf("the skip-marker termination file was not written: %v (output: %s)", err, out)
		}
		if !strings.Contains(string(data), verifyCanarySkipMarker) {
			t.Fatalf("the termination file lacks the skip marker %q. contents: %s", verifyCanarySkipMarker, string(data))
		}
	})

	t.Run("env contract: BASE_COMMIT unset → 44 (fail closed, no 46, no skip)", func(t *testing.T) {
		verify, baseSHA := f94SetupRepo(t,
			map[string]string{
				"go.mod":    "module demo\n\ngo 1.26\n",
				"main.go":   "package demo\n\nfunc Add(a, b int) int { return a + b }\n",
				"a_test.go": "package demo\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {}\n",
			},
			"base: with tests")
		if out, err := exec.Command("git", "-C", verify, "rm", "a_test.go").CombinedOutput(); err != nil {
			t.Fatalf("git rm a_test.go: %v: %s", err, string(out))
		}
		if out, err := exec.Command("git", "-C", verify, "commit", "-m", "head: tests removed").CombinedOutput(); err != nil {
			t.Fatalf("git commit: %v: %s", err, string(out))
		}

		root := t.TempDir()
		term := filepath.Join(root, "termination.log")
		// baseCommit "" models the env being unset (the script's
		// ${BASE_COMMIT:-} test treats an empty value as missing).
		out, code := f94RunCanary(t, verify, "", term, verifyCanarySkipMarker)
		if code != verifyCanaryUnavailableExit {
			t.Fatalf("canary exited %d with BASE_COMMIT unset and no *_test.go at HEAD; want %d (the fail-closed fallback must be the pre-F94 44 — never a 46 it cannot support)", code, verifyCanaryUnavailableExit)
		}
		if strings.Contains(out, "tests removed") {
			t.Fatalf("the fail-closed fallback must not report tests-removed. output: %s", out)
		}
		if _, err := os.Stat(term); !os.IsNotExist(err) {
			t.Fatalf("the fail-closed path must not write the termination file (stat err: %v)", err)
		}
		_ = baseSHA
	})

	t.Run("env contract: TERMINATION_MESSAGE_PATH/MARKER unset → the pre-F94 44 (no skip marker)", func(t *testing.T) {
		verify, baseSHA := f94SetupRepo(t,
			map[string]string{
				"go.mod":  "module demo\n\ngo 1.26\n",
				"main.go": "package demo\n\nfunc Add(a, b int) int { return a + b }\n",
			},
			"base: no tests")
		f94AddCommit(t, verify, "notes.txt", "agent work\n", "head: still no tests")

		root := t.TempDir()
		term := filepath.Join(root, "termination.log")
		// The path is modelled but the marker env is unset (marker ""):
		// the never-had-tests branch would have to write an UNKNOWN marker,
		// so the whole env group fails closed.
		_, code := f94RunCanary(t, verify, baseSHA, term, "")
		if code != verifyCanaryUnavailableExit {
			t.Fatalf("canary exited %d with TERMINATION_MESSAGE_MARKER unset; want %d (the fail-closed fallback must be the pre-F94 44 — a skip with an unknown marker would mislead the session-2 reader)", code, verifyCanaryUnavailableExit)
		}
		if _, err := os.Stat(term); !os.IsNotExist(err) {
			t.Fatalf("the fail-closed path must not write a half-marker to the termination file (stat err: %v)", err)
		}
	})
}

// f94SetupRepo inits a git repo at a temp dir, commits the given files as the
// base commit (author identity configured locally) and returns the dir and
// the base commit SHA (the BASE_COMMIT env value).
func f94SetupRepo(t *testing.T, files map[string]string, baseMsg string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, string(out))
		}
		return string(out)
	}
	git("init", "-b", "main", ".")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	for name, content := range files {
		writeFile(t, dir, name, content)
		git("add", "--", name)
	}
	git("commit", "-m", baseMsg)
	return dir, strings.TrimSpace(git("rev-parse", "HEAD"))
}

// f94AddCommit writes file (under dir) and commits it with msg.
func f94AddCommit(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	writeFile(t, dir, file, content)
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, string(out))
		}
		return string(out)
	}
	git("add", "--", file)
	git("commit", "-m", msg)
}

// f94RunCanary runs the REAL canaryCheckScript against verifyDir as the
// /verify checkout, with the F94 env (BASE_COMMIT, TERMINATION_MESSAGE_PATH,
// TERMINATION_MESSAGE_MARKER) exported before the script body. The harness
// is copied from runCanaryScript (F60): only the path constants
// (TMPDIR and the cp -a source) are substituted and an empty env value is
// NOT exported (the script's ${VAR:-} test then sees it as unset). Returns
// the combined output and the exit code.
func f94RunCanary(t *testing.T, verifyDir, baseCommit, termPath, marker string) (string, int) {
	t.Helper()
	root := t.TempDir()
	scratch := filepath.Join(root, "tmp")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	script := canaryCheckScript()
	script = strings.ReplaceAll(script, `export TMPDIR="/tmp"`, `export TMPDIR="`+scratch+`"`)
	script = strings.ReplaceAll(script, `cp -a /verify/.`, `cp -a "`+verifyDir+`/."`)
	// The operator sets the F94 env on the canary container; export it (a
	// non-empty value only) before the script so the real body sees it
	// exactly as the container would.
	if baseCommit != "" || termPath != "" || marker != "" {
		script = fmt.Sprintf("export BASE_COMMIT=%q\nexport TERMINATION_MESSAGE_PATH=%q\nexport TERMINATION_MESSAGE_MARKER=%q\n",
			baseCommit, termPath, marker) + script
	}
	// A STABLE, ABSOLUTE, ASCII-safe script path (runCanaryScript's
	// rationale: the t.TempDir() run name can carry spaces, parens and dots;
	// the file is fed to bash through stdin, so the argv stays `bash -s`).
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
	sanRoot := filepath.Join(os.TempDir(), "coxs-f94-canary", safe(root))
	if err := os.MkdirAll(sanRoot, 0o755); err != nil {
		t.Fatalf("mkdir sanRoot: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sanRoot) })
	scriptPath := filepath.Join(sanRoot, "canary.sh")
	mustWriteFile(t, scriptPath, []byte(script))
	in, err := os.Open(scriptPath)
	if err != nil {
		t.Fatalf("open script: %v", err)
	}
	cmd := exec.Command("bash", "-s")
	cmd.Stdin = in
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
