// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// D119 step 2 unit tests: the merge (union by key, max counts, later time)
// and the size bound (lowest-count records dropped first, ties by key). The
// ConfigMap create/update and the learn-mode gate are covered in
// loop_learn_summary_envtest_test.go (envtest).

package controller

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/alerts"
	"github.com/papawattu/coxswain/internal/policy"
)

// learnSummaryTestRecord returns a distinct record with the given key fields
// and count.
func learnSummaryTestRecord(target string, count int64, ts string) alerts.Record {
	return alerts.Record{
		Time:   ts,
		Loop:   "l1",
		Source: alerts.SourceKubeArmor,
		Action: "Audit",
		Target: target,
		Detail: "operation=File, pod=l1-sandbox",
		Count:  count,
	}
}

// TestLearnSummaryMergeIsUnionByMaxCount covers the merge's core contract:
// a key present in both existing and new is the union — Count is
// max(existing, new); Time is the later of the two. A key present in only
// one side is copied verbatim.
//
// Mutation m3: count = new instead of max in the merge → the "existing
// count wins when it is larger" case fails (the merged count equals the new
// count, not the existing one).
func TestLearnSummaryMergeIsUnionByMaxCount(t *testing.T) {
	existing := []alerts.Record{
		learnSummaryTestRecord("/tmp/a", 5, "2026-11-08T10:00:00Z"),
		learnSummaryTestRecord("/tmp/b", 3, "2026-11-08T11:00:00Z"),
	}
	newRecs := []alerts.Record{
		// Same key as existing[0] (/tmp/a): a LOWER count (3 < 5) and an
		// EARLIER time. The merge must keep 5 (the max) and the later time
		// (existing's 11:00:00Z? no — existing is 10:00:00Z; new is 09:00:00Z
		// here, so existing's 10:00:00Z wins).
		learnSummaryTestRecord("/tmp/a", 3, "2026-11-08T09:00:00Z"),
		// Same key as existing[1] (/tmp/b): a HIGHER count (10 > 3) and a
		// LATER time. The merge must take 10 and the later time.
		learnSummaryTestRecord("/tmp/b", 10, "2026-11-09T00:00:00Z"),
		// A new key not in existing.
		learnSummaryTestRecord("/tmp/c", 1, "2026-11-10T00:00:00Z"),
	}
	merged := mergeLearnRecords(existing, newRecs)
	if len(merged) != 3 {
		t.Fatalf("merged has %d records, want 3 (the union of 2 + 3 with 2 shared keys)", len(merged))
	}
	// The merge is sorted by key; the targets here are unique so the order
	// is /tmp/a, /tmp/b, /tmp/c.
	if merged[0].Target != "/tmp/a" || merged[1].Target != "/tmp/b" || merged[2].Target != "/tmp/c" {
		t.Fatalf("merged order = %v %v %v, want /tmp/a /tmp/b /tmp/c (key-sorted)", merged[0].Target, merged[1].Target, merged[2].Target)
	}
	// /tmp/a: existing count 5 beats new count 3; existing time 10:00 beats
	// new time 09:00.
	if merged[0].Count != 5 {
		t.Errorf("/tmp/a merged count = %d, want 5 (max(existing 5, new 3) — mutation m3 takes new instead of max)", merged[0].Count)
	}
	if merged[0].Time != "2026-11-08T10:00:00Z" {
		t.Errorf("/tmp/a merged time = %q, want the existing's 10:00:00Z (later than new's 09:00:00Z)", merged[0].Time)
	}
	// /tmp/b: new count 10 beats existing count 3; new time 11-09 beats
	// existing's 11-08.
	if merged[1].Count != 10 {
		t.Errorf("/tmp/b merged count = %d, want 10 (max(existing 3, new 10))", merged[1].Count)
	}
	if merged[1].Time != "2026-11-09T00:00:00Z" {
		t.Errorf("/tmp/b merged time = %q, want the new's 11-09 (later than existing's 11-08)", merged[1].Time)
	}
	// /tmp/c: present only in new, copied verbatim.
	if merged[2].Count != 1 || merged[2].Time != "2026-11-10T00:00:00Z" {
		t.Errorf("/tmp/c merged = count %d time %q, want count 1 time 11-10 (copied verbatim from new)", merged[2].Count, merged[2].Time)
	}
}

