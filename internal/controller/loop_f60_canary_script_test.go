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

// TestCanaryCheckScript (F60, D17.1) runs the operator's real canary script
// (canaryCheckScript — the SAME function the verify Job's canary init runs)
// against a scratch Go package in a t.TempDir, over the four canary outcomes:
//
//   - healthy: a package with a real passing test. The canary appends its own
//     forced-failure test (zzz_canary_test.go), go test runs the agent's tests
//     (which pass) and then the canary's t.Fatal → go test exits non-zero →
//     the canary exits 0 and prints the token (the proof the honest failure
//     ran).
//   - subverted: the agent's test binary exits 0 regardless of tests (an
//     init() that os.Exit(0)s — the round-8 "binary exits 0 regardless" probe
//     that actually produces exit 0 under modern Go's testing framework,
//     which panics on a per-test os.Exit). The canary exits 43.
//   - unavailable: a package with no *_test.go. The canary exits 44.
//   - build error: a package whose source does not compile. The canary exits
//     45 (an infra failure, never a subversion verdict).
//
// The script cd's into /verify (the verify pod's scratch mount) and runs the
// canary on a copy in /tmp; the test rewrites both /verify and /tmp to the
// scratch dir so the real script runs against the scratch package. It needs a
// Go toolchain in PATH (go test on the copied package).
func TestCanaryCheckScript(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain in PATH; canary script needs `go test`")
	}

	t.Run("healthy: the forced-failure canary test makes go test non-zero", func(t *testing.T) {
		root := t.TempDir()
		verify := filepath.Join(root, "verify")
		os.MkdirAll(verify, 0o755)
		mustWriteFile(t, filepath.Join(verify, "go.mod"), []byte("module demo\n\ngo 1.26\n"))
		writeFile(t, verify, "main.go", "package demo\n\nfunc Add(a, b int) int { return a + b }\n")
		writeFile(t, verify, "main_test.go",
			"package demo\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1, 2) != 3 { t.Fatal(\"bad\") } }\n")

		out, code := runCanaryScript(t, root, verify)
		if code != 0 {
			t.Fatalf("canary exited %d for a healthy package; want 0 (the healthy negative control). output: %s", code, out)
		}
		// The token must be in the log (the proof the honest t.Fatal fired).
		if !strings.Contains(out, "canary-") {
			t.Fatalf("canary output lacks the token (canary-...): %s", out)
		}
		if !strings.Contains(out, "respects failures") {
			t.Fatalf("canary output lacks the healthy verdict. output: %s", out)
		}
	})

	t.Run("subverted: the test binary exits 0 regardless (init os.Exit(0) probe)", func(t *testing.T) {
		root := t.TempDir()
		verify := filepath.Join(root, "verify")
		os.MkdirAll(verify, 0o755)
		mustWriteFile(t, filepath.Join(verify, "go.mod"), []byte("module demo\n\ngo 1.26\n"))
		writeFile(t, verify, "main.go", "package demo\n\nfunc Add(a, b int) int { return a + b }\n")
		// The subversion: a test init() that exits the binary 0 BEFORE any test
		// runs (the ADR's "a binary that exits 0 regardless of tests"). Under
		// modern Go a per-test os.Exit(0) PANICS (exit 1), so init() is the
		// form that actually makes the binary exit 0 — the round-8 probe class.
		writeFile(t, verify, "probe_test.go",
			"package demo\n\nimport \"os\"\n\nfunc init() { os.Exit(0) }\n")

		out, code := runCanaryScript(t, root, verify)
		if code != 43 {
			t.Fatalf("canary exited %d for a subverted package; want 43 (subversion detected). output: %s", code, out)
		}
		if !strings.Contains(out, "SUBVERTED") {
			t.Fatalf("canary output lacks the subverted verdict. output: %s", out)
		}
	})

	t.Run("unavailable: a package with no *_test.go", func(t *testing.T) {
		root := t.TempDir()
		verify := filepath.Join(root, "verify")
		os.MkdirAll(verify, 0o755)
		mustWriteFile(t, filepath.Join(verify, "go.mod"), []byte("module demo\n\ngo 1.26\n"))
		writeFile(t, verify, "main.go", "package demo\n\nfunc Add(a, b int) int { return a + b }\n")

		_, code := runCanaryScript(t, root, verify)
		if code != 44 {
			t.Fatalf("canary exited %d for a package with no tests; want 44 (canary unavailable).", code)
		}
	})

	t.Run("build error: a package that does not compile", func(t *testing.T) {
		root := t.TempDir()
		verify := filepath.Join(root, "verify")
		os.MkdirAll(verify, 0o755)
		mustWriteFile(t, filepath.Join(verify, "go.mod"), []byte("module demo\n\ngo 1.26\n"))
		// A non-test source file with a syntax error: the package does not
		// compile → the canary exits 45 (infra), never 43 (subversion).
		writeFile(t, verify, "main.go", "package demo\n\nfunc Add(a, b int int { return a }\n")
		writeFile(t, verify, "main_test.go", "package demo\n\nimport \"testing\"\nfunc TestAdd(t *testing.T) {}\n")

		_, code := runCanaryScript(t, root, verify)
		if code != 45 {
			t.Fatalf("canary exited %d for a non-compiling package; want 45 (build error, not subversion).", code)
		}
	})
}

// runCanaryScript runs the real canaryCheckScript against a scratch Go
// package: it rewrites the script's /verify (the verify pod's scratch mount)
// to verifyDir and /tmp to a scratch tmp dir under root, writes the rewritten
// script to a file, and runs it with bash. It returns the combined output and
// the exit code.
func runCanaryScript(t *testing.T, root, verifyDir string) (string, int) {
	t.Helper()
	if err := os.MkdirAll(verifyDir, 0o755); err != nil {
		t.Fatalf("mkdir verify: %v", err)
	}
	// A per-run scratch /tmp (the script mkdir's canary-<seed> under it and
	// trap-deletes it; pointing /tmp at a root subdir keeps it contained).
	scratch := filepath.Join(root, "tmp")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	script := canaryCheckScript()
	// The script hardcodes /verify and /tmp; rewrite both to the scratch paths.
	script = strings.ReplaceAll(script, "cd /verify", "cd "+verifyDir)
	script = strings.ReplaceAll(script, "find /verify", "find "+verifyDir)
	script = strings.ReplaceAll(script, "$PKG_DIR/#/verify/", "$PKG_DIR/#"+verifyDir+"/")
	script = strings.ReplaceAll(script, "SCRATCH=/tmp/canary-", "SCRATCH="+scratch+"/canary-")
	// The export TMPDIR=/tmp and the cd /verify at the top: TMPDIR is only a
	// cache hint; leave it (harmless). The package copy uses cp from $PKG_DIR
	// (already rewritten via find/dirname) so no other /verify refs matter.
	scriptPath := filepath.Join(root, "canary.sh")
	mustWriteFile(t, scriptPath, []byte(script))
	out, err := exec.Command("bash", scriptPath).CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if okExitError(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("running canary script: %v (output: %s)", err, string(out))
		}
	}
	return string(out), code
}
