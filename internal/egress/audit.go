package egress

import "encoding/json"

// AuditRecord is the Q4 audit envelope (ADR-0007 Q4) for the egress proxy. The
// proxy emits one per connection attempt (allowed and blocked) on stdout as a
// single JSON line. It deliberately carries NO iteration field: the proxy is a
// long-lived pod and its env is immutable, so it cannot read
// status.iteration. The relay (which joins records from all sources into the
// Q4 stream) fills iteration by time window — the same join it does for the
// model proxy and eBPF engine sources. The policy hash in detail
// disambiguates if the effective policy changed mid-iteration.
//
// detail carries the resolved IP the tunnel dials (the SSRF backstop — a
// rebind to a different IP is visible after the fact) and the policy hash
// (correlation with status.policy.effectiveHash).
type AuditRecord struct {
	Time      string `json:"time"`
	Loop      string `json:"loop"`
	Namespace string `json:"namespace"`
	Source    string `json:"source"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Verdict   string `json:"verdict"`
	Detail    string `json:"detail"`
}

// Source is the audit source value for the egress proxy (distinct from
// "model-proxy" and "kubearmor").
const Source = "egress-proxy"

// Marshal renders the record as a single-line JSON object (the form the relay
// consumes from stdout).
func (a AuditRecord) Marshal() ([]byte, error) {
	return json.Marshal(a)
}
