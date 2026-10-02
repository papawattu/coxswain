// Test helper: a fake OpenAI-compatible model server. This is the one system
// boundary we mock (the model API). It is test-only; the runner under test
// never imports it — the test points the runner's base URL at this server.
package testhelper

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// Received is the last request body the fake server saw, decoded.
type Received struct {
	Model    string            `json:"model"`
	Messages []ReceivedMessage `json:"messages"`
	Tools    []json.RawMessage `json:"tools"`
}

// ToolResultMessages returns the messages in a request that are tool results
// (role "tool"), so a test can assert the runner fed a tool call's output back.
func (r Received) ToolResultMessages() []ReceivedMessage {
	var out []ReceivedMessage
	for _, m := range r.Messages {
		if m.Role == "tool" {
			out = append(out, m)
		}
	}
	return out
}

type ReceivedMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// FakeModel is a fake /chat/completions endpoint. The next response is taken
// from the Responses queue; when the queue is empty it returns a plain text
// final answer.
type FakeModel struct {
	*httptest.Server
	Responses []ModelResponse
	mu        sync.Mutex
	Requests  []Received
	// customHandler, when non-nil, replaces the default handler (tests use it
	// to observe raw request bodies).
	customHandler http.HandlerFunc
}

// SetHandler installs a custom request handler (nil restores the default).
func (fm *FakeModel) SetHandler(h http.HandlerFunc) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	fm.customHandler = h
}

// RecordHandler wraps the default ordered-responses handler so every raw
// request body is appended to bodies before the normal handling. It is
// concurrency-safe (all calls are serialized by the runner's sequential
// model calls).
func (fm *FakeModel) RecordHandler(bodies *[][]byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error":"read body"}`, http.StatusBadRequest)
			return
		}
		fm.mu.Lock()
		*bodies = append(*bodies, append([]byte(nil), body...))
		fm.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		fm.handle(w, r)
	}
}

// ModelResponse is a single chat-completions response the fake will emit.
type ModelResponse struct {
	// Content is a plain assistant message (no tool call).
	Content string
	// ToolCall, when non-nil, makes the assistant message a tool call.
	ToolCall *ToolCall
	// I5: when StatusCode != 0, the fake returns this HTTP status with RawBody
	// (as the body) instead of encoding a normal completion — to test the
	// runner's non-200 handling.
	StatusCode int
	RawBody    string
	// I5: when true, a queued ToolCall is emitted even if the request did not
	// advertise a tool of that name (lets a test exercise unknown-tool
	// rejection without the I1 advertise guard).
	SkipAdvertiseCheck bool
	// I11: delay the response by this duration (lets a test exercise the
	// runner's model timeout: set a small ModelTimeout and a larger Delay to
	// force a timeout).
	Delay time.Duration
}

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON string, e.g. {"command":"..."}
}

// New starts a fake model server that emits the given responses in order, then
// a default final answer "done" forever after.
func New(responses ...ModelResponse) *FakeModel {
	fm := &FakeModel{Responses: responses}
	fm.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fm.handler()(w, r)
	}))
	return fm
}

// handler returns the current request handler. Default is the ordered-
// responses handler (fm.handle); a test may install a wrapper that records
// the raw body and then calls fm.handle.
func (fm *FakeModel) handler() http.HandlerFunc {
	fm.mu.Lock()
	h := fm.customHandler
	fm.mu.Unlock()
	if h != nil {
		return h
	}
	return fm.handle
}

// DefaultHandler returns the fake's default ordered-responses handler, so
// tests can wrap it (e.g. to 404 every path but /v1/chat/completions) and
// delegate to it. Concurrency-safe.
func (fm *FakeModel) DefaultHandler() http.HandlerFunc {
	return fm.handle
}

func (fm *FakeModel) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/chat/completions" {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var rec Received
	_ = json.Unmarshal(body, &rec)
	fm.mu.Lock()
	fm.Requests = append(fm.Requests, rec)
	idx := len(fm.Requests) - 1
	resp := fm.defaultResponse()
	if idx < len(fm.Responses) {
		resp = fm.Responses[idx]
	}
	if resp.Delay > 0 {
		time.Sleep(resp.Delay)
	}
	// I5: emit a raw HTTP error if this response is one.
	if resp.StatusCode != 0 {
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write([]byte(resp.RawBody))
		return
	}
	// Hardening (I1): a queued tool call is only emitted if the request
	// actually advertised a tool of that name. This keeps R3 honest — it can
	// no longer pass while the runner forgets to send its `tools` array.
	if resp.ToolCall != nil && !resp.SkipAdvertiseCheck && !advertisesTool(rec, resp.ToolCall.Name) {
		resp = ModelResponse{Content: "no shell tool advertised; cannot call it"}
	}
	fm.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(encodeResponse(resp))
}

// advertisesTool reports whether the request's tools array declares a function
// named `name` (OpenAI function-calling schema).
func advertisesTool(rec Received, name string) bool {
	for _, raw := range rec.Tools {
		var tool struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &tool) == nil && tool.Function.Name == name {
			return true
		}
	}
	return false
}

// defaultResponse is emitted once the queued responses are exhausted.
func (fm *FakeModel) defaultResponse() ModelResponse {
	return ModelResponse{Content: "done"}
}

func encodeResponse(r ModelResponse) map[string]any {
	msg := map[string]any{"role": "assistant", "content": r.Content}
	if r.ToolCall != nil {
		msg["tool_calls"] = []map[string]any{{
			"id":   r.ToolCall.ID,
			"type": "function",
			"function": map[string]any{
				"name":      r.ToolCall.Name,
				"arguments": r.ToolCall.Arguments,
			},
		}}
	}
	return map[string]any{
		"choices": []map[string]any{{"message": msg}},
	}
}
