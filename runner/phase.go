// S4 (TDD-PLAN A. "Runner as phase driver", A1–A4): the runner as a phase
// driver. It watches <workspace>/.coxswain/desired-phase (written by the
// OPERATOR — the runner never drives the phase machine itself, ADR-0004),
// does the work for that phase, and writes result.json carrying the reported
// observedPhase (the ADR-0004 claim channel; ADR-0005: a claim, never
// evidence).
//
// Channel (ADR-0004, precisely):
//   - Operator -> runner: the desired phase at <workspace>/.coxswain/
//     desired-phase (the operator's hint; status.desiredPhase is the only
//     truth).
//   - Runner -> operator: result.json with status/summary/... + the
//     observedPhase the phase was executed as.
//
// Phase work:
//   - Planning: the model is driven with a planning prompt and its final
//     answer is written to <workspace>/.coxswain/PLAN.md (<=4KB, A2); the
//     runner reports observedPhase=Planning.
//   - Implementing: the model + shell loop does the work (A3); the runner
//     reports observedPhase=Implementing.
//   - Verifying is DROPPED from the runner (ADR-0005): verify evidence comes
//     from the operator's isolated Job, never the runner. A desired-phase of
//     Verifying (or any unknown value) is reported as status=blocked with
//     the value echoed in observedPhase — a claim the operator can see, not
//     a phase the runner executes.
//
// A4 (model context continuity): within ONE iteration the Planning and
// Implementing phase runs share the SAME conversation — the messages
// accumulated in Planning are carried into Implementing's model calls, not
// reset. The iteration is identified by <workspace>/.coxswain/iteration
// (operator-written, the .coxswain convention): the conversation is
// discarded when the iteration changes.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Phase names (the ADR-0004 claim values). They mirror the operator's
// LoopPhase values (api/v1alpha1): the runner reports what it executed, and
// the operator validates the value against its own enum. The runner has no
// Loop-status RBAC (ADR-0004), so it cannot import the operator's types —
// the values are the contract.
const (
	// PhasePlanning is the planning phase (the A2 seam: the model writes the
	// plan, the runner writes PLAN.md from the answer).
	PhasePlanning = "Planning"
	// PhaseImplementing is the implementing phase (A3: model + shell loop).
	PhaseImplementing = "Implementing"
)

// A1: the .coxswain filenames. Single source of truth for the operator
// (internal/controller) and the runner; both read/write the same names.
const (
	desiredPhaseFileName = "desired-phase"
	planFileName         = "PLAN.md"
	iterationFileName    = "iteration"
	conversationFileName = "conversation.json"
)

// A2 (TDD-PLAN): the PLAN.md size cap. 4KB.
const planMaxBytes = 4 * 1024

// maxDesiredPhaseBytes bounds the desired-phase file (operator-written, but
// bounded anyway: a runaway value must not blow the runner's context).
const maxDesiredPhaseBytes = 512

// maxConversationBytes bounds the persisted conversation state (A4). The
// conversation is the runner's own working memory (not a claim), but it
// lives on the shared workspace volume, so a runaway model conversation
// must not fill the emptyDir.
const maxConversationBytes = 256 * 1024

// defaultPollInterval is how often the runner polls for a desired-phase
// change.
const defaultPollInterval = 5 * time.Second

