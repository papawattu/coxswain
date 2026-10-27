// F79 (pi3 .samples/prereview-241.md finding 4; design .samples/design-f79-f82-refresh.md
// §F79): the runner's I89 retry documented "times out OR returns a transport error" but
// only retried context deadlines, so an immediate transport failure surfaced as
// ModelUnavailable after one attempt and blocked the claim. The curator's Judge already
// retried that class (F41).
//
// The class is NARROW on purpose (F79 review F2): only a failure that proves the request
// never reached the model is retried — DNS, or a DIAL failure (connection refused, no
// route, unreachable). Anything after the request was written (EOF, reset, a read error)
// may have been accepted and processed, so re-sending it would bill and meter the same
// completion twice for the one answer that is used.

package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/papawattu/coxswain/runner/testhelper"
)

// f79Logs is a log sink safe for concurrent use: the retry-then-succeed spec
// waits (in another goroutine) for the first retry line before it starts
// serving, and the runner's goroutine writes that line.
type f79Logs struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *f79Logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *f79Logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// f79LogCapture redirects the standard logger into a concurrent-safe buffer for
// the test's duration (tests in this package do not run in parallel, so the
// global swap cannot interleave with another test's output).
func f79LogCapture(t *testing.T) *f79Logs {
	t.Helper()
	logs := &f79Logs{}
	prev := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	return logs
}

// f79FreeAddr reserves a loopback address and releases it, so the caller can
// have a genuinely closed port (the DIAL class) before it starts serving.
func f79FreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release the loopback port: %v", err)
	}
	return addr
}

// f79AbortConnection closes the connection without writing a response — the
// request WAS received, which is exactly the class that must not be retried.
func f79AbortConnection(w http.ResponseWriter) error {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return errors.New("the response writer is not an http.Hijacker")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return err
	}
	return conn.Close()
}

// TestF79ModelRetriesDialFailureAndSucceeds is the F79 detector: the first
// attempt fails to CONNECT (the port is closed), the server then starts
// listening on that port, and the second attempt gets a real answer.
func TestF79ModelRetriesDialFailureAndSucceeds(t *testing.T) {
	fm := testhelper.New() // the fake's default final answer is "done"
	defer fm.Close()
	addr := f79FreeAddr(t)

	logs := f79LogCapture(t)
	retries := 3
	// The listener the watcher binds is handed back here so the test can close
	// it (the Server would otherwise serve for the rest of the package run).
	lnCh := make(chan net.Listener, 1)
	t.Cleanup(func() {
		select {
		case l := <-lnCh:
			_ = l.Close()
		default:
		}
	})
	go func() {
		// Start serving once the runner has logged its first retry: the first
		// attempt is then a real ECONNREFUSED (the dial class) and the injected
		// 50ms backoff leaves ample time to bind.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(logs.String(), "call model: retry 1/") {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("start serving on %s after the first retry: %v", addr, err)
			return
		}
		lnCh <- l
		srv := &http.Server{Handler: fm.DefaultHandler(), ReadHeaderTimeout: 5 * time.Second}
		_ = srv.Serve(l) // returns http.ErrServerClosed when the test closes the listener
	}()

	res := run(runConfig{
		Prompt:            doSomethingPrompt,
		Workspace:         t.TempDir(),
		BaseURL:           "http://" + addr,
		Model:             fakeModelName,
		MaxSteps:          1,
		ModelRetries:      retries,
		ModelRetryBackoff: 50 * time.Millisecond,
	})

	if res.Status != statusSuccess {
		t.Fatalf("status = %q (notes: %s), want success: a dial failure must be retried",
			res.Status, res.VerificationNotes)
	}
	if res.Summary == "" {
		t.Fatal("summary empty, want the second attempt's answer")
	}
	if n := strings.Count(logs.String(), "call model: retry "); n != 1 {
		t.Errorf("log has %d retry lines, want exactly 1 (the second attempt succeeded): %q", n, logs.String())
	}
	if n := f79CountRetryTraces(res.ToolTrace); n != 1 {
		t.Errorf("trace has %d retry lines, want 1: %v", n, res.ToolTrace)
	}
}

