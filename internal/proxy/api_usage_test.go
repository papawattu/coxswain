package proxy

// D148 (docs/PLAN-AGENT-ADAPTERS.md O2): the proxy meters the two APIs the
// Claude Code and Codex adapters speak, Anthropic Messages and OpenAI
// Responses, and sends the credential in the header each API expects. The
// testdata files are REAL vLLM output, captured byte for byte from the lab
// endpoint (vLLM 0.27, qwen3.8-27b, 11-23) with curl; AGENTS.md: a feature that
// consumes a third-party component's output is proven with that output.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseUsageRealMessagesAndResponses(t *testing.T) {
	for _, name := range []string{"vllm-messages.json", "vllm-responses.json"} {
		p, c, ok := parseUsage(readTestdata(t, name))
		if !ok || p != 14 || c != 2 {
			t.Errorf("%s: usage = (%d, %d, %v); want (14, 2, true) (input_tokens 14, output_tokens 2)", name, p, c, ok)
		}
	}
}

func TestParseStreamUsageRealMessagesAndResponses(t *testing.T) {
	// The Messages stream carries usage twice (message_start and a cumulative
	// message_delta, both with input_tokens 14): summing would report 28.
	for _, name := range []string{"vllm-messages-stream.sse", "vllm-responses-stream.sse"} {
		p, c, ok := parseStreamUsage(readTestdata(t, name))
		if !ok || p != 14 || c != 2 {
			t.Errorf("%s: stream usage = (%d, %d, %v); want (14, 2, true)", name, p, c, ok)
		}
	}
}

func TestParseUsageCacheTokensCountAsInput(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":10,"cache_creation_input_tokens":3,"cache_read_input_tokens":7,"output_tokens":4}}`)
	if p, c, ok := parseUsage(body); !ok || p != 20 || c != 4 {
		t.Fatalf("usage = (%d, %d, %v); want (20, 4, true): cache tokens are input the model processed", p, c, ok)
	}
}

// A streamed Messages call through the proxy reaches the meter: the reading
// moves by the stream's real usage.
func TestMeteringMessagesStreamThroughTheProxy(t *testing.T) {
	sse := readTestdata(t, "vllm-messages-stream.sse")
	up := startFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(sse)
	})
	m := newTestMeter(t, "qwen", time.Now())
	p, err := NewMetered(Config{Upstream: mustURL(t, up.URL), Model: "qwen", Meter: m})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		bytes.NewBufferString(`{"model":"qwen","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"Say OK"}]}`))
	w := httptest.NewRecorder()
	p.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), sse) {
		t.Fatalf("the agent must get the stream unchanged: code %d", w.Code)
	}
	r := m.Reading()
	if r.PromptTokens != 14 || r.CompletionTokens != 2 || r.Requests != 1 || r.UnmeteredRequests != 0 {
		t.Fatalf("reading = %+v; want 14 prompt, 2 completion, 1 request, 0 unmetered", r)
	}
}

func TestProxyCredentialHeaderPerAPI(t *testing.T) {
	cases := []struct {
		name, path, cred string
		want             map[string]string // header -> value; "" means absent
	}{
		{"messages takes x-api-key", "/v1/messages", "real-key",
			map[string]string{"X-Api-Key": "real-key", "Authorization": "", "Api-Key": ""}},
		{"count_tokens is a messages path", "/v1/messages/count_tokens", "real-key",
			map[string]string{"X-Api-Key": "real-key", "Authorization": ""}},
		{"responses takes a bearer token", "/v1/responses", "real-key",
			map[string]string{"Authorization": "Bearer real-key", "X-Api-Key": "", "Api-Key": ""}},
		{"chat completions takes a bearer token", "/v1/chat/completions", "real-key",
			map[string]string{"Authorization": "Bearer real-key", "X-Api-Key": ""}},
		{"no credential: the agent's dummy keys still never reach the upstream", "/v1/messages", "",
			map[string]string{"Authorization": "", "X-Api-Key": "", "Api-Key": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen http.Header
			up := startFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				seen = r.Header.Clone()
				_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
			})
			p, err := NewMetered(Config{Upstream: mustURL(t, up.URL), Credential: tc.cred})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewBufferString(`{"model":"x"}`))
			// What the adapters' CLIs send: a dummy key in every header they know.
			req.Header.Set("Authorization", "Bearer unused")
			req.Header.Set("X-Api-Key", "unused")
			req.Header.Set("Api-Key", "unused")
			p.Handler().ServeHTTP(httptest.NewRecorder(), req)
			for h, want := range tc.want {
				if got := seen.Get(h); got != want {
					t.Errorf("upstream %s = %q; want %q", h, got, want)
				}
			}
		})
	}
}
