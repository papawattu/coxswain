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

package cli

import (
	"bytes"
	"strings"
	"testing"
)

// P6b-2: the operator log mixes zap lines, non-JSON lines and audit records;
// only this Loop's records shape the candidate.
func TestSuggestPolicyFromOperatorLog(t *testing.T) {
	in := strings.Join([]string{
		`{"level":"info","ts":1,"msg":"Reconciling Loop","loop":"l1"}`,
		`not json at all`,
		`{"time":"2026-11-06T10:00:00Z","loop":"l1","namespace":"ns","iteration":"0","source":"kubearmor","action":"Audit","target":"/usr/bin/git","verdict":"allowed","detail":"operation=Process, pod=l1-sandbox, policy=p"}`,
		`{"time":"2026-11-06T10:00:01Z","loop":"l1","namespace":"ns","iteration":"0","source":"egress-proxy","action":"connect","target":"github.com:443","verdict":"blocked","detail":""}`,
		`{"time":"2026-11-06T10:00:02Z","loop":"l2","namespace":"ns","iteration":"0","source":"kubearmor","action":"Audit","target":"/bin/sh","verdict":"allowed","detail":"operation=Process"}`,
	}, "\n")
	var out bytes.Buffer
	if err := suggestPolicy(strings.NewReader(in), &out, "l1", "ns", "l1-suggested"); err != nil {
		t.Fatalf("suggest: %v", err)
	}
	got := out.String()
	for _, want := range []string{"name: l1-suggested", "namespace: ns", `- "/usr/bin/git"`, `- "github.com:443"`, "Platform minimum"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "/bin/sh") {
		t.Errorf("another Loop's activity leaked into the candidate:\n%s", got)
	}
}

// R52 finding 11-09: with -f the command reads only a local file, so a bad
// kubeconfig must not be loaded and its error must not mask suggest's own
// error (the "open" error for a missing input file).
func TestPolicySuggestOfflineShowsItsOwnError(t *testing.T) {
	oldDeps := currentDeps
	currentDeps = Deps{}
	t.Cleanup(func() { currentDeps = oldDeps })

	var out, errBuf bytes.Buffer
	root := NewRootCommand(Deps{})
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"policy", "suggest", "l1", "--namespace", "ns", "--kubeconfig", "/nonexistent/kubeconfig", "-f", "/nonexistent/input"})
	runErr := root.Execute()
	if runErr == nil {
		t.Fatal("Execute() = nil, want an error")
	}
	errOut := runErr.Error() + "\n" + errBuf.String() + "\n" + out.String()
	for _, want := range []string{"open /nonexistent/input", "no such file or directory"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("error output lacks %q:\n%s", want, errOut)
		}
	}
	for _, masked := range []string{"invalid configuration", "/nonexistent/kubeconfig"} {
		if strings.Contains(errOut, masked) {
			t.Errorf("kubeconfig error leaked into the output:\n%s", errOut)
		}
	}
}

// P6b-2: the command is registered as `policy suggest`.
func TestPolicySuggestRegistered(t *testing.T) {
	root := NewRootCommand(Deps{})
	cmd, _, err := root.Find([]string{"policy", "suggest"})
	if err != nil || cmd == nil || cmd.Name() != "suggest" {
		t.Fatalf("policy suggest not registered: %v", err)
	}
}
