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
//     (http://<loop>-proxy.<ns>.svc:8080). The proxy holds the key; the agent
//     sends no key (ADR-0006).
//   - COX_WORKSPACE: the workspace directory (default /workspace).
//   - COX_MODEL: the model name passed to /chat/completions (default
//     "local-model"). The proxy forwards to MODEL_ENDPOINT regardless; vLLM
//     ignores the model field.
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/papawattu/coxswain/runner"
)

func main() {
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
		APIKey: "",
		Model:  model,
	})
	fmt.Fprintf(os.Stderr, "runner: status=%s summary=%q\n", res.Status, res.Summary)
	if res.VerificationNotes != "" {
		log.Printf("runner: verification: %s", res.VerificationNotes)
	}
	// S3: the runner is one-shot. After writing result.json it must NOT exit
	// (restartPolicy=Always would loop it forever, hammering the model). Block
	// until the operator kills the pod (S4 makes it a phase driver that watches
	// .coxswain/desired-phase and exits when told to).
	select {}
}
