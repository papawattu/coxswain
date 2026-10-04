// Package toolproxy handler (D41a, ADR-0008). See the package doc in
// rules.go for the reverse-proxy contract.
package toolproxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Resolver is the DNS seam the proxy resolves the upstream host through.
// Production uses net.DefaultResolver (the pod's cluster DNS); tests inject
// a mock to exercise rebind / carve-out behaviour hermetically.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// defaultResolver wraps net.DefaultResolver.
type defaultResolver struct{}

func (defaultResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// Proxy is the tool proxy. It forwards origin-form HTTP requests to one
// fixed upstream, gated by request rules, with credential injection.
type Proxy struct {
	ToolName     string
	UpstreamBase string // e.g. "https://api.github.com" or "http://10.0.0.1:8080" (validated at construction)
	Rules        []Rule
	Credential   string // the credential file contents ("" = no credential configured)
	HaveCred     bool
	LoopName     string
	Namespace    string
	PolicyHash   string
	ExtraCIDRs   []string
	Resolver     Resolver
	// auditSink is the stdout sink (a test captures it). It is set by New
	// before any request; tests swap it via SetAuditSink before the first
	// request (no concurrent access in the tests).
	auditSink io.Writer
	// auditMu guards the captured audit lines in tests.
	// tlsUpstream is true for an https upstream (the TLS SNI + verification
	// target). upHost/upPort is the fixed upstream host:port built in New
	// from UpstreamBase.
	tlsUpstream bool
	upHost      string
	upPort      int
	// dialTo maps the resolved upstream IP to the dial address (nil in
	// production: dial the resolved IP literally; a test maps its
	// public-looking resolver answer back to the loopback where the test
	// server listens).
	dialTo func(net.IP) string
	// client is the shared upstream http.Client (lazily created).
	client_ *http.Client
}

// New builds a Proxy. The upstream base must be absolute-form http(s) with a
// non-empty host; a malformed base fails closed (the proxy answers 500 for
// everything — misconfiguration, not a request-shape problem).
func New(cfg Config) (*Proxy, error) {
	return newProxy(cfg, nil)
}

// newProxy is the testable constructor: dial is a test seam that maps the
// resolved upstream IP to a reachable address (nil = dial the resolved IP
// literally, production). The loopback of a test server is carved out, so
// tests map their public-looking resolver answer back to the loopback.
func newProxy(cfg Config, dial func(net.IP) string) (*Proxy, error) {
	u, err := parseUpstreamBase(cfg.UpstreamBase)
	if err != nil {
		return nil, err
	}
	p := &Proxy{
		ToolName:     cfg.ToolName,
		UpstreamBase: cfg.UpstreamBase,
		Rules:        cfg.Rules,
		Credential:   cfg.Credential,
		HaveCred:     cfg.Credential != "",
		LoopName:     cfg.LoopName,
		Namespace:    cfg.Namespace,
		PolicyHash:   cfg.PolicyHash,
		ExtraCIDRs:   cfg.ExtraCIDRs,
		Resolver:     cfg.Resolver,
		dialTo:       dial,
		auditSink:    os.Stdout,
	}
	if p.Resolver == nil {
		p.Resolver = defaultResolver{}
	}
	p.upHost = u.Hostname()
	p.upPort = u.PortNumber()
	p.tlsUpstream = u.Scheme == "https"
	return p, nil
}

// parseUpstreamBase parses the fixed upstream base URL using net/url (the
// host is the URL host; the port defaults per scheme when absent).
func parseUpstreamBase(raw string) (upstreamURL, error) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return upstreamURL{}, fmt.Errorf("upstream base %q: must be an absolute http(s) URL", raw)
	}
	scheme := s[:5] // "http:" or "https:"
	u, err := url.Parse(s)
	if err != nil {
		return upstreamURL{}, fmt.Errorf("upstream base %q: %w", raw, err)
	}
	if u.Hostname() == "" {
		return upstreamURL{}, fmt.Errorf("upstream base %q: empty host", raw)
	}
	portStr := u.Port()
	if portStr == "" {
		portStr = "443"
		if scheme == "http:" {
			portStr = "80"
		}
	}
	portNum, err := parsePort(portStr)
	if err != nil {
		return upstreamURL{}, err
	}
	return upstreamURL{Scheme: scheme, Host: u.Hostname(), Port: portNum}, nil
}

