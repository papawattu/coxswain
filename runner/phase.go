// S4 (TDD-PLAN A. "Runner as phase driver", A1–A4): the runner as a ONE-SHOT
// phase driver.
//
// One container run per phase (the S4 design choice, documented in
// internal/controller/loop_s4_phase.go): the operator writes the desired
// phase to <workspace>/.coxswain/desired-phase (the phase-init container),
// the runner reads it, does THAT phase's work, writes the claim (result.json
// + /dev/termination-log), and EXITS. The operator reads the claim from the
// agent container's termination message (the APIReader path — ADR-0004),
// advances status.phase one step, and recreates the sandbox pod; the new
// pod's phase-init writes the NEXT phase and the runner runs again. The
// termination message is one-shot per container run, so a stable phase must
// not re-execute on the old pod — the pod recycle is the re-run mechanism.
//
// Channel (ADR-0004, precisely):
//   - Operator -> runner: the desired phase at <workspace>/.coxswain/
//     desired-phase (the operator's hint; status.desiredPhase is the only
//     truth; the operator is the sole writer of the phase).
//   - Runner -> operator: the CLAIM — result.json with status/summary/... +
//     the observedPhase the phase was executed as, mirrored (size-limited,
//     strict JSON) into /dev/termination-log (the kubelet-capped 40-char
//     termination message: a strict {"observedPhase","status",
//     "blockedReason"} object, no iteration field — the iteration rides in
//     result.json only). Exit code 0 = the phase completed the operator's
//     ask; exit code 1 = the phase ended blocked. The operator never trusts
//     the claim (ADR-0005): size-limited, strict-parsed, and never a gate
//     input — the only thing it does with it is the pure nextPhase match.
//
// Phase work:
//   - Planning: the model is driven with a planning prompt and its final
//     answer is written to <workspace>/.coxswain/PLAN.md (<=4KB, A2); the
//     runner reports observedPhase=Planning.
//   - Implementing: the model + shell loop does the work (A3); the runner
//     reports observedPhase=Implementing.
//   - Verifying is DROPPED from the runner (ADR-0005): verify evidence comes
//     from the operator's isolated Job, never the runner. A desired-phase of
//     Verifying (or any unknown value) is reported as status=blocked with the
//     value echoed in observedPhase — a claim the operator can see, not a
//     phase the runner executes.
//
// A4 (model context continuity): within ONE iteration the Planning and
// Implementing phase runs share the SAME conversation — the messages
// accumulated in Planning are carried into Implementing's model calls, not
// reset. The iteration is identified by <workspace>/.coxswain/iteration
// (operator-written, the .coxswain convention): the conversation is
// discarded when the iteration changes. (A phase advance RECYCLES the pod
// (fresh emptyDir), so the conversation file is discarded on a phase
// boundary by the pod, and the reference runner re-plans/re-implements from
// the repo — the plan survives as PLAN.md in the workspace. The continuity
// matters when a single runner process executes consecutive phases WITHOUT
// a pod recycle (the test seam, and a future multi-phase single-pod design):
// the conversation file is the seam that carries it.)
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
	// PhaseVerifying is the verifying phase — the runner never executes it
	// (ADR-0005): verify evidence comes from the operator's isolated Job
	// (B3). It is a desired-phase value the operator may write (the phase
	// machine lands the Loop here); the runner recognizes it and idles until
	// SIGTERM instead of exiting blocked (see PhaseRun).
	PhaseVerifying = "Verifying"
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

// terminationLogPath is the kernel-managed file the kubelet caps at 4096
// bytes and surfaces as the container's termination message (the ADR-0004
// claim channel to the operator). The runner writes the STRICT claim JSON
// here (see writeClaim) — a size-limited, strict object, no free text.
const terminationLogPath = "/dev/termination-log"

// claimMaxBytes bounds the claim the runner writes to /dev/termination-log
// (the kubelet cap is 4096; the runner keeps well under it so the strict
// JSON object is never truncated mid-field by the kubelet's cap).
const claimMaxBytes = 4096

// claimWritePath is the file the ADR-0004 claim is written to (the
// /dev/termination-log the operator reads back from the container status).
// A variable (not a const) so the unit tests can redirect the write to a
// temp file (/dev/termination-log is a kernel-managed path the runner only
// has inside its container; the unit tests exercise the same writeClaim code
// against a writable path).
var claimWritePath = terminationLogPath

