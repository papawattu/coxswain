package runner

// CLIEngine (agent adapters, S1/S2; docs/PLAN-AGENT-ADAPTERS.md): one phase's
// agent work done by an external coding-agent CLI (Claude Code, Codex, pi,
// Hermes, opencode), run non-interactively in the workspace. The CLI talks to
// the Loop's model proxy (COX_MODEL_BASE_URL) and holds no credential: the
// spec's config gives it a dummy key, and the proxy injects the real one
// (ADR-0006).
//
// The spec is a JSON file baked into the agent image
// (/etc/coxswain/engine.json). It is operator-trusted image content, not Loop
// input: a Loop author cannot change it (D147: only operator-listed images run
// the contract).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DefaultEngineSpecPath is where an agent image carries its engine spec.
const DefaultEngineSpecPath = "/etc/coxswain/engine.json"

const (
	// defaultEngineTimeout bounds one CLI run (a whole phase).
	defaultEngineTimeout = 45 * time.Minute
	// maxEngineStdout caps the captured stdout (the answer); the rest is
	// dropped, not buffered.
	maxEngineStdout = 1 << 20
	// maxEngineStderrTail is how much of the CLI's stderr the blocked reason
	// and the trace keep (the last bytes, where the error usually is).
	maxEngineStderrTail = 4 << 10
	// maxPromptArgBytes keeps the prompt under Linux's single-argument limit
	// (MAX_ARG_STRLEN, 128 KiB) with margin.
	maxPromptArgBytes = 120 << 10
	// engineKillGrace is how long the CLI's process group gets between
	// SIGTERM and SIGKILL when the phase is cancelled or times out.
	engineKillGrace = 5 * time.Second
)

// EngineSpec is the agent image's engine.json.
type EngineSpec struct {
	// Name names the agent in traces and blocked reasons ("pi", "codex", …).
	Name string `json:"name"`
	// Command is the argv. Each element is expanded with ${VAR} (see
	// engineVars); ${COX_PROMPT} is the phase prompt and is allowed ONLY in
	// Command, never in Files.
	Command []string `json:"command"`
	// Env is added to the CLI's environment (values expanded like Files).
	Env map[string]string `json:"env,omitempty"`
	// Files are rendered before the CLI starts (its config: provider base
	// URL, model, dummy key, telemetry and updates off). Paths and content
	// are expanded; a path must be absolute and under $HOME or /tmp.
	Files []EngineFile `json:"files,omitempty"`
	// Answer says where the final answer is: "stdout" (default) or "file"
	// (AnswerPath, expanded).
	Answer     string `json:"answer,omitempty"`
	AnswerPath string `json:"answerPath,omitempty"`
	// Timeout is a Go duration for one phase run (default 45m).
	Timeout string `json:"timeout,omitempty"`
}

// EngineFile is one rendered file.
type EngineFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// LoadEngineSpec reads and validates an engine spec.
func LoadEngineSpec(path string) (EngineSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return EngineSpec{}, err
	}
	var s EngineSpec
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return EngineSpec{}, fmt.Errorf("engine spec %s: %w", path, err)
	}
	return s, s.validate()
}

func (s EngineSpec) validate() error {
	if s.Name == "" {
		return errors.New("engine spec: name is empty")
	}
	if len(s.Command) == 0 || s.Command[0] == "" {
		return errors.New("engine spec: command is empty")
	}
	switch s.Answer {
	case "", "stdout":
	case "file":
		if s.AnswerPath == "" {
			return errors.New(`engine spec: answer "file" needs answerPath`)
		}
	default:
		return fmt.Errorf("engine spec: answer %q is not stdout or file", s.Answer)
	}
	if s.Timeout != "" {
		if d, err := time.ParseDuration(s.Timeout); err != nil || d <= 0 {
			return fmt.Errorf("engine spec: timeout %q is not a positive duration", s.Timeout)
		}
	}
	for _, f := range s.Files {
		if strings.Contains(f.Path, "${COX_PROMPT}") || strings.Contains(f.Content, "${COX_PROMPT}") {
			return errors.New("engine spec: ${COX_PROMPT} is allowed only in command")
		}
	}
	for k, v := range s.Env {
		if strings.Contains(v, "${COX_PROMPT}") {
			return fmt.Errorf("engine spec: env %s: ${COX_PROMPT} is allowed only in command", k)
		}
	}
	return nil
}

