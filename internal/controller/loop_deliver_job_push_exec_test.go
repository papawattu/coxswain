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
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// TestDeliverPushScriptExecutes (S6, kind-run finding 2026-10-03 s6b/s6c)
// runs the deliver Job's REAL push container script end-to-end with sh, the
// same way the push container runs it in the pod:
//
//   - the push script is extracted from the container the operator builds
//     (deliverPushContainer) — the exact script text that runs in the Job;
//   - the git origin is a LOCAL bare repo (the remote the push pushes to);
//   - the provider API (Gitea) is an httptest stand-in that records the PR
//     create POST + auth header and returns a draft PR number;
//   - the credential is read from files (the /workspace-creds mount stand-in);
//   - the termination log (/dev/termination-log) is a local file the operator
//     would read back via the APIReader.
//
// The assertions close the gap the envtests (which build the Job spec but
// never execute the script) and the static ordering check (which parses the
// script text) leave: this runs the script and proves it
//   - pushes the pinned verifiedCommit to the delivery branch (the remote
//     branch ref == the pinned commit),
//   - creates EXACTLY ONE draft PR (the API saw one POST) carrying the
//     credential's Basic auth header, and
//   - writes a termination message the operator's strict parser accepts.
//
// It must FAIL on a996122: that script's API_AUTH header line referenced
// $AUTH ABOVE the AUTH assignment, so under set -u the script died with
// 'AUTH: parameter not set' before the git push — the s6b/s6c kind-run
// failure. The 4156154 fix moves the API auth below the push; this test
// passes only on that ordering.
func TestDeliverPushScriptExecutes(t *testing.T) {
	r := &LoopReconciler{}

	// The remote (the deliver Job's origin stand-in): a local bare repo with
	// a base branch. The push pushes HEAD (the pinned commit) to a new
	// delivery branch here.
	remoteDir := t.TempDir()
	remoteGit := filepath.Join(remoteDir, "remote.git")
	mustGit(t, remoteDir, "init", "-q", "-b", baseBranch, "--bare", remoteGit)

	// The agent repo: a base commit (the remote's base branch) + one agent
	// commit on top (the pinned verifiedCommit — what verify pinned in
	// status.currentVerify). The agent repo's base commit is pushed into the
	// bare remote so the base branch pre-exists (clone-base's requirement:
	// the base branch exists on the remote).
	agentRepo := t.TempDir()
	mustGit(t, agentRepo, "init", "-q", "-b", baseBranch)
	mustGit(t, agentRepo, "config", "user.email", "agent@coxswain.test")
	mustGit(t, agentRepo, "config", "user.name", "Agent")
	writeFile(t, agentRepo, "round.go", "package main\n\nfunc Round(x float64) int { return int(x) }\n")
	mustGit(t, agentRepo, "add", "-A")
	mustGit(t, agentRepo, "commit", "-q", "-m", "base")
	baseSHA := gitSHA(t, agentRepo)
	// Push the base branch into the bare remote (origin for the scratch clone).
	mustGit(t, agentRepo, "remote", "add", "origin", remoteGit)
	mustGit(t, agentRepo, "push", "-q", "origin", baseBranch)
	// The agent commit (the pinned verifiedCommit).
	writeFile(t, agentRepo, "round.go", "package main\n\nfunc Round(x float64) int { return int(x + 0.5) }\n")
	mustGit(t, agentRepo, "add", "-A")
	mustGit(t, agentRepo, "commit", "-q", "-m", "agent: fix rounding")
	verifySHA := gitSHA(t, agentRepo)
	if verifySHA == baseSHA {
		t.Fatal("the agent commit must differ from the base commit")
	}

	// The Loop the push script was built for: a Gitea-compatible repo (the
	// provider API + PR URL derive from its host; the git push uses the
	// origin remote, which we point at the local bare repo).
	const loopName = "pushtask1"
	repoURL := "http://gitea.example:3000/samples/gocli.git"
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
	branch := deliverBranchName(deliverBranchPrefix(loop), loopName)

	// The fake Gitea API: GET /pulls (idempotency lookup) -> empty list;
	// POST /pulls (create) -> draft PR number 42. Records every POST so the
	// test can assert exactly one POST with the credential's Basic header.
	// The html_url the fake returns names the REAL repo (samples/gocli) —
	// the push script records it verbatim as prURL.
	apiBase, posts := newFakeGiteaAPI(t, "samples", "gocli", "http", "gitea.example:3000", baseBranch)

	// The credential files (the /workspace-creds mount stand-in).
	credsDir := t.TempDir()
	mustWriteFile(t, filepath.Join(credsDir, workspaceCredsUsernameKey), []byte("samples"))
	mustWriteFile(t, filepath.Join(credsDir, workspaceCredsPasswordKey), []byte("s3cret-pw"))

	// The scratch dir (the /deliver stand-in). Set up exactly as clone-base +
	// import-agent would: a clone of the remote at the base commit, then the
	// pinned commit fetched INTO that clone and checked out (HEAD == pinned).
	scratch := t.TempDir()
	mustGit(t, scratch, "clone", "-q", remoteGit, ".")
	// The scratch clone has the base; fetch the pinned commit into it (as
	// import-agent does) and detach onto it.
	mustGit(t, scratch, "-c", "protocol.file.allow=always", "-c", "core.hooksPath=/dev/null",
		"fetch", "file://"+agentRepo+"/.git", verifySHA)
	mustGit(t, scratch, "checkout", "-q", "--detach", verifySHA)
	if got := gitSHA(t, scratch); got != verifySHA {
		t.Fatalf("scratch HEAD is %s; want the pinned verified commit %s (the push pushes this)", got, verifySHA)
	}
	// The push script's origin must be the local bare remote (the git push
	// uses 'origin', not $REPO). Point it at the bare remote.
	mustGit(t, scratch, "remote", "set-url", "origin", remoteGit)

	// Extract the REAL push script and rewrite the pod paths to the test
	// stand-ins: /deliver -> scratch, /workspace-creds -> credsDir,
	// /dev/termination-log -> a local file, and API_BASE -> the fake API.
	// REPO (the Gitea URL) is left as-is: it drives owner/name (the script
	// assembles them for the API calls) — the termination message's prURL is
	// the provider's own html_url, which the script records verbatim.
	pushScript := deliverContainerScript(t, r.deliverPushContainer(loop, verifySHA, branch, baseBranch))
	termFile := filepath.Join(t.TempDir(), "termination-log")
	pushScript = strings.ReplaceAll(pushScript, deliverScratchPath, scratch)
	pushScript = strings.ReplaceAll(pushScript, "/workspace-creds", credsDir)
	pushScript = strings.ReplaceAll(pushScript, "/dev/termination-log", termFile)
	// Override ONLY the API base (the curl calls): the script records the
	// fake API's html_url as prURL.
	wantAPIBase := "'" + deliverAPIBase(repoURL, deliverProviderGitea) + "'"
	if !strings.Contains(pushScript, wantAPIBase) {
		t.Fatalf("push script does not carry the expected API_BASE %q (did the shape change?)", wantAPIBase)
	}
	pushScript = strings.ReplaceAll(pushScript, wantAPIBase, "'"+apiBase+"'")

	pushPath := filepath.Join(t.TempDir(), "push.sh")
	mustWriteFile(t, pushPath, []byte(pushScript))

	// Run 1 (refusal): the script's HEAD assert must REFUSE before anything
	// is pushed when the scratch clone's HEAD is NOT the pinned commit (the
	// regression the assert exists for: a stale or moved HEAD must never be
	// delivered as the verifiedCommit). Move HEAD off the pinned commit and
	// run: the script must exit non-zero and the delivery branch must not
	// exist on the remote. (This must FAIL on the pre-fix script, which
	// pushed "HEAD:refs/heads/<branch>" and would deliver the moved commit.)
	mustGit(t, scratch, "-c", "protocol.file.allow=always", "-c", "core.hooksPath=/dev/null",
		"fetch", "file://"+agentRepo+"/.git", baseSHA)
	mustGit(t, scratch, "checkout", "-q", "--detach", baseSHA)
	if out, err := exec.Command("sh", pushPath).CombinedOutput(); err == nil {
		t.Fatalf("with HEAD moved off the pinned commit, the push script must refuse (exit non-zero); it exited 0: %s", out)
	}
	if out, err := exec.Command("git", "-C", remoteGit, "rev-parse", "refs/heads/"+branch).CombinedOutput(); err == nil {
		t.Fatalf("with HEAD moved off the pinned commit, the delivery branch must not exist on the remote, but it is at %s", strings.TrimSpace(string(out)))
	}
	// Restore HEAD to the pinned commit (the normal state: import-agent left
	// it here) for the successful run below.
	mustGit(t, scratch, "checkout", "-q", "--detach", verifySHA)

	// Run 2 (success): the full end-to-end push with HEAD == the pinned
	// commit.
	if out, err := exec.Command("sh", pushPath).CombinedOutput(); err != nil {
		t.Fatalf("push script failed (the kind-run failure mode); want 0. err: %v, output: %s", err, string(out))
	}

	// 1. The remote's delivery branch must be the pinned verifiedCommit
	// (the push pushed the verified commit, not a claim's head).
	branchRef := gitRemoteRef(t, remoteGit, "refs/heads/"+branch)
	if branchRef != verifySHA {
		t.Fatalf("remote branch %s is at %s; want the pinned verified commit %s", branch, branchRef, verifySHA)
	}

	// 2. The API saw EXACTLY one POST (the PR create), and it carried the
	// credential's Basic auth header (built from the mounted files:
	// samples:s3cret-pw, base64 -w 0).
	if len(*posts) != 1 {
		t.Fatalf("the PR create POST was seen %d times; want exactly 1 (the GET is a separate lookup)", len(*posts))
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("samples:s3cret-pw"))
	if (*posts)[0].auth != wantAuth {
		t.Fatalf("the PR create POST carried auth %q; want %q (the credential's Basic header)", (*posts)[0].auth, wantAuth)
	}

	// 3. The termination message must parse STRICTLY (the operator's trust
	// boundary): branch == the delivery branch, commit == the pinned commit,
	// prNumber == 42 (the fake API's draft PR number), prURL on the repo host.
	termBytes, err := os.ReadFile(termFile)
	if err != nil {
		t.Fatalf("read the termination log: %v", err)
	}
	outcome, ok := parseDeliverTermination(string(termBytes), loop)
	if !ok {
		t.Fatalf("the termination message did not parse (the operator would reject it): %q", string(termBytes))
	}
	if outcome.Commit != verifySHA {
		t.Fatalf("termination commit %s; want the pinned verified commit %s", outcome.Commit, verifySHA)
	}
	if outcome.Branch != branch {
		t.Fatalf("termination branch %s; want the delivery branch %s", outcome.Branch, branch)
	}
	if outcome.PRNumber != 42 {
		t.Fatalf("termination prNumber %d; want 42 (the fake API's draft PR number)", outcome.PRNumber)
	}
	// prURL is the provider's own html_url (the script no longer assembles it):
	// the PR page of THIS repo (samples/gocli), not a base that drops the
	// repo name (the old deliverPRURLBase produced .../samples/pulls/42 —
	// the strict parser rejects that path now, which is the regression this
	// check guards).
	wantPRURL := "http://gitea.example:3000/samples/gocli/pulls/42"
	if outcome.PRURL != wantPRURL {
		t.Fatalf("termination prURL %s; want the provider html_url %s (the PR page of THIS repo)", outcome.PRURL, wantPRURL)
	}
}