// TestLearnSummaryMergeEmptySides covers the degenerate cases: empty
// existing (the create path), empty new (should not happen — recordLearnSummary
// bails on ok=false — but the merge itself must not panic), and both empty.
func TestLearnSummaryMergeEmptySides(t *testing.T) {
	newRecs := []alerts.Record{learnSummaryTestRecord("/tmp/a", 1, "2026-11-08T10:00:00Z")}

	merged := mergeLearnRecords(nil, newRecs)
	if len(merged) != 1 || merged[0].Target != "/tmp/a" {
		t.Errorf("merge(nil, new) = %+v, want the new record verbatim", merged)
	}

	existing := []alerts.Record{learnSummaryTestRecord("/tmp/b", 2, "2026-11-08T09:00:00Z")}
	merged = mergeLearnRecords(existing, nil)
	if len(merged) != 1 || merged[0].Target != "/tmp/b" || merged[0].Count != 2 {
		t.Errorf("merge(existing, nil) = %+v, want the existing record verbatim", merged)
	}

	merged = mergeLearnRecords(nil, nil)
	if len(merged) != 0 {
		t.Errorf("merge(nil, nil) = %+v, want empty", merged)
	}
}

// TestLearnSummarySizeBoundDropsLowestCountsFirst covers the 900 KiB bound:
// 2000 records with 600-byte targets render to a body well above 900 KiB;
// the bound trims the lowest-count records first (ties by key) until the
// body fits, and returns the number dropped.
func TestLearnSummarySizeBoundDropsLowestCountsFirst(t *testing.T) {
	// 2000 records, each with a distinct 600-byte target. Counts are 1..2000
	// so the lowest-count records are unambiguous.
	recs := make([]alerts.Record, 0, 2000)
	for i := range 2000 {
		target := "/tmp/" + strings.Repeat("x", 600) + "-" + string(rune('a'+i%26)) + string(rune('0'+i/26%10)) + "-" + strconv.Itoa(i)
		// Give each record a distinct count (1..2000) so the drop order is
		// unambiguous: the lowest counts (1, 2, 3, …) are dropped first.
		recs = append(recs, learnSummaryTestRecord(target, int64(i+1), "2026-11-08T10:00:00Z"))
	}
	// Render the unbounded body so we know the bound is actually hit.
	unbounded := renderLearnRecordsJSONL(recs)
	if len(unbounded) < learnSummarySizeBoundBytes {
		t.Fatalf("the unbounded body is %d bytes, below the %d bound; the test fixture must exceed the bound", len(unbounded), learnSummarySizeBoundBytes)
	}
	kept, dropped := boundLearnRecords(recs, learnSummarySizeBoundBytes)
	if dropped == 0 {
		t.Fatalf("the bound dropped 0 records; the body exceeds the bound so some must be dropped")
	}
	if len(kept)+int(dropped) != len(recs) {
		t.Errorf("kept (%d) + dropped (%d) = %d, want %d (the full input)", len(kept), dropped, len(kept)+int(dropped), len(recs))
	}
	keptBody := renderLearnRecordsJSONL(kept)
	if len(keptBody) > learnSummarySizeBoundBytes {
		t.Errorf("the kept body is %d bytes, over the %d bound; the bound must trim to at most the bound", len(keptBody), learnSummarySizeBoundBytes)
	}
	// The lowest-count records (count 1, 2, 3, …) must be the ones dropped.
	// Build the set of kept counts and verify the lowest counts are absent.
	keptCounts := map[int64]bool{}
	for _, r := range kept {
		keptCounts[r.Count] = true
	}
	// dropped records are the lowest counts 1..N (N = dropped). The kept
	// set must NOT contain any count <= dropped.
	for c := int64(1); c <= dropped; c++ {
		if keptCounts[c] {
			t.Fatalf("the bound kept a record with count %d, but that is within the lowest-%d that must be dropped first", c, dropped)
		}
	}
	// The kept set MUST contain every count > dropped (the higher-count
	// records are all kept).
	for c := dropped + 1; c <= 2000; c++ {
		if !keptCounts[c] {
			t.Fatalf("the bound dropped a record with count %d, but that is above the lowest-%d; the highest counts must be kept", c, dropped)
		}
	}
}

