package runner

import (
	"encoding/json"
	"testing"
)

// The OpenAI/vLLM request schema requires each echoed assistant tool_call to
// carry "type": "function" (plus its id and function{name, arguments-as-
// string}), and each tool result to be a role "tool" message with
// tool_call_id. vLLM 400s the SECOND request of a tool round ("missing
// ChatCompletionMessageFunctionToolCallParam.type", verified in kind
// 2026-10-02). This pins the round-trip shape the runner marshals: a parsed
// assistant tool call echoed back into history must carry type=function, and
// the tool result message must carry role=tool + tool_call_id.
func TestToolCallRoundTripMarshalsTypeFunction(t *testing.T) {
	// (1) the response decode: a vLLM/OpenAI tool-call response with NO type
	// field on the tool call (the response schema does not require it).
	response := []byte(`{
		"choices": [{
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [{
					"id": "call_abc123",
					"function": {
						"name": "shell",
						"arguments": "{\"command\":\"go test ./...\"}"
					}
				}]
			}
		}]
	}`)
	var parsed struct {
		Choices []struct {
			Message assistantMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(response, &parsed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(parsed.Choices) != 1 || len(parsed.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("want one tool call, got %+v", parsed)
	}
	msg := parsed.Choices[0].Message
	tc := msg.ToolCalls[0]
	if tc.ID != "call_abc123" {
		t.Fatalf("tool call id = %q, want call_abc123", tc.ID)
	}
	if tc.Function.Name != toolNameShell {
		t.Fatalf("tool call name = %q, want %q", tc.Function.Name, toolNameShell)
	}
	if tc.Function.Arguments != `{"command":"go test ./..."}` {
		t.Fatalf("tool call arguments = %q (must stay a raw JSON string)", tc.Function.Arguments)
	}

	// (2) the echo: what the runner appends to the history for the next
	// request. It must carry type=function — exactly what the driveModel echo
	// does.
	echoed := chatMessage{Role: jsonRoleAssistant, Content: msg.Content, ToolCalls: msg.ToolCalls}
	for i := range echoed.ToolCalls {
		echoed.ToolCalls[i].Type = jsonToolFunction
	}
	raw, err := json.Marshal(echoed)
	if err != nil {
		t.Fatalf("marshal echoed assistant: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal echoed assistant: %v", err)
	}
	if m["role"] != "assistant" {
		t.Fatalf("echoed role = %v", m["role"])
	}
	tcs, _ := m["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls len = %d, want 1; raw=%s", len(tcs), raw)
	}
	tm := tcs[0].(map[string]any)
	if tm["type"] != "function" {
		t.Fatalf("tool_calls[0].type = %v, want \"function\" (vLLM 400s without it); raw=%s", tm["type"], raw)
	}
	if tm["id"] != "call_abc123" {
		t.Fatalf("tool_calls[0].id = %v", tm["id"])
	}
	fn, _ := tm["function"].(map[string]any)
	if fn["name"] != toolNameShell {
		t.Fatalf("function.name = %v", fn["name"])
	}
	if fn["arguments"] != `{"command":"go test ./..."}` {
		t.Fatalf("function.arguments = %v (must be the raw JSON string)", fn["arguments"])
	}

	// (3) the tool result message: role=tool + tool_call_id (what driveModel
	// appends after executing the shell tool).
	result := chatMessage{Role: jsonRoleTool, ToolCallID: tc.ID, Content: "ok"}
	rawResult, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal tool result: %v", err)
	}
	var rm map[string]any
	if err := json.Unmarshal(rawResult, &rm); err != nil {
		t.Fatalf("unmarshal tool result: %v", err)
	}
	if rm["role"] != "tool" {
		t.Fatalf("result role = %v, want tool", rm["role"])
	}
	if rm["tool_call_id"] != "call_abc123" {
		t.Fatalf("result tool_call_id = %v, want call_abc123", rm["tool_call_id"])
	}
}
