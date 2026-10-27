// F79 (pi3 .samples/prereview-241.md finding 4; design .samples/design-f79-f82-refresh.md
// §F79): the runner's I89 retry documented "times out OR returns a transport error" but
// only retried context deadlines, so an immediate transport failure (no HTTP response at
// all) surfaced as ModelUnavailable and blocked the claim. The curator's Judge already
// retried that class (F41). These specs drive the REAL driveModel/run path with a
// millisecond ladder, so nothing sleeps through the production 2s+4s+8s.

package runner

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/papawattu/coxswain/runner/testhelper"
)

// f79LogCapture redirects the standard logger into the returned buffer for the
// duration of the test (tests in this package do not run in parallel, so the
// global swap cannot interleave with another test's output).
func f79LogCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// f79Ladder is the millisecond ladder the specs inject through runConfig.
func f79Ladder(retries int) (int, time.Duration) { return retries, time.Millisecond }

// f79AbortConnection closes the connection without writing a response, which
// is what client.Do reports as a *url.Error — the transport class under test.
func f79AbortConnection(w http.ResponseWriter) error {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return io.ErrUnexpectedEOF
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return err
	}
	return conn.Close()
}

// TestF79ModelRetriesTransportFailureAndSucceeds is the F79 detector: the first
// two attempts are aborted at the transport level, the third gets a real answer,
// and the runner returns it — with one trace line and one LOG line per retry.
func TestF79ModelRetriesTransportFailureAndSucceeds(t *testing.T) {
	fm := testhelper.New() // default final answer "done"
	defer fm.Close()

	var mu sync.Mutex
	attempts := 0
	fm.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n <= 2 {
			if err := f79AbortConnection(w); err != nil {
				t.Errorf("abort attempt %d: %v", n, err)
			}
			return
		}
		fm.DefaultHandler()(w, r)
	})

	logs := f79LogCapture(t)
	retries, backoff := f79Ladder(3)
	res := run(runConfig{
		Prompt:            doSomethingPrompt,
		Workspace:         t.TempDir(),
		BaseURL:           fm.URL,
		Model:             fakeModelName,
		MaxSteps:          1,
		ModelRetries:      retries,
		ModelRetryBackoff: backoff,
	})

	if res.Status != statusSuccess {
		t.Fatalf("status = %q (notes: %s), want success: a transport failure must be retried, not surfaced as definitive",
			res.Status, res.VerificationNotes)
	}
	if res.Summary == "" {
		t.Fatal("summary empty, want the third attempt's answer")
	}
	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 3 {
		t.Fatalf("attempts = %d, want 3 (two transport failures, then success)", got)
	}
	retryTraces := 0
	for _, l := range res.ToolTrace {
		if strings.HasPrefix(l, "call model: retry ") {
			retryTraces++
		}
	}
	if retryTraces != 2 {
		t.Errorf("trace has %d retry lines, want 2: %v", retryTraces, res.ToolTrace)
	}
	if n := strings.Count(logs.String(), "call model: retry "); n != 2 {
		t.Errorf("log has %d retry lines, want 2 (a retry must be visible — the trace is not read by the operator): %q",
			n, logs.String())
	}
}

// TestF79ModelTransportFailureExhaustsAsModelUnavailable: every attempt fails
// at the transport level. The runner exhausts the ladder (retries+1 attempts,
// counted at the listener), logs one line per retry, and surfaces
// ModelUnavailable with the transport error text.
func TestF79ModelTransportFailureExhaustsAsModelUnavailable(t *testing.T) {
	// A listener that accepts and immediately closes: every attempt is a
	// transport failure and the accept count IS the attempt count.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	var mu sync.Mutex
	accepts := 0
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepts++
			mu.Unlock()
			_ = conn.Close()
		}
	}()

	logs := f79LogCapture(t)
	const retries = 2
	res := run(runConfig{
		Prompt:            doSomethingPrompt,
		Workspace:         t.TempDir(),
		BaseURL:           "http://" + l.Addr().String(),
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
	mu.Lock()
	got := accepts
	mu.Unlock()
	if got != retries+1 {
		t.Fatalf("attempts = %d, want %d (the injected ladder: 1 initial + %d retries)", got, retries+1, retries)
	}
	if n := strings.Count(logs.String(), "call model: retry "); n != retries {
		t.Errorf("log has %d retry lines, want %d (one per retry, not per attempt)", n, retries)
	}
}

// TestF79ServerErrorIsNotRetried is the negative half of the classification:
// the server ANSWERED (HTTP 500), so the error is definitive and one attempt is
// made. A mutation that retried every error would fail here.
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

// TestF79IsRetryableModelError pins the classifier directly (both classes plus
// the definitive ones).
func TestF79IsRetryableModelError(t *testing.T) {
	closed := func() error {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr := l.Addr().String()
		_ = l.Close()
		_, err = (&http.Client{Timeout: time.Second}).Post("http://"+addr+"/x", "application/json", strings.NewReader("{}"))
		if err == nil {
			t.Fatal("expected a connection error from the closed port")
		}
		return err
	}()
	if !isRetryableModelError(closed) {
		t.Errorf("a transport error (%v) must be retryable", closed)
	}
	if !isRetryableModelError(&url.Error{Op: "Post", Err: context.DeadlineExceeded}) {
		t.Error("a context deadline must be retryable")
	}
	if isRetryableModelError(io.EOF) {
		t.Error("a bare io.EOF (not a *url.Error) is not the transport class the runner sees; must not be retryable")
	}
	if !isRetryableModelError(&url.Error{Op: "Post", Err: io.EOF}) {
		t.Error("a *url.Error (no HTTP response) must be retryable whatever the cause")
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