// TestLearnSummarySizeBoundTiesByKey covers the tie-break: two records with
// the same count — the one with the LARGER key is dropped first (the drop
// order is count ascending, then key ascending, so the smaller key is kept).
func TestLearnSummarySizeBoundTiesByKey(t *testing.T) {
	// Two records with the same count (1) and keys that are large enough to
	// hit the bound. We use a very small bound (just under the two records'
	// combined size) so exactly one is dropped.
	recA := alerts.Record{Time: "2026-11-08T10:00:00Z", Loop: "l1", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/a" + strings.Repeat("x", 500), Detail: "operation=File", Count: 1}
	recB := alerts.Record{Time: "2026-11-08T10:00:00Z", Loop: "l1", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/b" + strings.Repeat("y", 500), Detail: "operation=File", Count: 1}
	var aBuf, bBuf bytesBuffer
	_ = alerts.WriteRecord(&aBuf, recA)
	_ = alerts.WriteRecord(&bBuf, recB)
	sizeA := aBuf.Len()
	// A bound that fits exactly one record (both records are the same size).
	_ = bBuf.Len()
	kept, dropped := boundLearnRecords([]alerts.Record{recA, recB}, sizeA)
	if dropped != 1 {
		t.Fatalf("the bound dropped %d records, want exactly 1 (the bound fits only one of the two equal-size records)", dropped)
	}
	if len(kept) != 1 {
		t.Fatalf("the bound kept %d records, want exactly 1", len(kept))
	}
	// The two records have the same count (1). The drop order is count
	// ascending, then key ascending — the SMALLER key is dropped first (the
	// smallest key goes into dropSet first). So the KEPT record is the one
	// with the LARGER key.
	keyA := alerts.LearnKey(recA)
	keyB := alerts.LearnKey(recB)
	if keyA < keyB {
		// recA has the smaller key; the bound keeps recB.
		if kept[0].Target != recB.Target {
			t.Errorf("the bound kept %q, want %q (the larger-key record — ties by key, smaller key dropped first)", kept[0].Target, recB.Target)
		}
	} else {
		if kept[0].Target != recA.Target {
			t.Errorf("the bound kept %q, want %q (the larger-key record — ties by key, smaller key dropped first)", kept[0].Target, recA.Target)
		}
	}
}

// TestLearnSummaryBoundKeepsRareConnectAndExec covers the class-ordered
// eviction (D119 step 2c, run 13: the lowest-count-first bound dropped the
// one egress connect and a rare exec in favor of a thousand file paths): a
// summary over the bound keeps the count-1 egress connect and the count-1
// Process record while evicting File records first (and, if still over, then
// Process records — never connects while File or Process records remain).
//
// Mutation m1 (evict by count only, ignoring the class): the connect and/or
// the exec are dropped → this spec fails.
func TestLearnSummaryBoundKeepsRareConnectAndExec(t *testing.T) {
	recs := make([]alerts.Record, 0, 3002)
	for i := range 3000 {
		j := i * 7 % 13
		// 300-byte targets in distinct, non-busy dirs so compaction cannot
		// collapse them (/f/<i>/<j>, at most 13 entries per dir < the
		// collapse threshold).
		target := "/f/" + strconv.Itoa(i) + "/" + strconv.Itoa(j) + strings.Repeat("x", 300)
		recs = append(recs, alerts.Record{
			Time:   "2026-11-12T10:00:00Z",
			Loop:   "l1",
			Source: alerts.SourceKubeArmor,
			Action: "Audit",
			Target: target,
			Detail: "operation=File, pod=l1-sandbox",
			Count:  1,
		})
	}
	// The one egress connect (count 1) and the rare exec (count 1).
	recs = append(recs,
		alerts.Record{Time: "2026-11-12T11:00:00Z", Loop: "l1", Source: alerts.SourceEgress, Action: "connect", Target: "proxy.golang.org:443", Count: 1},
		alerts.Record{Time: "2026-11-12T11:30:00Z", Loop: "l1", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/bin/base64", Detail: "operation=Process, pod=l1-sandbox", Count: 1},
	)
	bound := 400 * 1024 // well under the 3002 records' rendered size
	unbounded := renderLearnRecordsJSONL(recs)
	if len(unbounded) < bound {
		t.Fatalf("the unbounded body is %d bytes, below the %d bound; the fixture must force eviction", len(unbounded), bound)
	}
	kept, dropped := boundLearnRecords(recs, bound)
	if dropped == 0 {
		t.Fatalf("the bound dropped 0 records; the body exceeds the bound so some must be dropped")
	}
	found := map[string]bool{}
	for _, r := range kept {
		found[r.Target] = true
	}
	if !found["proxy.golang.org:443"] {
		t.Fatalf("the bound dropped the egress connect (kept %d records, dropped %d); connects are evicted LAST — only when nothing else is left", len(kept), dropped)
	}
	if !found["/bin/base64"] {
		t.Fatalf("the bound dropped the rare exec /bin/base64 (kept %d records, dropped %d); Process records are evicted after File records", len(kept), dropped)
	}
	if len(renderLearnRecordsJSONL(kept)) > bound {
		t.Fatalf("the kept body is %d bytes, over the %d bound", len(renderLearnRecordsJSONL(kept)), bound)
	}
}

// TestLearnSummaryCompactsWideTree covers the compaction step (D119 step 2c,
// run 13: 669 of the 2684 kept records were go-build cache files, and the
// ConfigMap candidate differed from the full-log candidate because of them):
// 300 File records under /scratch/.cache/go-build/<xx>/<hash> (100 dirs × 3)
// become ONE record for /scratch/.cache/go-build/ with the summed count 300.
//
// Mutation m2 (skip the compaction): the 300 raw paths are not merged →
// this spec fails.
func TestLearnSummaryCompactsWideTree(t *testing.T) {
	recs := make([]alerts.Record, 0, 300)
	for i := range 300 {
		xx := strconv.FormatInt(int64(i/3)%100, 10) // 100 dirs, 3 files each
		// Padded minute (i%60): the RFC3339Nano strings compare
		// lexicographically, which is chronological only when the layout is
		// fixed-width.
		recs = append(recs, alerts.Record{
			Time:   "2026-11-12T10:" + fmt.Sprintf("%02d", i%60) + ":00Z",
			Loop:   "l1",
			Source: alerts.SourceKubeArmor,
			Action: "Audit",
			Target: "/scratch/.cache/go-build/" + xx + "/" + strconv.Itoa(i),
			Detail: "operation=File, pod=l1-sandbox",
			Count:  1,
		})
	}
	compacted := compactLearnFileRecords(recs, "l1")
	if len(compacted) != 1 {
		t.Fatalf("compaction kept %d records, want 1 (the wide go-build tree collapses to one dir entry); got targets: %v", len(compacted), targetsOf(compacted))
	}
	rec := compacted[0]
	if rec.Target != "/scratch/.cache/go-build/" {
		t.Fatalf("compacted target = %q, want %q", rec.Target, "/scratch/.cache/go-build/")
	}
	if rec.Count != 300 {
		t.Errorf("compacted count = %d, want 300 (the sum of the 300 file records)", rec.Count)
	}
	if rec.Detail != "operation=File, pod=l1-sandbox" {
		t.Errorf("compacted detail = %q, want %q", rec.Detail, "operation=File, pod=l1-sandbox")
	}
	if rec.Loop != "l1" || rec.Source != alerts.SourceKubeArmor {
		t.Errorf("compacted loop/source = %q/%q, want l1/kubearmor (copied from an original record)", rec.Loop, rec.Source)
	}
	// The Time is the LATEST of the originals (i=59 → 10:59:00Z).
	if rec.Time != "2026-11-12T10:59:00Z" {
		t.Errorf("compacted time = %q, want the latest of the originals (10:59:00Z)", rec.Time)
	}
	// Cross-check the exported compaction matches what the controller applied
	// (the same target→count map, the same result).
	targets := map[string]int{}
	for _, r := range recs {
		targets[r.Target]++
	}
	if got := policy.CompactFileTargets(targets); len(got) != 1 || got["/scratch/.cache/go-build/"] != 300 {
		t.Errorf("policy.CompactFileTargets = %v, want the single entry /scratch/.cache/go-build/ = 300", got)
	}
}

// TestLearnSummaryCompactKeepsCounts covers the re-review P1 (compaction
// must sum the records' COUNTS, not count the records): the merged input
// holds the ConfigMap's existing records whose counts are already summed, so
// collapsing them and re-deriving the count from the number of records would
// reset every stored count on every write.
//
// Mutation m3 (targets[rec.Target]++ instead of += max(1, rec.Count)): the
// count-50 record compacts to count 1 and the 300+1 pair compacts to 2.
func TestLearnSummaryCompactKeepsCounts(t *testing.T) {
	// Case 1: a single File record with a stored count of 50 compacts to a
	// count of 50 (not 1).
	one := []alerts.Record{{
		Time: "2026-11-13T10:00:00Z", Loop: "l1", Source: alerts.SourceKubeArmor,
		Action: "Audit", Target: "/etc/passwd", Detail: "operation=File, pod=l1-sandbox", Count: 50,
	}}
	compacted := compactLearnFileRecords(one, "l1")
	if len(compacted) != 1 {
		t.Fatalf("compaction kept %d records, want 1; targets: %v", len(compacted), targetsOf(compacted))
	}
	if compacted[0].Target != "/etc/passwd" {
		t.Fatalf("compacted target = %q, want /etc/passwd (a single unbusy path is unchanged)", compacted[0].Target)
	}
	if compacted[0].Count != 50 {
		t.Errorf("compacted count = %d, want 50 (the record's stored count, not the number of records — mutation m3 counts records)", compacted[0].Count)
	}

	// Case 2: a stored /scratch/.cache/go-build/ record (count 300, the
	// collapse from a previous write) plus one new raw path under it (count 1)
	// compact to ONE record /scratch/.cache/go-build/ with count 301.
	pair := []alerts.Record{
		{Time: "2026-11-13T10:00:00Z", Loop: "l1", Source: alerts.SourceKubeArmor, Action: "Audit",
			Target: "/scratch/.cache/go-build/", Detail: "operation=File, pod=l1-sandbox", Count: 300},
		{Time: "2026-11-13T11:00:00Z", Loop: "l1", Source: alerts.SourceKubeArmor, Action: "Audit",
			Target: "/scratch/.cache/go-build/ab/cd", Detail: "operation=File, pod=l1-sandbox", Count: 1},
	}
	compacted = compactLearnFileRecords(pair, "l1")
	if len(compacted) != 1 {
		t.Fatalf("compaction kept %d records, want 1 (the stored dir entry and the path under it collapse together); targets: %v", len(compacted), targetsOf(compacted))
	}
	if compacted[0].Target != "/scratch/.cache/go-build/" {
		t.Fatalf("compacted target = %q, want /scratch/.cache/go-build/", compacted[0].Target)
	}
	if compacted[0].Count != 301 {
		t.Errorf("compacted count = %d, want 301 (the stored 300 + the new record's 1 — mutation m3 would give 2, the number of records)", compacted[0].Count)
	}
	// The Time is the latest of the two originals (the new raw path's 11:00).
	if compacted[0].Time != "2026-11-13T11:00:00Z" {
		t.Errorf("compacted time = %q, want the latest of the originals (11:00:00Z)", compacted[0].Time)
	}
}

// TestLearnSummaryBoundEvictsProcessBeforeConnect covers the re-review P2
// (class-ordered eviction: egress connects are evicted LAST, after Process
// records — before the rank-2 fix they shared rank 1 and a count-1 connect
// lost lowest-count-first to count-5 Process records):
// 50 Process records with count 5 and one egress connect with count 1; at a
// bound just under the full body the connect must survive and a Process
// record must be dropped.
//
// Mutation m4 (delete the rank-2 line): the connect (count 1) is the lowest
// count and is evicted first → this spec fails.
func TestLearnSummaryBoundEvictsProcessBeforeConnect(t *testing.T) {
	recs := make([]alerts.Record, 0, 51)
	for i := range 50 {
		target := "/bin/p" + strconv.Itoa(200+i) + string(rune('a'+i%26)) + string(rune('a'+i/26))
		recs = append(recs, alerts.Record{
			Time:   "2026-11-13T10:00:00Z",
			Loop:   "l1",
			Source: alerts.SourceKubeArmor,
			Action: "Audit",
			Target: target,
			Detail: "operation=Process, pod=l1-sandbox",
			Count:  5,
		})
	}
	recs = append(recs, alerts.Record{
		Time: "2026-11-13T11:00:00Z", Loop: "l1", Source: alerts.SourceEgress, Action: "connect", Target: "proxy.golang.org:443", Count: 1,
	})
	// A bound just under the full rendered body: it forces at least one
	// eviction (a connect line is >100 bytes, so dropping the connect alone
	// would still fit; one Process line is also >100 bytes, so the bound
	// keeps everything but forces the evictions to actually trim size).
	bound := len(renderLearnRecordsJSONL(recs)) - 100
	kept, dropped := boundLearnRecords(recs, bound)
	if dropped < 1 {
		t.Fatalf("the bound dropped %d records, want >= 1 (the body is over the bound)", dropped)
	}
	found := map[string]bool{}
	for _, r := range kept {
		found[r.Target] = true
	}
	if !found["proxy.golang.org:443"] {
		t.Fatalf("the bound dropped the egress connect (kept %d, dropped %d); connects are evicted LAST (rank 2) — after every Process record (mutation m4 shares rank 1)", len(kept), dropped)
	}
	for _, r := range recs {
		if r.Source == alerts.SourceKubeArmor && !found[r.Target] {
			return // a Process record was evicted, as the class order requires
		}
	}
	t.Fatalf("no Process record was evicted (kept %d, dropped %d); the Process records must be evicted before the connect", len(kept), dropped)
}

// targetsOf returns the targets of the records (for failure messages).
func targetsOf(recs []alerts.Record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Target)
	}
	return out
}

// TestIsLearnModeGatesOnPolicyEnforcedCondition covers the learn-mode gate:
// the ConfigMap is written ONLY when the PolicyEnforced condition is
// False / reason AuditOnly.
//
// Mutation m1: remove the learn-mode check in recordLearnSummary → the
// "non-learn-mode loop gets no ConfigMap" case fails (the ConfigMap is
// written regardless).
func TestIsLearnModeGatesOnPolicyEnforcedCondition(t *testing.T) {
	learnLoop := &coxv1alpha1.Loop{
		Status: coxv1alpha1.LoopStatus{
			Conditions: []metav1.Condition{
				{Type: "PolicyEnforced", Status: metav1.ConditionFalse, Reason: "AuditOnly", Message: "learn mode"},
			},
		},
	}
	if !isLearnMode(learnLoop) {
		t.Errorf("isLearnMode = false for a PolicyEnforced=False/AuditOnly Loop; the gate must recognise learn mode")
	}

	enforcingLoop := &coxv1alpha1.Loop{
		Status: coxv1alpha1.LoopStatus{
			Conditions: []metav1.Condition{
				{Type: "PolicyEnforced", Status: metav1.ConditionTrue, Reason: "Enforcing", Message: "enforcing"},
			},
		},
	}
	if isLearnMode(enforcingLoop) {
		t.Errorf("isLearnMode = true for a PolicyEnforced=True/Enforcing Loop; the gate must not recognise an enforcing Loop as learn mode")
	}

	noCondLoop := &coxv1alpha1.Loop{}
	if isLearnMode(noCondLoop) {
		t.Errorf("isLearnMode = true for a Loop with no PolicyEnforced condition; the gate must be false when the condition is absent")
	}

	// A PolicyEnforced=False with a DIFFERENT reason (e.g. KaptTampered) is
	// NOT learn mode.
	tamperedLoop := &coxv1alpha1.Loop{
		Status: coxv1alpha1.LoopStatus{
			Conditions: []metav1.Condition{
				{Type: "PolicyEnforced", Status: metav1.ConditionFalse, Reason: "KaptTampered", Message: "tampered"},
			},
		},
	}
	if isLearnMode(tamperedLoop) {
		t.Errorf("isLearnMode = true for a PolicyEnforced=False/KaptTampered Loop; only reason AuditOnly is learn mode")
	}
}

// bytesBuffer is a tiny io.Writer that records the bytes (for the WriteRecord
// sizing calls above).
type bytesBuffer struct{ buf []byte }

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	return len(p), nil
}
func (b *bytesBuffer) Len() int { return len(b.buf) }

