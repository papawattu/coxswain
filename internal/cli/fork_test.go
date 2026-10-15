package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// B6 (the cox fork Create-vs-Update pin): the fork command applies the fork
// Loop as a NEW object via LoopClient.Create (the main-resource POST). The
// B6 run 12 failure: forkLoop applied the fork via LoopClient.Update, which
// PUTs to the STATUS subresource and 404s for a Loop that does not exist yet
// ("apply the fork Loop ... not found"). The fake client accepted the Update
// (a nil error) so the bug was invisible until the real apiserver; this test
// pins the real call.
//
// B6 (the cox fork spec deep-copy pin): the fork's spec is a DEEP COPY of the
// source's spec (the fork inherits the source's goal, agent, verify,
// delivery, loop and checkpoints — the fork is a copy, not a new objective).
// The B6 run 13 failure: buildForkLoop copied only spec.workspace and
// spec.model (a top-level spec.model that does not exist in LoopSpec — the
// model lives in spec.agent.model), so the fork had no goal (the CRD
// rejects 'spec.goal: Required value') and no agent/verify/delivery/loop/
// checkpoints. This test pins the full spec copy.
//
// Mutation 1 (the norm): switching fork.go's Create call back to Update
// makes this test FAIL (f.creates is empty; the fork Loop is recorded in
// f.updates instead) — recorded in the PR.
//
// Mutation 2 (the norm): reverting buildForkLoop to copy only workspace
// (the old behaviour) makes this test FAIL (the fork's spec is missing
// goal/agent/verify/delivery/loop/checkpoints — the source's spec is not
// inherited) — recorded in the PR.
func TestFork_CreatesNewLoop_NotUpdates(t *testing.T) {
	// The source Loop (the fork inherits its spec; it must exist for
	// forkLoop's Get to succeed). The source's spec carries goal, agent,
	// verify, delivery, loop, checkpoints, and restoreFrom (the fork must
	// DROP restoreFrom and any existing forkOf, and SET forkOf to the
	// source's loop + iteration).
	src := makeLoop("Succeeded", 2)
	// The source's spec (the fork deep-copies it). Use a JSON round-trip to
	// build the spec so the spec carries only JSON types (the runtime
	// helpers panic on raw Go ints — a JSON round-trip converts them to
	// float64, which the helpers can deep-copy).
	specJSON := `{"goal":"make the thing work","agent":{"image":"ghcr.io/papawattu/coxswain-agent:latest","model":{"endpoint":"http://model:8000"}},"verify":{"checks":["make -n docs"]},"delivery":{"kind":"PullRequest"},"loop":{"maxIterations":4},"checkpoints":["checkpoint-loop1-iter-2"],"restoreFrom":"checkpoint-loop1-iter-1"}`
	var specMap map[string]any
	if err := json.Unmarshal([]byte(specJSON), &specMap); err != nil {
		t.Fatalf("unmarshal the source's spec: %v", err)
	}
	if err := unstructured.SetNestedMap(src.Object, specMap, "spec"); err != nil {
		t.Fatalf("set the source's spec: %v", err)
	}
	fc := newFakeLoopClient()
	fc.setLoop(src)
	deps := Deps{Loops: fc}

	out := new(bytes.Buffer)
	err := forkLoop(context.Background(), deps, "samples", "b6-fork", "loop1", 2, out)
	if err != nil {
		t.Fatalf("forkLoop: %v (output: %s)", err, out.String())
	}
	if len(fc.creates) != 1 {
		t.Fatalf("the fork Loop was applied via Create %d time(s), want exactly 1 (a NEW object goes to the main resource, not the status subresource)", len(fc.creates))
	}
	if len(fc.updates) != 0 {
		t.Fatalf("the fork Loop was ALSO applied via Update %d time(s), want 0 (an Update PUTs to the status subresource and 404s for a Loop that does not exist yet — the B6 run 12 failure)", len(fc.updates))
	}
	created, ok := fc.creates[0].(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("Create received %T, want *unstructured.Unstructured", fc.creates[0])
	}
	if got := created.GetName(); got != "b6-fork" {
		t.Errorf("the created Loop's name is %q, want b6-fork", got)
	}
	if got := created.GetNamespace(); got != "samples" {
		t.Errorf("the created Loop's namespace is %q, want samples", got)
	}
	// The fork's spec must be the source's spec (deep-copied), with
	// restoreFrom and any existing forkOf DROPPED, and forkOf SET to the
	// source's loop + iteration.
	forkSpec, ok := created.Object["spec"].(map[string]any)
	if !ok {
		t.Fatalf("the created Loop has no spec")
	}
	// spec.forkOf must carry the source's loop + iteration (the fork's seed).
	foMap, _ := forkSpec["forkOf"].(map[string]any)
	if foMap == nil {
		t.Fatal("spec.forkOf is absent on the created Loop")
	}
	if loop, _ := foMap["loop"].(string); loop != "loop1" {
		t.Errorf("spec.forkOf.loop is %v, want loop1", foMap["loop"])
	}
	iter, ok := foMap["iteration"]
	if !ok {
		t.Fatal("spec.forkOf.iteration is absent on the created Loop")
	}
	if i, ok := iter.(int); !ok || i != 2 {
		t.Errorf("spec.forkOf.iteration is %v (%T), want 2", iter, iter)
	}
	// The fork inherits the source's goal (the CRD requires a non-empty
	// goal — the B6 run 13 failure: 'spec.goal: Required value').
	if g, ok := forkSpec["goal"].(string); !ok || g == "" {
		t.Errorf("the created Loop's spec.goal is %v (ok=%v), want the source's non-empty goal (the CRD requires it)", forkSpec["goal"], ok)
	} else if g != "make the thing work" {
		t.Errorf("the created Loop's spec.goal is %q, want the source's goal (make the thing work)", g)
	}
	// The fork inherits the source's agent (the fork is a copy — the source's
	// model is inherited, not re-authored). The B6 run 13 failure: buildForkLoop
	// copied only workspace + model (a top-level spec.model that does not
	// exist in LoopSpec — the model lives in spec.agent.model), so the fork
	// had no agent.
	agent, ok := forkSpec["agent"].(map[string]any)
	if !ok || agent == nil {
		t.Errorf("the created Loop's spec.agent is %v (ok=%v), want the source's agent (the fork inherits the source's model)", forkSpec["agent"], ok)
	}
	// The fork inherits the source's verify (the fork's acceptance checks are
	// the source's — the fork is a copy, not a new objective). EXCEPTION: the
	// fork DROPS the source's forced-fail verify (the B6 step 4's 'false'
	// check) and sets a fresh, passing check (the fork succeeds where the
	// source's forced check cannot — the B6 run 14 failure: the fork
	// inherited the source's 'false' verify check and hit
	// MaxIterationsExceeded). The fork's verify is 'make -n docs' (a check
	// that succeeds on any well-formed repo).
	verify, ok := forkSpec["verify"].(map[string]any)
	if !ok || verify == nil {
		t.Errorf("the created Loop's spec.verify is %v (ok=%v), want a non-nil verify (the fork sets a fresh, passing check)", forkSpec["verify"], ok)
	} else if checks, ok := verify["checks"].([]any); !ok || len(checks) != 1 || checks[0] != "make -n docs" {
		t.Errorf("the created Loop's spec.verify.checks is %v, want [make -n docs] (the fork's fresh, passing check — NOT the source's forced-fail 'false')", verify["checks"])
	}
	// The fork inherits the source's delivery (the fork's delivery config is
	// the source's — the fork delivers the same way).
	if _, ok := forkSpec["delivery"].(map[string]any); !ok {
		t.Errorf("the created Loop's spec.delivery is %v, want the source's delivery (the fork inherits the source's delivery config)", forkSpec["delivery"])
	}
	// The fork inherits the source's loop (the fork's loop config is the
	// source's — the fork runs the same way).
	if _, ok := forkSpec["loop"].(map[string]any); !ok {
		t.Errorf("the created Loop's spec.loop is %v, want the source's loop (the fork inherits the source's loop config)", forkSpec["loop"])
	}
	// The fork inherits the source's checkpoints (the fork's checkpoint list
	// is the source's — the fork is a copy, not a new objective).
	if _, ok := forkSpec["checkpoints"].([]any); !ok {
		t.Errorf("the created Loop's spec.checkpoints is %v, want the source's checkpoints (the fork inherits the source's checkpoint list)", forkSpec["checkpoints"])
	}
	// The fork DROPS restoreFrom (the fork's restore is one-shot at creation
	// from the source's checkpoint; a stale restoreFrom would re-arm the
	// fork's own restore path and break the fork's one-shot semantics).
	if _, ok := forkSpec["restoreFrom"]; ok {
		t.Errorf("the created Loop's spec.restoreFrom is %v, want ABSENT (the fork DROPS restoreFrom — the fork's restore is one-shot at creation from the source's checkpoint)", forkSpec["restoreFrom"])
	}
	// The CLI's output names the fork (the user-facing confirmation).
	if !strings.Contains(out.String(), "b6-fork") {
		t.Errorf("fork output does not name the fork Loop: %s", out.String())
	}
}
