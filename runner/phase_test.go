// S4 (TDD-PLAN A. "Runner as phase driver", A1–A4): unit tests for the
// one-shot phase driver.
//
// These tests exercise PhaseRun against the fake model server (the one
// system boundary the runner mocks — the model API) with the claim write
// redirected from /dev/termination-log (a kernel-managed path the runner only
// has inside its container) to a temp file. The tests are package runner
// (white-box: they swap claimWritePath and drive PhaseRun directly).
package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/papawattu/coxswain/runner/testhelper"
)

// s4FakeModel returns a fake model server that answers with a plain final
// answer (no tool calls) — the one-shot phase driver's normal path.
func s4FakeModel() *testhelper.FakeModel {
	return testhelper.New(testhelper.ModelResponse{Content: "plan: step one, step two"})
}

// s4ClaimPath redirects the claim write to a temp file for the test's
// duration (the real runner writes /dev/termination-log; the unit test cannot
// and does not need to — the claim's CONTENT is what is under test). It
// returns the temp path (the test asserts on it) and a cleanup func.
func s4ClaimPath(t *testing.T) (string, func()) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claim.json")
	old := claimWritePath
	claimWritePath = p
	return p, func() { claimWritePath = old }
}

// writeDesiredPhase writes the operator's desired-phase file (the phase-init
// container's job in a live pod) so PhaseRun reads it immediately.
func writeDesiredPhase(t *testing.T, workspace, phase string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(workspace, resultDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, resultDirName, desiredPhaseFileName),
		[]byte(phase), 0o644); err != nil {
		t.Fatal(err)
	}
}

// parseClaim reads a strict claim JSON object back from the claim path.
func parseClaim(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read claim: %v", err)
	}
	var c map[string]any
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("claim is not a strict JSON object (%s): %v", string(data), err)
	}
	return c
}

func TestPhaseRunPlanningWritesPlanAndClaim(t *testing.T) {
	fake := s4FakeModel()
	defer fake.Close()
	claimPath, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	writeDesiredPhase(t, ws, PhasePlanning)
	stop := make(chan any) // never closed (the phase is present immediately)

	res := PhaseRun(PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
		PollInterval: 5 * time.Millisecond}, stop)

	if res.Status != statusSuccess {
		t.Fatalf("Planning phase: status = %q, want %q (notes: %s)", res.Status, statusSuccess, res.VerificationNotes)
	}
	if res.ObservedPhase != PhasePlanning {
		t.Fatalf("Planning phase: observedPhase = %q, want %q", res.ObservedPhase, PhasePlanning)
	}
	// A2: the model's plan answer is written to PLAN.md (<=4KB cap).
	plan, err := os.ReadFile(filepath.Join(ws, resultDirName, planFileName))
	if err != nil {
		t.Fatalf("PLAN.md not written: %v", err)
	}
	if len(plan) > planMaxBytes {
		t.Fatalf("PLAN.md is %d bytes; the A2 cap is %d", len(plan), planMaxBytes)
	}
	// A1: result.json is written with the phase's status + observedPhase.
	resultBytes, err := os.ReadFile(filepath.Join(ws, resultDirName, resultFileName))
	if err != nil {
		t.Fatalf("result.json not written: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("result.json is not a JSON object: %v", err)
	}
	if result["status"] != statusSuccess {
		t.Fatalf("result.json status = %v, want %q", result["status"], statusSuccess)
	}
	if result["observedPhase"] != PhasePlanning {
		t.Fatalf("result.json observedPhase = %v, want %q", result["observedPhase"], PhasePlanning)
	}
	// The claim (the termination message the operator reads back) is the
	// STRICT {"observedPhase","status","blockedReason"} object.
	claim := parseClaim(t, claimPath)
	if claim["observedPhase"] != PhasePlanning {
		t.Fatalf("claim observedPhase = %v, want %q", claim["observedPhase"], PhasePlanning)
	}
	if claim["status"] != statusSuccess {
		t.Fatalf("claim status = %v, want %q", claim["status"], statusSuccess)
	}
	if _, ok := claim["iteration"]; ok {
		t.Fatalf("the claim must NOT carry iteration (it rides in result.json only): %v", claim)
	}
}

