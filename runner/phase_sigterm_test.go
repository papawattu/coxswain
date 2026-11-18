// R58 I273 (D142 "separately"): the runner acts on SIGTERM DURING the model
// loop. It writes no claim and returns a zero Result immediately, so a
// deleted pod exits 0 before the kubelet's SIGKILL at the grace deadline
// (exit 137), instead of finishing (or losing) its phase mid-request.
//
// The spec's system boundary is the model API: a fake server whose handler
// blocks until the request's context is done (a model call that is still in
// flight when SIGTERM arrives). PhaseRun drives one phase against it;
// closing `stop` (SIGTERM) must cancel the in-flight request and end
// PhaseRun within the spec's 5 s bound with a zero Result: no result.json
// claim, no termination-log claim, no PLAN.md write (Planning), no commit
// (Implementing), no lesson file.
package runner

import (
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/papawattu/coxswain/runner/testhelper"
)

// s4StopFakeModel starts a fake model server whose handler blocks until the
// request's context is done (the model call is still in flight when SIGTERM
// arrives). It returns the fake (close it) and a counter of the requests
// seen.
func s4StopFakeModel(t *testing.T) (*testhelper.FakeModel, func() int) {
	t.Helper()
	fm := testhelper.New()
	var requests atomic.Int32
	fm.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		<-r.Context().Done()
	})
	return fm, func() int { return int(requests.Load()) }
}

