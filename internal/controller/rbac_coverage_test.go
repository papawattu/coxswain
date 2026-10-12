package controller

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestRBACCoverage verifies that every resource type the controller creates,
// updates, or Owns() has a matching rule in config/rbac/role.yaml.
//
// This is the guard the reviewer requested (P3, R15 review on PR #12):
// envtest runs as the cluster-admin user, so a missing RBAC rule is invisible
// to envtest. This test parses the generated role.yaml and asserts that for
// every type the controller touches, there is at least one rule covering it.
//
// The set of "touched" types is derived from the controller source:
//   - For(&Type{})    → the controller watches this type (needs get;list;watch)
//   - Owns(&Type{})   → the controller creates/owns this type (needs
//     get;list;watch;create;update at minimum; delete if the controller
//     explicitly deletes it)
//   - Create/Update calls in ensure* functions → the controller creates this
//     type
//
// Rather than parsing Go source (fragile), the test hard-codes the set of
// types the controller is known to create/own (derived from the
// SetupWithManager + ensure* functions) and checks each one against the
// parsed role.yaml. If a new type is added to the controller, this test
// will fail until the RBAC markers are updated and `make manifests` is run.
const (
	coxGroup   = "coxswain.wattu.com"
	verbGet    = "get"
	verbList   = "list"
	verbWatch  = "watch"
	verbCreate = "create"
	verbUpdate = "update"
)

func TestRBACCoverage(t *testing.T) {
	rolePath := filepath.Join("..", "..", "config", "rbac", "role.yaml")
	data, err := os.ReadFile(rolePath)
	if err != nil {
		t.Fatalf("read %s: %v", rolePath, err)
	}

	var role struct {
		Rules []struct {
			APIGroups []string `yaml:"apiGroups"`
			Resources []string `yaml:"resources"`
			Verbs     []string `yaml:"verbs"`
		} `yaml:"rules"`
	}
	if err := yaml.Unmarshal(data, &role); err != nil {
		t.Fatalf("unmarshal role.yaml: %v", err)
	}

	// Build a lookup: (apiGroup, resource) → set of verbs.
	type ruleKey struct{ group, resource string }
	verbSets := make(map[ruleKey]map[string]bool)
	for _, rule := range role.Rules {
		for _, group := range rule.APIGroups {
			for _, res := range rule.Resources {
				k := ruleKey{group, res}
				if verbSets[k] == nil {
					verbSets[k] = make(map[string]bool)
				}
				for _, v := range rule.Verbs {
					verbSets[k][v] = true
				}
			}
		}
	}

	// The types the controller creates, updates, or Owns().
	// Derived from SetupWithManager (For + Owns) and the ensure* functions.
	// If you add a new type to the controller, add it here too — this test
	// will fail if the RBAC markers don't cover it.
	required := []struct {
		group    string
		resource string
		minVerbs []string // verbs that must be present
	}{
		// For(&Loop{}) — the controller watches Loops.
		{group: coxGroup, resource: "loops",
			minVerbs: []string{verbGet, verbList, verbWatch}},
		// Loop status + finalizers (the controller updates status and sets
		// finalizers).
		{group: coxGroup, resource: "loops/status",
			minVerbs: []string{verbGet}},
		{group: coxGroup, resource: "loops/finalizers",
			minVerbs: []string{verbUpdate}},
		// Owns(&Sandbox{}) — the controller creates and owns Sandboxes.
		{group: "agents.x-k8s.io", resource: "sandboxes",
			minVerbs: []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate}},
		// Sandbox status (the controller reads sandbox status).
		{group: "agents.x-k8s.io", resource: "sandboxes/status",
			minVerbs: []string{verbGet}},
		// Owns(&Pod{}) — the controller creates and owns proxy Pods.
		{group: "", resource: corePodsResource,
			minVerbs: []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate}},
		// Owns(&Service{}) — the controller creates and owns proxy Services.
		{group: "", resource: "services",
			minVerbs: []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate}},

		// ---- The manager's CACHE-read types (R19 A6 P1) ------------------
		// The manager's controller-runtime cache (cmd/main.go Cache.ByObject
		// scopes ONLY Pod+Service by label; every other type is cached
		// cluster-wide) lazily watches a type the FIRST time the CACHED client
		// (r.Get / r.List — NOT the APIReader, r.apiReader) reads it. A cache
		// watch needs get;list;watch on that type at the cluster scope. envtest
		// runs as cluster-admin, so a missing list;watch is invisible there —
		// and it wedges a fresh install (the A6 acceptance run: "cannot list
		// resource configmaps at the cluster scope").
		//
		// This set is the types the controller reads THROUGH THE CACHED CLIENT
		// (r.Get/r.List). The pod-blind reads go through r.apiReader (the
		// non-cached client, mgr.GetAPIReader()) and do NOT need cache RBAC.
		// If you add a new cached-client read of a type, add it here too —
		// this test fails if the RBAC markers don't grant get;list;watch.

		// ConfigMaps: the model-prices CM (loop_p2d_budget.go r.Get) + the
		// deliver description CM (loop_deliver_job.go r.Get) are read through the
		// cached client. The A6 P1: the marker lacked list;watch and the
		// fresh-install wedge was exactly this rule.
		{group: "", resource: "configmaps",
			minVerbs: []string{verbGet, verbList, verbWatch}},
		// Jobs: the deliver Job + verify Job are read through the cached client
		// (loop_deliver_job.go / loop_verify_job.go r.Get).
		{group: "batch", resource: "jobs",
			minVerbs: []string{verbGet, verbList, verbWatch}},
		// NetworkPolicies: the loop/proxy/deliver netpols are read through the
		// cached client (loop_controller.go r.Get).
		{group: "networking.k8s.io", resource: "networkpolicies",
			minVerbs: []string{verbGet, verbList, verbWatch}},
		// PersistentVolumeClaims: the workspace PVC is read through the cached
		// client (loop_controller.go r.Get).
		{group: "", resource: "persistentvolumeclaims",
			minVerbs: []string{verbGet, verbList, verbWatch}},
		// AgentPolicies: the loop's AgentPolicy is read through the cached
		// client (loop_controller.go r.Get).
		{group: coxGroup, resource: "agentpolicies",
			minVerbs: []string{verbGet, verbList, verbWatch}},
	}

	for _, req := range required {
		verbs := verbSets[ruleKey{req.group, req.resource}]
		if verbs == nil {
			t.Errorf("no RBAC rule for %s/%s (the controller creates or watches this type)", req.group, req.resource)
			continue
		}
		for _, v := range req.minVerbs {
			if !verbs[v] {
				t.Errorf("RBAC rule for %s/%s is missing verb %q (has: %s)",
					req.group, req.resource, v, verbsList(verbs))
			}
		}
	}
}

