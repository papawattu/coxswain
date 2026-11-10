package controller

import (
	"regexp"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxs "github.com/papawattu/coxswain/api/v1alpha1"
)

// TestF95ImportGitCallsCarrySafeDir checks that EVERY git call in the
// deliver import-agent script carries the safe.directory flags (F95: since
// F67 the kind run died in import-agent with "detected dubious ownership"
// because the two F67 object-copy git calls lacked the flags the later
// rev-parse and checkout calls already had).
func TestF95ImportGitCallsCarrySafeDir(t *testing.T) {
	r := &LoopReconciler{CNIProber: testCNIProber()}

	agentRepo := t.TempDir()
	mustGit(t, agentRepo, "init", "-q", "-b", "main")
	mustGit(t, agentRepo, "config", "user.email", "agent@coxswain.test")
	mustGit(t, agentRepo, "config", "user.name", "Agent")
	writeFile(t, agentRepo, "round.go", "package main\n\nfunc Round(x float64) int { return int(x) }\n")
	mustGit(t, agentRepo, "add", "-A")
	mustGit(t, agentRepo, "commit", "-q", "-m", "base")
	writeFile(t, agentRepo, "round.go", "package main\n\nfunc Round(x float64) int { return int(x + 0.5) }\n")
	mustGit(t, agentRepo, "add", "-A")
	mustGit(t, agentRepo, "commit", "-q", "-m", "agent: fix rounding")
	verifySHA := gitSHA(t, agentRepo)

	loop := &coxs.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "f95deliver", Namespace: "f95-ns", UID: "f95-uid"},
		Spec: coxs.LoopSpec{
			Workspace: coxs.Workspace{
				Repo:                agentRepo + "/.git",
				Ref:                 "main",
				GitCredentialSecret: testCredSecretName,
			},
		},
		Status: coxs.LoopStatus{
			CurrentVerify: &coxs.CurrentVerifyStatus{VerifiedCommit: verifySHA},
		},
	}

	script := deliverContainerScript(t, r.deliverImportAgentContainer(loop))

	gitLine := regexp.MustCompile(`\bgit -c`)
	var gitCalls []string
	for line := range strings.SplitSeq(script, "\n") {
		if gitLine.MatchString(line) {
			gitCalls = append(gitCalls, line)
		}
	}
	// Positive control: the import script has at least the two object-copy
	// calls plus the rev-parse and the checkout.
	if len(gitCalls) < 4 {
		t.Fatalf("import-agent script has %d git -c lines; want at least 4 (positive control)", len(gitCalls))
	}
	for _, line := range gitCalls {
		if !strings.Contains(line, "safe.directory=/deliver") {
			t.Errorf("git call lacks safe.directory=/deliver:\n%s", line)
		}
	}
}
