// Package runner implements the Coxswain agent runner: it takes a prompt and
// a workspace, drives a model with a shell tool, and writes result.json
// (docs/CONTEXT.md: "Result file").
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// String constants (goconst). The runner advertises and reports these to the
// model and in result.json; the tests reference the same literals, so keeping
// them as consts avoids a 4-6 occurrence lint failure across the module.
const (
	systemPrompt   = "You are a coding agent inside a Kubernetes sandbox. Be concise."
	resultDirName  = ".coxswain"
	resultFileName = "result.json"

	// result.json status values (Phase 0 subset).
	statusSuccess = "success"
	statusBlocked = "blocked"

	// OpenAI-compatible chat field-name keys (map keys, repeated in the
	// request/response builders).
	jsonKeyRole      = "role"
	jsonKeyType      = "type"
	jsonKeyContent   = "content"
	jsonKeyModel     = "model"
	jsonKeyMessages  = "messages"
	jsonKeyTools     = "tools"
	jsonKeyFunction  = "function"
	jsonKeyName      = "name"
	jsonKeyID        = "id"
	jsonKeyObject    = "object"
	jsonKeyString    = "string"
	jsonKeyCommand   = "command"
	jsonKeyArguments = "arguments"

	// role values.
	jsonRoleSystem    = "system"
	jsonRoleUser      = "user"
	jsonRoleAssistant = "assistant"
	jsonRoleTool      = "tool"

	// tool type value.
	jsonToolFunction = "function"
)

// runConfig is the input to run. BaseURL points at an OpenAI-compatible
// /chat/completions endpoint; the test points it at a fake.
type runConfig struct {
	Prompt    string
	Workspace string
	BaseURL   string
	APIKey    string
	Model     string
}

// Result is the result file schema (Phase 0 subset of the Phase 1 contract).
type Result struct {
	Status            string   `json:"status"`
	Summary           string   `json:"summary"`
	FilesChanged      []string `json:"filesChanged,omitempty"`
	VerificationNotes string   `json:"verificationNotes,omitempty"`
	NextIterationPlan string   `json:"nextIterationPlan,omitempty"`
	Lessons           []string `json:"lessons,omitempty"`
	ToolTrace         []string `json:"toolTrace,omitempty"`
}

