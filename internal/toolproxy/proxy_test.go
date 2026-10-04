package toolproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Reverse-proxy shape (PR #70 review P1).
// ---------------------------------------------------------------------------

// A CONNECT request → 405 with no upstream dial (the upstream request log
// stays empty).
func TestConnect405NoDial(t *testing.T) {
	log := &requestLog{}
	up := newTestServer(t, log, 200, "")
	var sink stringsBuilder
	p := testProxy(t, up, rulesGET("/repos/acme/"), "cred", nil, &sink)

	req := httptest.NewRequest(http.MethodConnect, hostOf(t, up.URL), nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 405 {
		t.Fatalf("CONNECT status = %d, want 405", rec.Code)
	}
	if log.len() != 0 {
		t.Fatalf("CONNECT must not dial the upstream; saw %d requests", log.len())
	}
	// The audit line carries the 405 (audited).
	lines := auditLines(t, &sink)
	if len(lines) != 1 || lines[0]["status"] != float64(405) {
		t.Fatalf("CONNECT must be audited with status 405, got %+v", lines)
	}
}

// An absolute-form request (GET http://host/path) → 405, no dial.
func TestAbsoluteForm405NoDial(t *testing.T) {
	log := &requestLog{}
	up := newTestServer(t, log, 200, "")
	var sink stringsBuilder
	p := testProxy(t, up, rulesGET("/repos/acme/"), "cred", nil, &sink)

	// An absolute-form request: the URL is absolute and RequestURI carries
	// the full URI, as a real forward-proxy client sends it.
	u, err := url.Parse(up.URL + "/repos/acme/repo")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	req := &http.Request{
		Method:     "GET",
		URL:        u,
		Host:       u.Host,
		Header:     http.Header{},
		Body:       http.NoBody,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		RequestURI: u.String(),
	}
	req = req.Clone(context.Background())
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 405 {
		t.Fatalf("absolute-form status = %d, want 405", rec.Code)
	}
	if log.len() != 0 {
		t.Fatalf("absolute-form must not dial the upstream; saw %d requests", log.len())
	}
}

// An allowed origin-form request to an https upstream (an httptest TLS
// server whose CA the test trusts) arrives at the upstream with the
// credential header — proving the proxy originates a verified TLS
// connection and can see what a tunnel cannot.
//
// The TLS server's certificate is for example.test; the upstream base is
// https://example.test; the resolver maps example.test to publicIP and the
// dial seam maps publicIP to the loopback where the server listens. The
// proxy dials the resolved IP with SNI = the upstream HOSTNAME and verifies
// the certificate against that hostname (not the IP).
func TestAllowedRequestTLSCredential(t *testing.T) {
	log := &requestLog{}
	srv, ca := newTLSTestServer(t, log, "example.test")
	// The TLS server's certificate is for example.test. The upstream base
	// is https://example.test:443 (the configured hostname); the resolver
	// maps example.test to publicIP (carve-out clean) and the dial seam
	// maps publicIP back to the loopback where the server listens. The
	// proxy dials the resolved IP with SNI = the upstream HOSTNAME
	// (example.test) and verifies the certificate against it (not the
	// dial IP).
	lp := loopbackHostPort(t, srv.URL)
	resolver := &mockResolver{hosts: map[string][]string{"example.test": {publicIP}}, calls: map[string]int{}}
	var sink stringsBuilder
	// The upstream base is the configured hostname (example.test), not the
	// loopback: the resolver maps it to publicIP (carve-out clean) and the
	// dial seam maps publicIP back to the loopback where the server
	// listens. The proxy dials the resolved IP with SNI = the upstream
	// HOSTNAME (example.test) and verifies the certificate against it (not
	// the dial IP). The certificate is issued for example.test.
	p, err := newProxy(Config{
		ToolName:     "tool-x",
		UpstreamBase: "https://example.test:443",
		Rules:        rulesGET("/repos/acme/"),
		Credential:   "cred-secret",
		LoopName:     "loop-x",
		Namespace:    "ns-x",
		PolicyHash:   "pol-abc",
		Resolver:     resolver,
	}, dialToPublic(lp))
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	p.SetAuditSink(&sink)
	// Trust the test CA (the proxy verifies the upstream's certificate).
	tr, ok := p.client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("transport is not *http.Transport")
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("https upstream must have a TLS client config (SNI + verification)")
	}
	tr.TLSClientConfig.RootCAs = ca

	req := httptest.NewRequest("GET", "/repos/acme/repo", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (proxy must originate a verified TLS connection): %s", rec.Code, rec.Body.String())
	}
	entries := log.entriesList()
	if len(entries) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(entries))
	}
	if got := entries[0].headers.Get("Authorization"); got != "Bearer cred-secret" {
		t.Fatalf("upstream Authorization = %q, want the injected credential", got)
	}
	// The audit line must not contain the credential.
	if strings.Contains(sink.String(), "cred-secret") {
		t.Fatalf("audit line must not contain the credential: %s", sink.String())
	}
	// Exactly one lookup (no re-resolution).
	if n := resolver.count("example.test"); n != 1 {
		t.Fatalf("upstream host looked up %d times, want exactly 1", n)
	}
}

