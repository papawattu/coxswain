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
	"net/textproto"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
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
	// dial is the upstream dial seam (nil in production: dial the
	// checked IP:port directly; a test swaps it for a fake-upstream dialer).
	dialer func(ctx context.Context, network, addr string) (net.Conn, error)
	// transport is the shared upstream Transport (lazily created by
	// transport()).
	transport_ *http.Transport
	// relayIdle is the per-Read idle timeout for the relay (relayIdleTimeout
	// in production; a test swaps it for a short value to exercise teardown).
	relayIdle time.Duration
}

// sniReadTimeout bounds the read of the TLS ClientHello record over a
// CONNECT tunnel. A silent client must time out (audited as blocked), not
// pin the relay goroutine; 10s is generous for a local client.
const sniReadTimeout = 10 * time.Second

// dnsTimeout bounds a DNS lookup so a slow resolver cannot pin handlers.
const dnsTimeout = 5 * time.Second

// relayIdleTimeout bounds an idle CONNECT tunnel: after the SNI is verified
// the tunnel is pure byte relay, and a tunnel with no traffic for this long
// is torn down (the client reconnects; a fresh CONNECT re-runs the checks).
const relayIdleTimeout = 5 * time.Minute

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
		relayIdle:  relayIdleTimeout,
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
	// Resolve the host ourselves and check the resolved IP (SSRF backstop),
	// with a bounded lookup (a slow resolver must not pin the handler).
	resolvedIP, ok, resolveErr := p.resolveAndCheck(r.Context(), host)
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
	// relaying the payload. Read the ClientHello record explicitly (not a
	// fixed-size Peek, which blocks on short hellos and misses the SNI on
	// long ones): set a read deadline, read the 5-byte TLS record header,
	// reject a non-handshake type, cap the record length, io.ReadFull the
	// record, then parse the SNI. On success the deadline is cleared and
	// header + record + the rest are relayed. A client that sends nothing
	// times out (deadline) and is audited as blocked.
	var peeked []byte
	var sni string
	sniOK := false
	if err := clientConn.SetReadDeadline(time.Now().Add(sniReadTimeout)); err != nil {
		p.audit("connect", r.Host, "blocked", "reason=read-deadline-failed, ip="+resolvedIP.String()+", policy="+p.policyHash)
		closeQuietly(serverConn)
		closeQuietly(clientConn)
		return
	}
	var hdr [5]byte
	if _, err := io.ReadFull(clientBuf, hdr[:]); err == nil && hdr[0] == 0x16 {
		// Handshake record. Cap recLen (a ClientHello is far smaller than
		// 16 KiB; anything larger is not a ClientHello and is denied) before
		// allocating.
		recLen := int(hdr[3])<<8 | int(hdr[4])
		if recLen > 0 && recLen <= 16*1024 {
			record := make([]byte, recLen)
			if _, err := io.ReadFull(clientBuf, record); err == nil {
				peeked = append(hdr[:], record...)
				sni, sniOK = egress.ExtractSNI(peeked)
			}
		}
	}
	_ = clientConn.SetReadDeadline(time.Time{}) // clear: the relay reads with no deadline
	if !sniOK {
		// The SNI could not be verified: a silent client (deadline timeout),
		// a non-TLS CONNECT (no 0x16 record type), a truncated/oversized
		// record, or a ClientHello without server_name. Deny is the
		// fail-closed choice (ADR-0007 I42 resolution). sni=absent-in-hello
		// marks the last case (a well-formed hello, no server_name); a
		// silent client or non-TLS bytes are audited as sni=absent.
		detail := "sni=absent, ip=" + resolvedIP.String() + ", policy=" + p.policyHash
		if len(peeked) >= 5 && peeked[0] == 0x16 {
			detail = "sni=absent-in-hello, ip=" + resolvedIP.String() + ", policy=" + p.policyHash
		}
		p.audit("connect", r.Host, "blocked", detail)
		closeQuietly(serverConn)
		closeQuietly(clientConn)
		return
	}
	if sni != host {
		p.audit("connect", r.Host, "blocked",
			"sni="+sni+", ip="+resolvedIP.String()+", policy="+p.policyHash)
		closeQuietly(serverConn)
		closeQuietly(clientConn)
		return
	}
	p.audit("connect", r.Host, "allowed",
		"sni="+host+", proto=https, ip="+resolvedIP.String()+", policy="+p.policyHash)
	relay(clientConn, clientBuf, serverConn, peeked, p.relayIdle)
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
	resolvedIP, ok, resolveErr := p.resolveAndCheck(r.Context(), hostNoPort)
	if !ok {
		detail := "reason=resolved-ip-rejected"
		if resolveErr != nil {
			detail = "dns=nxdomain"
		}
		p.audit("http", r.Host, "blocked", detail+", policy="+p.policyHash)
		http.Error(w, "host resolves to a disallowed address", http.StatusForbidden)
		return
	}
	if err := p.proxyRequest(w, r, resolvedIP, port); err != nil {
		// The upstream failed: the attempt did not succeed end-to-end. The
		// audit records it as blocked (reason=upstream-error) rather than
		// claimed allowed (the allowed audit is emitted only after the
		// upstream responds).
		p.audit("http", r.Host, "blocked", "reason=upstream-error, policy="+p.policyHash)
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	p.audit("http", r.Host, "allowed",
		"proto=http, ip="+resolvedIP.String()+", policy="+p.policyHash)
}

