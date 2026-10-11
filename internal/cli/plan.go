package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// planPath is the workspace path of the plan the agent wrote (A3: `.coxswain/PLAN.md`).
const planPath = ".coxswain/PLAN.md"

// newPlanCommand builds `cox plan <loop>`: read the plan from the workspace
// PVC (A3).
func newPlanCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:     "plan <loop>",
		Short:   "Read the plan (.coxswain/PLAN.md) from the Loop's workspace",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return planLoop(cmd.Context(), currentDeps, namespaceOf(cmd), args[0], cmd.OutOrStdout())
		},
	}
}

// planLoop reads and prints the plan (the plan command's core action). The real
// boundary is WorkspaceReader (a read-only peek pod against the workspace
// PVC); tests stub it.
func planLoop(ctx context.Context, dc Deps, ns, name string, out io.Writer) error {
	if dc.Workspace == nil {
		dc.Workspace = &peekWorkspaceReader{ns: ns, kubeconfig: Kubeconfig}
	}
	content, err := dc.Workspace.Read(ctx, name, planPath)
	if err != nil {
		return fmt.Errorf("read %s from loop %q: %w", planPath, name, err)
	}
	_, werr := io.WriteString(out, content)
	return werr
}
