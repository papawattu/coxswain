package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestParseModelTimeout(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"":      0,
		"90s":   90 * time.Second,
		"45":    45 * time.Second,
		" 2m ":  2 * time.Minute,
		"0":     0,
		"-5s":   0,
		"bogus": 0,
	} {
		if got := parseModelTimeout(in); got != want {
			t.Errorf("parseModelTimeout(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestEntrypointHonoursModelTimeout (F46): the built runner binary, given
// COX_MODEL_TIMEOUT=1s and a model that never answers, abandons the first
// request at ~1s and retries (2s backoff), so a second request arrives well
// inside 10s. With the env ignored the default is 120s and no second request
// ever arrives in the window.
func TestEntrypointHonoursModelTimeout(t *testing.T) {
	var mu sync.Mutex
	var arrivals []time.Time
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		mu.Unlock()
		select { // never answer; return when the client gives up
		case <-r.Context().Done():
		case <-done:
		case <-time.After(60 * time.Second):
		}
	}))
	defer srv.Close()
	defer close(done) // runs before srv.Close: release any parked handler

	bin := filepath.Join(t.TempDir(), "runner")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".coxswain"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".coxswain", "desired-phase"), []byte("Planning"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "COX_GOAL=g", "COX_MODEL_BASE_URL="+srv.URL,
		"COX_WORKSPACE="+ws, "COX_MODEL_TIMEOUT=1s")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(arrivals)
		mu.Unlock()
		if n >= 2 {
			mu.Lock()
			gap := arrivals[1].Sub(arrivals[0])
			mu.Unlock()
			if gap > 8*time.Second {
				t.Fatalf("second attempt after %v; the 1s timeout was not applied", gap)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("no second model request within 10s: COX_MODEL_TIMEOUT=1s was not applied (default is 120s)")
}
