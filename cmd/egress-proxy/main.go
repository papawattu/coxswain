// egress-proxy is the I42 egress proxy (ADR-0007 I42 resolution). It is an
// HTTP/HTTPS forward proxy that enforces the effective AgentPolicy network
// allows (the "host:port" pairs from policy.EffectivePolicy.Network) at the
// CONNECT/Host + SNI layer. The enforcement checks live in internal/egress
// (pure, unit-tested); this binary wires them into a forward proxy.
//
// Enforcement (per connection):
//  1. CONNECT host:port (HTTPS) or Host header (plain HTTP) is checked against
//     the allows (host AND port — this retires I41's port loss).
//  2. The host is resolved by the proxy itself (via the pod's DNS); the
//     resolved IP is checked against the carve-outs (SSRF backstop) before the
//     dial, and the dial goes to the SAME resolved IP (no TOCTOU).
//  3. For CONNECT, the SNI from the TLS ClientHello must equal the CONNECT
//     host and be present; absent/disagreeing SNI → tunnel closed.
//  4. Every attempt (allowed and blocked) emits a Q4 audit record on stdout.
//
// It does NOT terminate TLS, inspect the payload, or MITM: after the checks it
// relays bytes. It holds no secrets, no SA token; read-only rootfs, UID 65534.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/papawattu/coxswain/internal/egress"
)

func main() {
	port := envOr("EGRESS_PROXY_PORT", "3128")
	// The effective network allows are passed as EGRESS_POLICY_JSON: a JSON
	// array of "host:port" strings (the same shape as
	// policy.EffectivePolicy.Network). Phase 1 reads it from env; a future
	// version mounts a file.
	allows := readAllows(os.Getenv("EGRESS_POLICY_JSON"))
	policyHash := os.Getenv("EGRESS_POLICY_HASH")
	loopName := os.Getenv("LOOP_NAME")
	namespace := os.Getenv("LOOP_NAMESPACE")
	// Operator-configured pod/service CIDRs (kind/k3s exposes these via
	// --pod-network-cidr / --service-cluster-ip-range). Comma-separated; the
	// proxy carves them out of the dialable range alongside the standard
	// private/link-local ranges.
	var extraCIDRs []string
	if raw := strings.TrimSpace(os.Getenv("POD_CIDR")); raw != "" {
		extraCIDRs = append(extraCIDRs, raw)
	}
	if raw := strings.TrimSpace(os.Getenv("SERVICE_CIDR")); raw != "" {
		extraCIDRs = append(extraCIDRs, raw)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatalf("egress-proxy: listen :%s: %v", port, err)
	}
	log.Printf("egress-proxy: listening on :%s, %d allows, policy=%s\n", port, len(allows), policyHash)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	// A single forward proxy server handles both CONNECT (HTTPS) and plain
	// HTTP. The handler applies the egress checks per request. The resolver
	// is the pod's own net.Resolver (cluster DNS); a seam so unit tests can
	// inject a mock (rebind / NXDOMAIN cases).
	srv := &http.Server{
		Handler:           newHandlerWithResolver(allows, extraCIDRs, loopName, namespace, policyHash, net.DefaultResolver),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Fatalf("egress-proxy: serve: %v", err)
	}
}

// readAllows parses the EGRESS_POLICY_JSON env value (a JSON array of
// "host:port" strings) into the allows slice. A bad/empty value yields an
// empty (default-deny) allow set — fail-closed.
func readAllows(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		// A malformed policy is a misconfiguration; log and fail closed
		// (no allows) rather than crash-loop. The operator validates the
		// effective policy before creating the pod, so this should not fire.
		log.Printf("egress-proxy: bad EGRESS_POLICY_JSON %q: %v (default-deny)", raw, err)
		return nil
	}
	return out
}

// resolver is the DNS seam the proxy resolves CONNECT/Host names through.
// The production binary uses net.DefaultResolver (the pod's cluster DNS); unit
// tests inject a mock to exercise rebind / NXDOMAIN behaviour hermetically.
type resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// h is the forward-proxy handler. It applies the egress checks to each
// connection and relays bytes after the checks pass.
type h struct {
	allows     []string
	extraCIDRs []string
	loopName   string
	namespace  string
	policyHash string
	resolver   resolver
}

func newHandler(allows, extraCIDRs []string, loopName, namespace, policyHash string) *h {
	return newHandlerWithResolver(allows, extraCIDRs, loopName, namespace, policyHash, net.DefaultResolver)
}

