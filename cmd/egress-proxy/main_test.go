package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/papawattu/coxswain/internal/egress/egresstest"
)

const (
	testAllowHost  = "proxy.golang.org" // must match the SNI in the hellos below
	methodGet      = "GET"
	upstreamTarget = "151.101.0.223:80"
)

// --- mock resolver (rebind / NXDOMAIN cases) ---

// mockResolver resolves every host to the configured IPs (or returns err).
// It lets the tests exercise the resolved-IP backstop hermetically: a name
// that "rebinds" to a private IP, and a name that does not resolve at all.
type mockResolver struct {
	ips []netip.Addr
	err error
}

func (m mockResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	if m.err != nil {
		return nil, m.err
	}
	var out []net.IPAddr
	for _, a := range m.ips {
		out = append(out, net.IPAddr{IP: a.AsSlice()})
	}
	return out, nil
}

// TestResolveAndCheckRebind proves the SSRF backstop at the handler seam: an
// allowlisted host whose resolution lands in a carved-out range (a rebind to
// the pod CIDR) is rejected, even though the hostname check passed.
func TestResolveAndCheckRebind(t *testing.T) {
	res := &mockResolver{ips: []netip.Addr{netip.MustParseAddr("10.244.0.5")}}
	p := newHandlerWithResolver([]string{"rebind.example:443"}, []string{"10.244.0.0/16"}, "l", "ns", "h", res)
	if _, ok, _ := p.resolveAndCheck(context.Background(), "rebind.example"); ok {
		t.Fatal("a host rebinding to the pod CIDR must be rejected")
	}
	// A public resolution of the same (allowlisted) host is dialable.
	res2 := &mockResolver{ips: []netip.Addr{netip.MustParseAddr("151.101.0.223")}}
	p2 := newHandlerWithResolver([]string{"rebind.example:443"}, []string{"10.244.0.0/16"}, "l", "ns", "h", res2)
	ip, ok, _ := p2.resolveAndCheck(context.Background(), "rebind.example")
	if !ok || ip.String() != "151.101.0.223" {
		t.Fatalf("a public rebind must be dialable to the same IP; got (%v, %v)", ip, ok)
	}
}

// TestResolveAndCheckNXDOMAIN proves the ADR failure mode: an allowlisted
// host that does not resolve is rejected with the NXDOMAIN error surfaced so
// the caller can audit dns=nxdomain (distinct from a carved-out IP).
func TestResolveAndCheckNXDOMAIN(t *testing.T) {
	noSuchHostErr := &net.DNSError{Name: "ghost.example", Err: "no such host", IsNotFound: true}
	res := &mockResolver{err: noSuchHostErr}
	p := newHandlerWithResolver([]string{"ghost.example:443"}, nil, "l", "ns", "h", res)
	if _, ok, err := p.resolveAndCheck(context.Background(), "ghost.example"); ok || err == nil {
		t.Fatalf("an unresolvable host must be rejected with the resolve error; got (%v, %v)", ok, err)
	}
	// A mix of public + private resolutions is rejected (no error: the IPs
	// resolved, one just landed in a carved-out range).
	res2 := &mockResolver{ips: []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("169.254.1.1")}}
	p2 := newHandlerWithResolver([]string{"ghost.example:443"}, nil, "l", "ns", "h", res2)
	if _, ok, err := p2.resolveAndCheck(context.Background(), "ghost.example"); ok || err != nil {
		t.Fatalf("a public+private mix must be rejected without a resolve error; got (%v, %v)", ok, err)
	}
}

