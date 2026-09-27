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

package engine

import (
	"slices"
	"strings"

	"github.com/papawattu/coxswain/internal/policy"
)

// kaptAllowAction is the KubeArmor action for an allow rule.
const kaptAllowAction = "Allow"

// KubeArmorPolicy is a MINIMAL typed representation of the subset of the
// KubeArmorPolicy (security.kubearmor.com/v1) the Coxswain emitter needs. It is
// a plain struct (not the KubeArmor Go module's type) so the emitter is a pure,
// testable function with no dependency on the engine's Go module. It serializes
// to the real CRD's shape; the KubeArmor emitter implementation (in prod)
// converts this into a KubeArmorPolicy object and creates it.
type KubeArmorPolicy struct {
	// Name is the KubeArmorPolicy name (owned by the Loop).
	Name string
	// Namespace is the namespace to create it in.
	Namespace string
	// OwnerLoop is the Loop name (set as an owner ref by the emitter).
	OwnerLoop string
	// Selector is the pod label match (KubeArmor matches pods, not containers).
	Selector map[string]string
	// Action is the base action: "Allow" for the emitted allows. KubeArmor's
	// base action for the policy is the platform minimum; each rule adds an Allow.
	Action string
	// Syscalls carries the allowed exec paths/syscalls.
	Syscalls *KubeArmorSyscalls
	// Network carries the allowed egress.
	Network *KubeArmorNetwork
	// File carries the allowed file paths.
	File *KubeArmorFile
}

// KubeArmorSyscalls is the syscalls block of a KubeArmorPolicy.
type KubeArmorSyscalls struct {
	Action        string   `json:"action"`
	MatchPaths    []string `json:"matchPaths,omitempty"`
	MatchSyscalls []string `json:"matchSyscalls,omitempty"`
}

// KubeArmorNetwork is the network block of a KubeArmorPolicy. KubeArmor matches
// egress by DNS query name + protocol, not host:port.
type KubeArmorNetwork struct {
	Action          string   `json:"action"`
	MatchDNSQueries []string `json:"matchDNSQueries,omitempty"`
	MatchProtocols  []string `json:"matchProtocols,omitempty"`
}

// KubeArmorFile is the file block of a KubeArmorPolicy.
type KubeArmorFile struct {
	Action           string   `json:"action"`
	MatchPaths       []string `json:"matchPaths,omitempty"`
	MatchDirectories []string `json:"matchDirectories,omitempty"`
}

// EmitKubeArmorPolicy translates Coxswain's EnginePolicy (per-container, D29)
// into a single pod-level KubeArmorPolicy carrying the UNION of the agent and
// proxy allows (KubeArmor matches pods, not containers; the per-container egress
// split — agent=localhost, proxy=model-endpoint — is enforced by the
// NetworkPolicy, C3). The selector targets the sandbox pod by a stable label.
// This is a pure function (no cluster, no engine) and is tested directly.
func EmitKubeArmorPolicy(loopName, namespace string, ep policy.EnginePolicy) *KubeArmorPolicy {
	// KubeArmor matches by pod label; the sandbox pod carries
	// coxswain.io/loop=<name> (the operator sets this on the sandbox).
	selector := map[string]string{"coxswain.io/loop": loopName}

	kap := &KubeArmorPolicy{
		Name:      "coxswain-" + loopName,
		Namespace: namespace,
		OwnerLoop: loopName,
		Selector:  selector,
		Action:    kaptAllowAction,
	}

	// Union the agent and proxy allows (pod-level; the NetworkPolicy does the
	// per-container split). exec -> syscalls.matchPaths, network ->
	// network.matchDNSQueries, files -> file.matchPaths.
	var execPaths, dnsQueries, filePaths []string
	for _, cp := range ep.Containers {
		for _, c := range cp.Allows {
			switch c.Type {
			case policy.AllowExec:
				execPaths = append(execPaths, c.Match)
			case policy.AllowNetwork:
				dnsQueries = append(dnsQueries, hostFromEndpoint(c.Match))
			case policy.AllowFile:
				filePaths = append(filePaths, c.Match)
			}
		}
	}
	dedupeSorted(&execPaths)
	dedupeSorted(&dnsQueries)
	dedupeSorted(&filePaths)

	if len(execPaths) > 0 {
		kap.Syscalls = &KubeArmorSyscalls{Action: kaptAllowAction, MatchPaths: execPaths}
	}
	if len(dnsQueries) > 0 {
		kap.Network = &KubeArmorNetwork{Action: kaptAllowAction, MatchDNSQueries: dnsQueries}
	}
	if len(filePaths) > 0 {
		kap.File = &KubeArmorFile{Action: kaptAllowAction, MatchPaths: filePaths}
	}
	return kap
}

// hostFromEndpoint strips a ":port" suffix from a host:port endpoint so the
// KubeArmor network rule matches the DNS name (KubeArmor matches by DNS query,
// not port). "localhost" has no port and is returned as-is.
func hostFromEndpoint(endpoint string) string {
	if i := strings.LastIndexByte(endpoint, ':'); i >= 0 {
		// Only strip if the suffix is a numeric port (avoid IPv6 ambiguity for
		// the common case; the operator's allows are host:port or bare hosts).
		port := endpoint[i+1:]
		if isNumeric(port) {
			return endpoint[:i]
		}
	}
	return endpoint
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// dedupeSorted sorts and de-duplicates a string slice in place.
func dedupeSorted(s *[]string) {
	if *s == nil {
		return
	}
	slices.Sort(*s)
	seen := map[string]bool{}
	out := (*s)[:0]
	for _, v := range *s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	*s = out
}
