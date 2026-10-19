package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/papawattu/coxswain/runner/testhelper"
)

// I93 (R29 P1): the plan-hash check runs at the START of an Implementing run,
// BEFORE any agent work. On a mismatch (the current PLAN.md's SHA-256 is not
// the approved hash), the agent is NEVER invoked (the fake model server sees
// zero requests) and NOTHING is committed (the workspace repo's head is
// unchanged and the claim carries no headCommit). The claim is blocked with
// PlanMismatch=true and a blockedReason naming both hashes. AGENTS.md:
// "Every gate spec must FAIL when the gate is disabled" — the named mutation
// is "move the check back after drivePhaseOnce" (the original defect): with
// that mutation the agent is invoked, work is committed, and these assertions
// fail.

// TestPlanHashMismatchBlocksBeforeAgent runs a full Implementing PhaseRun
// against a mismatched plan and asserts the agent is never invoked and
// nothing is committed.
func TestPlanHashMismatchBlocksBeforeAgent(t *testing.T) {
	fake := testhelper.New(testhelper.ModelResponse{Content: "done"})
	defer fake.Close()
	claimPath, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	initTestRepo(t, ws) // the Implementing commit step needs a git repo
	base := gitIn(t, ws, "rev-parse", "HEAD")
	writeDesiredPhase(t, ws, PhaseImplementing)
	// The approved hash is for "the approved plan"; the current PLAN.md is a
	// different plan (the plan changed after the user approved).
	approved := sha256Hex([]byte("the approved plan"))
	if err := os.WriteFile(filepath.Join(ws, resultDirName, planFileName), []byte("a different plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, resultDirName, planHashFile), []byte(approved), 0o644); err != nil {
		t.Fatal(err)
	}
	stop := make(chan any)

	res := PhaseRun(PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
		PollInterval: 5 * time.Millisecond}, stop)

	if res.Status != statusBlocked {
		t.Fatalf("Implementing with a mismatched plan: status = %q, want %q", res.Status, statusBlocked)
	}
	if !res.PlanMismatch {
		t.Fatal("PlanMismatch = false, want true (the current plan's hash does not match the approved hash)")
	}
	if len(fake.Requests) != 0 {
		t.Fatalf("the agent was invoked %d times, want 0 (a mismatch must block BEFORE work)", len(fake.Requests))
	}
	if res.HeadCommit != "" {
		t.Fatalf("HeadCommit = %q, want empty (nothing may be committed on a mismatch)", res.HeadCommit)
	}
	head := gitIn(t, ws, "rev-parse", "HEAD")
	if head != base {
		t.Fatalf("the workspace repo's head moved from %s to %s; a mismatch must not commit anything", base, head)
	}
	// The claim is blocked with PlanMismatch=true and a blockedReason naming
	// both hashes (the operator's claim channel).
	claim := parseClaim(t, claimPath)
	if claim["status"] != statusBlocked {
		t.Fatalf("claim status = %v, want %q", claim["status"], statusBlocked)
	}
	if res.VerificationNotes == "" {
		t.Fatal("the blockedReason (VerificationNotes) is empty; it must name both hashes")
	}
	if got := res.VerificationNotes; !strings.Contains(got, approved) {
		t.Fatalf("the blockedReason must name the approved hash %s: %s", approved, got)
	}
}

// TestPlanHashMissingPlanBlocksBeforeAgent is the missing-plan case (R29 P1.2):
// a non-empty approved hash with no PLAN.md on disk must ALSO block before
// any agent work (the more dangerous case: an approved Loop whose plan
// vanished — a silent no-op would let the agent implement an unknown plan).
func TestPlanHashMissingPlanBlocksBeforeAgent(t *testing.T) {
	fake := testhelper.New(testhelper.ModelResponse{Content: "done"})
	defer fake.Close()
	claimPath, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	initTestRepo(t, ws)
	writeDesiredPhase(t, ws, PhaseImplementing)
	// A non-empty approved hash; no PLAN.md at all.
	wrote := os.WriteFile(
		filepath.Join(ws, resultDirName, planHashFile),
		[]byte(sha256Hex([]byte("the approved plan"))), 0o644)
	if wrote != nil {
		t.Fatal(wrote)
	}
	stop := make(chan any)

	res := PhaseRun(PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
		PollInterval: 5 * time.Millisecond}, stop)

	if res.Status != statusBlocked {
		t.Fatalf("missing plan with approved hash: status = %q, want %q", res.Status, statusBlocked)
	}
	if !res.PlanMismatch {
		t.Fatal("PlanMismatch = false, want true (PLAN.md is missing with a non-empty approved hash)")
	}
	if len(fake.Requests) != 0 {
		t.Fatalf("the agent was invoked %d times, want 0 (must block BEFORE any agent work)", len(fake.Requests))
	}
	claim := parseClaim(t, claimPath)
	if claim["status"] != statusBlocked {
		t.Fatalf("claim status = %v, want %q", claim["status"], statusBlocked)
	}
}