// TestLearnSummaryCompactTimeIsPerEntry covers the string-prefix defect in
// the compact-time walk (D119 step 2 leftovers, #439): a raw target and a
// sibling that only shares a string prefix (/a/b vs /a/bc) must each keep
// ITS OWN time — the old O(raw x compacted) HasPrefix scan let /a/b match
// the entry /a/bc and copy its time across.
//
// Mutation m2 (walk the ancestors but match compacted entries by plain
// strings.HasPrefix instead of the "/"-terminated ancestor): the two
// uncollapsed siblings cross-match and the times smear → this spec fails.
func TestLearnSummaryCompactTimeIsPerEntry(t *testing.T) {
	recs := []alerts.Record{
		{Time: "2026-11-14T10:00:00Z", Loop: "l1", Source: alerts.SourceKubeArmor,
			Action: "Audit", Target: "/a/b", Detail: "operation=File, pod=l1-sandbox", Count: 1},
		{Time: "2026-11-14T11:00:00Z", Loop: "l1", Source: alerts.SourceKubeArmor,
			Action: "Audit", Target: "/a/bc", Detail: "operation=File, pod=l1-sandbox", Count: 1},
	}
	compacted := compactLearnFileRecords(recs, "l1")
	times := map[string]string{}
	for _, r := range compacted {
		times[r.Target] = r.Time
	}
	if len(compacted) != 2 {
		t.Fatalf("compaction kept %d records, want 2 (distinct, unbusy paths are not collapsed); targets: %v", len(compacted), targetsOf(compacted))
	}
	if times["/a/b"] != "2026-11-14T10:00:00Z" {
		t.Errorf("time for /a/b = %q, want its own 10:00:00Z (it must not take /a/bc's time via a string-prefix match)", times["/a/b"])
	}
	if times["/a/bc"] != "2026-11-14T11:00:00Z" {
		t.Errorf("time for /a/bc = %q, want its own 11:00:00Z (the old prefix walk could not match /a/bc to anything and could smear /a/b's time here)", times["/a/bc"])
	}
}

// TestLearnSummaryForgetClearsNameTaken covers the #439 leftover: a deleted
// Loop's name-taken UID must leave loopNameTakenSummaryEvents, or a NEW Loop
// that reuses... (UIDs never reuse, so the real cost is the map living on —
// one entry per deleted Loop until the operator restarts, exactly the leak
// R52 I249 flagged for the relay state).
//
// Mutation m1 (delete the Delete loop.UID line): the map keeps the UID and
// this spec fails.
func TestLearnSummaryForgetClearsNameTaken(t *testing.T) {
	r := &LoopReconciler{}
	uid := types.UID("i255-uid")
	loop := &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{UID: uid, Name: "l1", Namespace: "ns"}}
	r.loopNameTakenSummaryEvents.Store(uid, struct{}{})
	r.forgetAuditState(loop)
	if _, ok := r.loopNameTakenSummaryEvents.Load(uid); ok {
		t.Errorf("loopNameTakenSummaryEvents still holds the deleted Loop's UID after forgetAuditState; the name-taken set must be pruned like the relay state")
	}
}
