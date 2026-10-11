// Command kubectl-cox is the kubectl plugin for coxswain Loops (A3). kubectl's
// plugin lookup finds a binary named `kubectl-cox` on PATH, so `kubectl cox
// <cmd>` resolves here. It is a thin shim over the same command tree as the
// standalone `cox` binary (internal/cli); the only difference is the binary
// name (kubectl prefixes subcommands with `cox `).
package main

import (
	"os"

	"github.com/papawattu/coxswain/internal/cli"
)

func main() {
	// kubectl passes the plugin name as argv[0] ("kubectl-cox") and the
	// subcommand as the rest. The shared tree's root Use is "cox", so kubectl
	// calls us as `kubectl-cox <cmd> ...` and our root command sees
	// os.Args[1:] = ["<cmd>", ...] — the same shape as `cox <cmd> ...`.
	if err := cli.Execute(cli.Deps{}); err != nil {
		os.Exit(1)
	}
}
