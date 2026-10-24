package controller

import (
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
