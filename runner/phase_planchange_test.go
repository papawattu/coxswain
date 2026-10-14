package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// writePlanAndHash writes PLAN.md (the plan bytes) and the approved-hash file
// (the approved bytes) to a temp workspace, returning the workspace path. This
// mirrors the operator's phase-init container (the sole writer of
// .coxswain/plan-hash) + the runner's writePlan (the sole writer of PLAN.md).
func writePlanAndHash(t *testing.T, plan, approved []byte) string {
	t.Helper()
	ws := t.TempDir()
	dir := filepath.Join(ws, resultDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, planFileName), plan, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, planHashFile), approved, 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// B3: the runner's plan-change detection (the gate that re-holds a Loop whose
// plan changed after approval). AGENTS.md: "Every gate spec must FAIL when the
// gate is disabled." A mismatch (a fresh PLAN.md whose SHA-256 is NOT the
// approved hash) must set PlanChanged=true; a match or an absent/empty
// approved-hash file must set it false.
func TestPlanChangedMismatch(t *testing.T) {
	// The approved hash is H; the fresh PLAN.md hashes to something else.
	ws := writePlanAndHash(t, []byte("a different plan"), []byte(sha256Hex([]byte("the approved plan"))))
	if !planChanged(ws) {
		t.Fatal("planChanged = false, want true (the fresh plan's hash does not match the approved hash)")
	}
}

func TestPlanChangedMatch(t *testing.T) {
	// The approved hash is H; the fresh PLAN.md hashes to H (no change).
	plan := []byte("the approved plan")
	ws := writePlanAndHash(t, plan, []byte(sha256Hex(plan)))
	if planChanged(ws) {
		t.Fatal("planChanged = true, want false (the fresh plan's hash matches the approved hash)")
	}
}

func TestPlanChangedNoApprovedHash(t *testing.T) {
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
	if planChanged(ws) {
		t.Fatal("planChanged = true, want false (no approved-hash file: a no-op)")
	}
}

func TestPlanChangedEmptyApprovedHash(t *testing.T) {
	// An empty approved-hash file (the operator wrote it empty — no approved
	// plan yet): a no-op.
	ws := writePlanAndHash(t, []byte("a plan"), []byte(""))
	if planChanged(ws) {
		t.Fatal("planChanged = true, want false (an empty approved-hash file: a no-op)")
	}
}