// defaultPollInterval is how long PhaseRun waits for the operator to write a
// desired-phase (a fresh pod's phase-init writes it almost immediately; the
// poll is a safety net for a slow init / a manual pod).
const defaultPollInterval = 5 * time.Second

// defaultIdleTimeout bounds the Verifying idle (PhaseRun blocks until SIGTERM
// with a Verifying desired phase; this is only a safety net so a mis-set
// desired phase cannot hold the container forever — the operator recreates
// the sandbox from the Job's evidence and SIGTERMs the pod, and the pod has
// no liveness probe that would kill a healthy idler).
const defaultIdleTimeout = 24 * time.Hour

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
	// MaxSteps caps the number of model rounds WITHIN the phase (tool-call
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
	// IdleTimeout bounds how long PhaseRun blocks when the desired phase is
	// one the runner does not execute (Verifying is operator-owned, B3): a
	// defensive bound so a mis-set desired phase cannot hold the container
	// forever. The S3 production pod never hits it (SIGTERM arrives when the
	// operator recreates the sandbox). Zero uses defaultIdleTimeout.
	IdleTimeout time.Duration
}

// PhaseRun is the one-shot phase driver (S4). It waits for the operator's
// desired-phase at <workspace>/.coxswain/desired-phase (bounded by wait
// or the absence of the file), executes THAT phase exactly once, writes
// result.json AND the strict claim to /dev/termination-log, and returns
// the Result (the caller in cmd/main.go exits 0 for success, 1 for blocked).
//
// The model conversation (A4) is read from <workspace>/.coxswain/
// conversation.json (the previous phase run's state, when the pod was not
// recycled) and written back after the phase, so a single runner process
// that executes consecutive phases carries the context forward. A pod
// recycle (a phase advance) discards the file (fresh emptyDir) and the
// conversation starts fresh from the repo.
//
// It exits (returns) when:
//   - the desired phase is read and the phase's model loop completes (the
//     normal path: one phase, one exit),
//   - the desired phase is unknown (a typo, or a phase the runner does not
//     execute, e.g. Verifying is operator-owned per ADR-0005): result.json
//     carries status=blocked with the value echoed in observedPhase — a
//     claim the operator can see, not a phase the runner executes (exit 1),
//   - stop is closed before a desired phase appears (SIGTERM: the pod is
//     being deleted; no claim is written).
func PhaseRun(cfg PhaseConfig, stop <-chan any) Result {
	planCap := cfg.PlanMaxBytes
	if planCap <= 0 {
		planCap = planMaxBytes
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}

	phase, ok := awaitDesiredPhase(cfg.Workspace, poll, stop)
	if !ok {
		// No desired phase before stop: the pod is being deleted (SIGTERM)
		// or the operator never set one. No claim (a zero Result means
		// "no phase executed"; cmd/main.go exits 0 on a stop with no phase —
		// a deleted pod must not look like a blocked run).
		return Result{}
	}

	// Verifying is operator-owned (ADR-0005): the verify evidence comes from
	// the operator's isolated Job (B3, S5), never from the runner. With a
	// Verifying desired phase the runner does NOT exit blocked (an exit would
	// restart the one-shot container in a crash loop under restartPolicy
	// Always, churning the pod and the operator's progress record): it logs
	// once, makes no model call, and blocks until SIGTERM (bounded by
	// IdleTimeout as a safety net) so the sandbox pod stays Running until the
	// operator recreates it from the Job's evidence.
	if phase == PhaseVerifying {
		log.Printf("runner: desired-phase %q is operator-owned (B3); idling until the operator recreates the pod", phase)
		idle := cfg.IdleTimeout
		if idle <= 0 {
			idle = defaultIdleTimeout
		}
		select {
		case <-stop:
		case <-time.After(idle):
		}
		return Result{}
	}

	// Restart-safety: the sandbox pod's restartPolicy is Always, so a
	// one-shot exit RESTARTS the agent container and re-runs the phase.
	// If result.json already holds a COMPLETED claim for the current
	// desired phase (status=success, observedPhase == phase), this restart
	// must NOT call the model again — it re-emits the prior claim to the
	// termination log (exit 0) and stops. A BLOCKED claim is NOT re-emitted:
	// it may retry (the model failure was likely transient), and the
	// kubelet's back-off caps the retry rate.
	if res, done := priorCompletedResult(filepath.Join(cfg.Workspace, resultDirName, resultFileName), phase); done {
		writeClaim(claimWritePath, res)
		return res
	}

	// A4: the conversation (the previous phase's messages, when the pod was
	// not recycled) is the starting history. A new iteration (the
	// operator-written .coxswain/iteration changed since the conversation
	// was written) discards it.
	conversation, convIter := readConversation(filepath.Join(cfg.Workspace, resultDirName, conversationFileName))
	curIter := readIteration(cfg.Workspace)
	if curIter != "" && curIter != convIter {
		conversation = nil
	}

	var res Result
	switch phase {
	case PhasePlanning:
		res = drivePhaseOnce(cfg, planningPrompt(cfg.Goal, planCap), phase, conversation,
			func(answer string) { writePlan(cfg.Workspace, answer, planCap) })
	case PhaseImplementing:
		res = drivePhaseOnce(cfg, implementingPrompt(cfg.Workspace, cfg.Goal), phase, conversation)
	default:
		// Unknown phase (a typo, or a phase the runner does not execute,
		// e.g. Verifying is operator-owned per ADR-0005). Report it as
		// blocked with the value echoed in observedPhase (the operator sees
		// the claim and acts). Do NOT loop forever on it.
		res = Result{
			Status: statusBlocked,
			Summary: fmt.Sprintf("unknown desired-phase %q; the runner executes only %s or %s",
				phase, PhasePlanning, PhaseImplementing),
			ObservedPhase: phase,
		}
	}
	// A4: persist the conversation for a next phase run WITHOUT a pod
	// recycle (the test seam / a future multi-phase single-pod design). The
	// state is the runner's working memory (not a claim); a failed write is
	// non-fatal (the next run simply starts without the prior conversation).
	if len(res.toolConversation) > 0 {
		_ = writeConversationState(
			filepath.Join(cfg.Workspace, resultDirName, conversationFileName), res.toolConversation, curIter,
		)
	}
	// The claim channel: result.json (the full ADR-0004 file) + the strict
	// termination-log object (the operator's read-back). A write failure is
	// recorded (the claim is the runner's ONLY output channel; a failed
	// write is a blocked run the operator must see).
	res.Iteration = claimIteration(curIter)
	if err := writeResult(filepath.Join(cfg.Workspace, resultDirName, resultFileName), res); err != nil {
		res.Status = statusBlocked
		res.VerificationNotes = fmt.Sprintf("write result: %v", err)
	}
	writeClaim(claimWritePath, res)
	return res
}

