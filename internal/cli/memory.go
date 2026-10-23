package cli

// P5a (Phase 5 shared memory, PLAN-PHASE5.md:34; D66 accepted): `cox memory
// <repo-url>` — inspect the per-(namespace, git URL) memory PVC (the lessons
// from prior Loops on this repo). A read-only peek pod against the memory PVC
// (the same pattern as `cox plan`/`cox diff` read the workspace PVC), so the
// user can see what the copy-in init container will hand the agent.
//
// The PVC name is the shared keying (memkey.PVCName — the operator and the
// CLI agree on the name; a mismatch would make the CLI read a PVC that is not
// the Loop's). The PVC is in the CLI's namespace (the -n flag), per-namespace
// (D66: cross-namespace sharing is out of scope).
//
// This is INSPECTION ONLY: it reads the memory PVC. Writing lessons is P5b
// (the writer Job); export/import is P5c (manual).

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/papawattu/coxswain/pkg/memkey"
)

// newMemoryCommand builds `cox memory <repo-url>`: read the memory PVC's
// contents (the shared lessons for this repo in this namespace) via a
// read-only peek pod.
func newMemoryCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "memory <repo-url>",
		Short: "Inspect the shared memory PVC (lessons from prior Loops) for a repo",
		Long: "cox memory <repo-url> reads the per-(namespace, repo) memory PVC — the\n" +
			"lessons from prior Loops on this repo (written by P5b's writer Job). The PVC\n" +
			"name is memory-<sha256-prefix-of-repo-url>; it is in the current namespace\n" +
			"(a -n flag selects it). A fresh repo (no lessons yet) prints an empty note.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return memoryRepo(cmd.Context(), currentDeps, namespaceOf(), args[0], cmd.OutOrStdout())
		},
	}
	c.AddCommand(newMemoryExportCommand(), newMemoryImportCommand())
	return c
}

// memoryRepo reads and prints the memory PVC's contents (the command's core
// action). The real boundary is MemoryReader (a read-only peek pod against
// the memory PVC); tests stub it. A repo with no memory PVC (a repo-less
// namespace, or a URL the operator has never run a Loop on) prints an explicit
// "no memory" note (not an error — a fresh repo is the common case).
func memoryRepo(ctx context.Context, dc Deps, ns, repoURL string, out io.Writer) error {
	if dc.Memory == nil {
		dc.Memory = &peekMemoryReader{ns: ns}
	}
	name := memkey.PVCName(repoURL)
	if name == "" {
		// An empty repo URL: no memory (the keying returns "" for it).
		_, werr := io.WriteString(out, "No memory: an empty repo URL has no memory PVC (a repo-less Loop has no shared memory).\n")
		return werr
	}
	content, found, err := dc.Memory.Read(ctx, name)
	if err != nil {
		// The PVC is absent (a fresh repo, never reconciled) is NOT an error:
		// print the note. A real read failure (the peek pod failed, the PVC is
		// not Bound) is an error.
		if isNoMemoryErr(err) {
			_, werr := io.WriteString(out, "No memory yet: the memory PVC "+name+" does not exist in namespace "+ns+" (a fresh repo; no prior Loop has written lessons).\n")
			return werr
		}
		return fmt.Errorf("read memory PVC %s in namespace %s: %w", name, ns, err)
	}
	if !found {
		_, werr := io.WriteString(out, "No memory yet: the memory PVC "+name+" is empty in namespace "+ns+".\n")
		return werr
	}
	_, werr := io.WriteString(out, content)
	return werr
}

// memoryNotFoundErr is the sentinel the real MemoryReader returns when the
// memory PVC does not exist (a fresh repo) — the CLI prints a note, not an
// error.
type memoryNotFoundErr struct{}

func (memoryNotFoundErr) Error() string { return "memory PVC not found" }

func isNoMemoryErr(err error) bool {
	_, ok := err.(memoryNotFoundErr)
	return ok
}
