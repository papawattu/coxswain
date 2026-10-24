package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// f37Values are the values shellQuote must round-trip through sh byte for
// byte: 0..3 apostrophes, backslashes, command substitution, backticks,
// newlines, and the injection payload '; id; ' (F37: the three-quote escape
// left the string open after one apostrophe and ran `id` after two).
var f37Values = []string{
	"",
	"plain",
	"no apostrophe at all",
	"one'apostrophe",
	"two'apos'trophes",
	"three'a'p'ostrophes",
	"'",
	"''",
	"'''",
	`back\slash and \' and \\`,
	"$(touch PWNED)",
	"`touch PWNED`",
	"$HOME ${HOME} $?",
	"line one\nline two\n",
	"'; touch PWNED; '",
	"x' ; touch PWNED ; echo '",
	"Fix the user's login",
}

// TestShellQuoteRoundTripsThroughSh executes sh with the quoted value and
// asserts the output equals the value byte for byte and nothing else ran
// (the working dir stays free of PWNED). Mutation: restore the three-quote
// replacement (three apostrophes) and the apostrophe cases FAIL.
func TestShellQuoteRoundTripsThroughSh(t *testing.T) {
	for _, v := range f37Values {
		dir := t.TempDir()
		cmd := exec.Command("sh", "-c", "printf '%s' "+shellQuote(v))
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("value %q: sh failed: %v: %s", v, err, out)
			continue
		}
		if string(out) != v {
			t.Errorf("value %q: sh printed %q", v, out)
		}
		if _, err := os.Stat(filepath.Join(dir, "PWNED")); err == nil {
			t.Errorf("value %q: injected command ran (PWNED exists)", v)
		}
	}
}

// TestPhaseInitScriptApostropheFeedback runs the REAL generated phase-init
// script (only /workspace substituted) with plan feedback carrying
// apostrophes and an injection payload, and asserts the feedback file holds
// the value byte for byte and nothing else ran.
func TestPhaseInitScriptApostropheFeedback(t *testing.T) {
	for _, fb := range []string{"the plan doesn't cover the user's case", "x'; touch PWNED; echo '"} {
		ws := t.TempDir()
		script := phaseInitScript(coxv1alpha1.LoopPhasePlanning, 1, 2, strings.Repeat("a", 64), fb)
		script = strings.ReplaceAll(script, "DEST=/workspace", "DEST="+ws)
		path := filepath.Join(t.TempDir(), "init.sh")
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", path)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("feedback %q: phase-init failed: %v: %s", fb, err, out)
		}
		got, err := os.ReadFile(filepath.Join(ws, ".coxswain", coxPlanFeedbackFile))
		if err != nil {
			t.Fatalf("feedback %q: %v", fb, err)
		}
		if string(got) != fb {
			t.Errorf("feedback file = %q, want %q", got, fb)
		}
		if _, err := os.Stat(filepath.Join(ws, "PWNED")); err == nil {
			t.Errorf("feedback %q: injected command ran", fb)
		}
	}
}
