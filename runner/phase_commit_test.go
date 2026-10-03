// S5a (B3): unit tests for the Implementing commit step — commitWorkspace
// (the agent's commit, .coxswain excluded) and the headCommit ride-through
// into the ADR-0004 claim. The workspace is a REAL git repo (t.TempDir) so
// the git invocations are hermetic; the model is the one fake (the same
// boundary the other phase tests use).
package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/papawattu/coxswain/runner/testhelper"
)

// sha40Hex is the shape a git commit SHA must have (the operator's strict
// validation mirror — the runner must produce it for the pin to work).
var sha40Hex = regexp.MustCompile(`^[0-9a-f]{40}$`)

// gitIn runs git in dir and panics on failure (test helper: a broken git
// environment is a test error, not a code error).
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{"-C", dir, "-c", "user.name=test", "-c", "user.email=t@t",
		"-c", "commit.gpgsign=false"}
	out, err := exec.Command("git", append(base, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// initTestRepo initialises a git repo in dir with one base commit (the
// .coxswain dir present but untracked, as in a live sandbox workspace).
func initTestRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, resultDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "base")
}

func TestCommitWorkspaceCommitsAndReturnsHeadSHA(t *testing.T) {
	ws := t.TempDir()
	initTestRepo(t, ws)
	base := gitIn(t, ws, "rev-parse", "HEAD")

	// The agent's work: a new file + a modified one. The .coxswain dir holds
	// the operator's result files (must NOT enter the commit).
	if err := os.WriteFile(filepath.Join(ws, "newfile.go"), []byte("package main\nfunc New() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, resultDirName, resultFileName), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	head := commitWorkspace(ws)
	if head == "" {
		t.Fatal("commitWorkspace returned an empty SHA")
	}
	if !sha40Hex.MatchString(head) {
		t.Fatalf("commitWorkspace returned %q, want a 40-hex SHA", head)
	}
	if head == base {
		t.Fatal("the agent made changes; the head must have moved past the base commit")
	}
	// The committed identity is the fixed agent identity (never the test's).
	out := gitIn(t, ws, "log", "-1", "--format=%an <%ae>")
	if out != "coxswain-agent <agent@localhost>" {
		t.Fatalf("commit author = %q, want coxswain-agent <agent@localhost>", out)
	}
	// .coxswain must NOT be tracked (the operator's dir stays out of the
	// verified commit).
	ls := gitIn(t, ws, "ls-files")
	for line := range strings.SplitSeq(ls, "\n") {
		if filepath.FromSlash(line) == filepath.Join(resultDirName, resultFileName) ||
			filepath.FromSlash(line) == resultDirName ||
			len(line) > len(resultDirName) && line[:len(resultDirName)] == resultDirName {
			t.Fatalf(".coxswain content leaked into the commit: %s", line)
		}
	}
	// The working file is in the commit.
	if got := gitIn(t, ws, "show", "HEAD:main.go"); got == "" {
		t.Fatal("the committed tree is empty")
	}
}

func TestCommitWorkspaceNoChangesReturnsExistingHead(t *testing.T) {
	ws := t.TempDir()
	initTestRepo(t, ws)
	base := gitIn(t, ws, "rev-parse", "HEAD")

	head := commitWorkspace(ws)
	if head != base {
		t.Fatalf("no changes: head = %q, want the existing head %q", head, base)
	}
}

func TestCommitWorkspaceNotAGitRepoReturnsEmpty(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := commitWorkspace(ws); got != "" {
		t.Fatalf("non-repo workspace: commitWorkspace = %q, want empty (ADR-0005 fail-closed)", got)
	}
}

// TestCommitWorkspaceUsesSafeDirectory verifies that commitWorkspace passes
// -c safe.directory=<workspace> to all git calls. The PVC mount /workspace
// is root-owned (init container runs as root) while the runner runs as
// uid 65532; without safe.directory git fails with 'dubious ownership' and
// headCommit is empty (the Verifying advance is blocked). The test uses a
// git shim (a shell script in PATH) that records its arguments and
// delegates to the real git.
func TestCommitWorkspaceUsesSafeDirectory(t *testing.T) {
	ws := t.TempDir()
	initTestRepo(t, ws)

	// Build a git shim: a shell script that appends its args to a log file
	// then execs the real git with the same args.
	binDir := t.TempDir()
	gitLog := filepath.Join(binDir, "git-args.log")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("real git not found: %v", err)
	}
	shim := filepath.Join(binDir, "git")
	shimSrc := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\nexec %q \"$@\"\n", gitLog, realGit)
	if err := os.WriteFile(shim, []byte(shimSrc), 0o755); err != nil {
		t.Fatal(err)
	}

	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+oldPath)
	// t.Setenv registers a cleanup that restores PATH automatically.

	head := commitWorkspace(ws)
	if head == "" {
		t.Fatal("commitWorkspace returned an empty SHA (shim may have broken git)")
	}
	if !sha40Hex.MatchString(head) {
		t.Fatalf("commitWorkspace returned %q, want a 40-hex SHA", head)
	}

	// Verify all git invocations carried -c safe.directory=<ws>.
	data, err := os.ReadFile(gitLog)
	if err != nil {
		t.Fatalf("git shim log not written: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 git invocations (add, commit, rev-parse), got %d", len(lines))
	}
	for i, line := range lines {
		want := fmt.Sprintf("-c safe.directory=%s", ws)
		if !strings.Contains(line, want) {
			t.Errorf("git invocation %d: missing %q\n  got: %s", i+1, want, line)
		}
	}
}

