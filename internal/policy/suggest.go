// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package policy

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/papawattu/coxswain/internal/alerts"
)

// Suggestion is the candidate allows `cox policy suggest` derives from a Loop's
// recorded activity (P6b-2, ADR-0007 learn mode, D109): only what was observed,
// deduplicated and sorted. It never carries a deny.
type Suggestion struct {
	Exec    []string
	Files   []string
	Network []string
}

// SuggestFromRecords maps the operator's Q4 audit records for one Loop to
// candidate allows. KubeArmor records name their operation first in the detail
// ("operation=Process|File|Network", P6b-2 step 1): Process with an absolute
// target is an exec allow, File is a files allow, Network is skipped (the
// egress proxy's connect records carry host:port, which an AgentPolicy network
// allow needs). Egress-proxy "connect" records become network allows. Both
// allowed and blocked records count: in learn mode KubeArmor's Audit verdict
// reads as allowed, and a blocked connect is exactly what the policy is missing.
func SuggestFromRecords(records []alerts.Record, loop, namespace string) Suggestion {
	exec, files, network := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range records {
		if r.Loop != loop || r.Namespace != namespace || r.Target == "" {
			continue
		}
		switch r.Source {
		case alerts.SourceKubeArmor:
			switch {
			case strings.HasPrefix(r.Detail, "operation=Process"):
				if path.IsAbs(r.Target) {
					exec[r.Target] = true
				}
			case strings.HasPrefix(r.Detail, "operation=File"):
				files[r.Target] = true
			}
		case alerts.SourceEgress:
			if r.Action == "connect" {
				network[r.Target] = true
			}
		}
	}
	return Suggestion{Exec: sortedKeys(exec), Files: sortedKeys(files), Network: sortedKeys(network)}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// suggestHeader is the platform minimum the operator always adds itself (D109:
// listed, never duplicated as allows).
const suggestHeader = `# Candidate AgentPolicy from recorded activity (cox policy suggest, P6b-2).
# Review before applying: these are the allows the agent USED, not what it needs.
# Platform minimum, added by the operator and NOT listed below: the model-proxy
# and egress-proxy Service FQDNs, DNS (udp/tcp 53), and the agent entrypoint.
`

// RenderSuggestion renders s as an AgentPolicy YAML named name (no auditOnly:
// the candidate is meant to enforce).
func RenderSuggestion(s Suggestion, name, namespace string) string {
	var b strings.Builder
	b.WriteString(suggestHeader)
	fmt.Fprintf(&b, "apiVersion: coxswain.wattu.com/v1alpha1\nkind: AgentPolicy\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n", name, namespace)
	for _, sec := range []struct {
		key  string
		vals []string
	}{{"exec", s.Exec}, {"files", s.Files}, {"network", s.Network}} {
		if len(sec.vals) == 0 {
			continue
		}
		fmt.Fprintf(&b, "  %s:\n", sec.key)
		for _, v := range sec.vals {
			fmt.Fprintf(&b, "    - %q\n", v)
		}
	}
	return b.String()
}