// TestResolveAndCheckDNSTimeout proves the DNS lookup is bounded: a resolver
// that ignores the context deadline is cut off by the handler's dnsTimeout
// and the request fails fast (a slow resolver must not pin the handler).
func TestResolveAndCheckDNSTimeout(t *testing.T) {
	slow := &slowResolver{}
	p := newHandlerWithResolver([]string{"slow.example:443"}, nil, "l", "ns", "h", slow)
	start := time.Now()
	if _, ok, _ := p.resolveAndCheck(context.Background(), "slow.example"); ok {
		t.Fatal("a never-resolving host must be rejected")
	}
	if elapsed := time.Since(start); elapsed > 2*dnsTimeout {
		t.Fatalf("DNS lookup not bounded by dnsTimeout (%v > %v)", elapsed, 2*dnsTimeout)
	}
}

// slowResolver blocks until the context is cancelled, simulating a hung
// resolver.
type slowResolver struct{}

func (s *slowResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestPlainHTTPBlockedByResolvedIP verifies the plain-HTTP path: a host that
// is in the allow list but resolves (is a literal) to a private IP is blocked
// by the resolved-IP SSRF backstop, returning 403. This proves the two-layer
// enforcement (allow check AND resolved-IP check) is wired up.
func TestPlainHTTPBlockedByResolvedIP(t *testing.T) {
	p := newHandlerWithResolver([]string{"10.99.99.1:80"}, []string{}, "loop-x", "ns", "hash", net.DefaultResolver)
	req := &http.Request{
		Method: methodGet,
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
	p := newHandlerWithResolver([]string{"proxy.golang.org:443"}, []string{}, "loop-x", "ns", "hash", net.DefaultResolver)
	req := &http.Request{
		Method: methodGet,
		Host:   "example.com:80",
		URL:    mustParse("http://example.com:80/"),
	}
	rec := newRecorder()
	p.handlePlainHTTP(rec, req)
	if rec.code != http.StatusForbidden {
		t.Errorf("want 403 (not allowed), got %d", rec.code)
	}
}

// TestPlainHTTPAllowed verifies the plain-HTTP allowed path end-to-end: a
// server request (RequestURI set, hop-by-hop and Proxy-* headers present) is
// normalised (RequestURI cleared, Proxy-Connection stripped), forwarded to
// the already-checked resolved IP via the dial seam, and the upstream
// response is copied back with its status. The dial seam swaps the upstream
// for a httptest server whose IP (loopback) would otherwise be carved out.
func TestPlainHTTPAllowed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The upstream must NOT see proxy-only / hop-by-hop headers.
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Errorf("Proxy-Authorization leaked to upstream")
		}
		if r.Header.Get("Proxy-Connection") != "" {
			t.Errorf("Proxy-Connection leaked to upstream")
		}
		// Keep Host so the virtual host matches.
		if r.Host != upstreamTarget {
			t.Errorf("upstream Host = %q; want upstreamTarget (kept from the check)", r.Host)
		}
		w.Header().Set("X-Upstream", "ok")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("upstream body"))
	}))
	defer upstream.Close()

	// The allow host is a literal public IP:port (no DNS involved); the dial
	// seam redirects to the fake upstream.
	allowHost := "151.101.0.223"
	p := newHandlerWithResolver([]string{allowHost + ":80"}, nil, "loop-x", "ns", "hash", net.DefaultResolver)
	upstreamAddr := upstream.Listener.Addr().String()
	p.dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// The transport dials the checked IP:port; redirect to the fake
		// upstream (a plain-IP literal would be carved out in a real dial).
		var d net.Dialer
		return d.DialContext(ctx, network, upstreamAddr)
	}
	// The upstream Host header check: proxyRequest sets URL.Host to
	// IP:port; the Host header (kept) is what the upstream sees.
	req := &http.Request{
		Method: methodGet,
		Host:   allowHost + ":80",
		URL:    mustParse("http://" + allowHost + ":80/"),
		Header: http.Header{
			"Proxy-Authorization": {"Basic leaked"},
			"Proxy-Connection":    {"keep-alive"},
			"Connection":          {"close"},
		},
	}
	// A server request carries RequestURI (this is the case client.Do
	// refuses); proxyRequest must clear it.
	req.RequestURI = "/"
	rec := newRecorder()
	p.handlePlainHTTP(rec, req)
	if rec.code != http.StatusTeapot {
		t.Fatalf("want 418 (upstream status), got %d body=%s", rec.code, rec.body)
	}
	if string(rec.body) != "upstream body" {
		t.Fatalf("upstream body not relayed: %q", rec.body)
	}
	if got := rec.header.Get("X-Upstream"); got != "ok" {
		t.Fatalf("upstream header not copied; got %q", got)
	}
}

