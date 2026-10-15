package cli

import (
	"bytes"
	"context"
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
// Mutation (the norm): switching fork.go's Create call back to Update makes
// this test FAIL (f.creates is empty; the fork Loop is recorded in
// f.updates instead) — recorded in the PR.
func TestFork_CreatesNewLoop_NotUpdates(t *testing.T) {
	// The source Loop (the fork inherits its workspace; it must exist for
	// forkLoop's Get to succeed).
	src := makeLoop("Succeeded", 2)
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
	// spec.forkOf must carry the source's loop + iteration (the fork's seed).
	// The CLI builds spec.forkOf with an int; read it as a map (the
	// unstructured helpers deep-copy nested values, which panics on raw ints
	// — read the map instead).
	var foMap map[string]any
	if spec, ok := created.Object["spec"].(map[string]any); ok {
		foMap, _ = spec["forkOf"].(map[string]any)
	}
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
	// The CLI's output names the fork (the user-facing confirmation).
	if !strings.Contains(out.String(), "b6-fork") {
		t.Errorf("fork output does not name the fork Loop: %s", out.String())
	}
}
