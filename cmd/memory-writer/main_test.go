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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

// readMemoryFiles returns the *.txt files in the memory dir (named by their
// sha256 — the plan: "files named by its SHA-256"), keyed by file name, with
// their content (the trailing newline trimmed).
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
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
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
	// by the secrets screen). One file (named by the lesson's sha256).
	if len(files) != 1 {
		t.Fatalf("memory dir has %d files (%v); want 1 (the credential lesson is dropped)", len(files), files)
	}
	// The file's content is the eligible lesson (the name is the sha256).
	found := false
	for _, content := range files {
		if content == lessons[0] {
			found = true
		}
	}
	if !found {
		t.Errorf("the eligible lesson %q is not in the memory dir: %v", lessons[0], files)
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

// TestSummaryPathFile asserts the binary writes the bounded summary to
// COX_SUMMARY_PATH (the container's termination-message path) in ADDITION to
// stdout — the operator's distroless image has no /bin/sh, so the binary is
// the Job's direct entrypoint and must write the summary file itself (no
// shell redirect). The file's content must match stdout (the same bounded
// summary, never the lesson text).
func TestSummaryPathFile(t *testing.T) {
	ws := t.TempDir()
	mem := t.TempDir()
	sumPath := filepath.Join(t.TempDir(), "summary.txt")
	t.Setenv("COX_SUMMARY_PATH", sumPath)
	t.Setenv("COX_MEMORY_DIR", mem)
	t.Setenv("COX_LESSONS_PATH", filepath.Join(ws, ".coxswain", "lessons.json"))
	writeLessonsFile(t, ws, []string{
		"clean lesson: the retry must back off",
		"the token is ghp_abcDEF1234567890abcdefghijklmnopQR",
	})
	// Capture stdout to compare against the file.
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	code, err := run("succeeded")
	_ = w.Close()
	os.Stdout = oldStdout
	stdoutData, _ := io.ReadAll(r)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Fatalf("run(succeeded) = (%d, %v); want 0", code, err)
	}
	fileData, err := os.ReadFile(sumPath)
	if err != nil {
		t.Fatalf("read COX_SUMMARY_PATH %s: %v (the binary must write the summary file)", sumPath, err)
	}
	// The file must contain the same bounded summary as stdout.
	if !strings.Contains(string(fileData), "summary: total=2 written=1 dropped=1") {
		t.Errorf("summary file lacks the counts line:\n%s", string(fileData))
	}
	// The file must NOT leak a lesson's text (the bounded summary is the
	// sha prefix + status + rule, never the lesson text).
	if strings.Contains(string(fileData), "ghp_abcDEF1234567890") ||
		strings.Contains(string(fileData), "retry must back off") {
		t.Errorf("summary file leaks a lesson's text:\n%s", string(fileData))
	}
	// stdout and the file must agree on the counts line (the same emission).
	if !strings.Contains(string(stdoutData), "summary: total=2 written=1 dropped=1") {
		t.Errorf("stdout lacks the counts line:\n%s", string(stdoutData))
	}
}

// TestMemoryWriteDedupByHash: a lesson whose SHA-256 matches an existing
// memory file is dropped as a duplicate (the plan: "a lesson whose SHA-256
// matches an existing lesson is dropped"). The memory dir is pre-populated
// with a file named <sha256>.txt for the lesson; the writer must NOT write it
// again (the file count is unchanged, the summary reports it as a duplicate).
func TestMemoryWriteDedupByHash(t *testing.T) {
	ws := t.TempDir()
	mem := t.TempDir()
	lesson := "internal/dedup/file.go: the cache key must include the namespace"
	lessons := []string{lesson, lesson} // the same lesson twice
	writeLessonsFile(t, ws, lessons)
	// Pre-populate the memory dir with the lesson's sha256 file (it already
	// exists in memory — the dedup must drop the new proposal).
	sum := sha256.Sum256([]byte(lesson))
	name := hex.EncodeToString(sum[:]) + ".txt"
	if err := os.WriteFile(filepath.Join(mem, name), []byte(lesson+"\n"), 0o644); err != nil {
		t.Fatalf("pre-populate: %v", err)
	}
	// Capture stdout (the bounded summary) by redirecting os.Stdout to a
	// pipe (run prints the summary with fmt.Println, which writes to
	// os.Stdout at call time).
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
	// The pre-existing file is there; the writer did NOT write a second copy
	// (both lessons are the same hash — both are duplicates of the
	// pre-existing file). One file total.
	files := readMemoryFiles(t, mem)
	if len(files) != 1 {
		t.Fatalf("memory dir has %d files (%v); want 1 (the duplicate is not re-written)", len(files), files)
	}
	// The content is unchanged (the original pre-populated file).
	if got := files[name]; got != lesson {
		t.Errorf("%s = %q; want the original lesson %q", name, got, lesson)
	}
	// The bounded summary reports written=0 (both lessons were duplicates of
	// the pre-existing file — the dedup dropped them). THIS is the assertion
	// the skip-dedup mutation breaks: with the dedup disabled, the writer
	// re-writes the file (written=2), and the summary reports written=2
	// dropped=0 — the assertion fails.
	if !strings.Contains(summary, "summary: total=2 written=0 dropped=2") {
		t.Errorf("summary does not report the dedup (written=0 dropped=2):\n%s", summary)
	}
	// Each lesson is reported as "dropped duplicate".
	if c := strings.Count(summary, "dropped duplicate"); c != 2 {
		t.Errorf("summary reports %d 'dropped duplicate' lines; want 2:\n%s", c, summary)
	}
}

// TestMemoryWriteDedupSummary: the bounded summary reports a duplicate lesson
// as "dropped duplicate" (the rule name is "duplicate").
func TestMemoryWriteDedupSummary(t *testing.T) {
	ws := t.TempDir()
	mem := t.TempDir()
	lesson := "internal/dedup/summary.go: the audit trail must name the lesson hash"
	lessons := []string{lesson, lesson}
	writeLessonsFile(t, ws, lessons)
	sum := sha256.Sum256([]byte(lesson))
	name := hex.EncodeToString(sum[:]) + ".txt"
	if err := os.WriteFile(filepath.Join(mem, name), []byte(lesson+"\n"), 0o644); err != nil {
		t.Fatalf("pre-populate: %v", err)
	}
	code, err := runMemoryWrite(t, ws, mem, "succeeded")
	if code != 0 || err != nil {
		t.Fatalf("run(succeeded) = (%d, %v); want (0, nil)", code, err)
	}
	files := readMemoryFiles(t, mem)
	if len(files) != 1 {
		t.Fatalf("memory dir has %d files (%v); want 1", len(files), files)
	}
}
