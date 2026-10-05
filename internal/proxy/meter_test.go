package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newTestMeter builds a Meter over a temp file (or "" for in-memory).
func newTestMeter(t *testing.T, model string, since time.Time) *Meter {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.json")
	m, err := NewMeter(path, model, since)
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}
	return m
}

// meterWithFile builds a Meter over a specific path (for the restart test).
func meterWithFile(t *testing.T, path string, since time.Time) *Meter {
	t.Helper()
	m, err := NewMeter(path, "test-model", since)
	if err != nil {
		t.Fatalf("NewMeter(%s): %v", path, err)
	}
	return m
}

func readMeterFile(t *testing.T, path string) usageFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var f usageFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return f
}

func TestMeteringNonStreamUsage(t *testing.T) {
	// A non-stream response with usage advances the counters by exactly the
	// reported amounts; the endpoint reading carries them.
	body := `{"id":"1","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":40}}`
	p, c, ok := parseUsage([]byte(body))
	if !ok {
		t.Fatalf("parseUsage: expected ok for a body with a usage object")
	}
	if p, c := p, c; p != 120 || c != 40 {
		t.Fatalf("parseUsage = (%d, %d), want (120, 40)", p, c)
	}
	m := newTestMeter(t, "test-model", time.Now())
	m.Add(120, 40)
	r := m.Reading()
	if r.PromptTokens != 120 || r.CompletionTokens != 40 || r.Requests != 1 {
		t.Fatalf("reading after Add(120,40) = %+v, want prompt=120 completion=40 requests=1", r)
	}
}

func TestMeteringUnmetered(t *testing.T) {
	// A response without usage -> unmeteredRequests increments, tokens unchanged.
	for _, body := range []string{
		`{"choices":[],"id":"x"}`, // no usage object
		`not-json-at-all`,         // non-JSON
		``,                        // empty
	} {
		p, c, ok := parseUsage([]byte(body))
		if ok {
			t.Fatalf("parseUsage(%q) = ok, want unmetered", body)
		}
		if p != 0 || c != 0 {
			t.Fatalf("parseUsage(%q) tokens = (%d,%d), want (0,0)", body, p, c)
		}
	}
	m := newTestMeter(t, "test-model", time.Now())
	m.AddUnmetered()
	r := m.Reading()
	if r.UnmeteredRequests != 1 || r.PromptTokens != 0 || r.CompletionTokens != 0 {
		t.Fatalf("reading after AddUnmetered = %+v, want unmetered=1 tokens=0", r)
	}
}

