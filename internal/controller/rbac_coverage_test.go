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
	coxGroup        = "coxswain.wattu.com"
	storageGroup    = "storage.k8s.io"
	agentsGroup     = "agents.x-k8s.io"
	networkingGroup = "networking.k8s.io"
	batchGroup      = "batch"
	coreGroup       = ""
	verbGet         = "get"
	verbList        = "list"
	verbWatch       = "watch"
	verbCreate      = "create"
	verbPatch       = "patch"
	verbUpdate      = "update"
)

// knownResourceGroup is the trusted table of resource -> its real API group,
// derived from the Kubernetes API (the RESTMapper). It exists so the RBAC
// coverage test can catch a rule placed under the WRONG group: the generated
// role.yaml must grant the verbs on (group, resource) where group is the
// resource's real group, not an arbitrary one. envtest runs as cluster-admin
// so a wrong group is invisible there (the storageclasses B1 P1: the marker
// and this test both said the core group "", so the test passed on the bug);
// this table pins the group so a wrong marker (and a wrong test entry) both
// fail.
//
// Only the groups the controller touches are listed. A resource NOT in the
// table defaults to the core group "" (the K8s convention: core resources
// live in group "").
var knownResourceGroup = map[string]string{
	// core (group "")
	corePodsResource:         coreGroup,
	"services":               coreGroup,
	"configmaps":             coreGroup,
	"persistentvolumeclaims": coreGroup,
	// non-core
	"storageclasses":        storageGroup,
	"sandboxes":             agentsGroup,
	"sandboxes/status":      agentsGroup,
	"volumesnapshots":       snapshotGroup,
	"volumesnapshotclasses": snapshotGroup,
	"networkpolicies":       networkingGroup,
	"jobs":                  batchGroup,
	"loops":                 coxGroup,
	"loops/status":          coxGroup,
	"loops/finalizers":      coxGroup,
	"agentpolicies":         coxGroup,
}

// groupForResource returns the resource's real API group from the trusted
// table, defaulting to the core group for unlisted (core) resources.
func groupForResource(resource string) string {
	if g, ok := knownResourceGroup[resource]; ok {
		return g
	}
	return coreGroup
}

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
		{group: agentsGroup, resource: "sandboxes",
			minVerbs: []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate}},
		// Sandbox status (the controller reads sandbox status).
		{group: agentsGroup, resource: "sandboxes/status",
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
		{group: batchGroup, resource: "jobs",
			minVerbs: []string{verbGet, verbList, verbWatch}},
		// NetworkPolicies: the loop/proxy/deliver netpols are read through the
		// cached client (loop_controller.go r.Get).
		{group: networkingGroup, resource: "networkpolicies",
			minVerbs: []string{verbGet, verbList, verbWatch}},
		// PersistentVolumeClaims: the workspace PVC is read through the cached
		// client (loop_controller.go r.Get).
		{group: "", resource: "persistentvolumeclaims",
			minVerbs: []string{verbGet, verbList, verbWatch}},
		// ServiceAccounts: the operator ensures the memory-writer Job's
		// dedicated ServiceAccount in the Loop's namespace
		// (ensureMemoryWriterServiceAccount, loop_memory_writer_job.go — the
		// Job runs in the Loop's namespace; a Job that references a missing SA
		// cannot create its pod). r.Get (idempotency, cached read — needs
		// get;list;watch) + r.Create (ensure). The SA is NOT owned by the Loop
		// (shared per namespace) and has NO RoleBinding (the Job's pod is
		// no-token — automountServiceAccountToken: false — so it makes no API
		// calls; the operator only creates the SA, not the binding).
		{group: "", resource: "serviceaccounts",
			minVerbs: []string{verbGet, verbList, verbWatch, verbCreate}},
		// AgentPolicies: the loop's AgentPolicy is read through the cached
		// client (loop_controller.go r.Get).
		{group: coxGroup, resource: "agentpolicies",
			minVerbs: []string{verbGet, verbList, verbWatch}},
		// VolumeSnapshots: the B1 checkpoint reads (ensureCheckpoint's
		// AlreadyExists re-read, applyCheckpointRetention's list + delete,
		// gcLoopCheckpoints' label list) go through the cached client (I86):
		// the manager's cache lazily watches the type the first time the cached
		// client reads it, so the marker needs get;list;watch IN ADDITION TO
		// create;delete;patch (a create-only marker wedges a fresh install on
		// the first cached read, exactly like the A6 configmaps P1). The patch
		// verb is for the fork-retention label (removeForkRetentionLabel
		// patches the source's VolumeSnapshot).
		{group: snapshotGroup, resource: "volumesnapshots",
			minVerbs: []string{verbGet, verbList, verbWatch, verbCreate, verbPatch}},
		// VolumeSnapshotClasses: the checkpoint support check reads the
		// cluster's VolumeSnapshotClasses (cluster-scoped, cached read).
		{group: snapshotGroup, resource: "volumesnapshotclasses",
			minVerbs: []string{verbGet, verbList, verbWatch}},
		// StorageClasses: the checkpoint support check reads the workspace
		// PVC's StorageClass (the CSI driver) through the cached client. The
		// group is storage.k8s.io (NOT core "") — storageclasses is a
		// cluster-scoped resource in the storage group. The B1 P1: the marker
		// (and this entry) wrongly used the core group, which envtest (admin)
		// could not catch; groupForResource pins the real group.
		{group: storageGroup, resource: "storageclasses",
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

	// Group-correctness assertion (the B1 storageclasses P1 guard): for every
	// required entry, the group listed here must be the resource's REAL API
	// group (per the trusted knownResourceGroup table). A wrong group in this
	// test list fails the test, so the test cannot pass on a bug where the
	// marker and the test list both name the wrong group (exactly the
	// storageclasses case: both said the core group ""). This is what envtest
	// cannot catch (it runs as cluster-admin).
	for _, req := range required {
		if want := groupForResource(req.resource); req.group != want {
			t.Errorf("required RBAC entry for %s names group %q but the resource's real group is %q (wrong group fails here — the marker must use %q)",
				req.resource, req.group, want, want)
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
		{"", "serviceaccounts"},                  // r.Get the memory-writer SA (the ensure's idempotency read; the Loop's namespace)
		{coxGroup, "loops"},                      // r.Get/r.List the Loops
		{coxGroup, "agentpolicies"},              // r.Get the loop's AgentPolicy
		{agentsGroup, "sandboxes"},               // r.Get the sandbox
		// B1 (I86): the checkpoint VolumeSnapshots are read through the cached
		// client (ensureCheckpoint / applyCheckpointRetention / gcLoopCheckpoints).
		{snapshotGroup, "volumesnapshots"},
		{snapshotGroup, "volumesnapshotclasses"}, // the support check
		{storageGroup, "storageclasses"},         // the support check (the PVC's CSI driver; group storage.k8s.io, NOT core "")
	}

	// Group-correctness assertion (the B1 storageclasses P1 guard): for every
	// required and cache-read entry, the group listed here must be the
	// resource's REAL API group (per the trusted knownResourceGroup table).
	// A wrong group in this test list fails the test, so the test cannot pass
	// on a bug where the marker and the test list both name the wrong group
	// (exactly the storageclasses case: both said the core group ""). This is
	// what envtest cannot catch (it runs as cluster-admin). The required-entry
	// group check lives in TestRBACCoverage (where `required` is scoped).
	for _, cr := range cacheReads {
		if want := groupForResource(cr.resource); cr.group != want {
			t.Errorf("cache-read entry for %s names group %q but the resource's real group is %q (wrong group fails here)",
				cr.resource, cr.group, want)
		}
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
