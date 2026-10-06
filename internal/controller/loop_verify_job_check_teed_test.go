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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckTeedExecutesUnderSh (P2e reviewer P1, PR #82 review 5423900772)
// runs the script checkTeed generates with the production shell, /bin/sh
// (verifySh). The container's /bin/sh is dash (alpine) or busybox sh — bash
// only by exception — so the wrapper MUST be POSIX sh, and the user command
// must be shell-quoted exactly once (the earlier version double-quoted with
// %q and used ${PIPESTATUS[0]}, which is a bad substitution under dash (exit
// 2) and runs the whole command as one word under bash (exit 127), so EVERY
// check failed). envtest never executes the script, which is why CI is green
// while the pod would fail; this is the execution test the norm "any shell
// script embedded in Go has an execution test" requires.
//
// The test writes the generated command to a file and runs it with /bin/sh,
// substituting ONLY the operator's verifyTerminationLogPath constant with a
// temp file (the path is otherwise an operator constant the test cannot
// write). The exit code the script produces is the user command's exit code
// (the verify evidence); the temp file holds the user command's stdout+stderr
// (the terminationMessage the stall detector hashes).
func TestCheckTeedExecutesUnderSh(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "termination.log")

	// runScript executes `sh -c <generated>` and returns (exitCode, logBytes).
	// The generated command is a complete sh -c invocation (the verify Job
	// container's argv is [verifySh, -c, generated]); writing it to a file and
	// running it with /bin/sh -f mirrors the container's exec.
	runScript := func(t *testing.T, generated string) (int, []byte) {
		t.Helper()
		// Substitute the operator log-path constant with the temp file (the
		// ONLY substitution — the test does not touch the user command or the
		// quoting).
		generated = strings.ReplaceAll(generated, verifyTerminationLogPath, logPath)
		scriptPath := filepath.Join(t.TempDir(), "check.sh")
		mustWriteFile(t, scriptPath, []byte(generated+"\n"))
		// Run under the production shell (/bin/sh), not bash. The file is the
		// whole `sh -c '...' > log 2>&1; rc=$?; ...` line; running it as a
		// script (sh scriptPath) parses the outer line, whose first word
		// (`/bin/sh`) re-invokes sh for the inner -c, exactly as the container
		// does.
		cmd := exec.Command(verifySh, scriptPath)
		out, err := cmd.CombinedOutput()
		_ = out // the script's stdout is the cat'd log; the file holds the evidence
		code := 0
		if err != nil {
			var exitErr *exec.ExitError
			if okExitError(err, &exitErr) {
				code = exitErr.ExitCode()
			} else {
				t.Fatalf("unexpected error running check script (command: %s): %v\noutput: %s", generated, err, string(out))
			}
		}
		logBytes, lerr := os.ReadFile(logPath)
		if lerr != nil {
			t.Fatalf("could not read the termination log at %s: %v", logPath, lerr)
		}
		return code, logBytes
	}

	t.Run("true exits 0", func(t *testing.T) {
		code, log := runScript(t, checkTeed("true", verifyTerminationLogPath))
		if code != 0 {
			t.Fatalf("`true` must exit 0, got %d (log: %s)", code, log)
		}
	})

	t.Run("exit 3 exits 3", func(t *testing.T) {
		code, _ := runScript(t, checkTeed("exit 3", verifyTerminationLogPath))
		if code != 3 {
			t.Fatalf("`exit 3` must exit 3, got %d (a bad substitution under sh is exit 2; a double-quoted command is 127)", code)
		}
	})

	t.Run("failing check with output exits 1 and logs both streams", func(t *testing.T) {
		code, log := runScript(t, checkTeed("echo out; echo err >&2; false", verifyTerminationLogPath))
		if code != 1 {
			t.Fatalf("`echo out; echo err >&2; false` must exit 1 (the user command's code), got %d", code)
		}
		// The log (the terminationMessage) holds BOTH the stdout line and the
		// stderr line — the normaliser's input.
		if !strings.Contains(string(log), "out") {
			t.Fatalf("the termination log must contain the stdout line `out`, got: %s", log)
		}
		if !strings.Contains(string(log), "err") {
			t.Fatalf("the termination log must contain the stderr line `err`, got: %s", log)
		}
	})

	t.Run("command with spaces and single quotes exits 0", func(t *testing.T) {
		code, _ := runScript(t, checkTeed(`test 'a b' = 'a b'`, verifyTerminationLogPath))
		if code != 0 {
			t.Fatalf("a command with spaces and single quotes (`test 'a b' = 'a b'`) must exit 0; a double-quoted command runs it as one word (127) — got %d", code)
		}
	})
}
