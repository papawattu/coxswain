package runner

import (
	"testing"

	"github.com/papawattu/coxswain/runner/testhelper"
)

// R2: The runner sends the prompt as the USER message (not system, not
// dropped), for the configured model, to the model endpoint.
// Seam: the fake model's recorded request body — the system boundary we mock.
func TestRunnerSendsPromptAsUserMessage(t *testing.T) {
	fake := testhelper.New()
	defer fake.Close()

	const prompt = "refactor the auth middleware"
	run(runConfig{
		Prompt:    prompt,
		Workspace: t.TempDir(),
		BaseURL:   fake.URL,
		Model:     "fake-model",
	})

	if len(fake.Requests) == 0 {
		t.Fatal("model was never called")
	}
	req := fake.Requests[0]
	if req.Model != "fake-model" {
		t.Errorf("model = %q, want fake-model", req.Model)
	}

	var sawUser, sawSystem bool
	for _, msg := range req.Messages {
		if msg.Role == "user" && msg.Content == prompt {
			sawUser = true
		}
		if msg.Role == "system" && msg.Content == prompt {
			sawSystem = true
		}
	}
	if !sawUser {
		t.Fatalf("prompt not sent as a user message; messages=%+v", req.Messages)
	}
	if sawSystem {
		t.Fatalf("prompt leaked into the system message; messages=%+v", req.Messages)
	}
}
