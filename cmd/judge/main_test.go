// Execution tests for the Judge binary (AGENTS.md: the shell-script/entrypoint
// norm — run the REAL built binary, not a re-implementation). go build puts
// the binary in t.TempDir(), exec.Command runs it against an httptest
// server standing in for the model proxy's /v1/chat/completions, and the
// result (stdout or the COX_JUDGE_RESULT_PATH file) is parsed as JSON and
// checked on its FIELDS (AGENTS.md: a validation checks the content's
// meaning, not just that it parses).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBinary compiles the real binary into dir and returns its path.
func buildBinary(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "judge-bin")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	cmd.Dir = wd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// judgeEnv returns the env for a run: PATH plus the base Judge inputs
// (the diff is written to path by the caller).
func judgeEnv(t *testing.T, diffPath string) []string {
	t.Helper()
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"COX_JUDGE_BASE_URL=<set-by-test>",
		"COX_JUDGE_MODEL=test-model",
		"COX_JUDGE_MAX_TOKENS=2048",
		"COX_JUDGE_GOAL=add a feature",
		"COX_JUDGE_PLAN=" + "- step one",
		"COX_JUDGE_CHECKS=[{\"name\":\"make test\",\"exitCode\":0}]",
		"COX_JUDGE_DIFF_PATH=" + diffPath,
		"COX_JUDGE_RESULT_PATH=", // stdout only by default
		"COX_JUDGE_TIMEOUT_SECONDS=30",
	}
	return env
}

// runJudge runs the binary with the given base URL and result path and
// returns (exitCode, stdout).
func runJudge(t *testing.T, bin, baseURL, resultPath string, diffPath string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	if resultPath == "" {
		resultPath = filepath.Join(dir, "result.txt")
	}
	env := judgeEnv(t, diffPath)
	for i, e := range env {
		if e == "COX_JUDGE_BASE_URL=<set-by-test>" {
			env[i] = "COX_JUDGE_BASE_URL=" + baseURL
		}
		if e == "COX_JUDGE_RESULT_PATH=" {
			env[i] = "COX_JUDGE_RESULT_PATH=" + resultPath
		}
	}
	cmd := exec.Command(bin)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return code, string(out)
}

// writeDiff writes a diff file of n bytes (deterministic content).
func writeDiff(t *testing.T, n int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "diff.patch")
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = 'a' + byte(i%26)
	}
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatalf("write diff: %v", err)
	}
	return p
}

// resultFile reads and JSON-decodes the result file, asserting it is exactly
// one JSON line.
func resultFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("result file %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("result file %s is not a JSON object: %v (content: %q)", path, err, string(data))
	}
	return m
}

// jsonContent wraps s in a JSON string literal (the envelope's content
// field is a JSON-quoted string, so the verdict JSON inside it must be
// escaped — the literal's outer quotes plus the inner escaped quotes).
func jsonContent(s string) string {
	b, err := json.Marshal(s)
	if err != nil { // cannot happen (plain string)
		panic(err)
	}
	return string(b)
}

// proxyServer starts a fake model proxy that answers /v1/chat/completions
// with the given reply body (200).
func proxyServer(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestValidReplyWritesVerdictLine(t *testing.T) {
	bin := buildBinary(t, t.TempDir())
	content := `{"verdict":true,"score":87,"reason":"meets the goal"}`
	srv := proxyServer(t, `{"choices":[{"message":{"content":`+jsonContent(content)+`}}]}`)
	diff := writeDiff(t, 64)
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result.txt")
	code, stdout := runJudge(t, bin, srv.URL, resultPath, diff)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stdout: %s)", code, stdout)
	}
	m := resultFile(t, resultPath)
	if m["unavailable"] != nil {
		t.Fatalf("result has an unavailable field: %v", m)
	}
	verdict, ok := m["verdict"].(bool)
	if !ok || !verdict {
		t.Fatalf("verdict = %v, want true", m["verdict"])
	}
	score, ok := m["score"].(float64)
	if !ok || score != 87 {
		t.Fatalf("score = %v, want 87", m["score"])
	}
	if reason, _ := m["reason"].(string); reason != "meets the goal" {
		t.Fatalf("reason = %q, want \"meets the goal\"", m["reason"])
	}
	if truncated, ok := m["truncated"].(bool); !ok || truncated {
		t.Fatalf("truncated = %v, want false", m["truncated"])
	}
}

func TestProseReplyIsUnavailable(t *testing.T) {
	bin := buildBinary(t, t.TempDir())
	srv := proxyServer(t, `{"choices":[{"message":{"content":"I think this looks good overall."}}]}`)
	diff := writeDiff(t, 64)
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result.txt")
	code, _ := runJudge(t, bin, srv.URL, resultPath, diff)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (a failure line is the verdict channel, not the exit code)", code)
	}
	m := resultFile(t, resultPath)
	unavailable, ok := m["unavailable"].(string)
	if !ok || unavailable == "" {
		t.Fatalf("unavailable = %v, want a non-empty string", m["unavailable"])
	}
	if m["verdict"] != nil {
		t.Fatalf("result must not carry a verdict on a parse failure: %v", m)
	}
	if len([]rune(unavailable)) > unavailableCauseMax {
		t.Fatalf("unavailable cause is %d chars, at most %d", len([]rune(unavailable)), unavailableCauseMax)
	}
}

