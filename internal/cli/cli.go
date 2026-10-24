// Package cli implements the `cox` / `kubectl cox` command tree (A3,
// PLAN-ALPHA.md A3; the draft docs/CLI-PLAN.md). It talks ONLY to the
// Kubernetes API (the Loop CRD + pod logs); there is no operator HTTP API.
//
// One shared cobra tree (NewRootCommand) serves both entry points: the
// standalone `cox` binary and the `kubectl-cox` plugin (kubectl's plugin
// lookup finds a binary named `kubectl-cox` on PATH). The two thin mains
// (cmd/cox, cmd/kubectl-cox) call Execute.
//
// TDD seam (CLI-PLAN.md "TDD seams"): the K8s API client is the system
// boundary, so it is injected behind the LoopClient interface. Unit tests
// inject a fake client; the real impl wraps the client-go dynamic client.
// The pod-log boundary (logs command) is behind LogStreamer, stubbed in
// tests. The exec/cp boundary (plan) is behind WorkspaceReader.
package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
)

// Namespace is the active namespace (from the -n flag or the kubeconfig
// context). The CLI resolves it once at command build time so every subcommand
// shares it.
var Namespace string

// Context is the kubeconfig context (from the --context flag; empty = the
// ambient context). Passed to the real client and the kubectl subcommands.
var Context string

// Kubeconfig is the kubeconfig path (or "" for the ambient context).
var Kubeconfig string

// LoopClient is the K8s API boundary for Loop CRD operations (the TDD seam).
// The real impl wraps the client-go dynamic client; tests inject a fake.
type LoopClient interface {
	// Get returns the named Loop (or a not-found error).
	Get(ctx context.Context, name string) (runtime.Object, error)
	// Create applies a NEW Loop object (the main resource; the `fork` command
	// uses it — a new Loop must not go through Update, which PUTs to the
	// status subresource and 404s for a Loop that does not exist yet).
	Create(ctx context.Context, obj runtime.Object) error
	// Patch applies a JSON merge patch (or a raw patch) to the named Loop.
	// The subresource is "" for the main resource, "status" for the status
	// subresource.
	Patch(ctx context.Context, name, subresource string, patch []byte) error
	// Update writes the whole object (used to set annotations / spec.suspend).
	Update(ctx context.Context, obj runtime.Object) error
}

// LogStreamer streams pod logs (the `logs` command boundary). Tests stub the
// command line + output; the real impl shells out to `kubectl logs`.
type LogStreamer interface {
	Stream(ctx context.Context, loop, component string) error
}

// WorkspaceReader reads a file out of the workspace PVC (the `plan` command
// boundary). Tests stub the file contents; the real impl runs a read-only
// `kubectl cp`-equivalent against a peek pod.
type WorkspaceReader interface {
	Read(ctx context.Context, loop, path string) (string, error)
}

// Deps wires the seams a command needs. Zero values select the real
// (client-go) implementations; tests set the fake fields.
type Deps struct {
	Loops     LoopClient
	Logs      LogStreamer
	Workspace WorkspaceReader
	// Memory reads the memory PVC (the `cox memory` command; P5a). nil = the
	// real peek pod (peekMemoryReader).
	Memory MemoryReader
	// MemoryStore is the export/import boundary (P5d); nil = real pods.
	MemoryStore MemoryStore
	// MemoryAdmin is the `memory delete` boundary (I101); nil = kubectl.
	MemoryAdmin MemoryAdmin
}

// currentDeps is the active seam set. It is set by NewRootCommand/Execute and
// read by each command's RunE. (A package var keeps the command constructors
// simple; a CLI is single-threaded per process.)
var currentDeps Deps

// NewRootCommand builds the `cox` command tree. deps selects the boundaries
// (nil = real client-go impl). It is built per-invocation so the -n/-o flags
// take effect.
func NewRootCommand(deps Deps) *cobra.Command {
	currentDeps = deps
	root := &cobra.Command{
		Use:           "cox",
		Short:         "Drive coxswain Loops (kubectl plugin: kubectl cox)",
		SilenceUsage:  true,
		SilenceErrors: false,
		// Build the real client-go deps after the persistent flags
		// (--namespace/--context/--kubeconfig) are parsed, unless the caller
		// already injected a deps set (tests inject a fake before Execute).
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			if currentDeps.Loops == nil {
				d, err := NewDeps()
				if err != nil {
					cmd.SilenceErrors = true
					if _, ferr := fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err); ferr != nil {
						return
					}
					return
				}
				currentDeps = d
			}
		},
	}
	root.PersistentFlags().StringVarP(&Namespace, "namespace", "n", "", "the namespace for the coxswain objects (from the kubeconfig context if unset)")
	root.PersistentFlags().StringVar(&Context, "context", "", "the kubeconfig context to use (the ambient context if unset)")
	root.PersistentFlags().StringVar(&Kubeconfig, "kubeconfig", "", "the kubeconfig file to use (the ambient context if unset)")

	root.AddCommand(
		newDescribeCommand(),
		newHistoryCommand(),
		newLogsCommand(),
		newPlanCommand(),
		newPauseCommand(),
		newResumeCommand(),
		newRedeliverCommand(),
		newForkCommand(),
		newApproveCommand(),
		newRejectCommand(),
		newDiffCommand(),
		newMemoryCommand(),
	)
	return root
}

// Execute runs the root command (os.Args[1:]). Both mains call this.
func Execute(deps Deps) error {
	return NewRootCommand(deps).ExecuteContext(context.Background())
}

// kubeFlagArgs returns the kubectl/client-go global flag args the CLI's
// subcommands need: --context, --namespace, --kubeconfig (each omitted when
// empty, so the ambient kubeconfig context is used).
func kubeFlagArgs(ns string) []string {
	var args []string
	if Context != "" {
		args = append(args, "--context", Context)
	}
	if ns != "" {
		args = append(args, "--namespace", ns)
	}
	if Kubeconfig != "" {
		args = append(args, "--kubeconfig", Kubeconfig)
	}
	return args
}
