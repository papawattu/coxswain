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
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// testRepoURL is the fixture workspace repo used across the controller tests.
// It's a placeholder that the tests never actually clone (envtest has no
// git), so one shared constant avoids goconst churn.
const testRepoURL = "https://example.com/repo.git"

// testWorkspace returns a LoopSpec workspace pointing at the shared fixture
// repo, so the individual test files don't each repeat the literal.
func testWorkspace() coxv1alpha1.Workspace {
	return coxv1alpha1.Workspace{Repo: testRepoURL}
}

// nowSuffix returns a unique-enough suffix (nanoseconds) for self-contained
// envtest specs that create/delete their own namespace. The established idiom
// across the controller test files; centralised here so new specs stay
// consistent.
func nowSuffix() string {
	return fmt.Sprint(time.Now().UnixNano())
}

// Shared unstructured-Loop test fixture values. Named package-level constants
// (goconst) so the repeated literals across the controller test files are
// defined once.
const (
	loopAPIVersion = "coxswain.wattu.com/v1alpha1"
	loopKind       = "Loop"
	loopGoal       = "make the failing test pass"
	loopRepo       = testRepoURL
	loopRef        = "main"
	loopCheckCmd   = "go test ./..."

	// agentContainerName is the name of the Loop's agent container in the
	// Sandbox pod spec (goconst: it appears in several test files).
	agentContainerName = "agent"

	unstructuredGoal = "goal"
	unstructuredWs   = "workspace"
	unstructuredRepo = "repo"
	unstructuredRef  = "ref"
	unstructuredVer  = "verify"
	unstructuredAcck = "acceptanceChecks"
	unstructuredKind = "kind"
	unstructuredMeta = "metadata"
	unstructuredAPI  = "apiVersion"
	unstructuredSpec = "spec"
	unstructuredName = "name"
)

// mkUnstructuredLoop builds an unstructured Loop with the given name,
// namespace, and repo. The loop: block is omitted so the CRD's defaulting is
// what supplies spec.loop.* — used to exercise defaulting in envtest.
func mkUnstructuredLoop(name, ns, repo string) *unstructured.Unstructured {
	spec := map[string]any{
		unstructuredGoal: loopGoal,
		unstructuredWs: map[string]any{
			unstructuredRepo: repo,
			unstructuredRef:  loopRef,
		},
		unstructuredVer: map[string]any{
			unstructuredAcck: []any{loopCheckCmd},
		},
	}
	return &unstructured.Unstructured{Object: map[string]any{
		unstructuredAPI:  loopAPIVersion,
		unstructuredKind: loopKind,
		unstructuredMeta: map[string]any{unstructuredName: name, "namespace": ns},
		unstructuredSpec: spec,
	}}
}