func TestPhaseRunImplementingReportsImplementing(t *testing.T) {
	fake := s4FakeModel()
	defer fake.Close()
	claimPath, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	writeDesiredPhase(t, ws, PhaseImplementing)
	stop := make(chan any)

	res := PhaseRun(PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
		PollInterval: 5 * time.Millisecond}, stop)

	if res.Status != statusSuccess {
		t.Fatalf("Implementing phase: status = %q, want %q (notes: %s)", res.Status, statusSuccess, res.VerificationNotes)
	}
	if res.ObservedPhase != PhaseImplementing {
		t.Fatalf("Implementing phase: observedPhase = %q, want %q", res.ObservedPhase, PhaseImplementing)
	}
	claim := parseClaim(t, claimPath)
	if claim["observedPhase"] != PhaseImplementing {
		t.Fatalf("claim observedPhase = %v, want %q", claim["observedPhase"], PhaseImplementing)
	}
	// The implementing prompt carries the goal (A3). The fake saw the request.
	if len(fake.Requests) == 0 {
		t.Fatal("the implementing phase must drive the model")
	}
}

func TestPhaseRunVerifyingIdlesUntilStop(t *testing.T) {
	fake := s4FakeModel()
	defer fake.Close()
	claimPath, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	// Verifying is operator-owned (ADR-0005 / B3): the runner must NOT exit
	// blocked with it (an exit would crash-loop the one-shot container under
	// restartPolicy Always). It logs, makes no model call, writes no claim,
	// and blocks until stop (SIGTERM).
	writeDesiredPhase(t, ws, "Verifying")
	stop := make(chan any, 1)

	done := make(chan Result, 1)
	go func() {
		res := PhaseRun(PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
			PollInterval: 5 * time.Millisecond, IdleTimeout: time.Hour}, stop)
		done <- res
	}()

	select {
	case <-done:
		t.Fatal("PhaseRun returned before stop with a Verifying desired phase (it must idle until SIGTERM)")
	case <-time.After(200 * time.Millisecond):
	}
	stop <- struct{}{}

	select {
	case res := <-done:
		if res.Status != "" {
			t.Fatalf("idler returned a claim (status=%q); want a zero Result (no phase executed)", res.Status)
		}
		if res.ObservedPhase != "" {
			t.Fatalf("idler returned observedPhase=%q; want empty (no claim)", res.ObservedPhase)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PhaseRun did not return after stop (SIGTERM must break the idle)")
	}
	// No model call and no claim (the operator owns Verifying; the Job, not
	// the runner, produces the verify evidence).
	if len(fake.Requests) != 0 {
		t.Fatalf("a Verifying desired phase must not drive the model (%d requests)", len(fake.Requests))
	}
	if _, err := os.Stat(claimPath); !os.IsNotExist(err) {
		t.Fatalf("the idler must not write a claim (claim path %s, stat err %v)", claimPath, err)
	}
}

func TestPhaseRunModelFailureBlocked(t *testing.T) {
	// A model failure (non-200) makes the phase end blocked: the claim
	// carries status=blocked with the reason (the operator requeues).
	fake := testhelper.New(testhelper.ModelResponse{StatusCode: 500, RawBody: `{"error":"boom"}`})
	defer fake.Close()
	claimPath, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	writeDesiredPhase(t, ws, PhasePlanning)
	stop := make(chan any)

	res := PhaseRun(PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
		PollInterval: 5 * time.Millisecond}, stop)

	if res.Status != statusBlocked {
		t.Fatalf("model failure: status = %q, want %q", res.Status, statusBlocked)
	}
	if res.ObservedPhase != PhasePlanning {
		t.Fatalf("model failure: observedPhase = %q, want %q (the phase the run was IN)", res.ObservedPhase, PhasePlanning)
	}
	claim := parseClaim(t, claimPath)
	if claim["status"] != statusBlocked {
		t.Fatalf("claim status = %v, want %q", claim["status"], statusBlocked)
	}
	if reason, _ := claim["blockedReason"].(string); reason == "" {
		t.Fatal("claim blockedReason must carry the model failure reason")
	}
}

