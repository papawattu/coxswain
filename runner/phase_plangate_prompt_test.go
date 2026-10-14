package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// B3: the planning prompt must include the rejection reason the operator
// wrote to .coxswain/plan-feedback (the cox reject --reason reason), so the
// re-plan addresses the rejection. An absent/empty file means "no prior
// rejection" (the prompt is unchanged). AGENTS.md: "Every gate spec must FAIL
// when the gate is disabled."
func TestPlanningPromptIncludesPlanFeedback(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, resultDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const reason = "plan misses the edge case: it does not handle empty input"
	if err := os.WriteFile(filepath.Join(dir, planFeedbackFile), []byte(reason), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := planningPrompt(ws, "append a newline to README.md", 512)
	if !strings.Contains(prompt, reason) {
		t.Fatalf("the planning prompt does not include the plan-feedback reason %q;\nprompt:\n%s", reason, prompt)
	}
}

func TestPlanningPromptNoFeedback(t *testing.T) {
	// No plan-feedback file (a fresh Loop, no prior rejection): the prompt is
	// the base planning prompt (no REJECTION FEEDBACK section).
	ws := t.TempDir()
	prompt := planningPrompt(ws, "append a newline to README.md", 512)
	if strings.Contains(prompt, "REJECTION FEEDBACK") {
		t.Fatalf("the planning prompt includes a REJECTION FEEDBACK section with no feedback file;\nprompt:\n%s", prompt)
	}
}

func TestPlanningPromptEmptyFeedback(t *testing.T) {
	// An empty plan-feedback file (the operator wrote it empty): the prompt is
	// unchanged (no REJECTION FEEDBACK section).
	ws := t.TempDir()
	dir := filepath.Join(ws, resultDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, planFeedbackFile), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := planningPrompt(ws, "append a newline to README.md", 512)
	if strings.Contains(prompt, "REJECTION FEEDBACK") {
		t.Fatalf("the planning prompt includes a REJECTION FEEDBACK section with an empty feedback file;\nprompt:\n%s", prompt)
	}
}
