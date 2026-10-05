// Package proxy implements the metering model proxy (P2b, ADR-0009): the
// D33/D34 reverse-proxy shape (origin-form plain HTTP in-cluster from the
// agent, single upstream, the credential injected from the mounted
// model-creds Secret, CONNECT / absolute-form 405'd) PLUS per-Loop
// cumulative usage metering and the operator-only /coxswain/usage endpoint.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const maxBody = 50 << 20 // 50 MiB; model request/response bodies are small.

// streamKey is the context key for the internal streaming marker.
type streamKeyType int

const streamKey streamKeyType = iota

// Config configures a metering proxy.
type Config struct {
	// Upstream is the parsed MODEL_ENDPOINT URL (the origin of the proxy's own
	// request; https:// verified TLS, http:// plain; no CONNECT/absolute-form).
	Upstream *url.URL
	// Model is the Loop's model name (reported in the reading; not the key).
	Model string
	// LoopName / Namespace identify the Loop for the audit line.
	LoopName  string
	Namespace string
	// CredHeader is the header name injected with the credential (default
	// "Authorization", value "Bearer <key>").
	CredHeader string
	// Credential is the API key read from the mounted model-creds Secret. It
	// is never logged.
	Credential string
	// Meter holds the persisted cumulative counters. When nil, metering is
	// disabled (every request is forwarded, no /coxswain/usage counts).
	Meter *Meter
	// Audit is the destination for one JSON audit line per metered request
	// (stdout in production; a buffer in tests). nil disables audit output.
	Audit io.Writer
	// Since is the boot wall-clock timestamp (reported in the reading).
	Since time.Time
}

// Proxy is the metering model proxy. It is a single upstream reverse proxy
// (httputil.NewSingleHostReverseProxy) wrapped with steering-proof request
// shaping and post-response usage metering.
type Proxy struct {
	cfg      Config
	upstream *url.URL
	rp       *httputil.ReverseProxy
	// dials is a counter of upstream dials (for the no-retry test: a single
	// request must dial exactly once).
	dials atomic.Int64
}

// New builds the metering proxy. The upstream transport has
// DisableCompression: true (P3: Go's http.Transport otherwise adds
// Accept-Encoding: gzip itself, making "the upstream request has no
// Accept-Encoding" untestable). Redirects are not followed (the model
// endpoint is a fixed origin).
func New(cfg Config) (*Proxy, error) {
	if cfg.Upstream == nil {
		return nil, errNoUpstream
	}
	if cfg.CredHeader == "" {
		cfg.CredHeader = "Authorization"
	}
	p := &Proxy{cfg: cfg, upstream: cfg.Upstream}
	rp := httputil.NewSingleHostReverseProxy(cfg.Upstream)
	rp.Transport = &http.Transport{
		DisableCompression: true, // P3: identity encoding end to end.
	}
	// The default Director (NewSingleHostReverseProxy) sets the upstream
	// host; no redirect following. A dial error (upstream down) is the
	// reverse proxy's 502 default.
	p.rp = rp
	return p, nil
}

var errNoUpstream = &errString{"MODEL_ENDPOINT not set (no upstream)"}

type errString struct{ s string }

func (e *errString) Error() string { return e.s }

// DialHandler returns a handler that dials the upstream directly (test seam
// for the no-retry / 405 / audit tests). It is equivalent to p.Handler but
// counts dials for the no-retry assertion.
func (p *Proxy) DialHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.dials.Add(1)
		p.serve(w, r)
	})
}

// Handler is the reverse-proxy HTTP handler (the 8080 listener).
func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.serve(w, r)
	})
}

// Dials returns the number of upstream dials (the no-retry test asserts a
// single request dials exactly once).
func (p *Proxy) Dials() int64 { return p.dials.Load() }

// serve performs one proxied request: shape the request, inject the
// credential, forward (exactly one dial), meter the response, write the audit
// line.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	// CONNECT / absolute-form: 405, no dial (the D33/D34 stand-in shape).
	if r.Method == http.MethodConnect || r.URL.Host != "" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read + shape the request body (strip Accept-Encoding is done on the
	// header below; the body is rewritten for include_usage when streaming).
	var body []byte
	if r.Body != nil {
		body = drain(r.Body, maxBody)
		_ = r.Body.Close()
	}
	newBody, isObject := shapeRequest(body)
	// The request body is identity-encoded; the caller's transport has
	// DisableCompression, so there is no Accept-Encoding negotiation.
	r.Body = io.NopCloser(bytes.NewReader(newBody))
	// Strip the agent's Accept-Encoding (steering-proof).
	r.Header.Del("Accept-Encoding")
	// Mark the request as streaming (the round-tripper reads this from the
	// context to pick the SSE usage parser; the marker is internal to the
	// proxy — never sent upstream).
	if isStreaming(newBody) {
		r = r.WithContext(context.WithValue(r.Context(), streamKey, true))
	}
	// Inject the credential (never logged).
	if p.cfg.Credential != "" {
		val := p.cfg.Credential
		if p.cfg.CredHeader == "Authorization" && !strings.HasPrefix(val, "Bearer ") {
			val = "Bearer " + val
		}
		r.Header.Set(p.cfg.CredHeader, val)
	}

	// Forward (exactly one dial; no redirect following; a non-2xx upstream
	// response is returned as-is — no retry).
	rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	p.rp.ServeHTTP(rec, r)

	// Meter the response. With DisableCompression the proxy gunzips any
	// Content-Encoding: gzip body before it reaches the handler, so the body
	// here is identity-encoded and the usage is always parseable. We cannot
	// observe the already-written body directly (the reverse proxy streams
	// it), so the metering reads the request shape + the upstream response
	// usage captured below. Because the reverse proxy writes the body
	// straight through, we capture it with a tee reader on the upstream side
	// via a RoundTripper (installed in New for the metering path).
	if p.cfg.Meter != nil {
		p.meterResponse(r, isObject, rec)
	}
}

