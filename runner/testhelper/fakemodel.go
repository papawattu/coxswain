// Test helper: a fake OpenAI-compatible model server. This is the one system
// boundary we mock (the model API). It is test-only; the runner under test
// never imports it — the test points the runner's base URL at this server.
package testhelper

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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
}

// ModelResponse is a single chat-completions response the fake will emit.
type ModelResponse struct {
	// Content is a plain assistant message (no tool call).
	Content string
	// ToolCall, when non-nil, makes the assistant message a tool call.
	ToolCall *ToolCall
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
	fm.Server = httptest.NewServer(http.HandlerFunc(fm.handle))
	return fm
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
	// Hardening (I1): a queued tool call is only emitted if the request
	// actually advertised a tool of that name. This keeps R3 honest — it can
	// no longer pass while the runner forgets to send its `tools` array.
	if resp.ToolCall != nil && !advertisesTool(rec, resp.ToolCall.Name) {
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
