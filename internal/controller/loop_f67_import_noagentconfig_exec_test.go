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

// F67 (P2, sec): the verify and deliver Jobs' `import-agent` init containers
// must not start a git SERVER process inside the agent's repository.
//
// `git fetch file:///agent-src/.git <sha>` runs `git upload-pack` with the
// agent's repo as its repository, so git obeys THAT repository's config — a
// file the agent owns. The exec-capable keys are uploadpack.packObjectsHook
// (git runs it INSTEAD of pack-objects), core.fsmonitor (a command) and
// core.alternateRefsCommand (a shell command). What stops the first one today
// is a rule of the git version in the Job image — git honours it only from
// PROTECTED config (git-config(5), "SCOPES"), never from a fetched repo's
// config — and the image is operator-configurable (--workspace-git-image), so
// the protection is a property of the image, not of our code. F67 replaces the
// fetch with a read of the agent's object store through
// GIT_ALTERNATE_OBJECT_DIRECTORIES (a path: no config is read from it) plus
// `pack-objects` into the job's OWN store, which the later containers (verify)
// and the push container (deliver) need anyway.
//
// These tests execute the REAL generated scripts (the AGENTS.md exec-test rule)
// against a source repo whose config points every exec-capable key at a
// marker-writing program. Two independent detectors:
//   - the marker file (an agent-chosen program actually ran), with a positive
//     control that the marker mechanism is live;
//   - GIT_TRACE output: a fetch must show `git-upload-pack` being started, so
//     the trace is the behavioural gate against reinstating the fetch (the
//     marker alone cannot be: git refuses packObjectsHook from unprotected
//     config, so even a restored fetch runs no program on a current git).
package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// f67HostileConfig points every exec-capable key a repository OWNER controls at
// a program that appends to the marker file. A git command that obeys this
// repo's config runs one of them and the marker appears.
func f67HostileConfig(t *testing.T, repo, marker string) {
	t.Helper()
	evil := filepath.Join(repo, "evil.sh")
	body := "#!/bin/sh\necho ran >> " + marker + "\ncat >/dev/null 2>/dev/null || true\n"
	if err := os.WriteFile(evil, []byte(body), 0o755); err != nil {
		t.Fatalf("write the hostile program: %v", err)
	}
	f43Git(t, repo, "config", "uploadpack.packObjectsHook", evil)
	f43Git(t, repo, "config", "core.fsmonitor", evil)
	f43Git(t, repo, "config", "core.alternateRefsCommand", evil)
}

// f67MarkerPresent reports whether any hostile program ran.
func f67MarkerPresent(t *testing.T, marker string) bool {
	t.Helper()
	_, err := os.Stat(marker)
	if err == nil {
		return true
	}
	if !os.IsNotExist(err) {
		t.Fatalf("stat marker: %v", err)
	}
	return false
}

// f67RunScript executes a generated script with sh, a clean git config, GIT_TRACE
// on (so the trace shows whether `git-upload-pack` was started) and the given
// extra env.
func f67RunScript(t *testing.T, script string, extraEnv ...string) (int, string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TRACE=1")
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("run script: %v", err)
	return -1, ""
}

// f67AssertNoServerProcess fails if the traced output shows git starting
// upload-pack, i.e. if git ran AS A SERVER in the agent's repo.
func f67AssertNoServerProcess(t *testing.T, out string) {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "git-upload-pack") {
			t.Fatalf("git started upload-pack (a server process) in the agent's repo; F67 removed that path: %s", line)
		}
	}
}

