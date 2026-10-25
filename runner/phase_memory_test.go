package runner

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P5a (Phase 5 shared memory, PLAN-PHASE5.md:34): the prompt includes the
// contents of .coxswain/memory/ when present, and is unchanged when absent
// (a fresh repo).
//
// writeMemoryFile is a test helper: write a memory lesson file into the
// workspace's .coxswain/memory/ dir.
func writeMemoryFile(t *testing.T, workspace, name, content string) {
	t.Helper()
	dir := filepath.Join(workspace, resultDirName, memoryDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir memory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write memory file: %v", err)
	}
}

// TestPlanningPromptIncludesMemoryWhenPresent: a memory file present in
// .coxswain/memory/ is included in the planning prompt (the P5a spec: "the
// prompt includes the contents of .coxswain/memory/ when present").
func TestPlanningPromptIncludesMemoryWhenPresent(t *testing.T) {
	ws := t.TempDir()
	writeMemoryFile(t, ws, "abc123def456abcdef", "the go build fills /tmp; set GOTMPDIR to a larger volume")

	got := planningPrompt(ws, "do the thing", planMaxBytes)
	if !strings.Contains(got, "SHARED MEMORY") {
		t.Fatalf("planning prompt missing the SHARED MEMORY header:\n%s", got)
	}
	if !strings.Contains(got, "abc123def456abcdef") {
		t.Fatalf("planning prompt missing the memory file name:\n%s", got)
	}
	if !strings.Contains(got, "the go build fills /tmp; set GOTMPDIR to a larger volume") {
		t.Fatalf("planning prompt missing the lesson content:\n%s", got)
	}
}

// TestImplementingPromptIncludesMemoryWhenPresent: the implementing prompt
// includes the memory too (the P5a spec: "Planning and Implementing prompts
// both").
func TestImplementingPromptIncludesMemoryWhenPresent(t *testing.T) {
	ws := t.TempDir()
	writeMemoryFile(t, ws, "abc123def456abcdef", "use cp not mv for the workspace copy")

	got := implementingPrompt(ws, "do the thing")
	if !strings.Contains(got, "SHARED MEMORY") {
		t.Fatalf("implementing prompt missing the SHARED MEMORY header:\n%s", got)
	}
	if !strings.Contains(got, "use cp not mv for the workspace copy") {
		t.Fatalf("implementing prompt missing the lesson content:\n%s", got)
	}
}

// TestPromptUnchangedWhenMemoryAbsent (a fresh repo): with no .coxswain/
// memory dir, the PLANNING prompt is EXACTLY the pre-P5a prompt (the P5a
// spec: "unchanged when absent (a fresh repo)"). The IMPLEMENTING prompt
// carries P5b's lessons section (the lessons transport is always present —
// it is the runner's proposal channel, independent of whether the shared
// memory exists); the planning prompt is unchanged.
func TestPromptUnchangedWhenMemoryAbsent(t *testing.T) {
	ws := t.TempDir() // no .coxswain/memory dir at all.

	// Planning: the baseline prompt (no rejection feedback, no memory).
	wantPlanning := "You are in the PLANNING phase. Produce a concise plan (under " +
		"4096 bytes" +
		") to accomplish the goal below. The plan must be a summary of the steps you will take, " +
		"not the implementation itself. End your reply with the plan text (it will be written to PLAN.md).\n\nGOAL:\n" +
		"do the thing"
	if got := planningPrompt(ws, "do the thing", planMaxBytes); got != wantPlanning {
		t.Fatalf("planning prompt with absent memory differs from the pre-P5a\n"+
			"baseline:\nGOT:\n%s\nWANT:\n%s", got, wantPlanning)
	}

	// Implementing: the baseline prompt (no plan, no memory) PLUS P5b's
	// lessons section (the lessons transport is always present in the
	// implementing prompt — the runner's proposal channel).
	wantImpl := "You are in the IMPLEMENTING phase. Use your shell tool to make the goal's changes in the workspace. " +
		"Run the acceptance checks to confirm your work. " +
		"Your plan lives in .coxswain/PLAN.md; do not create a root PLAN.md. " +
		"End your reply with a summary of the files you changed and the verification you ran.\n\nGOAL:\n" +
		"do the thing" + lessonsPromptSection()
	if got := implementingPrompt(ws, "do the thing"); got != wantImpl {
		t.Fatalf("implementing prompt with absent memory differs from the P5b\n"+
			"baseline (pre-P5a + lessons section):\nGOT:\n%s\nWANT:\n%s", got, wantImpl)
	}
}

