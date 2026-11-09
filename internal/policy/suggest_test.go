package policy

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/papawattu/coxswain/internal/alerts"
)

func p6b2Records() []alerts.Record {
	return []alerts.Record{
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/usr/local/go/bin/go", Verdict: "allowed", Detail: "operation=Process, pod=l1-sandbox, policy=p"},
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/usr/local/go/bin/go", Verdict: "allowed", Detail: "operation=Process, pod=l1-sandbox"},
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/usr/bin/git", Verdict: "allowed", Detail: "operation=Process, pod=l1-sandbox"},
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "go", Verdict: "allowed", Detail: "operation=Process, pod=l1-sandbox"},
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/workspace/go.mod", Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"},
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "domain=proxy.golang.org", Verdict: "allowed", Detail: "operation=Network, pod=l1-sandbox"},
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/usr/local/bin/verify", Verdict: "allowed", Detail: "operation=Process, pod=l1-verify-1-x"},
		{Loop: "l1", Namespace: "ns", Source: "egress-proxy", Action: "connect", Target: "proxy.golang.org:443", Verdict: "blocked"},
		{Loop: "l1", Namespace: "ns", Source: "egress-proxy", Action: "connect", Target: "proxy.golang.org:443", Verdict: "allowed"},
		{Loop: "other", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/bin/sh", Verdict: "allowed", Detail: "operation=Process, pod=other-sandbox"},
		{Loop: "l1", Namespace: "other-ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/bin/bash", Verdict: "allowed", Detail: "operation=Process, pod=l1-sandbox"},
	}
}

// P6b-2/D109: each observed allow appears once, sorted; other Loops and
// namespaces are ignored; KubeArmor records for another pod (the sandbox also
// runs verify probes, D112) are ignored; KubeArmor Network records are
// skipped (egress connects carry host:port); a relative exec target is
// dropped.
func TestSuggestFromRecords(t *testing.T) {
	s := SuggestFromRecords(p6b2Records(), "l1", "ns")
	if want := []string{"/usr/bin/git", "/usr/local/go/bin/go"}; !slices.Equal(s.Exec, want) {
		t.Errorf("exec = %v, want %v", s.Exec, want)
	}
	if want := []string{"/workspace/go.mod"}; !slices.Equal(s.Files, want) {
		t.Errorf("files = %v, want %v", s.Files, want)
	}
	if want := []string{"proxy.golang.org:443"}; !slices.Equal(s.Network, want) {
		t.Errorf("network = %v, want %v", s.Network, want)
	}
}

// D112: a record whose pod is not the Loop's agent pod is ignored — the
// sandbox pod also runs verify probes (pod=l1-verify-1-x), whose paths are not
// the agent's.
func TestSuggestFromRecordsIgnoresNonAgentPods(t *testing.T) {
	recs := []alerts.Record{
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/bin/verify", Verdict: "allowed", Detail: "operation=Process, pod=l1-verify-1-x, policy=p"},
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/tmp/verify.log", Verdict: "allowed", Detail: "operation=File, pod=l1-verify-1-x"},
	}
	s := SuggestFromRecords(recs, "l1", "ns")
	if len(s.Exec) != 0 || len(s.Files) != 0 || len(s.Network) != 0 {
		t.Errorf("non-agent-pod records leaked into the suggestion: %s", s)
	}
}

// D112: an exec target is the full command line; only its first field (the
// binary path) is kept, and it must be absolute.
func TestSuggestFromRecordsExecIsFirstField(t *testing.T) {
	recs := []alerts.Record{
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/bin/sh -c echo hi", Verdict: "allowed", Detail: "operation=Process, pod=l1-sandbox"},
	}
	s := SuggestFromRecords(recs, "l1", "ns")
	if want := []string{"/bin/sh"}; !slices.Equal(s.Exec, want) {
		t.Errorf("exec = %v, want %v", s.Exec, want)
	}
}

// D112: a whitespace-only process target has no fields; it is skipped
// instead of panicking on an out-of-range index.
func TestSuggestFromRecordsBlankProcessTarget(t *testing.T) {
	recs := []alerts.Record{
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: " ", Verdict: "allowed", Detail: "operation=Process, pod=l1-sandbox"},
	}
	s := SuggestFromRecords(recs, "l1", "ns")
	if len(s.Exec) != 0 {
		t.Errorf("exec = %v, want none", s.Exec)
	}
}

