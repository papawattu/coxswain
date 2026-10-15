// B2 (PLAN-BETA, PLAN.md Phase 3 part 2): the `cox fork` command.
//
// The CLI fork command seeds a new one-shot Loop from an existing Loop's
// checkpoint (ADR-0003: forks, not re-tasks). The operator's fork path
// (the controller's spec.forkOf handling) restores the fork's OWN workspace
// PVC from the source's checkpoint snapshot at the fork's start; the fork
// then runs a normal one-shot Loop (own branch, own deliver, own PR).
//
// The command is thin: it reads the SOURCE Loop (to inherit the workspace
// and model — the fork is a copy, not a new repo or a new model
// experiment), builds a Loop object with spec.forkOf set (loop +
// iteration), and applies it (via the LoopClient's Update, the same path
// as the other CLI commands). The operator does the rest (the fork's PVC
// restore, the branch, the deliver). The CLI does NOT create the snapshot
// or the PVC (the operator does that at the fork's start — the CLI's job
// is to express the fork's intent, not to perform it).
//
// The `--loop` flag names the SOURCE Loop (the Loop whose checkpoint the
// fork seeds from). The `--iteration` flag names the checkpoint's
// iteration (the source Loop's status.checkpoints entry; the CLI does not
// read the source Loop's status — the operator does, at the fork's start;
// the CLI's --iteration is the fork's intent, and the operator defers the
// fork's restore when the snapshot is not ReadyToUse, and rejects it when
// the snapshot is absent). The positional arg names the FORK Loop (the new
// Loop's name).
package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// newForkCommand builds `cox fork <fork-name> --loop <source> --iteration <n>`:
// seed a new one-shot Loop from an existing Loop's checkpoint (ADR-0003:
// forks, not re-tasks). The operator does the fork's PVC restore (the
// controller's spec.forkOf handling); the CLI expresses the fork's intent
// (a Loop object with spec.forkOf set).
func newForkCommand() *cobra.Command {
	var (
		sourceLoop string // the SOURCE Loop (the Loop whose checkpoint the fork seeds from)
		iteration  int    // the checkpoint's iteration (the source Loop's status.checkpoints entry)
	)
	cmd := &cobra.Command{
		Use:   "fork <fork-name>",
		Short: "Seed a new one-shot Loop from an existing Loop's checkpoint",
		Long: `Fork seeds a new one-shot Loop from an existing Loop's checkpoint.

The new Loop (the fork) inherits the source Loop's workspace (the repo)
and model, and is seeded from the source Loop's checkpoint at the given
iteration. The fork runs a normal one-shot Loop (own branch, own deliver,
own PR) — it does NOT re-task the source Loop (ADR-0003: forks, not
re-tasks).

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
is False, reason RestoreSourceMissing, the fork is terminal).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return forkLoop(cmd.Context(), currentDeps, namespaceOf(), args[0], sourceLoop, iteration, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&sourceLoop, "loop", "", "the SOURCE Loop's name (the Loop whose checkpoint the fork seeds from)")
	cmd.Flags().IntVar(&iteration, "iteration", 0, "the checkpoint's iteration (the source Loop's status.checkpoints entry; a positive integer)")
	return cmd
}

// forkLoop reads the SOURCE Loop (to inherit the workspace and model),
// builds the FORK Loop object (a new Loop with spec.forkOf set), and
// applies it (the operator does the rest). It does NOT create the snapshot
// or the PVC (the operator does that at the fork's start). It prints the
// fork's name and namespace on success.
func forkLoop(ctx context.Context, dc Deps, ns, forkName, sourceLoop string, iteration int, out io.Writer) error {
	if sourceLoop == "" {
		return fmt.Errorf("--loop is required (the SOURCE Loop's name)")
	}
	if iteration < 1 {
		return fmt.Errorf("--iteration must be a positive integer (the checkpoint's iteration)")
	}
	if dc.Loops == nil {
		return fmt.Errorf("cli: no Loop client (the real client-go impl is not wired)")
	}
	// The fork's workspace: the source Loop's workspace (the fork inherits
	// the source's repo). The CLI reads the source Loop to get its workspace
	// (a read, not a write — the operator does the fork's PVC restore; the
	// CLI's read is to build the fork's Loop object correctly).
	src, err := dc.Loops.Get(ctx, sourceLoop)
	if err != nil {
		return fmt.Errorf("read the source Loop %s: %w (the source must exist)", sourceLoop, err)
	}
	srcU, ok := src.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("cli: expected the source Loop as *unstructured.Unstructured, got %T", src)
	}
	// The fork's workspace is the source's workspace (the fork inherits the
	// source's repo — the fork is a copy, not a new repo).
	workspace, found, _ := unstructured.NestedMap(srcU.Object, "spec", "workspace")
	if !found || workspace == nil {
		// A source Loop with no workspace (a workspace-less Loop) is forked
		// with no workspace (the fork is a copy — the source has no repo, so
		// the fork has no repo). This is a no-op (the fork's PVC is an empty
		// scratch workspace; the fork's one-shot goal is a fresh start).
		workspace = nil
	}
	// The fork's model is the source's model (the fork inherits the source's
	// model; the fork is a one-shot continuation of the source's work, not a
	// new model experiment).
	model, _, _ := unstructured.NestedMap(srcU.Object, "spec", "model")

	// Build the fork's Loop object (a new Loop with spec.forkOf set).
	fork := buildForkLoop(forkName, ns, sourceLoop, iteration, workspace, model)
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

// buildForkLoop builds the fork's Loop object (a new Loop with spec.forkOf
// set). The fork's name is the fork name; the fork's namespace is the
// source's namespace (the fork is in the source's namespace); the fork's
// workspace is the source's workspace (the fork inherits the source's
// repo); the fork's model is the source's model (the fork inherits the
// source's model); the fork's forkOf is {loop: sourceLoop, iteration:
// iteration} (the fork's seed).
func buildForkLoop(forkName, ns, sourceLoop string, iteration int, workspace, model map[string]any) *unstructured.Unstructured {
	spec := map[string]any{
		"forkOf": map[string]any{
			"loop":      sourceLoop,
			"iteration": iteration,
		},
	}
	// The fork's workspace: the source's workspace (a map; the fork inherits
	// the source's repo). A nil workspace (a workspace-less source) is
	// omitted (the fork has no workspace — the fork's PVC is an empty
	// scratch workspace).
	if workspace != nil {
		spec["workspace"] = workspace
	}
	// The fork's model: the source's model (a map; the fork inherits the
	// source's model). A nil model is omitted (the fork uses the operator's
	// default model).
	if model != nil {
		spec["model"] = model
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "coxswain.wattu.com/v1alpha1",
		"kind":       "Loop",
		"metadata":   map[string]any{"name": forkName, "namespace": ns},
		"spec":       spec,
	}}
}
