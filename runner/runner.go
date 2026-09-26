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
	ID        string `json:"id"`
	Function  fnCall `json:"function"`
}

type fnCall struct {
	Arguments string `json:"arguments"` // raw JSON string, e.g. {"command":"..."}
}

// run drives the model and writes the result file. It returns the result it
// wrote. This is the seam the tests observe.
func run(cfg runConfig) Result {
	client := &http.Client{Timeout: 60 * time.Second}

	messages := []map[string]any{
		{"role": "system", "content": "You are a coding agent inside a Kubernetes sandbox. Be concise."},
		{"role": "user", "content": cfg.Prompt},
	}

	answer, trace, modelErr := driveModel(context.Background(), client, cfg.BaseURL, cfg.APIKey, cfg.Model, cfg.Workspace, messages)

	res := Result{
		Status:    "success",
		Summary:   answer,
		ToolTrace: trace,
	}
	if modelErr != "" {
		res.Status = "blocked"
		res.VerificationNotes = modelErr
	}
	// R5's path is created here; R1 only needs it to exist.
	if err := writeResult(filepath.Join(cfg.Workspace, ".coxswain", "result.json"), res); err != nil {
		res.Status = "blocked"
		res.VerificationNotes = fmt.Sprintf("write result: %v", err)
	}
	return res
}

// callModel posts one chat-completions request and returns the assistant
// message (with any tool calls).
func callModel(ctx context.Context, client *http.Client, baseURL, apiKey, model string, messages []map[string]any) (assistantMessage, error) {
	reqBody, err := json.Marshal(map[string]any{"model": model, "messages": messages})
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
	defer resp.Body.Close()

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

// assistantMessageFor serializes an assistant turn (with tool calls) back into
// the message list for the next request.
func assistantMessageFor(m assistantMessage) map[string]any {
	out := map[string]any{"role": "assistant", "content": m.Content}
	if len(m.ToolCalls) > 0 {
		tcs := make([]map[string]any, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			tcs = append(tcs, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      "shell",
					"arguments": tc.Function.Arguments,
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
func driveModel(ctx context.Context, client *http.Client, baseURL, apiKey, model, workspace string, messages []map[string]any) (string, []string, string) {
	trace := []string{}
	const maxSteps = 5

	for step := 0; step < maxSteps; step++ {
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
				"role": "tool", "tool_call_id": tc.ID, "content": out,
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
