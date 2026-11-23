package runner

// Agent adapters (docs/PLAN-AGENT-ADAPTERS.md, S1): the phase driver
// (PhaseRun) is split from the model loop. An Engine is what does a phase's
// agent work: given the phase prompt and the workspace, it returns the final
// answer. Everything else stays in the phase driver, so every engine gets it
// unchanged: the desired-phase read, the prompts (memory, lessons), PLAN.md,
// the operator-filtered commit, lessons.json, and the claim.
//
// Two engines exist:
//   - modelEngine (this file): the reference runner's own shell-tool loop
//     (driveModel). It is the default when PhaseConfig.Engine is nil, so the
//     reference runner's behaviour is unchanged.
//   - CLIEngine (cli_engine.go): an external coding-agent CLI (Claude Code,
//     Codex, pi, Hermes, opencode) run non-interactively.

import (
	"context"
	"net/http"
	"time"
)

// EngineRequest is one phase's agent work.
type EngineRequest struct {
	// Phase is the phase being executed (PhasePlanning / PhaseImplementing).
	Phase string
	// Prompt is the phase prompt (planningPrompt / implementingPrompt).
	Prompt string
	// Workspace is the directory the agent works in (its cwd).
	Workspace string
	// Messages is the full chat history for the built-in engine: the system
	// prompt, the carried conversation (A4), and the prompt as the last user
	// message. A CLI engine ignores it (the CLI keeps its own history; a phase
	// advance recycles the pod, so there is no conversation to carry).
	Messages []chatMessage
}

// EngineResult is what a phase's agent work produced.
type EngineResult struct {
	// Answer is the agent's final answer (the plan text in Planning, the
	// summary of changes in Implementing).
	Answer string
	// Trace is a short, human-readable record of what the engine did.
	Trace []string
	// Err is non-empty when the engine could not produce an answer. It is the
	// claim's blocked reason (e.g. "ModelUnavailable: …", "EngineFailed: …").
	Err string
}

// Engine does one phase's agent work. Run must honour ctx: a cancelled ctx
// (SIGTERM, a pod deletion) ends the work promptly.
type Engine interface {
	Run(ctx context.Context, req EngineRequest) EngineResult
}

// modelEngine is the reference runner's built-in engine: the model plus the
// shell tool (driveModel), unchanged.
type modelEngine struct {
	baseURL      string
	apiKey       string
	model        string
	extraBody    map[string]any
	maxSteps     int
	shellTimeout time.Duration
	modelTimeout time.Duration
	ladder       modelRetryLadder
}

// newModelEngine builds the built-in engine from the phase config, applying
// the same defaults drivePhaseOnce applied before the split.
func newModelEngine(cfg PhaseConfig) modelEngine {
	e := modelEngine{
		baseURL:      cfg.BaseURL,
		apiKey:       cfg.APIKey,
		model:        cfg.Model,
		extraBody:    cfg.ExtraBody,
		maxSteps:     cfg.MaxSteps,
		shellTimeout: cfg.ShellTimeout,
		modelTimeout: cfg.ModelTimeout,
		// F79: the phase drive runs the PRODUCTION ladder. The injectable
		// seam is runConfig's ModelRetries/ModelRetryBackoff, which the F79
		// specs use against the same shared driveModel.
		ladder: modelRetryLadderFor(0, 0),
	}
	if e.modelTimeout <= 0 {
		e.modelTimeout = defaultModelTimeout
	}
	if e.shellTimeout <= 0 {
		e.shellTimeout = defaultShellTimeout
	}
	if e.maxSteps <= 0 {
		e.maxSteps = defaultMaxSteps
	}
	return e
}

// Run drives the model with the shell tool.
func (e modelEngine) Run(ctx context.Context, req EngineRequest) EngineResult {
	answer, trace, modelErr := driveModel(
		ctx, &http.Client{}, e.baseURL, e.apiKey, e.model, req.Workspace,
		req.Messages, e.maxSteps, e.shellTimeout, e.modelTimeout, e.ladder, e.extraBody,
	)
	return EngineResult{Answer: answer, Trace: trace, Err: modelErr}
}