// proxyRequest forwards a plain-HTTP request to the already-checked resolved
// IP and copies the response to w. It returns the upstream error (nil on
// success). The request is cloned and normalised into a client request:
// RequestURI is cleared (a server request must not be re-sent as a client
// request), hop-by-hop and Proxy-* headers are stripped (they terminate at
// the proxy; forwarding Proxy-Authorization would leak it upstream), and
// Host is kept so the upstream virtual host matches.
func (p *h) proxyRequest(w http.ResponseWriter, r *http.Request, resolvedIP net.IP, port int) error {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme = "http"
	out.URL.Host = net.JoinHostPort(resolvedIP.String(), strconv.Itoa(port))
	out.Header.Del("Proxy-Authorization")
	out.Header.Del("Proxy-Connection")
	// Hop-by-hop headers (RFC 9110 §7.6.1) are not end-to-end; "Connection"
	// plus the header names it lists are stripped too.
	if connList := out.Header.Values("Connection"); len(connList) > 0 {
		for _, c := range connList {
			for h := range strings.SplitSeq(c, ",") {
				out.Header.Del(textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(h)))
			}
		}
		out.Header.Del("Connection")
	}
	// The target is already the checked IP:port; the dial goes there via the
	// shared transport's dialer (no re-resolution, no re-lookup).
	client := &http.Client{
		Transport: p.transport(),
		Timeout:   60 * time.Second,
	}
	resp, err := client.Do(out)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return nil
}

// transport returns the shared upstream Transport: one pool for the process
// (instead of allocating a Transport per request) with bounded timeouts —
// ResponseHeaderTimeout caps the wait for upstream headers, the Client
// Timeout caps the whole exchange, and the dial goes through the handler's
// dial seam.
func (p *h) transport() *http.Transport {
	if p.transport_ != nil {
		return p.transport_
	}
	t := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return p.dial(ctx, network, addr)
		},
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	p.transport_ = t
	return t
}

