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
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// f43Git runs git in dir with a clean config and returns trimmed output.
func f43Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// f43Run executes the REAL generated import-agent script with only the path
// constants substituted (/verify -> verifyDir, /agent-src/.git/objects -> the
// agent repo's object store). F67 changed the import from a fetch to an
// alternate-object-store copy, so the substituted constant is the object path
// the script reads, not a file:// URL.
func f43Run(t *testing.T, verifyDir, agentDir, base, pin string) (int, string) {
	t.Helper()
	script := importAgentScript(base, pin)
	script = strings.ReplaceAll(script, "cd /verify", "cd "+verifyDir)
	script = strings.ReplaceAll(script, "/agent-src/.git/objects", agentDir+"/.git/objects")
	cmd := exec.Command("sh", "-c", script)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	err := cmd.Run()
	if err == nil {
		return 0, out.String()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), out.String()
	}
	t.Fatalf("run: %v", err)
	return -1, ""
}

// TestImportAgentScriptRefusesNonDescendantPin (F43): the real import-agent
// script accepts a descendant of baseCommit (and baseCommit itself), refuses a
// pin on unrelated history with verifyNotDescendantExit, and a git failure (an
// unknown pin) is a different non-zero exit (an infra failure, not 97).
func TestImportAgentScriptRefusesNonDescendantPin(t *testing.T) {
	agent := t.TempDir()
	f43Git(t, agent, "init", "-q")
	write := func(name, body string) {
		if err := os.WriteFile(agent+"/"+name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "base\n")
	f43Git(t, agent, "add", "a.txt")
	f43Git(t, agent, "commit", "-q", "-m", "base")
	base := f43Git(t, agent, "rev-parse", "HEAD")
	write("a.txt", "work\n")
	f43Git(t, agent, "commit", "-q", "-am", "work")
	descendant := f43Git(t, agent, "rev-parse", "HEAD")
	// An unrelated root commit in the same repo (pre-existing / forged history).
	f43Git(t, agent, "checkout", "-q", "--orphan", "other")
	write("b.txt", "other\n")
	f43Git(t, agent, "add", "b.txt")
	f43Git(t, agent, "commit", "-q", "-m", "orphan")
	orphan := f43Git(t, agent, "rev-parse", "HEAD")

	newVerify := func() string {
		v := t.TempDir()
		f43Git(t, v, "init", "-q")
		f43Git(t, v, "-c", "protocol.file.allow=always", "fetch", "-q", "--no-tags", "file://"+agent+"/.git", base)
		f43Git(t, v, "checkout", "-q", "--detach", base)
		return v
	}
	cases := []struct {
		name string
		pin  string
		want func(int) bool
		desc string
	}{
		{"descendant", descendant, func(c int) bool { return c == 0 }, "exit 0"},
		{"the base itself", base, func(c int) bool { return c == 0 }, "exit 0"},
		{"unrelated root commit", orphan, func(c int) bool { return c == verifyNotDescendantExit }, "exit 97"},
		{"unknown pin (git fetch fails)", strings.Repeat("0", 40), func(c int) bool { return c != 0 && c != verifyNotDescendantExit }, "non-zero, not 97"},
	}
	for _, tc := range cases {
		v := newVerify()
		code, out := f43Run(t, v, agent, base, tc.pin)
		if !tc.want(code) {
			t.Errorf("%s: exit=%d, want %s\n%s", tc.name, code, tc.desc, out)
		}
		switch code {
		case 0:
			if head := f43Git(t, v, "rev-parse", "HEAD"); head != tc.pin {
				t.Errorf("%s: checked out %s, want %s", tc.name, head, tc.pin)
			}
		case verifyNotDescendantExit:
			if head := f43Git(t, v, "rev-parse", "HEAD"); head != base {
				t.Errorf("%s: a refused pin was checked out (HEAD %s)", tc.name, head)
			}
		}
	}
}

// TestVerifyOutcomeImportAgentExits (F43): import-agent exit 97 is the
// terminal not-descendant outcome; any other non-zero stays an infra failure.
func TestVerifyOutcomeImportAgentExits(t *testing.T) {
	pod := func(code int32) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
			{Name: verifyCloneBaseInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}},
			{Name: verifyImportAgentInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}}},
		}}}
	}
	for _, tc := range []struct {
		name string
		code int32
		want int
	}{
		{"97 -> not descendant", verifyNotDescendantExit, verifyNotDescendant},
		{"2 (git error) -> infra", 2, verifyInfraFailed},
		{"128 (fetch failed) -> infra", 128, verifyInfraFailed},
	} {
		got, _, name, _ := verifyOutcome(pod(tc.code), 1, false, false)
		if got != tc.want || name != verifyImportAgentInit {
			t.Errorf("%s: verifyOutcome = %d (%q), want %d (%q)", tc.name, got, name, tc.want, verifyImportAgentInit)
		}
	}
	// The clone-base init exiting 97 is NOT a not-descendant (only import-agent).
	p := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
		{Name: verifyCloneBaseInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: verifyNotDescendantExit}}},
	}}}
	if got, _, _, _ := verifyOutcome(p, 1, false, false); got != verifyInfraFailed {
		t.Errorf("clone-base exit 97: verifyOutcome = %d, want infra (%d)", got, verifyInfraFailed)
	}
}