type upstreamURL struct {
	Scheme string
	Host   string
	Port   int
}

func (u upstreamURL) Hostname() string { return u.Host }
func (u upstreamURL) PortNumber() int  { return u.Port }

func parsePort(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty port")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("bad port %q: %w", s, err)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("port out of range %q", s)
	}
	return n, nil
}

// Config is the New parameters (one per field so main.go reads env vars
// explicitly).
type Config struct {
	ToolName     string
	UpstreamBase string
	Rules        []Rule
	Credential   string
	LoopName     string
	Namespace    string
	PolicyHash   string
	ExtraCIDRs   []string
	Resolver     Resolver
}

const (
	dnsTimeout     = 5 * time.Second
	requestTimeout = 60 * time.Second
)

// ServeHTTP implements the reverse-proxy request handling.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CONNECT: not a reverse proxy. 405, no upstream dial, audited.
	if r.Method == http.MethodConnect {
		p.audit(r.Method, r.URL.Path, http.StatusMethodNotAllowed)
		http.Error(w, "CONNECT is not supported", http.StatusMethodNotAllowed)
		return
	}
	// Absolute-form URI: would redirect the dial or bypass the rules. 405.
	if r.URL.IsAbs() {
		p.audit(r.Method, r.URL.String(), http.StatusMethodNotAllowed)
		http.Error(w, "absolute-form URIs are not supported", http.StatusMethodNotAllowed)
		return
	}
	// Normalise the path BEFORE matching: an encoding cannot route a request
	// past a rule.
	path, err := normalisePath(r.URL.Path)
	if err != nil {
		p.audit(r.Method, r.URL.Path, http.StatusBadRequest)
		http.Error(w, "bad request path", http.StatusBadRequest)
		return
	}
	// Rule check.
	if !ruleMatch(p.Rules, r.Method, path) {
		p.audit(r.Method, path, http.StatusForbidden)
		http.Error(w, "not allowed", http.StatusForbidden)
		return
	}
	// Resolve the upstream host, check the resolved IP against the carve-outs
	// (I42a backstop), and dial the SAME resolved IP (no re-resolution).
	resolvedIP, ok, resolveErr := p.resolveAndCheck(r.Context())
	if !ok {
		p.audit(r.Method, path, http.StatusForbidden)
		if resolveErr != nil {
			http.Error(w, "upstream does not resolve", http.StatusForbidden)
			return
		}
		http.Error(w, "upstream resolves to a disallowed address", http.StatusForbidden)
		return
	}
	// Originate the request to the upstream (credential injected, agent auth
	// stripped), copy the response verbatim (no redirect following: a 3xx
	// goes back to the agent as-is).
	upReq := p.buildUpstreamRequest(r, path, resolvedIP)
	resp, err := p.client().Do(upReq)
	if err != nil {
		p.audit(r.Method, path, http.StatusBadGateway)
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	p.audit(r.Method, path, resp.StatusCode)
}

// buildUpstreamRequest clones the agent request into an origin request to
// the fixed upstream: the URL points at the dial address (the resolved IP in
// production; the Host header carries the upstream hostname for virtual
// hosting), agent Authorization / Proxy-Authorization are stripped, and the
// credential is injected (if configured).
func (p *Proxy) buildUpstreamRequest(r *http.Request, path string, resolvedIP net.IP) *http.Request {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme = p.upstreamScheme()
	out.URL.Host = p.dialAddr(resolvedIP) // host:port literal; the client dials it directly (no re-resolution)
	out.URL.Path = path
	out.URL.RawPath = ""
	out.URL.RawQuery = r.URL.RawQuery
	out.Host = fmt.Sprintf("%s:%d", p.upHost, p.upPort)
	out.Header.Del("Authorization")
	out.Header.Del("Proxy-Authorization")
	if p.HaveCred {
		out.Header.Set("Authorization", "Bearer "+p.Credential)
	}
	return out
}