// priorCompletedResult reads result.json and, when it holds a COMPLETED claim
// for the given desired phase (status=success AND observedPhase == phase),
// returns that Result (re-emitted to the termination log WITHOUT a model
// call — the restart-safety path). It returns (Result{}, false) when there is
// no prior result, or the prior result is for a DIFFERENT phase (a stale
// result from before a phase advance: the fresh pod's phase-init has written
// the new desired phase, and the phase must run), or the prior result is
// BLOCKED (a blocked phase may retry — the model failure was likely
// transient — and the kubelet's restart back-off caps the retry rate).
func priorCompletedResult(resultPath, phase string) (Result, bool) {
	data, err := os.ReadFile(resultPath)
	if err != nil {
		return Result{}, false
	}
	var res Result
	if err := json.Unmarshal(data, &res); err != nil {
		return Result{}, false
	}
	if res.Status != statusSuccess || res.ObservedPhase != phase {
		return Result{}, false
	}
	return res, true
}

// awaitDesiredPhase polls <workspace>/.coxswain/desired-phase until it is
// present (ok=true) or stop is closed (ok=false). The phase-init container
// writes it almost immediately on a fresh pod; the poll is a safety net for
// a slow init / a manual pod.
func awaitDesiredPhase(workspace string, poll time.Duration, stop <-chan any) (string, bool) {
	for {
		if phase, ok := readDesiredPhase(workspace); ok {
			return phase, true
		}
		select {
		case <-stop:
			return "", false
		case <-time.After(poll):
		}
	}
}

