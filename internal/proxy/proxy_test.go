package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// startFakeUpstream returns a test server that records the last request it saw
// (headers + body) and responds with the given handler. It is used to prove
// what the proxy actually sent upstream (steering-proof assertions).
func startFakeUpstream(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	rec := &recordedReq{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.last = &reqSeen{headers: r.Header.Clone(), body: body, path: r.URL.Path, method: r.Method}
		rec.n++
		rec.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type reqSeen struct {
	headers http.Header
	body    []byte
	path    string
	method  string
}

type recordedReq struct {
	mu   sync.Mutex
	last *reqSeen
	n    int
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return u
}

func TestRejectsAbsoluteURLOrCONNECT(t *testing.T) {
	// CONNECT and absolute-form (steering attempts) -> 405, and the upstream is
	// never dialed.
	seen := 0
	up := startFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { seen++ })
	p, err := New(Config{Upstream: mustURL(t, up.URL)})
	if err != nil {
		t.Fatal(err)
	}
	h := p.Handler()

	// CONNECT (absolute-form CONNECT host:port).
	req, _ := http.NewRequest(http.MethodConnect, "evil.example:8080", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("CONNECT -> %d, want 405", w.Code)
	}

	// Absolute-form GET (an absolute URI in the request target).
	req2 := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Scheme: "http", Host: "evil.example", Path: "/chat"},
		Header: http.Header{},
	}
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusMethodNotAllowed {
		t.Fatalf("absolute-form -> %d, want 405", w2.Code)
	}

	if seen != 0 {
		t.Fatalf("upstream dialed %d times for steering attempts, want 0", seen)
	}
}

func TestNoRetrySingleDial(t *testing.T) {
	// A 4xx from the model is returned as-is (no retry): exactly one dial, the
	// client sees the same 4xx.
	var count int
	mu := sync.Mutex{}
	up := startFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		http.Error(w, "bad request", http.StatusBadRequest)
	})
	p, err := NewMetered(Config{Upstream: mustURL(t, up.URL)})
	if err != nil {
		t.Fatal(err)
	}
	h := p.Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"x"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	mu.Lock()
	dials := count
	mu.Unlock()
	if dials != 1 {
		t.Fatalf("upstream dialed %d times for one 4xx request, want exactly 1 (no retry)", dials)
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("client saw %d, want the upstream's 400 returned as-is", w.Code)
	}
}

func TestNoRetrySingleDialSuccess(t *testing.T) {
	// A 2xx also dials exactly once (the success path is not retried either).
	var count int
	mu := sync.Mutex{}
	up := startFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	p, err := NewMetered(Config{Upstream: mustURL(t, up.URL)})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"x"}`))
	w := httptest.NewRecorder()
	p.Handler().ServeHTTP(w, req)

	mu.Lock()
	dials := count
	mu.Unlock()
	if dials != 1 {
		t.Fatalf("upstream dialed %d times for one 2xx request, want exactly 1", dials)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("client saw %d, want 200", w.Code)
	}
}

func TestAuditLinePerMeteredRequest(t *testing.T) {
	// A metered request produces one JSON audit line carrying the token counts
	// and the model. The audit channel (stdout) is the observability-only path.
	var audit bytes.Buffer
	up := startFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":11,"completion_tokens":5}}`))
	})
	m := newTestMeter(t, "test-model", time.Now())
	p, err := NewMetered(Config{
		Upstream:  mustURL(t, up.URL),
		Model:     "test-model",
		LoopName:  "loop-a",
		Namespace: "ns",
		Audit:     &audit,
		Meter:     m,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"x"}`))
	w := httptest.NewRecorder()
	p.Handler().ServeHTTP(w, req)

	// Exactly one audit line, valid JSON, with the token counts.
	lines := 0
	var decoded map[string]any
	for line := bufio.NewScanner(bytes.NewReader(audit.Bytes())); line.Scan(); {
		lineStr := line.Text()
		if lineStr == "" {
			continue
		}
		lines++
		if err := json.Unmarshal([]byte(lineStr), &decoded); err != nil {
			t.Fatalf("audit line %q is not valid JSON: %v", lineStr, err)
		}
	}
	if lines != 1 {
		t.Fatalf("expected exactly 1 audit line, got %d\nbuf=%q", lines, audit.String())
	}
	if decoded["promptTokens"] != float64(11) || decoded["completionTokens"] != float64(5) {
		t.Fatalf("audit line tokens = %v/%v, want 11/5; line=%v", decoded["promptTokens"], decoded["completionTokens"], decoded)
	}
	if decoded["model"] != "test-model" {
		t.Fatalf("audit line model = %v, want test-model", decoded["model"])
	}
	if decoded["loop"] != "loop-a" {
		t.Fatalf("audit line loop = %v, want loop-a", decoded["loop"])
	}
}

func TestOriginRequestHasNoAcceptEncoding(t *testing.T) {
	// The proxy's own request (the origin) carries no Accept-Encoding header.
	// The agent sent one (simulating its client), but the proxy stripped it
	// before dialing. This is the P3 steering-proof assertion.
	var sawAcceptEncoding bool
	up := startFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "" {
			sawAcceptEncoding = true
		}
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	p, err := NewMetered(Config{Upstream: mustURL(t, up.URL)})
	if err != nil {
		t.Fatal(err)
	}
	// The agent's request carries Accept-Encoding: gzip (a real client does).
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"x"}`))
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	p.Handler().ServeHTTP(w, req)

	if sawAcceptEncoding {
		t.Fatalf("the proxy's origin request carried Accept-Encoding; it must be stripped (a gzip body would hide the usage from the meter)")
	}
}
