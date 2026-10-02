// The Coxswain runner's agent entrypoint (S3b, GAP 1). When the operator runs
// the agent image as the runner (--runner-image), the sandbox pod's agent
// container Command is /usr/local/bin/runner; this main reads the operator-set
// environment (the I34-protected COX_* namespace a Loop cannot override) and
// drives the model against the workspace.
//
// It is deliberately minimal in S3: it takes the goal (COX_GOAL) as its
// prompt and runs the existing runner model loop. The phase-driver contract
// (reading .coxswain/desired-phase, writing result.json.observedPhase) is the
// S4 slice; until then the runner reports a single-phase run.
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
	extraBody := flag.String("extra-body", "", "JSON object merged into every chat-completions request body (server tuning knobs, e.g. Qwen's chat_template_kwargs)")
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
	res := runner.Run(runner.RunConfig{
		Prompt:    goal,
		Workspace: workspace,
		BaseURL:   baseURL,
		// ADR-0006: the agent holds no model key; the proxy injects auth. The
		// runner sends no API key (the proxy does not require one from the agent).
		APIKey:    "",
		Model:     model,
		MaxSteps:  *maxSteps,
		ExtraBody: extra,
	})
	fmt.Fprintf(os.Stderr, "runner: status=%s summary=%q\n", res.Status, res.Summary)
	if res.VerificationNotes != "" {
		log.Printf("runner: verification: %s", res.VerificationNotes)
	}
	// S3: the runner is one-shot. After writing result.json it must NOT exit
	// (restartPolicy=Always would loop it forever, hammering the model). Block
	// on a termination signal (S4 makes it a phase driver that watches
	// .coxswain/desired-phase and exits when told to). A bare 'select {}' with
	// no other live goroutines aborts with 'fatal error: all goroutines are
	// asleep - deadlock!' — the runner must have a live wait (verified in kind
	// 2026-10-02: CrashLoopBackOff). Exiting on SIGTERM keeps the pod deletion
	// clean.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	<-sig
}