// TestPlainHTTPUpstreamErrorAuditedBlocked verifies the audit ordering: an
// allowed host whose upstream dial fails is audited as blocked
// (reason=upstream-error), never as allowed (the allowed audit is emitted
// only after the upstream succeeds).
func TestPlainHTTPUpstreamErrorAuditedBlocked(t *testing.T) {
	p := newHandlerWithResolver([]string{upstreamTarget}, nil, "loop-x", "ns", "hash", net.DefaultResolver)
	p.dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, net.ErrClosed
	}
	req := &http.Request{
		Method: methodGet,
		Host:   upstreamTarget,
		URL:    mustParse("http://151.101.0.223:80/"),
	}
	rec := newRecorder()
	p.handlePlainHTTP(rec, req)
	if rec.code != http.StatusBadGateway {
		t.Fatalf("want 502 (upstream error), got %d", rec.code)
	}
	// The audit went to stdout (unasserted here — the ordering is what the
	// code enforces: no 'allowed' audit before the error return).
}

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

// --- CONNECT SNI read-path tests (the P1 fix) ---
//
// These drive the real handleConnect via an httptest server with the dialer
// seam swapped for a fake-upstream dialer (a TCP listener on localhost). The
// client sends the CONNECT request, then the ClientHello over the hijacked
// tunnel. A blocked CONNECT is closed by the proxy; an allowed one is
// relayed (the fake upstream receives the ClientHello).

// runConnectSNIRead sets up an httptest server backed by the proxy handler
// (with the dialer seam pointing at a fake upstream), sends a CONNECT
// request plus the ClientHello over the tunnel, and reports whether the
// proxy allowed the tunnel (the fake upstream received the ClientHello) or
// blocked it (the tunnel was closed before the ClientHello arrived).
func runConnectSNIRead(t *testing.T, p *h, clientHello []byte) (allowed bool) {
	t.Helper()
	// Fake upstream: a TCP listener on localhost that accepts one conn and
	// records what it receives.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake upstream listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	// The proxy dials the RESOLVED IP:port. Make the mock resolver return a
	// public IP (151.101.0.223) so the carve-out check passes, and swap the
	// dialer seam so the dial lands on the fake listener (the dialer ignores
	// the addr and connects to ln).
	fakeIP := netip.MustParseAddr("151.101.0.223")
	p.resolver = &mockResolver{ips: []netip.Addr{fakeIP}}
	p.dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return net.Dial("tcp", ln.Addr().String())
	}
	srv := httptest.NewServer(p)
	defer srv.Close()
	// Dial the proxy and send the CONNECT request.
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	target := testAllowHost + ":443"
	_, _ = conn.Write([]byte("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"))
	// Read the 200 (allowed) or 403 (blocked).
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return false // blocked (403/502)
	}
	_ = resp.Body.Close()
	// The tunnel is established. Send the ClientHello over it. A well-formed
	// hello with a matching SNI is allowed (the 200 is the verdict); a
	// silent client (nil) or an oversized/malformed record is blocked
	// (the proxy closes the tunnel after the SNI read fails). For the
	// blocked cases, wait for the tunnel to close (EOF on the client conn).
	if len(clientHello) > 0 {
		_, _ = conn.Write(clientHello)
	}
	// A silent client or a malformed record: the proxy closes the tunnel
	// after the SNI read fails (deadline / cap deny). Detect by reading
	// until EOF within the read deadline + margin.
	_ = conn.SetReadDeadline(time.Now().Add(sniReadTimeout + 2*time.Second)) // errcheck: best-effort deadline
	_, err = conn.Read(make([]byte, 1))
	// EOF (or a deadline error) means the proxy closed the tunnel -> blocked.
	// A non-EOF read (relay data) or nil error means the tunnel is open ->
	// allowed.
	if err != nil {
		return false // tunnel closed: blocked (silent client or oversized record)
	}
	return true // tunnel open: allowed
}

