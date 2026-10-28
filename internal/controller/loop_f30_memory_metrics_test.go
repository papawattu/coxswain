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

// TestMemoryWriteVerdictsTruncatedSplit (F80b; pi3 review-postmerge-4 P2-4's F2):
// the writer bounds its termination message to 4000 bytes and cuts WHOLE lines,
// per-lesson lines first — the very lines the curator/static split comes from.
// The counts line stays authoritative, but the drops of the cut lessons cannot
// be attributed, so they must not be topped up into static-drop (dropped 20 −
// curatorSeen 2 = 18 "static", silently). The writer's marker says how many
// per-lesson lines were cut, and the split counts only the lines present.
func TestMemoryWriteVerdictsTruncatedSplit(t *testing.T) {
	const truncated = "lesson-001 aaaaaaaaaaaa dropped curator: restates the goal\n" +
		"lesson-002 bbbbbbbbbbbb dropped curator unavailable\n" +
		"… 18 more lines (18 lesson lines)\n" +
		"summary: total=20 written=0 dropped=20"
	if kept, static, cur := memoryWriteVerdicts(truncated); kept != 0 || static != 0 || cur != 2 {
		t.Fatalf("a marked summary counted kept=%d static=%d curator=%d, want 0/0/2 "+
			"(the 18 cut lessons are unattributed, not static drops)", kept, static, cur)
	}

	// Positive control: the same lines WITHOUT the marker is the pre-F80b
	// arithmetic — nothing was cut, so the 18 unaccounted drops are static.
	const unmarked = "lesson-001 aaaaaaaaaaaa dropped curator: restates the goal\n" +
		"lesson-002 bbbbbbbbbbbb dropped curator unavailable\n" +
		"summary: total=20 written=0 dropped=20"
	if kept, static, cur := memoryWriteVerdicts(unmarked); kept != 0 || static != 18 || cur != 2 {
		t.Fatalf("unmarked: kept=%d static=%d curator=%d, want 0/18/2", kept, static, cur)
	}

	// A marker that says only audit lines were cut leaves the split complete,
	// so the subtraction is exact and stays.
	const auditOnly = "lesson-001 aaaaaaaaaaaa written eligible\n" +
		"lesson-002 bbbbbbbbbbbb dropped secrets-pattern\n" +
		"… 1 more lines (0 lesson lines)\n" +
		"summary: total=2 written=1 dropped=1"
	if kept, static, cur := memoryWriteVerdicts(auditOnly); kept != 1 || static != 1 || cur != 0 {
		t.Fatalf("audit-only cut: kept=%d static=%d curator=%d, want 1/1/0", kept, static, cur)
	}

	// A marker we cannot read is the same uncertainty as one that names cut
	// lesson lines: do not invent static drops.
	const unreadable = "lesson-001 aaaaaaaaaaaa dropped curator: restates the goal\n" +
		"… 3 more lines\n" +
		"summary: total=4 written=0 dropped=4"
	if kept, static, cur := memoryWriteVerdicts(unreadable); kept != 0 || static != 0 || cur != 1 {
		t.Fatalf("unreadable marker: kept=%d static=%d curator=%d, want 0/0/1", kept, static, cur)
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
