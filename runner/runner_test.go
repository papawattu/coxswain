package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/papawattu/coxswain/runner/testhelper"
)

// fakeModelName is the model string the fake server is configured with. It is
// a test fixture (I8) — it lives in the test package, not runner.go.
const fakeModelName = "fake-model"

// R1: Given a prompt, the runner writes a result.json with
// status:"success" and a non-empty summary. The model is a fake (the one
// system boundary we mock); the seam is the result file, not the model call.
func TestRunnerWritesSuccessResult(t *testing.T) {
	fake := testhelper.New() // default: a plain final answer "done"
	defer fake.Close()

	workdir := t.TempDir()

	res := run(runConfig{
		Prompt:    "make the failing test pass",
		Workspace: workdir,
		BaseURL:   fake.URL,
		Model:     fakeModelName,
	})

	if res.Status != "success" {
		t.Fatalf("status = %q, want success", res.Status)
	}
	if res.Summary == "" {
		t.Fatalf("summary empty, want non-empty")
	}

	// The seam: the result must actually be on disk at the canonical path.
	resultPath := filepath.Join(workdir, ".coxswain", "result.json")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("result.json not written at %s: %v", resultPath, err)
	}
	if len(data) == 0 {
		t.Fatalf("result.json is empty")
	}
}
