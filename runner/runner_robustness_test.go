package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// I5: a chatty shell command's output must be truncated before it is fed back
// to the model (cap ~16 KiB), so the context can't be blown.
func TestRunnerTruncatesLongToolOutput(t *testing.T) {
	workdir := t.TempDir()
	fake := testhelper.New(
		testhelper.ModelResponse{
			ToolCall: &testhelper.ToolCall{
				ID:        "call_big",
				Name:      toolNameShell,
				Arguments: `{"command":"yes A | head -n 200000"}`,
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

	// The tool message fed back (request 2) must be bounded, not 200k chars.
	if len(fake.Requests) < 2 {
		t.Fatalf("expected >=2 requests, got %d", len(fake.Requests))
	}
	toolMsgs := fake.Requests[1].ToolResultMessages()
	if len(toolMsgs) == 0 {
		t.Fatalf("no tool-result message in request 2")
	}
	got := len(toolMsgs[0].Content)
	if got > maxToolOutputBytes+64 { // + slack for the truncation marker
		t.Errorf("tool output length = %d bytes; want <= ~%d (truncated)", got, maxToolOutputBytes)
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
