package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/papawattu/coxswain/runner/testhelper"
)

// I5: a non-200 model response (401) must surface as a blocked result with the
// HTTP status in the notes — not a confusing "no choices in response".
func TestRunnerSurfacesHTTPErrorAsBlocked(t *testing.T) {
	fake := testhelper.New(testhelper.ModelResponse{
		StatusCode: 401,
		RawBody:    `{"error":{"message":"invalid api key"}}`,
	})
	defer fake.Close()

	workdir := t.TempDir()
	res := run(runConfig{
		Prompt:    doSomethingPrompt,
		Workspace: workdir,
		BaseURL:   fake.URL,
		Model:     fakeModelName,
	})

	if res.Status != statusBlocked {
		t.Fatalf("status = %q, want blocked (a 401 must not read as success)", res.Status)
	}
	if !strings.Contains(res.VerificationNotes, "401") {
		t.Errorf("verificationNotes = %q; want it to mention the HTTP 401", res.VerificationNotes)
	}
	if !strings.Contains(res.VerificationNotes, "invalid api key") {
		t.Errorf("verificationNotes = %q; want the server's error body to be visible", res.VerificationNotes)
	}
}

// I5 (refined I12): a chatty shell command's output must be truncated before it
// is fed back to the model (cap ~16 KiB), keeping the head and the tail with an
// elided marker (the useful part of a build/test is usually at the end).
func TestRunnerTruncatesLongToolOutput(t *testing.T) {
	workdir := t.TempDir()
	// Print a distinctive head, then a lot of filler, then a distinctive tail.
	// After truncation the head and the tail must both survive, with an elided
	// marker between them.
	fake := testhelper.New(
		testhelper.ModelResponse{
			ToolCall: &testhelper.ToolCall{
				ID:        "call_big",
				Name:      toolNameShell,
				Arguments: `{"command":"printf 'HEAD-MARKER'; yes FILLER | head -n 200000; printf 'TAIL-MARKER'"}`,
			},
		},
	)
	defer fake.Close()

	run(runConfig{
		Prompt:    "print a lot",
		Workspace: workdir,
		BaseURL:   fake.URL,
		Model:     fakeModelName,
	})

	if len(fake.Requests) < 2 {
		t.Fatalf("expected >=2 requests, got %d", len(fake.Requests))
	}
	toolMsgs := fake.Requests[1].ToolResultMessages()
	if len(toolMsgs) == 0 {
		t.Fatalf("no tool-result message in request 2")
	}
	got := toolMsgs[0].Content
	if len(got) > maxToolOutputBytes+64 { // + slack for the elided marker
		t.Errorf("tool output length = %d bytes; want <= ~%d (truncated)", len(got), maxToolOutputBytes)
	}
	if !strings.Contains(got, "HEAD-MARKER") {
		t.Errorf("tool output lost the head; want the head kept on truncation")
	}
	if !strings.Contains(got, "TAIL-MARKER") {
		t.Errorf("tool output lost the tail; I12 wants head+tail kept on truncation")
	}
	if !strings.Contains(got, "bytes elided") {
		t.Errorf("tool output has no elided marker; want a marker between head and tail")
	}
}

