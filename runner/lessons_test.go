package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P5b's lessons transport specs (the runner half): parseLessons (the LESSON:
// line parser), capLessons (the shape/count caps), and writeLessonsFile (the
// workspace file write, the plan's "a workspace file, not the 4096-byte
// claim"). The operator's static policy (pkg/memorywrite) is the other half
// (its specs live there). The plan's acceptance for the transport: "the
// lessons transport (a workspace file, not the 4096-byte claim) and the size
// cap (≤500 chars) all have specs".

// TestParseLessonsNone: a reply with no LESSON: lines yields no lessons
// (a fresh run with nothing to propose).
func TestParseLessonsNone(t *testing.T) {
	got := parseLessons("I did the thing and ran the tests. All pass.")
	if len(got) != 0 {
		t.Fatalf("no LESSON lines: got %v, want empty", got)
	}
}

// TestParseLessonsBasic: a reply with two LESSON: lines yields both, in
// reply order (the operator persists them as lesson-001.txt, lesson-002.txt).
func TestParseLessonsBasic(t *testing.T) {
	reply := ("made the change and verified it.\n" +
		"LESSON: the build order is compile, test, deploy\n" +
		"LESSON: the verify image is golang:1.26\n" +
		"Done.")
	got := parseLessons(reply)
	if len(got) != 2 {
		t.Fatalf("basic: got %d lessons, want 2: %v", len(got), got)
	}
	if got[0] != "the build order is compile, test, deploy" || got[1] != "the verify image is golang:1.26" {
		t.Fatalf("basic: got %v", got)
	}
}

// TestParseLessonsIgnoresNonLessonLines: lines that merely CONTAIN "LESSON:"
// mid-line are NOT lessons (the instruction says a line that STARTS with
// LESSON: after whitespace; a mid-line mention is prose).
func TestParseLessonsIgnoresNonLessonLines(t *testing.T) {
	reply := "the LESSON: prefix is the convention.\nLESSON: only this is a lesson\nsee the lesson doc for details"
	got := parseLessons(reply)
	if len(got) != 1 || got[0] != "only this is a lesson" {
		t.Fatalf("mid-line: got %v, want the one true lesson", got)
	}
}

// TestParseLessonsTrimsWhitespace: leading whitespace before LESSON: and
// trailing whitespace after the text are trimmed.
func TestParseLessonsTrimsWhitespace(t *testing.T) {
	got := parseLessons("   LESSON:   the padded lesson   ")
	if len(got) != 1 || got[0] != "the padded lesson" {
		t.Fatalf("trim: got %v", got)
	}
}

// TestParseLessonsEmptyText: a `LESSON:` line with no text is not a lesson.
func TestParseLessonsEmptyText(t *testing.T) {
	got := parseLessons("LESSON:\nLESSON: real lesson")
	if len(got) != 1 || got[0] != "real lesson" {
		t.Fatalf("empty text: got %v", got)
	}
}

// TestCapLessonsBlankDropped: blank or whitespace-only proposals are dropped
// (the operator's screen would drop them as shape; the runner drops them at
// proposal time too — a blank lesson is never written).
func TestCapLessonsBlankDropped(t *testing.T) {
	got := capLessons([]string{"", "   ", "real lesson"})
	if len(got) != 1 || got[0] != "real lesson" {
		t.Fatalf("blank: got %v", got)
	}
}

// TestCapLessonsMultiLineCut: a multi-line proposal is cut to its first line
// (a newline would corrupt the flat memory dir; the rest is dropped).
func TestCapLessonsMultiLineCut(t *testing.T) {
	got := capLessons([]string{"first line\nsecond line"})
	if len(got) != 1 || got[0] != "first line" {
		t.Fatalf("multiline: got %v", got)
	}
}

// TestCapLessonsSizeCap: a proposal over maxLessonChars runes is truncated to
// the cap (P5b's size cap, ≤500 chars, the plan's named spec). Exactly 500 is
// kept; 501 is cut to 500.
func TestCapLessonsSizeCap(t *testing.T) {
	at500 := strings.Repeat("é", 500)
	at501 := strings.Repeat("x", 501)
	got := capLessons([]string{at500, at501})
	if len(got) != 2 {
		t.Fatalf("size cap: got %d lessons, want 2", len(got))
	}
	if r := []rune(got[0]); len(r) != 500 {
		t.Fatalf("size cap: 500-rune lesson has %d runes", len(r))
	}
	if r := []rune(got[1]); len(r) != 500 {
		t.Fatalf("size cap: 501-rune lesson was not cut to 500 (got %d)", len(r))
	}
}

