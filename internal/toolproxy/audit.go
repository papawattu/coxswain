package toolproxy

import "time"

// AuditRecord is the D41a tool-proxy audit line (ADR-0008): one JSON object
// on stdout per request, allowed and blocked. It carries NO headers and NO
// credential value — redaction is structural (the fields are method, path,
// status and the policy hash; nothing header-derived is ever written).
type AuditRecord struct {
	Time      string `json:"time"`
	Loop      string `json:"loop"`
	Namespace string `json:"namespace"`
	Source    string `json:"source"`
	Action    string `json:"action"`
	Tool      string `json:"tool"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	Policy    string `json:"policy"`
}

// Source is the audit source value for the tool proxy.
const Source = "tool-proxy"

// NewAuditRecord builds a record for a request. status is the response
// status (the upstream's for allowed requests; 403/405/400 for blocked).
func NewAuditRecord(toolName, loop, namespace, method, path string, status int, policyHash string) AuditRecord {
	return AuditRecord{
		Time:      time.Now().UTC().Format(time.RFC3339),
		Loop:      loop,
		Namespace: namespace,
		Source:    Source,
		Action:    "request",
		Tool:      toolName,
		Method:    method,
		Path:      path,
		Status:    status,
		Policy:    policyHash,
	}
}
