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
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// TestDeliverPlanCarriedFromPVCNotCheckout (R26 P1) runs the deliver Job's REAL
// import-agent and push scripts in sequence against throwaway local stand-ins
// and asserts the plan the PR body carries comes from the AGENT WORKSPACE PVC
// (.coxswain/PLAN.md — agent data EXCLUDED from the agent commit, I80/D51),
// carried by import-agent into the scratch volume OUTSIDE the git tree
// (/deliver/plan.md), and read by the push script from that copy.
//
// The scenario the kind run proved broken (the plan section was always empty):
// the push script read .coxswain/PLAN.md from the scratch CHECKOUT, which can
// never carry it — the file is excluded from the agent commit — and the push
// container does not mount the agent PVC. The fix: import-agent (which mounts
// the PVC read-only and the scratch volume writable) copies the plan into the
// scratch volume OUTSIDE the git tree; the push script reads that copy.
//
// The assertions (the body is read back from the fake Gitea API's recorded
// PR-create POST, exactly the bytes the operator would read from the Gitea
// API):
//  1. A PLAN.md present on the agent PVC but ABSENT from the commit appears in
//     the PR body (the body carries the plan text, fenced + labelled);
//  2. A SYMLINKED PLAN.md is not followed (the body renders the no-plan line;
//     the symlink target's content never reaches the body);
//  3. A MISSING plan renders the no-plan line ("_(no plan recorded)_").
//
// It must FAIL on the mutation that reads the plan from the scratch checkout
// again (PLAN_FILE=${SRC}/.coxswain/PLAN.md in the push script): in that
// mutation the plan is absent from the checkout (excluded from the commit), so
// the body renders the no-plan line and assertion 1 fails.
func TestDeliverPlanCarriedFromPVCNotCheckout(t *testing.T) {
	r := &LoopReconciler{}

	// The remote (the deliver Job's origin stand-in): a local bare repo with
	// a base branch.
	remoteDir := t.TempDir()
	remoteGit := filepath.Join(remoteDir, "remote.git")
	mustGit(t, remoteDir, "init", "-q", "-b", baseBranch, "--bare", remoteGit)

	// The agent repo: a base commit (the remote's base branch) + one agent
	// commit (the pinned verifiedCommit). .coxswain/ is NEVER committed
	// (I80/D51 — the agent commit excludes it), so it is absent from the
	// scratch checkout by construction.
	agentRepo := t.TempDir()
	mustGit(t, agentRepo, "init", "-q", "-b", baseBranch)
	mustGit(t, agentRepo, "config", "user.email", "agent@coxswain.test")
	mustGit(t, agentRepo, "config", "user.name", "Agent")
	writeFile(t, agentRepo, "round.go", "package main\n\nfunc Round(x float64) int { return int(x) }\n")
	mustGit(t, agentRepo, "add", "-A")
	mustGit(t, agentRepo, "commit", "-q", "-m", "base")
	baseSHA := gitSHA(t, agentRepo)
	mustGit(t, agentRepo, "remote", "add", "origin", remoteGit)
	mustGit(t, agentRepo, "push", "-q", "origin", baseBranch)
	writeFile(t, agentRepo, "round.go", "package main\n\nfunc Round(x float64) int { return int(x + 0.5) }\n")
	mustGit(t, agentRepo, "add", "-A")
	mustGit(t, agentRepo, "commit", "-q", "-m", "agent: fix rounding")
	verifySHA := gitSHA(t, agentRepo)
	if verifySHA == baseSHA {
		t.Fatal("the agent commit must differ from the base commit")
	}

	const loopName = "plantask1"
	repoURL := testGiteaExampleRepoURL
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: loopName},
		Spec: coxv1alpha1.LoopSpec{
			Workspace: coxv1alpha1.Workspace{
				Repo:                repoURL,
				Ref:                 baseBranch,
				GitCredentialSecret: testCredSecretName,
			},
			Delivery: &coxv1alpha1.DeliveryConfig{Mode: coxv1alpha1.DeliveryModePullRequest},
		},
		Status: coxv1alpha1.LoopStatus{
			CurrentVerify: &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: verifySHA},
		},
	}
	branch := deliverBranchName(deliverBranchPrefix(loop), loopName, string(loop.UID))

	// The plan content (on the PVC only, never in a commit).
	const planText = "Agent plan: fix Round to round half away from zero.\n- step 1: change round.go\n- step 2: run go test ./...\n"

	// The credential files (the /workspace-creds mount stand-in).
	credsDir := t.TempDir()
	mustWriteFile(t, filepath.Join(credsDir, workspaceCredsUsernameKey), []byte("samples"))
	mustWriteFile(t, filepath.Join(credsDir, workspaceCredsPasswordKey), []byte("s3cret-pw"))

	// The scratch dir (the /deliver stand-in; shared by clone-base,
	// import-agent and push, as in the pod).
	scratch := t.TempDir()
	importScript := deliverContainerScript(t, r.deliverImportAgentContainer(loop))
	importScript = rewriteDeliverScriptPaths(importScript, scratch, agentRepo)
	pushScript := deliverContainerScript(t, r.deliverPushContainer(loop, verifySHA, branch, baseBranch))
	termFile := filepath.Join(t.TempDir(), "termination-log")
	pushScript = strings.ReplaceAll(pushScript, deliverScratchPath, scratch)
	pushScript = strings.ReplaceAll(pushScript, "/workspace-creds", credsDir)
	pushScript = strings.ReplaceAll(pushScript, "/dev/termination-log", termFile)
	// The push script's REPO is the Gitea URL; the push uses 'origin' (the
	// local bare remote), and the PR calls go to the fake API. The facts body
	// (FACTS) is empty in these runs: the description ConfigMap is not
	// mounted, so the push script uses its minimal fallback line — the plan
	// section is what this test asserts.
	wantAPIBase := "'" + deliverAPIBase(repoURL, deliverProviderGitea) + "'"
	if !strings.Contains(pushScript, wantAPIBase) {
		t.Fatalf("push script does not carry the expected API_BASE %q (did the shape change?)", wantAPIBase)
	}

	// The plan section the python appends (labelled + fenced, or the no-plan
	// line). The push reads the plan from the scratch-volume copy (outside
	// the git tree), which import-agent carries from the agent PVC.
	const noPlanLine = "_(no plan recorded)_"

	// runCase runs the REAL import-agent + push scripts in sequence (a fresh
	// scratch clone each time) and returns the PR-create body the fake Gitea
	// API recorded (the exact bytes the push sent). The API base is rewritten
	// into the push script per case (the fake stands up a fresh server per
	// case).
	runCase := func(t *testing.T, apiBase string, label string) string {
		t.Helper()
		// re-clone the remote at the base (clone-base's work; the clone-base
		// script is separately tested in TestDeliverCloneImportScriptsInSequence).
		if _, err := exec.Command("rm", "-rf", filepath.Join(scratch, ".git"), filepath.Join(scratch, "round.go"), filepath.Join(scratch, "plan.md")).CombinedOutput(); err != nil {
			t.Fatalf("[%s] wipe the scratch dir: %v", label, err)
		}
		mustGit(t, scratch, "clone", "-q", remoteGit, ".")
		// import-agent: fetch the pinned commit from the agent repo INTO the
		// existing base clone and detach onto it, AND carry the plan (if
		// present, regular, non-symlink) into the scratch volume.
		if out, err := exec.Command("bash", "-c", importScript).CombinedOutput(); err != nil {
			t.Fatalf("[%s] import-agent script failed; want 0. err: %v, output: %s", label, err, string(out))
		}
		if got := gitSHA(t, scratch); got != verifySHA {
			t.Fatalf("[%s] import-agent left HEAD at %s; want the pinned verified commit %s", label, got, verifySHA)
		}
		// The push: rewrite the API base for this run and run the REAL
		// script. The push POSTs the PR create to the fake API; the fake
		// records the POST body.
		p := strings.ReplaceAll(pushScript, wantAPIBase, "'"+apiBase+"'")
		pushPath := filepath.Join(t.TempDir(), "push.sh")
		mustWriteFile(t, pushPath, []byte(p))
		if out, err := exec.Command("sh", pushPath).CombinedOutput(); err != nil {
			t.Fatalf("[%s] push script failed; want 0. err: %v, output: %s", label, err, string(out))
		}
		// Read the PR-create body the fake recorded (the push POSTs exactly
		// one PR per run; the body is the exact bytes the operator would
		// read from the Gitea API).
		return postCreateBody(t, apiBase)
	}

	// Case 1: a PLAN.md present on the agent PVC (ABSENT from the commit)
	// appears in the PR body. Write the plan to the agent workspace's
	// .coxswain/ (never committed — git add only round.go; .coxswain is
	// untracked, mirroring I80/D51's exclusion).
	planDir := filepath.Join(agentRepo, ".coxswain")
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		t.Fatalf("mkdir the agent .coxswain dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(planDir, "PLAN.md"), []byte(planText), 0o644); err != nil {
		t.Fatalf("write the agent plan: %v", err)
	}
	// The pinned commit's checkout must NOT carry .coxswain (excluded from
	// the commit). Guard: the agent repo's .coxswain is untracked.
	if out, err := exec.Command("git", "-C", agentRepo, "ls-files", ".coxswain").CombinedOutput(); err != nil {
		t.Fatalf("git ls-files .coxswain: %v", err)
	} else if strings.TrimSpace(string(out)) != "" {
		t.Fatalf(".coxswain is tracked in the agent repo (the scenario requires it excluded from the commit); git ls-files: %s", strings.TrimSpace(string(out)))
	}
	apiBase, _ := newFakeGiteaAPI(t, "samples", "gocli", "http", "gitea.example:3000", baseBranch)
	body := runCase(t, apiBase, "plan present")
	// The body carries the plan content (fenced + labelled). The plan is on
	// the PVC only (never in the commit), so this proves the push read it
	// from import-agent's carried copy, not from the checkout.
	if !strings.Contains(body, planText) {
		t.Fatalf("case 1: the PR body does not carry the plan (present on the agent PVC, absent from the commit).\n\nplan:\n%s\n\nbody:\n%s", planText, body)
	}
	if !strings.Contains(body, "## Plan (written by the agent, untrusted)") {
		t.Fatalf("case 1: the PR body does not carry the labelled plan section:\n%s", body)
	}
	// The scratch-volume copy must be OUTSIDE the git tree (a sibling of the
	// checkout, not inside .git or the work tree). The file must exist at
	// <scratch>/plan.md and must NOT be tracked by the scratch repo.
	if _, err := os.Stat(filepath.Join(scratch, "plan.md")); err != nil {
		t.Fatalf("case 1: import-agent must carry the plan to %s/plan.md (outside the git tree): %v", scratch, err)
	}
	if out, err := exec.Command("git", "-C", scratch, "ls-files", "plan.md").CombinedOutput(); err != nil {
		t.Fatalf("case 1: git ls-files plan.md in the scratch clone: %v", err)
	} else if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("case 1: the carried plan must be OUTSIDE the git tree (not tracked by the scratch checkout); git ls-files plan.md: %s", strings.TrimSpace(string(out)))
	}

	// Case 2: a SYMLINKED PLAN.md is not followed (the body renders the
	// no-plan line; the symlink target's content never reaches the body).
	// The symlink target is a file whose content differs from any plan the
	// body might carry: if the import followed the link, that content would
	// be in the body and this assertion fails.
	targetFile := filepath.Join(t.TempDir(), "link-target.md")
	if err := os.WriteFile(targetFile, []byte("SYMLINK TARGET — must never appear in the PR body\n"), 0o644); err != nil {
		t.Fatalf("write the symlink target: %v", err)
	}
	if err := os.Remove(filepath.Join(planDir, "PLAN.md")); err != nil {
		t.Fatalf("remove the case-1 plan: %v", err)
	}
	if err := os.Symlink(targetFile, filepath.Join(planDir, "PLAN.md")); err != nil {
		t.Fatalf("symlink PLAN.md to the target: %v", err)
	}
	apiBase2, _ := newFakeGiteaAPI(t, "samples", "gocli", "http", "gitea.example:3000", baseBranch)
	body2 := runCase(t, apiBase2, "plan symlinked")
	if strings.Contains(body2, "SYMLINK TARGET") {
		t.Fatalf("case 2: the PR body carries the symlink target's content — a symlinked PLAN.md must NOT be followed:\n%s", body2)
	}
	if !strings.Contains(body2, noPlanLine) {
		t.Fatalf("case 2: the PR body must render the no-plan line for a symlinked PLAN.md:\n%s", body2)
	}

	// Case 3: a MISSING plan renders the no-plan line (the body has the
	// labelled section with the no-plan line, not an empty fence).
	if err := os.Remove(filepath.Join(planDir, "PLAN.md")); err != nil {
		t.Fatalf("remove the case-2 symlink: %v", err)
	}
	apiBase3, _ := newFakeGiteaAPI(t, "samples", "gocli", "http", "gitea.example:3000", baseBranch)
	body3 := runCase(t, apiBase3, "plan missing")
	if !strings.Contains(body3, noPlanLine) {
		t.Fatalf("case 3: the PR body must render the no-plan line for a missing plan:\n%s", body3)
	}
	if !strings.Contains(body3, "## Plan (written by the agent, untrusted)") {
		t.Fatalf("case 3: the PR body must carry the labelled plan section (with the no-plan line):\n%s", body3)
	}
}

// postCreateBody returns the body of the PR-create POST the fake Gitea API
// recorded for the LAST create. The push POSTs the PR create to the fake, then
// does a GET /pulls lookup; the fake's GET serves the recorded body in the
// list response (matching Gitea's behaviour of returning the PR object with
// its body). So this GETs the same /pulls path the push GETs and parses the
// body out of the list response — the exact bytes the push sent, which is
// what the operator would read from the Gitea API.
func postCreateBody(t *testing.T, apiBase string) string {
	t.Helper()
	resp, err := http.Get(apiBase + "/pulls")
	if err != nil {
		t.Fatalf("postCreateBody: GET the fake's /pulls: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("postCreateBody: read the PR list: %v", err)
	}
	var list []struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("postCreateBody: parse the PR list: %v", err)
	}
	if len(list) == 0 {
		t.Fatalf("postCreateBody: the fake returned no open PR (the push must have created one)")
	}
	return list[0].Body
}
