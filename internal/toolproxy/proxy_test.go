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
	p := testProxy(t, up, rulesGET(), cred, &sink)

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
	p := testProxy(t, up, rulesGET(), cred, &sink)

	// An absolute-form request: the URL is absolute and RequestURI carries
	// the full URI, as a real forward-proxy client sends it.
	u, err := url.Parse(up.URL + toolPath)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	req := &http.Request{
		Method:     methodGET,
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
	srv, ca := newTLSTestServer(t, log, tlsUpstreamHost)
	// The TLS server's certificate is issued for example.test. The upstream
	// base is https://example.test:443 (the configured hostname); the
	// resolver maps example.test to publicIP (carve-out clean) and the dial
	// seam maps publicIP back to the loopback where the server listens.
	// The proxy dials the RESOLVED IP with SNI = the upstream HOSTNAME
	// (example.test) and verifies the certificate against that hostname
	// (not the dial IP) — proving it originates a verified TLS connection
	// and can see what a tunnel cannot.
	port := loopbackPort(t, srv.URL)
	resolver := &mockResolver{hosts: map[string][]string{tlsUpstreamHost: {publicIP}}, calls: map[string]int{}}
	var sink stringsBuilder
	p, err := newProxy(Config{
		ToolName:     toolName,
		UpstreamBase: "https://example.test:443",
		Rules:        rulesGET(),
		Credential:   "cred-secret",
		LoopName:     loopName,
		Namespace:    nsName,
		PolicyHash:   policy,
		Resolver:     resolver,
	}, dialToPublic(port))
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	p.SetAuditSink(&sink)
	// The transport's TLS client config sends SNI (ServerName) = the
	// upstream hostname and verifies the certificate against it.
	tr, ok := p.client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("transport is not *http.Transport")
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("https upstream must have a TLS client config (SNI + verification)")
	}
	if tr.TLSClientConfig.ServerName != tlsUpstreamHost {
		t.Fatalf("TLS ServerName = %q, want the upstream hostname (SNI), not the dial IP", tr.TLSClientConfig.ServerName)
	}
	tr.TLSClientConfig.RootCAs = ca

	req := httptest.NewRequest(methodGET, toolPath, nil)
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
	if n := resolver.count(tlsUpstreamHost); n != 1 {
		t.Fatalf("upstream host looked up %d times, want exactly 1", n)
	}
}