// CLIEngine runs an EngineSpec.
type CLIEngine struct {
	Spec EngineSpec
	// Getenv reads the runner's environment (os.Getenv in production; the
	// specs inject a map).
	Getenv func(string) string
}

// engineVars are the only names ${…} expands to. Anything else is left as an
// error, so a typo in a spec fails loudly instead of rendering an empty value.
var engineVars = []string{"COX_MODEL_BASE_URL", "COX_MODEL", "COX_WORKSPACE", "COX_PHASE", "HOME"}

// safeFileValue reports whether v can go into a rendered config file (JSON,
// TOML or YAML string) without escaping: no quotes, backslashes or control
// characters. The values are operator-set (the proxy URL, the model name),
// so a failure is a misconfiguration, not an attack, and it blocks the phase.
func safeFileValue(v string) bool {
	for _, r := range v {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' || r == '\'' {
			return false
		}
	}
	return true
}

func (e CLIEngine) vars(req EngineRequest) map[string]string {
	get := e.Getenv
	if get == nil {
		get = os.Getenv
	}
	m := map[string]string{}
	for _, k := range engineVars {
		m[k] = get(k)
	}
	m["COX_WORKSPACE"] = req.Workspace
	m["COX_PHASE"] = req.Phase
	return m
}

// expand replaces ${NAME} with vars[NAME]. An unknown name, or (for file
// content) an unsafe value, is an error.
func expand(s string, vars map[string]string, fileSafe bool) (string, error) {
	var bad error
	out := os.Expand(s, func(name string) string {
		v, ok := vars[name]
		if !ok {
			if bad == nil {
				bad = fmt.Errorf("unknown variable ${%s}", name)
			}
			return ""
		}
		if fileSafe && !safeFileValue(v) {
			if bad == nil {
				bad = fmt.Errorf("${%s} has a character that cannot go into a config file", name)
			}
			return ""
		}
		return v
	})
	return out, bad
}