// D112: a directory with more than 5 distinct file targets collapses to one
// dir + "/" entry; exactly 5 stay as exact paths.
func TestSuggestFromRecordsCollapsesBusyDirs(t *testing.T) {
	recs := make([]alerts.Record, 0, 11)
	for i := range 6 {
		recs = append(recs, alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/a/b/file" + string(rune('a'+i)), Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"})
	}
	for i := range 5 {
		recs = append(recs, alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/c/file" + string(rune('a'+i)), Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"})
	}
	s := SuggestFromRecords(recs, "l1", "ns")
	wantFiles := make([]string, 0, 6)
	wantFiles = append(wantFiles, "/a/b/")
	for i := range 5 {
		wantFiles = append(wantFiles, "/c/file"+string(rune('a'+i)))
	}
	slices.Sort(wantFiles)
	if !slices.Equal(s.Files, wantFiles) {
		t.Errorf("files = %v, want %v", s.Files, wantFiles)
	}
}

// R49 I228: busy directory trees collapse recursively: 6 sub-directories each
// with 6 files collapse to the single deepest busy ancestor, and 6 busy
// top-level directories do NOT collapse to "/" itself.
func TestSuggestFromRecordsCollapsesRecursively(t *testing.T) {
	recs := make([]alerts.Record, 0, 72)
	for i := range 6 {
		for j := range 6 {
			recs = append(recs, alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: fmt.Sprintf("/a/b/c%d/f%d", i, j), Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"})
		}
	}
	s := SuggestFromRecords(recs, "l1", "ns")
	if want := []string{"/a/b/"}; !slices.Equal(s.Files, want) {
		t.Errorf("files = %v, want %v", s.Files, want)
	}

	recs = recs[:0]
	for i := range 6 {
		for j := range 6 {
			recs = append(recs, alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: fmt.Sprintf("/x%d/f%d", i, j), Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"})
		}
	}
	s = SuggestFromRecords(recs, "l1", "ns")
	wantFiles := make([]string, 0, 6)
	for i := range 6 {
		wantFiles = append(wantFiles, fmt.Sprintf("/x%d/", i))
	}
	if !slices.Equal(s.Files, wantFiles) {
		t.Errorf("files = %v, want %v (top level must not collapse to root)", s.Files, wantFiles)
	}
}

// R49 I228 (review on #420): a collapsed "dir/" entry is recursive (D113),
// so entries under it are dropped into their nearest strict ancestor "dir/"
// entry, and unrelated entries survive.
func TestSuggestFromRecordsDropsEntriesUnderACollapsedDir(t *testing.T) {
	// /a/ holds 6 distinct files -> it collapses to "/a/" (threshold 5).
	// /a/b/c.txt, /a/d/ (inside it) and /z.txt (unrelated) must not survive.
	recs := make([]alerts.Record, 0, 10)
	for i := range 6 {
		recs = append(recs, alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/a/f" + string(rune('a'+i)), Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"})
	}
	recs = append(recs,
		alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/a/b/c.txt", Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"},
		alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/a/d/x.txt", Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"},
		alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/a/d/y.txt", Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"},
		alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/z.txt", Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"},
	)
	s := SuggestFromRecords(recs, "l1", "ns")
	if want := []string{"/a/", "/z.txt"}; !slices.Equal(s.Files, want) {
		t.Errorf("files = %v, want %v", s.Files, want)
	}
	// /a/ must carry the sum of the distinct entries it covers (the 6
	// collapsed files + 1 /a/b/c.txt): the collapsed map keeps exactly the
	// sum, counted once (R51 I237 fix-up: the /a/d/ entry's count is
	// covered by the /a/ entry itself, so it is not added a second time).
	if got := collapseFileDirs(map[string]int{
		"/a/fa": 1, "/a/fb": 1, "/a/fc": 1, "/a/fd": 1, "/a/fe": 1, "/a/ff": 1,
		"/a/b/c.txt": 1, "/a/d/": 1,
	}); len(got) != 1 || got["/a/"] != 7 {
		t.Errorf("collapsed = %v, want {/a/: 7} (sum of distinct covered entries, counted once)", got)
	}

	// A collapsed directory UNDER another collapsed directory is subsumed by
	// the OUTERMOST ancestor too (R49 I228, review on #420: run 8 listed
	// /usr/local/go/src/ next to /usr/local/go/src/compress/flate/). The
	// result is exactly ["/a/"] carrying all three counts, and the unrelated
	// /z.txt survives.
	recs = recs[:0]
	for i := range 6 {
		recs = append(recs, alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/a/f" + string(rune('a'+i)), Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"})
	}
	recs = append(recs,
		alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/a/b/c.txt", Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"},
		alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/z.txt", Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"},
	)
	s = SuggestFromRecords(recs, "l1", "ns")
	if want := []string{"/a/", "/z.txt"}; !slices.Equal(s.Files, want) {
		t.Errorf("files = %v, want %v (nested collapsed dir subsumed by its outermost ancestor)", s.Files, want)
	}
	// Direct subsumption of the input {"/a/", "/a/b/", "/a/b/c.txt"} (plus the
	// unrelated /z.txt survivor): the result is exactly "/a/" carrying all
	// three counts.
	if got := collapseFileDirs(map[string]int{"/a/": 1, "/a/b/": 1, "/a/b/c.txt": 1, "/z.txt": 1}); len(got) != 2 || got["/a/"] != 3 || got["/z.txt"] != 1 {
		t.Errorf("collapsed = %v, want {/a/: 3, /z.txt: 1} (nested dir/ and its file roll into the outermost ancestor)", got)
	}
}

