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

package alerts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// D119 = (b) (R51 I236): the relay keeps a bounded per-Loop learn summary of
// the records `cox policy suggest` derives its allows from, because the
// operator's own log rotates away during a learn run.

// TestLearnRelevant covers the relevance rule: the sandbox pod's Process and
// File records and the egress connects feed the summary; verify-pod and
// Network records do not; a record without a Loop or a Target does not.
//
// Mutation m1: LearnRelevant returns true for any kubearmor record → the
// verify-pod and Network cases fail.
func TestLearnRelevant(t *testing.T) {
	cases := []struct {
		name string
		rec  Record
		want bool
	}{
		{
			name: "sandbox Process",
			rec:  Record{Loop: "l1", Target: "/bin/sh -c hi", Source: SourceKubeArmor, Action: "Audit", Detail: "operation=Process, pod=l1-sandbox, policy=p"},
			want: true,
		},
		{
			name: "sandbox File",
			rec:  Record{Loop: "l1", Target: "/tmp/x", Source: SourceKubeArmor, Action: "Audit", Detail: "operation=File, pod=l1-sandbox"},
			want: true,
		},
		{
			name: "verify-pod File",
			rec:  Record{Loop: "l1", Target: "/tmp/verify.log", Source: SourceKubeArmor, Action: "Audit", Detail: "operation=File, pod=l1-verify-1-x"},
			want: false,
		},
		{
			name: "kubearmor Network",
			rec:  Record{Loop: "l1", Target: "domain=proxy.golang.org", Source: SourceKubeArmor, Action: "Audit", Detail: "operation=Network, pod=l1-sandbox"},
			want: false,
		},
		{
			name: "egress connect",
			rec:  Record{Loop: "l1", Target: "proxy.golang.org:443", Source: SourceEgress, Action: "connect"},
			want: true,
		},
		{
			name: "empty target",
			rec:  Record{Loop: "l1", Source: SourceEgress, Action: "connect"},
			want: false,
		},
		{
			name: "empty loop",
			rec:  Record{Target: "/bin/sh", Source: SourceKubeArmor, Action: "Audit", Detail: "operation=Process, pod=l1-sandbox"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LearnRelevant(tc.rec); got != tc.want {
				t.Errorf("LearnRelevant(%+v) = %v, want %v", tc.rec, got, tc.want)
			}
		})
	}
}

// TestHolderLearnSummaryCountsAndBounds is D119 step 1's holder contract:
// repeated records bump Count and keep the later Time; the bound is honoured
// with a dropped count rather than overflowed; the summary is a copy, sorted
// by key.
//
// Mutation m2: drop the dropped++ → the bound is silent and this fails.
func TestHolderLearnSummaryCountsAndBounds(t *testing.T) {
	uid := types.UID("uid-test")
	h := NewHolder()

	t1 := "2026-11-08T10:00:00Z"
	t2 := "2026-11-08T10:05:00Z"
	base := Record{Loop: "l1", Namespace: "ns", Source: SourceKubeArmor, Action: "Audit", Target: "/tmp/x", Detail: "operation=File, pod=l1-sandbox"}
	// Add the same record three times (out of order in time on purpose).
	h.addLearn(uid, withTime(base, t1))
	h.addLearn(uid, withTime(base, t2))
	h.addLearn(uid, withTime(base, t1))
	recs, dropped, ok := h.LearnSummary(uid)
	if !ok || len(recs) != 1 {
		t.Fatalf("summary = %d entries (ok=%v), want exactly one distinct record", len(recs), ok)
	}
	if recs[0].Count != 3 {
		t.Errorf("count = %d, want 3 (three occurrences of one record)", recs[0].Count)
	}
	if recs[0].Time != t2 {
		t.Errorf("time = %q, want the later of the two %q", recs[0].Time, t2)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0 under the bound", dropped)
	}

	// A second Loop with learnSummaryMax+2 distinct records: the summary holds
	// the first learnSummaryMax, and the last two are counted as dropped.
	uid2 := types.UID("uid-bounded")
	h2 := NewHolder()
	for i := range learnSummaryMax + 2 {
		rec := base
		rec.Target = fmt.Sprintf("/f%d", i)
		h2.addLearn(uid2, rec)
	}
	recs2, dropped2, ok2 := h2.LearnSummary(uid2)
	if !ok2 {
		t.Fatal("the bounded summary must exist")
	}
	if len(recs2) != learnSummaryMax {
		t.Fatalf("summary = %d entries, want the bound %d", len(recs2), learnSummaryMax)
	}
	if dropped2 != 2 {
		t.Errorf("dropped = %d, want 2 (the two records refused at the bound)", dropped2)
	}
	// The bound keeps the FIRST distinct records in insertion order: the two
	// dropped ones (/f20000, /f20001) must not be among the kept entries.
	kept := make([]string, 0, len(recs2))
	for _, r := range recs2 {
		if r.Target == "/f20000" || r.Target == "/f20001" {
			t.Errorf("a dropped record made it into the summary: %q", r.Target)
		}
		kept = append(kept, r.Target)
	}
	if !slices.IsSorted(kept) {
		t.Errorf("the summary must be sorted by key (targets here are unique): first = %v, last = %v", kept[:3], kept[len(kept)-3:])
	}

	// LearnSummary is a COPY: mutating it must not touch the holder's state.
	recs2[0].Target = "mutated"
	recs2again, _, _ := h2.LearnSummary(uid2)
	if recs2again[0].Target == "mutated" {
		t.Errorf("LearnSummary must return a copy, not the stored records")
	}

	// No summary at all for a Loop with no learns-relevant record.
	if _, _, ok := h.LearnSummary(types.UID("uid-unknown")); ok {
		t.Errorf("LearnSummary for an unknown Loop must report ok=false")
	}
}

