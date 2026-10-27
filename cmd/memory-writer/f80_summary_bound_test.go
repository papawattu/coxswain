// F80 (pi3 .samples/review-postmerge-4.md P2-4; design .samples/design-f74-f82.md
// §PR D and .samples/design-f79-f82-refresh.md §F80): the memory writer's summary was
// not bounded. Kubernetes caps a container's termination message at 4096 bytes and cuts
// anything longer SILENTLY, so a 20-lesson batch with long curator reasons arrived
// truncated and unmarked. The fix bounds the termination-message COPY to 4000 bytes
// (whole lines + "… N more lines") and leaves stdout whole — the Job log is where the
// complete per-lesson audit is read (TROUBLESHOOTING says so, and
// FallbackToLogsOnError makes the log tail a crash's message).

package main

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// f80Summary builds a summary bigger than the kubelet's 4096-byte cap: 20 lessons
// whose curator reasons are the 200-rune cap (pkg/curator), plus two audit lines.
func f80Summary() []string {
	const lessons = 20
	const reasonLen = 200
	bodies := make([]string, lessons)
	reasons := make([]string, lessons)
	for i := range bodies {
		bodies[i] = "a lesson body that is not echoed in the summary " + strconv.Itoa(i)
		reasons[i] = "curator-dropped: " + strings.Repeat("x", reasonLen)
	}
	audit := []string{"audit evicted 0123456789ab cap", "audit batch truncated (20 of 25 processed)"}
	return summaryLines(bodies, reasons, audit, 25, lessons, lessons)
}

// f80EmitStdout calls emitSummary with stdout captured, and returns the full
// stdout bytes (the same os.Pipe idiom the other specs use).
func f80EmitStdout(t *testing.T, lines []string) []byte {
	t.Helper()
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	emitSummary(lines)
	if err := w.Close(); err != nil {
		t.Fatalf("close the stdout pipe: %v", err)
	}
	os.Stdout = oldStdout
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	return out
}

// TestSummaryFileBoundedAtKubeletCap is the F80 detector: a summary over the
// kubelet's cap is written to COX_SUMMARY_PATH bounded and MARKED, while stdout
// keeps every line.
func TestSummaryFileBoundedAtKubeletCap(t *testing.T) {
	sumPath := filepath.Join(t.TempDir(), "summary.txt")
	t.Setenv("COX_SUMMARY_PATH", sumPath)

	lines := f80Summary()
	full := renderSummary(lines)
	if len(full) <= 4096 {
		t.Fatalf("fixture too small: the full summary is %d bytes; the point is to exceed "+
			"the kubelet's 4096-byte cap", len(full))
	}

	stdout := f80EmitStdout(t, lines)
	fileData, err := os.ReadFile(sumPath)
	if err != nil {
		t.Fatalf("read the summary file: %v", err)
	}

	if len(fileData) > summaryFileMaxBytes {
		t.Errorf("the termination-message copy is %d bytes, want <= %d (the kubelet cuts at 4096)",
			len(fileData), summaryFileMaxBytes)
	}
	fileLines := strings.Split(strings.TrimSuffix(string(fileData), "\n"), "\n")
	if want := summaryLine(25, 20, 20); fileLines[0] != want {
		t.Errorf("the first line is %q, want the counts line %q (a capped summary must still report the counts)",
			fileLines[0], want)
	}
	last := fileLines[len(fileLines)-1]
	marker := regexp.MustCompile(`^… (\d+) more lines \((\d+) lesson lines\)$`)
	m := marker.FindStringSubmatch(last)
	if m == nil {
		t.Fatalf("the last line is %q, want the truncation marker %q", last, `… N more lines (M lesson lines)`)
	}
	dropped, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse the marker's count: %v", err)
	}
	lessonsCut, err := strconv.Atoi(m[2])
	if err != nil {
		t.Fatalf("parse the marker's lesson count: %v", err)
	}
	kept := len(fileLines) - 1
	if kept+dropped != len(lines) {
		t.Errorf("kept %d + dropped %d = %d, want the %d lines of the summary (never a lost line)",
			kept, dropped, kept+dropped, len(lines))
	}
	// F80b: the marker names how many of the cut lines were PER-LESSON lines,
	// because the operator derives the curator/static drop split from them and
	// must be able to tell a complete split from a cut one.
	wantLessonsCut := 0
	for _, line := range lines[kept:] {
		if strings.HasPrefix(line, lessonLinePrefix) {
			wantLessonsCut++
		}
	}
	if lessonsCut != wantLessonsCut {
		t.Errorf("the marker says %d lesson lines were cut, want %d (of the %d cut lines)",
			lessonsCut, wantLessonsCut, len(lines)-kept)
	}
	if wantLessonsCut == 0 {
		t.Fatalf("fixture cut no per-lesson line: the point is that the verdict split is the part that is lost")
	}
	// Whole lines only: every kept line is the corresponding summary line, and
	// the last kept line is not a partial one.
	for i := range kept {
		if fileLines[i] != lines[i] {
			t.Errorf("kept line %d = %q, want the whole summary line %q", i, fileLines[i], lines[i])
		}
	}
	if strings.HasSuffix(fileLines[kept-1], "…") {
		t.Errorf("the last kept line %q looks like a byte cut; the cut is line-oriented", fileLines[kept-1])
	}
	// The "keep stdout whole" half of the design, which nothing else pins.
	if string(stdout) != string(full) {
		t.Errorf("stdout is not the FULL summary (%d vs %d bytes): the Job log is where the complete audit is read",
			len(stdout), len(full))
	}
	if len(stdout) <= len(fileData) {
		t.Errorf("stdout (%d bytes) is not longer than the bounded file (%d bytes)", len(stdout), len(fileData))
	}
}

