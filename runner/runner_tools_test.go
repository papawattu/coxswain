package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/papawattu/coxswain/runner/testhelper"
)

// I1: the runner must advertise its tools to the model in the OpenAI
// function-calling schema. Without a `tools` array a real OpenAI-compatible
// model will never emit a `shell` tool call — R3 only passed before because
// the fake emitted tool calls unconditionally.
//
// Seam: the fake model's request body (the `tools` array the runner sent). We
// assert the first request advertises a function named "shell" with a
// "command" string parameter.
func TestRunnerAdvertisesShellToolToModel(t *testing.T) {
	fake := testhelper.New()
	defer fake.Close()

	run(runConfig{
		Prompt:    "do something",
		Workspace: t.TempDir(),
		BaseURL:   fake.URL,
		Model:     "fake-model",
	})

	if len(fake.Requests) == 0 {
		t.Fatalf("expected at least one model request")
	}

	tools := fake.Requests[0].Tools
	if len(tools) == 0 {
		t.Fatalf("request 1 advertised no tools; a real model needs the shell tool to call it")
	}

	// Find the "shell" function and confirm it declares a "command" string param.
	var foundShell bool
	for _, raw := range tools {
		var tool struct {
			Type     string `json:"type"`
			Function struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Parameters  struct {
					Type                 string `json:"type"`
					Properties           map[string]struct {
						Type string `json:"type"`
					} `json:"properties"`
					Required []string `json:"required"`
				} `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &tool); err != nil {
			t.Fatalf("tool entry is not valid JSON: %v (%s)", err, string(raw))
		}
		if tool.Type != "function" {
			t.Fatalf("tool %s has type %q, want \"function\"", tool.Function.Name, tool.Type)
		}
		if tool.Function.Name != "shell" {
			continue
		}
		foundShell = true
		if tool.Function.Description == "" {
			t.Errorf("shell tool has no description")
		}
		if tool.Function.Parameters.Type != "object" {
			t.Errorf("shell tool parameters.type = %q, want \"object\"", tool.Function.Parameters.Type)
		}
		cmdProp, ok := tool.Function.Parameters.Properties["command"]
		if !ok {
			t.Fatalf("shell tool parameters have no \"command\" property: %s", string(raw))
		}
		if !strings.EqualFold(cmdProp.Type, "string") {
			t.Errorf("shell tool command param type = %q, want \"string\"", cmdProp.Type)
		}
		if len(tool.Function.Parameters.Required) == 0 {
			t.Errorf("shell tool parameters.required is empty; command should be required")
		}
	}
	if !foundShell {
		t.Fatalf("no tool named \"shell\" advertised; tools=%s", joinTools(tools))
	}
}

// joinTools renders the raw tool entries for a failure message.
func joinTools(raws []json.RawMessage) string {
	parts := make([]string, 0, len(raws))
	for _, r := range raws {
		parts = append(parts, string(r))
	}
	return strings.Join(parts, ", ")
}
