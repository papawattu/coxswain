// I80/D51 (R24 Q1): an UNTRACKED root PLAN.md is the agent's own plan (its
// plan lives in .coxswain/PLAN.md, which commitWorkspace already excludes).
// Keep it out of the verified commit so it is not delivered. A base-TRACKED
// root PLAN.md that the agent modified is source work and is still committed,
// so a repo's own PLAN.md can be changed.
package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommitWorkspaceExcludesUntrackedRootPlan: the agent writes a root
// PLAN.md (untracked in the base) and the commit EXCLUDES it (D51: the plan
// belongs in .coxswain/PLAN.md, not the delivered PR).
func TestCommitWorkspaceExcludesUntrackedRootPlan(t *testing.T) {
	ws := t.TempDir()
	initTestRepo(t, ws)
	base := gitIn(t, ws, "rev-parse", "HEAD")

	// A normal agent change (so the commit is non-empty) plus an UNTRACKED
	// root PLAN.md the agent wrote (it should NOT enter the commit).
	if err := os.WriteFile(filepath.Join(ws, "newfile.go"), []byte("package main\nfunc New() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "PLAN.md"), []byte("# agent plan\nstep 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	head, staged, refuse := commitWorkspace(ws)
	if refuse != "" {
		t.Fatalf("no refusal expected, got %q", refuse)
	}
	if head == "" || !sha40Hex.MatchString(head) {
		t.Fatalf("commitWorkspace returned %q (staged %v), want a 40-hex SHA", head, staged)
	}
	if head == base {
		t.Fatal("the agent made a change (newfile.go); the head must have moved")
	}
	// The newfile.go is staged; the root PLAN.md is NOT.
	foundNew, foundPlan := false, false
	for _, p := range staged {
		if p == "newfile.go" {
			foundNew = true
		}
		if p == "PLAN.md" {
			foundPlan = true
		}
	}
	if !foundNew {
		t.Errorf("staged set %v: newfile.go is missing", staged)
	}
	if foundPlan {
		t.Errorf("staged set %v: the untracked root PLAN.md must be excluded (D51)", staged)
	}
	// PLAN.md is still on disk (the commit excludes it, it does not delete it).
	if _, err := os.Stat(filepath.Join(ws, "PLAN.md")); err != nil {
		t.Errorf("PLAN.md should still exist on disk (excluded, not deleted): %v", err)
	}
}

// TestCommitWorkspaceKeepsTrackedRootPlan: a base-TRACKED root PLAN.md that
// the agent MODIFIED is source work and its change is KEPT in the commit (a
// repo's own PLAN.md can be changed — the exclude is only for an untracked
// PLAN.md the agent created).
func TestCommitWorkspaceKeepsTrackedRootPlan(t *testing.T) {
	ws := t.TempDir()
	// A base commit that TRACKS a root PLAN.md (the repo's own plan file).
	if err := os.WriteFile(filepath.Join(ws, "PLAN.md"), []byte("original plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, resultDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, ws, "init", "-q")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "base")
	base := gitIn(t, ws, "rev-parse", "HEAD")

	// The agent MODIFIES the tracked root PLAN.md (source work — must be kept).
	if err := os.WriteFile(filepath.Join(ws, "PLAN.md"), []byte("modified plan by the agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	head, staged, refuse := commitWorkspace(ws)
	if refuse != "" {
		t.Fatalf("no refusal expected, got %q", refuse)
	}
	if head == "" || !sha40Hex.MatchString(head) {
		t.Fatalf("commitWorkspace returned %q (staged %v), want a 40-hex SHA", head, staged)
	}
	if head == base {
		t.Fatal("the agent modified PLAN.md; the head must have moved")
	}
	foundPlan := false
	for _, p := range staged {
		if p == "PLAN.md" {
			foundPlan = true
		}
	}
	if !foundPlan {
		t.Errorf("staged set %v: a base-tracked root PLAN.md that the agent modified must be KEPT (not excluded)", staged)
	}
}

// TestImplementingPromptTellsAgentPlanLivesInCoxswain: the implementing prompt
// (D51) tells the agent its plan lives in .coxswain/PLAN.md and that it must
// not create a root PLAN.md, so the agent does not write its plan to a root
// PLAN.md (which the commit would otherwise exclude, but which would still
// clutter the workspace and could be mistaken for source).
func TestImplementingPromptTellsAgentPlanLivesInCoxswain(t *testing.T) {
	p := implementingPrompt(t.TempDir(), "make the goal's changes")
	if !strings.Contains(p, "Your plan lives in .coxswain/PLAN.md") {
		t.Errorf("implementing prompt does not tell the agent its plan lives in .coxswain/PLAN.md:\n%s", p)
	}
	if !strings.Contains(p, "do not create a root PLAN.md") {
		t.Errorf("implementing prompt does not tell the agent not to create a root PLAN.md:\n%s", p)
	}
}