// R51 I237: a wide, shallow tree — 10 subdirectories with 2 files each —
// collapses to the parent directory, because the parent has more than 5
// distinct immediate children (the subdirectories themselves); the
// unrelated file survives and nothing overflows.
func TestSuggestFromRecordsCollapsesWideShallowTree(t *testing.T) {
	recs := make([]alerts.Record, 0, 21)
	for i := range 10 {
		dir := fmt.Sprintf("/scratch/.cache/go-build/%c", '0'+i)
		for j := range 2 {
			recs = append(recs, alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: dir + "/f" + string(rune('a'+j)), Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"})
		}
	}
	recs = append(recs, alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/etc/hosts", Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"})
	s := SuggestFromRecords(recs, "l1", "ns")
	if want := []string{"/etc/hosts", "/scratch/.cache/go-build/"}; !slices.Equal(s.Files, want) {
		t.Errorf("files = %v, want %v", s.Files, want)
	}
	if len(s.FilesOverflow) != 0 {
		t.Errorf("files overflow = %v, want none", s.FilesOverflow)
	}
}

// R51 I237 fix-up: a collapsed directory's count is the sum of its entries,
// counted once. 14 files under /s/c/{7 dirs}/{x,y} give /s/c/ = 14, not 28
// (the double-count: the second loop set /s/c/ to the sum of the deeper
// entries still in out, and the outermost-ancestor drop then rolled them in
// again).
func TestSuggestFromRecordsCollapsedCountIsExact(t *testing.T) {
	in := map[string]int{"/etc/hosts": 1}
	for _, d := range []string{"0a", "0b", "0c", "0d", "0e", "0f", "0g"} {
		in["/s/c/"+d+"/x"] = 1
		in["/s/c/"+d+"/y"] = 1
	}
	if got := collapseFileDirs(in); len(got) != 2 || got["/etc/hosts"] != 1 || got["/s/c/"] != 14 {
		t.Errorf("collapsed = %v, want {/etc/hosts: 1, /s/c/: 14}", got)
	}
}

