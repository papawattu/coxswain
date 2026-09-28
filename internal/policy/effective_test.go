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
// Q2/Q3/D29).
package policy

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestTranslateDefaultDenyPlatformMinimum covers the no-policy case: the agent
// allows only localhost (the proxy) and the proxy only the model endpoint; both
// default to deny. This is the ADR-0007 Q2 guarantee — with no policy the agent
// can do only the platform minimum.
func TestTranslateDefaultDenyPlatformMinimum(t *testing.T) {
	got := Translate(EffectivePolicy{})
	want := EnginePolicy{
		Containers: []ContainerPolicy{
			{
				Container: ContainerAgent,
				Default:   Deny,
				Allows:    []Allow{{Type: AllowNetwork, Match: localhostEndpoint}},
			},
			{
				Container: ContainerProxy,
				Default:   Deny,
				Allows:    []Allow{{Type: AllowNetwork, Match: modelEndpoint}},
			},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Translate(empty) mismatch (-want +got):\n%s", diff)
	}
}

// TestTranslateAgentAllowsCoverThePolicyAsserts covers the case with exec,
// network, and file allows: the agent container carries all of them plus
// localhost, each exactly once. The proxy container carries only the model
// endpoint — it never gets the user's allows (D29).
func TestTranslateAgentAllowsCoverThePolicy(t *testing.T) {
	got := Translate(EffectivePolicy{
		Exec:    []string{"git", "go"},
		Network: []string{"proxy.golang.org:443", "sum.golang.org:443"},
		Files:   []string{"/workspace"},
	})

	var agent, proxy *ContainerPolicy
	for i := range got.Containers {
		switch got.Containers[i].Container {
		case ContainerAgent:
			agent = &got.Containers[i]
		case ContainerProxy:
			proxy = &got.Containers[i]
		}
	}
	if agent == nil || proxy == nil {
		t.Fatalf("the engine policy must have both the agent and proxy containers (got %+v)", got.Containers)
	}

	if agent.Default != Deny {
		t.Errorf("the agent container must default to deny (got %q)", agent.Default)
	}
	// Every user allow must be present exactly once.
	expectAgent := map[string]int{
		AllowNetwork + ":" + localhostEndpoint: 1,
		AllowNetwork + ":proxy.golang.org:443": 1,
		AllowNetwork + ":sum.golang.org:443":   1,
		AllowExec + ":git":                     1,
		AllowExec + ":go":                      1,
		AllowFile + ":/workspace":              1,
	}
	count := map[string]int{}
	for _, a := range agent.Allows {
		count[a.Type+":"+a.Match]++
	}
	for k, want := range expectAgent {
		if count[k] != want {
			t.Errorf("agent allows: %q got count %d, want %d (full: %+v)", k, count[k], want, agent.Allows)
		}
	}
	// No unexpected allows.
	if len(count) != len(expectAgent) {
		t.Errorf("agent has %d allows, want %d (got %+v)", len(count), len(expectAgent), agent.Allows)
	}

	// The proxy gets ONLY the model endpoint — never the user's allows (D29).
	if proxy.Default != Deny {
		t.Errorf("the proxy container must default to deny (got %q)", proxy.Default)
	}
	if len(proxy.Allows) != 1 || proxy.Allows[0].Type != AllowNetwork || proxy.Allows[0].Match != modelEndpoint {
		t.Errorf("the proxy must allow only the model endpoint (got %+v)", proxy.Allows)
	}
}

// TestEffectiveHashNoCommaCollision verifies that the hash is unambiguous for
// entries containing commas (R15 P3: ["a,b"] and ["a","b"] must NOT collide).
func TestEffectiveHashNoCommaCollision(t *testing.T) {
	p1 := EffectivePolicy{Exec: []string{"a,b"}}
	p2 := EffectivePolicy{Exec: []string{"a", "b"}}
	h1 := EffectiveHash(p1)
	h2 := EffectiveHash(p2)
	if h1 == h2 {
		t.Errorf("EffectiveHash collision: %q and %q both hash to %s",
			[]string{"a,b"}, []string{"a", "b"}, h1)
	}
	// Sanity: identical policies produce identical hashes.
	p3 := EffectivePolicy{Exec: []string{"a", "b"}}
	if EffectiveHash(p3) != h2 {
		t.Errorf("EffectiveHash not deterministic: %q and %q hash differently",
			[]string{"a", "b"}, []string{"b", "a"})
	}
}
