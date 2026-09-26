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
	"syscall"
	"time"
	"unicode/utf8"
)

// String constants. systemPrompt/resultDirName/resultFileName are the runner's
// fixed strings; the jsonRole*/jsonToolFunction consts are protocol *values*
// (OpenAI role and tool-type strings) that the tests legitimately assert
// against. (I7: the wire format is now built from typed structs with json tags,
// so the jsonKey* field-name consts are gone.)
const (
	systemPrompt   = "You are a coding agent inside a Kubernetes sandbox. Be concise."
	resultDirName  = ".coxswain"
	resultFileName = "result.json"

	// result.json status values (Phase 0 subset).
	statusSuccess = "success"
	statusBlocked = "blocked"

	// OpenAI chat role values (asserted by tests).
	jsonRoleSystem    = "system"
	jsonRoleUser      = "user"
	jsonRoleAssistant = "assistant"
	jsonRoleTool      = "tool"

	// OpenAI tool type value (asserted by the I1 tools test).
	jsonToolFunction = "function"

	// I5: cap on shell output fed back to the model, so a chatty command can't
	// blow the context or the result. 16 KiB.
	maxToolOutputBytes = 16 * 1024

	// I5: default max model steps (tool-call rounds). 5 (the Phase 0 value) was
	// too low for a real agent; the test default is overridden via runConfig.
	defaultMaxSteps = 25

	// I5: the single tool the runner advertises and executes. A const so the
	// tests reference the same name instead of a repeated literal (goconst).
	toolNameShell = "shell"

	// I5: shell command timeout (I10: injectable via runConfig.ShellTimeout;
	// this is the default).
	defaultShellTimeout = 60 * time.Second

	// I11: default per-model-request timeout (was a hard 60s, too short for a
	// local 27B-class model or a long-context request). Overridable via
	// runConfig.ModelTimeout.
	defaultModelTimeout = 10 * time.Minute

	// I10: how long Wait tolerates pipes held open by straggler processes after
	// the command's main process exits, before it stops waiting on them.
	shellWaitDelay = 2 * time.Second
)

// runConfig is the input to run. BaseURL points at an OpenAI-compatible
// /chat/completions endpoint; the test points it at a fake.
type runConfig struct {
	Prompt    string
	Workspace string
	BaseURL   string
	APIKey    string
	Model     string
	// MaxSteps caps the number of model rounds (tool-call loops). I5: was a
	// hard const of 5; now configurable. Zero uses defaultMaxSteps.
	MaxSteps int
	// ShellTimeout bounds a single shell tool call (I10: was a hard const 60s).
	// Zero uses defaultShellTimeout. The whole process group is killed on
	// timeout, so a command that backgrounds a child can't hang the runner.
	ShellTimeout time.Duration
	// ModelTimeout bounds a single model HTTP request (I11: was a hard const
	// 60s, too short for a local 27B-class model / long-context request). Zero
	// uses defaultModelTimeout. The per-request context is kept so a run
	// deadline can cancel it later.
	ModelTimeout time.Duration
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

// ---------------------------------------------------------------------------
// OpenAI-compatible chat-completions wire types (I7).
//
// These are the typed request/response shapes. The request is built from
// these (no map[string]any, no key-typo risk); the response is decoded into
// them. Field names live in the json tags.
// ---------------------------------------------------------------------------

// chatRequest is one POST /chat/completions body.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []toolDef     `json:"tools,omitempty"`
}

// chatMessage is one message in the conversation. Role is one of
// system/user/assistant/tool. Assistant messages may carry ToolCalls; tool
// messages reference the tool call they answer via ToolCallID.
type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// toolDef is one advertised tool (function-calling schema).
type toolDef struct {
	Type     string `json:"type"` // "function"
	Function fnDef  `json:"function"`
}

// fnDef is the function part of a tool definition.
type fnDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  paramSchema `json:"parameters"`
}

