package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// logsComponents are the component selectors `cox logs <loop> --component`
// accepts (A3: agent, proxy, verify, deliver).
var logsComponents = []string{compAgent, "proxy", "verify", "deliver"}

// compAgent is the default component (the agent container in the sandbox pod).
const compAgent = "agent"

// newLogsCommand builds `cox logs <loop> [--component agent|proxy|verify|deliver]
// [-f]`: stream a component's logs (A3).
func newLogsCommand() *cobra.Command {
	var component string
	var follow bool
	var tail int
	cmd := &cobra.Command{
		Use:   "logs <loop>",
		Short: "Stream a Loop component's logs (agent, proxy, verify, deliver)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return logsLoop(cmd.Context(), currentDeps, namespaceOf(), args[0], component, follow, tail, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&component, "component", compAgent, "which component to tail: "+joinComponents(logsComponents))
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow the log stream")
	cmd.Flags().IntVar(&tail, "tail", 0, "number of lines to tail (0 = all)")
	return cmd
}

// logsLoop streams the selected component's logs (the logs command's core
// action). The real boundary is LogStreamer (shells out to kubectl logs); tests
// stub it.
func logsLoop(ctx context.Context, dc Deps, ns, name, component string, follow bool, tail int, out io.Writer) error {
	if component == "" {
		component = compAgent
	}
	if !contains(logsComponents, component) {
		return fmt.Errorf("unknown component %q (expected one of %s)", component, joinComponents(logsComponents))
	}
	if dc.Logs == nil {
		dc.Logs = &kubectlLogStreamer{ns: ns, follow: follow, tail: tail}
	}
	if follow {
		newWriter(out).printf("# following %s logs for %s", component, name)
	}
	return dc.Logs.Stream(ctx, name, component)
}

func joinComponents(cs []string) string {
	return strings.Join(cs, ", ")
}

func contains(cs []string, c string) bool {
	return slices.Contains(cs, c)
}