// TestDeliverTerminationPRURLStrict (S6 review P1) pins the strict
// prURL validation the operator applies to the push container's html_url:
// an EXACT per-provider path match for the repo spec.workspace.repo names.
// The two "real" URLs (the kind Gitea repo and a GitHub repo) FAIL on the
// old code: the old validator's /pulls/<n> suffix accepted the GitHub
// /pull/<n> shape, and the old push-script PR_BASE builder dropped the repo
// name (deliverPRURLBase -> .../samples/pulls/1, the kind-recorded prURL).
func TestDeliverTerminationPRURLStrict(t *testing.T) {
	// The kind Gitea repo (the real kind-run URL).
	giteaLoop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "urltask1"},
		Spec: coxv1alpha1.LoopSpec{
			Workspace: coxv1alpha1.Workspace{Repo: "http://gitea.samples.svc:3000/samples/gocli.git"},
			Delivery:  &coxv1alpha1.DeliveryConfig{Mode: coxv1alpha1.DeliveryModePullRequest},
		},
		Status: coxv1alpha1.LoopStatus{CurrentVerify: &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: "1111111111111111111111111111111111111111"}},
	}
	branch := deliverBranchName(deliverBranchPrefix(giteaLoop), giteaLoop.Name)

	t.Run("Gitea: the kind repo's own PR page is accepted", func(t *testing.T) {
		msg := "branch=" + branch + "\ncommit=1111111111111111111111111111111111111111\nprNumber=1\nprURL=http://gitea.samples.svc:3000/samples/gocli/pulls/1\n"
		if _, ok := parseDeliverTermination(msg, giteaLoop); !ok {
			t.Fatal("the kind Gitea repo's own PR page URL (its html_url) must be accepted")
		}
	})

	t.Run("Gitea: the kind-recorded wrong-repo URL is rejected", func(t *testing.T) {
		// The s6e kind run recorded this (deliverPRURLBase dropped the repo
		// name). It names a different repo: rejected.
		msg := "branch=" + branch + "\ncommit=1111111111111111111111111111111111111111\nprNumber=1\nprURL=http://gitea.samples.svc:3000/samples/pulls/1\n"
		if _, ok := parseDeliverTermination(msg, giteaLoop); ok {
			t.Fatal("a prURL that drops the repo name (.../samples/pulls/1) must be rejected (it names a different repo)")
		}
	})

	// A GitHub delivery.
	ghLoop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "urltask1"},
		Spec: coxv1alpha1.LoopSpec{
			Workspace: coxv1alpha1.Workspace{Repo: "https://github.com/samples/gocli.git"},
			Delivery:  &coxv1alpha1.DeliveryConfig{Mode: coxv1alpha1.DeliveryModePullRequest},
		},
		Status: coxv1alpha1.LoopStatus{CurrentVerify: &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: "1111111111111111111111111111111111111111"}},
	}

	t.Run("GitHub: the /pull/<n> page is accepted", func(t *testing.T) {
		msg := "branch=" + branch + "\ncommit=1111111111111111111111111111111111111111\nprNumber=7\nprURL=https://github.com/samples/gocli/pull/7\n"
		if _, ok := parseDeliverTermination(msg, ghLoop); !ok {
			t.Fatal("GitHub's /pull/<n> PR page (its html_url) must be accepted")
		}
	})

	t.Run("GitHub: the /pulls/<n> path is rejected (wrong segment)", func(t *testing.T) {
		msg := "branch=" + branch + "\ncommit=1111111111111111111111111111111111111111\nprNumber=7\nprURL=https://github.com/samples/gocli/pulls/7\n"
		if _, ok := parseDeliverTermination(msg, ghLoop); ok {
			t.Fatal("a github.com prURL with /pulls/<n> must be rejected (GitHub's PR page is /pull/<n>)")
		}
	})

	t.Run("Gitea: a path that names a different repo is rejected", func(t *testing.T) {
		msg := "branch=" + branch + "\ncommit=1111111111111111111111111111111111111111\nprNumber=1\nprURL=http://gitea.samples.svc:3000/samples/other-repo/pulls/1\n"
		if _, ok := parseDeliverTermination(msg, giteaLoop); ok {
			t.Fatal("a prURL naming a different repo than spec.workspace.repo must be rejected")
		}
	})
}

