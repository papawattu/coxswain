package policy

import (
	"regexp"
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

// D112: 70 distinct exec paths overflow the 64-entry cap; the kept 64 are
// the highest-count entries (ties broken by name), the rest go to
// ExecOverflow.
func TestSuggestFromRecordsCapsAt64(t *testing.T) {
	var recs []alerts.Record
	for i := range 70 {
		target := "/bin/ta" + string(rune('0'+i%60)) + string(rune('a'+i/60))
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
	if want := []string{"/bin/ta0a", "/bin/ta1a", "/bin/ta2a", "/bin/ta3a", "/bin/ta4a", "/bin/ta5a"}; !slices.Equal(s.ExecOverflow, want) {
		t.Errorf("exec overflow = %v, want %v", s.ExecOverflow, want)
	}
	// Everything is sorted by name.
	if !slices.IsSorted(s.Exec) || !slices.IsSorted(s.ExecOverflow) {
		t.Errorf("kept %v and overflow %v must both be sorted by name", s.Exec, s.ExecOverflow)
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
		recs = append(recs, alerts.Record{Loop: "l1", Namespace: "ns", Source: alerts.SourceKubeArmor, Action: "Audit", Target: "/bin/ta" + string(rune('0'+i%10)) + string(rune('a'+i/10)), Verdict: "allowed", Detail: "operation=Process, pod=l1-sandbox"})
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
	// Every listed exec entry matches the AgentPolicy exec pattern
	// (absolute paths only, one character class).
	re := regexp.MustCompile(`^(/[A-Za-z0-9._+-]+)+$`)
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
		if !re.MatchString(v) {
			t.Errorf("listed exec entry %q does not match the AgentPolicy exec pattern", v)
		}
	}
	if len(listed) != 64 {
		t.Errorf("rendered candidate lists %d exec entries, want 64:\n%s", len(listed), out)
	}
}
