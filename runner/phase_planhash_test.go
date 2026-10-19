package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePlanAndHash and sha256Hex are defined in phase_planchange_test.go (the
// B3 plan-change specs). The I93 specs below reuse them.

// I93 (R29 P1): the runner's plan-hash-mismatch detection at the START of an
// Implementing run (BEFORE any agent work; the gate that re-holds a Loop whose
// plan changed between the user's approval and the Implementing run).
// AGENTS.md: "Every gate spec must FAIL when the gate is disabled." A mismatch
// (the current PLAN.md whose SHA-256 is NOT the approved hash) must return
// true with a blockedReason naming both hashes; a match or an absent/empty
// approved-hash file must return false; a MISSING PLAN.md with a non-empty
// approved hash must return true (R29 P1.2: the approved Loop whose plan
// vanished is the more dangerous case, not a no-op).
func TestPlanHashMismatchAtStart_Mismatch(t *testing.T) {
	// The approved hash is H (for "the approved plan"); the current PLAN.md
	// hashes to something else (the plan changed after approval).
	approved := sha256Hex([]byte("the approved plan"))
	ws := writePlanAndHash(t, []byte("a different plan"), []byte(approved))
	got, reason := planHashMismatchAtStart(ws)
	if !got {
		t.Fatal("planHashMismatchAtStart = false, want true (the current plan's hash does not match the approved hash)")
	}
	if !strings.Contains(reason, approved) {
		t.Fatalf("blockedReason must name the approved hash %s: %s", approved, reason)
	}
}

func TestPlanHashMismatchAtStart_Match(t *testing.T) {
	// The approved hash is H; the current PLAN.md hashes to H (no change).
	plan := []byte("the approved plan")
	ws := writePlanAndHash(t, plan, []byte(sha256Hex(plan)))
	got, reason := planHashMismatchAtStart(ws)
	if got {
		t.Fatalf("planHashMismatchAtStart = true (reason %q), want false (hashes match)", reason)
	}
}

func TestPlanHashMismatchAtStart_NoApprovedHash(t *testing.T) {
	// No approved hash (a fresh Loop, or a plan not yet approved): a no-op.
	ws := t.TempDir()
	dir := filepath.Join(ws, resultDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, planFileName), []byte("a plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No .coxswain/plan-hash file at all.
	got, reason := planHashMismatchAtStart(ws)
	if got {
		t.Fatalf("planHashMismatchAtStart = true (reason %q), want false (no approved-hash file: a no-op)", reason)
	}
}

func TestPlanHashMismatchAtStart_EmptyApprovedHash(t *testing.T) {
	// An empty approved-hash file (the operator wrote it empty — no approved
	// plan yet): a no-op.
	ws := writePlanAndHash(t, []byte("a plan"), []byte(""))
	got, reason := planHashMismatchAtStart(ws)
	if got {
		t.Fatalf("planHashMismatchAtStart = true (reason %q), want false (an empty approved-hash file: a no-op)", reason)
	}
}

func TestPlanHashMismatchAtStart_MissingPlanFile(t *testing.T) {
	// An approved hash is present but PLAN.md is missing (the plan the user
	// approved vanished). R29 P1.2: this MUST block — the more dangerous case
	// (a silent no-op would let the agent implement an unknown plan).
	ws := t.TempDir()
	dir := filepath.Join(ws, resultDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	approved := sha256Hex([]byte("some plan"))
	// Write only the approved hash; no PLAN.md.
	if err := os.WriteFile(filepath.Join(dir, planHashFile), []byte(approved), 0o644); err != nil {
		t.Fatal(err)
	}
	got, reason := planHashMismatchAtStart(ws)
	if !got {
		t.Fatal("planHashMismatchAtStart = false, want true (PLAN.md is missing with a non-empty approved hash: block)")
	}
	if !strings.Contains(reason, approved) {
		t.Fatalf("blockedReason must name the approved hash %s: %s", approved, reason)
	}
}
