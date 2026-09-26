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
	"path/filepath"
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

// run drives the model and writes the result file. It returns the result it
// wrote. This is the seam the tests observe.
func run(cfg runConfig) Result {
	client := &http.Client{Timeout: 60 * time.Second}

	messages := []map[string]string{
		{"role": "system", "content": "You are a coding agent inside a Kubernetes sandbox. Be concise."},
		{"role": "user", "content": cfg.Prompt},
	}

	answer, trace := driveModel(context.Background(), client, cfg.BaseURL, cfg.APIKey, cfg.Model, messages)

	res := Result{
		Status:    "success",
		Summary:   answer,
		ToolTrace: trace,
	}
	// R5's path is created here; R1 only needs it to exist.
	if err := writeResult(filepath.Join(cfg.Workspace, ".coxswain", "result.json"), res); err != nil {
		res.Status = "blocked"
		res.VerificationNotes = fmt.Sprintf("write result: %v", err)
	}
	return res
}

// driveModel sends the messages and returns the assistant's text answer plus a
// trace. Phase 0 returns on the first plain answer; tool calls are R3.
func driveModel(ctx context.Context, client *http.Client, baseURL, apiKey, model string, messages []map[string]string) (string, []string) {
	trace := []string{}
	reqBody, _ := json.Marshal(map[string]any{"model": model, "messages": messages})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return "", append(trace, "build request: "+err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", append(trace, "call model: "+err.Error())
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", append(trace, "decode response: "+err.Error())
	}
	if len(parsed.Choices) == 0 {
		return "", append(trace, "no choices in response")
	}
	return parsed.Choices[0].Message.Content, trace
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