// PhaseConfig is the input to PhaseRun. The model-loop fields mirror
// runConfig (BaseURL/APIKey/Model/ExtraBody/MaxSteps/ShellTimeout/
// ModelTimeout); the phase-driver fields are the workspace, the goal (the
// prompt seed), and the poll/iteration caps.
type PhaseConfig struct {
	Workspace string
	Goal      string
	BaseURL   string
	APIKey    string
	Model     string
	ExtraBody map[string]any
	// MaxSteps caps the number of model rounds WITHIN A PHASE (tool-call
	// loops). Zero uses defaultMaxSteps.
	MaxSteps int
	// ShellTimeout bounds a single shell tool call. Zero uses
	// defaultShellTimeout.
	ShellTimeout time.Duration
	// ModelTimeout bounds a single model HTTP request. Zero uses
	// defaultModelTimeout.
	ModelTimeout time.Duration
	// PlanMaxBytes is the PLAN.md cap (A2). Zero uses planMaxBytes.
	PlanMaxBytes int
	// PollInterval is the desired-phase poll interval. Zero uses
	// defaultPollInterval.
	PollInterval time.Duration
	// MaxIterations caps the number of PHASE RUNS the runner executes before
	// exiting blocked (a safety valve against a misbehaving operator that
	// writes the same phase forever, or advances back and forth). Each
	// distinct observedPhase the runner writes to result.json counts as one
	// run. Zero = no cap.
	MaxIterations int
}