// drivePhaseOnce drives the model for ONE phase (starting from the A4
// conversation history), runs the optional onAnswer hook (the phase's
// post-processing, e.g. the PLAN.md write), and returns the phase's Result
// (with the observedPhase set and the conversation captured for A4).
func drivePhaseOnce(cfg PhaseConfig, prompt, phase string, conversation []chatMessage,
	onAnswer ...func(string)) Result {
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
	messages := make([]chatMessage, 0, 1+len(conversation)+1)
	messages = append(messages, chatMessage{Role: jsonRoleSystem, Content: systemPrompt})
	messages = append(messages, conversation...)
	messages = append(messages, chatMessage{Role: jsonRoleUser, Content: prompt})

	client := &http.Client{}
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
	res.toolConversation = messages
	if modelErr != "" {
		res.Status = statusBlocked
		res.VerificationNotes = modelErr
	}
	if len(onAnswer) > 0 && onAnswer[0] != nil {
		onAnswer[0](answer)
	}
	return res
}

// writePlan (A2) writes the model's plan answer to <workspace>/.coxswain/
// PLAN.md, truncated to the cap (by BYTES, not runes — the ADR-0004 contract
// is a <=4KB file). The runner (not the model) writes the file: the ADR-0004
// channel is the result file, and the PLAN.md is an operator-readable
// artifact (the make sample-run approval step surfaces it). A write failure
// is non-fatal (the plan is in result.json's summary regardless).
func writePlan(workspace, answer string, capBytes int) {
	plan := cutBytePrefix(answer, capBytes)
	planPath := filepath.Join(workspace, resultDirName, planFileName)
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		log.Printf("runner: PLAN.md mkdir: %v", err)
		return
	}
	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		log.Printf("runner: PLAN.md write: %v", err)
	}
}