func TestPhaseRunNoDesiredPhaseBeforeStop(t *testing.T) {
	fake := s4FakeModel()
	defer fake.Close()
	_, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	// No desired-phase file: the runner waits for the operator. A SIGTERM
	// (stop closed) before it appears returns a zero Result (no phase
	// executed) — a deleted pod must not look like a blocked run.
	stop := make(chan any)
	go func() { time.Sleep(20 * time.Millisecond); close(stop) }()

	res := PhaseRun(PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
		PollInterval: 5 * time.Millisecond}, stop)

	if res.ObservedPhase != "" {
		t.Fatalf("no desired phase before stop: observedPhase = %q, want empty", res.ObservedPhase)
	}
	if res.Status != "" {
		t.Fatalf("no desired phase before stop: status = %q, want empty (a zero Result)", res.Status)
	}
	if len(fake.Requests) != 0 {
		t.Fatalf("no phase executed: the model must not be driven (%d requests)", len(fake.Requests))
	}
}

func TestPhaseRunPlanCapTruncatesByBytes(t *testing.T) {
	// A2: the PLAN.md write is capped at planMaxBytes BYTES (the ADR-0004
	// contract is a <=4KB file). A long answer must be truncated to the cap.
	long := strings.Repeat("x", planMaxBytes*3)
	fake := testhelper.New(testhelper.ModelResponse{Content: long})
	defer fake.Close()
	_, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	writeDesiredPhase(t, ws, PhasePlanning)
	stop := make(chan any)

	res := PhaseRun(PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
		PollInterval: 5 * time.Millisecond}, stop)

	if res.Status != statusSuccess {
		t.Fatalf("status = %q, want %q (notes: %s)", res.Status, statusSuccess, res.VerificationNotes)
	}
	plan, err := os.ReadFile(filepath.Join(ws, resultDirName, planFileName))
	if err != nil {
		t.Fatalf("PLAN.md not written: %v", err)
	}
	if len(plan) > planMaxBytes {
		t.Fatalf("PLAN.md is %d bytes; the A2 cap is %d (the cap must truncate by bytes)", len(plan), planMaxBytes)
	}
}

func TestPhaseRunIterationChangeDiscardsConversation(t *testing.T) {
	// A4: the conversation is discarded when the iteration changes (the
	// operator-written .coxswain/iteration differs from the iteration the
	// conversation was written under). A new iteration starts fresh from the
	// repo.
	fake := testhelper.New(testhelper.ModelResponse{Content: "PLAN-A"}, testhelper.ModelResponse{Content: "PLAN-B"})
	defer fake.Close()
	_, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	stop := make(chan any)

	// Run 1: Planning under iteration "1" (the operator's marker file is
	// written before the phase run reads it).
	if err := os.MkdirAll(filepath.Join(ws, resultDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, resultDirName, iterationFileName), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDesiredPhase(t, ws, PhasePlanning)
	cfg := PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
		PollInterval: 5 * time.Millisecond}
	PhaseRun(cfg, stop)

	// The operator advances the iteration (a new iteration): the conversation
	// written under iteration "1" must be discarded.
	if err := os.WriteFile(filepath.Join(ws, resultDirName, iterationFileName), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Run 2: Planning under iteration "2". Its first request must NOT carry
	// run 1's assistant turn (the conversation was discarded). The prior
	// result (run 1's COMPLETED claim, same desired phase) is deleted so the
	// restart-safety re-emit does not short-circuit the phase run (in a live
	// pod a phase change recycles the pod — fresh emptyDir, fresh result.json
	// — and the conversation file is discarded on a phase boundary by the pod).
	if err := os.Remove(filepath.Join(ws, resultDirName, resultFileName)); err != nil {
		t.Fatal(err)
	}
	writeDesiredPhase(t, ws, PhasePlanning)
	cfg2 := PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
		PollInterval: 5 * time.Millisecond}
	PhaseRun(cfg2, stop)

	if len(fake.Requests) < 2 {
		t.Fatalf("the two runs must each drive the model (%d requests)", len(fake.Requests))
	}
	last := fake.Requests[len(fake.Requests)-1]
	for _, m := range last.Messages {
		if m.Role == jsonRoleAssistant && strings.Contains(m.Content, "PLAN-A") {
			t.Fatalf("A4: a changed iteration must discard the prior conversation; run 2 carried run 1's assistant turn: %+v",
				last.Messages)
		}
	}
}

