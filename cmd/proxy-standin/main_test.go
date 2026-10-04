package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// captureLog replaces the standard logger output for the duration of f and
// returns what it wrote. The proxy's log lines go through the standard
// logger (log.Printf), which by default writes to stderr.
func captureLog(t *testing.T, f func()) string {
	t.Helper()
	old := log.Writer()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	log.SetOutput(w)
	log.SetFlags(0)
	f()
	_ = w.Close()
	log.SetOutput(old)
	out, _ := io.ReadAll(r)
	_ = r.Close()
	return string(out)
}

// newHandlerFor builds the forwarding handler for a backend URL.
func newHandlerFor(t *testing.T, backendURL string) http.Handler {
	t.Helper()
	u, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	return newForwardingHandler(u)
}

func countForwardedLines(s string) int {
	n := 0
	for _, line := range splitNonEmpty(s) {
		if strings.HasPrefix(line, "proxy: forwarded ") {
			n++
		}
	}
	return n
}

func TestOneLogLinePerForwardedRequest(t *testing.T) {
	// Backend writes a distinctive body so the test can assert the
	// response was forwarded (and that the body did not leak into the log).
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "backend-body-marker-SECRET") //nolint:errcheck
	}))
	defer backend.Close()
	h := newHandlerFor(t, backend.URL)

	cases := []struct{ method, path string }{
		{"POST", "/v1/chat/completions"},
		{"GET", "/v1/models"},
	}
	logOut := captureLog(t, func() {
		for _, c := range cases {
			req := httptest.NewRequest(c.method, c.path, nil)
			// A distinctive auth header that MUST NOT appear in the log.
			req.Header.Set("Authorization", "Bearer secret-token-DO-NOT-LOG")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Body.String() != "backend-body-marker-SECRET" {
				t.Fatalf("forwarding failed for %s %s: body=%q", c.method, c.path, w.Body.String())
			}
		}
	})

	if got, want := countForwardedLines(logOut), len(cases); got != want {
		t.Fatalf("expected exactly %d 'proxy: forwarded' lines, got %d; log:\n%s", want, got, logOut)
	}
	if strings.Contains(logOut, "secret-token-DO-NOT-LOG") {
		t.Fatalf("log leaked the Authorization header:\n%s", logOut)
	}
	if strings.Contains(logOut, "Authorization") {
		t.Fatalf("log leaked a header name:\n%s", logOut)
	}
	if strings.Contains(logOut, "backend-body-marker-SECRET") {
		t.Fatalf("log leaked the response body:\n%s", logOut)
	}
}

func TestLogLineFields(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer backend.Close()
	h := newHandlerFor(t, backend.URL)

	logOut := captureLog(t, func() {
		req := httptest.NewRequest("POST", "/v1/complete", nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	})
	for _, want := range []string{
		"method=POST",
		`path="/v1/complete"`,
		"status=418",
		"duration_ms=",
	} {
		if !strings.Contains(logOut, want) {
			t.Errorf("log line missing %q:\n%s", want, logOut)
		}
	}
}

func splitNonEmpty(s string) []string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
