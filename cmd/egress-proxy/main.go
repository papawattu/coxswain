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
	// HTTP. The handler applies the egress checks per request.
	srv := &http.Server{
		Handler:           newHandler(allows, extraCIDRs, loopName, namespace, policyHash),
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

// h is the forward-proxy handler. It applies the egress checks to each
// connection and relays bytes after the checks pass.
type h struct {
	allows     []string
	extraCIDRs []string
	loopName   string
	namespace  string
	policyHash string
}

func newHandler(allows, extraCIDRs []string, loopName, namespace, policyHash string) *h {
	return &h{
		allows:     allows,
		extraCIDRs: extraCIDRs,
		loopName:   loopName,
		namespace:  namespace,
		policyHash: policyHash,
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
		p.audit(r, "connect", r.Host, "blocked", "reason=bad-host")
		http.Error(w, "bad CONNECT host", http.StatusBadGateway)
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		p.audit(r, "connect", r.Host, "blocked", "reason=bad-port")
		http.Error(w, "bad CONNECT port", http.StatusBadGateway)
		return
	}
	if !egress.CheckAllow(p.allows, host, port) {
		p.audit(r, "connect", r.Host, "blocked", "reason=not-allowed, policy="+p.policyHash)
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}
	// Resolve the host ourselves and check the resolved IP (SSRF backstop).
	resolvedIP, ok := p.resolveAndCheck(host)
	if !ok {
		p.audit(r, "connect", r.Host, "blocked", "reason=resolved-ip-rejected, policy="+p.policyHash)
		http.Error(w, "host resolves to a disallowed address", http.StatusForbidden)
		return
	}
	// Open the tunnel: hijack the response and relay to the resolved IP.
	hj, ok := w.(http.Hijacker)
	if !ok {
		p.audit(r, "connect", r.Host, "blocked", "reason=no-hijack")
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		p.audit(r, "connect", r.Host, "blocked", "reason=hijack-failed")
		http.Error(w, "hijack failed", http.StatusInternalServerError)
		return
	}
	target := net.JoinHostPort(resolvedIP.String(), strconv.Itoa(port))
	serverConn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		p.audit(r, "connect", r.Host, "blocked", "reason=dial-failed, ip="+resolvedIP.String())
		_, _ = clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		clientConn.Close()
		serverConn = nil
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
			p.audit(r, "connect", r.Host, "blocked",
				"sni="+sni+", ip="+resolvedIP.String()+", policy="+p.policyHash)
			serverConn.Close()
			clientConn.Close()
			return
		}
	} else {
		// No SNI (or not TLS): the ADR says deny on absent SNI for the
		// TLS path. A non-TLS CONNECT (e.g. a SOCKS-like tunnel) has no
		// SNI to check; the Host check above already applied. Deny here is
		// the fail-closed choice for the TLS case; we cannot reliably tell
		// the two apart, so deny on no-SNI.
		p.audit(r, "connect", r.Host, "blocked", "sni=absent, ip="+resolvedIP.String()+", policy="+p.policyHash)
		serverConn.Close()
		clientConn.Close()
		return
	}
	p.audit(r, "connect", r.Host, "allowed",
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
		p.audit(r, "http", r.Host, "blocked", "reason=not-allowed, policy="+p.policyHash)
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}
	resolvedIP, ok := p.resolveAndCheck(hostNoPort)
	if !ok {
		p.audit(r, "http", r.Host, "blocked", "reason=resolved-ip-rejected, policy="+p.policyHash)
		http.Error(w, "host resolves to a disallowed address", http.StatusForbidden)
		return
	}
	p.audit(r, "http", r.Host, "allowed",
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
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// resolveAndCheck resolves host and returns the first resolved IP that passes
// the carve-out check. It returns (nil, false) if the host does not resolve or
// every resolved IP is in a carved-out range. The caller dials the SAME IP
// that is returned (no TOCTOU re-resolution).
func (p *h) resolveAndCheck(host string) (net.IP, bool) {
	// A literal IP (not a hostname) is checked directly.
	if ip := net.ParseIP(host); ip != nil {
		if egress.CheckResolvedIPWithCarveOuts(ip, p.extraCIDRs) {
			return ip, true
		}
		return nil, false
	}
	ips, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil || len(ips) == 0 {
		return nil, false
	}
	// Prefer the first dialable IP; if the first is rejected but a later one
	// is not, reject (fail-closed: a name that resolves to a mix is
	// suspicious). Actually the ADR says reject if ANY resolved IP is in a
	// carved-out range, so check all.
	for _, ip := range ips {
		if !egress.CheckResolvedIPWithCarveOuts(ip.IP, p.extraCIDRs) {
			return nil, false
		}
	}
	return ips[0].IP, true
}

// audit emits a Q4 audit record on stdout. It is best-effort: a failed write
// does not fail the connection (the proxy's job is the tunnel; the audit is
// observability).
func (p *h) audit(r *http.Request, action, target, verdict, detail string) {
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
func relay(client net.Conn, clientBuf *bufio.ReadWriter, server net.Conn) {
	defer client.Close()
	defer server.Close()
	// Drain the peeked client bytes (the ClientHello the proxy already saw
	// for the SNI check) into the server so the server sees the full
	// handshake.
	if clientBuf != nil && clientBuf.Reader.Buffered() > 0 {
		buf := make([]byte, clientBuf.Reader.Buffered())
		if _, err := clientBuf.Reader.Read(buf); err == nil {
			_, _ = server.Write(buf)
		}
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(server, client)
		done <- struct{}{}
		_ = server.Close()
	}()
	go func() {
		_, _ = io.Copy(client, server)
		done <- struct{}{}
		_ = client.Close()
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
