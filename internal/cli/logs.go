package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// logsComponents are the component selectors `cox logs <loop> --component`
// accepts (A3: agent, proxy, verify, deliver).
var logsComponents = []string{"agent", "proxy", "verify", "deliver"}

// newLogsCommand builds `cox logs <loop> [--component agent|proxy|verify|deliver]
// [-f]`: stream a component's logs (A3).
func newLogsCommand(deps Deps) *cobra.Command {
	var component string
	var follow bool
	cmd := &cobra.Command{
		Use:     "logs <loop>",
		Short:   "Stream a Loop component's logs (agent, proxy, verify, deliver)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return logsLoop(cmd.Context(), currentDeps, namespaceOf(cmd), args[0], component, follow, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&component, "component", "agent", "which component to tail: "+joinComponents(logsComponents))
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow the log stream")
	return cmd
}

// logsLoop streams the selected component's logs (the logs command's core
// action). The real boundary is LogStreamer (shells out to kubectl logs); tests
// stub it.
func logsLoop(ctx context.Context, dc Deps, ns, name, component string, follow bool, out io.Writer) error {
	if component == "" {
		component = "agent"
	}
	if !contains(logsComponents, component) {
		return fmt.Errorf("unknown component %q (expected one of %s)", component, joinComponents(logsComponents))
	}
	if dc.Logs == nil {
		dc.Logs = &kubectlLogStreamer{ns: ns, kubeconfig: Kubeconfig, follow: follow}
	}
	if follow {
		fmt.Fprintln(out, "# following "+component+" logs for "+name)
	}
	return dc.Logs.Stream(ctx, name, component)
}

func joinComponents(cs []string) string {
	s := ""
	for i, c := range cs {
		if i > 0 {
			s += ", "
		}
		s += c
	}
	return s
}

func contains(cs []string, c string) bool {
	for _, x := range cs {
		if x == c {
			return true
		}
	}
	return false
}
