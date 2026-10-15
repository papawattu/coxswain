// B2 (PLAN-BETA, PLAN.md Phase 3 part 2): the `cox fork` command.
//
// The CLI fork command seeds a new one-shot Loop from an existing Loop's
// checkpoint (ADR-0003: forks, not re-tasks). The operator's fork path
// (the controller's spec.forkOf handling) restores the fork's OWN workspace
// PVC from the source's checkpoint snapshot at the fork's start; the fork
// then runs a normal one-shot Loop (own branch, own deliver, own PR).
//
// The command is thin: it reads the SOURCE Loop (to deep-copy its spec —
// the fork inherits the source's goal, agent, verify, delivery, loop and
// checkpoints), builds a Loop object with spec.forkOf set (loop +
// iteration), and applies it (via the LoopClient's Create, a new object).
// The operator does the rest (the fork's PVC restore, the branch, the
// deliver). The CLI does NOT create the snapshot or the PVC (the operator
// does that at the fork's start — the CLI's job is to express the fork's
// intent, not to perform it).
//
// The `--loop` flag names the SOURCE Loop (the Loop whose checkpoint the
// fork seeds from). The `--iteration` flag names the checkpoint's
// iteration (the source Loop's status.checkpoints entry; the CLI does not
// read the source Loop's status — the operator does, at the fork's start;
// the CLI's --iteration is the fork's intent, and the operator defers the
// fork's restore when the snapshot is not ReadyToUse, and rejects it when
// the snapshot is absent). The positional arg names the FORK Loop (the new
// Loop's name).
//
// Optional overrides: `--check <cmd>` (repeatable; when given, it replaces
// spec.verify.acceptanceChecks — the fork's acceptance checks — while keeping
// the rest of verify intact (preset, protectedPaths, onVerifyFail, etc.)).
// A fork WITHOUT --check inherits the source's acceptance checks (a user
// forks to try another path toward the SAME acceptance). `--goal <text>`
// (when given, it replaces spec.goal — the fork's objective). A fork
// WITHOUT --goal inherits the source's goal.
package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// newForkCommand builds `cox fork <fork-name> --loop <source> --iteration <n>`:
// seed a new one-shot Loop from an existing Loop's checkpoint (ADR-0003:
// forks, not re-tasks). The operator does the fork's PVC restore (the
// controller's spec.forkOf handling); the CLI expresses the fork's intent
// (a Loop object with spec.forkOf set).
func newForkCommand() *cobra.Command {
	var (
		sourceLoop string   // the SOURCE Loop (the Loop whose checkpoint the fork seeds from)
		iteration  int      // the checkpoint's iteration (the source Loop's status.checkpoints entry)
		checks     []string // optional: the fork's acceptance checks (when given, replaces spec.verify.acceptanceChecks)
		goal       string   // optional: the fork's goal (when given, replaces spec.goal)
	)
	cmd := &cobra.Command{
		Use:   "fork <fork-name>",
		Short: "Seed a new one-shot Loop from an existing Loop's checkpoint",
		Long: `Fork seeds a new one-shot Loop from an existing Loop's checkpoint.

The new Loop (the fork) inherits the source Loop's spec (goal, agent,
verify, delivery, loop, checkpoints) and is seeded from the source Loop's
checkpoint at the given iteration. The fork runs a normal one-shot Loop
(own branch, own deliver, own PR) — it does NOT re-task the source Loop
(ADR-0003: forks, not re-tasks).

The operator does the fork's PVC restore (the controller's spec.forkOf
handling restores the fork's OWN workspace PVC from the source's checkpoint
snapshot at the fork's start); the CLI expresses the fork's intent (a Loop
object with spec.forkOf set). The CLI does NOT create the snapshot or the
PVC (the operator does that at the fork's start).

The source Loop's checkpoint must exist (the source's
status.checkpoints must have an entry for the given iteration) and must be
ReadyToUse (the source's VolumeSnapshot must be ReadyToUse) — the operator
defers the fork's restore when the snapshot is not ReadyToUse (the
Restore condition is False, reason RestoreSourceNotReady, the requeue
retries) and rejects it when the snapshot is absent (the Restore condition
is False, reason RestoreSourceMissing, the fork is terminal).

Optional overrides:
  --check <cmd>   (repeatable; when given, replaces the fork's acceptance
                  checks — spec.verify.acceptanceChecks — while keeping the
                  rest of verify intact (preset, protectedPaths, etc.).
                  A fork WITHOUT --check inherits the source's acceptance
                  checks.)
  --goal <text>   (when given, replaces the fork's goal — spec.goal.
                  A fork WITHOUT --goal inherits the source's goal.)`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return forkLoop(cmd.Context(), currentDeps, namespaceOf(), args[0], sourceLoop, iteration, checks, goal, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&sourceLoop, "loop", "", "the SOURCE Loop's name (the Loop whose checkpoint the fork seeds from)")
	cmd.Flags().IntVar(&iteration, "iteration", 0, "the checkpoint's iteration (the source Loop's status.checkpoints entry; a positive integer)")
	cmd.Flags().StringArrayVar(&checks, "check", nil, "the fork's acceptance checks (repeatable; when given, replaces spec.verify.acceptanceChecks; a fork WITHOUT --check inherits the source's acceptance checks)")
	cmd.Flags().StringVar(&goal, "goal", "", "the fork's goal (when given, replaces spec.goal; a fork WITHOUT --goal inherits the source's goal)")
	return cmd
}

// forkLoop reads the SOURCE Loop (to deep-copy its spec), builds the FORK
// Loop object (a new Loop with spec.forkOf set, the source's spec otherwise
// intact — the fork inherits the source's goal, agent, verify, delivery,
// loop and checkpoints; restoreFrom and any existing forkOf are DROPPED, and
// forkOf is SET to the source's loop + iteration), and applies it (the
// operator does the rest). It does NOT create the snapshot or the PVC (the
// operator does that at the fork's start). It prints the fork's name and
// namespace on success.
//
// Optional overrides: checks (when non-empty, replaces spec.verify
// .acceptanceChecks — the fork's acceptance checks — while keeping the rest
// of verify intact); goal (when non-empty, replaces spec.goal — the fork's
// objective). A fork WITHOUT overrides inherits the source's spec as-is.
func forkLoop(ctx context.Context, dc Deps, ns, forkName, sourceLoop string, iteration int, checks []string, goal string, out io.Writer) error {
	if sourceLoop == "" {
		return fmt.Errorf("--loop is required (the SOURCE Loop's name)")
	}
	if iteration < 1 {
		return fmt.Errorf("--iteration must be a positive integer (the checkpoint's iteration)")
	}
	if dc.Loops == nil {
		return fmt.Errorf("cli: no Loop client (the real client-go impl is not wired)")
	}
	// The fork's spec is a DEEP COPY of the source's spec (the fork inherits
	// the source's goal, agent, verify, delivery, loop and checkpoints — the
	// fork is a copy, not a new objective). The CLI reads the source Loop to
	// get its spec (a read, not a write — the operator does the fork's PVC
	// restore; the CLI's read is to build the fork's Loop object correctly).
	src, err := dc.Loops.Get(ctx, sourceLoop)
	if err != nil {
		return fmt.Errorf("read the source Loop %s: %w (the source must exist)", sourceLoop, err)
	}
	srcU, ok := src.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("cli: expected the source Loop as *unstructured.Unstructured, got %T", src)
	}
	// Deep-copy the source's spec (the fork is a copy — the source's spec is
	// inherited, not re-authored). A nil spec (a source with no spec) is
	// treated as an empty spec (the fork's spec is just forkOf).
	spec, found, _ := unstructured.NestedMap(srcU.Object, "spec")
	if !found || spec == nil {
		spec = map[string]any{}
	}
	// Deep-copy the spec (a shallow copy would share nested maps with the
	// source's object; the fork's spec must be independent so the fork's
	// mutation does not leak into the source's object).
	specCopy := runtime.DeepCopyJSONValue(spec)
	forkSpec, ok := specCopy.(map[string]any)
	if !ok || forkSpec == nil {
		return fmt.Errorf("cli: the deep-copied spec is %T, want map[string]any", specCopy)
	}
	// DROP restoreFrom (the fork's restore is one-shot at creation from the
	// source's checkpoint; a stale restoreFrom would re-arm the fork's own
	// restore path and break the fork's one-shot semantics).
	delete(forkSpec, "restoreFrom")
	// DROP any existing forkOf (the source's forkOf — the source was itself
	// a fork — would be stale; the fork's forkOf is the source's loop +
	// iteration, set below).
	delete(forkSpec, "forkOf")
	// SET forkOf (the fork's seed: the source's loop + iteration).
	forkSpec["forkOf"] = map[string]any{
		"loop":      sourceLoop,
		"iteration": iteration,
	}
	// Apply the optional overrides: --check (replaces spec.verify
	// .acceptanceChecks — the fork's acceptance checks — while keeping the
	// rest of verify intact); --goal (replaces spec.goal — the fork's
	// objective). A fork WITHOUT overrides inherits the source's spec
	// as-is (a user forks to try another path toward the SAME acceptance).
	if len(checks) > 0 {
		// Set the fork's acceptance checks (the rest of verify is kept —
		// preset, protectedPaths, onVerifyFail, etc. are inherited from the
		// source). If the source had no verify (a verify-less source), create
		// a minimal verify with just the checks.
		verify, ok := forkSpec["verify"].(map[string]any)
		if !ok || verify == nil {
			verify = map[string]any{}
		}
		// Convert the []string checks to a []any (the unstructured type).
		checksAny := make([]any, len(checks))
		for i, c := range checks {
			checksAny[i] = c
		}
		verify["acceptanceChecks"] = checksAny
		forkSpec["verify"] = verify
	}
	if goal != "" {
		// Set the fork's goal (the source's goal is replaced — the fork has
		// a different objective).
		forkSpec["goal"] = goal
	}
	// Build the fork's Loop object (a new Loop with the fork's spec).
	fork := buildForkLoop(forkName, ns, forkSpec)
	// Apply the fork as a NEW object (Create, the main resource) — not Update,
	// which PUTs to the status subresource and 404s for a Loop that does not
	// exist yet (the B6 run 12's `cox fork` failure).
	if err := dc.Loops.Create(ctx, fork); err != nil {
		return fmt.Errorf("apply the fork Loop %s: %w", forkName, err)
	}
	w := newWriter(out)
	w.printf("forked Loop %s from %s (iter-%d)", forkName, sourceLoop, iteration)
	w.printf("the operator will restore the fork's workspace PVC from the source's checkpoint at the fork's start")
	return nil
}

// buildForkLoop builds the fork's Loop object (a new Loop with the fork's
// spec). The fork's name is the fork name; the fork's namespace is the
// source's namespace (the fork is in the source's namespace); the fork's
// spec is the source's spec (deep-copied, with restoreFrom and any existing
// forkOf DROPPED, and forkOf SET to the source's loop + iteration — the
// fork's seed).
func buildForkLoop(forkName, ns string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "coxswain.wattu.com/v1alpha1",
		"kind":       "Loop",
		"metadata":   map[string]any{"name": forkName, "namespace": ns},
		"spec":       spec,
	}}
}
