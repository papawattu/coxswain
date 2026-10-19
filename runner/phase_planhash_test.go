package runner

import (
	"os"
	"path/filepath"
	"testing"
)

// writePlanAndHash and sha256Hex are defined in phase_planchange_test.go (the
// B3 plan-change specs). The I93 specs below reuse them.

// I93 (R28 P1): the runner's plan-hash-mismatch detection at the START of an
// Implementing run (the gate that re-holds a Loop whose plan changed between
// the user's approval and the Implementing run). AGENTS.md: "Every gate spec
// must FAIL when the gate is disabled." A mismatch (the current PLAN.md whose
// SHA-256 is NOT the approved hash) must set PlanMismatch=true; a match or an
// absent/empty approved-hash file must set it false.
func TestPlanHashMismatchAtStart_Mismatch(t *testing.T) {
	// The approved hash is H (for "the approved plan"); the current PLAN.md
	// hashes to something else (the plan changed after approval).
	ws := writePlanAndHash(t, []byte("a different plan"), []byte(sha256Hex([]byte("the approved plan"))))
	if !planHashMismatchAtStart(ws) {
		t.Fatal("planHashMismatchAtStart = false, want true (the current plan's hash does not match the approved hash)")
	}
}

func TestPlanHashMismatchAtStart_Match(t *testing.T) {
	// The approved hash is H; the current PLAN.md hashes to H (no change).
	plan := []byte("the approved plan")
	ws := writePlanAndHash(t, plan, []byte(sha256Hex(plan)))
	if planHashMismatchAtStart(ws) {
		t.Fatal("planHashMismatchAtStart = true, want false (the current plan's hash matches the approved hash)")
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
	if planHashMismatchAtStart(ws) {
		t.Fatal("planHashMismatchAtStart = true, want false (no approved-hash file: a no-op)")
	}
}

func TestPlanHashMismatchAtStart_EmptyApprovedHash(t *testing.T) {
	// An empty approved-hash file (the operator wrote it empty — no approved
	// plan yet): a no-op.
	ws := writePlanAndHash(t, []byte("a plan"), []byte(""))
	if planHashMismatchAtStart(ws) {
		t.Fatal("planHashMismatchAtStart = true, want false (an empty approved-hash file: a no-op)")
	}
}

func TestPlanHashMismatchAtStart_MissingPlanFile(t *testing.T) {
	// An approved hash is present but PLAN.md was never written (the Planning
	// run did not write a plan, or the workspace was cleared): a read error is
	// treated as "no plan" (a no-op).
	ws := t.TempDir()
	dir := filepath.Join(ws, resultDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write only the approved hash; no PLAN.md.
	if err := os.WriteFile(filepath.Join(dir, planHashFile), []byte(sha256Hex([]byte("some plan"))), 0o644); err != nil {
		t.Fatal(err)
	}
	if planHashMismatchAtStart(ws) {
		t.Fatal("planHashMismatchAtStart = true, want false (PLAN.md is missing: a no-op)")
	}
}
