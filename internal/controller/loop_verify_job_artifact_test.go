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

// TestArtifactScriptRejectsSelfCommittedBinary (I47, REVIEW-PHASE1-R20 PR #55)
// runs the operator's artifact check script against a scratch git repo in a
// t.TempDir: the AGENT commits a binary (the P1 the runner's commitWorkspace
// filter misses — commitWorkspace only inspects UNCOMMITTED changes, so a
// self-committed binary is never re-filtered). The script must exit non-zero
// and name the offending path. A clean source-only commit must exit 0.
//
// The script is the SAME function the verify Job's artifact container runs
// (artifactScript), so the test exercises the real script against a real git
// repo — not a copy. The script cd's into /verify (the verify pod's scratch
// mount); the test rewrites that path to the temp dir's repo so the script
// runs against the scratch repo. The NUL-delimited pipe (read -d ”) and the
// od hex-word NUL detection run under bash (the container's busybox ash
// supports read -d ”; the system dash sh does not, so the test uses bash to
// match the container's POSIX-sh-with-ash-semantics).
func TestArtifactScriptRejectsSelfCommittedBinary(t *testing.T) {
	repo := t.TempDir()
	// git init the scratch repo (the /verify stand-in).
	mustGit(t, repo, "init", "-q", "-b", "main")
	mustGit(t, repo, "config", "user.email", "agent@coxswain.test")
	mustGit(t, repo, "config", "user.name", "Agent")

	// baseCommit: a clean source-only file.
	writeFile(t, repo, "round.go", "package main\n\nfunc main() {}\n")
	mustGit(t, repo, "add", "round.go")
	mustGit(t, repo, "commit", "-q", "-m", "base")
	baseSHA := gitSHA(t, repo)

	// The AGENT commits a binary (the P1): a NUL byte in the first 8 KiB.
	// A small binary (<< 1 MiB) so the SIZE cap does not fire — the BINARY
	// detection (NUL byte) must be what trips the check.
	binPath := filepath.Join(repo, "gocli")
	binContent := make([]byte, 4096)
	binContent[0] = '\x7f' // ELF magic (a real binary starts with NUL/ELF bytes)
	binContent[1] = 0x00
	mustWriteFile(t, binPath, binContent)
	// The agent runs 'git add -A && git commit' (the S5a run's agent commit
	// 3442307 'Add sum command...'): the binary rides into the verified commit.
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-q", "-m", "agent: add sum command (self-committed)")
	verifySHA := gitSHA(t, repo)

	// The artifact script (the SAME function the verify Job's artifact
	// container runs), rewritten to the scratch repo: cd /verify -> cd $repo,
	// TMPDIR=/verify -> TMPDIR=$repo (the writable scratch).
	script := artifactScript(baseSHA, verifySHA)
	script = strings.ReplaceAll(script, "cd /verify", "cd "+repo)
	script = strings.ReplaceAll(script, "TMPDIR=/verify", "TMPDIR="+repo)
	scriptPath := filepath.Join(repo, "artifact-check.sh")
	mustWriteFile(t, scriptPath, []byte(script))

	out, err := exec.Command("bash", scriptPath).CombinedOutput()
	t.Logf("script output: %s", string(out))
	if err == nil {
		t.Fatalf("artifact script exited 0 for a self-committed binary; want non-zero. output: %s", string(out))
	}
	var exitErr *exec.ExitError
	if !okExitError(err, &exitErr) {
		t.Fatalf("unexpected error running artifact script: %v (output: %s)", err, string(out))
	}
	if exitErr.ExitCode() == 0 {
		t.Fatalf("artifact script exit code 0 for a self-committed binary; want non-zero")
	}
	// The offending path must be named in the output (the operator's
	// evidence in the pod logs).
	if !strings.Contains(string(out), "gocli") {
		t.Fatalf("artifact script output does not name the offending path 'gocli': %s", string(out))
	}
	if !strings.Contains(string(out), "binary") {
		t.Fatalf("artifact script output does not say the file is binary: %s", string(out))
	}

	// Clean source-only commit -> exit 0.
	cleanRepo := t.TempDir()
	mustGit(t, cleanRepo, "init", "-q", "-b", "main")
	mustGit(t, cleanRepo, "config", "user.email", "agent@coxswain.test")
	mustGit(t, cleanRepo, "config", "user.name", "Agent")
	writeFile(t, cleanRepo, "round.go", "package main\n\nfunc main() {}\n")
	mustGit(t, cleanRepo, "add", "round.go")
	mustGit(t, cleanRepo, "commit", "-q", "-m", "base")
	cleanBaseSHA := gitSHA(t, cleanRepo)
	// Source-only change: a new .go file (no binary, no size cap).
	writeFile(t, cleanRepo, "sum.go", "package main\n\nfunc Sum() int { return 1 }\n")
	mustGit(t, cleanRepo, "add", "-A")
	mustGit(t, cleanRepo, "commit", "-q", "-m", "agent: add sum (source only)")
	cleanVerifySHA := gitSHA(t, cleanRepo)

	cleanScript := artifactScript(cleanBaseSHA, cleanVerifySHA)
	cleanScript = strings.ReplaceAll(cleanScript, "cd /verify", "cd "+cleanRepo)
	cleanScript = strings.ReplaceAll(cleanScript, "TMPDIR=/verify", "TMPDIR="+cleanRepo)
	cleanScriptPath := filepath.Join(cleanRepo, "artifact-check.sh")
	mustWriteFile(t, cleanScriptPath, []byte(cleanScript))
	if out, err := exec.Command("bash", cleanScriptPath).CombinedOutput(); err != nil {
		t.Fatalf("artifact script exited non-zero for a clean source-only commit; want 0. err: %v, output: %s", err, string(out))
	} else if !strings.Contains(string(out), "clean") {
		t.Fatalf("artifact script output does not say clean: %s", string(out))
	}
}

// mustGit runs a git command in dir and fails the test on a non-zero exit.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (output: %s)", args, err, string(out))
	}
}

// gitSHA returns the HEAD commit SHA of the repo.
func gitSHA(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// writeFile writes a text file (0644).
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	mustWriteFile(t, filepath.Join(dir, name), []byte(content))
}

// mustWriteFile writes a file (0644), failing on error.
func mustWriteFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// okExitError is a type-assertion helper (errors.As) for *exec.ExitError.
func okExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}