// newHandlerWithResolver builds the handler with an explicit resolver. The
// production binary passes net.DefaultResolver (the pod's DNS); unit tests
// pass a mock to exercise rebind / NXDOMAIN behaviour hermetically.
func newHandlerWithResolver(allows, extraCIDRs []string, loopName, namespace, policyHash string, r resolver) *h {
	return &h{
		allows:     allows,
		extraCIDRs: extraCIDRs,
		loopName:   loopName,
		namespace:  namespace,
		policyHash: policyHash,
		resolver:   r,
	}
}

// ServeHTTP handles both CONNECT and plain HTTP. For CONNECT it applies the
// egress checks, opens a tunnel to the resolved IP, and (for TLS) verifies the
// SNI. For plain HTTP it checks the Host header and proxies the request.
func (p *h) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handlePlainHTTP(w, r)
}

// handleConnect implements the HTTPS path: check host:port, resolve + check
// the IP, open a tunnel to that IP, verify the SNI, then relay.
func (p *h) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, portStr, err := net.SplitHostPort(r.Host)
	if err != nil {
		p.audit("connect", r.Host, "blocked", "reason=bad-host")
		http.Error(w, "bad CONNECT host", http.StatusBadGateway)
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		p.audit("connect", r.Host, "blocked", "reason=bad-port")
		http.Error(w, "bad CONNECT port", http.StatusBadGateway)
		return
	}
	if !egress.CheckAllow(p.allows, host, port) {
		p.audit("connect", r.Host, "blocked", "reason=not-allowed, policy="+p.policyHash)
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}
	// Resolve the host ourselves and check the resolved IP (SSRF backstop).
	resolvedIP, ok, resolveErr := p.resolveAndCheck(host)
	if !ok {
		detail := "reason=resolved-ip-rejected"
		if resolveErr != nil {
			// An allowed host that does not resolve (NXDOMAIN) is blocked and
			// audited with dns=nxdomain (ADR failure mode), distinct from an
			// allowlisted name whose resolution lands in a carved-out range.
			detail = "dns=nxdomain"
		}
		p.audit("connect", r.Host, "blocked", detail+", policy="+p.policyHash)
		http.Error(w, "host resolves to a disallowed address", http.StatusForbidden)
		return
	}
	// Open the tunnel: hijack the response and relay to the resolved IP.
	hj, ok := w.(http.Hijacker)
	if !ok {
		p.audit("connect", r.Host, "blocked", "reason=no-hijack")
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		p.audit("connect", r.Host, "blocked", "reason=hijack-failed")
		http.Error(w, "hijack failed", http.StatusInternalServerError)
		return
	}
	target := net.JoinHostPort(resolvedIP.String(), strconv.Itoa(port))
	serverConn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		p.audit("connect", r.Host, "blocked", "reason=dial-failed, ip="+resolvedIP.String())
		_, _ = clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		closeQuietly(clientConn)
		return
	}
	// 200 for the CONNECT so the client starts the TLS handshake over the
	// tunnel.
	_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	// The proxy sees the ClientHello over the tunnel; verify the SNI before
	// relaying the payload.
	peeked, _ := clientBuf.Peek(512)
	if sni, sniOK := egress.ExtractSNI(peeked); sniOK {
		if sni != host {
			p.audit("connect", r.Host, "blocked",
				"sni="+sni+", ip="+resolvedIP.String()+", policy="+p.policyHash)
			closeQuietly(serverConn)
			closeQuietly(clientConn)
			return
		}
	} else {
		// No SNI (or not TLS): the ADR says deny on absent SNI for the
		// TLS path. A non-TLS CONNECT (e.g. a SOCKS-like tunnel) has no
		// SNI to check; the Host check above already applied. Deny here is
		// the fail-closed choice for the TLS case; we cannot reliably tell
		// the two apart, so deny on no-SNI.
		p.audit("connect", r.Host, "blocked", "sni=absent, ip="+resolvedIP.String()+", policy="+p.policyHash)
		closeQuietly(serverConn)
		closeQuietly(clientConn)
		return
	}
	p.audit("connect", r.Host, "allowed",
		"sni="+host+", proto=https, ip="+resolvedIP.String()+", policy="+p.policyHash)
	relay(clientConn, clientBuf, serverConn)
}