// I5: an unknown tool name must be rejected, not executed as a shell command.
func TestRunnerRejectsUnknownTool(t *testing.T) {
	workdir := t.TempDir()
	// First the model calls an unknown tool "rmrf"; then it gives a final
	// answer. The unknown tool must NOT create the marker (it wasn't run).
	fake := testhelper.New(
		testhelper.ModelResponse{
			SkipAdvertiseCheck: true, // the runner only advertises "shell"; force the
			// unknown "rmrf" tool call through so we can test rejection.
			ToolCall: &testhelper.ToolCall{
				ID:        "call_unknown",
				Name:      "rmrf",
				Arguments: `{"command":"echo x > marker.txt"}`,
			},
		},
	)
	defer fake.Close()

	res := run(runConfig{
		Prompt:    "use a bad tool",
		Workspace: workdir,
		BaseURL:   fake.URL,
		Model:     fakeModelName,
	})

	// The unknown tool must not have executed: marker.txt must not exist.
	if _, err := os.Stat(filepath.Join(workdir, "marker.txt")); !os.IsNotExist(err) {
		t.Fatalf("unknown tool was executed (marker.txt exists); want it rejected")
	}

	// The tool error must have been fed back to the model.
	if len(fake.Requests) < 2 {
		t.Fatalf("expected >=2 requests, got %d", len(fake.Requests))
	}
	toolMsgs := fake.Requests[1].ToolResultMessages()
	if len(toolMsgs) == 0 {
		t.Fatalf("no tool-result message fed back for the unknown tool")
	}
	if !strings.Contains(toolMsgs[0].Content, "unknown tool") {
		t.Errorf("tool message = %q; want an 'unknown tool' error fed back", toolMsgs[0].Content)
	}
	_ = res
}

// I5: maxSteps is configurable. With MaxSteps=1 and a model that keeps
// tool-calling, the runner stops (blocked, maxSteps note) instead of looping.
func TestRunnerRespectsConfigurableMaxSteps(t *testing.T) {
	// Queue more tool calls than the (tiny) step budget so the loop must stop
	// at maxSteps.
	fake := testhelper.New(
		testhelper.ModelResponse{
			ToolCall: &testhelper.ToolCall{ID: "c1", Name: toolNameShell, Arguments: `{"command":"echo 1"}`},
		},
		testhelper.ModelResponse{
			ToolCall: &testhelper.ToolCall{ID: "c2", Name: toolNameShell, Arguments: `{"command":"echo 2"}`},
		},
	)
	defer fake.Close()

	res := run(runConfig{
		Prompt:    "loop",
		Workspace: t.TempDir(),
		BaseURL:   fake.URL,
		Model:     fakeModelName,
		MaxSteps:  1,
	})

	// With a budget of 1 step and 2 tool-calls queued, the loop must stop at
	// maxSteps (blocked) — not run to the end or hang.
	if res.Status != statusBlocked {
		t.Fatalf("status = %q, want blocked (maxSteps reached)", res.Status)
	}
	if !strings.Contains(res.VerificationNotes, "maxSteps") {
		t.Errorf("verificationNotes = %q; want a maxSteps note", res.VerificationNotes)
	}
}