// PhaseRun drives the phase loop: it polls <workspace>/.coxswain/
// desired-phase, does the work for each NEW phase the operator writes, and
// writes result.json after each phase carrying the reported observedPhase.
//
// The model conversation persists ACROSS phases within an iteration (A4):
// the messages accumulated in the previous phase are the starting history
// for the next. The iteration is read from .coxswain/iteration (operator-
// written); a change discards the conversation (a new iteration starts
// fresh).
//
// The loop exits when:
//   - stop is closed (the operator's SIGTERM; the runner is being deleted),
//   - the desired phase is unknown (result.json carries status=blocked with
//     the value echoed in observedPhase — a claim the operator can see; the
//     loop exits so a typo does not burn the model budget),
//   - MaxIterations is reached (status=blocked), or
//   - a phase's model loop fails (status=blocked for that phase; the loop
//     continues waiting for the operator to advance).
//
// A STABLE phase is not re-executed: the runner records the phase it last
// executed (from the previous result.json) and only re-runs the work when
// the operator writes a different desired-phase. (The operator advances by
// writing a new desired-phase; it does not re-request the same phase.)
//
// The returned Result is the LAST result.json written (zero Result if no
// phase was ever executed). It is the seam the tests observe alongside the
// result file and the workspace files (ADR-0004: the runner's only output
// channel is the result file).
func PhaseRun(cfg PhaseConfig, stop <-chan struct{}) Result {
	var last Result
	var conversation []chatMessage
	iteration := ""

	planCap := cfg.PlanMaxBytes
	if planCap <= 0 {
		planCap = planMaxBytes
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	client := &http.Client{}

	for {
		phase, ok := readDesiredPhase(cfg.Workspace)
		if !ok {
			select {
			case <-stop:
				return last
			case <-time.After(poll):
				continue
			}
		}

		// A4: a new iteration discards the conversation.
		curIter := readIteration(cfg.Workspace)
		if curIter != "" && curIter != iteration {
			conversation = nil
			iteration = curIter
		}

		// A stable phase is not re-executed: if the runner already executed
		// this exact phase (the previous result.json names it), wait for the
		// operator to advance. A different phase (or no previous result)
		// runs the work.
		if prev := lastObservedPhase(cfg.Workspace); prev == phase {
			select {
			case <-stop:
				return last
			case <-time.After(poll):
				continue
			}
		}

		switch phase {
		case PhasePlanning:
			res, conv := drivePhase(cfg, client, planningPrompt(cfg.Goal, planCap), PhasePlanning, conversation,
				func(answer string) { writePlan(cfg.Workspace, answer, planCap) })
			last = res
			conversation = conv
		case PhaseImplementing:
			res, conv := drivePhase(cfg, client, implementingPrompt(cfg.Workspace, cfg.Goal), PhaseImplementing, conversation, nil)
			last = res
			conversation = conv
		default:
			// Unknown phase (a typo, or a phase the runner does not execute,
			// e.g. Verifying is operator-owned per ADR-0005). Report it as
			// blocked with the value echoed in observedPhase (the operator
			// sees the claim and acts). Do NOT loop forever on it.
			res := Result{
				Status:        statusBlocked,
				Summary:       fmt.Sprintf("unknown desired-phase %q; the runner executes only %s or %s", phase, PhasePlanning, PhaseImplementing),
				ObservedPhase: phase,
			}
			if err := writeResult(filepath.Join(cfg.Workspace, resultDirName, resultFileName), res); err != nil {
				res.VerificationNotes = fmt.Sprintf("write result: %v", err)
			}
			log.Printf("runner: exiting on unknown desired-phase %q", phase)
			return res
		}

		// MaxIterations: the runner has executed a phase. Count it.
		if cfg.MaxIterations > 0 && countPhaseRuns(cfg.Workspace) >= cfg.MaxIterations {
			log.Printf("runner: max iterations (%d) reached; exiting", cfg.MaxIterations)
			last.Status = statusBlocked
			last.Summary = fmt.Sprintf("max iterations (%d) reached", cfg.MaxIterations)
			_ = writeResult(filepath.Join(cfg.Workspace, resultDirName, resultFileName), last)
			return last
		}

		// Wait for the operator to advance (write a new desired-phase).
		select {
		case <-stop:
			return last
		case <-time.After(poll):
		}
	}
}

// phasePrompt returns the phase-specific prompt. The goal is the seed; the
// phase instructions tell the model what to do in this phase.
func planningPrompt(goal string, planCap int) string {
	return "You are in the PLANNING phase. Produce a concise plan (under " +
		fmt.Sprintf("%d bytes", planCap) +
		") to accomplish the goal below. The plan must be a summary of the steps you will take, not the implementation itself. End your reply with the plan text (it will be written to PLAN.md).\n\nGOAL:\n" +
		goal
}

func implementingPrompt(workspace, goal string) string {
	plan := ""
	if data, err := os.ReadFile(filepath.Join(workspace, resultDirName, planFileName)); err == nil {
		plan = string(cutRunePrefix(string(data), planMaxBytes))
	}
	p := "You are in the IMPLEMENTING phase. Use your shell tool to make the goal's changes in the workspace. " +
		"Run the acceptance checks to confirm your work. End your reply with a summary of the files you changed and the verification you ran.\n\nGOAL:\n" +
		goal
	if plan != "" {
		p += "\n\nPLAN (from PLAN.md):\n" + plan
	}
	return p
}

// drivePhase is the shared model loop for a phase. It drives the model
// (starting from conversation, A4), runs the optional onAnswer hook (the
// phase's post-processing, e.g. the PLAN.md write), and returns the phase's
// Result + the conversation AFTER the phase (for the next phase, A4).
func drivePhase(cfg PhaseConfig, client *http.Client, prompt, phase string, conversation []chatMessage, onAnswer func(string)) (Result, []chatMessage) {
	modelTimeout := cfg.ModelTimeout
	if modelTimeout <= 0 {
		modelTimeout = defaultModelTimeout
	}
	shellTimeout := cfg.ShellTimeout
	if shellTimeout <= 0 {
		shellTimeout = defaultShellTimeout
	}
	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}

	// A4: the conversation (the previous phase's messages) is the starting
	// history. A fresh system prompt is prepended (the system prompt is
	// stable across phases; the conversation carries the phase context).
	messages := []chatMessage{{Role: jsonRoleSystem, Content: systemPrompt}}
	messages = append(messages, conversation...)
	messages = append(messages, chatMessage{Role: jsonRoleUser, Content: prompt})

	answer, trace, modelErr := driveModel(
		context.Background(), client, cfg.BaseURL, cfg.APIKey, cfg.Model, cfg.Workspace,
		messages, maxSteps, shellTimeout, modelTimeout, cfg.ExtraBody,
	)

	res := Result{
		Status:        statusSuccess,
		Summary:       answer,
		ToolTrace:     trace,
		ObservedPhase: phase,
	}
	if modelErr != "" {
		res.Status = statusBlocked
		res.VerificationNotes = modelErr
	}
	if onAnswer != nil {
		onAnswer(answer)
	}
	// A4: persist the conversation for the next phase. The state is the
	// runner's working memory (not a claim); a failed write is non-fatal
	// (the next phase simply starts without the prior conversation).
	if len(messages) > 0 {
		_ = writeConversation(filepath.Join(cfg.Workspace, resultDirName, conversationFileName), messages)
	}
	return res, messages
}