// R51 I237: a narrow tree — 3 subdirectories with 2 files each — does NOT
// collapse: 3 immediate children is under the 5-children threshold, so all
// 6 files are listed.
func TestSuggestFromRecordsKeepsANarrowTree(t *testing.T) {
	recs := make([]alerts.Record, 0, 6)
	for i := range 3 {
		dir := fmt.Sprintf("/srv/a/d%d", i)
		for j := range 2 {
			recs = append(recs, alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: dir + "/f" + string(rune('a'+j)), Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"})
		}
	}
	s := SuggestFromRecords(recs, "l1", "ns")
	want := []string{
		"/srv/a/d0/fa", "/srv/a/d0/fb",
		"/srv/a/d1/fa", "/srv/a/d1/fb",
		"/srv/a/d2/fa", "/srv/a/d2/fb",
	}
	if !slices.Equal(s.Files, want) {
		t.Errorf("files = %v, want %v (narrow tree must not collapse)", s.Files, want)
	}
	if len(s.FilesOverflow) != 0 {
		t.Errorf("files overflow = %v, want none", s.FilesOverflow)
	}
}

// D119 = (b) step 1: a record's weight is max(1, r.Count). The learn summary
// carries one record per distinct key with its occurrence count; a one-off
// line from the log has no count (Count=0, weight 1). The cap keeps the
// highest-weight entries.
//
// Mutation m3: SuggestFromRecords adds 1 and ignores Count → /a (weight 50)
// is treated as weight 1 and falls into the overflow.
func TestSuggestFromRecordsUsesCount(t *testing.T) {
	// Two exec records: /a with Count 50, /b with no count (weight 1).
	// Pad with 70 other distinct paths (each weight 1) so the total exceeds
	// the 64-entry cap. /a must be kept (its weight 50 beats every weight-1
	// entry); /bin/zzz-b (weight 1, tied with the others) is dropped by the
	// name tie-break.
	var recs = make([]alerts.Record, 0, 72)
	recs = append(recs, alerts.Record{
		Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit",
		Target: "/bin/zzz-a", Verdict: "allowed", Detail: "operation=Process, pod=l1-sandbox",
		Count: 50,
	})
	recs = append(recs, alerts.Record{
		Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit",
		Target: "/bin/zzz-b", Verdict: "allowed", Detail: "operation=Process, pod=l1-sandbox",
	})
	// 70 more distinct paths, each one record (weight 1).
	for i := range 70 {
		recs = append(recs, alerts.Record{
			Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit",
			Target: fmt.Sprintf("/bin/ta%02d", i), Verdict: "allowed",
			Detail: "operation=Process, pod=l1-sandbox",
		})
	}
	s := SuggestFromRecords(recs, "l1", "ns")
	// /a must be in the kept list (weight 50 > every weight-1 entry).
	if !slices.Contains(s.Exec, "/bin/zzz-a") {
		t.Errorf("/bin/zzz-a (Count 50) is not in the kept exec list; the cap ignored the count. kept=%v", s.Exec)
	}
	// /bin/zzz-a must NOT be in the overflow.
	if slices.Contains(s.ExecOverflow, "/bin/zzz-a") {
		t.Errorf("/bin/zzz-a (Count 50) is in the exec overflow; the cap ignored the count. overflow=%v", s.ExecOverflow)
	}
	// 72 distinct paths total, 64 kept, 8 overflow.
	if len(s.Exec) != 64 {
		t.Errorf("kept exec = %d entries, want 64", len(s.Exec))
	}
	if len(s.ExecOverflow) != 8 {
		t.Errorf("exec overflow = %d entries, want 8 (72 distinct - 64 kept): %v", len(s.ExecOverflow), s.ExecOverflow)
	}
}

// D112: 70 distinct exec paths overflow the 64-entry cap; the kept 64 are
// the highest-count entries (ties broken by name), the rest go to
// ExecOverflow.
func TestSuggestFromRecordsCapsAt64(t *testing.T) {
	var recs []alerts.Record
	for i := range 70 {
		target := fmt.Sprintf("/bin/ta%02d", i)
		// 6 paths get one observation, the other 64 get two.
		obs := 2
		if i < 6 {
			obs = 1
		}
		for o := 0; o < obs; o++ {
			recs = append(recs, alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: target, Verdict: "allowed", Detail: "operation=Process, pod=l1-sandbox"})
		}
	}
	s := SuggestFromRecords(recs, "l1", "ns")
	if len(s.Exec) != 64 {
		t.Fatalf("exec = %d entries, want 64", len(s.Exec))
	}
	if want := 6; len(s.ExecOverflow) != want {
		t.Fatalf("exec overflow = %v (%d), want %d", s.ExecOverflow, len(s.ExecOverflow), want)
	}
	// The 6 single-observation paths are the overflow: they sort first by
	// name, so the six smallest are exactly ta0..tf0.
	if want := []string{"/bin/ta00", "/bin/ta01", "/bin/ta02", "/bin/ta03", "/bin/ta04", "/bin/ta05"}; !slices.Equal(s.ExecOverflow, want) {
		t.Errorf("exec overflow = %v, want %v", s.ExecOverflow, want)
	}
	// Everything is sorted by name.
	if !slices.IsSorted(s.Exec) || !slices.IsSorted(s.ExecOverflow) {
		t.Errorf("kept %v and overflow %v must both be sorted by name", s.Exec, s.ExecOverflow)
	}
}