// f79CountRetryTraces counts the retry lines in the result's trace.
func f79CountRetryTraces(trace []string) int {
	n := 0
	for _, l := range trace {
		if strings.HasPrefix(l, "call model: retry ") {
			n++
		}
	}
	return n
}

// TestF79ModelDialFailureExhaustsAsModelUnavailable: every attempt is a dial
// failure (the port stays closed). The runner spends the ladder, logs one line
// per retry, and surfaces ModelUnavailable with the transport text.
func TestF79ModelDialFailureExhaustsAsModelUnavailable(t *testing.T) {
	addr := f79FreeAddr(t)
	logs := f79LogCapture(t)
	const retries = 2
	res := run(runConfig{
		Prompt:            doSomethingPrompt,
		Workspace:         t.TempDir(),
		BaseURL:           "http://" + addr,
		Model:             fakeModelName,
		MaxSteps:          1,
		ModelRetries:      retries,
		ModelRetryBackoff: time.Millisecond,
	})

	if res.Status != statusBlocked {
		t.Fatalf("status = %q, want blocked when every attempt fails", res.Status)
	}
	if !strings.Contains(res.VerificationNotes, "ModelUnavailable") {
		t.Errorf("VerificationNotes must carry ModelUnavailable, got %q", res.VerificationNotes)
	}
	if !strings.Contains(res.VerificationNotes, "connection refused") {
		t.Errorf("the notes must name the dial failure, got %q", res.VerificationNotes)
	}
	// One line per retry, and the final attempt logs none: `retries` lines is
	// exactly `retries+1` attempts, which the closed port cannot be counted
	// from directly.
	if n := strings.Count(logs.String(), "call model: retry "); n != retries {
		t.Errorf("log has %d retry lines, want %d (one per retry, not per attempt)", n, retries)
	}
	if n := f79CountRetryTraces(res.ToolTrace); n != retries {
		t.Errorf("trace has %d retry lines, want %d", n, retries)
	}
}

// TestF79RequestReceivedThenConnectionLostIsNotRetried is the F2 narrowing: the
// handler RECEIVED the request and then the connection died (EOF), so the
// completion may have been accepted and processed. It must NOT be retried —
// otherwise the model bills and the proxy meters the same call twice for one
// answer.
func TestF79RequestReceivedThenConnectionLostIsNotRetried(t *testing.T) {
	fm := testhelper.New()
	defer fm.Close()
	var mu sync.Mutex
	attempts := 0
	fm.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		if err := f79AbortConnection(w); err != nil {
			t.Errorf("abort: %v", err)
		}
	})

	res := run(runConfig{
		Prompt:            doSomethingPrompt,
		Workspace:         t.TempDir(),
		BaseURL:           fm.URL,
		Model:             fakeModelName,
		MaxSteps:          1,
		ModelRetries:      3,
		ModelRetryBackoff: time.Millisecond,
	})

	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 1 {
		t.Fatalf("attempts = %d, want 1: the request reached the server, so a retry "+
			"could re-send an accepted completion (double billing/metering)", got)
	}
	if res.Status != statusBlocked || !strings.Contains(res.VerificationNotes, "ModelUnavailable") {
		t.Errorf("status/notes = %q/%q, want blocked/ModelUnavailable", res.Status, res.VerificationNotes)
	}
}

// TestF79ServerErrorIsNotRetried is the "server answered" half: an HTTP status is
// definitive, so one attempt.
func TestF79ServerErrorIsNotRetried(t *testing.T) {
	fm := testhelper.New()
	defer fm.Close()
	var mu sync.Mutex
	attempts := 0
	fm.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	})

	res := run(runConfig{
		Prompt:            doSomethingPrompt,
		Workspace:         t.TempDir(),
		BaseURL:           fm.URL,
		Model:             fakeModelName,
		MaxSteps:          1,
		ModelRetries:      3,
		ModelRetryBackoff: time.Millisecond,
	})

	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 1 {
		t.Fatalf("attempts = %d, want 1: an HTTP status is the server speaking, so it is definitive", got)
	}
	if res.Status != statusBlocked || !strings.Contains(res.VerificationNotes, "ModelUnavailable") {
		t.Errorf("status/notes = %q/%q, want blocked/ModelUnavailable", res.Status, res.VerificationNotes)
	}
	if !strings.Contains(res.VerificationNotes, "500") {
		t.Errorf("the notes must name the status the server returned, got %q", res.VerificationNotes)
	}
}