// f67GitOut runs a git command in dir with a clean config and returns the
// trimmed stdout.
func f67GitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// TestF67ImportAgentRunsNoAgentRepoProgram runs the verify Job's REAL
// import-agent script against a source repo whose config is hostile, and
// asserts the import succeeds without running any of those programs and
// without starting a git server in that repo.
func TestF67ImportAgentRunsNoAgentRepoProgram(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")

	agent := t.TempDir()
	f43Git(t, agent, "init", "-q", "-b", "main")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(agent, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "base\n")
	f43Git(t, agent, "add", "a.txt")
	f43Git(t, agent, "commit", "-q", "-m", "base")
	base := f43Git(t, agent, "rev-parse", "HEAD")
	write("a.txt", "work\n")
	f43Git(t, agent, "commit", "-q", "-am", "work")
	pin := f43Git(t, agent, "rev-parse", "HEAD")

	// The /verify stand-in: clone-base's shape (a shallow fetch of the base
	// from origin) BEFORE the hostile config exists — clone-base fetches from
	// the trusted origin, never from the agent's repo, so it is not the
	// subject of this test.
	v := t.TempDir()
	f43Git(t, v, "init", "-q")
	f43Git(t, v, "-c", "protocol.file.allow=always", "fetch", "-q", "--no-tags", "file://"+agent+"/.git", base)
	f43Git(t, v, "checkout", "-q", "--detach", base)

	f67HostileConfig(t, agent, marker)

	// Positive control 1: a git command that DOES read the source repo's config
	// runs one of the hostile programs (core.fsmonitor is consulted by status).
	f43Git(t, agent, "status", "--porcelain")
	if !f67MarkerPresent(t, marker) {
		t.Fatalf("positive control: the hostile repo config did not run any program for a local git command, so the marker check below proves nothing")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("clear the marker: %v", err)
	}

	// Positive control 2: the trace detector matches a git command that DOES
	// start a server in the source repo.
	control := t.TempDir()
	f43Git(t, control, "init", "-q")
	f43Git(t, control, "-c", "protocol.file.allow=always", "fetch", "-q", "--no-tags", "file://"+agent+"/.git", base)
	if out := f67GitTrace(t, control, "-c", "protocol.file.allow=always", "fetch", "--no-tags", "file://"+agent+"/.git", pin); !strings.Contains(out, "git-upload-pack") {
		t.Fatalf("positive control: the trace detector does not see git-upload-pack for a fetch from the agent's repo, so the assertion below proves nothing:\n%s", out)
	}

	// The positive controls above may have run a hostile program; start clean.
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clear the marker: %v", err)
	}

	script := importAgentScript(base, pin)
	script = strings.ReplaceAll(script, "cd /verify", "cd "+v)
	script = strings.ReplaceAll(script, "/agent-src/.git/objects", agent+"/.git/objects")
	code, out := f67RunScript(t, script)
	if code != 0 {
		t.Fatalf("the real import-agent script failed (exit %d) against a repo with a hostile config; want 0\n%s", code, out)
	}
	if head := f43Git(t, v, "rev-parse", "HEAD"); head != pin {
		t.Fatalf("import-agent checked out %s; want the pinned commit %s", head, pin)
	}
	if f67MarkerPresent(t, marker) {
		t.Fatalf("the agent's repo config ran a program during import: the import must not run a git server (or read the config) in the agent's repo\nscript output:\n%s", out)
	}
	f67AssertNoServerProcess(t, out)
	// The pin's objects must be LOCAL afterwards: the alternate is an env var
	// that dies with the container while tamper/check read /verify/.git.
	cat := exec.Command("git", "-C", v, "cat-file", "-e", pin+"^{commit}")
	cat.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cat.CombinedOutput(); err != nil {
		t.Fatalf("the pinned commit is not in /verify's own object store after import (a later container cannot read it): %v\n%s", err, out)
	}
}