// TestPhaseRunLoadedConversationDropsSystemMessage (reviewer root cause 1):
// a persisted A4 conversation that contains a system message (the previous
// run's res.toolConversation includes the fresh system prompt at index 0)
// must NOT produce a second system message in the next run's model request.
// vLLM with the qwen chat template rejects a second system message with
// HTTP 400 "System message must be at the beginning." — the claim ended
// blocked and the sandbox CrashLooped on the live run. The loader filters
// role=system so the request carries exactly one system message, at 0.
func TestPhaseRunLoadedConversationDropsSystemMessage(t *testing.T) {
	fake := testhelper.New(testhelper.ModelResponse{Content: "plan-drops"},
		testhelper.ModelResponse{Content: "plan: two"})
	defer fake.Close()
	claimPath, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	// Seed the workspace as if a prior run under iteration "1" left a
	// conversation that INCLUDES a system message (the real shape of
	// res.toolConversation: system prompt at index 0).
	if err := os.MkdirAll(filepath.Join(ws, resultDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	st := conversationState{Iteration: "1", Messages: []chatMessage{
		{Role: jsonRoleSystem, Content: "a stale system message from the prior run"},
		{Role: jsonRoleUser, Content: "earlier question"},
		{Role: jsonRoleAssistant, Content: "earlier answer"},
	}}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, resultDirName, conversationFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, resultDirName, iterationFileName), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A blocked prior result (same phase) is NOT re-emitted (it may retry)
	// — but here there is no prior result at all, which is the normal
	// fresh-pod shape for the second phase.
	writeDesiredPhase(t, ws, PhasePlanning)
	stop := make(chan any)
	cfg := PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
		PollInterval: 5 * time.Millisecond}
	res := PhaseRun(cfg, stop)
	if res.Status != statusSuccess {
		t.Fatalf("phase status = %q, want %q (notes: %s)", res.Status, statusSuccess, res.VerificationNotes)
	}
	if len(fake.Requests) != 1 {
		t.Fatalf("got %d model requests, want exactly 1", len(fake.Requests))
	}
	msgs := fake.Requests[0].Messages
	var systemIdx []int
	for i, m := range msgs {
		if m.Role == jsonRoleSystem {
			systemIdx = append(systemIdx, i)
		}
	}
	if len(systemIdx) != 1 || systemIdx[0] != 0 {
		t.Fatalf("request must carry exactly ONE system message at index 0; got %d system messages at %v",
			len(systemIdx), systemIdx)
	}
	// The stale system message content must not appear anywhere.
	for _, m := range msgs {
		if strings.Contains(m.Content, "stale system message") {
			t.Fatalf("request carried the prior run's system message content: %q", m.Content)
		}
	}
	// The non-system history must be carried (user/assistant survive the filter).
	foundUser, foundAssistant := false, false
	for _, m := range msgs {
		if m.Role == jsonRoleUser && m.Content == "earlier question" {
			foundUser = true
		}
		if m.Role == jsonRoleAssistant && m.Content == "earlier answer" {
			foundAssistant = true
		}
	}
	if !foundUser || !foundAssistant {
		t.Fatalf("loaded conversation must carry the user/assistant turns (user=%v assistant=%v): %+v",
			foundUser, foundAssistant, msgs)
	}
	if _, err := os.Stat(claimPath); err != nil {
		t.Fatalf("claim must be written: %v", err)
	}
}