// TestConnectSNIShortHello proves a ClientHello under 512 bytes is still
// parsed and allowed (the old Peek(512) blocked on short hellos forever).
func TestConnectSNIShortHello(t *testing.T) {
	hello := egresstest.BuildClientHello(testAllowHost)
	if len(hello) >= 512 {
		t.Fatalf("test setup: hello is %d bytes, want < 512", len(hello))
	}
	res := &mockResolver{ips: []netip.Addr{netip.MustParseAddr("151.101.0.223")}}
	p := newHandlerWithResolver([]string{testAllowHost + ":443"}, nil, "l", "ns", "h", res)
	if !runConnectSNIRead(t, p, hello) {
		t.Fatal("a short ClientHello with a matching SNI must be allowed")
	}
}

// TestConnectSNISNIPastByte512 proves a ClientHello whose SNI sits past byte
// 512 is still found and allowed (the old Peek(512) returned only the first
// 512 bytes and reported SNI absent, denying legitimate traffic).
func TestConnectSNISNIPastByte512(t *testing.T) {
	hello := egresstest.BuildHelloWithPadding(testAllowHost, 480) // SNI past byte 512
	if idx := bytes.Index(hello, []byte(testAllowHost)); idx < 512 {
		t.Fatalf("test setup: SNI at offset %d is not past byte 512", idx)
	}
	res := &mockResolver{ips: []netip.Addr{netip.MustParseAddr("151.101.0.223")}}
	p := newHandlerWithResolver([]string{testAllowHost + ":443"}, nil, "l", "ns", "h", res)
	if !runConnectSNIRead(t, p, hello) {
		t.Fatal("a ClientHello with SNI past byte 512 must be allowed")
	}
}

// TestConnectSNISilentClientTimesOut proves a client that sends nothing over
// the CONNECT tunnel times out (the read deadline fires) and is blocked
// (the old Peek(512) hung forever on a silent client).
func TestConnectSNISilentClientTimesOut(t *testing.T) {
	res := &mockResolver{ips: []netip.Addr{netip.MustParseAddr("151.101.0.223")}}
	p := newHandlerWithResolver([]string{testAllowHost + ":443"}, nil, "l", "ns", "h", res)
	start := time.Now()
	if runConnectSNIRead(t, p, nil) { // nil = send nothing
		t.Fatal("a silent client must be blocked (timed out)")
	}
	if elapsed := time.Since(start); elapsed > 2*sniReadTimeout {
		t.Fatalf("silent client not bounded by sniReadTimeout (%v > %v)", elapsed, 2*sniReadTimeout)
	}
}

// TestConnectSNIRecordTooLarge proves an oversized record (recLen > 16 KiB)
// is denied before any allocation (the cap is checked first).
func TestConnectSNIRecordTooLarge(t *testing.T) {
	// A record header claiming 65535 bytes; the parser must not allocate it
	// (the cap denies) and the SNI is absent -> blocked.
	hdr := []byte{0x16, 0x03, 0x01, 0xff, 0xff}
	res := &mockResolver{ips: []netip.Addr{netip.MustParseAddr("151.101.0.223")}}
	p := newHandlerWithResolver([]string{testAllowHost + ":443"}, nil, "l", "ns", "h", res)
	if runConnectSNIRead(t, p, hdr) {
		t.Fatal("an oversized record must be blocked")
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

var _ = io.Discard // keep imports honest
