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
	"cmp"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/papawattu/coxswain/internal/alerts"
)

// suggestLimit is the AgentPolicy's MaxItems for each allow list (D112: run 4's
// 6260 observed file paths overflowed the CRD, and the rendered candidate
// must be valid as-is).
const suggestLimit = 64

// suggestDirCollapse is the per-directory threshold for files: more distinct
// targets in one directory becomes one dir + "/" entry, so a busy tree
// (run 4: 6260 files under few roots) cannot swamp the list (D112).
const suggestDirCollapse = 5

// Suggestion is the candidate allows `cox policy suggest` derives from a Loop's
// recorded activity (P6b-2, ADR-0007 learn mode, D109): only what was observed,
// deduplicated and sorted. It never carries a deny. The Overflow fields hold
// the entries that exceeded the AgentPolicy's 64-entry MaxItems (D112): they
// are rendered as comments, never applied.
type Suggestion struct {
	Exec            []string
	Files           []string
	Network         []string
	ExecOverflow    []string
	FilesOverflow   []string
	NetworkOverflow []string
	// ExecRejected holds the observed exec paths the CRD's exec XValidation
	// would reject (under a writable mount, or not canonical — R15). They are
	// never listed as allows and never counted toward the cap; they are
	// rendered as a comment (D112, kind run 7: the candidate was rejected by
	// `kubectl apply --dry-run=server` because observed go-build paths sit
	// under /workspace).
	ExecRejected []string
}

// suggestPodItem reports whether r names this Loop's agent pod as a whole
// detail item (D112: the sandbox pod also runs verify and checkpoint probes,
// whose paths are not the agent's; the detail carries pod=<loop>-sandbox,
// joined with ", ").
func suggestPodItem(r alerts.Record, loop string) bool {
	want := "pod=" + loop + "-sandbox"
	return slices.Contains(strings.Split(r.Detail, ", "), want)
}

// SuggestFromRecords maps the operator's Q4 audit records for one Loop to
// candidate allows. Only the Loop's agent pod counts (pod=<loop>-sandbox in
// the detail, D112); KubeArmor records name their operation first in the
// detail ("operation=Process|File|Network", P6b-2 step 1): Process with an
// absolute target is an exec allow (the target is the full command line; only
// its first field, the binary path, is kept), File is a files allow (a busy
// directory collapses to dir + "/"), Network is skipped (the egress proxy's
// connect records carry host:port, which an AgentPolicy network allow needs).
// Egress-proxy "connect" records become network allows. Both allowed and
// blocked records count: in learn mode KubeArmor's Audit verdict reads as
// allowed, and a blocked connect is exactly what the policy is missing. Each
// list is capped at suggestLimit (the CRD's MaxItems): the most observed
// entries are kept, the rest go to the Suggestion's Overflow fields.
func SuggestFromRecords(records []alerts.Record, loop, namespace string) Suggestion {
	exec, files, network := map[string]int{}, map[string]int{}, map[string]int{}
	var execRejected map[string]bool
	for _, r := range records {
		if r.Loop != loop || r.Namespace != namespace || r.Target == "" {
			continue
		}
		switch r.Source {
		case alerts.SourceKubeArmor:
			if !suggestPodItem(r, loop) {
				continue
			}
			switch {
			case strings.HasPrefix(r.Detail, "operation=Process"):
				// The target is the full command line ("/bin/sh -c echo hi");
				// the binary path is its first field, and only absolute paths
				// are valid AgentPolicy exec entries.
				// A whitespace-only target has no fields; skip it instead of
				// indexing out of range.
				fields := strings.Fields(r.Target)
				if len(fields) == 0 {
					continue
				}
				bin := fields[0]
				if path.IsAbs(bin) && execAllowable(bin) {
					exec[bin]++
				} else if path.IsAbs(bin) {
					// R15: a binary the agent can write cannot be allowed by
					// path; paths the CRD's exec rule rejects go to
					// ExecRejected, not the exec cap (D112 kind run 7).
					if execRejected == nil {
						execRejected = map[string]bool{}
					}
					execRejected[bin] = true
				}
			case strings.HasPrefix(r.Detail, "operation=File"):
				files[r.Target]++
			}
		case alerts.SourceEgress:
			if r.Action == "connect" {
				network[r.Target]++
			}
		}
	}
	fileEntries := collapseFileDirs(files)
	execL, execO := capList(exec)
	filesL, filesO := capList(fileEntries)
	netL, netO := capList(network)
	rejected := make([]string, 0, len(execRejected))
	for p := range execRejected {
		rejected = append(rejected, p)
	}
	slices.Sort(rejected)
	return Suggestion{
		Exec:            execL,
		Files:           filesL,
		Network:         netL,
		ExecOverflow:    execO,
		FilesOverflow:   filesO,
		NetworkOverflow: netO,
		ExecRejected:    rejected,
	}
}

// execAllowable is a Go copy of the AgentPolicy exec XValidation rule in
// api/v1alpha1/agentpolicy_types.go:66. The two must change together: an
// entry the CRD rejects must not be suggested, or the candidate fails
// `kubectl apply --dry-run=server` (R15, D112 kind run 7).
var execPathRe = regexp.MustCompile(`^(/[A-Za-z0-9._+-]+)+$`)

func execAllowable(p string) bool {
	return execPathRe.MatchString(p) &&
		!strings.Contains(p, "/./") &&
		!strings.Contains(p, "/../") &&
		!strings.HasSuffix(p, "/.") &&
		!strings.HasSuffix(p, "/..") &&
		p != "/workspace" && !strings.HasPrefix(p, "/workspace/") &&
		p != "/scratch" && !strings.HasPrefix(p, "/scratch/") &&
		p != "/tmp" && !strings.HasPrefix(p, "/tmp/")
}