// assistantMessage is the parsed assistant turn from the model, including any
// tool calls.
type assistantMessage struct {
	Content   string     `json:"content"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Function fnCall `json:"function"`
}

type fnCall struct {
	Arguments string `json:"arguments"` // raw JSON string, e.g. {"command":"..."}
}

// run drives the model and writes the result file. It returns the result it
// wrote. This is the seam the tests observe.
func run(cfg runConfig) Result {
	client := &http.Client{Timeout: 60 * time.Second}

	messages := []map[string]any{
		{jsonKeyRole: jsonRoleSystem, jsonKeyContent: systemPrompt},
		{jsonKeyRole: jsonRoleUser, jsonKeyContent: cfg.Prompt},
	}

	answer, trace, modelErr := driveModel(
		context.Background(), client, cfg.BaseURL, cfg.APIKey, cfg.Model, cfg.Workspace, messages,
	)

	res := Result{
		Status:    statusSuccess,
		Summary:   answer,
		ToolTrace: trace,
	}
	if modelErr != "" {
		res.Status = statusBlocked
		res.VerificationNotes = modelErr
	}
	// R5's path is created here; R1 only needs it to exist.
	if err := writeResult(filepath.Join(cfg.Workspace, resultDirName, resultFileName), res); err != nil {
		res.Status = statusBlocked
		res.VerificationNotes = fmt.Sprintf("write result: %v", err)
	}
	return res
}

// callModel posts one chat-completions request and returns the assistant
// message (with any tool calls). The request always advertises the runner's
// tools (I1) so a real model can emit a shell tool call.
func callModel(
	ctx context.Context, client *http.Client, baseURL, apiKey, model string, messages []map[string]any,
) (assistantMessage, error) {
	reqBody, err := json.Marshal(map[string]any{
		jsonKeyModel:    model,
		jsonKeyMessages: messages,
		jsonKeyTools:    shellToolSchema(),
	})
	if err != nil {
		return assistantMessage{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return assistantMessage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return assistantMessage{}, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			_ = cerr
		}
	}()

	raw, _ := io.ReadAll(resp.Body)
	var parsed struct {
		Choices []struct {
			Message assistantMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return assistantMessage{}, fmt.Errorf("decode response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return assistantMessage{}, fmt.Errorf("no choices in response")
	}
	return parsed.Choices[0].Message, nil
}

// shellToolSchema is the OpenAI function-calling schema for the runner's shell
// tool. It is advertised on every request so the model knows it can call
// shell({command: string}).
func shellToolSchema() []map[string]any {
	return []map[string]any{
		{
			jsonKeyType: jsonToolFunction,
			jsonKeyFunction: map[string]any{
				jsonKeyName:   "shell",
				"description": "Run a shell command in the workspace and return its combined output.",
				"parameters": map[string]any{
					jsonKeyType: jsonKeyObject,
					"properties": map[string]any{
						jsonKeyCommand: map[string]any{jsonKeyType: jsonKeyString, "description": "The shell command to run."},
					},
					"required": []string{jsonKeyCommand},
				},
			},
		},
	}
}

// assistantMessageFor serializes an assistant turn (with tool calls) back into
// the message list for the next request.
func assistantMessageFor(m assistantMessage) map[string]any {
	out := map[string]any{jsonKeyRole: jsonRoleAssistant, jsonKeyContent: m.Content}
	if len(m.ToolCalls) > 0 {
		tcs := make([]map[string]any, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			tcs = append(tcs, map[string]any{
				jsonKeyID:   tc.ID,
				jsonKeyType: jsonToolFunction,
				jsonKeyFunction: map[string]any{
					jsonKeyName:      "shell",
					jsonKeyArguments: tc.Function.Arguments,
				},
			})
		}
		out["tool_calls"] = tcs
	}
	return out
}

// execShell runs the shell command named in a tool call's arguments JSON in
// the workspace and returns combined output.
func execShell(workspace, argsJSON string) (string, error) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("bad tool args: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", args.Command)
	cmd.Dir = workspace
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out) + " (exit: " + err.Error() + ")", nil
	}
	return string(out), nil
}

func itoa(n int) string { return strconv.Itoa(n) }

// driveModel sends the messages and returns the assistant's text answer plus a
// trace. It loops on tool calls: when the model returns shell tool calls, each
// is executed in the workspace and the combined output is fed back as a tool
// message, until the model returns a plain answer (or maxSteps is reached).
func driveModel(
	ctx context.Context, client *http.Client, baseURL, apiKey, model, workspace string, messages []map[string]any,
) (string, []string, string) {
	trace := []string{}
	const maxSteps = 5

	for step := range maxSteps {
		msg, err := callModel(ctx, client, baseURL, apiKey, model, messages)
		if err != nil {
			return "", append(trace, "call model: "+err.Error()), err.Error()
		}

		// No tool calls → the model is done; its content is the final answer.
		if len(msg.ToolCalls) == 0 {
			return msg.Content, trace, ""
		}

		// Record the assistant turn, then execute each tool call and feed the
		// results back.
		messages = append(messages, assistantMessageFor(msg))
		for _, tc := range msg.ToolCalls {
			out, err := execShell(workspace, tc.Function.Arguments)
			if err != nil {
				out = err.Error()
			}
			trace = append(trace, "step "+itoa(step)+": "+out)
			messages = append(messages, map[string]any{
				jsonKeyRole:    jsonRoleTool,
				"tool_call_id": tc.ID,
				jsonKeyContent: out,
			})
		}
	}
	return "", append(trace, "maxSteps reached without a final answer"), "maxSteps reached without a final answer"
}

// writeResult marshals r to path, creating parent directories.
func writeResult(path string, r Result) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