// allowedFilePath: rendered files go only under $HOME or /tmp (both writable
// in the sandbox; the root filesystem is read-only), never into the workspace
// (the agent's work product) or anywhere else.
func allowedFilePath(p, home string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	c := filepath.Clean(p)
	for _, root := range []string{home, "/tmp"} {
		if root == "" || root == "/" {
			continue
		}
		r := filepath.Clean(root)
		if c != r && strings.HasPrefix(c, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// prepare renders the files and builds argv and env.
func (e CLIEngine) prepare(req EngineRequest) (argv []string, env []string, answerPath string, err error) {
	vars := e.vars(req)
	for _, f := range e.Spec.Files {
		p, err := expand(f.Path, vars, true)
		if err != nil {
			return nil, nil, "", fmt.Errorf("file path %q: %w", f.Path, err)
		}
		if !allowedFilePath(p, vars["HOME"]) {
			return nil, nil, "", fmt.Errorf("file path %q is not under $HOME or /tmp", p)
		}
		content, err := expand(f.Content, vars, true)
		if err != nil {
			return nil, nil, "", fmt.Errorf("file %s: %w", p, err)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, nil, "", err
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			return nil, nil, "", err
		}
	}
	if len(req.Prompt) > maxPromptArgBytes {
		return nil, nil, "", fmt.Errorf("the prompt is %d bytes; the CLI argument limit is %d",
			len(req.Prompt), maxPromptArgBytes)
	}
	argVars := map[string]string{"COX_PROMPT": req.Prompt}
	maps.Copy(argVars, vars)
	for _, a := range e.Spec.Command {
		x, err := expand(a, argVars, false)
		if err != nil {
			return nil, nil, "", fmt.Errorf("command %q: %w", a, err)
		}
		argv = append(argv, x)
	}
	env = os.Environ()
	for k, v := range e.Spec.Env {
		x, err := expand(v, vars, true)
		if err != nil {
			return nil, nil, "", fmt.Errorf("env %s: %w", k, err)
		}
		env = append(env, k+"="+x)
	}
	if e.Spec.Answer == "file" {
		answerPath, err = expand(e.Spec.AnswerPath, vars, true)
		if err != nil {
			return nil, nil, "", fmt.Errorf("answerPath: %w", err)
		}
		if !allowedFilePath(answerPath, vars["HOME"]) {
			return nil, nil, "", fmt.Errorf("answerPath %q is not under $HOME or /tmp", answerPath)
		}
		_ = os.Remove(answerPath) // a stale answer from an earlier run must not be read
	}
	return argv, env, answerPath, nil
}

// cappedBuffer keeps at most max bytes (head), dropping the rest.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

// tailBuffer keeps the last max bytes.
type tailBuffer struct {
	b   []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

// lastLine is the last non-empty line of s (the usual place for a CLI's error).
func lastLine(s string) string {
	for _, line := range slices.Backward(strings.Split(strings.TrimSpace(s), "\n")) {
		if l := strings.TrimSpace(line); l != "" {
			return l
		}
	}
	return ""
}

// Run runs the CLI once for the phase.
func (e CLIEngine) Run(ctx context.Context, req EngineRequest) EngineResult {
	name := e.Spec.Name
	argv, env, answerPath, err := e.prepare(req)
	if err != nil {
		return EngineResult{Err: "EngineMisconfigured: " + name + ": " + err.Error()}
	}
	timeout := defaultEngineTimeout
	if e.Spec.Timeout != "" {
		timeout, _ = time.ParseDuration(e.Spec.Timeout) // validated by LoadEngineSpec
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = req.Workspace
	cmd.Env = env
	cmd.Stdin = nil // /dev/null: a CLI must not wait for input
	stdout := &cappedBuffer{max: maxEngineStdout}
	stderr := &tailBuffer{max: maxEngineStderrTail}
	cmd.Stdout = stdout
	// The CLI's stderr goes to the container log too (kubectl logs), and its
	// tail is kept for the blocked reason.
	cmd.Stderr = io.MultiWriter(stderr, os.Stderr)
	// Its own process group, so the whole tree (the CLI and every tool
	// command it started) is signalled on cancel or timeout: SIGTERM first,
	// SIGKILL after engineKillGrace (WaitDelay).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = engineKillGrace
	defer func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()

	start := time.Now()
	runErr := cmd.Run()
	took := time.Since(start).Round(time.Second)
	trace := []string{fmt.Sprintf("engine %s: %s in %s", name, exitText(runErr), took)}
	if tail := strings.TrimSpace(string(stderr.b)); tail != "" {
		trace = append(trace, "engine "+name+" stderr (tail): "+tail)
	}

	switch {
	case ctx.Err() != nil:
		return EngineResult{Trace: trace, Err: "context cancelled"}
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return EngineResult{Trace: trace, Err: fmt.Sprintf("EngineTimeout: %s did not finish in %s", name, timeout)}
	case runErr != nil:
		reason := fmt.Sprintf("EngineFailed: %s %s", name, exitText(runErr))
		if l := lastLine(string(stderr.b)); l != "" {
			reason += ": " + l
		}
		return EngineResult{Trace: trace, Err: reason}
	}

	answer := stdout.buf.String()
	if answerPath != "" {
		data, err := os.ReadFile(answerPath)
		if err != nil {
			return EngineResult{Trace: trace,
				Err: fmt.Sprintf("EngineFailed: %s wrote no answer file %s: %v", name, answerPath, err)}
		}
		if len(data) > maxEngineStdout {
			data = data[:maxEngineStdout]
		}
		answer = string(data)
	}
	return EngineResult{Answer: strings.TrimSpace(answer), Trace: trace}
}

func exitText(err error) string {
	if err == nil {
		return "exit 0"
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return "killed by signal " + ws.Signal().String()
		}
		return "exit " + strconv.Itoa(ee.ExitCode())
	}
	return err.Error()
}
