/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/papawattu/coxswain/internal/alerts"
	"github.com/papawattu/coxswain/internal/policy"
)

func newPolicyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Work with AgentPolicies (P6b learn mode)",
	}
	cmd.AddCommand(newPolicySuggestCommand())
	return cmd
}

func newPolicySuggestCommand() *cobra.Command {
	var file, name string
	cmd := &cobra.Command{
		Use: "suggest <loop>",
		// Offline with -f: the file flag reads only a local file, so the
		// kubeconfig must not be loaded and a bad one must not mask this
		// command's own error (R52). Without -f it reads the learn-summary
		// ConfigMap and needs the client-go deps (D119 = (b)).
		Annotations: map[string]string{annotationOffline: "true"},
		Short:       "Print a candidate AgentPolicy from a Loop's recorded activity (the learn summary, or the operator's audit lines)",
		Long: "Reads the Loop's learn summary (the operator's per-Loop ConfigMap <loop>-learn-summary, kept for a\n" +
			"learn-mode Loop) and prints a candidate AgentPolicy with the allows the Loop's agent used.\n" +
			"With -f, read the audit lines from an operator log instead (e.g. `kubectl logs -n coxswain-system\n" +
			"deploy/coxswain-controller-manager` or that log piped on stdin, -f -). Review the candidate before applying.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				name = args[0] + "-suggested"
			}
			if cmd.Flags().Changed("file") {
				in := cmd.InOrStdin()
				if file != "-" {
					f, err := os.Open(file)
					if err != nil {
						return err
					}
					defer func() { _ = f.Close() }()
					in = f
				}
				return suggestPolicy(in, cmd.OutOrStdout(), args[0], namespaceOf(), name)
			}
			// Default: read the learn-summary ConfigMap (D119 = (b)).
			if currentDeps.LearnSummary == nil {
				return fmt.Errorf("cox policy suggest: no learn summary reader is configured (the CLI could not build its client)")
			}
			records, dropped, err := currentDeps.LearnSummary.ReadLearnSummary(cmd.Context(), namespaceOf(), args[0])
			if err != nil {
				return err
			}
			if dropped > 0 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: the learn summary dropped %d record(s) at its bound; the candidate may be missing allows\n", dropped)
			}
			return suggestPolicy(strings.NewReader(records), cmd.OutOrStdout(), args[0], namespaceOf(), name)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "read the audit lines from this operator log instead of the learn summary (- for stdin)")
	cmd.Flags().StringVar(&name, "name", "", "the candidate AgentPolicy's name (default <loop>-suggested)")
	return cmd
}

// suggestPolicy reads JSON lines, keeps the ones that decode as audit records,
// and writes the rendered candidate (P6b-2, D109).
func suggestPolicy(in io.Reader, out io.Writer, loop, ns, name string) error {
	var recs []alerts.Record
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var r alerts.Record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		recs = append(recs, r)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read audit lines: %w", err)
	}
	s := policy.SuggestFromRecords(recs, loop, ns)
	if len(s.Exec)+len(s.Files)+len(s.Network)+len(s.ExecOverflow)+len(s.FilesOverflow)+len(s.NetworkOverflow)+len(s.ExecRejected) == 0 {
		return fmt.Errorf("no audit records for Loop %s/%s's agent in the input; if it came from kubectl logs, lines before the operator's last log rotation are not included", ns, loop)
	}
	_, err := io.WriteString(out, policy.RenderSuggestion(s, name, ns))
	return err
}
