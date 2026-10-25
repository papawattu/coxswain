package controller

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// F33 (p5-code P3-5): the bounded audit text is cut on a rune boundary, so a
// multi-byte curator reason is never split into invalid UTF-8.
func TestMemoryWriteAuditTextCutsOnARuneBoundary(t *testing.T) {
	// 999 ASCII bytes then a 3-byte rune straddling the 1000-byte cut.
	summary := strings.Repeat("a", 999) + "€" + strings.Repeat("b", 50)
	got := memoryWriteAuditText(summary)
	if !utf8.ValidString(got) {
		t.Fatalf("audit text is not valid UTF-8 after the cut: %q", got[len(got)-8:])
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a truncated audit text must end with the ellipsis")
	}
	if len(got) > 1000+len("…") {
		t.Fatalf("audit text is %d bytes, over the bound", len(got))
	}
}

// f78Summary builds a writer summary in the real shape: the counts line first,
// an eviction line, then n per-lesson lines (each with a long curator reason).
func f78Summary(n int) (string, []string) {
	lines := []string{fmt.Sprintf("summary: total=%d written=%d dropped=0", n, n), "evicted 0123456789ab cap"}
	for i := range n {
		lines = append(lines, fmt.Sprintf("lesson-%03d %012x written curator-keep: %s", i+1, i, strings.Repeat("r", 60)))
	}
	return strings.Join(lines, "\n"), lines
}

// TestMemoryWriteAuditTextKeepsCountsLineWhenOver1000Bytes (F78): a summary
// over 1000 bytes still shows the counts line (it is first), and ends on a
// whole line plus the "more lines" note, never a mid-line fragment.
func TestMemoryWriteAuditTextKeepsCountsLineWhenOver1000Bytes(t *testing.T) {
	summary, lines := f78Summary(20)
	if len(summary) <= 1000 {
		t.Fatalf("setup: summary is %d bytes, want > 1000", len(summary))
	}
	got := memoryWriteAuditText(summary)
	if !strings.HasPrefix(got, "summary: total=20 written=20 dropped=0") {
		t.Fatalf("the counts line must lead the audit text, got: %q", got)
	}
	if !strings.Contains(got, "evicted 0123456789ab cap") {
		t.Fatalf("the eviction line must survive the cut, got: %q", got)
	}
	i := strings.Index(got, " … ")
	if i < 0 || !strings.HasSuffix(got, " more lines") {
		t.Fatalf("a cut text must end with the %q note, got: %q", " … N more lines", got)
	}
	// The kept part is exactly the first k whole lines.
	kept := strings.Split(got[:i], " lesson-")
	k := len(kept) - 1 + 2 // counts + evicted + the lesson lines
	if want := strings.Join(lines[:k], " "); got[:i] != want {
		t.Fatalf("the kept text is not a whole-line prefix:\n got %q\nwant %q", got[:i], want)
	}
	var more int
	if _, err := fmt.Sscanf(got[i:], " … %d more lines", &more); err != nil || more != len(lines)-k {
		t.Fatalf("note says %d more lines (err %v), want %d", more, err, len(lines)-k)
	}
	if len(got) > 1000 {
		t.Fatalf("audit text is %d bytes, over the 1000 bound", len(got))
	}
}

// TestMemoryWriteAuditTextShortSummaryUntouched (F78): a summary under the cap
// is joined whole, with no note.
func TestMemoryWriteAuditTextShortSummaryUntouched(t *testing.T) {
	summary, lines := f78Summary(3)
	if got, want := memoryWriteAuditText(summary), strings.Join(lines, " "); got != want {
		t.Fatalf("short summary altered:\n got %q\nwant %q", got, want)
	}
}
