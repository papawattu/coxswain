// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use the file except in compliance with the License.
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

// The operator's manager scopes its Pod/Service cache to ProxyComponentSelector
// so it does not cache every Pod and Service in the cluster. If a component
// the operator Gets through that cache is not in the selector, the cached Get
// misses and a CreateOrUpdate falls through to Create (AlreadyExists) on every
// reconcile of that resource (I42b kind acceptance: the egress proxy was
// created outside the scoped cache and the operator could never see it).
// This test pins the selector against the two label sets the operator owns,
// so adding a new operator-owned component without widening the selector is a
// compile/test failure.

import (
	"testing"

	"k8s.io/apimachinery/pkg/labels"
)

// modelProxyLabels mirrors internal/controller.proxyLabels (the model proxy
// pod/Service label set, D33).
func modelProxyLabels(loopName string) map[string]string {
	return map[string]string{
		ComponentLabelKey:       ComponentProxyLabel,
		"coxswain.io/proxy-for": loopName,
	}
}

// egressProxyLabelsMirror mirrors internal/controller.egressProxyLabels (the
// egress proxy pod/Service label set, I42b). It is DISJOINT from the model
// proxy and the agent (no coxswain.io/loop) — see the I42b review P1.
func egressProxyLabelsMirror(loopName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "coxswain-egress-proxy",
		"app.kubernetes.io/instance":   loopName,
		ComponentLabelKey:              ComponentEgressProxyLabel,
		"app.kubernetes.io/part-of":    "coxswain",
		"coxswain.io/egress-proxy-for": loopName,
	}
}

// TestProxyComponentSelectorMatchesBothProxies asserts the selector matches
// both the model proxy and the egress proxy label sets.
func TestProxyComponentSelectorMatchesBothProxies(t *testing.T) {
	sel := ProxyComponentSelector()

	for name, lset := range map[string]map[string]string{
		"model-proxy":  modelProxyLabels("some-loop"),
		"egress-proxy": egressProxyLabelsMirror("some-loop"),
	} {
		if !sel.Matches(labelsSet(lset)) {
			t.Errorf("ProxyComponentSelector does not match %s label set %v", name, lset)
		}
	}
}

// TestProxyComponentSelectorExcludesOthers asserts the selector does NOT
// match the agent (or any unrelated component), so the cache stays scoped.
func TestProxyComponentSelectorExcludesOthers(t *testing.T) {
	sel := ProxyComponentSelector()

	for name, lset := range map[string]map[string]string{
		"agent": {ComponentLabelKey: "agent", "coxswain.io/loop": "some-loop"},
		"none":  {"other": "label"},
	} {
		if sel.Matches(labelsSet(lset)) {
			t.Errorf("ProxyComponentSelector must not match %s label set %v", name, lset)
		}
	}
}

// labelsSet adapts a map to the labels.Set type the selector's Matches
// method takes.
func labelsSet(m map[string]string) labels.Set { return m }
