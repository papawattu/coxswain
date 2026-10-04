package toolproxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test plumbing.
// ---------------------------------------------------------------------------

// mockResolver is the DNS seam: fixed answers + a lookup counter (the
// no-re-resolution assertion).
type mockResolver struct {
	mu    sync.Mutex
	hosts map[string][]string
	errs  map[string]error
	calls map[string]int
}

func (m *mockResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	return m.lookup(host)
}

// lookup is the unexported core of LookupIPAddr (called directly so the
// resolver can be used without a context in helper code).
func (m *mockResolver) lookup(host string) ([]net.IPAddr, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls[host]++
	if err, ok := m.errs[host]; ok {
		return nil, err
	}
	var out []net.IPAddr
	for _, s := range m.hosts[host] {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out, nil
}

func (m *mockResolver) count(host string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[host]
}

// requestLog records upstream requests.
type requestLog struct {
	mu      sync.Mutex
	entries []logEntry
}

type logEntry struct {
	method  string
	path    string
	host    string
	headers http.Header
}

func (l *requestLog) append(e logEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
}

func (l *requestLog) entriesList() []logEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]logEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

func (l *requestLog) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// newTestServer builds an httptest server that records requests and answers
// with the given status + optional Location.
func newTestServer(t *testing.T, log *requestLog, status int, location string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := http.Header{}
		for k := range r.Header {
			for _, v := range r.Header[k] {
				h.Add(k, v)
			}
		}
		log.append(logEntry{method: r.Method, path: r.URL.Path, host: r.Host, headers: h})
		if location != "" {
			w.Header().Set("Location", location)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte("body"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTLSTestServer builds an httptest TLS server whose certificate is for
// the given hostname. Returns the server and a *x509.CertPool trusting the
// cert (the test's RootCAs).
func newTLSTestServer(t *testing.T, log *requestLog, certHost string) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: certHost},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{certHost},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	ca := x509.NewCertPool()
	ca.AppendCertsFromPEM(certPEM)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := http.Header{}
		for k := range r.Header {
			for _, v := range r.Header[k] {
				h.Add(k, v)
			}
		}
		log.append(logEntry{method: r.Method, path: r.URL.Path, host: r.Host, headers: h})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("tls-ok"))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, ca
}

// loopbackPort returns the port a loopback test server listens on (the host
// is always 127.0.0.1; the dial seam returns "127.0.0.1:port").
func loopbackPort(t *testing.T, urlStr string) string {
	t.Helper()
	u, err := url.Parse(urlStr)
	if err != nil {
		t.Fatalf("parse %q: %v", urlStr, err)
	}
	if u.Host == "" {
		t.Fatalf("test server %q has no host", urlStr)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("SplitHostPort %q: %v", u.Host, err)
	}
	if host != "127.0.0.1" && host != "::1" {
		t.Fatalf("test server %q is not on loopback (host %q)", urlStr, host)
	}
	return port
}

// dialToPublic maps the resolver's public answer back to the loopback
// address the test server listens on (empty port → the public IP itself).
func dialToPublic(port string) func(net.IP) string {
	return func(ip net.IP) string {
		if ip.String() == publicIP {
			if port == "" {
				return publicIP
			}
			return "127.0.0.1:" + port
		}
		return ""
	}
}

// publicIP is a TEST-NET-2 address that is NOT in the carve-out set: the
// resolver answers with it, and the dial seam maps it to the loopback where
// the test server listens (mirroring cmd/egress-proxy's main_test, which
// serves a fake upstream from a public-looking address because 127/8 is
// carved out of the dialable range).
const publicIP = "198.19.0.1"

func hostOf(t *testing.T, urlStr string) string {
	t.Helper()
	u, err := url.Parse(urlStr)
	if err != nil {
		t.Fatalf("parse %q: %v", urlStr, err)
	}
	return u.Hostname()
}

func rulesGET(prefix string) []Rule {
	return []Rule{{Methods: []string{"GET"}, Paths: []string{prefix}}}
}

// testProxy builds a proxy pointed at the given upstream (loopback test
// server) with the credential + audit sink. The resolver answers publicIP
// for the upstream host (carve-out clean) and the dial seam maps it to the
// loopback.
func testProxy(t *testing.T, upstream *httptest.Server, rules []Rule, cred string, resolver *mockResolver, sink *stringsBuilder) *Proxy {
	t.Helper()
	lp := loopbackHostPort(t, upstream.URL)
	host := hostOf(t, upstream.URL)
	if resolver == nil {
		resolver = &mockResolver{hosts: map[string][]string{}, calls: map[string]int{}}
	}
	if resolver.hosts == nil {
		resolver.hosts = map[string][]string{}
	}
	if resolver.calls == nil {
		resolver.calls = map[string]int{}
	}
	resolver.hosts[host] = []string{publicIP}
	p, err := newProxy(Config{
		ToolName:     "tool-x",
		UpstreamBase: upstream.URL,
		Rules:        rules,
		Credential:   cred,
		LoopName:     "loop-x",
		Namespace:    "ns-x",
		PolicyHash:   "pol-abc",
		Resolver:     resolver,
	}, dialToPublic(lp))
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	p.SetAuditSink(sink)
	return p
}

// stringsBuilder is the captured-audit sink.
type stringsBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *stringsBuilder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *stringsBuilder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// auditLines parses the captured audit JSON lines.
func auditLines(t *testing.T, sb *stringsBuilder) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(sb.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad audit line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}