// TestCapLessonsCountCap: more than maxLessons proposals are capped to the
// FIRST maxLessons (a bounded proposal is better than a truncated one).
func TestCapLessonsCountCap(t *testing.T) {
	in := make([]string, 0, maxLessons+5)
	for i := range maxLessons + 5 {
		in = append(in, "lesson "+string(rune('a'+i)))
	}
	got := capLessons(in)
	if len(got) != maxLessons {
		t.Fatalf("count cap: got %d lessons, want %d", len(got), maxLessons)
	}
	if got[0] != "lesson a" || got[maxLessons-1] != "lesson j" {
		t.Fatalf("count cap: first=%q last=%q, want lesson a / lesson j", got[0], got[maxLessons-1])
	}
}

// TestWriteLessonsFileJSON: writeLessonsFile writes a JSON array of strings
// to <workspace>/.coxswain/lessons.json (the plan's "a workspace file"), with
// the 0o644 mode and the parent dir created. The file is NOT the 4096-byte
// claim (it lives on the workspace volume; the claim is /dev/termination-log).
func TestWriteLessonsFileJSON(t *testing.T) {
	ws := t.TempDir()
	writeLessonsFile(ws, []string{"the first lesson", "the second lesson"})
	path := filepath.Join(ws, resultDirName, lessonsFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lessons.json: %v", err)
	}
	var got []string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("lessons.json is not a JSON array of strings: %v", err)
	}
	if len(got) != 2 || got[0] != "the first lesson" || got[1] != "the second lesson" {
		t.Fatalf("lessons.json: got %v", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("lessons.json mode: got %v, want 0644", info.Mode().Perm())
	}
}

// TestWriteLessonsFileEmptyNoFile: a proposal with no lessons (all dropped at
// the cap) writes NO file (the operator finds no file and persists nothing —
// a fresh run with nothing to persist).
func TestWriteLessonsFileEmptyNoFile(t *testing.T) {
	ws := t.TempDir()
	writeLessonsFile(ws, []string{"", "   "})
	path := filepath.Join(ws, resultDirName, lessonsFileName)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("no-lessons proposal: a file was written (err=%v), want none", err)
	}
}

// TestWriteLessonsFileNonFatal: a write failure (a read-only dir) does not
// panic or return (the function is non-fatal by design — ADR-0004: a failed
// memory write does not fail the Loop; the file simply is absent).
func TestWriteLessonsFileNonFatal(t *testing.T) {
	ws := t.TempDir()
	// Make the .coxswain dir read-only (the MkdirAll/WriteFile will fail).
	coxDir := filepath.Join(ws, resultDirName)
	if err := os.MkdirAll(coxDir, 0o555); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A write failure must not panic (the function logs and returns). The
	// test is in the same process, so a panic would fail it.
	writeLessonsFile(ws, []string{"the lesson"})
}

// TestLessonsPromptSectionPresent: the implementing prompt carries the
// lessons section (the runner's proposal channel is always present — the
// plan's spec that the transport is a prompt-instructed workspace file, not
// an implicit claim field).
func TestLessonsPromptSectionPresent(t *testing.T) {
	ws := t.TempDir()
	got := implementingPrompt(ws, "do the thing")
	if !strings.Contains(got, "LESSON:") {
		t.Fatalf("implementing prompt does not mention the LESSON: convention:\n%s", got)
	}
	if !strings.Contains(got, "500") {
		t.Fatalf("implementing prompt does not state the 500-char cap:\n%s", got)
	}
}

// TestLessonsNotInPlanningPrompt: the lessons transport is IMPLEMENTING-only
// (the lessons are what the run LEARNED while implementing; the planning
// prompt is unchanged by P5b — the P5a spec "unchanged when absent" still
// holds for the planning prompt).
func TestLessonsNotInPlanningPrompt(t *testing.T) {
	ws := t.TempDir()
	got := planningPrompt(ws, "do the thing", planMaxBytes)
	if strings.Contains(got, "LESSON:") {
		t.Fatalf("planning prompt carries the lessons section (P5b is implementing-only):\n%s", got)
	}
}
