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
// git), so one shared constant avoids goconst churn. It is deliberately an
// IN-CLUSTER host (F55): an external repo makes the sandbox gate wait for the
// egress proxy pod (workspaceInitProxyCreated), which envtest never makes
// Ready, so specs that expect a Running sandbox must not use one. Specs about
// the egress proxy gate use their own external URL.
const testRepoURL = "http://gitea.samples.svc:3000/samples/gocli.git"

// agentPolicyExecGit is the exec allow the AgentPolicy test fixtures reference
// (git), shared across the policy test files to avoid goconst churn.
const agentPolicyExecGit = "/usr/bin/git"

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
	runnerImage    = "docker.io/library/golang:1.26"
	testModel      = "local-model"
	loopCheckCmd   = "go test ./..."
	sleepInfinity  = "sleep infinity"
	httpMethodGet  = "GET"

	// agentContainerName is the name of the Loop's agent container in the
	// Sandbox pod spec (goconst: it appears in several test files).
	agentContainerName = "agent"
)

// unstructuredGoal is the "goal" key (a goconst-shared constant across the
// controller's unstructured object builders).
const unstructuredGoal = "goal"

// unstructuredWs is the "workspace" key (a goconst-shared constant across the
// controller's unstructured object builders).
const unstructuredWs = "workspace"

// unstructuredRepo is the "repo" key (a goconst-shared constant across the
// controller's unstructured object builders).
const unstructuredRepo = "repo"

// unstructuredRef is the "ref" key (a goconst-shared constant across the
// controller's unstructured object builders).
const unstructuredRef = "ref"

// unstructuredVer is the "verify" key (a goconst-shared constant across the
// controller's unstructured object builders).
const unstructuredVer = "verify"

// unstructuredAcck is the "acceptanceChecks" key (a goconst-shared constant
// across the controller's unstructured object builders).
const unstructuredAcck = "acceptanceChecks"

// unstructuredMeta is the "metadata" key (a goconst-shared constant across the
// controller's unstructured object builders).
const unstructuredMeta = "metadata"

// unstructuredSpec is the "spec" key (a goconst-shared constant across the
// controller's unstructured object builders).
const unstructuredSpec = "spec"

// unstructuredName is the "name" key (a goconst-shared constant across the
// controller's unstructured object builders).
const unstructuredName = "name"

// unstructuredEnv is the "env" key (a goconst-shared constant across the
// controller's unstructured object builders).
const unstructuredEnv = "env"

// unstructuredValue is the "value" key (a goconst-shared constant across the
// controller's unstructured object builders).
const unstructuredValue = "value"

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
		unstructuredMeta: map[string]any{unstructuredName: name, unstructuredNs: ns},
		unstructuredSpec: spec,
	}}
}
