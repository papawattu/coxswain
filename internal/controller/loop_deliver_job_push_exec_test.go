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
	apiBase, posts := newFakeGiteaAPI(t)

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
	// REPO (the Gitea URL) is left as-is: it drives owner/name + the PR URL
	// base (the termination message's prURL must parse against the repo
	// host), and the git push uses the origin remote, not $REPO.
	pushScript := deliverContainerScript(t, r.deliverPushContainer(loop, verifySHA, branch, baseBranch))
	termFile := filepath.Join(t.TempDir(), "termination-log")
	pushScript = strings.ReplaceAll(pushScript, deliverScratchPath, scratch)
	pushScript = strings.ReplaceAll(pushScript, "/workspace-creds", credsDir)
	pushScript = strings.ReplaceAll(pushScript, "/dev/termination-log", termFile)
	// Override ONLY the API base (the curl calls). PR_BASE stays the Gitea
	// PR URL so the termination message's prURL parses against the repo host.
	wantAPIBase := "'" + deliverAPIBase(repoURL, deliverProviderGitea) + "'"
	if !strings.Contains(pushScript, wantAPIBase) {
		t.Fatalf("push script does not carry the expected API_BASE %q (did the shape change?)", wantAPIBase)
	}
	pushScript = strings.ReplaceAll(pushScript, wantAPIBase, "'"+apiBase+"'")

	pushPath := filepath.Join(t.TempDir(), "push.sh")
	mustWriteFile(t, pushPath, []byte(pushScript))

	// Run the push script with sh (dash — POSIX-sh; the container's busybox
	// ash is the same dialect). A non-zero exit is a failure.
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
	if !strings.Contains(outcome.PRURL, "/pulls/42") {
		t.Fatalf("termination prURL %s; want a /pulls/42 path on the repo host", outcome.PRURL)
	}
}

// fakeGiteaAPICall records one PR-create POST the fake Gitea API saw.
type fakeGiteaAPICall struct {
	method string
	auth   string
}

// newFakeGiteaAPI stands up a Gitea-compatible provider API: GET /pulls
// (idempotency lookup) -> [] (no open PR yet, so the create path runs); POST
// /pulls (create) -> a draft PR with number 42. It returns the API base
// (rewritten into the script's API_BASE) and the recorded PR-create POSTs.
// The test inspects `posts` AFTER the script runs, so it is returned by
// pointer.
func newFakeGiteaAPI(t *testing.T) (apiBase string, posts *[]fakeGiteaAPICall) {
	t.Helper()
	var mu sync.Mutex
	var postList []fakeGiteaAPICall
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// No open PR yet: the create path runs.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
		case http.MethodPost:
			mu.Lock()
			postList = append(postList, fakeGiteaAPICall{method: r.Method, auth: r.Header.Get("Authorization")})
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"number":42,"state":"open","draft":true,"url":"http://gitea.example:3000/samples/gocli/pulls/42"}`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	apiBase = ts.URL + "/api/v1/repos"
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