// paramSchema is a JSON-Schema object describing the function's arguments.
type paramSchema struct {
	Type       string                `json:"type"` // "object"
	Properties map[string]propSchema `json:"properties"`
	Required   []string              `json:"required"`
}

// propSchema is one property in the function's parameter schema.
type propSchema struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

// assistantMessage is the parsed assistant turn from the model. It is the same
// shape as chatMessage for the assistant role; kept as its own type for the
// response decode.
type assistantMessage struct {
	Content   string     `json:"content"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

// toolCall is a model-requested function call.
type toolCall struct {
	ID       string `json:"id"`
	Function fnCall `json:"function"`
}

// fnCall is the function part of a tool call (name + raw arguments JSON).
type fnCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON string, e.g. {"command":"..."}
}

// shellToolSchema is the OpenAI function-calling schema for the runner's shell
// tool (I7: typed, built from the wire structs).
func shellToolSchema() []toolDef {
	return []toolDef{
		{
			Type: jsonToolFunction,
			Function: fnDef{
				Name:        toolNameShell,
				Description: "Run a shell command in the workspace and return its combined output.",
				Parameters: paramSchema{
					Type: "object",
					Properties: map[string]propSchema{
						"command": {Type: "string", Description: "The shell command to run."},
					},
					Required: []string{"command"},
				},
			},
		},
	}
}

// run drives the model and writes the result file. It returns the result it
// wrote. This is the seam the tests observe.
func run(cfg runConfig) Result {
	modelTimeout := cfg.ModelTimeout
	if modelTimeout <= 0 {
		modelTimeout = defaultModelTimeout
	}
	client := &http.Client{Timeout: modelTimeout}

	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}

	shellTimeout := cfg.ShellTimeout
	if shellTimeout <= 0 {
		shellTimeout = defaultShellTimeout
	}

	messages := []chatMessage{
		{Role: jsonRoleSystem, Content: systemPrompt},
		{Role: jsonRoleUser, Content: cfg.Prompt},
	}

	answer, trace, modelErr := driveModel(
		context.Background(), client, cfg.BaseURL, cfg.APIKey, cfg.Model, cfg.Workspace,
		messages, maxSteps, shellTimeout, modelTimeout,
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
	ctx context.Context, client *http.Client, baseURL, apiKey, model string, messages []chatMessage,
) (assistantMessage, error) {
	body := chatRequest{
		Model:    model,
		Messages: messages,
		Tools:    shellToolSchema(),
	}
	reqBody, err := json.Marshal(body)
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
	defer func() { _ = resp.Body.Close() }()

	// I5: check the HTTP status before decoding. A 401/403/500 was previously
	// misreported as "no choices in response" (the body is an error, not a
	// completion).
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return assistantMessage{}, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Include the server's error body (truncated) so the cause is visible
		// in the result's verificationNotes. I12: cut on a rune boundary.
		errBody := string(raw)
		if len(errBody) > 256 {
			errBody = cutRunePrefix(errBody, 256) + "…"
		}
		return assistantMessage{}, fmt.Errorf("model returned HTTP %d: %s", resp.StatusCode, errBody)
	}

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

// execShell runs the shell command named in a tool call's arguments JSON in
// the workspace and returns combined output. I5: the output is capped to
// maxToolOutputBytes before being fed back to the model. I10: the command runs
// in its own process group; on timeout (or after the command returns) the whole
// group is killed so a backgrounded child can't hold the pipe open and hang the
// runner past the timeout.
func execShell(workspace, argsJSON string, timeout time.Duration) (string, error) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("bad tool args: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", args.Command)
	cmd.Dir = workspace
	// I10: run in its own process group so we can kill the whole group (the
	// command plus any children it spawned) on timeout, not just `sh`.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Kill the entire process group when the context is done (timeout) —
	// CommandContext's default only signals the direct child.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// I10: stop waiting on pipes held by straggler processes once the main
	// process has exited and been waited on.
	cmd.WaitDelay = shellWaitDelay
	// Belt-and-suspenders: also kill the group if Wait returns early (the
	// default WaitDelay path) before Cancel fires. On both success and failure
	// the group must not outlive the tool call, or a backgrounded process
	// leaks across steps.
	defer func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()

	out, err := cmd.CombinedOutput()
	if err != nil {
		return truncateToolOutput(string(out) + " (exit: " + err.Error() + ")"), nil
	}
	return truncateToolOutput(string(out)), nil
}

// truncateToolOutput caps s to maxToolOutputBytes, keeping the head and the
// tail (the useful part of a build/test is usually at the end) with a marker
// between them (I5, refined I12: head+tail, not head-only). Cuts are made on
// UTF-8 rune boundaries so a multi-byte character is not split.
func truncateToolOutput(s string) string {
	if len(s) <= maxToolOutputBytes {
		return s
	}
	const headBytes = 4096
	tailBytes := maxToolOutputBytes - headBytes // keep head + tail within the cap
	elided := max(0, len(s)-headBytes-tailBytes)
	head := cutRunePrefix(s, headBytes)
	tail := cutRuneSuffix(s, tailBytes)
	return head + fmt.Sprintf("… (%d bytes elided) …\n", elided) + tail
}

// cutRunePrefix returns the first n bytes of s, backing off to the previous
// UTF-8 rune start if s[n] is a continuation byte (so a rune is not split).
func cutRunePrefix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// cutRuneSuffix returns the last n bytes of s, advancing to the next UTF-8 rune
// start if the first byte of the slice is a continuation byte.
func cutRuneSuffix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// knownTools is the set of tool names the runner will execute (I5); an unknown
// name is rejected rather than silently run as a shell command. I12: a
// package-level map instead of rebuilding it on every driveModel call.
var knownTools = map[string]bool{toolNameShell: true}

func knownToolNames() map[string]bool { return knownTools }

// driveModel sends the messages and returns the assistant's text answer plus a
// trace. It loops on tool calls: when the model returns tool calls, each known
// one is executed in the workspace and the output fed back as a tool message,
// until the model returns a plain answer (or maxSteps is reached). I5:
// unknown tool names are rejected (not executed), and shell output is
// truncated before being fed back.
func driveModel(
	ctx context.Context, client *http.Client, baseURL, apiKey, model, workspace string,
	messages []chatMessage, maxSteps int, shellTimeout, modelTimeout time.Duration,
) (string, []string, string) {
	known := knownToolNames()
	trace := []string{}

	for step := range maxSteps {
		// I11: bound each model request with modelTimeout, derived from the
		// caller's ctx so a run deadline can still cancel it.
		reqCtx, cancel := context.WithTimeout(ctx, modelTimeout)
		msg, err := callModel(reqCtx, client, baseURL, apiKey, model, messages)
		cancel()
		if err != nil {
			return "", append(trace, "call model: "+err.Error()), err.Error()
		}

		// No tool calls → the model is done; its content is the final answer.
		if len(msg.ToolCalls) == 0 {
			return msg.Content, trace, ""
		}

		// Record the assistant turn (with its tool calls) for the next request.
		messages = append(messages, chatMessage{
			Role:      jsonRoleAssistant,
			Content:   msg.Content,
			ToolCalls: msg.ToolCalls,
		})
		for _, tc := range msg.ToolCalls {
			if !known[tc.Function.Name] {
				// I5: reject an unknown tool instead of executing it as a shell
				// command. Feed the model a tool error so it can recover.
				msg := "unknown tool: " + tc.Function.Name
				trace = append(trace, "step "+strconv.Itoa(step)+": "+msg)
				messages = append(messages, chatMessage{
					Role:       jsonRoleTool,
					ToolCallID: tc.ID,
					Content:    msg,
				})
				continue
			}
			out, err := execShell(workspace, tc.Function.Arguments, shellTimeout)
			if err != nil {
				out = err.Error()
			}
			trace = append(trace, "step "+strconv.Itoa(step)+": "+out)
			messages = append(messages, chatMessage{
				Role:       jsonRoleTool,
				ToolCallID: tc.ID,
				Content:    out,
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
