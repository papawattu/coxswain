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

package controller

// The operator's manager scopes its Pod/Service cache to
// policy.ProxyComponentSelector so it does not cache every Pod and Service in
// the cluster. If a component the operator Gets through that cache is not in
// the selector, the cached Get misses and a CreateOrUpdate falls through to
// Create (AlreadyExists) on every reconcile of that resource (I42b kind
// acceptance: the egress proxy was created outside the scoped cache and the
// operator could never see it, so the sandbox stayed Suspended).
//
// This test pins the selector against the REAL label sets the operator owns
// (proxyLabels and egressProxyLabels, the actual functions in
// loop_controller.go — not mirrored copies) so that widening the cache scope
// when a new operator-owned proxy component is added is a compile/test
// failure. It complements the internal/policy test, which asserts the
// selector's shape; this one asserts the selector matches the labels the
// controller actually puts on its proxy pods.

import (
	"testing"

	"github.com/papawattu/coxswain/internal/policy"
	"k8s.io/apimachinery/pkg/labels"
)

const selectorTestLoopName = "selector-test-loop"

// labelSetOf adapts the controller's map[string]string label sets to the
// labels.Set the selector's Matches method takes (map[string]string IS a
// labels.Set — this is a no-op type assertion made explicit for readability).
func labelSetOf(m map[string]string) labels.Set { return m }

// TestProxyComponentSelectorMatchesRealProxyLabels asserts the cache selector
// matches the controller's real model-proxy label set (proxyLabels) and the
// real egress-proxy label set (egressProxyLabels). A cache miss here would
// turn into a spurious Create (AlreadyExists) on the first reconcile of that
// proxy and a stuck Suspended sandbox.
func TestProxyComponentSelectorMatchesRealProxyLabels(t *testing.T) {
	sel := policy.ProxyComponentSelector()

	if !sel.Matches(labelSetOf(proxyLabels(selectorTestLoopName))) {
		t.Errorf("ProxyComponentSelector does not match the real model-proxy label set: %v",
			proxyLabels(selectorTestLoopName))
	}
	if !sel.Matches(labelSetOf(egressProxyLabels(selectorTestLoopName))) {
		t.Errorf("ProxyComponentSelector does not match the real egress-proxy label set: %v",
			egressProxyLabels(selectorTestLoopName))
	}
}

// TestProxyComponentSelectorExcludesAgent asserts the selector does NOT match
// the agent's label set, so the cache stays scoped to the two proxy components
// (the agent carries coxswain.io/loop, not a proxy component label).
func TestProxyComponentSelectorExcludesAgent(t *testing.T) {
	sel := policy.ProxyComponentSelector()

	if sel.Matches(labelSetOf(agentPodLabels(selectorTestLoopName))) {
		t.Errorf("ProxyComponentSelector must not match the agent label set: %v",
			agentPodLabels(selectorTestLoopName))
	}
}
