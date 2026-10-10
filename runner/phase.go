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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
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
	// desired phase (status=success, observedPhase == phase, iteration ==
	// current .coxswain/iteration), this restart must NOT call the model
	// again — it re-emits the prior claim to the termination log (exit 0)
	// and stops. For Implementing, a stored success WITHOUT a headCommit
	// triggers a re-commit (cheap, no model call) before re-emitting: the
	// first run's commit may have failed transiently, and the claim must
	// never carry an Implementing success without a valid 40-hex headCommit.
	// A BLOCKED claim is NOT re-emitted: it may retry (the model failure was
	// likely transient), and the kubelet's back-off caps the retry rate.
	// A stale-iteration result (the operator sent the Loop back to the same
	// phase for a new iteration) is NOT reused: the phase must run again
	// with fresh model work.
	curIterForPrior := readIteration(cfg.Workspace)
	if res, done := priorCompletedResult(
		filepath.Join(cfg.Workspace, resultDirName, resultFileName), phase, claimIteration(curIterForPrior),
	); done {
		// S5a (B3): an Implementing success without a headCommit re-commits
		// (the first run's commit may have failed; the workspace is on the
		// PVC so the work is still there). Never emit an Implementing success
		// claim without a valid 40-hex headCommit: if the re-commit also
		// fails, the claim is blocked (the operator holds the advance).
		if phase == PhaseImplementing && res.HeadCommit == "" {
			if sha, staged, commitErr := commitWorkspace(cfg.Workspace); sha != "" {
				res.HeadCommit = sha
				res.Summary = stagedClaimSummary(res.Summary, staged)
				log.Printf("runner: headCommit=%s (re-committed on restart)", sha)
				// Persist the fixed result so future restarts see it.
				_ = writeResult(filepath.Join(cfg.Workspace, resultDirName, resultFileName), res)
			} else {
				log.Printf("runner: commitWorkspace failed on restart; emitting blocked claim")
				res.Status = statusBlocked
				if commitErr != "" {
					res.VerificationNotes = "commit failed: re-commit on restart: " + commitErr
				} else {
					res.VerificationNotes = "commit failed: re-commit on restart returned empty headCommit"
				}
				writeClaim(claimWritePath, res)
				return res
			}
		}
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
		// S5a (B3): at the end of a successful Implementing, commit the work in
		// the workspace repo (excluding the operator-owned .coxswain dir) and
		// record the head SHA in the claim. A CLAIM (ADR-0005): the operator
		// strictly validates the 40-hex shape before pinning it to
		// status.currentVerify.verifiedCommit; a failed commit leaves the field
		// empty (the operator then blocks the advance). On a BLOCKED run the
		// work is not committed (nothing succeeded) and the claim carries no
		// headCommit (no evidence to pin).
		// I47: the staged paths ride into the claim summary (evidence: what the
		// verified commit contains), and a refused commit (an oversized or
		// binary new file) BLOCKS the claim with the reason — no headCommit,
		// the operator holds the Verifying advance.
		if res.Status == statusSuccess {
			sha, staged, commitErr := commitWorkspace(cfg.Workspace)
			if sha != "" {
				res.HeadCommit = sha
				res.Summary = stagedClaimSummary(res.Summary, staged)
				log.Printf("runner: headCommit=%s staged=%d", sha, len(staged))
			} else {
				// The commit failed (I47 rejection or git failure): the work is
				// not safe to verify (a build artifact would ride into the
				// verified commit, or the commit itself failed). The claim is
				// BLOCKED with the reason — the operator sees it in progress
				// and the advance is held (ADR-0005 fail-closed: no evidence,
				// no advance).
				res.Status = statusBlocked
				if commitErr != "" {
					res.VerificationNotes = "commit failed: " + commitErr
				} else {
					res.VerificationNotes = "commit failed: commitWorkspace returned empty headCommit"
				}
				res.Summary = res.Summary + "; " + res.VerificationNotes
				log.Printf("runner: %s; emitting blocked claim (operator will hold the advance)", res.VerificationNotes)
			}
		}
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
// for the given desired phase (status=success, observedPhase == phase,
// iteration == currentIteration), returns that Result (re-emitted to the
// termination log WITHOUT a model call — the restart-safety path). It returns
// (Result{}, false) when there is no prior result, the prior result is for a
// DIFFERENT phase (a stale result from before a phase advance), the prior
// result is BLOCKED (a blocked phase may retry), or the prior result is for a
// STALE iteration (the operator sent the Loop back to this phase for a new
// iteration: the phase must run again with fresh model work, not re-emit the
// old success). The iteration check is the ADR-0005 D11 guard: after a failed
// verify sends the Loop back to Implementing (iteration+1), the runner must
// NOT re-emit the old success claim without new model work.
func priorCompletedResult(resultPath, phase string, currentIteration int) (Result, bool) {
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
	// Stale-iteration guard: a result from a previous iteration is not
	// reusable for the current iteration (the phase must run fresh).
	if res.Iteration != currentIteration {
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
		HeadCommit    string `json:"headCommit,omitempty"`
		// S4 (R19 OS1): the .coxswain/iteration the runner read. It MUST ride
		// the claim (not just result.json): the operator's stale-iteration
		// guard compares claim.Iteration against loop.Status.Iteration and
		// discards a mismatched claim — and a missing "iteration" key
		// parses as 0, so an omitted field reads as a STALE claim from
		// iteration 0 (the P2h kind-run stall: the Implementing-iteration-2
		// claim was ignored as {"observedPhase":"Implementing","status":
		// "success","headCommit":"…"} — no iteration — and the Loop could
		// never advance past Verifying -> Stalled).
		Iteration int `json:"iteration"`
	}
	c := claim{
		ObservedPhase: res.ObservedPhase,
		Status:        res.Status,
		BlockedReason: res.VerificationNotes,
		HeadCommit:    res.HeadCommit,
		Iteration:     res.Iteration,
	}
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

// commitWorkspace (S5a, B3; I47) commits the workspace repo's uncommitted
// SOURCE work and returns (headSHA, stagedPaths, refuseReason). The commit
// identity is fixed (the agent's commit, never an operator or a system
// identity): git -c user.name=coxswain-agent -c user.email=agent@localhost.
// A no-op commit (nothing to commit) returns the EXISTING head SHA — the
// verification target is still the repo head, even when the agent's changes
// were already committed during the phase run.
//
// I47 (REVIEW-PHASE1-R20): everything the agent leaves in the workspace must
// not ride into the verified commit. The flow:
//   - reset the index to HEAD (the agent's own `git add` calls during the
//     phase run leave a dirty index — an artifact the agent added is NOT
//     trusted; the verified commit is the OPERATOR's evidence and is built
//     from the refusal-filtered set below, never from the agent's index),
//   - refuse (refuseReason set, headSHA "") any NEW file over
//     commitFileMaxBytes (1 MiB) or any binary (a NUL byte in the first 8
//     KiB): an agent edit to an existing large tracked file is still
//     committed (the agent's own source work), a large or binary NEW file
//     is not — the claim is blocked with the reason and the operator holds
//     the Verifying advance (ADR-0005 fail-closed),
//   - stage ONLY the listed, non-refused paths (a per-path `git add`, never
//     `git add -A`): an IGNORED path (a .gitignore'd build output) is
//     silently skipped by the add and never enters the commit,
//   - record the staged paths (returned; the caller lists them in the claim
//     summary — I47 evidence of what the verified commit contains).
//
// On any git failure (a non-git workspace, a failed add/commit, a
// short/uppercase SHA) it returns ("", paths, "") and the caller leaves the
// claim's headCommit empty and the operator blocks the Verifying advance
// (ADR-0005 fail-closed — no evidence, no advance). The .coxswain exclusion
// is a pathspec so the operator's result files (result.json, PLAN.md,
// desired-phase, the conversation state) never enter the verified commit.
const (
	// commitFileMaxBytes is the I47 per-new-file size cap: a single new file
	// over this is a build artifact (the gocli binary is 2.5 MB), never
	// source. The verified commit must not carry artifacts (S6 delivery
	// pushes it to the repo).
	commitFileMaxBytes = 1 * 1024 * 1024
	// commitBinaryScanBytes is the I47 binary-detection window: a NUL byte
	// in the first 8 KiB of a new file marks it binary.
	commitBinaryScanBytes = 8 * 1024
)

func gitC(workspace string, args ...string) (string, error) {
	// safe.directory: the PVC mount /workspace is root-owned (the init
	// container runs as root) while the runner runs as uid 65532. Without
	// this git refuses to operate on the repo ('dubious ownership') and all
	// git calls fail. The per-command -c avoids writing a config file (the
	// workspace is shared with the agent and the PVC is the operator's
	// evidence store — the runner must not leave a .gitconfig behind).
	full := append([]string{"-C", workspace, "-c", "safe.directory=" + workspace}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	return string(out), err
}

// isHex40 reports whether s is a 40-character hex string (a commit SHA).
func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// gitRevParseHead returns the workspace repo's HEAD as a 40-hex SHA, or ""
// when the repo is broken (not a git repo, an empty repo, a git failure).
func gitRevParseHead(workspace string) string {
	out, err := gitC(workspace, "rev-parse", "HEAD")
	if err != nil {
		log.Printf("runner: workspace rev-parse: %v: %s", err, out)
		return ""
	}
	sha := strings.TrimSpace(out)
	if !isHex40(sha) {
		return ""
	}
	return sha
}

// isTrackedPath reports whether the path is tracked in the workspace repo
// (I47: the size/binary refusal applies to NEW files only — an agent edit to
// a large existing tracked file is source work and is committed as-is).
// It consults HEAD (not the index): a file the AGENT staged during the phase
// run is not yet in HEAD and must be treated as new (the agent's index is
// not trusted — commitWorkspace resets it).
func isTrackedPath(workspace, path string) bool {
	_, err := gitC(workspace, "cat-file", "-e", "HEAD:"+path)
	return err == nil
}

// isBinaryFile reports whether the file at path carries a NUL byte in its
// first scanLimit bytes (the I47 binary heuristic: build outputs and other
// non-source files are refused; a NUL in the first 8 KiB is a reliable
// marker without reading a multi-MiB file into memory).
func isBinaryFile(path string, scanLimit int) bool {
	rc, err := os.Open(path)
	if err != nil {
		return false // unreadable: the size cap and the git-failure paths still apply
	}
	buf := make([]byte, scanLimit)
	n, _ := io.ReadFull(rc, buf)
	_ = rc.Close()
	return bytes.IndexByte(buf[:n], 0) >= 0
}

// stagedClaimSummary appends the I47 staged-paths list to the claim summary
// (the claim summary is the runner's one-line status note; the path list is
// capped so a runaway workspace cannot blow the termination-log claim size
// budget — the claim must stay well under claimMaxBytes).
func stagedClaimSummary(summary string, staged []string) string {
	if len(staged) == 0 {
		return summary
	}
	const maxListed = 8
	listed := staged
	suffix := ""
	if len(staged) > maxListed {
		listed = staged[:maxListed]
		suffix = fmt.Sprintf(" (+%d more)", len(staged)-maxListed)
	}
	line := fmt.Sprintf("\n[staged: %d file(s): %s%s]", len(staged), strings.Join(listed, ", "), suffix)
	// The whole claim (summary + blockedReason ride into the claim; the
	// operator strict-parses it) must stay under claimMaxBytes; cap the
	// appended list so it cannot push the claim over (the paths are also in
	// result.json's tool trace and in the commit itself).
	if len(summary)+len(line) > claimMaxBytes-256 {
		return summary
	}
	return summary + line
}

func commitWorkspace(workspace string) (string, []string, string) {
	head := gitRevParseHead(workspace)
	if head == "" {
		// Not a git repo (or git failed): no evidence, the caller blocks.
		return "", nil, ""
	}

	// I47: the agent's own `git add` calls during the phase run leave a
	// dirty index (an artifact the agent added). The verified commit is the
	// OPERATOR's evidence and is built from the refusal-filtered set below —
	// reset the index first so a pre-staged artifact cannot ride in (the
	// plain `git add -A` mutation, run against the workspace, is undone here
	// before the runner's own staging).
	if out, err := gitC(workspace, "reset", "-q"); err != nil {
		log.Printf("runner: workspace commit reset: %v: %s", err, out)
		return "", nil, ""
	}

	// The paths the agent left (porcelain -z: NUL-separated so a path with a
	// newline or quote round-trips; --untracked-files=all lists every
	// untracked file, not a collapsed directory; the .coxswain pathspec
	// exclusion keeps the operator's result files out of the list even on a
	// .gitignore-less workspace).
	status, err := gitC(workspace, "-c", "core.quotepath=false",
		"status", "--porcelain=v1", "-z", "--untracked-files=all",
		"--", ":(exclude).coxswain")
	if err != nil {
		log.Printf("runner: workspace commit status: %v: %s", err, status)
		return "", nil, ""
	}
	if strings.TrimSpace(status) == "" {
		// No changes: the head SHA is still the verification target (the
		// agent's work was already committed during the phase run).
		return head, nil, ""
	}

	// The refusal set is EVERY path the worktree differs on (modified,
	// deleted, renamed — the records with a status prefix) PLUS every
	// untracked file (the "?? PATH" records). A rename's new path is the
	// worktree file (refused as new when it is a binary/oversized); its old
	// path is a deletion (always allowed — it removes, it does not add).
	var paths []string
	for rec := range strings.SplitSeq(status, "\x00") {
		if len(rec) < 4 {
			continue
		}
		path := rec[3:]
		if path == resultDirName || strings.HasPrefix(path, resultDirName+"/") {
			continue // defensive: the pathspec above already excludes it
		}
		if rec[0] == 'R' || rec[0] == 'C' {
			// Rename/copy: the NEW path (rec[3:]) is the worktree file; the
			// next record is the OLD (prefix-free) path — skip both as
			// refusal candidates (the new path is added below as untracked
			// via the ?? entry git emits for the destination when the source
			// was untracked; a tracked-source rename is source work).
			continue
		}
		if rec[:2] == "??" {
			if info, statErr := os.Stat(filepath.Join(workspace, path)); statErr == nil && info.IsDir() {
				continue // a dir entry is not a stageable file
			}
			// I80/D51: an UNTRACKED root PLAN.md is the agent's own plan (its
			// plan lives in .coxswain/PLAN.md, which is excluded above). Keep
			// it out of the verified commit so it is not delivered. A
			// base-TRACKED root PLAN.md that the agent modified is source work
			// (rec[:2] != "??") and is still committed, so a repo's own
			// PLAN.md can be changed.
			if path == "PLAN.md" && !isTrackedPath(workspace, path) {
				continue
			}
			paths = append(paths, path) // untracked: new — refuse-eligible
			continue
		}
		// A worktree-differing tracked file (M/D/R/C/A): source work —
		// allowed (a modification/deletion is never a build artifact).
		if rec[1] != ' ' {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return head, nil, ""
	}

	// I47: refuse a NEW file (not in HEAD) that is over the size cap or is
	// binary. isTrackedPath consults HEAD (the index was reset above): an
	// agent-edit to a tracked file passes, a new artifact is refused. A
	// refused file UNSTAGES nothing (the index is already HEAD) and the
	// whole commit is blocked — the claim carries no headCommit (ADR-0005
	// fail-closed) and the reason names the artifact.
	for _, p := range paths {
		if isTrackedPath(workspace, p) {
			continue
		}
		info, statErr := os.Lstat(filepath.Join(workspace, p))
		if statErr != nil {
			continue // deleted / gone: nothing to refuse
		}
		if info.Size() > commitFileMaxBytes {
			return "", paths, fmt.Sprintf(
				"new file %s is %d bytes (cap %d); build artifacts must be gitignored "+
					"(add a .gitignore entry) or removed before re-running",
				p, info.Size(), commitFileMaxBytes)
		}
		if isBinaryFile(filepath.Join(workspace, p), commitBinaryScanBytes) {
			return "", paths, fmt.Sprintf(
				"new file %s is binary (NUL byte in the first %d KiB); build "+
					"artifacts must be gitignored or removed before re-running",
				p, commitBinaryScanBytes/1024)
		}
	}

	// Stage ONLY the listed paths (never `git add -A`: an unignored build
	// output would stage and the refusal above would have caught it; an
	// IGNORED path — a .gitignore'd build output — is silently skipped by
	// the add and never enters the verified commit even though it is on
	// disk). The paths are argv elements (exec.Command, no shell), so a
	// path with a space or quote reaches git verbatim. A deletion stages as
	// a removal (git add -- <deleted-path> records the delete since git
	// 2.0; the reset above made the worktree the sole source of truth).
	args := append([]string{"add", "-A", "--"}, paths...)
	if out, addErr := gitC(workspace, args...); addErr != nil {
		log.Printf("runner: workspace commit add: %v: %s", addErr, out)
		return "", paths, ""
	}

	// The actual staged set (NUL-separated, core.quotepath=false so
	// non-ASCII and odd-named paths round-trip verbatim): the claim summary
	// lists what the commit will carry (I47 evidence).
	stagedOut, err := gitC(workspace, "-c", "core.quotepath=false",
		"diff", "--cached", "-z", "--name-only")
	if err != nil {
		log.Printf("runner: workspace commit staged list: %v: %s", err, stagedOut)
		return "", paths, ""
	}
	var stagedPaths []string
	for p := range strings.SplitSeq(stagedOut, "\x00") {
		if p != "" {
			stagedPaths = append(stagedPaths, p)
		}
	}
	if len(stagedPaths) == 0 {
		// Nothing actually staged (a pure delete set whose path was also
		// gone, or every path was ignored): the head is unchanged and the
		// commit is a no-op.
		return head, nil, ""
	}

	commitArgs := []string{"-c", "user.name=coxswain-agent", "-c", "user.email=agent@localhost",
		"commit", "-m", "coxswain: implement"}
	if out, commitErr := gitC(workspace, commitArgs...); commitErr != nil {
		log.Printf("runner: workspace commit (nothing to commit or git error): %v: %s", commitErr, out)
		return "", stagedPaths, ""
	}

	newHead := gitRevParseHead(workspace)
	if newHead == "" {
		return "", stagedPaths, ""
	}
	return newHead, stagedPaths, ""
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
		"Your plan lives in .coxswain/PLAN.md; do not create a root PLAN.md. " +
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