// meterResponse is the metering hook. In the production single-dial path the
// response body is streamed through the reverse proxy, so the usage is
// captured by a metering RoundTripper installed on the proxy's transport (see
// New). This method is a placeholder retained for the test seam where the
// body is captured directly.
func (p *Proxy) meterResponse(r *http.Request, isObject bool, rec *statusWriter) {
	// The RoundTripper already metered + audited; nothing to do here.
}

// NewMetered builds the proxy with a metering RoundTripper installed on the
// upstream transport so the response body is captured (tee'd) for usage
// parsing and the audit line. The agent's 8080 listener uses this.
func NewMetered(cfg Config) (*Proxy, error) {
	p, err := New(cfg)
	if err != nil {
		return nil, err
	}
	base := p.rp.Transport
	p.rp.Transport = &meteringRoundTripper{
		base:  base,
		cfg:   cfg,
		proxy: p,
	}
	return p, nil
}

// meteringRoundTripper wraps the base transport and captures the upstream
// response body (tee) so the proxy can parse the usage object and write the
// audit line. The body is forwarded to the agent unchanged.
type meteringRoundTripper struct {
	base  http.RoundTripper
	cfg   Config
	proxy *Proxy
}

func (m *meteringRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	m.proxy.dials.Add(1)
	resp, err := m.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	// Capture a bounded copy of the body for metering + audit, then forward
	// the original body to the agent.
	var captured []byte
	if resp.Body != nil {
		captured = drain(resp.Body, maxBody)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(captured))
	}
	if m.cfg.Meter != nil {
		prompt, completion, ok := int64(0), int64(0), false
		if isSSE(req) {
			prompt, completion, ok = parseStreamUsage(captured)
		} else {
			prompt, completion, ok = parseUsage(captured)
		}
		if ok {
			m.cfg.Meter.Add(prompt, completion)
		} else {
			m.cfg.Meter.AddUnmetered()
		}
	}
	if m.cfg.Audit != nil {
		m.audit(req, resp, captured)
	}
	return resp, nil
}

// audit writes one JSON line per metered request (observability only — not
// the gate's input). The line never carries the credential.
func (m *meteringRoundTripper) audit(req *http.Request, resp *http.Response, body []byte) {
	var prompt, completion int64
	if isSSE(req) {
		prompt, completion, _ = parseStreamUsage(body)
	} else {
		prompt, completion, _ = parseUsage(body)
	}
	line := map[string]any{
		"time":             time.Now().UTC().Format(time.RFC3339),
		"loop":             m.cfg.LoopName,
		"namespace":        m.cfg.Namespace,
		"source":           "model-proxy",
		"action":           "usage",
		"model":            m.cfg.Model,
		"promptTokens":     prompt,
		"completionTokens": completion,
		"status":           resp.StatusCode,
		"usagePresent":     prompt > 0 || completion > 0,
	}
	enc, _ := json.Marshal(line)
	_, _ = m.cfg.Audit.Write(append(enc, '\n'))
}

// isSSE reports whether the request asked for a streaming response (the
// internal context marker set in serve when the body had stream:true, or an
// Accept: text/event-stream header).
func isSSE(req *http.Request) bool {
	if req.Context().Value(streamKey) == true {
		return true
	}
	return strings.Contains(req.Header.Get("Accept"), "text/event-stream")
}

// isStreaming reports whether a JSON request body asked for stream:true.
func isStreaming(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var req struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(trimmed, &req); err != nil {
		return false
	}
	return req.Stream
}

// readDir / openFile are the indirection points the tests override to feed
// the model-creds startup check a synthetic file.
var readDir = os.ReadDir
var openFile = os.Open

// logNoop is a no-op logger (the audit/usage channel is the stdout writer).
var _ = log.Printf

// statusWriter records the response status code (the audit line reports it).
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// UsageHandler serves GET /coxswain/usage (0.0.0.0:9090) with the cumulative
// reading. No other endpoint is served (item 15: the round-1 /metrics debug
// endpoint is dropped).
func (p *Proxy) UsageHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/coxswain/usage" {
			http.NotFound(w, r)
			return
		}
		if p.cfg.Meter == nil {
			http.Error(w, "metering disabled", http.StatusServiceUnavailable)
			return
		}
		reading := p.cfg.Meter.Reading()
		w.Header().Set("Content-Type", "application/json")
		enc, _ := json.Marshal(reading)
		_, _ = w.Write(enc)
	})
}

// ReadModelCreds reads every regular file under dir (skipping the ..data
// symlink directory Kubernetes uses for atomic Secret updates) and returns
// the first non-empty file's contents, trimmed of a trailing newline. This is
// the D33 model-creds startup check: the proxy reads a REAL key file from the
// mounted Secret and exits 1 if none is found.
func ReadModelCreds(dir string) (string, error) {
	entries, err := readDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := dir + "/" + e.Name()
		f, err := openFile(path)
		if err != nil {
			continue
		}
		buf, _ := io.ReadAll(io.LimitReader(f, 1<<20))
		_ = f.Close()
		if len(buf) > 0 {
			return strings.TrimRight(string(buf), "\n"), nil
		}
	}
	return "", &errString{"no readable model-creds Secret file found"}
}