// TestPhaseRunReEmitsCompletedClaimWithoutModelCall (reviewer root cause 3):
// the sandbox pod's restartPolicy is Always, so a one-shot exit restarts the
// agent container. When result.json already holds a COMPLETED claim for the
// current desired phase (status=success, observedPhase == phase), the
// restart must NOT call the model — it re-emits the prior claim to the
// termination log and exits. A BLOCKED prior claim may retry (it is NOT
// re-emitted).
func TestPhaseRunReEmitsCompletedClaimWithoutModelCall(t *testing.T) {
	fake := testhelper.New(testhelper.ModelResponse{Content: "plan-reemit"})
	defer fake.Close()
	claimPath, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	writeDesiredPhase(t, ws, PhasePlanning)
	stop := make(chan any)
	cfg := PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
		PollInterval: 5 * time.Millisecond}
	first := PhaseRun(cfg, stop)
	if first.Status != statusSuccess {
		t.Fatalf("first run status = %q, want %q (notes: %s)", first.Status, statusSuccess, first.VerificationNotes)
	}
	if len(fake.Requests) != 1 {
		t.Fatalf("first run: got %d model requests, want 1", len(fake.Requests))
	}
	// The kubelet would now restart the agent container (the pod is still
	// there; result.json + conversation.json are on the workspace volume).
	// The fresh process must re-emit the completed claim WITHOUT a model call.
	// Clear the claim file so the test asserts the RE-EMIT wrote it.
	if err := os.Remove(claimPath); err != nil {
		t.Fatal(err)
	}
	restart := PhaseRun(cfg, stop)
	if restart.Status != statusSuccess {
		t.Fatalf("restart status = %q, want %q (re-emit must carry the prior claim)", restart.Status, statusSuccess)
	}
	if len(fake.Requests) != 1 {
		t.Fatalf("restart: got %d model requests, want 1 (no new model call on a completed prior claim)", len(fake.Requests))
	}
	claim := parseClaim(t, claimPath)
	if claim["observedPhase"] != PhasePlanning || claim["status"] != statusSuccess {
		t.Fatalf("re-emitted claim = %v, want observedPhase=Planning status=success", claim)
	}
}

// TestPhaseRunBlockedClaimRetries (the companion case): a prior BLOCKED claim
// for the same desired phase is NOT re-emitted — the phase retries (the model
// failure was likely transient; the kubelet's restart back-off caps the rate).
func TestPhaseRunBlockedClaimRetries(t *testing.T) {
	fake := testhelper.New(testhelper.ModelResponse{Content: "plan-blocked"})
	defer fake.Close()
	claimPath, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	writeDesiredPhase(t, ws, PhasePlanning)
	stop := make(chan any)
	cfg := PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
		PollInterval: 5 * time.Millisecond}
	// Plant a BLOCKED prior result for the current phase (as if a prior run
	// ended blocked and the container restarted).
	blocked := Result{Status: statusBlocked, ObservedPhase: PhasePlanning, VerificationNotes: "model timeout"}
	if err := writeResult(filepath.Join(ws, resultDirName, resultFileName), blocked); err != nil {
		t.Fatal(err)
	}
	PhaseRun(cfg, stop)
	if len(fake.Requests) != 1 {
		t.Fatalf("blocked prior claim must retry the phase: got %d model requests, want 1", len(fake.Requests))
	}
	claim := parseClaim(t, claimPath)
	if claim["status"] != statusSuccess {
		t.Fatalf("after the retry, the claim must be success (the retry drove the model): %v", claim)
	}
}