// TestSummaryFileUnderCapIsUnchanged: a summary under the cap is written
// byte-identical and MARKER-FREE (the bound must not churn the normal case).
func TestSummaryFileUnderCapIsUnchanged(t *testing.T) {
	sumPath := filepath.Join(t.TempDir(), "summary.txt")
	t.Setenv("COX_SUMMARY_PATH", sumPath)

	lines := summaryLines(
		[]string{"a clean lesson", "a lesson with a token ghp_abcDEF1234567890abcdefghijklmnopQR"},
		[]string{"", "drop: secret"},
		[]string{"audit evicted 0123456789ab cap"},
		2, 1, 1,
	)
	full := renderSummary(lines)
	if len(full) > summaryFileMaxBytes {
		t.Fatalf("fixture too big: %d bytes", len(full))
	}
	stdout := f80EmitStdout(t, lines)
	fileData, err := os.ReadFile(sumPath)
	if err != nil {
		t.Fatalf("read the summary file: %v", err)
	}
	if string(fileData) != string(full) {
		t.Errorf("an under-cap file differs from the full summary:\nfile:   %q\nfull:   %q", string(fileData), string(full))
	}
	if string(stdout) != string(full) {
		t.Errorf("under the cap, stdout and the file (and the full summary) must be identical")
	}
	if strings.Contains(string(fileData), "more lines") {
		t.Errorf("an under-cap file carries a truncation marker:\n%s", string(fileData))
	}
}

// TestBoundSummaryKeepsWholeLines pins the budget arithmetic: the marker is
// reserved before each line is added (a kept line can never crowd it out), the
// counts line survives a huge second line, and the output is whole lines only.
// The fixture's output is fully determined, so this asserts EQUALITY: a byte cut
// (of any line, of any bytes) cannot produce it.
func TestBoundSummaryKeepsWholeLines(t *testing.T) {
	const counts = "summary: total=2 written=1 dropped=1"
	huge := lessonLinePrefix + "002 " + strings.Repeat("y", 5000)
	lines := []string{counts, huge}

	got := string(boundSummary(lines, summaryFileMaxBytes))
	if want := counts + "\n… 1 more lines (1 lesson lines)\n"; got != want {
		t.Fatalf("boundSummary = %q, want %q (the counts line, then the marker)", got, want)
	}

	// Exact-fit boundary: the counts line plus its newline, with nothing else,
	// fits exactly; one byte less must drop it and emit only the marker.
	exact := len(counts) + 1
	if out := string(boundSummary([]string{counts}, exact)); out != counts+"\n" {
		t.Errorf("a summary that fits exactly was changed:\n%q", out)
	}
	if out := string(boundSummary([]string{counts}, exact-1)); out != "… 1 more lines (0 lesson lines)\n" {
		t.Errorf("one byte short: got %q, want only the marker", out)
	}
}

// TestBoundSummaryMultiByteIsByteBounded (F80 F4) closes the gap the ASCII
// fixtures leave: the bound counts BYTES and cuts WHOLE LINES, so a summary of
// 3-byte runes must (a) still fit the byte budget — a rune-counting bound would
// keep this line — and (b) stay valid UTF-8, with no line cut mid-rune.
func TestBoundSummaryMultiByteIsByteBounded(t *testing.T) {
	const counts = "summary: total=2 written=1 dropped=1"
	// 1400 three-byte runes = 4200 bytes: over the BYTE budget, well under it
	// counted as runes.
	huge := lessonLinePrefix + "002 " + strings.Repeat("数", 1400)
	if n := utf8.RuneCountInString(huge); n >= summaryFileMaxBytes {
		t.Fatalf("fixture is not discriminating: %d runes against a %d rune budget", n, summaryFileMaxBytes)
	}
	if len(huge) <= summaryFileMaxBytes {
		t.Fatalf("fixture is not discriminating: %d bytes against a %d byte budget", len(huge), summaryFileMaxBytes)
	}
	lines := []string{counts, huge}

	got := string(boundSummary(lines, summaryFileMaxBytes))
	if len(got) > summaryFileMaxBytes {
		t.Errorf("boundSummary returned %d bytes, want <= %d (the budget is in BYTES)",
			len(got), summaryFileMaxBytes)
	}
	if !utf8.ValidString(got) {
		t.Errorf("boundSummary returned invalid UTF-8 (a rune was split):\n%q", got)
	}
	// Whole lines only: every output line is a kept summary line, and the last
	// is the marker.
	out := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if out[0] != counts {
		t.Errorf("the first line is %q, want the counts line %q", out[0], counts)
	}
	if want := "… 1 more lines (1 lesson lines)"; out[len(out)-1] != want {
		t.Errorf("the last line is %q, want the marker %q", out[len(out)-1], want)
	}
	for i, line := range out[:len(out)-1] {
		if line != lines[i] {
			t.Errorf("output line %d = %q, want the whole summary line %q", i, line, lines[i])
		}
	}
}