// The SNI/verification is REAL: an upstream certificate that does NOT cover
// the hostname the proxy dials must be REJECTED (502, no upstream request
// seen), not accepted. This proves the proxy verifies the certificate
// against the upstream hostname rather than trusting any peer cert — a
// tunnel would have no certificate to verify at all.
func TestTLSCertNotForDialHostRejected(t *testing.T) {
	log := &requestLog{}
	// The upstream base is example.test:443, but the certificate the test
	// server presents is issued for a DIFFERENT hostname (wrong.test), so
	// verification against example.test must fail.
	srv, _ := newTLSTestServer(t, log, "wrong.test")
	port := loopbackPort(t, srv.URL)
	resolver := &mockResolver{hosts: map[string][]string{tlsUpstreamHost: {publicIP}}, calls: map[string]int{}}
	var sink stringsBuilder
	p, err := newProxy(Config{
		ToolName:     toolName,
		UpstreamBase: "https://example.test:443",
		Rules:        rulesGET(),
		Credential:   "cred-secret",
		LoopName:     loopName,
		Namespace:    nsName,
		PolicyHash:   policy,
		Resolver:     resolver,
	}, dialToPublic(port))
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	p.SetAuditSink(&sink)
	// No CA trust (and even with trust, the cert is for wrong.test, not
	// example.test, so verification against the SNI hostname fails).
	req := httptest.NewRequest(methodGET, toolPath, nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	// A certificate that does not cover the dial host is rejected: 502
	// (upstream TLS error), and the upstream server saw no completed
	// request (the handshake failed before any HTTP exchange).
	if rec.Code != 502 {
		t.Fatalf("status = %d, want 502 (certificate for %q must not verify against the dial host)", rec.Code, "wrong.test")
	}
	if n := log.len(); n != 0 {
		t.Fatalf("upstream saw %d requests; a failed TLS handshake must not complete an HTTP request", n)
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
	p := testProxy(t, up, rulesGET(), cred, &sink)

	bypassPaths := []struct{ name, raw string }{
		{"dotdot", traversalDotDot},
		{"percent-encoded", traversalPCT},
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
				Method:     methodGET,
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
		ToolName:     toolName,
		UpstreamBase: first.URL,
		Rules:        rulesGET(),
		Credential:   cred,
		LoopName:     loopName,
		Namespace:    nsName,
		PolicyHash:   policy,
		Resolver:     resolver,
	}, dialToPublic(loopbackPort(t, first.URL)))
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	p.SetAuditSink(&sink)

	req := httptest.NewRequest(methodGET, toolPath, nil)
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
	p := testProxy(t, up, rulesGET(), "server-cred", &sink)

	req := httptest.NewRequest(methodGET, toolPath, nil)
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
	p := testProxy(t, up, rulesGET(), "", &sink)

	req := httptest.NewRequest(methodGET, toolPath, nil)
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
// dial. The upstream base is a HOSTNAME (not a literal IP) so the resolver
// is consulted and the carve-out check runs.
func TestResolvedIPCarveOutNoDial(t *testing.T) {
	log := &requestLog{}
	_ = log
	upBase := upstreamBaseHTTPS // a hostname; the resolver answers 10.99.0.5
	resolver := &mockResolver{hosts: map[string][]string{upstreamHost: {"10.99.0.5"}}, calls: map[string]int{}}
	var sink stringsBuilder
	p, err := newProxy(Config{
		ToolName:     toolName,
		UpstreamBase: upBase,
		Rules:        rulesGET(),
		Credential:   cred,
		LoopName:     loopName,
		Namespace:    nsName,
		PolicyHash:   policy,
		Resolver:     resolver,
	}, nil)
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	p.SetAuditSink(&sink)

	req := httptest.NewRequest(methodGET, toolPath, nil)
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
	_ = log
	upBase := upstreamBaseHTTPS
	resolver := &mockResolver{hosts: map[string][]string{}, errs: map[string]error{upstreamHost: fmt.Errorf("no such host")}, calls: map[string]int{}}
	var sink stringsBuilder
	p, err := newProxy(Config{
		ToolName:     toolName,
		UpstreamBase: upBase,
		Rules:        rulesGET(),
		Credential:   cred,
		LoopName:     loopName,
		Namespace:    nsName,
		PolicyHash:   policy,
		Resolver:     resolver,
	}, nil)
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	p.SetAuditSink(&sink)

	req := httptest.NewRequest(methodGET, toolPath, nil)
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
	_ = log
	upBase := upstreamBaseHTTPS
	resolver := &mockResolver{hosts: map[string][]string{upstreamHost: {publicIP, "192.168.1.5"}}, calls: map[string]int{}}
	var sink stringsBuilder
	p, err := newProxy(Config{
		ToolName:     toolName,
		UpstreamBase: upBase,
		Rules:        rulesGET(),
		Credential:   cred,
		LoopName:     loopName,
		Namespace:    nsName,
		PolicyHash:   policy,
		Resolver:     resolver,
	}, nil)
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	p.SetAuditSink(&sink)

	req := httptest.NewRequest(methodGET, toolPath, nil)
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
	// A hostname upstream so the resolver is consulted; it answers the
	// loopback (carve-out clean? no — 127.0.0.1 is carved out; so answer a
	// public IP and dial-seam it back to the loopback).
	upBase := upstreamBaseHTTP // http (the test server is plain HTTP); the
	// resolver answers publicIP (carve-out clean) and the dial seam maps it
	// back to the loopback.
	port := loopbackPort(t, up.URL)
	resolver := &mockResolver{hosts: map[string][]string{upstreamHost: {publicIP}}, calls: map[string]int{}}
	var sink stringsBuilder
	p, err := newProxy(Config{
		ToolName:     toolName,
		UpstreamBase: upBase,
		Rules:        rulesGET(),
		Credential:   cred,
		LoopName:     loopName,
		Namespace:    nsName,
		PolicyHash:   policy,
		Resolver:     resolver,
	}, dialToPublic(port))
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	p.SetAuditSink(&sink)

	req := httptest.NewRequest(methodGET, toolPath, nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if n := resolver.count(upstreamHost); n != 1 {
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
	p := testProxy(t, up, rulesGET(), cred, &sink)

	// Allowed: the upstream's 418 status is carried through and audited.
	req := httptest.NewRequest(methodGET, toolPath, nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 418 {
		t.Fatalf("status = %d, want 418 (the upstream's status as-is)", rec.Code)
	}
	// Blocked: no match.
	req2 := httptest.NewRequest(methodGET, "/not-allowed", nil)
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
	if lines[0]["method"] != methodGET || lines[0]["path"] != toolPath {
		t.Fatalf("allowed audit method/path = %v %v", lines[0]["method"], lines[0]["path"])
	}
	if lines[0]["policy"] != policy {
		t.Fatalf("audit must carry the policy hash, got %v", lines[0]["policy"])
	}
	if lines[0]["source"] != Source {
		t.Fatalf("audit source = %v, want %s", lines[0]["source"], Source)
	}
	// Blocked: status 403 and the method/path.
	if lines[1]["status"] != float64(403) {
		t.Fatalf("blocked audit status = %v, want 403", lines[1]["status"])
	}
	if lines[1]["method"] != methodGET || lines[1]["path"] != "/not-allowed" {
		t.Fatalf("blocked audit method/path = %v %v", lines[1]["method"], lines[1]["path"])
	}
	if lines[1]["policy"] != policy {
		t.Fatalf("blocked audit must carry the policy hash, got %v", lines[1]["policy"])
	}
}

// The audit line for a credentialed request contains no substring of the
// credential value (redaction).
func TestAuditRedaction(t *testing.T) {
	log := &requestLog{}
	up := newTestServer(t, log, 200, "")
	var sink stringsBuilder
	p := testProxy(t, up, rulesGET(), "s3cr3t-value", &sink)

	req := httptest.NewRequest(methodGET, toolPath, nil)
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
	p := testProxy(t, up, rulesGET(), cred, &sink)

	cases := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{"allowed", methodGET, toolPath, 200},
		{"wrong method → 403", methodDELETE, toolPath, 403},
		{"disallowed prefix → 403", methodGET, "/orgs/acme/repo", 403},
		{"segment boundary → 403", methodGET, segmentBoundary, 403},
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

// The upstream base prefix (e.g. https://host/api/v3) is forwarded in
// front of the agent path: an allowed GET /repos/acme/x arrives at the
// upstream at /api/v3/repos/acme/x, while a disallowed path still 403s
// with no dial.
//
// This test FAILS on 870944a where parseUpstreamBase drops u.Path and
// buildUpstreamRequest forwards only the agent path (no prefix).
func TestBasePrefixForwarded(t *testing.T) {
	log := &requestLog{}
	// Upstream base is http://upstream.test/api/v3 (a literal-IP is not
	// available here; the resolver maps upstream.test to loopback via dialTo
	// nil, so the base host is a hostname resolved through the mock).
	// We use a non-IP hostname so the resolver is exercised.
	up := newTestServer(t, log, 200, "")
	// The test server listens on 127.0.0.1:port; the base prefix is
	// /api/v3. We configure the upstream base as the test URL with the
	// prefix appended, and dialTo nil (literal-IP check, no lookup).
	baseWithPrefix := up.URL + "/api/v3"

	var sink stringsBuilder
	p, err := newProxy(Config{
		ToolName:     toolName,
		UpstreamBase: baseWithPrefix,
		Rules:        rulesGET(),
		Credential:   cred,
		LoopName:     loopName,
		Namespace:    nsName,
		PolicyHash:   policy,
		// mockResolver: the base host is 127.0.0.1 (literal-IP, checked
		// directly against carve-outs, no lookup needed).
		Resolver: &mockResolver{hosts: map[string][]string{}, calls: map[string]int{}},
	}, nil)
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	p.SetAuditSink(&sink)

	// Allowed request: GET /repos/acme/repo → should arrive at /api/v3/repos/acme/repo
	req := httptest.NewRequest(methodGET, toolPath, nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("allowed request status = %d, want 200", rec.Code)
	}
	entries := log.entriesList()
	if len(entries) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(entries))
	}
	wantPath := "/api/v3" + toolPath
	if entries[0].path != wantPath {
		t.Fatalf("upstream path = %q, want %q (base prefix must be forwarded)", entries[0].path, wantPath)
	}

	// Disallowed path: still 403, no dial.
	log2 := &requestLog{}
	up2 := newTestServer(t, log2, 200, "")
	base2 := up2.URL + "/api/v3"
	var sink2 stringsBuilder
	p2, err := newProxy(Config{
		ToolName:     toolName,
		UpstreamBase: base2,
		Rules:        rulesGET(), // only allows /repos/acme/
		Credential:   cred,
		LoopName:     loopName,
		Namespace:    nsName,
		PolicyHash:   policy,
		Resolver:     &mockResolver{hosts: map[string][]string{}, calls: map[string]int{}},
	}, nil)
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	p2.SetAuditSink(&sink2)

	req2 := httptest.NewRequest(methodGET, "/etc/passwd", nil)
	rec2 := httptest.NewRecorder()
	p2.ServeHTTP(rec2, req2)
	if rec2.Code != 403 {
		t.Fatalf("disallowed request status = %d, want 403", rec2.Code)
	}
	if log2.len() != 0 {
		t.Fatalf("disallowed request must not dial the upstream; saw %d", log2.len())
	}
}

// Empty rule set → everything 403.
func TestEmptyRulesDenyAllAtHandler(t *testing.T) {
	log := &requestLog{}
	up := newTestServer(t, log, 200, "")
	var sink stringsBuilder
	p := testProxy(t, up, nil, cred, &sink)

	req := httptest.NewRequest(methodGET, toolPath, nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403 (no rules)", rec.Code)
	}
	if log.len() != 0 {
		t.Fatalf("no rules: must not dial the upstream")
	}
}