func TestHTTP500IsUnavailable(t *testing.T) {
	bin := buildBinary(t, t.TempDir())
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(s.Close)
	diff := writeDiff(t, 64)
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result.txt")
	code, _ := runJudge(t, bin, s.URL, resultPath, diff)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	m := resultFile(t, resultPath)
	unavailable, ok := m["unavailable"].(string)
	if !ok || unavailable == "" {
		t.Fatalf("unavailable = %v, want a non-empty string", m["unavailable"])
	}
}

func TestMissingBaseURLIsUnavailable(t *testing.T) {
	bin := buildBinary(t, t.TempDir())
	dir := t.TempDir()
	diff := writeDiff(t, 64)
	resultPath := filepath.Join(dir, "result.txt")
	cmd := exec.Command(bin)
	// No COX_JUDGE_BASE_URL at all (a fresh env, not judgeEnv).
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"COX_JUDGE_MODEL=test-model",
		"COX_JUDGE_MAX_TOKENS=2048",
		"COX_JUDGE_DIFF_PATH=" + diff,
		"COX_JUDGE_RESULT_PATH=" + resultPath,
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stdout: %s)", code, string(out))
	}
	m := resultFile(t, resultPath)
	unavailable, ok := m["unavailable"].(string)
	if !ok || unavailable == "" {
		t.Fatalf("unavailable = %v, want a non-empty string", m["unavailable"])
	}
}

func TestHugeDiffIsTruncated(t *testing.T) {
	bin := buildBinary(t, t.TempDir())
	content := `{"verdict":false,"score":12,"reason":"diff cut off"}`
	srv := proxyServer(t, `{"choices":[{"message":{"content":`+jsonContent(content)+`}}]}`)
	// 300 KiB > the 256 KiB cap.
	diff := writeDiff(t, 300*1024)
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result.txt")
	code, _ := runJudge(t, bin, srv.URL, resultPath, diff)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	m := resultFile(t, resultPath)
	if truncated, ok := m["truncated"].(bool); !ok || !truncated {
		t.Fatalf("truncated = %v, want true for a 300 KiB diff", m["truncated"])
	}
	// The verdict was still recorded (truncation marks the input, it does
	// not fail the call).
	if verdict, ok := m["verdict"].(bool); !ok || verdict {
		t.Fatalf("verdict = %v, want false", m["verdict"])
	}
}

// TestUnavailableLineReplacesProxyURL (I268): the unavailable cause must
// keep the ERROR, not the URL. A Go url.Error cause embeds the full proxy
// URL, which can use up the entire 120-rune budget and lose the actual
// error. The fix replaces the quoted URL with the bare token <proxy>
// BEFORE the cap.
func TestUnavailableLineReplacesProxyURL(t *testing.T) {
	const kindCause = `judge call: judge: call failed: Post ` +
		`"http://j1d-honest-proxy.samples.svc.cluster.local:8080/v1/chat/completions": ` +
		"context deadline exceeded (Client.Timeout exceeded while awaiting headers)"
	tests := []struct {
		name        string
		cause       string
		wantContain string
		wantAbsent  string
	}{
		{
			name:        "the exact kind-evidence cause keeps the error, not the URL",
			cause:       kindCause,
			wantContain: "context deadline exceeded",
			wantAbsent:  "http://",
		},
		{
			name:        "a cause without a URL is unchanged",
			cause:       `judge call: judge: call failed: connection refused`,
			wantContain: "connection refused",
		},
		{
			name:       "a 300-character cause without a URL is still capped at 120 runes",
			cause:      `judge call: judge: call failed: ` + strings.Repeat("x", 290),
			wantAbsent: `judge call: judge: call failed: ` + strings.Repeat("x", 281),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := unavailableLine(tc.cause, false)
			if tc.wantContain != "" && !strings.Contains(got, tc.wantContain) {
				t.Errorf("line must contain %q; got %s", tc.wantContain, got)
			}
			if tc.wantAbsent != "" && strings.Contains(got, tc.wantAbsent) {
				t.Errorf("line must NOT contain %q; got %s", tc.wantAbsent, got)
			}
			// The cap: the "unavailable" field must be at most 120 runes.
			var m map[string]any
			if err := json.Unmarshal([]byte(got), &m); err != nil {
				t.Fatalf("line is not valid JSON: %v; line: %s", err, got)
			}
			if u, ok := m["unavailable"].(string); ok {
				if r := []rune(u); len(r) > 120 {
					t.Errorf("unavailable field is %d runes; want at most 120; line: %s", len(r), got)
				}
			}
		})
	}
}