// dial is the upstream dial seam. Production dials the already-checked
// IP:port directly (bounded timeout; the address is a literal IP, so no
// re-resolution). Tests swap p.dialer for a dialer that serves a fake
// upstream from a public-looking address (127/8 is carved out of the
// dialable range).
func (p *h) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.dialer != nil {
		return p.dialer(ctx, network, addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// resolveAndCheck resolves host and returns (ip, ok, resolveErr). ok is true
// only if the host resolved AND every resolved IP passes the carve-out check.
// The caller dials the SAME IP that is returned (no TOCTOU re-resolution).
// resolveErr is the resolution error (nil on success); the caller uses it to
// record dns=nxdomain in the audit detail (ADR failure mode: an allowed host
// that does not exist is blocked and audited, not silently dropped).
func (p *h) resolveAndCheck(ctx context.Context, host string) (net.IP, bool, error) {
	// A literal IP (not a hostname) is checked directly.
	if ip := net.ParseIP(host); ip != nil {
		if egress.CheckResolvedIPWithCarveOuts(ip, p.extraCIDRs) {
			return ip, true, nil
		}
		return nil, false, nil
	}
	dnsCtx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	ips, err := p.resolver.LookupIPAddr(dnsCtx, host)
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

// relayConn is a net.Conn whose read deadline is refreshed before every Read
// from a SHARED last-activity timestamp. The deadline is set to
// lastActivity+idle, so a Read on EITHER direction that sees bytes pushes the
// deadline out for both: idle means no bytes in either direction, not no bytes
// in one direction. This is what keeps a long download alive — during a
// download the client->server side is silent, but the server->client side is
// active and keeps pushing the deadline, so the silent side is never cut mid-
// transfer. A deadline that fires while the other side was recently active is
// extended and retried (the other side's bytes will arrive and push the
// deadline); a deadline that fires with no activity in either direction is
// treated as a real idle timeout and the relay tears down.
type relayConn struct {
	net.Conn
	idle time.Duration
	last *atomic.Int64
}

// Read refreshes the shared deadline and reads, retrying on a deadline error
// if the other direction was recently active (extending the deadline so the
// in-flight bytes can arrive). Returns the first non-deadline error.
func (r *relayConn) Read(p []byte) (int, error) {
	for {
		lastNano := r.last.Load()
		if err := r.Conn.SetReadDeadline(time.Unix(0, lastNano).Add(r.idle)); err != nil {
			return 0, err
		}
		// The embedded conn's Read is called explicitly (not the promoted
		// r.Read, which would recurse); see the QF1008 exclusion in
		// .golangci.yml.
		n, err := r.Conn.Read(p) //nolint:staticcheck
		if err == nil {
			if n > 0 {
				r.last.Store(time.Now().UnixNano())
			}
			return n, nil
		}
		// A deadline error: if the OTHER direction saw bytes since the deadline
		// we set (lastNano advanced past it), the tunnel is active — extend the
		// deadline and retry instead of tearing down. Otherwise no activity in
		// either direction; return the error so the relay tears down.
		if errors.Is(err, os.ErrDeadlineExceeded) && r.last.Load() > lastNano {
			continue
		}
		return n, err
	}
}

// relay copies bytes between client and server until one direction ends. Idle
// (no bytes in EITHER direction) for idle tears the tunnel down; an active
// tunnel (bytes in either direction) runs until a real end. On a one-sided EOF
// (a side closed its write half) the relay half-closes (CloseWrite) the other
// direction so the remaining in-flight bytes drain, then tears down — instead
// of closing both conns mid-transfer. clientHello and any bytes buffered in
// clientBuf after the ClientHello are forwarded to the server first.
func relay(client net.Conn, clientBuf *bufio.ReadWriter, server net.Conn, clientHello []byte, idle time.Duration) {
	defer closeQuietly(client)
	defer closeQuietly(server)
	// The ClientHello record (header + record) was read from the client for
	// the SNI check. It was consumed out of the buffered reader; write it to
	// the server first to restore the full handshake stream.
	if len(clientHello) > 0 {
		_, _ = server.Write(clientHello)
	}
	// A bufio fill reads up to 4 KiB from the socket, so bytes the client sent
	// right after the ClientHello (0-RTT early data, a pipelined record,
	// anything in the same segment) sit in clientBuf. Forward them to the
	// server BEFORE starting the copy, then copy from the buffered reader (not
	// the raw conn) so no client bytes are skipped.
	// (clientBuf.Reader.Buffered/Peek are called via the explicit .Reader. to
	// avoid an ambiguous-selector error; see the QF1008 exclusion in
	// .golangci.yml.)
	if n := clientBuf.Reader.Buffered(); n > 0 {
		if buffered, _ := clientBuf.Reader.Peek(n); len(buffered) > 0 {
			_, _ = server.Write(buffered)
			_, _ = clientBuf.Discard(n)
		}
	}
	// Both directions share one last-activity timestamp so idle means no bytes
	// in either direction.
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	clientRC := &relayConn{Conn: client, idle: idle, last: &last}
	serverRC := &relayConn{Conn: server, idle: idle, last: &last}
	// copyDone signals that one direction's copy finished (a real EOF/error or
	// an idle teardown). It carries whether the finish was a one-sided EOF
	// (half-close) vs an idle timeout (tear down both).
	type copyResult struct {
		done bool
		half bool // true: one-sided EOF (CloseWrite the other); false: idle/teardown
	}
	resC, resS := make(chan copyResult, 1), make(chan copyResult, 1)
	go func() {
		_, err := io.Copy(server, clientRC) // client -> server
		if err == nil {
			// The client closed its write half (EOF): half-close the server's
			// read half so the server sees EOF and can finish sending (e.g. a
			// download that completes). The server->client copy keeps draining
			// the in-flight bytes until the peer closes; we wait for it (resS) at
			// the end so the transfer completes before the deferred closes.
			if tc, ok := server.(interface{ CloseWrite() error }); ok {
				_ = tc.CloseWrite()
			}
			resC <- copyResult{done: true, half: true}
			return
		}
		// Error (idle timeout or other): tear down BOTH conns so the other copy
		// returns (its Read errors on the closed conn) and signals resS.
		resC <- copyResult{done: true}
		closeQuietly(server)
		closeQuietly(client)
	}()
	go func() {
		_, err := io.Copy(client, serverRC) // server -> client
		if err == nil {
			// The server closed its write half (EOF): half-close the client's
			// read half so the client sees EOF. The client->server copy keeps
			// draining until the client closes; we wait for it (resC) at the end.
			if tc, ok := client.(interface{ CloseWrite() error }); ok {
				_ = tc.CloseWrite()
			}
			resS <- copyResult{done: true, half: true}
			return
		}
		// Error (idle timeout or other): tear down BOTH conns so the other copy
		// returns and signals resC.
		resS <- copyResult{done: true}
		closeQuietly(server)
		closeQuietly(client)
	}()
	// Wait for both directions to finish.
	//
	//   - Idle timeout: the finishing copy's goroutine already closed the
	//     conns (tear-down); the other copy returns on that close and signals,
	//     so both channels close promptly and relay returns — the deferred
	//     closes are no-ops.
	//   - One-sided EOF: the finishing copy half-closed its end (CloseWrite)
	//     and is waiting. The other copy keeps draining the in-flight bytes and
	//     finishes when the peer sees the EOF and closes; waiting for it here
	//     lets the transfer complete before the deferred closes fire (which are
	//     then no-ops on already-closed conns).
	<-resC
	<-resS
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
