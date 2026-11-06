package policy

import (
	"slices"
	"strings"
	"testing"

	"github.com/papawattu/coxswain/internal/alerts"
)

func p6b2Records() []alerts.Record {
	return []alerts.Record{
		{Loop: "l1", Namespace: "ns", Source: "kubearmor", Action: "Audit", Target: "/usr/local/go/bin/go", Verdict: "allowed", Detail: "operation=Process, policy=p"},
		{Loop: "l1", Namespace: "ns", Source: "kubearmor", Action: "Audit", Target: "/usr/local/go/bin/go", Verdict: "allowed", Detail: "operation=Process"},
		{Loop: "l1", Namespace: "ns", Source: "kubearmor", Action: "Audit", Target: "/usr/bin/git", Verdict: "allowed", Detail: "operation=Process"},
		{Loop: "l1", Namespace: "ns", Source: "kubearmor", Action: "Audit", Target: "go", Verdict: "allowed", Detail: "operation=Process"},
		{Loop: "l1", Namespace: "ns", Source: "kubearmor", Action: "Audit", Target: "/workspace/go.mod", Verdict: "allowed", Detail: "operation=File"},
		{Loop: "l1", Namespace: "ns", Source: "kubearmor", Action: "Audit", Target: "domain=proxy.golang.org", Verdict: "allowed", Detail: "operation=Network"},
		{Loop: "l1", Namespace: "ns", Source: "egress-proxy", Action: "connect", Target: "proxy.golang.org:443", Verdict: "blocked"},
		{Loop: "l1", Namespace: "ns", Source: "egress-proxy", Action: "connect", Target: "proxy.golang.org:443", Verdict: "allowed"},
		{Loop: "other", Namespace: "ns", Source: "kubearmor", Action: "Audit", Target: "/bin/sh", Verdict: "allowed", Detail: "operation=Process"},
		{Loop: "l1", Namespace: "other-ns", Source: "kubearmor", Action: "Audit", Target: "/bin/bash", Verdict: "allowed", Detail: "operation=Process"},
	}
}

// P6b-2/D109: each observed allow appears once, sorted; other Loops and
// namespaces are ignored; KubeArmor Network records are skipped (egress
// connects carry host:port); a relative exec target is dropped.
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