// writeClaim writes the ADR-0004 claim to path (the /dev/termination-log the
// operator reads back from the container status). It is a STRICT, size-
// limited JSON object — {"observedPhase","status","blockedReason"} (no
// iteration: the iteration rides in result.json only, and the claim must fit
// the kubelet's 4096-byte termination-message cap with room for the strict
// JSON overhead). A write failure is LOGGED, not fatal: the result.json is
// the full ADR-0004 file and the operator's read-back is the termination
// message — if that write fails, the operator sees no claim (a requeue) and
// the result.json remains on the workspace volume for debugging. The claim
// is size-limited to claimMaxBytes (well under the kubelet cap) so a long
// blockedReason cannot truncate the object mid-field.
func writeClaim(path string, res Result) {
	type claim struct {
		ObservedPhase string `json:"observedPhase"`
		Status        string `json:"status"`
		BlockedReason string `json:"blockedReason,omitempty"`
	}
	c := claim{ObservedPhase: res.ObservedPhase, Status: res.Status, BlockedReason: res.VerificationNotes}
	data, err := json.Marshal(c)
	if err != nil {
		log.Printf("runner: claim marshal: %v", err)
		return
	}
	if len(data) > claimMaxBytes {
		// A blockedReason that long must not truncate the strict object:
		// truncate the reason (the observedPhase + status are the gate
		// inputs; the reason is OS1-only context).
		c.BlockedReason = cutBytePrefix(c.BlockedReason, claimMaxBytes-len(data)+len(c.BlockedReason))
		data, err = json.Marshal(c)
		if err != nil {
			log.Printf("runner: claim re-marshal: %v", err)
			return
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("runner: claim write to %s: %v", path, err)
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

// planningPrompt (A2) returns the PLANNING phase prompt: it seeds the model
// with the goal and asks for a concise plan summary (not the implementation
// itself) that the runner writes to PLAN.md. The plan cap is stated so the
// model's answer fits the ADR-0004 <=4KB file.
func planningPrompt(goal string, planCap int) string {
	return "You are in the PLANNING phase. Produce a concise plan (under " +
		fmt.Sprintf("%d bytes", planCap) +
		") to accomplish the goal below. The plan must be a summary of the steps you will take, " +
		"not the implementation itself. End your reply with the plan text (it will be written to PLAN.md).\n\nGOAL:\n" +
		goal
}

// implementingPrompt (A3) returns the IMPLEMENTING phase prompt: it seeds the
// model with the goal + the plan (PLAN.md, when present) and asks it to use
// its shell tool to make the workspace changes and run the acceptance
// checks. The plan is carried so the model does not re-derive it (the A4
// conversation is the primary continuity; the plan file is the durable
// artifact).
func implementingPrompt(workspace, goal string) string {
	plan := ""
	if data, err := os.ReadFile(filepath.Join(workspace, resultDirName, planFileName)); err == nil {
		plan = cutRunePrefix(string(data), planMaxBytes)
	}
	p := "You are in the IMPLEMENTING phase. Use your shell tool to make the goal's changes in the workspace. " +
		"Run the acceptance checks to confirm your work. " +
		"End your reply with a summary of the files you changed and the verification you ran.\n\nGOAL:\n" +
		goal
	if plan != "" {
		p += "\n\nPLAN (from PLAN.md):\n" + plan
	}
	return p
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

// claimIteration converts the .coxswain/iteration string (the operator's
// iteration marker) to the int the claim/result carry (0 when unset — the
// OS1 progress iteration defaults to 0, the Loop's status.iteration is the
// authoritative count).
func claimIteration(iter string) int {
	if iter == "" {
		return 0
	}
	n := 0
	for _, r := range iter {
		if r < '0' || r > '9' {
			return 0 // not a pure integer marker: report 0 (the marker is
			// informational; the claim must not parse garbage).
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// conversationState is the persisted A4 conversation (the runner's working
// memory, not a claim): the messages + the iteration they were written under
// (a different iteration on the next run discards them).
type conversationState struct {
	Iteration string        `json:"iteration"`
	Messages  []chatMessage `json:"messages"`
}

// readConversation reads the persisted A4 conversation state from path. A
// missing/corrupt/oversized file is (nil, "") (no conversation — the safe
// default: the phase starts fresh from the repo).
//
// System messages are DROPPED from the loaded conversation: drivePhaseOnce
// prepends a fresh system prompt (jsonRoleSystem) at index 0, and the model
// endpoint (vLLM with the qwen chat template) rejects a request that carries
// a second system message ("System message must be at the beginning."). A
// persisted conversation may contain the previous run's system prompt
// (res.toolConversation includes it), so the load must filter it — the
// loaded history carries only user/assistant/tool turns.
func readConversation(path string) ([]chatMessage, string) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxConversationBytes {
		return nil, ""
	}
	var st conversationState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, ""
	}
	msgs := make([]chatMessage, 0, len(st.Messages))
	for _, m := range st.Messages {
		if m.Role == jsonRoleSystem {
			continue
		}
		msgs = append(msgs, m)
	}
	return msgs, st.Iteration
}

// writeConversationState persists the A4 conversation state to path (the
// runner's working memory, not a claim). A state whose marshaled JSON
// exceeds maxConversationBytes is truncated to its most recent half of
// messages (the recent context is the useful part for the next phase); if
// even that is too large, an empty list is written (a missing/empty
// conversation is the safe default for the next run).
func writeConversationState(path string, conv []chatMessage, iteration string) error {
	st := conversationState{Iteration: iteration, Messages: conv}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if len(data) > maxConversationBytes {
		if len(conv) > 2 {
			st.Messages = conv[len(conv)/2:]
			data, err = json.Marshal(st)
			if err != nil {
				return err
			}
		}
		if len(data) > maxConversationBytes {
			// Even the truncated conversation is too large: write an empty
			// list (the reader treats it as "no conversation", the safe
			// default).
			st.Messages = nil
			data = mustMarshal(tinyConversation())
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func tinyConversation() conversationState {
	return conversationState{Iteration: "", Messages: []chatMessage{}}
}

func mustMarshal(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

// cutBytePrefix returns s truncated to at most n BYTES (a prefix that never
// splits a UTF-8 rune). The ADR-0004 PLAN.md cap is a byte cap (<=4KB file),
// so the truncation is by bytes, not runes.
func cutBytePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Back up to a rune boundary so the prefix is valid UTF-8.
	i := n
	for i > 0 && !utf8RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// utf8RuneStart reports whether byte b is the first byte of a UTF-8 rune
// (not a continuation byte 10xxxxxx).
func utf8RuneStart(b byte) bool {
	return b&0xC0 != 0x80
}