// s4SigtermSpec runs the I273 spec for one phase against a blocking model:
// PhaseRun returns within 5 s of stop (SIGTERM), with a zero Result and
// nothing written (no result.json claim, no termination-log claim, no new
// commit; PLAN.md untouched for Planning).
func s4SigtermSpec(t *testing.T, phase string) {
	t.Helper()
	fake, requests := s4StopFakeModel(t)
	claimPath, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	writeDesiredPhase(t, ws, phase)
	baseHead := ""
	if phase == PhaseImplementing {
		// A real workspace repo with one base commit: the cancelled run
		// must leave HEAD where it found it (S5a commits only a successful
		// run, and this one was not one).
		initTestRepo(t, ws)
		baseHead = gitIn(t, ws, "rev-parse", "HEAD")
	}
	if phase == PhasePlanning {
		// A PLAN.md the user already approved: the cancelled run must leave
		// it unchanged (F43 P2-1's harm, reached by a stop).
		if err := os.WriteFile(filepath.Join(ws, resultDirName, planFileName),
			[]byte("the approved plan"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stop := make(chan any, 1)

	done := make(chan Result, 1)
	go func() {
		done <- PhaseRun(PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
			PollInterval: 5 * time.Millisecond}, stop)
	}()

	// Let the request be in flight, then SIGTERM it.
	time.Sleep(200 * time.Millisecond)
	stop <- struct{}{}
	close(stop)

	var res Result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("PhaseRun did not return within 5 s of SIGTERM during the model loop")
	}

	// The zero Result is not comparable (Result carries a []string
	// toolConversation), so check every public claim field: a zero Result
	// has none of them set.
	if res.Status != "" || res.ObservedPhase != "" || res.Summary != "" || res.HeadCommit != "" ||
		res.VerificationNotes != "" || res.NextIterationPlan != "" || res.Iteration != 0 || res.PlanAttempt != 0 ||
		res.PlanChanged || res.PlanMismatch || res.LessonsAvailable || len(res.FilesChanged) != 0 ||
		len(res.Lessons) != 0 || len(res.ToolTrace) != 0 {
		t.Fatalf("stop during %s returned %+v; want a zero Result (no phase executed, no claim)", phase, res)
	}
	// No result.json claim on the workspace volume: the early return for a
	// stop lands BEFORE the claim channel (the A4 conversation write and the
	// result.json write below it in PhaseRun), so no claim file may appear.
	if _, err := os.Stat(filepath.Join(ws, resultDirName, resultFileName)); !os.IsNotExist(err) {
		t.Fatalf("stop during %s wrote result.json (stat err %v); want no claim file", phase, err)
	}
	// No termination-log claim.
	if _, err := os.Stat(claimPath); !os.IsNotExist(err) {
		t.Fatalf("stop during %s wrote a termination-log claim (stat err %v); want none", phase, err)
	}
	// No lesson file (P5b writes it on a successful Implementing; a stop is
	// not one).
	if _, err := os.ReadFile(filepath.Join(ws, resultDirName, lessonsFileName)); !os.IsNotExist(err) {
		t.Fatalf("stop during %s wrote a lessons file; want none", phase)
	}
	if phase == PhasePlanning {
		plan, err := os.ReadFile(filepath.Join(ws, resultDirName, planFileName))
		if err != nil {
			t.Fatalf("PLAN.md disappeared after stop: %v", err)
		}
		if string(plan) != "the approved plan" {
			t.Fatalf("PLAN.md changed after stop: %q", string(plan))
		}
	} else {
		// No new commit: HEAD is still the base commit the spec made.
		if head := gitIn(t, ws, "rev-parse", "HEAD"); head != baseHead {
			t.Fatalf("stop during %s committed: HEAD moved from %s to %s", phase, baseHead, head)
		}
	}
	if n := requests(); n == 0 {
		t.Fatalf("the %s phase must drive the model (the spec's SIGTERM lands mid-request)", phase)
	}
}

func TestPhaseRunStopDuringPlanningModelLoop(t *testing.T) {
	s4SigtermSpec(t, PhasePlanning)
}

func TestPhaseRunStopDuringImplementingModelLoop(t *testing.T) {
	s4SigtermSpec(t, PhaseImplementing)
}

// R58 I273 P2: a SIGTERM during a shell tool command ends the command
// promptly. execShell now derives its timeout from the phase's stop ctx,
// so a stop cancels the in-flight shell command (sleep 20) instead of
// waiting for it to finish. PhaseRun must return within 5 s with a zero
// Result (no claim, no commit).
func TestPhaseRunStopDuringShellCommand(t *testing.T) {
	fake := testhelper.New(testhelper.ModelResponse{
		ToolCall: &testhelper.ToolCall{
			ID:        "call_1",
			Name:      "shell",
			Arguments: `{"command":"sleep 20"}`,
		},
	})
	defer fake.Close()
	claimPath, cleanup := s4ClaimPath(t)
	defer cleanup()

	ws := t.TempDir()
	writeDesiredPhase(t, ws, PhaseImplementing)
	initTestRepo(t, ws)
	baseHead := gitIn(t, ws, "rev-parse", "HEAD")

	stop := make(chan any, 1)
	done := make(chan Result, 1)
	start := time.Now()
	go func() {
		done <- PhaseRun(PhaseConfig{Workspace: ws, Goal: "g", BaseURL: fake.URL, Model: "m",
			PollInterval: 5 * time.Millisecond}, stop)
	}()

	// Let the shell command (sleep 20) be in flight, then SIGTERM it.
	time.Sleep(time.Second)
	stop <- struct{}{}
	close(stop)

	var res Result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("PhaseRun did not return within 5 s of SIGTERM during a shell command")
	}
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("PhaseRun returned %s after start (stop at 1s); want within 5 s", elapsed.Round(time.Millisecond).String())
	}

	// Zero Result (no claim, no phase executed).
	if res.Status != "" || res.ObservedPhase != "" || res.Summary != "" || res.HeadCommit != "" ||
		res.VerificationNotes != "" || res.NextIterationPlan != "" || res.Iteration != 0 || res.PlanAttempt != 0 ||
		res.PlanChanged || res.PlanMismatch || res.LessonsAvailable || len(res.FilesChanged) != 0 ||
		len(res.Lessons) != 0 || len(res.ToolTrace) != 0 {
		t.Fatalf("stop during shell command returned %+v; want a zero Result (no claim)", res)
	}
	// No result.json claim.
	if _, err := os.Stat(filepath.Join(ws, resultDirName, resultFileName)); !os.IsNotExist(err) {
		t.Fatalf("stop during shell command wrote result.json (stat err %v); want no claim file", err)
	}
	// No termination-log claim.
	if _, err := os.Stat(claimPath); !os.IsNotExist(err) {
		t.Fatalf("stop during shell command wrote a termination-log claim (stat err %v); want none", err)
	}
	// No new commit: HEAD is still the base commit.
	if head := gitIn(t, ws, "rev-parse", "HEAD"); head != baseHead {
		t.Fatalf("stop during shell command committed: HEAD moved from %s to %s", baseHead, head)
	}
}
