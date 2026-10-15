package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// assertForkSpec asserts the fork's spec (deep-copied from the source, with
// restoreFrom DROPPED and forkOf SET). Used by TestFork_CreatesNewLoop
// _NotUpdates to keep the main test under gocyclo's complexity limit.
func assertForkSpec(t *testing.T, forkSpec map[string]any) {
	t.Helper()
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
		t.Errorf("spec.forkOf.iteration is %v (%T), want 2 (int — the CLI sets it directly, not via a JSON round-trip)", iter, iter)
	}
	if g, ok := forkSpec["goal"].(string); !ok || g == "" {
		t.Errorf("the created Loop's spec.goal is %v (ok=%v), want the source's non-empty goal (the CRD requires it)", forkSpec["goal"], ok)
	} else if g != "make the thing work" {
		t.Errorf("the created Loop's spec.goal is %q, want the source's goal (make the thing work)", g)
	}
	verify, ok := forkSpec["verify"].(map[string]any)
	if !ok || verify == nil {
		t.Fatalf("the created Loop's spec.verify is %v (ok=%v), want the source's verify (the fork inherits the source's acceptance checks)", forkSpec["verify"], ok)
	}
	if checks, ok := verify["acceptanceChecks"].([]any); !ok || len(checks) != 1 || checks[0] != "false" {
		t.Errorf("the created Loop's spec.verify.acceptanceChecks is %v, want [false] (the source's forced-fail check — the fork INHERITS it by default)", verify["acceptanceChecks"])
	}
	if p, _ := verify["preset"].(string); p != "go" {
		t.Errorf("the created Loop's spec.verify.preset is %v, want go (the source's preset — the fork inherits it)", verify["preset"])
	}
	if pp, ok := verify["protectedPaths"].([]any); !ok || len(pp) != 1 || pp[0] != "Makefile" {
		t.Errorf("the created Loop's spec.verify.protectedPaths is %v, want [Makefile] (the source's protectedPaths — the fork inherits them)", verify["protectedPaths"])
	}
	if _, ok := forkSpec["agent"].(map[string]any); !ok {
		t.Errorf("the created Loop's spec.agent is %v, want the source's agent (the fork inherits the source's model)", forkSpec["agent"])
	}
	if _, ok := forkSpec["delivery"].(map[string]any); !ok {
		t.Errorf("the created Loop's spec.delivery is %v, want the source's delivery (the fork inherits the source's delivery config)", forkSpec["delivery"])
	}
	if _, ok := forkSpec["loop"].(map[string]any); !ok {
		t.Errorf("the created Loop's spec.loop is %v, want the source's loop (the fork inherits the source's loop config)", forkSpec["loop"])
	}
	if _, ok := forkSpec["checkpoints"].([]any); !ok {
		t.Errorf("the created Loop's spec.checkpoints is %v, want the source's checkpoints (the fork inherits the source's checkpoint list)", forkSpec["checkpoints"])
	}
	if _, ok := forkSpec["restoreFrom"]; ok {
		t.Errorf("the created Loop's spec.restoreFrom is %v, want ABSENT (the fork DROPS restoreFrom — the fork's restore is one-shot at creation from the source's checkpoint)", forkSpec["restoreFrom"])
	}
}