// collapseFileDirs turns busy directory trees into one "dir/" entry,
// recursively (R49 I228): a directory with more than suggestDirCollapse
// distinct targets is replaced by its dir + "/" (the sum of its targets'
// counts), then the collapsed tree is regrouped and re-collapsed until a
// full pass changes nothing — so a busy subtree stays collapsed to its
// deepest busy ancestor, not one level. The root "/" is never collapsed
// itself: a parent of "/" stays as distinct entries (D112).
func collapseFileDirs(files map[string]int) map[string]int {
	entries := map[string]int{}
	maps.Copy(entries, files)
	for {
		byDir := map[string]map[string]int{}
		for entry, count := range entries {
			dir := path.Dir(strings.TrimSuffix(entry, "/"))
			if byDir[dir] == nil {
				byDir[dir] = map[string]int{}
			}
			byDir[dir][entry] = count
		}
		out := map[string]int{}
		changed := false
		for dir, group := range byDir {
			if dir != "/" && len(group) > suggestDirCollapse {
				total := 0
				for _, count := range group {
					total += count
				}
				out[dir+"/"] = total
				changed = true
				continue
			}
			maps.Copy(out, group)
		}
		entries = out
		if !changed {
			break
		}
	}
	// A collapsed "dir/" entry is recursive (D113): every entry under it is
	// covered by it. Drop each entry in favor of its NEAREST strict ancestor
	// "dir/" entry, rolling the count into that ancestor (R49 I228, review on
	// #420: run 8 listed /scratch/.cache/go-build/ next to 1122 entries under
	// it, and /usr/local/go/src/ next to /usr/local/go/src/crypto/cipher/).
	for e, count := range entries {
		if strings.HasSuffix(e, "/") {
			continue
		}
		ancestor := ""
		for a := range entries {
			if !strings.HasSuffix(a, "/") || a == e || !strings.HasPrefix(e, a) {
				continue
			}
			if len(a) > len(ancestor) {
				ancestor = a
			}
		}
		if ancestor != "" {
			delete(entries, e)
			entries[ancestor] += count
		}
	}
	return entries
}

// capList keeps the suggestLimit entries with the highest counts (ties broken
// by name) and returns them sorted by name with the remainder (also sorted by
// name) as the overflow (D112).
func capList(counts map[string]int) (kept, overflow []string) {
	type entry struct {
		name  string
		count int
	}
	all := make([]entry, 0, len(counts))
	for name, count := range counts {
		all = append(all, entry{name, count})
	}
	slices.SortFunc(all, func(a, b entry) int {
		if c := cmp.Compare(b.count, a.count); c != 0 {
			return c
		}
		return cmp.Compare(a.name, b.name)
	})
	if len(all) > suggestLimit {
		all = all[:suggestLimit]
	}
	keptNames := map[string]bool{}
	kept = make([]string, 0, len(all))
	for _, e := range all {
		keptNames[e.name] = true
		kept = append(kept, e.name)
	}
	slices.Sort(kept)
	for name := range counts {
		if !keptNames[name] {
			overflow = append(overflow, name)
		}
	}
	slices.Sort(overflow)
	return kept, overflow
}

// suggestHeader is the platform minimum the operator always adds itself (D109:
// listed, never duplicated as allows).
const suggestHeader = `# Candidate AgentPolicy from recorded activity (cox policy suggest, P6b-2).
# Review before applying: these are the allows the agent USED, not what it needs.
# Platform minimum, added by the operator and NOT listed below: the model-proxy
# and egress-proxy Service FQDNs, DNS (udp/tcp 53), and the agent entrypoint.
`

// RenderSuggestion renders s as an AgentPolicy YAML named name (no auditOnly:
// the candidate is meant to enforce). Lists beyond the 64-entry cap are
// rendered as commented-out entries, so the candidate stays a valid
// AgentPolicy while still showing what was observed (D112).
func RenderSuggestion(s Suggestion, name, namespace string) string {
	var b strings.Builder
	b.WriteString(suggestHeader)
	fmt.Fprintf(&b, "apiVersion: coxswain.wattu.com/v1alpha1\nkind: AgentPolicy\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n", name, namespace)
	for _, sec := range []struct {
		key      string
		vals     []string
		overflow []string
	}{{AllowExec, s.Exec, s.ExecOverflow}, {"files", s.Files, s.FilesOverflow}, {"network", s.Network, s.NetworkOverflow}} {
		if len(sec.vals) == 0 && len(sec.overflow) == 0 && (sec.key != AllowExec || len(s.ExecRejected) == 0) {
			continue
		}
		if len(sec.vals) > 0 {
			fmt.Fprintf(&b, "  %s:\n", sec.key)
			for _, v := range sec.vals {
				fmt.Fprintf(&b, "    - %q\n", v)
			}
		}
		if len(sec.overflow) > 0 {
			fmt.Fprintf(&b, "  # %d more %s entries were observed but not listed (AgentPolicy allows at most 64):\n", len(sec.overflow), sec.key)
			for _, v := range sec.overflow {
				fmt.Fprintf(&b, "  #   %q\n", v)
			}
		}
		if sec.key == AllowExec && len(s.ExecRejected) > 0 {
			fmt.Fprintf(&b, "  # %d exec paths were observed but cannot be allowed (under a writable mount, or not canonical: R15):\n", len(s.ExecRejected))
			for _, v := range s.ExecRejected {
				fmt.Fprintf(&b, "  #   %q\n", v)
			}
		}
	}
	return b.String()
}
