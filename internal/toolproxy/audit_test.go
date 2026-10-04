package toolproxy

import (
	"encoding/json"
	"testing"
)

// The audit record is one JSON line per request with the exact keys the relay
// consumes. It carries NO credential value and NO headers (method, path,
// status and the policy hash only).
func TestAuditRecordShape(t *testing.T) {
	rec := NewAuditRecord(toolName, loopName, nsName, methodGET, toolPath, 200, policy)
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantKeys := []string{"time", "loop", "namespace", "source", "action", "tool", "method", "path", "status", "policy"}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Fatalf("audit record missing key %q: %s", k, data)
		}
	}
	if m["source"] != Source {
		t.Fatalf("source must be %s, got %v", Source, m["source"])
	}
	if m["action"] != "request" {
		t.Fatalf("action must be request, got %v", m["action"])
	}
	if m["tool"] != toolName {
		t.Fatalf("tool must be tool-x, got %v", m["tool"])
	}
	if m["status"] != float64(200) {
		t.Fatalf("status must be 200, got %v", m["status"])
	}
	if m["policy"] != policy {
		t.Fatalf("policy must be pol-abc, got %v", m["policy"])
	}
	// The credential is structurally never in the record: no headers, no
	// credential field.
	for _, k := range []string{"headers", "authorization", "credential", "iteration"} {
		if _, ok := m[k]; ok {
			t.Fatalf("audit record must NOT carry %q: %s", k, data)
		}
	}
}

func TestAuditRecordBlocked(t *testing.T) {
	rec := NewAuditRecord(toolName, loopName, nsName, methodDELETE, toolPath, 403, policy)
	data, _ := json.Marshal(rec)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	if m["status"] != float64(403) {
		t.Fatalf("blocked record status must be 403, got %v", m["status"])
	}
	if m["method"] != methodDELETE || m["path"] != toolPath {
		t.Fatalf("blocked record must carry method/path, got %v %v", m["method"], m["path"])
	}
}