func TestPhaseRunImplementingWritesHeadCommitClaim(t *testing.T) {
	fake := s4FakeModel()
	defer fake.Close()
	claimPath, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	initTestRepo(t, ws)
	writeDesiredPhase(t, ws, PhaseImplementing)
	stop := make(chan any)

	res := PhaseRun(PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
		PollInterval: 5 * time.Millisecond}, stop)
	if res.Status != statusSuccess {
		t.Fatalf("Implementing phase: status = %q (notes: %s)", res.Status, res.VerificationNotes)
	}
	if res.HeadCommit == "" {
		t.Fatal("a successful Implementing must carry headCommit in the result")
	}
	if !sha40Hex.MatchString(res.HeadCommit) {
		t.Fatalf("headCommit = %q, want a 40-hex SHA", res.HeadCommit)
	}
	data, err := os.ReadFile(claimPath)
	if err != nil {
		t.Fatal(err)
	}
	var claim struct {
		HeadCommit string `json:"headCommit"`
	}
	if err := json.Unmarshal(data, &claim); err != nil {
		t.Fatalf("claim is not JSON: %v", err)
	}
	if claim.HeadCommit != res.HeadCommit {
		t.Fatalf("claim headCommit = %q, want %q (must ride through the claim)",
			claim.HeadCommit, res.HeadCommit)
	}
}

func TestPhaseRunImplementingBlockedCarriesNoHeadCommit(t *testing.T) {
	// A model failure -> blocked: no commit, no headCommit in the claim.
	fake := testhelper.New(testhelper.ModelResponse{StatusCode: 500, RawBody: `{"error":"boom"}`})
	defer fake.Close()
	claimPath, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	initTestRepo(t, ws)
	writeDesiredPhase(t, ws, PhaseImplementing)
	stop := make(chan any)

	res := PhaseRun(PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
		PollInterval: 5 * time.Millisecond}, stop)
	if res.Status != statusBlocked {
		t.Fatalf("status = %q, want blocked", res.Status)
	}
	if res.HeadCommit != "" {
		t.Fatalf("a blocked run must not commit: headCommit = %q", res.HeadCommit)
	}
	data, err := os.ReadFile(claimPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); strings.Contains(got, "headCommit") {
		t.Fatalf("blocked claim must not carry headCommit: %s", got)
	}
}