// TestF79BadAnswersAreNotRetried covers the other two 200-shaped negatives the
// classifier's comment claims: malformed JSON (a decode error) and an empty
// choices array. Neither is retried.
func TestF79BadAnswersAreNotRetried(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"malformed JSON", `{"choices":`},
		{"no choices", `{"choices":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fm := testhelper.New()
			defer fm.Close()
			var mu sync.Mutex
			attempts := 0
			fm.SetHandler(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				attempts++
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			})

			res := run(runConfig{
				Prompt:            doSomethingPrompt,
				Workspace:         t.TempDir(),
				BaseURL:           fm.URL,
				Model:             fakeModelName,
				MaxSteps:          1,
				ModelRetries:      3,
				ModelRetryBackoff: time.Millisecond,
			})

			mu.Lock()
			got := attempts
			mu.Unlock()
			if got != 1 {
				t.Fatalf("attempts = %d, want 1: a 200 with a bad answer is definitive", got)
			}
			if res.Status != statusBlocked || !strings.Contains(res.VerificationNotes, "ModelUnavailable") {
				t.Errorf("status/notes = %q/%q, want blocked/ModelUnavailable", res.Status, res.VerificationNotes)
			}
		})
	}
}

// TestF79IsRetryableModelError pins the classifier per class: the dial/DNS
// classes are retryable, the post-write classes are not.
func TestF79IsRetryableModelError(t *testing.T) {
	closedDial := func() error {
		addr := f79FreeAddr(t)
		client := &http.Client{Timeout: 2 * time.Second}
		_, err := client.Post("http://"+addr+"/x", "application/json", strings.NewReader("{}"))
		if err == nil {
			t.Fatal("expected a connection error from the closed port")
		}
		return err
	}()
	urlErr := func(err error) error { return &url.Error{Op: "Post", URL: "http://x.invalid/", Err: err} }
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"real closed-port dial failure", closedDial, true},
		{"DNS failure (never resolved)", urlErr(&net.DNSError{
			Err: "no such host", Name: "x.invalid", IsNotFound: true,
		}), true},
		{"dial failure: refused", urlErr(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), true},
		{"dial failure: no route", urlErr(&net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}), true},
		{"context deadline (the I89 class)", urlErr(context.DeadlineExceeded), true},
		{"EOF after the request was sent", urlErr(io.EOF), false},
		{"reset after the request was sent", urlErr(&net.OpError{Op: "read", Err: syscall.ECONNRESET}), false},
		{"a plain error (an HTTP status)", errors.New("model returned HTTP 500"), false},
		{"a bare io.EOF", io.EOF, false},
	}
	for _, tc := range cases {
		if got := isRetryableModelError(tc.err); got != tc.want {
			t.Errorf("isRetryableModelError(%s) = %t, want %t (%v)", tc.name, got, tc.want, tc.err)
		}
	}
}

// TestF79RunnerInstallsProductionLadder pins the production ladder through the
// config path (F41 finding 1: a production constant left unkilled is how the
// curator's window went unspecced for a release).
func TestF79RunnerInstallsProductionLadder(t *testing.T) {
	prod := modelRetryLadderFor(0, 0)
	if prod.retries != defaultModelRetries || prod.backoff != defaultModelRetryBackoff {
		t.Fatalf("the default ladder = %+v, want the production %d retries / %s",
			prod, defaultModelRetries, defaultModelRetryBackoff)
	}
	if defaultModelRetries != 3 || defaultModelRetryBackoff != 2*time.Second {
		t.Fatalf("production ladder changed to %d/%s; the 494s worst case in the I89 comment assumes 3 retries at 2s/4s/8s",
			defaultModelRetries, defaultModelRetryBackoff)
	}
	override := modelRetryLadderFor(5, time.Millisecond)
	if override.retries != 5 || override.backoff != time.Millisecond {
		t.Fatalf("an explicit ladder must win, got %+v", override)
	}
	none := modelRetryLadderFor(-1, 0)
	if none.retries != 0 {
		t.Fatalf("a negative retries must mean no retries, got %d", none.retries)
	}
}
