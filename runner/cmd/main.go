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
// one-shot per container run). A blocked phase (unknown desired-phase, or a
// failed model loop) exits 1 (the operator sees the claim and requeues); a
// completed phase exits 0.
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
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/papawattu/coxswain/runner"
)

func main() {
	maxSteps := flag.Int("max-steps", 0, "cap on model rounds (0 = the runner default)")
	extraBody := flag.String("extra-body", "",
		"JSON object merged into every chat-completions request body (server tuning knobs)")
	flag.Parse()

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
	log.Printf("runner: goal=%q model=%s workspace=%s", goal, model, workspace)

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
		APIKey:    "",
		Model:     model,
		MaxSteps:  *maxSteps,
		ExtraBody: extra,
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
