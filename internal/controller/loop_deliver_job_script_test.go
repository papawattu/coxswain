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

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// TestDeliverCloneImportScriptsInSequence (S6, kind-run finding 2026-10-03)
// runs the deliver Job's clone-base + import-agent scripts IN SEQUENCE against
// throwaway local git repos, exactly the way the init containers run them in
// the pod: the same script text (extracted from the container definitions the
// operator builds), run with bash (matching the container's busybox-ash
// POSIX-sh semantics; the I47 artifact-script-test idiom).
//
// The kind run (S6 first pass, kind-coxswain-dev, Loop gocli-task1) proved
// the scripts fail in sequence: clone-base clones the base into /deliver
// (a checked-out working tree), and import-agent then wiped /deliver/.git
// and re-inited the dir, leaving the base's checked-out files behind as
// UNTRACKED files — so the pinned-commit checkout aborted with "untracked
// working tree files would be overwritten by checkout". The import-agent
// script must fetch the pinned commit INTO the existing base clone (the
// verify Job's import-agent pattern) instead of wiping + re-initing.
//
// The envtests (loop_s6_delivery_envtest_test.go) build the Job spec but
// never execute the scripts — this test closes that gap. It must fail on
// the wiping script and pass on the fetch-into-clone script.
func TestDeliverCloneImportScriptsInSequence(t *testing.T) {
	r := &LoopReconciler{}

	// The "agent workspace" repo: a base commit + one agent commit on top.
	// The pinned verifiedCommit is the agent commit (what the verify Job
	// pins in status.currentVerify).
	agentRepo := t.TempDir()
	mustGit(t, agentRepo, "init", "-q", "-b", "main")
	mustGit(t, agentRepo, "config", "user.email", "agent@coxswain.test")
	mustGit(t, agentRepo, "config", "user.name", "Agent")
	writeFile(t, agentRepo, "round.go", "package main\n\nfunc Round(x float64) int { return int(x) }\n")
	writeFile(t, agentRepo, "round_test.go", "package main\n")
	mustGit(t, agentRepo, "add", "-A")
	mustGit(t, agentRepo, "commit", "-q", "-m", "base")
	baseSHA := gitSHA(t, agentRepo)
	writeFile(t, agentRepo, "round.go", "package main\n\nfunc Round(x float64) int { return int(x + 0.5) }\n")
	mustGit(t, agentRepo, "add", "-A")
	mustGit(t, agentRepo, "commit", "-q", "-m", "agent: fix rounding")
	verifySHA := gitSHA(t, agentRepo)
	if verifySHA == baseSHA {
		t.Fatal("the agent commit must differ from the base commit")
	}

	// The Loop the scripts were built for: workspace.repo points at the
	// agent repo (the deliver Job's clone-base 'origin'), ref is the base
	// branch name, and the pinned verifiedCommit is the agent commit.
	loop := &coxv1alpha1.Loop{
		Spec: coxv1alpha1.LoopSpec{
			Workspace: coxv1alpha1.Workspace{
				Repo: agentRepo + "/.git",
				Ref:  baseBranch,
			},
		},
		Status: coxv1alpha1.LoopStatus{
			CurrentVerify: &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: verifySHA},
		},
	}

	// The deliver scratch dir (the /deliver stand-in; one shared dir — the
	// init containers share the scratch volume): clone-base runs first,
	// then import-agent, against the SAME dir.
	scratch := t.TempDir()
	cloneScript := deliverContainerScript(t, r.deliverCloneBaseContainer(loop))
	cloneScript = rewriteDeliverScriptPaths(cloneScript, scratch, agentRepo)
	importScript := deliverContainerScript(t, r.deliverImportAgentContainer(loop))
	importScript = rewriteDeliverScriptPaths(importScript, scratch, agentRepo)
	clonePath := filepath.Join(scratch, "clone-base.sh")
	importPath := filepath.Join(scratch, "import-agent.sh")
	mustWriteFile(t, clonePath, []byte(cloneScript))
	mustWriteFile(t, importPath, []byte(importScript))

	// clone-base: fetch the base ref from 'origin' (the agent repo) and
	// detach onto it. The base files (round.go, round_test.go) end up
	// checked out in the scratch dir.
	if out, err := exec.Command("bash", clonePath).CombinedOutput(); err != nil {
		t.Fatalf("clone-base script failed; want 0. err: %v, output: %s", err, string(out))
	}

	// import-agent: fetch the pinned commit from the agent workspace into
	// the existing base clone and detach onto it. The base's checked-out
	// files must NOT block the checkout (the kind-run failure mode), and
	// the final HEAD must be the pinned commit.
	if out, err := exec.Command("bash", importPath).CombinedOutput(); err != nil {
		t.Fatalf("import-agent script failed after a successful clone-base (the kind-run failure mode); want 0. err: %v, output: %s", err, string(out))
	}
	head := gitSHA(t, scratch)
	if head != verifySHA {
		t.Fatalf("import-agent left HEAD at %s; want the pinned verified commit %s", head, verifySHA)
	}
	// The pinned commit's tree must actually be checked out (the push
	// container pushes this tree).
	if out, err := exec.Command("git", "-C", scratch, "diff", "--quiet", verifySHA).CombinedOutput(); err != nil {
		t.Fatalf("working tree differs from the pinned commit after import-agent: %v (output: %s)", err, string(out))
	}
}

// deliverContainerScript extracts the shell script from a container built as
// [sh, -c, <script>] (the deliver Job's container pattern) and fails the
// test if the shape changed (the test rewrites the pod paths, so it must
// know exactly what it is running).
func deliverContainerScript(t *testing.T, c corev1.Container) string {
	t.Helper()
	if len(c.Command) != 3 || c.Command[0] != verifySh || c.Command[1] != "-c" {
		t.Fatalf("deliver container %q command %v; want [sh, -c, <script>]", c.Name, c.Command)
	}
	return c.Command[2]
}

// rewriteDeliverScriptPaths rewrites the pod paths in a deliver script to
// the test's local stand-ins: /deliver -> the scratch dir, /agent-src ->
// the agent repo, /workspace-creds -> a scratch credentials dir (the test
// Loops declare no gitCredentialSecret, so the credential lines are absent
// and only the path constants are rewritten).
func rewriteDeliverScriptPaths(script, scratch, agentRepo string) string {
	script = strings.ReplaceAll(script, deliverScratchPath, scratch)
	script = strings.ReplaceAll(script, deliverAgentSrc, agentRepo)
	script = strings.ReplaceAll(script, "/workspace-creds", scratch+"/creds")
	return script
}
