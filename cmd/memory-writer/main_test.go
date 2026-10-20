package main

// P5b (Phase 5 shared memory, PLAN-PHASE5.md slice P5b): the memory-writer
// Job's entrypoint execution test. The Job's entrypoint is the
// cmd/memory-writer binary (the operator-controlled code that reads the
// lessons file, screens it with the operator's static policy, and writes the
// eligible lessons to the memory PVC). This execution test runs the REAL
// entrypoint (the run function — the binary's core, called by main with the
// outcome arg) against a TEMP workspace + memory dir (via the COX_* env
// overrides) — not a paraphrased re-implementation (AGENTS.md test norm: the
// test exercises the real entrypoint, not a copy of its logic).
//
// The semantics tested (the plan's P5b + the reviewer's decision):
//   - The entrypoint reads the runner's proposed lessons from the workspace
//     file (COX_LESSONS_PATH) and writes the ELIGIBLE ones (the operator's
//     static policy, pkg/memorywrite.Screen) to the memory dir
//     (COX_MEMORY_DIR), one file per lesson (lesson-<n>.txt).
//   - The eligibility rule: a lesson is eligible only on a qualifying
//     terminal outcome (succeeded / failed-stalled). A non-qualifying
//     outcome (an empty / other arg) drops every lesson and writes nothing.
//   - A write failure (the memory dir is not writable) exits non-zero (the
//     Job's failure is the operator's MemoryWriteFailed evidence).
//   - The bounded summary (stdout) reports counts + per-lesson
//     sha256-prefix/written|dropped/rule — NEVER the lesson text.
//   - A missing lessons file (the runner proposed nothing) is a valid
//     "nothing to write": zero total, exit 0.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runMemoryWrite runs the REAL entrypoint (run) against a temp workspace +
// memory dir (the COX_* env overrides), with the given outcome arg. It
// returns (exitCode, error, stdout).
func runMemoryWrite(t *testing.T, workspace, memoryDir, outcome string) (int, error) {
	t.Helper()
	t.Setenv("COX_LESSONS_PATH", filepath.Join(workspace, ".coxswain", "lessons.json"))
	t.Setenv("COX_MEMORY_DIR", memoryDir)
	code, err := run(outcome)
	// stdout is not captured here (the test that needs the summary captures
	// os.Stdout directly — see the "bounded summary" spec).
	return code, err
}

// writeLessonsFile writes the given lessons to <workspace>/.coxswain/lessons.json.
func writeLessonsFile(t *testing.T, workspace string, lessons []string) {
	t.Helper()
	dir := filepath.Join(workspace, ".coxswain")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .coxswain: %v", err)
	}
	data, err := json.Marshal(lessons)
	if err != nil {
		t.Fatalf("marshal lessons: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lessons.json"), data, 0o644); err != nil {
		t.Fatalf("write lessons.json: %v", err)
	}
}

// readMemoryFiles returns the lesson-*.txt files in the memory dir, sorted by
// name, with their content (the trailing newline trimmed).
func readMemoryFiles(t *testing.T, memoryDir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(memoryDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}
		}
		t.Fatalf("read memory dir: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "lesson-") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(memoryDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out[e.Name()] = strings.TrimSuffix(string(b), "\n")
	}
	return out
}

// TestMemoryWriteWritesEligibleLessons: on a qualifying outcome (succeeded),
// the entrypoint reads the lessons file, screens it (the operator's static
// policy), and writes the ELIGIBLE lessons to the memory dir (one file per
// lesson). A lesson with a credential (the secrets screen) is DROPPED (not
// written). The bounded summary is on stdout (the counts, never the lesson
// text).
func TestMemoryWriteWritesEligibleLessons(t *testing.T) {
	ws := t.TempDir()
	mem := t.TempDir()
	// One eligible lesson (a normal path-reference), one with a credential
	// (dropped by the secrets screen).
	lessons := []string{
		"foo/bar: the retry must back off before the 500",     // eligible (path-shaped)
		"the key is sk-abc123def456ghi789jkl012mno345pq67890", // dropped (provider key)
	}
	writeLessonsFile(t, ws, lessons)
	code, err := runMemoryWrite(t, ws, mem, "succeeded")
	if code != 0 || err != nil {
		t.Fatalf("run(succeeded) = (%d, %v); want (0, nil)", code, err)
	}
	files := readMemoryFiles(t, mem)
	// Only the eligible lesson is written (the credential lesson is dropped
	// by the secrets screen). One file.
	if len(files) != 1 {
		t.Fatalf("memory dir has %d files (%v); want 1 (the credential lesson is dropped)", len(files), files)
	}
	if got := files["lesson-001.txt"]; got != lessons[0] {
		t.Errorf("lesson-001.txt = %q; want %q", got, lessons[0])
	}
	// The dropped lesson's content must NOT be on the memory PVC.
	for name, content := range files {
		if strings.Contains(content, "sk-abc123") {
			t.Errorf("%s contains a credential (%q); the secrets screen must drop it", name, content)
		}
	}
}

// TestMemoryWriteNonQualifyingOutcomeWritesNothing: on a NON-qualifying
// outcome (an empty arg — a non-terminal phase, or a missing outcome), the
// entrypoint writes nothing (the eligibility rule: a lesson is eligible only
// on a qualifying terminal outcome). The memory dir is empty; the exit code
// is 0 (a non-qualifying outcome is not a write failure — it is the
// operator's eligibility decision, and the operator only invokes the Job on
// a qualifying outcome).
func TestMemoryWriteNonQualifyingOutcomeWritesNothing(t *testing.T) {
	ws := t.TempDir()
	mem := t.TempDir()
	writeLessonsFile(t, ws, []string{"foo/bar: the retry must back off"})
	code, err := runMemoryWrite(t, ws, mem, "") // a non-qualifying outcome (empty)
	if code != 0 || err != nil {
		t.Fatalf("run(\"\") = (%d, %v); want (0, nil)", code, err)
	}
	files := readMemoryFiles(t, mem)
	if len(files) != 0 {
		t.Fatalf("memory dir has %d files (%v); want 0 (a non-qualifying outcome writes nothing)", len(files), files)
	}
}

