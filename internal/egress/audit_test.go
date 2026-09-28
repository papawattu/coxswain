package egress

import (
	"encoding/json"
	"testing"
)

// The audit record is the Q4 envelope (ADR-0007 Q4) with source=egress-proxy.
// The proxy emits it WITHOUT iteration (the relay fills iteration by time
// window); it carries the resolved IP so a DNS rebind is visible after the
// fact, and the policy hash for correlation with status.policy.effectiveHash.
func TestAuditRecordShape(t *testing.T) {
	rec := AuditRecord{
		Time:      "2026-09-28T12:00:00Z",
		Loop:      "loop-x",
		Namespace: "ns-x",
		Source:    "egress-proxy",
		Action:    "connect",
		Target:    "proxy.golang.org:443",
		Verdict:   "allowed",
		Detail:    "sni=proxy.golang.org, proto=https, ip=151.101.0.223, policy=abc123",
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Round-trip through a map to assert the exact JSON keys the relay
	// consumes (no iteration key).
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantKeys := []string{"time", "loop", "namespace", "source", "action", "target", "verdict", "detail"}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Fatalf("audit record missing key %q: %s", k, data)
		}
	}
	if _, ok := m["iteration"]; ok {
		t.Fatalf("audit record must NOT carry iteration (the relay fills it): %s", data)
	}
	if m["source"] != "egress-proxy" {
		t.Fatalf("source must be egress-proxy, got %v", m["source"])
	}
	// The resolved IP is in detail (the SSRF backstop observability).
	if !contains(m["detail"].(string), "ip=151.101.0.223") {
		t.Fatalf("detail must carry the resolved IP: %v", m["detail"])
	}
}

func TestAuditRecordBlocked(t *testing.T) {
	rec := AuditRecord{
		Loop:      "loop-x",
		Namespace: "ns-x",
		Source:    "egress-proxy",
		Action:    "connect",
		Target:    "evil.example.com:443",
		Verdict:   "blocked",
		Detail:    "reason=not-allowed, policy=abc123",
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["verdict"] != "blocked" {
		t.Fatalf("verdict must be blocked, got %v", m["verdict"])
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