// I10: a command that leaves a background child running (a dev server, a
// `sleep & wait`) must not hang the runner past the shell timeout.
// CommandContext kills only `sh`; the child inherits the pipe and
// CombinedOutput waits for its EOF (which for a server is never). Fix: run the
// command in its own process group, kill the whole group on timeout, and use
// WaitDelay so Wait stops waiting on stragglers. Seam: runConfig.ShellTimeout
// (injected ~1s) + the wall time run() takes + the result file.
func TestRunnerShellTimeoutKillsBackgroundChildren(t *testing.T) {
	fake := testhelper.New(
		testhelper.ModelResponse{
			ToolCall: &testhelper.ToolCall{
				ID:        "call_bg",
				Name:      toolNameShell,
				Arguments: `{"command":"sleep 30 & wait"}`,
			},
		},
	)
	defer fake.Close()

	done := make(chan struct{})
	var res Result
	start := time.Now()
	go func() {
		res = run(runConfig{
			Prompt:       "start a server",
			Workspace:    t.TempDir(),
			BaseURL:      fake.URL,
			Model:        fakeModelName,
			ShellTimeout: 1 * time.Second,
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("run() did not return within 5s; the shell timeout did not kill the " +
			"background child (hangs until the child exits)")
	}

	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("run() took %v; the 1s shell timeout + group kill should return in "+
			"~1-2s, not wait for the 30s child", elapsed)
	}
	if res.Status != statusSuccess && res.Status != statusBlocked {
		t.Fatalf("status = %q, want success or blocked", res.Status)
	}
	// I13: the timeout must be visible to the model — the tool-result message fed
	// back in request 2 (after the tool call) must carry the kill/exit text, so
	// the model knows the command was cut off rather than succeeding.
	if len(fake.Requests) < 2 {
		t.Fatalf("expected >=2 requests (tool call + tool result), got %d", len(fake.Requests))
	}
	toolMsgs := fake.Requests[1].ToolResultMessages()
	if len(toolMsgs) == 0 {
		t.Fatalf("no tool-result message in request 2")
	}
	got := toolMsgs[0].Content
	if !strings.Contains(got, "signal: killed") && !strings.Contains(got, "exit:") &&
		!strings.Contains(got, "timeout") && !strings.Contains(got, "killed") {
		t.Errorf("fed-back tool message = %q; want the timeout/kill to be visible to the model (signal: killed / exit)", got)
	}
}

// I10 (success path): a command that succeeds but detaches a background child
// must not leak that child across steps — the process group is killed after the
// command returns too, so the pipe is not held open.
func TestRunnerShellTimeoutKillsChildAfterSuccess(t *testing.T) {
	fake := testhelper.New(
		testhelper.ModelResponse{
			ToolCall: &testhelper.ToolCall{
				ID:        "call_bg2",
				Name:      toolNameShell,
				Arguments: `{"command":"(sleep 30 &) ; echo started"}`,
			},
		},
	)
	defer fake.Close()

	done := make(chan struct{})
	start := time.Now()
	go func() {
		res := run(runConfig{
			Prompt:       "detach a bg job",
			Workspace:    t.TempDir(),
			BaseURL:      fake.URL,
			Model:        fakeModelName,
			ShellTimeout: 2 * time.Second,
		})
		_ = res
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("run() did not return within 5s; the post-success group kill did not " +
			"release the pipe held by the background child")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("run() took %v; the post-success group kill should release the "+
			"pipe quickly", elapsed)
	}
}

// I11: the model HTTP timeout must be configurable (a local 27B-class model or
// a long-context request can exceed the old hard 60s). With a ModelTimeout
// smaller than the model's response delay, run() must time out -> blocked with a
// timeout message. With a ModelTimeout larger than the delay, it must succeed.
func TestRunnerModelTimeoutConfigurable(t *testing.T) {
	// Case 1: a 500ms delay against a 200ms timeout -> blocked (timeout).
	fakeSlow := testhelper.New(testhelper.ModelResponse{
		Content: "slow answer",
		Delay:   500 * time.Millisecond,
	})
	defer fakeSlow.Close()

	slowRes := run(runConfig{
		Prompt:       "think hard",
		Workspace:    t.TempDir(),
		BaseURL:      fakeSlow.URL,
		Model:        fakeModelName,
		ModelTimeout: 200 * time.Millisecond,
	})
	if slowRes.Status != statusBlocked {
		t.Fatalf("slow case: status = %q, want blocked (timeout)", slowRes.Status)
	}
	if !strings.Contains(slowRes.VerificationNotes, "timeout") &&
		!strings.Contains(slowRes.VerificationNotes, "deadline") {
		t.Errorf("slow case: verificationNotes = %q; want a timeout/deadline message", slowRes.VerificationNotes)
	}

	// Case 2: a 100ms delay against a 5s timeout -> success.
	fakeFast := testhelper.New(testhelper.ModelResponse{
		Content: "quick answer",
		Delay:   100 * time.Millisecond,
	})
	defer fakeFast.Close()

	fastRes := run(runConfig{
		Prompt:       "think",
		Workspace:    t.TempDir(),
		BaseURL:      fakeFast.URL,
		Model:        fakeModelName,
		ModelTimeout: 5 * time.Second,
	})
	if fastRes.Status != statusSuccess {
		t.Fatalf("fast case: status = %q, want success", fastRes.Status)
	}
	if fastRes.Summary != "quick answer" {
		t.Errorf("fast case: summary = %q, want the model's answer", fastRes.Summary)
	}
}