// handlePlainHTTP implements the plain-HTTP path: check the Host header and
// proxy the request to the resolved IP.
func (p *h) handlePlainHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	// Determine the host (without port) and the port (default 80) for the
	// allow check.
	hostNoPort := host
	port := 80
	if h, pStr, err := net.SplitHostPort(r.Host); err == nil {
		hostNoPort = h
		if n, err := strconv.Atoi(pStr); err == nil {
			port = n
		}
	}
	if !egress.CheckAllow(p.allows, hostNoPort, port) {
		p.audit("http", r.Host, "blocked", "reason=not-allowed, policy="+p.policyHash)
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}
	resolvedIP, ok, resolveErr := p.resolveAndCheck(hostNoPort)
	if !ok {
		detail := "reason=resolved-ip-rejected"
		if resolveErr != nil {
			detail = "dns=nxdomain"
		}
		p.audit("http", r.Host, "blocked", detail+", policy="+p.policyHash)
		http.Error(w, "host resolves to a disallowed address", http.StatusForbidden)
		return
	}
	p.audit("http", r.Host, "allowed",
		"proto=http, ip="+resolvedIP.String()+", policy="+p.policyHash)
	// Proxy the request to the resolved IP.
	r.URL.Scheme = "http"
	r.URL.Host = net.JoinHostPort(resolvedIP.String(), strconv.Itoa(port))
	proxy := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// addr is already the resolved IP:port; dial it directly (the
			// same IP we checked — no re-resolution).
			return net.DialTimeout(network, addr, 10*time.Second)
		},
	}
	client := &http.Client{Transport: proxy}
	resp, err := client.Do(r)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// resolveAndCheck resolves host and returns (ip, ok, resolveErr). ok is true
// only if the host resolved AND every resolved IP passes the carve-out check.
// The caller dials the SAME IP that is returned (no TOCTOU re-resolution).
// resolveErr is the resolution error (nil on success); the caller uses it to
// record dns=nxdomain in the audit detail (ADR failure mode: an allowed host
// that does not exist is blocked and audited, not silently dropped).
func (p *h) resolveAndCheck(host string) (net.IP, bool, error) {
	// A literal IP (not a hostname) is checked directly.
	if ip := net.ParseIP(host); ip != nil {
		if egress.CheckResolvedIPWithCarveOuts(ip, p.extraCIDRs) {
			return ip, true, nil
		}
		return nil, false, nil
	}
	ips, err := p.resolver.LookupIPAddr(context.Background(), host)
	if err != nil || len(ips) == 0 {
		return nil, false, err
	}
	// The ADR says reject if ANY resolved IP is in a carved-out range, so
	// check all (a name that resolves to a mix of public + private is a
	// rebind / split-horizon attack).
	for _, ip := range ips {
		if !egress.CheckResolvedIPWithCarveOuts(ip.IP, p.extraCIDRs) {
			return nil, false, nil
		}
	}
	return ips[0].IP, true, nil
}

// audit emits a Q4 audit record on stdout. It is best-effort: a failed write
// does not fail the connection (the proxy's job is the tunnel; the audit is
// observability).
func (p *h) audit(action, target, verdict, detail string) {
	rec := egress.AuditRecord{
		Time:      time.Now().UTC().Format(time.RFC3339),
		Loop:      p.loopName,
		Namespace: p.namespace,
		Source:    egress.Source,
		Action:    action,
		Target:    target,
		Verdict:   verdict,
		Detail:    detail,
	}
	data, err := rec.Marshal()
	if err != nil {
		return
	}
	data = append(data, '\n')
	if _, err := os.Stdout.Write(data); err != nil {
		log.Printf("egress-proxy: audit write: %v", err)
	}
}

// closeQuietly closes a conn, ignoring the error (best-effort cleanup after a
// rejection or relay teardown — the conn is going away either way).
func closeQuietly(c net.Conn) {
	_ = c.Close()
}

func relay(client net.Conn, clientBuf *bufio.ReadWriter, server net.Conn) {
	defer closeQuietly(client)
	defer closeQuietly(server)
	// Drain the peeked client bytes (the ClientHello the proxy already saw
	// for the SNI check) into the server so the server sees the full
	// handshake.
	if clientBuf != nil && clientBuf.Reader.Buffered() > 0 {
		buf := make([]byte, clientBuf.Reader.Buffered())
		if _, err := clientBuf.Read(buf); err == nil {
			_, _ = server.Write(buf)
		}
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(server, client)
		done <- struct{}{}
		closeQuietly(server)
	}()
	go func() {
		_, _ = io.Copy(client, server)
		done <- struct{}{}
		closeQuietly(client)
	}()
	<-done
}

// copyHeader copies response headers (a small subset; enough for the agent's
// HTTP clients).
func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// envOr returns the env value or the default.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