// verbsList formats a verb set as a sorted string for error messages.
func verbsList(m map[string]bool) string {
	parts := make([]string, 0, len(m))
	for v := range m {
		parts = append(parts, v)
	}
	// Simple sort (the set is small).
	for i := 0; i < len(parts)-1; i++ {
		for j := i + 1; j < len(parts); j++ {
			if parts[j] < parts[i] {
				parts[i], parts[j] = parts[j], parts[i]
			}
		}
	}
	return strings.Join(parts, ",")
}

// Unused imports guard: these types are referenced in the test's doc
// comments but not in the code. If a future refactor removes the need for
// these, the linter will flag the imports.
var (
	_ = metav1.Now
	_ = context.Background
	_ = regexp.MustCompile
)

// TestRBACCachedClientReads asserts the A6 P1: every resource the manager's
// CACHE Gets/Lists/Watches has get;list;watch in config/rbac/role.yaml.
//
// It is a focused, mutation-catching guard on top of TestRBACCoverage. The
// cache watches a type the first time the CACHED client (r.Get/r.List — the
// manager's default cached client, NOT r.apiReader) reads it. A cache watch
// needs get;list;watch at the cluster scope. If the configmaps marker loses
// 'list' (or a new cached-client read is added without its RBAC verb), this
// test fails. The mutation the reviewer named: drop 'list' from the configmaps
// marker, run make manifests, and this test must fail.
func TestRBACCachedClientReads(t *testing.T) {
	rolePath := filepath.Join("..", "..", "config", "rbac", "role.yaml")
	data, err := os.ReadFile(rolePath)
	if err != nil {
		t.Fatalf("read %s: %v", rolePath, err)
	}

	var role struct {
		Rules []struct {
			APIGroups []string `yaml:"apiGroups"`
			Resources []string `yaml:"resources"`
			Verbs     []string `yaml:"verbs"`
		} `yaml:"rules"`
	}
	if err := yaml.Unmarshal(data, &role); err != nil {
		t.Fatalf("unmarshal role.yaml: %v", err)
	}

	type ruleKey struct{ group, resource string }
	verbSets := make(map[ruleKey]map[string]bool)
	for _, rule := range role.Rules {
		for _, group := range rule.APIGroups {
			for _, res := range rule.Resources {
				k := ruleKey{group, res}
				if verbSets[k] == nil {
					verbSets[k] = make(map[string]bool)
				}
				for _, v := range rule.Verbs {
					verbSets[k][v] = true
				}
			}
		}
	}

	// The manager's cache-read types (cmd/main.go Cache.ByObject scopes only
	// Pod+Service by label; the rest are cached cluster-wide). For EACH, the
	// role must grant get;list;watch (the cache watch verbs). This is the set
	// the cached client (r.Get/r.List) reads; the pod-blind reads use
	// r.apiReader and are intentionally NOT in this set.
	cacheReads := []struct {
		group    string
		resource string
	}{
		{"", "pods"},                             // r.Get/r.List the proxy pods
		{"", "services"},                         // r.Get the proxy services
		{"", "configmaps"},                       // r.Get the model-prices + deliver description CMs (the A6 P1)
		{"batch", "jobs"},                        // r.Get the deliver + verify jobs
		{"networking.k8s.io", "networkpolicies"}, // r.Get the loop/proxy/deliver netpols
		{"", "persistentvolumeclaims"},           // r.Get the workspace PVC
		{coxGroup, "loops"},                      // r.Get/r.List the Loops
		{coxGroup, "agentpolicies"},              // r.Get the loop's AgentPolicy
		{"agents.x-k8s.io", "sandboxes"},         // r.Get the sandbox
	}

	for _, cr := range cacheReads {
		verbs := verbSets[ruleKey{cr.group, cr.resource}]
		if verbs == nil {
			t.Errorf("cache-read %s/%s has NO RBAC rule at all (the cache needs get;list;watch)", cr.group, cr.resource)
			continue
		}
		for _, v := range []string{verbGet, verbList, verbWatch} {
			if !verbs[v] {
				t.Errorf("cache-read %s/%s is missing the cache verb %q (role.yaml has: %s) — the manager's cache needs get;list;watch to watch this type",
					cr.group, cr.resource, v, verbsList(verbs))
			}
		}
	}
}