// f67GitTrace runs one git command in dir with GIT_TRACE on and returns the
// combined output (the positive control for the trace detector).
func f67GitTrace(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TRACE=1")
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// TestF67DeliverImportRunsNoAgentRepoProgram runs the deliver Job's REAL
// clone-base + import-agent scripts in sequence against a hostile source repo,
// then pushes from the scratch clone with NO access to the agent's repo (the
// push container does not mount the agent PVC). The push succeeding proves the
// pinned objects were copied into the scratch store, not merely referenced.
func TestF67DeliverImportRunsNoAgentRepoProgram(t *testing.T) {
	r := &LoopReconciler{CNIProber: testCNIProber()}
	marker := filepath.Join(t.TempDir(), "ran")

	agentRepo := t.TempDir()
	mustGit(t, agentRepo, "init", "-q", "-b", "main")
	mustGit(t, agentRepo, "config", "user.email", "agent@coxswain.test")
	mustGit(t, agentRepo, "config", "user.name", "Agent")
	writeFile(t, agentRepo, "round.go", "package main\n\nfunc Round(x float64) int { return int(x) }\n")
	mustGit(t, agentRepo, "add", "-A")
	mustGit(t, agentRepo, "commit", "-q", "-m", "base")
	baseSHA := f67GitOut(t, agentRepo, "rev-parse", "HEAD")
	writeFile(t, agentRepo, "round.go", "package main\n\nfunc Round(x float64) int { return int(x + 0.5) }\n")
	mustGit(t, agentRepo, "add", "-A")
	mustGit(t, agentRepo, "commit", "-q", "-m", "agent: fix rounding")
	verifySHA := gitSHA(t, agentRepo)
	if baseSHA == verifySHA {
		t.Fatal("the agent commit must differ from the base commit")
	}

	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "f67deliver", Namespace: "f67-ns", UID: "f67-uid"},
		Spec: coxv1alpha1.LoopSpec{
			Workspace: coxv1alpha1.Workspace{
				Repo:                agentRepo + "/.git",
				Ref:                 "main",
				GitCredentialSecret: testCredSecretName,
			},
		},
		Status: coxv1alpha1.LoopStatus{
			CurrentVerify: &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: verifySHA},
		},
	}

	scratch := t.TempDir()
	cloneScript := deliverContainerScript(t, r.deliverCloneBaseContainer(loop))
	cloneScript = rewriteDeliverScriptPaths(cloneScript, scratch, agentRepo)
	importScript := deliverContainerScript(t, r.deliverImportAgentContainer(loop))
	importScript = rewriteDeliverScriptPaths(importScript, scratch, agentRepo)
	clonePath := filepath.Join(scratch, "clone-base.sh")
	importPath := filepath.Join(scratch, "import-agent.sh")
	mustWriteFile(t, clonePath, []byte(cloneScript))
	mustWriteFile(t, importPath, []byte(importScript))

	if out, err := exec.Command("bash", clonePath).CombinedOutput(); err != nil {
		t.Fatalf("clone-base script failed; want 0. err: %v, output: %s", err, out)
	}

	// Hostile config AFTER clone-base (which fetches from the trusted origin,
	// here the same test repo) and before import-agent, which is the subject.
	f67HostileConfig(t, agentRepo, marker)

	code, out := f67RunScript(t, importScript)
	if code != 0 {
		t.Fatalf("the real deliver import-agent script failed against a repo with a hostile config; want 0\nerr: exit %d\n%s", code, out)
	}
	if head := gitSHA(t, scratch); head != verifySHA {
		t.Fatalf("import-agent left HEAD at %s; want the pinned verified commit %s", head, verifySHA)
	}
	if f67MarkerPresent(t, marker) {
		t.Fatalf("the agent's repo config ran a program during the deliver import: it must not run a git server (or read the config) in the agent's repo\nscript output:\n%s", out)
	}
	f67AssertNoServerProcess(t, out)

	// The push container does not mount the agent PVC: push from the scratch
	// clone with no access to the agent's object store. This only works if the
	// pinned commit's objects were COPIED into the scratch store.
	bare := t.TempDir()
	mustGit(t, bare, "init", "-q", "--bare")
	push := exec.Command("git", "-C", scratch, "-c", "protocol.file.allow=always", "push", bare, "HEAD:refs/heads/coxswain/task")
	push.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := push.CombinedOutput(); err != nil {
		t.Fatalf("the push after import failed (the pinned objects were not copied into the scratch store): %v\n%s", err, out)
	}
	pushed := f67GitOut(t, bare, "rev-parse", "refs/heads/coxswain/task")
	if pushed != verifySHA {
		t.Fatalf("the pushed branch is at %s; want the pinned commit %s", pushed, verifySHA)
	}
}
