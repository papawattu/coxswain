// The Coxswain runner's agent entrypoint (S3b, GAP 1 -> S4 phase driver).
//
// S4 (TDD-PLAN A. "Runner as phase driver", A1–A4): the runner is a ONE-SHOT
// phase driver. It waits for the operator's desired phase
// (.coxswain/desired-phase, written by the phase-init container), does THAT
// phase's work (Planning -> PLAN.md A2; Implementing -> model+shell A3; A4
// keeps the model conversation across phases within an iteration), writes the
// claim (result.json + the termination log), and EXITS. The operator reads
// the claim from the agent container's termination message (ADR-0004,
// APIReader path), advances the phase machine one step, and recreates the
// sandbox pod (one container run per phase — the termination message is
// one-shot per container run). A blocked phase (a failed model loop) exits 1
// (the operator sees the claim and requeues); a phase the runner does not
// execute (Verifying, or a value this binary does not implement) writes no
// claim and idles until SIGTERM (F52 C2); a completed phase exits 0.
//
// Environment (all operator-set; a Loop cannot set COX_* names, I34):
//   - COX_GOAL: the goal / prompt (spec.goal).
//   - COX_MODEL_BASE_URL: the per-Loop model proxy Service URL
//     (http://<loop>-proxy.<ns>.svc:8080, the bare base). The runner appends
//     /v1/chat/completions to it (vLLM serves its API under /v1; the proxy is
//     a transparent reverse proxy and does not rewrite the path). The proxy
//     holds the key; the agent sends no key (ADR-0006).
//   - COX_WORKSPACE: the workspace directory (default /workspace).
//   - COX_MODEL: the model name passed to /v1/chat/completions (default
//     "local-model"). It must match a model the upstream model server serves
//     (vLLM validates it; e.g. 'qwen3.8-27b' on the local 192.168.1.20:8000).
//   - COX_MODEL_TIMEOUT: optional per-request model timeout (F46), a Go
//     duration ("90s") or a bare number of seconds. Absent, invalid or <= 0
//     keeps the runner default (120s). Set by the operator from
//     spec.loop.modelTimeoutSeconds.
//
// Flags:
//   - -max-steps: cap on model rounds (0 = the runner default, 25).
//   - -extra-body: a JSON object merged into every chat-completions request
//     body, for server tuning knobs that are not part of the standard OpenAI
//     schema. The local Qwen vLLM serves a thinking model; without
//     chat_template_kwargs.enable_thinking=false the runner's tool calls land
//     in the reasoning output and the model loop cannot make progress (S3
//     acceptance, 2026-10-02).
//   - -termination-log: the path the runner writes its ADR-0004 claim to
//     (default /dev/termination-log, the kubelet-capped file the operator
//     reads back from the container status). Overridable in tests / a manual
//     pod where /dev/termination-log does not exist (the write is best-effort
//     — a failure is logged; the result.json on the workspace is the full
//     ADR-0004 file).
//   - -engine-spec: the agent image's engine spec (agent adapters,
//     docs/PLAN-AGENT-ADAPTERS.md; default /etc/coxswain/engine.json). When the
//     file exists, the phase's agent work is done by that external CLI
//     (runner.CLIEngine) instead of the built-in model + shell loop; the
//     reference runner image has no such file. A file that exists but does not
//     parse is fatal: an adapter image must never fall back silently to the
//     built-in engine.
//   - -selftest: check the image (engine spec, the CLI on PATH, a writable
//     $HOME and /tmp), print one JSON line, and exit 0 or 1. Used by the agent
//     image tests (T2); it needs no COX_* environment and runs no model call.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/papawattu/coxswain/runner"
)

// parseModelTimeout reads COX_MODEL_TIMEOUT (F46): a Go duration or a bare
// number of seconds. Zero (use the runner default) for absent, invalid or
// non-positive input; an invalid value is logged, not fatal.
func parseModelTimeout(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		n, nerr := strconv.Atoi(v)
		if nerr != nil {
			log.Printf("runner: ignoring invalid COX_MODEL_TIMEOUT %q: %v", v, err)
			return 0
		}
		d = time.Duration(n) * time.Second
	}
	if d <= 0 {
		return 0
	}
	return d
}

