package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// newPauseCommand builds `cox pause <loop>`: set spec.suspend=true (A3).
func newPauseCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "pause <loop>",
		Short: "Pause a Loop (set spec.suspend=true)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return suspendLoop(cmd.Context(), currentDeps, namespaceOf(cmd), args[0], true, cmd.OutOrStdout())
		},
	}
}

// newResumeCommand builds `cox resume <loop>`: clear spec.suspend and, for a
// Stall/Budget pause, set the coxswain.io/resume annotation (A3 / P2f).
func newResumeCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "resume <loop>",
		Short: "Resume a Loop (clear spec.suspend; set coxswain.io/resume for a Stall/Budget pause)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return resumeLoop(cmd.Context(), currentDeps, namespaceOf(cmd), args[0], cmd.OutOrStdout())
		},
	}
}

// suspendLoop sets or clears spec.suspend (the pause command's core action).
func suspendLoop(ctx context.Context, dc Deps, ns, name string, suspend bool, out io.Writer) error {
	if err := setSuspend(ctx, dc, name, suspend); err != nil {
		return err
	}
	if suspend {
		fmt.Fprintf(out, "paused %s (spec.suspend=true)\n", name)
	} else {
		fmt.Fprintf(out, "resumed %s (spec.suspend=false)\n", name)
	}
	return nil
}

// setSuspend sets spec.suspend on the named Loop (a JSON merge patch on the
// main resource).
func setSuspend(ctx context.Context, dc Deps, name string, suspend bool) error {
	patch := map[string]any{"spec": map[string]any{"suspend": suspend}}
	return dc.Loops.Patch(ctx, name, "", mustJSON(patch))
}

// resumeLoop clears spec.suspend and, if the Loop is paused for a Stall/Budget
// reason (not a Suspend), sets the one-shot coxswain.io/resume annotation
// (P2f: spec.suspend=false resumes only a Suspend pause; a Stall/Budget pause
// needs the annotation).
func resumeLoop(ctx context.Context, dc Deps, ns, name string, out io.Writer) error {
	u, err := getLoop(ctx, dc, name)
	if err != nil {
		return err
	}
	pausedReason, _ := getNestedString(u, "status", "pausedReason")
	needsAnnotation := pausedReason != "" && pausedReason != "Suspend"

	if err := setSuspend(ctx, dc, name, false); err != nil {
		return err
	}
	if needsAnnotation {
		if err := setAnnotation(ctx, dc, name, resumeAnnotation, "true"); err != nil {
			return err
		}
		fmt.Fprintf(out, "resumed %s (spec.suspend=false + %s=true)\n", name, resumeAnnotation)
	} else {
		fmt.Fprintf(out, "resumed %s (spec.suspend=false)\n", name)
	}
	return nil
}

// resumeAnnotation is the one-shot resume annotation (P2f, matches the
// operator's const in internal/controller/loop_p2f_paused.go).
const resumeAnnotation = "coxswain.io/resume"

// setAnnotation sets (or clears, when value=="") an annotation on the Loop
// (a JSON merge patch on metadata.annotations).
func setAnnotation(ctx context.Context, dc Deps, name, key, value string) error {
	ann := map[string]any{}
	if value != "" {
		ann[key] = value
	}
	patch := map[string]any{"metadata": map[string]any{"annotations": ann}}
	return dc.Loops.Patch(ctx, name, "", mustJSON(patch))
}