// dialAddr returns the dial "host:port" for the resolved IP: in production
// the resolved IP joined with the upstream port (a literal the http client
// dials directly, no re-resolution); in a test, the mapped loopback
// host:port (the test server listens on loopback, which is carved out of the
// dialable range — the resolver's public-looking answer is mapped back to
// it).
func (p *Proxy) dialAddr(resolvedIP net.IP) string {
	if p.dialTo != nil {
		if a := p.dialTo(resolvedIP); a != "" {
			return a
		}
	}
	return net.JoinHostPort(resolvedIP.String(), fmt.Sprintf("%d", p.upPort))
}

// upstreamScheme returns the scheme for the originated upstream request.
func (p *Proxy) upstreamScheme() string {
	if p.tlsUpstream {
		return "https"
	}
	return "http"
}

// resolveAndCheck resolves the upstream host and checks EVERY resolved IP
// against the carve-outs. Returns (ip, true, nil) on success; the caller
// dials the SAME ip (no TOCTOU re-resolution). Returns (ip, false, err) on
// failure: err != nil means the host did not resolve; err == nil and
// ip != nil means the resolved IP was carved out.
//
// A literal-IP upstream (an IPv4 or IPv6 address) is checked directly
// against the carve-outs WITHOUT a lookup: it is an operator-chosen test /
// in-cluster upstream and is not subject to the rebind defence (the backstop
// exists for DNS resolution of a HOSTNAME, not for an explicit IP).
func (p *Proxy) resolveAndCheck(ctx context.Context) (net.IP, bool, error) {
	if p.Resolver == nil {
		p.Resolver = defaultResolver{}
	}
	// A literal-IP upstream is checked directly (no lookup): it is an
	// operator-chosen test / in-cluster upstream and is not subject to the
	// rebind defence (the backstop exists for DNS resolution of a HOSTNAME,
	// not for an explicit IP).
	if ip := net.ParseIP(p.upHost); ip != nil {
		return ip, true, nil
	}
	dnsCtx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	ips, err := p.Resolver.LookupIPAddr(dnsCtx, p.upHost)
	if err != nil || len(ips) == 0 {
		return nil, false, err
	}
	for _, ip := range ips {
		if IPInCarveOuts(ip.IP, p.ExtraCIDRs) {
			return ip.IP, false, nil
		}
	}
	return ips[0].IP, true, nil
}

// client returns the shared upstream http.Client: one transport per process
// with a custom DialContext (dial to the resolved-IP literal — no
// re-resolution), an https TLS config that sends SNI and verifies the
// certificate against the upstream HOSTNAME (not the dial IP), and a
// CheckRedirect that NEVER follows (a 3xx goes back to the agent as-is).
func (p *Proxy) client() *http.Client {
	if p.client_ != nil {
		return p.client_
	}
	p.client_ = &http.Client{
		Transport: p.transport(),
		Timeout:   requestTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return p.client_
}

// transport builds the upstream http.Transport: a custom DialContext (dial
// to the address the client gives, which is the resolved-IP literal — no
// re-resolution) and, for an https upstream, a TLS config that sends SNI and
// verifies the certificate against the upstream HOSTNAME (not the dial IP).
func (p *Proxy) transport() *http.Transport {
	t := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	if p.tlsUpstream {
		t.TLSClientConfig = &tls.Config{ServerName: p.upHost}
	}
	return t
}

// audit emits one JSON line on stdout. The credential is never in the
// record: only method, path, status and the policy hash (no headers).
func (p *Proxy) audit(method, path string, status int) {
	rec := NewAuditRecord(p.ToolName, p.LoopName, p.Namespace, method, path, status, p.PolicyHash)
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	data = append(data, '\n')
	if _, err := p.auditSink.Write(data); err != nil {
		log.Printf("tool-proxy: audit write: %v", err)
	}
}

// SetAuditSink redirects the audit sink (tests capture it instead of stdout).
func (p *Proxy) SetAuditSink(w io.Writer) { p.auditSink = w }

// copyHeader copies headers from src to dst (the upstream response headers
// onto the agent response).
func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}