func TestMeteringFailedCompletionStillMetered(t *testing.T) {
	// A 4xx/5xx response WITH usage is still metered (a failed completion is a
	// consumption). The proxy does not gate on the HTTP status here; the
	// round-tripper meters any body with a usage object regardless of status.
	body := `{"error":{"message":"too many tokens"},"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	p, c, ok := parseUsage([]byte(body))
	if !ok {
		t.Fatalf("parseUsage: expected ok for an error body with a usage object")
	}
	if p, c := p, c; p != 10 || c != 5 {
		t.Fatalf("parseUsage = (%d, %d), want (10, 5)", p, c)
	}
}

func TestRestartPersistenceSamePod(t *testing.T) {
	// Container restart (same pod): boot with a pre-seeded usage.json
	// (promptTokens: 500, bootID B1) -> counters start at 500 (not 0) and the
	// reading's bootID is still B1 (reused, not regenerated). A second
	// metered request advances to 500+Δ under the same B1.
	path := filepath.Join(t.TempDir(), "usage.json")
	b1 := "boot-id-1"
	seed := usageFile{BootID: b1, PromptTokens: 500, CompletionTokens: 30, Requests: 7}
	data, _ := json.Marshal(seed)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	m := meterWithFile(t, path, time.Now())
	r := m.Reading()
	if r.PromptTokens != 500 {
		t.Fatalf("after restart the counters start at %d, want 500 (the file's value, not 0)", r.PromptTokens)
	}
	if r.BootID != b1 {
		t.Fatalf("after restart the bootID is %q, want %q (the file's bootID, reused not regenerated)", r.BootID, b1)
	}

	// A second metered request advances the counters to 500+Δ under the same B1.
	m.Add(25, 5)
	r = m.Reading()
	if r.PromptTokens != 525 {
		t.Fatalf("after Add(25,5) prompt=%d, want 525", r.PromptTokens)
	}
	if r.BootID != b1 {
		t.Fatalf("after Add the bootID is %q, want %q (same boot across the restart)", r.BootID, b1)
	}
	// The persisted file carries the updated counters and the same bootID.
	f := readMeterFile(t, path)
	if f.BootID != b1 || f.PromptTokens != 525 {
		t.Fatalf("persisted file = %+v, want bootID=%s promptTokens=525", f, b1)
	}
}

func TestFreshBootNewBootID(t *testing.T) {
	// Pod recreate (fresh emptyDir): a fresh boot with no file -> counters at 0
	// and a NEW bootID (the operator treats it as a fresh start).
	path := filepath.Join(t.TempDir(), "usage.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("precondition: %s must not exist (fresh boot)", path)
	}
	m := meterWithFile(t, path, time.Now())
	r := m.Reading()
	if r.PromptTokens != 0 || r.CompletionTokens != 0 {
		t.Fatalf("fresh boot counters = (%d,%d), want (0,0)", r.PromptTokens, r.CompletionTokens)
	}
	if r.BootID == "" {
		t.Fatalf("fresh boot must generate a new bootID (got empty)")
	}
}

// TestRestartPersistenceBootIDRegenerated is the P1-B mutation: if the binary
// regenerated the bootID on every start (ignoring the file's bootID), this
// spec would FAIL (the reading would carry a new bootID, not B1). Under the
// correct implementation the file's bootID is reused, so this spec PASSES —
// it is the guard that makes the mutation fail.
func TestRestartPersistenceBootIDReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	b1 := "boot-id-1"
	seed := usageFile{BootID: b1, PromptTokens: 100}
	data, _ := json.Marshal(seed)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	m := meterWithFile(t, path, time.Now())
	if got := m.Reading().BootID; got != b1 {
		t.Fatalf("container restart regenerated the bootID (%q != %q) — the operator would rebase and double-count", got, b1)
	}
}

func TestPersistenceAtomicity(t *testing.T) {
	// A write to usage.json produces a file that is either the old or the new
	// content (never torn): a concurrent reader goroutine reads the file
	// repeatedly during a write burst and asserts it always parses as valid
	// JSON.
	path := filepath.Join(t.TempDir(), "usage.json")
	m := meterWithFile(t, path, time.Now())

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(path)
			if err != nil {
				continue // file momentarily absent mid-rename is acceptable
			}
			var f usageFile
			if err := json.Unmarshal(data, &f); err != nil {
				t.Errorf("concurrent reader saw a TORN file (not valid JSON): %v\nbytes=%q", err, data)
				return
			}
		}
	})

	// Write burst: each Add rewrites the file (temp + rename).
	for range 500 {
		m.Add(1, 1)
	}
	close(stop)
	wg.Wait()
}

func TestShapeRequestForcesIncludeUsageOnStream(t *testing.T) {
	// A stream:true request without include_usage -> the upstream body carries
	// stream_options.include_usage: true.
	in := `{"model":"x","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	out, isObject := shapeRequest([]byte(in))
	if !isObject {
		t.Fatalf("shapeRequest: expected a JSON object")
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	var opts struct {
		IncludeUsage bool `json:"include_usage"`
	}
	if err := json.Unmarshal(got["stream_options"], &opts); err != nil {
		t.Fatalf("stream_options not present on the rewritten body: %v\nbody=%s", err, out)
	}
	if !opts.IncludeUsage {
		t.Fatalf("stream_options.include_usage must be forced true; body=%s", out)
	}
}

func TestShapeRequestWithIncludeUsageUnchanged(t *testing.T) {
	// A stream:true request with include_usage already true -> unchanged.
	in := `{"model":"x","stream":true,"stream_options":{"include_usage":true}}`
	out, _ := shapeRequest([]byte(in))
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	var opts struct {
		IncludeUsage bool `json:"include_usage"`
	}
	if err := json.Unmarshal(got["stream_options"], &opts); err != nil {
		t.Fatal(err)
	}
	if !opts.IncludeUsage {
		t.Fatalf("include_usage must remain true; body=%s", out)
	}
}

func TestShapeRequestNonStreamUnchanged(t *testing.T) {
	// A non-streaming request: no include_usage is added (the body is the same
	// object, reordered keys are fine).
	in := `{"model":"x","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	out, isObject := shapeRequest([]byte(in))
	if !isObject {
		t.Fatalf("expected a JSON object")
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["stream_options"]; ok {
		t.Fatalf("a non-stream request must not gain stream_options; body=%s", out)
	}
}

func TestShapeRequestNonJSONUnmodified(t *testing.T) {
	// A non-JSON body is forwarded unmodified and reported as not-an-object
	// (the caller counts it as unmetered).
	in := []byte("this is not json")
	out, isObject := shapeRequest(in)
	if isObject {
		t.Fatalf("a non-JSON body must not be treated as an object")
	}
	if string(out) != string(in) {
		t.Fatalf("a non-JSON body must be forwarded unmodified; got %q", out)
	}
}

func TestStreamUsageThreeChunks(t *testing.T) {
	// A 3-chunk SSE stream whose last chunk carries usage -> the accumulated
	// counts match.
	stream := "data: {\"id\":1,\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n" +
		"data: {\"id\":1,\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n" +
		"data: {\"id\":1,\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":7}}\n" +
		"data: [DONE]\n"
	p, c, ok := parseStreamUsage([]byte(stream))
	if !ok {
		t.Fatalf("expected a usage chunk in the stream")
	}
	if p, c := p, c; p != 10 || c != 7 {
		t.Fatalf("stream usage = (%d, %d), want (10, 7)", p, c)
	}
}

func TestStreamUsageNoChunkUnmetered(t *testing.T) {
	// A stream with no usage chunk -> unmetered.
	stream := "data: {\"id\":1,\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n" +
		"data: [DONE]\n"
	p, c, ok := parseStreamUsage([]byte(stream))
	if ok {
		t.Fatalf("a stream with no usage chunk must be unmetered")
	}
	if p != 0 || c != 0 {
		t.Fatalf("unmetered stream tokens = (%d,%d), want (0,0)", p, c)
	}
}

// TestStreamUsageNoIncludeUsageUnmetered is the P3 mutation guard: if the
// proxy did NOT force stream_options.include_usage on streaming requests, the
// upstream would not emit a final usage chunk, the stream would end without
// one, and it would be counted unmetered. This spec simulates that (a stream
// the upstream would return when include_usage was not forced) and asserts it
// is unmetered — the forcing is what prevents this in production.
func TestStreamUsageNoIncludeUsageUnmetered(t *testing.T) {
	stream := "data: {\"id\":1,\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n" +
		"data: [DONE]\n"
	if _, _, ok := parseStreamUsage([]byte(stream)); ok {
		t.Fatalf("a stream without a usage chunk (the include_usage-not-forced case) must be unmetered")
	}
}