// R15/D112: execAllowable is the Go copy of the AgentPolicy exec XValidation
// rule; it must accept exactly the paths the CRD accepts.
func TestExecAllowable(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/usr/bin/git", true},
		{"/bin/sh", true},
		{"/usr/local/go/bin/go", true},
		{"/workspace", false},
		{"/workspace/x", false},
		{"/scratch", false},
		{"/scratch/y", false},
		{"/tmp", false},
		{"/tmp/z", false},
		{"/usr/./bin/x", false},
		{"/a/../b", false},
		{"/a/.", false},
		{"/a/..", false},
		{"relative", false},
		{"/a b", false},
		{"/a//b", false},
	}
	for _, tc := range cases {
		if got := execAllowable(tc.path); got != tc.want {
			t.Errorf("execAllowable(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// R15/D112: a process binary under a writable mount (/workspace, /tmp) is
// never an exec allow; it goes to ExecRejected, where the render shows it
// cannot be allowed. A canonical binary still is.
func TestSuggestFromRecordsRejectsWritableMountExec(t *testing.T) {
	recs := []alerts.Record{
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/workspace/a/b.test -v", Verdict: "allowed", Detail: "operation=Process, pod=l1-sandbox"},
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/tmp/x", Verdict: "allowed", Detail: "operation=Process, pod=l1-sandbox"},
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/usr/bin/git status", Verdict: "allowed", Detail: "operation=Process, pod=l1-sandbox"},
	}
	s := SuggestFromRecords(recs, "l1", "ns")
	if want := []string{"/usr/bin/git"}; !slices.Equal(s.Exec, want) {
		t.Errorf("exec = %v, want %v", s.Exec, want)
	}
	if want := []string{"/tmp/x", "/workspace/a/b.test"}; !slices.Equal(s.ExecRejected, want) {
		t.Errorf("execRejected = %v, want %v", s.ExecRejected, want)
	}
}

// D112: when every observed exec path is rejected, the render still carries
// the rejected-exec comment (the exec section must not be skipped just
// because the allow list is empty).
func TestRenderSuggestionRejectedOnlyExec(t *testing.T) {
	recs := []alerts.Record{
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/tmp/x", Verdict: "allowed", Detail: "operation=Process, pod=l1-sandbox"},
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/workspace/a/b.test", Verdict: "allowed", Detail: "operation=Process, pod=l1-sandbox"},
	}
	s := SuggestFromRecords(recs, "l1", "ns")
	out := RenderSuggestion(s, "l1-suggested", "ns")
	if !strings.Contains(out, "cannot be allowed") {
		t.Errorf("rendered candidate lacks the rejected-exec comment:\n%s", out)
	}
}

// D109: the rendered candidate carries the platform-minimum header, the
// observed allows, no auditOnly, and never a deny.
func TestRenderSuggestion(t *testing.T) {
	out := RenderSuggestion(SuggestFromRecords(p6b2Records(), "l1", "ns"), "l1-suggested", "ns")
	for _, want := range []string{"Platform minimum", "kind: AgentPolicy", "name: l1-suggested", `- "/usr/bin/git"`, `- "proxy.golang.org:443"`, `- "/workspace/go.mod"`} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered candidate lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"auditOnly", "deny", "Deny", "/bin/sh"} {
		if strings.Contains(out, bad) {
			t.Errorf("rendered candidate must not contain %q:\n%s", bad, out)
		}
	}
}

// D112: the overflow is rendered as a comment block after its list, and every
// listed exec entry is a bare absolute path the CRD's pattern accepts
// (absolute paths only; a full command line is rejected).
func TestRenderSuggestionOverflowComment(t *testing.T) {
	recs := make([]alerts.Record, 0, 66)
	for i := range 66 {
		recs = append(recs, alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: fmt.Sprintf("/bin/ta%02d", i), Verdict: "allowed", Detail: "operation=Process, pod=l1-sandbox"})
	}
	s := SuggestFromRecords(recs, "l1", "ns")
	out := RenderSuggestion(s, "l1-suggested", "ns")
	wantComment := "  # 2 more exec entries were observed but not listed (AgentPolicy allows at most 64):"
	if !strings.Contains(out, wantComment) {
		t.Errorf("rendered candidate lacks the overflow comment %q:\n%s", wantComment, out)
	}
	// The overflow entries are rendered commented out, not as list items.
	for _, v := range s.ExecOverflow {
		if !strings.Contains(out, "  #   "+`"`+v+`"`) {
			t.Errorf("overflow entry %q not rendered as a comment line:\n%s", v, out)
		}
	}
	// Every listed entry that is an exec path (an absolute path) satisfies
	// execAllowable, the Go copy of the AgentPolicy exec XValidation rule
	// (api/v1alpha1/agentpolicy_types.go:66) — a full command line and a
	// path the CRD rejects are never listed.
	listed := map[string]bool{}
	for line := range strings.SplitSeq(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		v, ok := strings.CutPrefix(trimmed, "- ")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		if v == "" {
			continue
		}
		listed[v] = true
		if strings.HasPrefix(v, "/") && !execAllowable(v) {
			t.Errorf("listed exec entry %q is rejected by the AgentPolicy exec rule", v)
		}
	}
	if len(listed) != 64 {
		t.Errorf("rendered candidate lists %d exec entries, want 64:\n%s", len(listed), out)
	}
}

// R52: per-run file names become their stable directory. (a) A path segment
// that starts with ".." followed by a digit is a Secret volume's
// atomic-writer directory, rewritten on every mount; (b) the memory mount's
// files are named by a content hash.
func TestStableFilePath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/workspace-creds/..2026_11_07_21_02_45.2174268029/password", "/workspace-creds/"},
		{"/workspace-creds/..2026_11_07_21_02_45.2174268029/username", "/workspace-creds/"},
		{"/memory/19076493ba2fd0e1c7b5a48c6d9e2f304152637485960718293a4b5c6d7e8f8f.txt", "/memory/"},
		{"/memory", "/memory"},
		{"/etc/hosts", "/etc/hosts"},
		{"/a/..b/c", "/a/..b/c"},
		// I248: an atomic-writer segment that is the first segment under "/"
		// has no stable directory; returning "/" would be a whole-filesystem
		// recursive allow, so the path stays unchanged.
		{"/..2026_01_01_00_00_00.1/x", "/..2026_01_01_00_00_00.1/x"},
	}
	for _, tc := range tests {
		if got := stableFilePath(tc.in); got != tc.want {
			t.Errorf("stableFilePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// R52: two creds files (different atomic-writer dirs collapse to the same
// stable dir) plus three memory files give exactly two files entries.
func TestSuggestFromRecordsStablePaths(t *testing.T) {
	recs := []alerts.Record{
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/workspace-creds/..2026_11_07_21_02_45.2174268029/password", Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"},
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/workspace-creds/..2026_11_07_21_02_45.2174268029/username", Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"},
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/memory/19076493ba2fd0e1c7b5a48c6d9e2f304152637485960718293a4b5c6d7e8f8f.txt", Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"},
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/memory/a31f9b2c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8.txt", Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"},
		{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/memory/b52ca03d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7081b.txt", Verdict: "allowed", Detail: "operation=File, pod=l1-sandbox"},
	}
	s := SuggestFromRecords(recs, "l1", "ns")
	if want := []string{"/memory/", "/workspace-creds/"}; !slices.Equal(s.Files, want) {
		t.Errorf("files = %v, want %v", s.Files, want)
	}
}