func main() {
	maxSteps := flag.Int("max-steps", 0, "cap on model rounds (0 = the runner default)")
	engineSpec := flag.String("engine-spec", runner.DefaultEngineSpecPath,
		"agent image engine spec; used when the file exists (agent adapters)")
	selftest := flag.Bool("selftest", false, "check the image and exit (agent image tests)")
	extraBody := flag.String("extra-body", "",
		"JSON object merged into every chat-completions request body (server tuning knobs)")
	flag.Parse()

	engine, engineName, err := loadEngine(*engineSpec)
	if err != nil {
		log.Fatalf("runner: %v", err)
	}
	if *selftest {
		os.Exit(runSelftest(engine, engineName))
	}

	var extra map[string]any
	if *extraBody != "" {
		if err := json.Unmarshal([]byte(*extraBody), &extra); err != nil {
			log.Fatalf("--extra-body is not a JSON object: %v", err)
		}
	}
	goal := os.Getenv("COX_GOAL")
	if goal == "" {
		// The operator sets COX_GOAL for a runner agent; an empty goal means the
		// image was run without the operator's wiring (e.g. a manual pod). Fail
		// loudly rather than drive the model with no task.
		log.Fatal("COX_GOAL is empty; the runner requires the operator-set COX_GOAL (spec.goal)")
	}
	baseURL := os.Getenv("COX_MODEL_BASE_URL")
	if baseURL == "" {
		log.Fatal("COX_MODEL_BASE_URL is empty; the runner requires the operator-set model proxy URL")
	}
	workspace := os.Getenv("COX_WORKSPACE")
	if workspace == "" {
		workspace = "/workspace"
	}
	model := os.Getenv("COX_MODEL")
	if model == "" {
		model = "local-model"
	}
	modelTimeout := parseModelTimeout(os.Getenv("COX_MODEL_TIMEOUT"))
	log.Printf("runner: goal=%q model=%s workspace=%s modelTimeout=%s engine=%s",
		goal, model, workspace, modelTimeout, engineName)

	// The one-shot phase driver: wait for the desired phase, do the phase's
	// work, write the claim, and exit (0 = the phase completed the operator's
	// ask; 1 = the phase ended blocked — the operator sees the claim and
	// requeues). A SIGTERM (pod deletion) returns a zero Result (no phase
	// executed) before the claim write — a deleted pod must not look like a
	// blocked run.
	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGTERM, os.Interrupt)
	stop := make(chan any)
	go func() {
		s, ok := <-stopCh
		if ok {
			stop <- s
		}
		close(stop)
	}()
	res := runner.PhaseRun(runner.PhaseConfig{
		Workspace: workspace,
		Goal:      goal,
		BaseURL:   baseURL,
		// ADR-0006: the agent holds no model key; the proxy injects auth. The
		// runner sends no API key (the proxy does not require one from the
		// agent).
		APIKey:       "",
		Model:        model,
		MaxSteps:     *maxSteps,
		ExtraBody:    extra,
		ModelTimeout: modelTimeout,
		Engine:       engine,
	}, stop)
	code := 0
	if res.Status == "blocked" {
		code = 1
	}
	fmt.Fprintf(os.Stderr, "runner: status=%s observedPhase=%s summary=%q\n", res.Status, res.ObservedPhase, res.Summary)
	if res.VerificationNotes != "" {
		log.Printf("runner: verification: %s", res.VerificationNotes)
	}
	os.Exit(code)
}

// loadEngine returns the CLI engine when the image carries an engine spec, or
// nil (the built-in model + shell loop) when it does not. A spec that exists
// but cannot be read or does not validate is an error: an adapter image must
// not silently become the reference runner.
func loadEngine(path string) (runner.Engine, string, error) {
	if path == "" {
		return nil, "builtin", nil
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, "builtin", nil
	}
	spec, err := runner.LoadEngineSpec(path)
	if err != nil {
		return nil, "", fmt.Errorf("engine spec %s: %w", path, err)
	}
	return runner.CLIEngine{Spec: spec}, spec.Name, nil
}

// runSelftest checks what an agent image needs at run time, prints one JSON
// line, and returns the exit code. It runs no model call and needs no COX_*
// environment.
func runSelftest(engine runner.Engine, name string) int {
	out := map[string]any{"engine": name, "uid": os.Getuid(), "ok": true}
	fail := func(k, v string) { out[k] = v; out["ok"] = false }
	if cli, ok := engine.(runner.CLIEngine); ok {
		if p, err := exec.LookPath(cli.Spec.Command[0]); err != nil {
			fail("command", err.Error())
		} else {
			out["command"] = p
		}
	}
	for _, dir := range []string{os.Getenv("HOME"), os.TempDir()} {
		if dir == "" {
			fail("home", "HOME is not set")
			continue
		}
		f, err := os.CreateTemp(dir, ".cox-selftest-*")
		if err != nil {
			fail("writable:"+dir, err.Error())
			continue
		}
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	b, _ := json.Marshal(out)
	fmt.Println(string(b))
	if out["ok"] != true {
		return 1
	}
	return 0
}
