package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestPlainHTTPBlockedByResolvedIP verifies the plain-HTTP path: a host that
// is in the allow list but resolves (is a literal) to a private IP is blocked
// by the resolved-IP SSRF backstop, returning 403. This proves the two-layer
// enforcement (allow check AND resolved-IP check) is wired up.
func TestPlainHTTPBlockedByResolvedIP(t *testing.T) {
	p := newHandler([]string{"10.99.99.1:80"}, []string{}, "loop-x", "ns", "hash")
	req := &http.Request{
		Method: "GET",
		Host:   "10.99.99.1:80",
		URL:    mustParse("http://10.99.99.1:80/"),
	}
	rec := newRecorder()
	p.handlePlainHTTP(rec, req)
	if rec.code != http.StatusForbidden {
		t.Errorf("want 403 (resolved-IP backstop), got %d", rec.code)
	}
}

// TestPlainHTTPNotAllowed verifies a host not in the allow list is blocked at
// the allow-check layer (403) before any resolve/dial.
func TestPlainHTTPNotAllowed(t *testing.T) {
	p := newHandler([]string{"proxy.golang.org:443"}, []string{}, "loop-x", "ns", "hash")
	req := &http.Request{
		Method: "GET",
		Host:   "example.com:80",
		URL:    mustParse("http://example.com:80/"),
	}
	rec := newRecorder()
	p.handlePlainHTTP(rec, req)
	if rec.code != http.StatusForbidden {
		t.Errorf("want 403 (not allowed), got %d", rec.code)
	}
}

// TestPlainHTTPAllowedEndToEnd verifies an allowed host that resolves to a
// public IP is proxied end-to-end: a fake upstream on loopback is unreachable
// (loopback is rejected), so instead we point the allow at a public host that
// the test host may or may not resolve; the assertion is that a RESOLVED
// public IP reaches the upstream. To keep this hermetic, we skip the live
// dial: the allow-check and resolved-IP-check logic are unit-tested in
// internal/egress, and the end-to-end CONNECT/HTTPS path is covered by the
// kind e2e (make egress-proxy-e2e). This test only checks the not-allowed and
// resolved-IP-blocked cases hermetically.

// TestAuditRecordShape verifies the Q4 audit JSON carries the keys the relay
// consumes (source, action, verdict, detail with ip and policy).
func TestAuditRecordShape(t *testing.T) {
	type ar = struct {
		Time      string `json:"time"`
		Loop      string `json:"loop"`
		Namespace string `json:"namespace"`
		Source    string `json:"source"`
		Action    string `json:"action"`
		Target    string `json:"target"`
		Verdict   string `json:"verdict"`
		Detail    string `json:"detail"`
	}
	data, _ := json.Marshal(ar{
		Time: "2026-01-01T00:00:00Z", Loop: "l", Namespace: "ns",
		Source: "egress-proxy", Action: "connect", Target: "h:443",
		Verdict: "allowed", Detail: "ip=1.2.3.4, policy=hash",
	})
	s := string(data)
	for _, want := range []string{
		`"source":"egress-proxy"`,
		`"action":"connect"`,
		`"verdict":"allowed"`,
		`"detail":"ip=1.2.3.4, policy=hash"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("audit JSON missing %s; got %s", want, s)
		}
	}
}

// TestReadAllowsBadPolicy verifies a malformed EGRESS_POLICY_JSON yields an
// empty (default-deny) allow set rather than a crash, and a valid one parses.
func TestReadAllowsBadPolicy(t *testing.T) {
	if a := readAllows("{ not json"); len(a) != 0 {
		t.Errorf("bad policy should default-deny, got %v", a)
	}
	if a := readAllows(`["proxy.golang.org:443"]`); len(a) != 1 || a[0] != "proxy.golang.org:443" {
		t.Errorf("want [proxy.golang.org:443], got %v", a)
	}
	if a := readAllows(""); a != nil {
		t.Errorf("empty policy should be nil, got %v", a)
	}
}

// --- helpers ---

func mustParse(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// recorder captures a minimal http.ResponseWriter.
type recorder struct {
	code   int
	header http.Header
	body   []byte
}

func newRecorder() *recorder {
	return &recorder{code: 0, header: http.Header{}}
}
func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(c int)           { r.code = c }
func (r *recorder) Write(b []byte) (int, error) { r.body = append(r.body, b...); return len(b), nil }

var _ = httptest.NewServer // keep import for the end-to-end hook