// fakeGiteaAPICall records one PR-create POST the fake Gitea API saw.
type fakeGiteaAPICall struct {
	method string
	auth   string
}

// newFakeGiteaAPI stands up a Gitea-compatible provider API: GET /repos/{o}/{r}
// (default branch lookup) -> {"default_branch": ...}; GET /pulls
// (idempotency lookup) -> the single open PR when one exists (the script
// reuses it — idempotent), else []; POST /pulls (create) -> a draft PR with
// number 42. Every response carries the provider's OWN html_url (the
// PR page: <host>/<owner>/<repo>/pulls/<n>) — the push script records it as
// prURL and the operator's strict parser validates it exactly. It returns
// the API base (rewritten into the script's API_BASE) and the recorded
// PR-create POSTs. The test inspects `posts` AFTER the script runs, so it
// is returned by pointer.
func newFakeGiteaAPI(t *testing.T, owner, repoName, scheme, host, defaultBranch string) (apiBase string, posts *[]fakeGiteaAPICall) {
	t.Helper()
	var mu sync.Mutex
	var postList []fakeGiteaAPICall
	var openPR int // 0 = no open PR yet
	repoPath := "/repos/" + owner + "/" + repoName
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// GET /repos/{owner}/{repo} — the default-branch lookup.
		if r.Method == http.MethodGet && path == repoPath {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"name":"%s","default_branch":"%s"}`, repoName, defaultBranch)
			return
		}
		// GET /repos/{owner}/{repo}/pulls — the idempotency lookup.
		if r.Method == http.MethodGet && strings.HasSuffix(path, "/pulls") {
			mu.Lock()
			n := openPR
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if n == 0 {
				_, _ = w.Write([]byte("[]"))
				return
			}
			_, _ = fmt.Fprintf(w, `[{"number":%d,"state":"open","draft":true,"html_url":"%s://%s/%s/%s/pulls/%d"}]`,
				n, scheme, host, owner, repoName, n)
			return
		}
		// POST /repos/{owner}/{repo}/pulls — the create.
		if r.Method == http.MethodPost && strings.HasSuffix(path, "/pulls") {
			mu.Lock()
			postList = append(postList, fakeGiteaAPICall{method: r.Method, auth: r.Header.Get("Authorization")})
			openPR = 42
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"number":42,"state":"open","draft":true,"html_url":"%s://%s/%s/%s/pulls/42"}`,
				scheme, host, owner, repoName)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	apiBase = ts.URL
	return apiBase, &postList
}

