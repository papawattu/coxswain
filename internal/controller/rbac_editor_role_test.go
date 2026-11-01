/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestRBACEditorRoleIsStatusReadOnly verifies D85 (owner decision, R34): the
// editor role grants NO update or patch on loops/status — a user bound to it
// can no longer write the status the operator acts on (the verify/tamper
// evidence, the budget counters, the stall history, the checkpoints). The
// role keeps get on loops/status (read the conditions) and patch on loops
// (the cox approve / cox reject one-shot annotation is a metadata patch on
// the main resource, authorised by that existing grant).
//
// It is a YAML-parse guard, not an envtest assertion: envtest runs as
// cluster-admin, so an over-granted editor role is invisible there — exactly
// the class of RBAC defect this file guards against (the B1 storageclasses
// lesson).
func TestRBACEditorRoleIsStatusReadOnly(t *testing.T) {
	rolePath := filepath.Join("..", "..", "config", "rbac", "loop_editor_role.yaml")
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
		t.Fatalf("unmarshal loop_editor_role.yaml: %v", err)
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

	// The D85 guard: NO update or patch on loops/status (the grant that let
	// an editor-bound user forge every status field the operator acts on).
	status := verbSets[ruleKey{coxGroup, "loops/status"}]
	for _, v := range []string{"update", "patch"} {
		if status != nil && status[v] {
			t.Errorf("editor role grants %q on loops/status (D85: the editor role must be get-only on loops/status; the one-shot approval annotation goes through patch on loops)", v)
		}
	}
	if status == nil || !status[verbGet] {
		t.Errorf("editor role is missing get on loops/status (it must keep reading the conditions)")
	}

	// The existing grant that now carries the CLI write: patch on loops
	// (the metadata merge patch of the one-shot annotation).
	loops := verbSets[ruleKey{coxGroup, "loops"}]
	if loops == nil || !loops[verbPatch] {
		t.Errorf("editor role is missing patch on loops (the cox approve / cox reject one-shot annotation is a metadata patch on the main resource)")
	}
}