// TestPromptUnchangedWhenMemoryDirEmpty: an EMPTY .coxswain/memory dir (the
// copy-in init container creates it only when the PVC is non-empty, but a
// manual or stale empty dir must not change the prompt either).
func TestPromptUnchangedWhenMemoryDirEmpty(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, resultDirName, memoryDir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	wantPlanning := "You are in the PLANNING phase. Produce a concise plan (under " +
		"4096 bytes" +
		") to accomplish the goal below. The plan must be a summary of the steps you will take, " +
		"not the implementation itself. End your reply with the plan text (it will be written to PLAN.md).\n\nGOAL:\n" +
		"do the thing"
	if got := planningPrompt(ws, "do the thing", planMaxBytes); got != wantPlanning {
		t.Fatalf("planning prompt with an empty memory dir differs from the baseline:\n%s", got)
	}
}

// TestMemoryPromptIsSortedAndBounded: the section lists files in sorted
// order (deterministic) and caps an oversized file to memoryFileMaxBytes.
func TestMemoryPromptIsSortedAndBounded(t *testing.T) {
	ws := t.TempDir()
	writeMemoryFile(t, ws, "bbb", "second lesson")
	writeMemoryFile(t, ws, "aaa", "first lesson")
	big := strings.Repeat("x", memoryFileMaxBytes+100)
	writeMemoryFile(t, ws, "ccc", big)

	got := implementingPrompt(ws, "do the thing")
	ia := strings.Index(got, "-- aaa:")
	ib := strings.Index(got, "-- bbb:")
	ic := strings.Index(got, "-- ccc:")
	if ia == -1 || ib == -1 || ic == -1 || ia >= ib || ib >= ic {
		t.Fatalf("memory files not in sorted order (aaa<bbb<ccc): ia=%d ib=%d ic=%d\n%s", ia, ib, ic, got)
	}
	if strings.Contains(got, big) {
		t.Fatalf("oversized memory file was not capped to %d bytes", memoryFileMaxBytes)
	}
	if !strings.Contains(got, strings.Repeat("x", memoryFileMaxBytes)) {
		t.Fatalf("memory file was not included (capped content missing)")
	}
}

// TestMemorySectionStopsAtCapWholeFilesAndWarns (F32 P2-4): a memory dir whose
// total exceeds memoryPromptMaxBytes keeps the files that fit, WHOLE, omits the
// rest, and logs a warning; it no longer drops the whole section silently.
func TestMemorySectionStopsAtCapWholeFilesAndWarns(t *testing.T) {
	var logBuf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(old)

	ws := t.TempDir()
	dir := filepath.Join(ws, resultDirName, memoryDir)
	_ = os.MkdirAll(dir, 0o755)
	const n = 20 // 20 x ~1000B = ~20 KB > 16 KiB
	for i := range n {
		body := strings.Repeat(string(rune('a'+i)), 1000)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d", i)), []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	got := readMemorySection(ws)
	if got == "" {
		t.Fatal("the section was dropped wholesale; it must keep the files that fit")
	}
	if len(got) > memoryPromptMaxBytes+200 { // + the header
		t.Fatalf("section is %d bytes, over the cap %d", len(got), memoryPromptMaxBytes)
	}
	if !strings.Contains(got, "-- f00:") {
		t.Error("the first file is missing")
	}
	if strings.Contains(got, "-- f19:") {
		t.Error("the last file fit; the dir should have been cut at the cap")
	}
	// whole files only: every included file carries its full 1000 bytes
	included := 0
	for i := range n {
		if strings.Contains(got, fmt.Sprintf("-- f%02d:", i)) {
			included++
			if !strings.Contains(got, strings.Repeat(string(rune('a'+i)), 1000)) {
				t.Errorf("file f%02d is included but cut", i)
			}
		}
	}
	if included == 0 || included == n {
		t.Errorf("included %d of %d files; want some but not all", included, n)
	}
	if !strings.Contains(logBuf.String(), "memory section truncated") {
		t.Errorf("the truncation was not logged: %q", logBuf.String())
	}
}

// TestMemoryFileCapFitsAWriterLesson (F32 P2-3): the reader's per-file cap is
// >= 4 bytes per rune of the writer's 500-rune lesson cap.
func TestMemoryFileCapFitsAWriterLesson(t *testing.T) {
	if memoryFileMaxBytes < 4*500 {
		t.Fatalf("memoryFileMaxBytes=%d < 4*500: the reader would cut a lesson the writer accepted", memoryFileMaxBytes)
	}
	ws := t.TempDir()
	lesson := strings.Repeat("é", 500)  // 500 runes, 1000 bytes
	lesson4 := strings.Repeat("😀", 500) // 500 runes, 2000 bytes
	writeMemoryFile(t, ws, "a", lesson)
	writeMemoryFile(t, ws, "b", lesson4)
	got := readMemorySection(ws)
	if !strings.Contains(got, lesson) || !strings.Contains(got, lesson4) {
		t.Fatal("a 500-rune lesson was cut by the reader's per-file cap")
	}
}
