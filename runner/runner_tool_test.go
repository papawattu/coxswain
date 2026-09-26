package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/papawattu/coxswain/runner/testhelper"
)

// R3: When the model returns a shell tool call, the runner executes it in the
// workspace and feeds the command's output back to the model as a tool message.
// Seams: the workspace (the command's side effect) and the fake model (the tool
// result appears in a later request). We never assert on the exec internals.
func TestRunnerExecutesShellToolAndFeedsOutputBack(t *testing.T) {
	workdir := t.TempDir()
	marker := filepath.Join(workdir, "marker.txt")

	// The model first asks to run a command that creates marker.txt, then gives
	// a final answer. The command's output ("created") must come back to the
	// model in a subsequent request.
	fake := testhelper.New(
		testhelper.ModelResponse{
			ToolCall: &testhelper.ToolCall{
				ID:        "call_1",
				Name:      toolNameShell,
				Arguments: `{"command":"echo created > marker.txt"}`,
			},
		},
	)
	defer fake.Close()

	run(runConfig{
		Prompt:    "create marker.txt",
		Workspace: workdir,
		BaseURL:   fake.URL,
		Model:     "fake-model",
	})

	// Seam 1: the command actually ran in the workspace.
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker.txt not created by the shell tool: %v", err)
	}

	// Seam 2: the runner sent the tool result back to the model. The second
	// request (after the tool call) must contain a "tool" message.
	if len(fake.Requests) < 2 {
		t.Fatalf("expected >=2 model requests (initial + after tool call), got %d", len(fake.Requests))
	}
	toolMsgs := fake.Requests[1].ToolResultMessages()
	if len(toolMsgs) == 0 {
		t.Fatalf("no tool-result message fed back to the model in request 2; messages=%+v", fake.Requests[1].Messages)
	}
}
