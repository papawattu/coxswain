package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// R4: When the model endpoint is unreachable, the runner still writes a
// result.json and does NOT report success. Seam: the result file.
func TestRunnerWritesBlockedResultOnModelFailure(t *testing.T) {
	workdir := t.TempDir()

	// A base URL that points at a dead port — the model call cannot succeed.
	run(runConfig{
		Prompt:    doSomethingPrompt,
		Workspace: workdir,
		BaseURL:   "http://127.0.0.1:1", // port 1 is effectively unreachable
		Model:     fakeModelName,
	})

	resultPath := filepath.Join(workdir, ".coxswain", "result.json")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("result.json not written on model failure (runner must always write): %v", err)
	}

	var res Result
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatalf("result.json malformed: %v", err)
	}
	if res.Status == "success" {
		t.Fatalf("status = success after the model was unreachable; want blocked. trace=%v", res.ToolTrace)
	}
	if res.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", res.Status)
	}
}
