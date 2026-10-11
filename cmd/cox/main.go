// Command cox is the standalone coxswain CLI (A3). It drives Loops against the
// Kubernetes API only — no operator HTTP API. It shares its command tree with
// the kubectl-cox plugin (internal/cli); this is the standalone entry point.
package main

import (
	"os"

	"github.com/papawattu/coxswain/internal/cli"
)

func main() {
	if err := cli.Execute(cli.Deps{}); err != nil {
		os.Exit(1)
	}
}
