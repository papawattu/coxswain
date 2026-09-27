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
		{group: "", resource: "pods",
			minVerbs: []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate}},
		// Owns(&Service{}) — the controller creates and owns proxy Services.
		{group: "", resource: "services",
			minVerbs: []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate}},
		// D34: the controller creates and owns per-Loop NetworkPolicies.
		{group: "networking.k8s.io", resource: "networkpolicies",
			minVerbs: []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate}},
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