// withTime renders a record with the given RFC3339 time string.
func withTime(rec Record, ts string) Record {
	rec.Time = ts
	return rec
}

// kubeArmorFileLine builds a sandbox File audit line (the relay fixture's
// sandbox pod) at the given time, with the detail's pod matching the loop's
// sandbox pod name (l1-sandbox) so LearnRelevant accepts it.
func kubeArmorFileLine(at time.Time, resource string) string {
	return `{"UpdatedTime":"` + at.UTC().Format(time.RFC3339Nano) + `","Operation":"File","NamespaceName":"` + f62Sandbox + `","PodName":"l1-sandbox","ContainerName":"agent","Action":"Audit","Resource":"` + resource + `","PolicyName":"p"}`
}

// TestRelayFeedsLearnSummary is D119 step 1 through the relay fixture: a
// sandbox File line gives one summary entry; a verify-pod line (a different
// pod in the same loop, as `suggest` treats it) gives none.
func TestRelayFeedsLearnSummary(t *testing.T) {
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	src := &scriptedSource{attaches: []attachment{{lines: []string{
		kubeArmorFileLine(base.Add(time.Second), "/data/model.bin"),
		// A verify-pod line: a different pod in the same loop, so the
		// detail's pod field does not match loop-sandbox; the summary
		// must not carry it.
		`{"UpdatedTime":"` + base.Add(2*time.Second).UTC().Format(time.RFC3339Nano) + `","Operation":"File","NamespaceName":"` + f62Sandbox + `","PodName":"l1-verify-1-x","ContainerName":"agent","Action":"Audit","Resource":"/tmp/verify.log","PolicyName":"p"}`,
	}}}}
	relay := f62NewRelay(src)
	// The sandbox pod is named l1-sandbox (matching the loop's sandbox pod
	// name, as LearnRelevant expects); the verify pod is l1-verify-1-x.
	relay.Attributor = &fakeAttributor{
		pods: map[string][2]string{
			f62Sandbox + "/l1-sandbox":     {f62LoopNS, f62LoopName},
			f62Sandbox + "/l1-verify-1-x":  {f62LoopNS, f62LoopName},
			f62Sandbox + "/" + f62SandboxP: {f62LoopNS, f62LoopName},
		},
		uids: map[string]types.UID{f62LoopNS + "/" + f62LoopName: f62UID},
	}
	holder := startTestRelay(t, relay)

	waitFor(t, "a learn summary entry", func() bool {
		recs, _, ok := holder.LearnSummary(f62UID)
		return ok && len(recs) == 1
	})
	// Let the second line settle: it must NOT add a second entry.
	time.Sleep(20 * time.Millisecond)
	recs, dropped, ok := holder.LearnSummary(f62UID)
	if !ok {
		t.Fatal("the summary must exist")
	}
	if len(recs) != 1 {
		t.Fatalf("summary has %d entries, want exactly one (the sandbox File line): %v", len(recs), recs)
	}
	if recs[0].Target != "/data/model.bin" {
		t.Errorf("summary record target = %q, want the sandbox line's target /data/model.bin", recs[0].Target)
	}
	if recs[0].Loop != f62LoopName {
		t.Errorf("summary record loop = %q, want %q", recs[0].Loop, f62LoopName)
	}
	if recs[0].Source != SourceKubeArmor || recs[0].Action != "Audit" {
		t.Errorf("summary record source/action = %q/%q, want %q/Audit", recs[0].Source, recs[0].Action, SourceKubeArmor)
	}
	if got := firstDetailField(recs[0].Detail); got != "operation=File" {
		t.Errorf("summary record detail first field = %q, want operation=File", got)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
}

// TestRelayLearnSummaryMatchesTheEmittedLine is D119 step 1's "built exactly
// as emit builds it" clause: the summary's record carries the same fields as
// the Q4 line emitted for the same alert, so suggest fed the summary sees
// what suggest fed the log saw.
//
// Mutation: build the summary's record from the alert instead of the emit
// shape (or drop a field) → a field diverges and this fails.
func TestRelayLearnSummaryMatchesTheEmittedLine(t *testing.T) {
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	src := &scriptedSource{attaches: []attachment{{lines: []string{
		kubeArmorFileLine(base.Add(time.Second), "/data/model.bin"),
	}}}}
	relay := f62NewRelay(src)
	// The sandbox pod is named l1-sandbox (as LearnRelevant expects); the
	// attributor maps it to the loop.
	relay.Attributor = &fakeAttributor{
		pods: map[string][2]string{
			f62Sandbox + "/l1-sandbox":     {f62LoopNS, f62LoopName},
			f62Sandbox + "/" + f62SandboxP: {f62LoopNS, f62LoopName},
		},
		uids: map[string]types.UID{f62LoopNS + "/" + f62LoopName: f62UID},
	}
	var buf bytes.Buffer
	var mu sync.Mutex
	relay.Emit = &lockedWriter{w: &buf, mu: &mu}
	// The summary entry is added at accept time, BEFORE the iteration join;
	// the join must therefore not panic or block, and must be the same join
	// emit uses (asserted below on the emitted line).
	relay.Iteration = func(namespace, name string) string { return "3" }
	holder := startTestRelay(t, relay)

	waitFor(t, "the emitted line", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Count(buf.String(), "\n") >= 1
	})
	recs, _, ok := holder.LearnSummary(f62UID)
	if !ok || len(recs) != 1 {
		t.Fatalf("summary = %d entries (ok=%v), want exactly one after the emitted line", len(recs), ok)
	}
	sum := recs[0]

	mu.Lock()
	emittedLines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	mu.Unlock()
	if len(emittedLines) != 1 {
		t.Fatalf("got %d emitted lines, want exactly one", len(emittedLines))
	}
	var emitted map[string]string
	if err := json.Unmarshal([]byte(emittedLines[0]), &emitted); err != nil {
		t.Fatalf("the emitted line is not a JSON object: %v (%s)", err, emittedLines[0])
	}
	for key, got := range map[string]string{
		"time":      sum.Time,
		"loop":      sum.Loop,
		"namespace": sum.Namespace,
		"iteration": sum.Iteration,
		"source":    sum.Source,
		"action":    sum.Action,
		"target":    sum.Target,
		"verdict":   sum.Verdict,
		"detail":    sum.Detail,
	} {
		if emitted[key] != got {
			t.Errorf("summary %s = %q, want the emitted line's %q (the summary record must be built exactly as emit builds it)", key, got, emitted[key])
		}
	}
	if emitted["iteration"] != "3" {
		t.Errorf("the summary record's iteration = %q, want the joined status value 3 (the same join emit uses)", sum.Iteration)
	}
}
