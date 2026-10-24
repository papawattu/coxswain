package controller

import "testing"

// TestMemoryWriteVerdicts (F30): the summary's per-lesson lines map to the
// three verdicts; the counts line and blank/garbage lines are ignored.
func TestMemoryWriteVerdicts(t *testing.T) {
	kept, static, cur := memoryWriteVerdicts(f30Summary)
	if kept != 2 || static != 2 || cur != 2 {
		t.Fatalf("kept=%d static=%d curator=%d, want 2/2/2", kept, static, cur)
	}
	if k, s, c := memoryWriteVerdicts(""); k+s+c != 0 {
		t.Fatalf("empty summary counted %d/%d/%d", k, s, c)
	}
}

// TestMemoryWriteVerdictsDuplicateInBatch (F30, reviewer finding P3-2): two
// identical lessons in one batch make both per-lesson lines read "written"
// while the counts line says written=1 dropped=1. The verdicts follow the
// counts line (kept=1) and the extra drop is a static drop. (The summary
// itself is F33's to fix; this pins that the metric does not inherit it.)
func TestMemoryWriteVerdictsDuplicateInBatch(t *testing.T) {
	summary := "lesson-001 aaaaaaaaaaaa written eligible\n" +
		"lesson-002 aaaaaaaaaaaa written eligible\n" +
		"summary: total=2 written=1 dropped=1"
	kept, static, cur := memoryWriteVerdicts(summary)
	if kept != 1 || static != 1 || cur != 0 {
		t.Fatalf("kept=%d static=%d curator=%d, want 1/1/0", kept, static, cur)
	}
	// No counts line: fall back to the per-lesson lines.
	kept, static, cur = memoryWriteVerdicts("lesson-001 aaaaaaaaaaaa written eligible")
	if kept != 1 || static != 0 || cur != 0 {
		t.Fatalf("fallback kept=%d static=%d curator=%d, want 1/0/0", kept, static, cur)
	}
}
