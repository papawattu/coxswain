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
	"testing"

	"github.com/papawattu/coxswain/internal/policy"
)

func TestEmitKubeArmorPolicySelectorAndRules(t *testing.T) {
	ep := policy.Translate(policy.EffectivePolicy{
		Exec:    []string{"git", "go"},
		Network: []string{"proxy.golang.org:443"},
		Files:   []string{"/data"},
	})
	kap := EmitKubeArmorPolicy("loop-x", "ns-x", ep)

	// Selector targets the sandbox pod by the stable loop label.
	if got, ok := kap.Selector["coxswain.io/loop"]; !ok || got != "loop-x" {
		t.Fatalf("selector must target the sandbox pod by coxswain.io/loop=loop-x, got %v", kap.Selector)
	}
	if kap.Name != "coxswain-loop-x" || kap.Namespace != "ns-x" || kap.OwnerLoop != "loop-x" {
		t.Fatalf("identity fields wrong: %s/%s owner=%s", kap.Namespace, kap.Name, kap.OwnerLoop)
	}

	// exec -> syscalls.matchPaths (git, go).
	if kap.Syscalls == nil {
		t.Fatalf("exec allows must produce a syscalls block")
	}
	if !slices.Contains(kap.Syscalls.MatchPaths, "git") || !slices.Contains(kap.Syscalls.MatchPaths, "go") {
		t.Fatalf("syscalls.matchPaths must include git and go, got %v", kap.Syscalls.MatchPaths)
	}

	// network -> network.matchDNSQueries, with the :port stripped (D29 model:
	// KubeArmor matches by DNS name). proxy.golang.org:443 -> proxy.golang.org.
	if kap.Network == nil {
		t.Fatalf("network allows must produce a network block")
	}
	if !slices.Contains(kap.Network.MatchDNSQueries, "proxy.golang.org") {
		t.Fatalf("network.matchDNSQueries must be proxy.golang.org (port stripped), got %v", kap.Network.MatchDNSQueries)
	}
	if slices.Contains(kap.Network.MatchDNSQueries, "proxy.golang.org:443") {
		t.Fatalf("network.matchDNSQueries must not carry the :port, got %v", kap.Network.MatchDNSQueries)
	}

	// files -> file.matchPaths.
	if kap.File == nil {
		t.Fatalf("file allows must produce a file block")
	}
	if !slices.Contains(kap.File.MatchPaths, "/data") {
		t.Fatalf("file.matchPaths must include /data, got %v", kap.File.MatchPaths)
	}
}

func TestEmitKubeArmorPolicyUnionsContainers(t *testing.T) {
	// Translate already splits agent vs proxy; the emitter must union both
	// containers' allows into the pod-level policy (D29: the NetworkPolicy does
	// the per-container split, the KubeArmorPolicy is the pod-level fence).
	ep := policy.Translate(policy.EffectivePolicy{Exec: []string{"git"}, Network: []string{"api.example.com:443"}, Files: []string{"/data"}})
	// Translate puts Exec on the agent and Network+Files on the agent too (the
	// platform minimum localhost is on the agent, the model endpoint on the
	// proxy). The union must include both the user's exec and network.
	kap := EmitKubeArmorPolicy("l", "n", ep)
	if !slices.Contains(kap.Syscalls.MatchPaths, "git") {
		t.Fatalf("union must include the agent's exec allow git")
	}
	if !slices.Contains(kap.Network.MatchDNSQueries, "api.example.com") {
		t.Fatalf("union must include the network allow (DNS name), got %v", kap.Network.MatchDNSQueries)
	}
	// localhost is a platform minimum on the agent container -> DNS name localhost.
	if !slices.Contains(kap.Network.MatchDNSQueries, "localhost") {
		t.Fatalf("union must include the platform-minimum localhost on the agent, got %v", kap.Network.MatchDNSQueries)
	}
}

func TestEmitKubeArmorPolicyDefaultDeny(t *testing.T) {
	// No user allows -> the policy carries only the platform minimum (localhost
	// on the agent, model endpoint on the proxy); no user exec/file/network
	// blocks beyond the platform minimum.
	ep := policy.Translate(policy.EffectivePolicy{})
	kap := EmitKubeArmorPolicy("l", "n", ep)
	// exec: none (the platform minimum has no user exec allows).
	if kap.Syscalls != nil {
		t.Fatalf("default-deny must produce no sysallows block (no user exec), got %v", kap.Syscalls)
	}
	// network: only the platform minimum (localhost + the model endpoint), NOT a
	// user allow. The emitter emits a network block only if there are network
	// allows; the platform minimum localhost counts as one, so there IS a
	// network block with localhost.
	if kap.Network == nil {
		t.Fatalf("the platform minimum localhost must produce a network block")
	}
	if slices.Contains(kap.Network.MatchDNSQueries, "api.example.com") {
		t.Fatalf("default-deny must not carry a user network allow, got %v", kap.Network.MatchDNSQueries)
	}
}