// ---------------------------------------------------------------------------
// Rule-bypass cases (PR #70 review P2).
// ---------------------------------------------------------------------------

// Path normalisation: encoded traversal forms are rejected (400) or cleaned
// to the canonical path before matching; neither outcome dials the upstream
// and neither lands on the allowed prefix by accident.
func TestNormalisationRejectsOrCleans(t *testing.T) {
	log := &requestLog{}
	up := newTestServer(t, log, 200, "")
	var sink stringsBuilder
	p := testProxy(t, up, rulesGET("/repos/acme/"), "cred", nil, &sink)

	bypassPaths := []struct{ name, raw string }{
		{"dotdot", "/repos/acme/../../etc"},
		{"percent-encoded", "/repos/acme%2f..%2fetc"},
		{"double-slash traversal", "/repos/acme//../../etc"},
		{"deep dotdot", "/repos/acme/repo/../../../../etc"},
		{"backslash", "/repos/\\../x"},
		{"NUL", "/repos/a\000b"},
	}
	for _, tc := range bypassPaths {
		t.Run(tc.name, func(t *testing.T) {
			// Build a request whose URL.Path carries the raw (unnormalised)
			// encoding, exactly as the agent sent it.
			u := &url.URL{Path: tc.raw}
			req := &http.Request{
				Method:     "GET",
				URL:        u,
				Host:       hostOf(t, up.URL),
				Header:     http.Header{},
				Body:       http.NoBody,
				Proto:      "HTTP/1.1",
				ProtoMajor: 1,
				ProtoMinor: 1,
				RequestURI: tc.raw,
			}
			req = req.Clone(context.Background())
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			// The outcome is 400 (rejected) or 403 (cleaned off the allowed
			// prefix). In BOTH cases: no upstream dial.
			if rec.Code != 400 && rec.Code != 403 {
				t.Fatalf("%s: status = %d, want 400 (rejected) or 403 (cleaned off prefix)", tc.name, rec.Code)
			}
		})
	}
	if log.len() != 0 {
		t.Fatalf("bypass cases must not dial the upstream; saw %d requests", log.len())
	}
	// The audit carried one line per request, none of them a forward.
	lines := auditLines(t, &sink)
	if len(lines) != len(bypassPaths) {
		t.Fatalf("got %d audit lines, want %d", len(lines), len(bypassPaths))
	}
	for i, l := range lines {
		if l["status"] != float64(400) && l["status"] != float64(403) {
			t.Fatalf("bypass %s audited with status %v, want 400 or 403", bypassPaths[i].name, l["status"])
		}
	}
}