// TestMemoryWriteFailedStalledIsQualifying: on a Failed:Stalled outcome
// (failed-stalled), the entrypoint writes the eligible lessons (the plan's
// eligibility rule includes Failed:Stalled).
func TestMemoryWriteFailedStalledIsQualifying(t *testing.T) {
	ws := t.TempDir()
	mem := t.TempDir()
	writeLessonsFile(t, ws, []string{"foo/bar: the retry must back off"})
	code, err := runMemoryWrite(t, ws, mem, "failed-stalled")
	if code != 0 || err != nil {
		t.Fatalf("run(failed-stalled) = (%d, %v); want (0, nil)", code, err)
	}
	files := readMemoryFiles(t, mem)
	if len(files) != 1 {
		t.Fatalf("memory dir has %d files (%v); want 1", len(files), files)
	}
}

// TestMemoryWriteMissingFileWritesNothing: a missing lessons file (the
// runner proposed nothing — a fresh run) is a valid "nothing to write": zero
// total, exit 0, the memory dir is empty.
func TestMemoryWriteMissingFileWritesNothing(t *testing.T) {
	ws := t.TempDir() // no .coxswain/lessons.json (the runner proposed nothing)
	mem := t.TempDir()
	code, err := runMemoryWrite(t, ws, mem, "succeeded")
	if code != 0 || err != nil {
		t.Fatalf("run(succeeded, no file) = (%d, %v); want (0, nil)", code, err)
	}
	files := readMemoryFiles(t, mem)
	if len(files) != 0 {
		t.Fatalf("memory dir has %d files (%v); want 0", len(files), files)
	}
}

// TestMemoryWriteWriteFailureExitsNonZero: a write failure (the memory dir
// is not writable — a read-only dir) exits non-zero (the Job's failure is the
// operator's MemoryWriteFailed evidence). The bounded error on stderr does
// NOT include the lesson text.
func TestMemoryWriteWriteFailureExitsNonZero(t *testing.T) {
	ws := t.TempDir()
	// A read-only memory dir (the write fails — the PVC is not mounted
	// writable, or the disk is full).
	mem := t.TempDir()
	if err := os.Chmod(mem, 0o500); err != nil { // read+execute, no write
		t.Fatalf("chmod memory dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(mem, 0o755) })
	writeLessonsFile(t, ws, []string{"foo/bar: the retry must back off"})
	code, err := runMemoryWrite(t, ws, mem, "succeeded")
	if code == 0 {
		t.Fatalf(
			"run(succeeded, read-only memory dir) = (0, %v); want non-zero exit (a write failure is the Job's failure)",
			err,
		)
	}
	// The error must NOT include the lesson text (a write-failure error is a
	// diagnostic, not a lesson echo).
	if err != nil && strings.Contains(err.Error(), "the retry must back off") {
		t.Errorf("error includes the lesson text (%q); a write-failure error must not echo a lesson", err.Error())
	}
}

// TestMemoryWriteBoundedSummaryNeverLeaksLessonText: the bounded summary
// (stdout) reports counts + per-lesson sha256-prefix/written|dropped/rule,
// NEVER the lesson text. A lesson that failed the secrets screen must not be
// echoed into the Job's logs / termination message.
func TestMemoryWriteBoundedSummaryNeverLeaksLessonText(t *testing.T) {
	ws := t.TempDir()
	mem := t.TempDir()
	// A credential lesson (dropped) + an eligible lesson. The credential
	// lesson's text must NOT appear in the stdout summary.
	cred := "the token is ghp_abcDEF1234567890abcdefghijklmnopQR"
	lessons := []string{cred, "foo/bar: ok"}
	writeLessonsFile(t, ws, lessons)
	// Capture stdout (the bounded summary) by redirecting os.Stdout to a
	// pipe (run prints the summary with fmt.Println, which writes to
	// os.Stdout at call time — capturing requires the swap to be in place
	// before run runs).
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code, err := runMemoryWrite(t, ws, mem, "succeeded")
	_ = w.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	summary := buf.String()
	if code != 0 || err != nil {
		t.Fatalf("run(succeeded) = (%d, %v); want (0, nil)", code, err)
	}
	// The credential lesson's text must NOT appear in the summary (the
	// summary reports sha256-prefix + written|dropped + rule, never the
	// lesson text).
	if strings.Contains(summary, "ghp_abcDEF1234567890abcdefghijklmnopQR") {
		t.Errorf(
			"summary includes the credential lesson's text:\n%s\nthe bounded summary must never leak a lesson's content",
			summary,
		)
	}
	// The summary must report the counts (total=2, written=1, dropped=1).
	if !strings.Contains(summary, "summary: total=2 written=1 dropped=1") {
		t.Errorf("summary does not report the counts (total=2 written=1 dropped=1):\n%s", summary)
	}
	// The credential lesson must NOT be on the memory PVC.
	files := readMemoryFiles(t, mem)
	for name, content := range files {
		if strings.Contains(content, "ghp_abcDEF1234567890") {
			t.Errorf("%s contains a credential; the secrets screen must drop it", name)
		}
	}
}