// B6 (the cox fork Create-vs-Update pin + spec deep-copy pin + verify
// inheritance pin): the fork command applies the fork Loop as a NEW object
// via LoopClient.Create (the main-resource POST), deep-copies the source's
// spec (the fork inherits the source's goal, agent, verify, delivery, loop
// and checkpoints), DROPS restoreFrom and any existing forkOf, and SETS
// forkOf to the source's loop + iteration. The fork INHERITS the source's
// verify by default (a user forks to try another path toward the SAME
// acceptance); --check (repeatable) replaces spec.verify.acceptanceChecks
// while keeping the rest of verify intact.
//
// Mutation 1 (the norm): switching fork.go's Create call back to Update
// makes this test FAIL (f.creates is empty; the fork Loop is recorded in
// f.updates instead) — recorded in the PR.
//
// Mutation 2 (the norm): reverting buildForkLoop to copy only workspace
// (the old behaviour) makes this test FAIL (the fork's spec is missing
// goal/agent/verify/delivery/loop/checkpoints — the source's spec is not
// inherited) — recorded in the PR.
//
// Mutation 3 (the norm): ignoring the --check flag (the fork inherits the
// source's acceptance checks even when --check is given) makes the
// --check override assertion FAIL — recorded in the PR.
func TestFork_CreatesNewLoop_NotUpdates(t *testing.T) {
	// The source Loop (the fork inherits its spec; it must exist for
	// forkLoop's Get to succeed). The source's spec carries goal, agent,
	// verify, delivery, loop, checkpoints, and restoreFrom (the fork must
	// DROP restoreFrom and any existing forkOf, and SET forkOf to the
	// source's loop + iteration). The source's verify has a forced-fail
	// check (the B6 step 4's 'false' check) — the fork INHERITS it by
	// default (a user forks to try another path toward the SAME
	// acceptance).
	src := makeLoop("Succeeded", 2)
	// The source's spec (the fork deep-copies it). Use a JSON round-trip to
	// build the spec so the spec carries only JSON types (the runtime
	// helpers panic on raw Go ints — a JSON round-trip converts them to
	// float64, which the helpers can deep-copy).
	specJSON := `{"goal":"make the thing work","agent":{"image":"ghcr.io/papawattu/coxswain-agent:latest","model":{"endpoint":"http://model:8000"}},"verify":{"acceptanceChecks":["false"],"preset":"go","protectedPaths":["Makefile"]},"delivery":{"kind":"PullRequest"},"loop":{"maxIterations":4},"checkpoints":["checkpoint-loop1-iter-2"],"restoreFrom":"checkpoint-loop1-iter-1"}`
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

	// Test (a): with no --check or --goal flags, the fork inherits the
	// source's spec as-is (verify, goal, agent, delivery, loop,
	// checkpoints), with restoreFrom DROPPED and forkOf SET.
	out := new(bytes.Buffer)
	err := forkLoop(context.Background(), deps, "samples", "b6-fork", "loop1", 2, nil, "", out)
	if err != nil {
		t.Fatalf("forkLoop (no overrides): %v (output: %s)", err, out.String())
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
	assertForkSpec(t, forkSpec)
	// The CLI's output names the fork (the user-facing confirmation).
	if !strings.Contains(out.String(), "b6-fork") {
		t.Errorf("fork output does not name the fork Loop: %s", out.String())
	}

	// Test (b): --check x --check y gives acceptanceChecks [x, y] with the
	// rest of verify kept (preset, protectedPaths, etc. are inherited from
	// the source — only acceptanceChecks is replaced).
	fc2 := newFakeLoopClient()
	fc2.setLoop(src)
	deps2 := Deps{Loops: fc2}
	out2 := new(bytes.Buffer)
	err = forkLoop(context.Background(), deps2, "samples", "b6-fork2", "loop1", 2, []string{"make -n docs", "go test ./..."}, "", out2)
	if err != nil {
		t.Fatalf("forkLoop (--check x --check y): %v (output: %s)", err, out2.String())
	}
	if len(fc2.creates) != 1 {
		t.Fatalf("the fork Loop (--check) was applied via Create %d time(s), want exactly 1", len(fc2.creates))
	}
	created2, ok := fc2.creates[0].(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("Create (--check) received %T, want *unstructured.Unstructured", fc2.creates[0])
	}
	forkSpec2, ok := created2.Object["spec"].(map[string]any)
	if !ok {
		t.Fatalf("the created Loop (--check) has no spec")
	}
	// The fork's verify.acceptanceChecks is [x, y] (the --check values —
	// the source's ["false"] is REPLACED).
	verify2, ok := forkSpec2["verify"].(map[string]any)
	if !ok || verify2 == nil {
		t.Fatalf("the created Loop (--check)'s spec.verify is %v (ok=%v), want a non-nil verify", forkSpec2["verify"], ok)
	}
	if checks2, ok := verify2["acceptanceChecks"].([]any); !ok || len(checks2) != 2 || checks2[0] != "make -n docs" || checks2[1] != "go test ./..." {
		t.Errorf("the created Loop (--check)'s spec.verify.acceptanceChecks is %v, want [make -n docs, go test ./...] (the --check values — the source's [false] is REPLACED)", verify2["acceptanceChecks"])
	}
	// The rest of verify is KEPT (preset, protectedPaths — the fork
	// inherits them from the source; only acceptanceChecks is replaced).
	if p, _ := verify2["preset"].(string); p != "go" {
		t.Errorf("the created Loop (--check)'s spec.verify.preset is %v, want go (the source's preset — the rest of verify is KEPT)", verify2["preset"])
	}
	if pp, ok := verify2["protectedPaths"].([]any); !ok || len(pp) != 1 || pp[0] != "Makefile" {
		t.Errorf("the created Loop (--check)'s spec.verify.protectedPaths is %v, want [Makefile] (the source's protectedPaths — the rest of verify is KEPT)", verify2["protectedPaths"])
	}
	// The fork still inherits the source's goal (no --goal flag — the goal
	// is unchanged).
	if g, _ := forkSpec2["goal"].(string); g != "make the thing work" {
		t.Errorf("the created Loop (--check)'s spec.goal is %q, want the source's goal (make the thing work) — --check does NOT change the goal", g)
	}
	// The fork still drops restoreFrom.
	if _, ok := forkSpec2["restoreFrom"]; ok {
		t.Errorf("the created Loop (--check)'s spec.restoreFrom is %v, want ABSENT (the fork DROPS restoreFrom)", forkSpec2["restoreFrom"])
	}
	// The fork still sets forkOf.
	foMap2, _ := forkSpec2["forkOf"].(map[string]any)
	if foMap2 == nil {
		t.Fatalf("the created Loop (--check) has no spec.forkOf")
	}
	if loop2, _ := foMap2["loop"].(string); loop2 != "loop1" {
		t.Errorf("the created Loop (--check)'s spec.forkOf.loop is %v, want loop1", foMap2["loop"])
	}
}

// B6 (the cox fork --goal override pin): the --goal flag replaces spec.goal
// (the fork's objective). A fork WITHOUT --goal inherits the source's goal.
//
// Mutation (the norm): ignoring the --goal flag (the fork inherits the
// source's goal even when --goal is given) makes this test FAIL — recorded
// in the PR.
func TestFork_GoalOverride(t *testing.T) {
	src := makeLoop("Succeeded", 2)
	specJSON := `{"goal":"make the thing work","verify":{"acceptanceChecks":["false"]}}`
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

	// With --goal, the fork's goal is the override (not the source's goal).
	out := new(bytes.Buffer)
	err := forkLoop(context.Background(), deps, "samples", "b6-fork-goal", "loop1", 2, nil, "a new objective", out)
	if err != nil {
		t.Fatalf("forkLoop (--goal): %v (output: %s)", err, out.String())
	}
	if len(fc.creates) != 1 {
		t.Fatalf("the fork Loop (--goal) was applied via Create %d time(s), want exactly 1", len(fc.creates))
	}
	created, ok := fc.creates[0].(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("Create (--goal) received %T, want *unstructured.Unstructured", fc.creates[0])
	}
	forkSpec, ok := created.Object["spec"].(map[string]any)
	if !ok {
		t.Fatalf("the created Loop (--goal) has no spec")
	}
	if g, _ := forkSpec["goal"].(string); g != "a new objective" {
		t.Errorf("the created Loop (--goal)'s spec.goal is %q, want 'a new objective' (the --goal override — the source's goal is REPLACED)", g)
	}
	// The fork still inherits the source's verify (no --check flag — the
	// verify is unchanged).
	verify, ok := forkSpec["verify"].(map[string]any)
	if !ok || verify == nil {
		t.Fatalf("the created Loop (--goal)'s spec.verify is %v (ok=%v), want the source's verify (the fork inherits it — --goal does NOT change the verify)", forkSpec["verify"], ok)
	}
	if checks, ok := verify["acceptanceChecks"].([]any); !ok || len(checks) != 1 || checks[0] != "false" {
		t.Errorf("the created Loop (--goal)'s spec.verify.acceptanceChecks is %v, want [false] (the source's check — the fork INHERITS it; --goal does NOT change the verify)", verify["acceptanceChecks"])
	}
}

// B6 (the cox fork --check mutation pin): ignoring the --check flag (the
// fork inherits the source's acceptance checks even when --check is given)
// must make TestFork_CreatesNewLoop_NotUpdates FAIL (the assertion
// 'spec.verify.acceptanceChecks is [false], want [make -n docs, go
// test ./...]'). This test is the mutation: it calls forkLoop with --check
// and asserts the acceptanceChecks are the --check values (not the source's
// ["false"]).
//
// Mutation: remove the --check override in fork.go (the fork inherits the
// source's acceptance checks) and this test FAILS — recorded in the PR.
func TestFork_CheckOverride_Mutation(t *testing.T) {
	src := makeLoop("Succeeded", 2)
	specJSON := `{"goal":"make the thing work","verify":{"acceptanceChecks":["false"],"preset":"go"}}`
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
	err := forkLoop(context.Background(), deps, "samples", "b6-fork-mut", "loop1", 2, []string{"make -n docs"}, "", out)
	if err != nil {
		t.Fatalf("forkLoop (--check make -n docs): %v (output: %s)", err, out.String())
	}
	if len(fc.creates) != 1 {
		t.Fatalf("the fork Loop (--check) was applied via Create %d time(s), want exactly 1", len(fc.creates))
	}
	created, ok := fc.creates[0].(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("Create (--check) received %T, want *unstructured.Unstructured", fc.creates[0])
	}
	forkSpec, ok := created.Object["spec"].(map[string]any)
	if !ok {
		t.Fatalf("the created Loop (--check) has no spec")
	}
	verify, ok := forkSpec["verify"].(map[string]any)
	if !ok || verify == nil {
		t.Fatalf("the created Loop (--check)'s spec.verify is %v (ok=%v), want a non-nil verify", forkSpec["verify"], ok)
	}
	// The fork's acceptanceChecks is the --check value (NOT the source's
	// ["false"]). If the --check flag is ignored (the mutation), this
	// assertion FAILS.
	if checks, ok := verify["acceptanceChecks"].([]any); !ok || len(checks) != 1 || checks[0] != "make -n docs" {
		t.Errorf("the created Loop (--check)'s spec.verify.acceptanceChecks is %v, want [make -n docs] (the --check value — the source's [false] is REPLACED; if the --check flag is IGNORED, this FAILS)", verify["acceptanceChecks"])
	}
	// The rest of verify is KEPT (preset — the source's preset is inherited).
	if p, _ := verify["preset"].(string); p != "go" {
		t.Errorf("the created Loop (--check)'s spec.verify.preset is %v, want go (the source's preset — the rest of verify is KEPT)", verify["preset"])
	}
}