// gitRemoteRef returns the commit a ref points at in a repo (a bare repo's
// ref points directly at the commit). Fails the test on a git error.
func gitRemoteRef(t *testing.T, repo, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", ref+"^{commit}").Output()
	if err != nil {
		t.Fatalf("git rev-parse %s in %s: %v", ref, repo, err)
	}
	return strings.TrimSpace(string(out))
}

// TestDeliverPushScriptRefusesDefaultBranch (S6 review P2: refuse the repo's
// actual default branch) runs the REAL push script end-to-end with the fake
// Gitea API reporting default_branch=trunk and BRANCH=trunk: the script must
// exit non-zero (the default-branch refusal fires) and NO ref must be created
// on the bare remote. The script's main/master check is belt-and-suspenders;
// this case exercises the API read of default_branch (a branch name that is
// neither main nor master).
func TestDeliverPushScriptRefusesDefaultBranch(t *testing.T) {
	r := &LoopReconciler{}

	// The remote (the deliver Job's origin stand-in): a local bare repo with
	// a base branch (initial — NOT main/master, so the script's main/master
	// check does not fire; the default-branch refusal must catch trunk).
	remoteDir := t.TempDir()
	remoteGit := filepath.Join(remoteDir, "remote.git")
	mustGit(t, remoteDir, "init", "-q", "-b", "initial", "--bare", remoteGit)

	// The agent repo: a base commit (the remote's initial branch) + one agent
	// commit (the pinned verifiedCommit).
	agentRepo := t.TempDir()
	mustGit(t, agentRepo, "init", "-q", "-b", "initial")
	mustGit(t, agentRepo, "config", "user.email", "agent@coxswain.test")
	mustGit(t, agentRepo, "config", "user.name", "Agent")
	writeFile(t, agentRepo, "round.go", "package main\n")
	mustGit(t, agentRepo, "add", "-A")
	mustGit(t, agentRepo, "commit", "-q", "-m", "base")
	baseSHA := gitSHA(t, agentRepo)
	mustGit(t, agentRepo, "remote", "add", "origin", remoteGit)
	mustGit(t, agentRepo, "push", "-q", "origin", "initial")
	writeFile(t, agentRepo, "round.go", "package main\nfunc F() int { return 1 }\n")
	mustGit(t, agentRepo, "add", "-A")
	mustGit(t, agentRepo, "commit", "-q", "-m", "agent: change")
	verifySHA := gitSHA(t, agentRepo)
	if verifySHA == baseSHA {
		t.Fatal("the agent commit must differ from the base commit")
	}

	// The Loop: a Gitea-compatible repo. The delivery branch is passed
	// explicitly to deliverPushContainer ("trunk" — the repo's actual default
	// branch; the script must refuse it via the API read, not the
	// main/master check).
	const loopName = "trunktask1"
	repoURL := "http://gitea.example:3000/samples/gocli.git"
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: loopName},
		Spec: coxv1alpha1.LoopSpec{
			Workspace: coxv1alpha1.Workspace{
				Repo:                repoURL,
				Ref:                 "initial",
				GitCredentialSecret: testCredSecretName,
			},
			Delivery: &coxv1alpha1.DeliveryConfig{Mode: coxv1alpha1.DeliveryModePullRequest},
		},
		Status: coxv1alpha1.LoopStatus{
			CurrentVerify: &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: verifySHA},
		},
	}
	// The delivery branch is 'trunk' (passed explicitly — the repo's actual
	// default branch, which is neither main nor master, so the script's
	// main/master check does not fire; the API read of default_branch must
	// catch it).
	branch := "trunk"

	// The fake Gitea API: default_branch=trunk (the delivery branch IS the
	// default branch — the script must refuse). The repo is samples/gocli.
	apiBase, _ := newFakeGiteaAPI(t, "samples", "gocli", "http", "gitea.example:3000", "trunk")

	// The credential files.
	credsDir := t.TempDir()
	mustWriteFile(t, filepath.Join(credsDir, workspaceCredsUsernameKey), []byte("samples"))
	mustWriteFile(t, filepath.Join(credsDir, workspaceCredsPasswordKey), []byte("s3cret-pw"))

	// The scratch dir: set up as clone-base + import-agent would.
	scratch := t.TempDir()
	mustGit(t, scratch, "clone", "-q", remoteGit, ".")
	mustGit(t, scratch, "-c", "protocol.file.allow=always", "-c", "core.hooksPath=/dev/null",
		"fetch", "file://"+agentRepo+"/.git", verifySHA)
	mustGit(t, scratch, "checkout", "-q", "--detach", verifySHA)
	mustGit(t, scratch, "remote", "set-url", "origin", remoteGit)

	// Extract the REAL push script and rewrite the pod paths.
	// Note: the script's BASE is the loop's workspace ref (\"initial\"), but
	// the delivery branch is 'trunk'. The script's BASE refusal check
	// (BRANCH != BASE) passes because trunk != initial. The default-branch
	// refusal (BRANCH == default_branch) must fire.
	pushScript := deliverContainerScript(t, r.deliverPushContainer(loop, verifySHA, branch, "initial"))
	termFile := filepath.Join(t.TempDir(), "termination-log")
	pushScript = strings.ReplaceAll(pushScript, deliverScratchPath, scratch)
	pushScript = strings.ReplaceAll(pushScript, "/workspace-creds", credsDir)
	pushScript = strings.ReplaceAll(pushScript, "/dev/termination-log", termFile)
	wantAPIBase := "'" + deliverAPIBase(repoURL, deliverProviderGitea) + "'"
	if !strings.Contains(pushScript, wantAPIBase) {
		t.Fatalf("push script does not carry the expected API_BASE %q", wantAPIBase)
	}
	pushScript = strings.ReplaceAll(pushScript, wantAPIBase, "'"+apiBase+"'")

	pushPath := filepath.Join(t.TempDir(), "push.sh")
	mustWriteFile(t, pushPath, []byte(pushScript))

	// Run: the script must refuse (BRANCH=trunk == default_branch=trunk).
	// The script's main/master check does NOT fire (trunk is neither main nor
	// master); only the API read of default_branch catches it.
	out, err := exec.Command("sh", pushPath).CombinedOutput()
	if err == nil {
		t.Fatalf("the push script must refuse (exit non-zero) when BRANCH equals the repo's default branch (trunk); it exited 0. Output: %s", string(out))
	}
	// No ref on the bare remote.
	if out2, err := exec.Command("git", "-C", remoteGit, "rev-parse", "refs/heads/trunk").CombinedOutput(); err == nil {
		t.Fatalf("the delivery branch must not exist on the remote after a default-branch refusal; it is at %s", strings.TrimSpace(string(out2)))
	}
}
