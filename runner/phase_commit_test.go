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

	head, staged, refuse := commitWorkspace(ws)
	if refuse != "" {
		t.Fatalf("no refusal expected, got %q", refuse)
	}
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
	// I47: the staged paths list the files the commit carries (evidence).
	if len(staged) != 1 || staged[0] != "newfile.go" {
		t.Fatalf("staged = %v, want [newfile.go]", staged)
	}
}

func TestCommitWorkspaceNoChangesReturnsExistingHead(t *testing.T) {
	ws := t.TempDir()
	initTestRepo(t, ws)
	base := gitIn(t, ws, "rev-parse", "HEAD")

	head, _, refuse := commitWorkspace(ws)
	if refuse != "" {
		t.Fatalf("no refusal expected, got %q", refuse)
	}
	if head != base {
		t.Fatalf("no changes: head = %q, want the existing head %q", head, base)
	}
}

func TestCommitWorkspaceNotAGitRepoReturnsEmpty(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, _ := commitWorkspace(ws)
	if got != "" {
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
	// I47: leave uncommitted agent work so the commit path (add, commit)
	// runs (the no-change path would only rev-parse).
	if err := os.WriteFile(filepath.Join(ws, "work.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

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

	head, _, _ := commitWorkspace(ws)
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

// TestRestartReEmitsImplementingSuccessTriggersRecommit (S5a, reviewer fix 1):
// a stored Implementing success WITHOUT a headCommit triggers a re-commit on
// restart (cheap, no model call). The re-commit writes the headCommit into
// the claim. This is the fix for the live kind run where the first run's
// commitWorkspace returned ” (its log was lost) and every restart re-emitted
// the same headCommit-less claim forever.
func TestRestartReEmitsImplementingSuccessTriggersRecommit(t *testing.T) {
	fake := testhelper.New(testhelper.ModelResponse{Content: "implement-done"})
	defer fake.Close()
	claimPath, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	initTestRepo(t, ws)
	writeDesiredPhase(t, ws, PhaseImplementing)
	// Write the operator's iteration marker (the claim's iteration field
	// must match for the re-emit to fire).
	if err := os.WriteFile(filepath.Join(ws, resultDirName, iterationFileName), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	stop := make(chan any)
	cfg := PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
		PollInterval: 5 * time.Millisecond}
	// First run: the model completes but commitWorkspace is made to fail
	// (simulated by making the repo read-only so git add fails). The claim
	// carries no headCommit.
	// To simulate the first run's commit failure, we pre-write the result
	// as if the first run succeeded but the commit failed: status=success,
	// observedPhase=Implementing, headCommit="", iteration=1.
	firstRes := Result{
		Status:        statusSuccess,
		ObservedPhase: PhaseImplementing,
		Iteration:     1,
	}
	if err := writeResult(filepath.Join(ws, resultDirName, resultFileName), firstRes); err != nil {
		t.Fatal(err)
	}
	// Restart: the re-emit path fires (status=success, observedPhase matches,
	// iteration matches). Because headCommit is empty, it re-commits.
	restart := PhaseRun(cfg, stop)
	// The re-commit must have succeeded (the repo is writable in the test):
	// the claim now carries a valid 40-hex headCommit.
	if restart.Status != statusSuccess {
		t.Fatalf("restart status = %q, want %q (re-commit must succeed)", restart.Status, statusSuccess)
	}
	if len(fake.Requests) != 0 {
		t.Fatalf("restart: got %d model requests, want 0 (no model call on a completed prior claim)", len(fake.Requests))
	}
	if !isHex40(restart.HeadCommit) {
		t.Fatalf("re-emitted claim headCommit = %q, want a 40-hex SHA (re-commit must fill it)", restart.HeadCommit)
	}
	claim := parseClaim(t, claimPath)
	if hc, ok := claim["headCommit"].(string); !ok || !isHex40(hc) {
		t.Fatalf("claim headCommit = %v, want a 40-hex SHA", claim["headCommit"])
	}
}

// TestRestartStaleIterationResultNotReused (S5a, reviewer fix 3): a stored
// Implementing success from a PREVIOUS iteration is NOT re-emitted on a new
// iteration — the phase must run again with fresh model work. This is the
// ADR-0005 D11 guard: after a failed verify sends the Loop back to
// Implementing (iteration+1), the runner must NOT re-emit the old success
// claim.
func TestRestartStaleIterationResultNotReused(t *testing.T) {
	fake := testhelper.New(testhelper.ModelResponse{Content: "implement-iter2"})
	defer fake.Close()
	claimPath, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	initTestRepo(t, ws)
	writeDesiredPhase(t, ws, PhaseImplementing)
	// The operator sent the Loop back to Implementing for iteration 2: the
	// .coxswain/iteration is now "2".
	if err := os.WriteFile(filepath.Join(ws, resultDirName, iterationFileName), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Plant a stored result from iteration 1 (a prior Implementing success
	// with a valid headCommit): it must NOT be re-emitted for iteration 2.
	oldRes := Result{
		Status:        statusSuccess,
		ObservedPhase: PhaseImplementing,
		Iteration:     1,
		HeadCommit:    strings.Repeat("a", 40),
	}
	if err := writeResult(filepath.Join(ws, resultDirName, resultFileName), oldRes); err != nil {
		t.Fatal(err)
	}
	stop := make(chan any)
	cfg := PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
		PollInterval: 5 * time.Millisecond}
	res := PhaseRun(cfg, stop)
	// The stale-iteration result must NOT have been re-emitted: the phase
	// ran again (a model call was made).
	if len(fake.Requests) != 1 {
		t.Fatalf("stale-iteration result must NOT be re-emitted: "+
			"got %d model requests, want 1 (phase must run again)", len(fake.Requests))
	}
	// The new result is for iteration 2 (the current .coxswain/iteration).
	if res.Iteration != 2 {
		t.Fatalf("new result iteration = %d, want 2 (the current .coxswain/iteration)", res.Iteration)
	}
	_ = claimPath
}

// TestPhaseRunImplementingClaimCarriesCurrentIteration (S5a, the P2h
// kind-run stall): the claim written to the termination log MUST carry the
// .coxswain/iteration the runner read — the operator's stale-iteration
// guard compares claim.Iteration against loop.Status.Iteration and
// discards a mismatched claim, and a missing "iteration" key parses as 0,
// so an omitted field reads as a STALE claim from iteration 0. This is the
// P2h root cause: the Implementing-iteration-2 claim was written as
// {"observedPhase":"Implementing","status":"success","headCommit":"…"}
// (no iteration — writeClaim's claim struct omitted the field), the
// operator ignored it as stale (claimIteration=0, statusIteration=2), and
// the Loop could never advance past Verifying -> Stalled.
func TestPhaseRunImplementingClaimCarriesCurrentIteration(t *testing.T) {
	fake := s4FakeModel()
	defer fake.Close()
	claimPath, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	initTestRepo(t, ws)
	writeDesiredPhase(t, ws, PhaseImplementing)
	// The operator's phase-init wrote the CURRENT iteration marker: 2.
	if err := os.WriteFile(filepath.Join(ws, resultDirName, iterationFileName), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	stop := make(chan any)

	res := PhaseRun(PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
		PollInterval: 5 * time.Millisecond}, stop)
	if res.Status != statusSuccess {
		t.Fatalf("Implementing phase: status = %q (notes: %s)", res.Status, res.VerificationNotes)
	}
	data, err := os.ReadFile(claimPath)
	if err != nil {
		t.Fatal(err)
	}
	var claim struct {
		Iteration int `json:"iteration"`
	}
	if err := json.Unmarshal(data, &claim); err != nil {
		t.Fatalf("claim is not JSON: %v", err)
	}
	if claim.Iteration != 2 {
		t.Fatalf("claim iteration = %d, want 2 (the current .coxswain/iteration — a missing key parses as 0, which the operator's stale-iteration guard discards when status.iteration is 2; the P2h kind-run stall): %s", claim.Iteration, data)
	}
}

// TestWriteClaimCarriesIteration (S5a, the P2h kind-run stall): writeClaim's
// JSON (the /dev/termination-log the operator parses) MUST carry
// iteration == res.Iteration. The operator's stale-iteration guard reads
// claim.Iteration and a missing "iteration" key parses as 0 — so an
// omitted field reads as a STALE claim from iteration 0, and every Loop
// would stall at its first iterate (the P2h root cause: writeClaim's claim
// struct omitted the field, so the Implementing-iteration-2 claim was
// written as {"observedPhase","status","headCommit"} with no iteration,
// and the operator ignored it as stale).
func TestWriteClaimCarriesIteration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim.json")
	res := Result{Status: statusSuccess, ObservedPhase: PhaseImplementing,
		HeadCommit: "abc", Iteration: 2}
	writeClaim(path, res)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var claim struct {
		ObservedPhase string `json:"observedPhase"`
		Status        string `json:"status"`
		HeadCommit    string `json:"headCommit"`
		Iteration     *int   `json:"iteration"`
	}
	if err := json.Unmarshal(data, &claim); err != nil {
		t.Fatalf("claim is not JSON: %v (%s)", err, data)
	}
	if claim.ObservedPhase != PhaseImplementing || claim.Status != statusSuccess {
		t.Fatalf("claim observedPhase/status = %q/%q, want Implementing/success: %s", claim.ObservedPhase, claim.Status, data)
	}
	if claim.Iteration == nil {
		t.Fatalf("the claim MUST carry the iteration key (a missing key parses as 0 at the operator, which the stale-iteration guard discards when status.iteration is 2): %s", data)
	}
	if *claim.Iteration != res.Iteration {
		t.Fatalf("claim iteration = %d, want %d (res.Iteration): %s", *claim.Iteration, res.Iteration, data)
	}
}
