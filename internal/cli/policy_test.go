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
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	// The persistent flags bind into the package vars (Kubeconfig/Context), so
	// restore them: they leak into kubectl argv (and the client config) in
	// other tests in this package.
	oldKube, oldCtx := Kubeconfig, Context
	t.Cleanup(func() { Kubeconfig, Context = oldKube, oldCtx })
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

	// An input that holds only another Loop's audit lines must fail with
	// suggest's own "no audit records for Loop" error, not the kubeconfig's.
	other := `{"time":"2026-11-06T10:00:00Z","loop":"other","namespace":"ns","iteration":"0","source":"kubearmor","action":"Audit","target":"/bin/sh","verdict":"allowed","detail":"operation=Process, pod=other-sandbox, policy=p"}`
	f := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(f, []byte(other+"\n"), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	var out2, errBuf2 bytes.Buffer
	root2 := NewRootCommand(Deps{})
	root2.SetOut(&out2)
	root2.SetErr(&errBuf2)
	root2.SetArgs([]string{"policy", "suggest", "l1", "--namespace", "ns", "--kubeconfig", "/nonexistent/kubeconfig", "-f", f})
	runErr2 := root2.Execute()
	if runErr2 == nil {
		t.Fatal("Execute() = nil, want an error")
	}
	errOut2 := runErr2.Error() + "\n" + errBuf2.String() + "\n" + out2.String()
	if !strings.Contains(errOut2, "no audit records for Loop") {
		t.Errorf("error output lacks %q:\n%s", "no audit records for Loop", errOut2)
	}
	if strings.Contains(errOut2, "invalid configuration") {
		t.Errorf("kubeconfig error leaked into the output:\n%s", errOut2)
	}
}

// R51 I236(1): input that holds nothing for the Loop's agent (run 12: the
// operator log since its last rotation had none of this Loop's audit lines)
// must fail, not print an empty spec.
func TestSuggestPolicyEmptyInputFails(t *testing.T) {
	in := strings.Join([]string{
		`{"time":"2026-11-06T10:00:00Z","loop":"other","namespace":"ns","iteration":"0","source":"kubearmor","action":"Audit","target":"/bin/sh","verdict":"allowed","detail":"operation=Process, pod=other-sandbox, policy=p"}`,
		`{"time":"2026-11-06T10:00:01Z","loop":"other","namespace":"ns","iteration":"0","source":"egress-proxy","action":"connect","target":"example.com:443","verdict":"blocked","detail":""}`,
	}, "\n")
	var out bytes.Buffer
	err := suggestPolicy(strings.NewReader(in), &out, "l1", "ns", "l1-suggested")
	if err == nil || !strings.Contains(err.Error(), "no audit records for Loop") {
		t.Fatalf("want an error containing %q, got: %v", "no audit records for Loop", err)
	}
	if out.Len() != 0 {
		t.Fatalf("output should be empty, got:\n%s", out.String())
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

// D119 = (b): the fake LearnSummaryReader stands in for the learn-summary
// ConfigMap reader.
type fakeLearnSummaryReader struct {
	records string
	dropped int64
	err     error
}

func (f *fakeLearnSummaryReader) ReadLearnSummary(_ context.Context, _, _ string) (string, int64, error) {
	return f.records, f.dropped, f.err
}

// fakeDeps builds a Deps whose Loops is a fake, so the pre-run hook never
// loads the ambient kubeconfig (NewDeps needs no cluster) in the
// learn-summary specs.
func fakeDeps(ls LearnSummaryReader) Deps {
	return Deps{Loops: &fakeLoopClient{}, LearnSummary: ls}
}

// D119 = (b), R51 I236: without -f, `policy suggest` reads the learn-summary
// ConfigMap (via the LearnSummary seam) and shapes the candidate from its
// records.
func TestSuggestPolicyFromLearnSummary(t *testing.T) {
	oldDeps := currentDeps
	currentDeps = Deps{LearnSummary: &fakeLearnSummaryReader{
		records: strings.Join([]string{
			`{"time":"2026-11-06T10:00:00Z","loop":"l1","namespace":"ns","iteration":"0","source":"kubearmor","action":"Audit","target":"/usr/bin/git","verdict":"allowed","detail":"operation=File, pod=l1-sandbox, policy=p","count":5}`,
			`{"time":"2026-11-06T10:00:01Z","loop":"l1","namespace":"ns","iteration":"0","source":"egress-proxy","action":"connect","target":"github.com:443","verdict":"blocked","detail":""}`,
		}, "\n"),
	}}
	t.Cleanup(func() { currentDeps = oldDeps })

	var out, errBuf bytes.Buffer
	root := NewRootCommand(fakeDeps(currentDeps.LearnSummary))
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"policy", "suggest", "l1", "-n", "ns"})
	runErr := root.Execute()
	if runErr != nil {
		t.Fatalf("Execute() error: %v\n%s", runErr, errBuf.String())
	}
	got := out.String()
	for _, want := range []string{"/usr/bin/git", "github.com:443"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

// D119 = (b): a missing learn-summary ConfigMap (NotFound from the operator)
// fails with the explanatory error.
func TestSuggestPolicyLearnSummaryMissing(t *testing.T) {
	oldDeps := currentDeps
	currentDeps = Deps{LearnSummary: &fakeLearnSummaryReader{
		err: fmt.Errorf("no learn summary for Loop ns/l1 (ConfigMap l1-learn-summary not found): the operator writes it only for a learn-mode Loop (every referenced AgentPolicy auditOnly) once its sandbox has run; or pass -f <operator log>"),
	}}
	t.Cleanup(func() { currentDeps = oldDeps })

	var out, errBuf bytes.Buffer
	root := NewRootCommand(fakeDeps(currentDeps.LearnSummary))
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"policy", "suggest", "l1", "-n", "ns"})
	runErr := root.Execute()
	if runErr == nil {
		t.Fatal("Execute() = nil, want an error")
	}
	errOut := runErr.Error() + "\n" + errBuf.String()
	if !strings.Contains(errOut, "no learn summary for Loop") {
		t.Errorf("error output lacks %q:\n%s", "no learn summary for Loop", errOut)
	}
}

// D119 = (b): a learn summary that dropped records at its bound prints the
// warning to stderr.
func TestSuggestPolicyLearnSummaryDropped(t *testing.T) {
	oldDeps := currentDeps
	currentDeps = Deps{LearnSummary: &fakeLearnSummaryReader{
		records: `{"time":"2026-11-06T10:00:00Z","loop":"l1","namespace":"ns","iteration":"0","source":"kubearmor","action":"Audit","target":"/usr/bin/git","verdict":"allowed","detail":"operation=File, pod=l1-sandbox, policy=p","count":5}` + "\n",
		dropped: 3,
	}}
	t.Cleanup(func() { currentDeps = oldDeps })

	var out, errBuf bytes.Buffer
	root := NewRootCommand(fakeDeps(currentDeps.LearnSummary))
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"policy", "suggest", "l1", "-n", "ns"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v\n%s", err, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "dropped 3 record(s)") {
		t.Errorf("stderr lacks the dropped warning:\n%s", errBuf.String())
	}
}