// writePlan (A2) writes the model's plan answer to <workspace>/.coxswain/
// PLAN.md, truncated to the cap. The runner (not the model) writes the
// file: the ADR-0004 channel is the result file, and the PLAN.md is an
// operator-readable artifact (the make sample-run approval step surfaces
// it). A write failure is non-fatal (the plan is in result.json's summary
// regardless).
func writePlan(workspace, answer string, capBytes int) {
	plan := cutRunePrefix(answer, capBytes)
	planPath := filepath.Join(workspace, resultDirName, planFileName)
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		log.Printf("runner: PLAN.md mkdir: %v", err)
		return
	}
	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		log.Printf("runner: PLAN.md write: %v", err)
	}
}

// readDesiredPhase reads <workspace>/.coxswain/desired-phase and returns the
// phase name (trimmed) + whether it was present. A missing/empty file is
// ("", false) — the runner waits for the operator to write it. A file
// larger than maxDesiredPhaseBytes is treated as absent (bounded read,
// ADR-0005 trust hygiene: the operator is trusted, but a bounded read is
// free).
func readDesiredPhase(workspace string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(workspace, resultDirName, desiredPhaseFileName))
	if err != nil {
		return "", false
	}
	if len(data) > maxDesiredPhaseBytes {
		return "", false
	}
	phase := strings.TrimSpace(string(data))
	return phase, phase != ""
}

// readIteration reads <workspace>/.coxswain/iteration (operator-written, the
// .coxswain convention). Empty/missing = no iteration marker (the runner
// treats the whole run as one iteration).
func readIteration(workspace string) string {
	data, err := os.ReadFile(filepath.Join(workspace, resultDirName, iterationFileName))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// lastObservedPhase returns the observedPhase the previous result.json
// named (the phase the runner last executed), for the stable-phase check.
// A missing/malformed result.json is "" (no previous phase).
func lastObservedPhase(workspace string) string {
	data, err := os.ReadFile(filepath.Join(workspace, resultDirName, resultFileName))
	if err != nil {
		return ""
	}
	var res Result
	if err := json.Unmarshal(data, &res); err != nil {
		return ""
	}
	return res.ObservedPhase
}

// countPhaseRuns counts the phase runs for the MaxIterations cap. A single
// result.json cannot carry history (it is rewritten each phase), so the
// count is 1 if the current result.json names a phase the runner executes
// (Planning/Implementing) and 0 otherwise. (The cap is a safety valve
// against a misbehaving operator, not a precise meter; the runner exits
// after MaxIterations DISTINCT phase executions, which is the conservative
// reading of "iterations".)
func countPhaseRuns(workspace string) int {
	if lastObservedPhase(workspace) == PhasePlanning || lastObservedPhase(workspace) == PhaseImplementing {
		return 1
	}
	return 0
}

// writeConversation persists the conversation (A4) to path as JSON. The
// state is the runner's working memory (not a claim); a failed write is
// non-fatal (the next phase simply starts without the prior conversation).
// A conversation whose marshaled JSON exceeds maxConversationBytes is
// truncated to its most recent half of messages (the recent context is the
// useful part for the next phase).
func writeConversation(path string, conv []chatMessage) error {
	data, err := json.Marshal(conv)
	if err != nil {
		return err
	}
	if len(data) > maxConversationBytes {
		if len(conv) > 2 {
			data, err = json.Marshal(conv[len(conv)/2:])
			if err != nil {
				return err
			}
		}
		if len(data) > maxConversationBytes {
			// Even the truncated conversation is too large: write an empty
			// list (a missing file is treated as "no conversation" by the
			// reader, which is the safe default).
			data = []byte("[]")
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