// No redirect following: the upstream returns a 302 with a Location to a
// second server; the agent gets the 302 as-is and the second server sees no
// request.
func TestNoRedirectFollowing(t *testing.T) {
	log1 := &requestLog{}
	log2 := &requestLog{}
	second := newTestServer(t, log2, 200, "")
	first := newTestServer(t, log1, 302, second.URL+"/other")
	resolver := &mockResolver{hosts: map[string][]string{hostOf(t, first.URL): {publicIP}, hostOf(t, second.URL): {publicIP}}, calls: map[string]int{}}
	var sink stringsBuilder
	// Both test servers are on loopback; the dial seam maps the public
	// answer to FIRST's loopback (the proxy only dials the configured
	// upstream).
	p, err := newProxy(Config{
		ToolName:     "tool-x",
		UpstreamBase: first.URL,
		Rules:        rulesGET("/repos/acme/"),
		Credential:   "cred",
		LoopName:     "loop-x",
		Namespace:    "ns-x",
		PolicyHash:   "pol-abc",
		Resolver:     resolver,
	}, dialToPublic(loopbackHostPort(t, first.URL)))
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	p.SetAuditSink(&sink)

	req := httptest.NewRequest("GET", "/repos/acme/repo", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 302 {
		t.Fatalf("status = %d, want 302 as-is", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc == "" {
		t.Fatal("the 302's Location must be passed through to the agent")
	}
	if log2.len() != 0 {
		t.Fatalf("the second server saw %d requests; the proxy must NOT follow redirects", log2.len())
	}
}

// Agent-supplied auth is stripped: a forged Authorization +
// Proxy-Authorization never reach the upstream; the upstream sees the
// configured credential; the audit line contains neither value.
func TestAgentAuthStripped(t *testing.T) {
	log := &requestLog{}
	up := newTestServer(t, log, 200, "")
	var sink stringsBuilder
	p := testProxy(t, up, rulesGET("/repos/acme/"), "server-cred", nil, &sink)

	req := httptest.NewRequest("GET", "/repos/acme/repo", nil)
	req.Header.Set("Authorization", "Bearer agent-forged")
	req.Header.Set("Proxy-Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	entries := log.entriesList()
	if len(entries) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(entries))
	}
	if got := entries[0].headers.Get("Authorization"); got != "Bearer server-cred" {
		t.Fatalf("upstream Authorization = %q, want the configured credential (the agent's must be stripped)", got)
	}
	if got := entries[0].headers.Get("Proxy-Authorization"); got != "" {
		t.Fatalf("Proxy-Authorization must not reach the upstream, got %q", got)
	}
	// The audit line contains neither the agent's nor the server's value.
	for _, secret := range []string{"agent-forged", "server-cred", "dXNlcjpwYXNz"} {
		if strings.Contains(sink.String(), secret) {
			t.Fatalf("audit line must not contain %q: %s", secret, sink.String())
		}
	}
}

// Without a credential, there is no Authorization header upstream.
func TestNoCredentialNoHeader(t *testing.T) {
	log := &requestLog{}
	up := newTestServer(t, log, 200, "")
	var sink stringsBuilder
	p := testProxy(t, up, rulesGET("/repos/acme/"), "", nil, &sink)

	req := httptest.NewRequest("GET", "/repos/acme/repo", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	entries := log.entriesList()
	if len(entries) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(entries))
	}
	if got := entries[0].headers.Get("Authorization"); got != "" {
		t.Fatalf("no credential configured: Authorization must be absent, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Resolved-IP carve-outs (I42a backstop).
// ---------------------------------------------------------------------------

// A resolver returning a carved-out IP (pod CIDR 10/8) → 403 + audit, no
// dial.
func TestResolvedIPCarveOutNoDial(t *testing.T) {
	log := &requestLog{}
	up := newTestServer(t, log, 200, "")
	resolver := &mockResolver{hosts: map[string][]string{hostOf(t, up.URL): {"10.99.0.5"}}, calls: map[string]int{}}
	var sink stringsBuilder
	p := testProxy(t, up, rulesGET("/repos/acme/"), "cred", resolver, &sink)

	req := httptest.NewRequest("GET", "/repos/acme/repo", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403 (carved-out resolved IP)", rec.Code)
	}
	if log.len() != 0 {
		t.Fatalf("carved-out IP must not dial the upstream; saw %d requests", log.len())
	}
	lines := auditLines(t, &sink)
	if len(lines) != 1 || lines[0]["status"] != float64(403) {
		t.Fatalf("carved-out IP must be audited with status 403, got %+v", lines)
	}
}

// A resolver that fails (NXDOMAIN) → 403, no dial.
func TestResolvedIPNXDOMAIN(t *testing.T) {
	log := &requestLog{}
	up := newTestServer(t, log, 200, "")
	resolver := &mockResolver{hosts: map[string][]string{}, errs: map[string]error{hostOf(t, up.URL): fmt.Errorf("no such host")}, calls: map[string]int{}}
	var sink stringsBuilder
	p := testProxy(t, up, rulesGET("/repos/acme/"), "cred", resolver, &sink)

	req := httptest.NewRequest("GET", "/repos/acme/repo", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403 (NXDOMAIN)", rec.Code)
	}
	if log.len() != 0 {
		t.Fatalf("NXDOMAIN must not dial the upstream; saw %d requests", log.len())
	}
}

// The resolver answering a mix of public + private (a rebind /
// split-horizon answer) → 403, no dial.
func TestResolvedIPMixedPublicPrivate(t *testing.T) {
	log := &requestLog{}
	up := newTestServer(t, log, 200, "")
	resolver := &mockResolver{hosts: map[string][]string{hostOf(t, up.URL): {publicIP, "192.168.1.5"}}, calls: map[string]int{}}
	var sink stringsBuilder
	p := testProxy(t, up, rulesGET("/repos/acme/"), "cred", resolver, &sink)

	req := httptest.NewRequest("GET", "/repos/acme/repo", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403 (a private IP among the answers carves the host out)", rec.Code)
	}
	if log.len() != 0 {
		t.Fatalf("mixed public+private must not dial the upstream; saw %d requests", log.len())
	}
}

// No re-resolution: the proxy resolves the upstream host EXACTLY ONCE per
// request (the dial goes to that answer; a second lookup would be the TOCTOU
// re-resolution the backstop exists to prevent).
func TestNoReResolution(t *testing.T) {
	log := &requestLog{}
	up := newTestServer(t, log, 200, "")
	resolver := &mockResolver{hosts: map[string][]string{}, calls: map[string]int{}}
	var sink stringsBuilder
	p := testProxy(t, up, rulesGET("/repos/acme/"), "cred", resolver, &sink)

	req := httptest.NewRequest("GET", "/repos/acme/repo", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if n := resolver.count(hostOf(t, up.URL)); n != 1 {
		t.Fatalf("upstream host looked up %d times, want exactly 1 (no re-resolution: the dial uses the first answer)", n)
	}
}

// ---------------------------------------------------------------------------
// Audit (one line per request, allowed and blocked).
// ---------------------------------------------------------------------------

func TestAuditLineAllowedAndBlocked(t *testing.T) {
	log := &requestLog{}
	up := newTestServer(t, log, 418, "")
	var sink stringsBuilder
	p := testProxy(t, up, rulesGET("/repos/acme/"), "cred", nil, &sink)

	// Allowed: the upstream's 418 status is carried through and audited.
	req := httptest.NewRequest("GET", "/repos/acme/repo", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 418 {
		t.Fatalf("status = %d, want 418 (the upstream's status as-is)", rec.Code)
	}
	// Blocked: no match.
	req2 := httptest.NewRequest("GET", "/not-allowed", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	if rec2.Code != 403 {
		t.Fatalf("blocked status = %d, want 403", rec2.Code)
	}

	lines := auditLines(t, &sink)
	if len(lines) != 2 {
		t.Fatalf("got %d audit lines, want 2", len(lines))
	}
	// Allowed: status = the upstream's status (418).
	if lines[0]["status"] != float64(418) {
		t.Fatalf("allowed audit status = %v, want 418 (the upstream's status)", lines[0]["status"])
	}
	if lines[0]["method"] != "GET" || lines[0]["path"] != "/repos/acme/repo" {
		t.Fatalf("allowed audit method/path = %v %v", lines[0]["method"], lines[0]["path"])
	}
	if lines[0]["policy"] != "pol-abc" {
		t.Fatalf("audit must carry the policy hash, got %v", lines[0]["policy"])
	}
	if lines[0]["source"] != Source {
		t.Fatalf("audit source = %v, want %s", lines[0]["source"], Source)
	}
	// Blocked: status 403 and the method/path.
	if lines[1]["status"] != float64(403) {
		t.Fatalf("blocked audit status = %v, want 403", lines[1]["status"])
	}
	if lines[1]["method"] != "GET" || lines[1]["path"] != "/not-allowed" {
		t.Fatalf("blocked audit method/path = %v %v", lines[1]["method"], lines[1]["path"])
	}
	if lines[1]["policy"] != "pol-abc" {
		t.Fatalf("blocked audit must carry the policy hash, got %v", lines[1]["policy"])
	}
}

// The audit line for a credentialed request contains no substring of the
// credential value (redaction).
func TestAuditRedaction(t *testing.T) {
	log := &requestLog{}
	up := newTestServer(t, log, 200, "")
	var sink stringsBuilder
	p := testProxy(t, up, rulesGET("/repos/acme/"), "s3cr3t-value", nil, &sink)

	req := httptest.NewRequest("GET", "/repos/acme/repo", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(sink.String(), "s3cr3t-value") {
		t.Fatalf("audit line must not contain a substring of the credential: %s", sink.String())
	}
}

// ---------------------------------------------------------------------------
// Rule engine → status (the handler's verdicts, table-driven).
// ---------------------------------------------------------------------------

func TestRuleVerdicts(t *testing.T) {
	log := &requestLog{}
	up := newTestServer(t, log, 200, "")
	var sink stringsBuilder
	p := testProxy(t, up, rulesGET("/repos/acme/"), "cred", nil, &sink)

	cases := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{"allowed", "GET", "/repos/acme/repo", 200},
		{"wrong method → 403", "DELETE", "/repos/acme/repo", 403},
		{"disallowed prefix → 403", "GET", "/orgs/acme/repo", 403},
		{"segment boundary → 403", "GET", "/repos/acmer", 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("%s: status = %d, want %d", tc.name, rec.Code, tc.status)
			}
		})
	}
	// Only the allowed case dials the upstream.
	if log.len() != 1 {
		t.Fatalf("upstream saw %d requests, want 1 (only the allowed case)", log.len())
	}
	// Every request is audited (allowed + the three 403s).
	if len(auditLines(t, &sink)) != 4 {
		t.Fatalf("every request must be audited")
	}
}

// Empty rule set → everything 403.
func TestEmptyRulesDenyAllAtHandler(t *testing.T) {
	log := &requestLog{}
	up := newTestServer(t, log, 200, "")
	var sink stringsBuilder
	p := testProxy(t, up, nil, "cred", nil, &sink)

	req := httptest.NewRequest("GET", "/repos/acme/repo", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403 (no rules)", rec.Code)
	}
	if log.len() != 0 {
		t.Fatalf("no rules: must not dial the upstream")
	}
}
