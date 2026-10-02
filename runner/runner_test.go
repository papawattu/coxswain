package runner

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/papawattu/coxswain/runner/testhelper"
)

// fakeModelName is the model string the fake server is configured with. It is
// a test fixture (I8) — it lives in the test package, not runner.go.
const fakeModelName = "fake-model"

// doSomethingPrompt is a throwaway prompt used across several tests (goconst
// wants it shared rather than repeated). It is a test fixture, not protocol.
const doSomethingPrompt = "do something"

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

// ExtraBody is merged into every chat-completions request body: server tuning
// knobs that are not part of the standard OpenAI schema (e.g. Qwen's
// chat_template_kwargs that disable the reasoning pass) reach the wire, while
// the standard fields are untouched.
func TestRunnerExtraBodyMergedIntoRequests(t *testing.T) {
	fake := testhelper.New() // default: a plain final answer "done"
	defer fake.Close()

	var bodies [][]byte
	fake.SetHandler(fake.RecordHandler(&bodies))

	workdir := t.TempDir()
	res := run(runConfig{
		Prompt:    doSomethingPrompt,
		Workspace: workdir,
		BaseURL:   fake.URL,
		Model:     fakeModelName,
		ExtraBody: map[string]any{
			"chat_template_kwargs": map[string]any{"enable_thinking": false},
			"max_tokens":           200,
		},
	})
	if res.Status != "success" {
		t.Fatalf("status = %q, want success", res.Status)
	}

	if len(bodies) == 0 {
		t.Fatal("no request bodies recorded")
	}
	for i, raw := range bodies {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("request %d: bad JSON: %v", i, err)
		}
		if m["model"] != fakeModelName {
			t.Fatalf("request %d: model = %v, want %q (ExtraBody must not clobber standard fields)", i, m["model"], fakeModelName)
		}
		if _, ok := m["messages"]; !ok {
			t.Fatalf("request %d: messages missing", i)
		}
		if _, ok := m["tools"]; !ok {
			t.Fatalf("request %d: tools missing", i)
		}
		ctk, ok := m["chat_template_kwargs"].(map[string]any)
		if !ok || ctk["enable_thinking"] != false {
			t.Fatalf("request %d: chat_template_kwargs.enable_thinking = %v, want false", i, m["chat_template_kwargs"])
		}
		if m["max_tokens"] != float64(200) {
			t.Fatalf("request %d: max_tokens = %v, want 200", i, m["max_tokens"])
		}
	}
}

// The runner POSTs the OpenAI-compatible path /v1/chat/completions against the
// base URL (the model proxy is a transparent reverse proxy; vLLM serves its
// API under /v1). The /chat/completions path 404s — this pins the path the
// runner sends so the 404 cannot silently return.
func TestRunnerPostsV1ChatCompletionsPath(t *testing.T) {
	fake := testhelper.New()
	defer fake.Close()

	// Anything that is NOT /v1/chat/completions 404s (the vLLM error the
	// acceptance run hit); the real path is served by the fake's default
	// handler, so a 200 completion proves the path was right. Every request
	// path is recorded so the first one is assertable.
	var paths []string
	fake.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, `{"detail":"Not Found"}`, http.StatusNotFound)
			return
		}
		fake.DefaultHandler()(w, r)
	})

	workdir := t.TempDir()
	res := run(runConfig{
		Prompt:    doSomethingPrompt,
		Workspace: workdir,
		BaseURL:   fake.URL,
		Model:     fakeModelName,
	})
	if res.Status != "success" {
		t.Fatalf("status = %q (verificationNotes: %s), want success — the runner must POST /v1/chat/completions", res.Status, res.VerificationNotes)
	}
	if res.Summary == "" {
		t.Fatalf("summary empty, want non-empty")
	}
	if len(paths) == 0 || paths[0] != "/v1/chat/completions" {
		t.Fatalf("request paths = %v, want [/v1/chat/completions] first", paths)
	}
}
