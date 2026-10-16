package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// planPath is the ABSOLUTE path of the plan in the peek pod (A3:
// `.coxswain/PLAN.md` in the workspace, mounted at /workspace in the peek pod).
// F7 (dogfood 2 finding): the path must be absolute. The peek pod's WORKDIR is
// image-dependent (golang:1.26 sets /workspace; busybox does not), so a relative
// path like `.coxswain/PLAN.md` resolves against the image's WORKDIR and can
// point to the wrong place (busybox starts in /, so `cat .coxswain/PLAN.md`
// fails with "No such file" even though /workspace/.coxswain/PLAN.md exists).
// The absolute path /workspace/.coxswain/PLAN.md is correct regardless of the
// image's WORKDIR.
const planPath = "/workspace/.coxswain/PLAN.md"

// newPlanCommand builds `cox plan <loop>`: read the plan from the workspace
// PVC (A3).
func newPlanCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "plan <loop>",
		Short: "Read the plan (.coxswain/PLAN.md) from the Loop's workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return planLoop(cmd.Context(), currentDeps, namespaceOf(), args[0], cmd.OutOrStdout())
		},
	}
}

// planLoop reads and prints the plan (the plan command's core action). The real
// boundary is WorkspaceReader (a read-only peek pod against the workspace
// PVC); tests stub it.
func planLoop(ctx context.Context, dc Deps, ns, name string, out io.Writer) error {
	if dc.Workspace == nil {
		dc.Workspace = &peekWorkspaceReader{ns: ns}
	}
	content, err := dc.Workspace.Read(ctx, name, planPath)
	if err != nil {
		// F7 (dogfood 2 finding): the plan read is now an ERROR whenever it fails
		// (a missing file makes the peek pod exit non-zero — it is never returned
		// as content, and never hashed). Show the missing-plan hint explicitly
		// for the common case; the underlying error message carries the peek
		// pod's details for anything else.
		return fmt.Errorf("read %s from loop %q: %w (if the Loop has not finished Planning, the plan is not written yet)", planPath, name, err)
	}
	if strings.TrimSpace(content) == "" {
		// An empty read (a blank file) is "no plan": say so, don't print nothing.
		_, werr := io.WriteString(out, "No plan: "+planPath+" is empty in the workspace of loop "+name+"\n")
		return werr
	}
	_, werr := io.WriteString(out, content)
	return werr
}
