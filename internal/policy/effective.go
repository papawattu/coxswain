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

// Package policy holds the pure, engine-agnostic translation from a Loop's
// effective AgentPolicy allows into the engine's policy spec (C6a, ADR-0007
// Q2/Q3/D29). The translation is a total function of its input — no I/O, no
// engine import, no time — so it can be swapped for another engine (C6b emits
// the resulting spec into KubeArmor behind an interface) without touching this
// logic.
package policy

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
)

// Allow-type constants. The engine (C6b) maps these onto its own rule types.
const (
	// AllowExec is an exec rule: a command the agent may run.
	AllowExec = "exec"
	// AllowNetwork is a network rule: a host:port the agent may reach.
	AllowNetwork = "network"
	// AllowFile is a file rule: a path the agent may access.
	AllowFile = "file"
)

// Container names in the sandbox pod (D29 scopes the policy per container).
const (
	// ContainerAgent is the agent container (the untrusted agent).
	ContainerAgent = "agent"
	// ContainerProxy is the model proxy sidecar (trusted; holds the model key).
	ContainerProxy = "proxy"
)

// Deny is the default posture for anything not in a container's Allows
// (ADR-0007 Q2: default-deny; rules are only ever allows).
const Deny = "deny"

// The platform-minimum endpoints. These are always present and are not part of
// the user's AgentPolicy (Q6: the model proxy's own egress is platform
// infrastructure, allowed by the operator).
const (
	// localhostEndpoint is the agent's only always-allowed network target: the
	// model proxy sidecar on loopback (D29: the agent reaches the model only
	// through the proxy, never the endpoint directly).
	localhostEndpoint = "localhost:8080"
	// modelEndpoint is the proxy's only always-allowed network target: the
	// model endpoint.
	modelEndpoint = "model-endpoint"
)

// Allow is a single allow rule in the engine policy.
type Allow struct {
	// Type is the rule type: AllowExec, AllowNetwork, or AllowFile.
	Type string `json:"type"`
	// Match is the command, host:port, or path the rule allows.
	Match string `json:"match"`
}

// ContainerPolicy is the engine policy for one container in the sandbox pod.
type ContainerPolicy struct {
	// Container is the container name (ContainerAgent or ContainerProxy).
	Container string `json:"container"`
	// Default is the posture for anything not in Allows. Always Deny.
	Default string `json:"default"`
	// Allows is the additive set of allows for this container.
	Allows []Allow `json:"allows"`
}

// EnginePolicy is the per-container, default-deny policy the operator sends to
// the engine (C6b renders it into a KubeArmorPolicy).
type EnginePolicy struct {
	// Containers is the per-container policy for the sandbox pod.
	Containers []ContainerPolicy `json:"containers"`
}

// EffectivePolicy is a Loop's effective allows: the union of the allows across
// every AgentPolicy the Loop references (spec.policyRefs[]). It is the input to
// Translate. An empty EffectivePolicy means the Loop references no policy — the
// default-deny platform minimum.
type EffectivePolicy struct {
	// Exec is the commands the agent may run (the union across policies).
	Exec []string
	// Network is the host:port endpoints the agent may reach (the union across
	// policies).
	Network []string
	// Files is the paths the agent may access (the union across policies).
	Files []string
}

// Translate turns a Loop's effective allows into the engine policy (C6a). It is
// pure and total: the agent container gets localhost (the proxy) plus the
// effective allows; the proxy container gets only the model endpoint; both
// default to deny. With no effective allows, the agent gets only localhost and
// the proxy only the model endpoint — the platform minimum (default-deny).
func Translate(p EffectivePolicy) EnginePolicy {
	agentAllows := make([]Allow, 0, 1+len(p.Exec)+len(p.Network)+len(p.Files))
	agentAllows = append(agentAllows, Allow{Type: AllowNetwork, Match: localhostEndpoint})
	for _, c := range p.Exec {
		agentAllows = append(agentAllows, Allow{Type: AllowExec, Match: c})
	}
	for _, h := range p.Network {
		agentAllows = append(agentAllows, Allow{Type: AllowNetwork, Match: h})
	}
	for _, f := range p.Files {
		agentAllows = append(agentAllows, Allow{Type: AllowFile, Match: f})
	}

	proxyAllows := []Allow{{Type: AllowNetwork, Match: modelEndpoint}}

	return EnginePolicy{
		Containers: []ContainerPolicy{
			{Container: ContainerAgent, Default: Deny, Allows: agentAllows},
			{Container: ContainerProxy, Default: Deny, Allows: proxyAllows},
		},
	}
}

// EffectiveHash returns the canonical SHA-256 hex of an EffectivePolicy: the
// union of its allows, sorted and serialized with a stable form. Two policies
// with the same effective allows (regardless of how they were unioned or in
// what order) hash identically, so the decision audit can record "what the
// agent was allowed to do" without storing the allows themselves (D32). The
// platform-minimum endpoints (localhost for the agent, the model endpoint for
// the proxy) are NOT part of the hash — they are always present and not the
// user's policy; the hash is over the user's effective allows only.
func EffectiveHash(p EffectivePolicy) string {
	exec := append([]string{}, p.Exec...)
	network := append([]string{}, p.Network...)
	files := append([]string{}, p.Files...)
	slices.Sort(exec)
	slices.Sort(network)
	slices.Sort(files)
	form := fmt.Sprintf("exec=%s|network=%s|files=%s", strings.Join(exec, ","), strings.Join(network, ","), strings.Join(files, ","))
	sum := sha256.Sum256([]byte(form))
	return fmt.Sprintf("%x", sum)
}
