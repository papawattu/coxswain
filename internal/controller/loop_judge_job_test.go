// Phase 7 J1c2 (PLAN-PHASE7.md slice J1, ADR-0011): the Judge Job builder's
// unit tests (loop_judge_job.go). Builder only — no Reconcile wiring (J1c3).
//
// The diff init's script is an EXECUTION test (the AGENTS.md embedded-script
// norm): the REAL generated script (judgeDiffScript) is run with /bin/sh
// against a real temporary git repo with two commits (only the repo path is
// substituted — the production /agent-src path is the repo's root), and the
// test asserts the diff.patch (path-substituted) holds the change, and that
// an unknown commit exits non-zero (fail closed).
package controller

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// judgeTestLoop builds a Loop with a goal, a judge (maxTokens 512), a model,
// a baseCommit, and a verifiedCommit (status.currentVerify) — the inputs
// buildJudgeJob needs. The plan hash is absent (no PlanApproved condition).
func judgeTestLoop(name string) *coxv1alpha1.Loop {
	return &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "judge-ns"},
		Spec: coxv1alpha1.LoopSpec{
			Goal: "make the failing test pass",
			Workspace: coxv1alpha1.Workspace{
				Repo: testRepoURL,
			},
			Agent: coxv1alpha1.AgentConfig{
				Image:         runnerImage,
				Model:         testModel,
				ModelEndpoint: "vllm.svc:8000",
			},
			Verify: coxv1alpha1.VerifyConfig{
				AcceptanceChecks: []string{"go test ./..."},
				Judge:            &coxv1alpha1.JudgeConfig{Enabled: true, MaxTokens: 512},
			},
		},
		Status: coxv1alpha1.LoopStatus{
			BaseCommit: "base0001",
			CurrentVerify: &coxv1alpha1.CurrentVerifyStatus{
				VerifiedCommit: "verified0002",
			},
			Verify: &coxv1alpha1.VerifyStatus{
				LastCheckResults: int32Ptrs(0, -1),
			},
		},
	}
}

// int32Ptrs builds a []*int32 from int32 literals (LastCheckResults'
// element type). A small helper: the literals repeat across the tests.
func int32Ptrs(vs ...int32) []*int32 {
	out := make([]*int32, len(vs))
	for i, v := range vs {
		c := v
		out[i] = &c
	}
	return out
}

func TestJudgeJobName(t *testing.T) {
	// Iteration 0 -> <loop>-judge-1 (verifyJobName's mapping: <= 0 is 1).
	loop := &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: "loop-a"}}
	loop.Status.Iteration = 0
	if got := judgeJobName(loop); got != "loop-a-judge-1" {
		t.Fatalf("iteration 0: got %q, want loop-a-judge-1", got)
	}
	// Iteration 3 -> <loop>-judge-3.
	loop.Status.Iteration = 3
	if got := judgeJobName(loop); got != "loop-a-judge-3" {
		t.Fatalf("iteration 3: got %q, want loop-a-judge-3", got)
	}
	// The name lines up with the verify Job's iteration (same mapping).
	if want := strings.Replace(verifyJobName(loop), "-verify-", "-judge-", 1); judgeJobName(loop) != want {
		t.Fatalf("the Judge Job name must reuse the verify Job's iteration mapping: got %q, want %q", judgeJobName(loop), want)
	}
}

func TestJudgeBuildSpec(t *testing.T) {
	r := &LoopReconciler{
		MemoryWriterImage: "operator:judge-test",
		WorkspaceGitImage: "git:verify-test",
	}
	loop := judgeTestLoop("loop-b")
	job, err := r.buildJudgeJob(loop)
	if err != nil {
		t.Fatalf("buildJudgeJob: %v", err)
	}

	// The name.
	if job.Name != "loop-b-judge-1" {
		t.Fatalf("job name: got %q, want loop-b-judge-1", job.Name)
	}
	if job.Namespace != loop.Namespace {
		t.Fatalf("job namespace: got %q, want %q", job.Namespace, loop.Namespace)
	}

	spec := job.Spec
	// backoffLimit 0 (one run).
	if spec.BackoffLimit == nil || *spec.BackoffLimit != 0 {
		t.Fatalf("backoffLimit: got %v, want 0", spec.BackoffLimit)
	}
	// activeDeadlineSeconds 300 (the memory-writer's pattern).
	if spec.ActiveDeadlineSeconds == nil || *spec.ActiveDeadlineSeconds != 300 {
		t.Fatalf("activeDeadlineSeconds: got %v, want 300", spec.ActiveDeadlineSeconds)
	}

	pod := spec.Template.Spec
	// No SA token (F66/D12: the Job makes no API calls).
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken != false {
		t.Fatalf("automountServiceAccountToken: got %v, want false", pod.AutomountServiceAccountToken)
	}
	// The init container.
	if len(pod.InitContainers) != 1 {
		t.Fatalf("init containers: got %d, want 1", len(pod.InitContainers))
	}
	diffInit := pod.InitContainers[0]
	if diffInit.Name != "judge-diff" {
		t.Fatalf("init name: got %q, want judge-diff", diffInit.Name)
	}
	// The init uses the verify base image (it has git).
	if diffInit.Image != "git:verify-test" {
		t.Fatalf("init image: got %q, want git:verify-test (the verify base image)", diffInit.Image)
	}
	// The init mounts the PVC READ-ONLY.
	if !mountReadOnly(diffInit, "agent-workspace", "/agent-src") {
		t.Fatalf("the judge-diff init must mount the workspace PVC read-only at /agent-src")
	}
	// The /judge emptyDir mount.
	if !hasMount(diffInit, "judge", "/judge") {
		t.Fatalf("the judge-diff init must mount the /judge emptyDir")
	}
	// The init's command is the real generated script.
	if len(diffInit.Command) != 3 || diffInit.Command[0] != "/bin/sh" || diffInit.Command[1] != "-c" {
		t.Fatalf("init command: got %v, want [sh -c <script>]", diffInit.Command)
	}
	if !strings.Contains(diffInit.Command[2], "head -c 262145") {
		t.Fatalf("init script must carry the 256 KiB+1 diff cap (head -c 262145, the truncation marker), got:\n%s", diffInit.Command[2])
	}

	// The judge container.
	if len(pod.Containers) != 1 {
		t.Fatalf("containers: got %d, want 1", len(pod.Containers))
	}
	judge := pod.Containers[0]
	if judge.Name != "judge" {
		t.Fatalf("container name: got %q, want judge", judge.Name)
	}
	// The judge container uses the memory-writer (operator) image.
	if judge.Image != "operator:judge-test" {
		t.Fatalf("judge image: got %q, want operator:judge-test (the operator's own image)", judge.Image)
	}
	// The command.
	if len(judge.Command) != 1 || judge.Command[0] != "/usr/local/bin/coxswain-judge" {
		t.Fatalf("judge command: got %v, want [/usr/local/bin/coxswain-judge]", judge.Command)
	}
	// The security context (non-root, read-only root, no escalation, caps
	// dropped).
	assertJudgeSecurityContext(t, judge)
	// The termination message path (on the /tmp emptyDir).
	if judge.TerminationMessagePath != judgeResultPath {
		t.Fatalf("terminationMessagePath: got %q, want %s", judge.TerminationMessagePath, judgeResultPath)
	}
	if !hasMount(judge, termmsgVol, "/tmp") {
		t.Fatalf("the judge container must mount the termmsg emptyDir at /tmp")
	}
	// The diff path (read-only on the /judge emptyDir).
	if !mountReadOnly(judge, judgeVol, "/judge") {
		t.Fatalf("the judge container must mount /judge read-only (it writes its verdict to /tmp, never back to the diff)")
	}
	// The env (every var, exact values).
	assertJudgeEnv(t, judge)
	// The owner reference: the Loop, controller: true (the caller sets it
	// via ctrl.SetControllerReference — the builder's job is to NOT set a
	// foreign owner, and the test asserts the reference is absent before
	// the caller adds it [the memory-writer pattern: ensureMemoryWriterJobAndNetPolicy
	// calls SetControllerReference after build]).
	if len(job.OwnerReferences) != 0 {
		t.Fatalf("the builder must not stamp an owner reference (the caller does), got %v", job.OwnerReferences)
	}
	// The owner reference is settable: the builder's ObjectMeta carries the
	// Loop's namespace (a controller reference to the Loop is valid).
	if job.Namespace != loop.Namespace {
		t.Fatalf("the Job must be in the Loop's namespace (the owner reference is the Loop)")
	}
}

// assertJudgeSecurityContext checks the judge container's SecurityContext:
// non-root, read-only rootfs, no privilege escalation, ALL caps dropped.
func assertJudgeSecurityContext(t *testing.T, judge corev1.Container) {
	t.Helper()
	sc := judge.SecurityContext
	if sc == nil {
		t.Fatalf("the judge container must carry a SecurityContext")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Fatalf("the judge container must run non-root")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Fatalf("the judge container must run with a read-only rootfs")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation != false {
		t.Fatalf("the judge container must not allow privilege escalation")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("the judge container must drop ALL capabilities, got %+v", sc.Capabilities)
	}
}

// assertJudgeEnv checks the judge container's env vars (exact values).
func assertJudgeEnv(t *testing.T, judge corev1.Container) {
	t.Helper()
	env := envMap(judge.Env)
	want := map[string]string{
		"COX_JUDGE_BASE_URL":    "http://loop-b-proxy.judge-ns.svc.cluster.local:8080",
		"COX_JUDGE_MODEL":       testModel,
		"COX_JUDGE_MAX_TOKENS":  "512",
		"COX_JUDGE_GOAL":        "make the failing test pass",
		"COX_JUDGE_PLAN":        "",
		"COX_JUDGE_DIFF_PATH":   "/judge/diff.patch",
		"COX_JUDGE_RESULT_PATH": judgeResultPath,
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env %s: got %q, want %q", k, env[k], v)
		}
	}
	// COX_JUDGE_CHECKS: the JSON from the last verify's check commands + exit
	// codes. The NAME is the acceptance check COMMAND (spec.verify
	// .acceptanceChecks[i]), NOT the container name (check-0, check-1, …) —
	// the Judge needs to know WHAT the check verifies. The exit codes are
	// status.verify.lastCheckResults (one per acceptance check, in order —
	// the -1 sentinel means "did not run / no exit code yet").
	var checks []struct {
		Name     string `json:"name"`
		ExitCode *int32 `json:"exitCode,omitempty"`
	}
	if err := json.Unmarshal([]byte(env["COX_JUDGE_CHECKS"]), &checks); err != nil {
		t.Fatalf("COX_JUDGE_CHECKS is not a JSON array: %v (value %q)", err, env["COX_JUDGE_CHECKS"])
	}
	if len(checks) != 2 {
		t.Fatalf("COX_JUDGE_CHECKS: got %d entries, want 2: %v", len(checks), env["COX_JUDGE_CHECKS"])
	}
	if checks[0].Name != "go test ./..." {
		t.Fatalf("COX_JUDGE_CHECKS[0].name: got %q, want the acceptance check command %q (NOT the container name check-0)", checks[0].Name, "go test ./...")
	}
	if checks[0].ExitCode == nil || *checks[0].ExitCode != 0 {
		t.Fatalf("COX_JUDGE_CHECKS[0].exitCode: got %v, want 0", checks[0].ExitCode)
	}
	if checks[1].Name != "check-1" {
		t.Fatalf("COX_JUDGE_CHECKS[1].name: got %q, want the container name fallback (no acceptance check defined at index 1)", checks[1].Name)
	}
	if checks[1].ExitCode == nil || *checks[1].ExitCode != -1 {
		t.Fatalf("COX_JUDGE_CHECKS[1].exitCode: got %v, want -1", checks[1].ExitCode)
	}
}

// mountReadOnly reports whether the container mounts a volume name at
// mountPath with ReadOnly true.
func mountReadOnly(ct corev1.Container, volName, mountPath string) bool {
	for _, m := range ct.VolumeMounts {
		if m.Name == volName && m.MountPath == mountPath {
			return m.ReadOnly
		}
	}
	return false
}

// hasMount reports whether the container mounts a volume name at mountPath
// (regardless of the read-only flag).
func hasMount(ct corev1.Container, volName, mountPath string) bool {
	for _, m := range ct.VolumeMounts {
		if m.Name == volName && m.MountPath == mountPath {
			return true
		}
	}
	return false
}

// envMap converts a []corev1.EnvVar to a map (the tests' read form).
func envMap(env []corev1.EnvVar) map[string]string {
	out := map[string]string{}
	for _, e := range env {
		out[e.Name] = e.Value
	}
	return out
}

func TestJudgeNetworkPolicy(t *testing.T) {
	loop := judgeTestLoop("loop-c")
	np := judgeNetworkPolicy(loop)
	// The name + namespace.
	if np.Name != "loop-c-judge-netpol" {
		t.Fatalf("netpol name: got %q, want loop-c-judge-netpol", np.Name)
	}
	if np.Namespace != loop.Namespace {
		t.Fatalf("netpol namespace: got %q, want %q", np.Namespace, loop.Namespace)
	}
	// The podSelector is the judgeForLabel (the Job stamps it on the pod
	// template).
	if got := np.Spec.PodSelector.MatchLabels[judgeForLabel]; got != "loop-c" {
		t.Fatalf("podSelector: got %v, want {judgeForLabel: loop-c}", np.Spec.PodSelector.MatchLabels)
	}
	// Ingress + egress are both denied (the fence).
	var haveIngress, haveEgress bool
	for _, pt := range np.Spec.PolicyTypes {
		switch pt {
		case networkingv1.PolicyTypeIngress:
			haveIngress = true
		case networkingv1.PolicyTypeEgress:
			haveEgress = true
		}
	}
	if !haveIngress || !haveEgress {
		t.Fatalf("the netpol must carry Ingress + Egress policy types, got %v", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Ingress) != 0 {
		t.Fatalf("the netpol must deny ALL ingress (no Ingress rules), got %v", np.Spec.Ingress)
	}
	// The egress: ONLY the model proxy + DNS (the memory-writer's fence).
	// A Loop with a model endpoint has exactly two egress rules: the proxy
	// (port 8080 to the proxy-for pod) + DNS (port 53 to kube-dns).
	if len(np.Spec.Egress) != 2 {
		t.Fatalf("the netpol must have exactly two egress rules (proxy + DNS), got %d", len(np.Spec.Egress))
	}
	// The proxy rule.
	proxyRule := np.Spec.Egress[0]
	if len(proxyRule.Ports) != 1 || proxyRule.Ports[0].Port.IntValue() != 8080 {
		t.Fatalf("the proxy egress rule must carry port 8080, got %v", proxyRule.Ports)
	}
	if len(proxyRule.To) != 1 {
		t.Fatalf("the proxy egress rule must name one peer, got %v", proxyRule.To)
	}
	peer := proxyRule.To[0]
	// The peer is the proxy-for pod in the Loop's namespace (the
	// memory-writer's peer shape).
	if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "judge-ns" {
		t.Fatalf("the proxy peer must select the Loop's namespace, got %+v", peer.NamespaceSelector)
	}
	if peer.PodSelector == nil || peer.PodSelector.MatchLabels["coxswain.io/proxy-for"] != "loop-c" {
		t.Fatalf("the proxy peer must select the proxy-for pod, got %+v", peer.PodSelector)
	}
	// The DNS rule.
	dnsRule := np.Spec.Egress[1]
	if len(dnsRule.Ports) != 2 {
		t.Fatalf("the DNS egress rule must carry two ports (UDP+TCP 53), got %v", dnsRule.Ports)
	}
	for _, p := range dnsRule.Ports {
		if p.Port.IntValue() != 53 {
			t.Fatalf("the DNS egress rule must carry port 53, got %v", p.Port)
		}
	}
	if len(dnsRule.To) != 1 {
		t.Fatalf("the DNS egress rule must name one peer, got %v", dnsRule.To)
	}
	// The DNS peer is kube-dns in kube-system (the dnsPeer's shape).
	dnsPeer := dnsRule.To[0]
	if dnsPeer.NamespaceSelector == nil || dnsPeer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" {
		t.Fatalf("the DNS peer must select kube-system, got %+v", dnsPeer.NamespaceSelector)
	}
	if dnsPeer.PodSelector == nil || dnsPeer.PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
		t.Fatalf("the DNS peer must select kube-dns, got %+v", dnsPeer.PodSelector)
	}

	// A Loop with no model endpoint: the netpol has NO egress rules (the
	// deny-all fence — the judge container with no proxy URL fails its call
	// and is recorded unavailable, D124).
	loop2 := judgeTestLoop("loop-d")
	loop2.Spec.Agent.ModelEndpoint = ""
	np2 := judgeNetworkPolicy(loop2)
	if len(np2.Spec.Egress) != 0 {
		t.Fatalf("a Loop with no model endpoint must have NO egress rules (the deny-all fence), got %v", np2.Spec.Egress)
	}
}

// TestJudgeDiffScriptExecutes (the AGENTS.md embedded-script norm): the REAL
// generated diff script is run with /bin/sh against a real temporary git
// repo with two commits. Only the production /agent-src repo path is
// substituted (the test's temp dir is the repo's root); the diff.patch path
// is the production /judge/diff.patch with the /judge root substituted to
// the temp dir. The script must write the diff (holding the change) to
// diff.patch, and an unknown commit must exit non-zero (fail closed: a
// failing git diff is never read as an empty diff).
//
// Mutation m1 (the mount is read-write) is a spec-level mutation (the Job's
// spec, not the script); m2 (drop the fail-closed git exit check) is tested
// here: the unknown-commit case must FAIL (the script exits 0 and writes an
// empty patch) when the fail-closed check is absent.
func TestJudgeDiffScriptExecutes(t *testing.T) {
	// Build a scratch repo with a base commit + an agent commit (the same
	// shape as the tamper script's execution test, loop_f39_tamper_exec_test.go).
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	write("pkg/x.go", "package pkg\n\nfunc X() int { return 1 }\n")
	write("go.mod", "module m\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	baseSHA := git("rev-parse", "HEAD")
	// The agent's change: a new line in pkg/x.go.
	write("pkg/x.go", "package pkg\n\nfunc X() int { return 2 }\n")
	git("add", "-A")
	git("commit", "-q", "-m", "agent")
	verifySHA := git("rev-parse", "HEAD")

	// The production script: it writes to /judge/diff.patch and operates on
	// /agent-src (the production mount points). The execution test runs the
	// REAL script with only the two production paths substituted: /agent-src
	// -> dir (the repo's root), /judge -> the temp dir (the diff.patch lands
	// in dir/diff.patch). Everything else (the git command, the head -c
	// cap, the fail-closed git exit check) is the production script, byte for
	// byte.
	script := judgeDiffScript(baseSHA, verifySHA)
	judgeRoot := t.TempDir()
	script = strings.ReplaceAll(script, "/agent-src", dir)
	script = strings.ReplaceAll(script, "/judge", judgeRoot)

	run := func() (int, string) {
		t.Helper()
		var out bytes.Buffer
		cmd := exec.Command("sh", "-c", script)
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

	// The happy case: the diff.patch holds the change (the agent's new line
	// is in the unified diff).
	code, out := run()
	if code != 0 {
		t.Fatalf("the happy case must exit 0, got exit %d: %s", code, out)
	}
	diffPath := filepath.Join(judgeRoot, "diff.patch")
	diff, err := os.ReadFile(diffPath)
	if err != nil {
		t.Fatalf("the diff.patch must exist at %s: %v", diffPath, err)
	}
	if !strings.Contains(string(diff), "func X() int { return 2 }") {
		t.Fatalf("the diff.patch must hold the agent's change, got:\n%s", string(diff))
	}
	if !strings.Contains(string(diff), "func X() int { return 1 }") {
		t.Fatalf("the diff.patch must hold the base line (the unified diff's context), got:\n%s", string(diff))
	}

	// The unknown-commit case: a SHA the repo does not have. The script must
	// exit non-zero (fail closed: a failing git diff is never read as an
	// empty diff). Mutation m2 (drop the fail-closed git exit check) makes
	// this exit 0 and write an empty patch — the mutation must FAIL this case.
	unknownSHA := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	badScript := strings.ReplaceAll(judgeDiffScript(baseSHA, unknownSHA), "/agent-src", dir)
	badScript = strings.ReplaceAll(badScript, "/judge", judgeRoot)
	var badOut bytes.Buffer
	badCmd := exec.Command("sh", "-c", badScript)
	badCmd.Stdout, badCmd.Stderr = &badOut, &badOut
	badCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	err = badCmd.Run()
	if err == nil {
		t.Fatalf("the unknown-commit case must exit non-zero (fail closed), got exit 0: %s", badOut.String())
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() != judgeGitErrorExit {
		t.Fatalf("the unknown-commit case must exit %d (git error), got %d: %s", judgeGitErrorExit, ee.ExitCode(), badOut.String())
	}

	// The over-cap case: an agent commit that adds a file > 300000 bytes
	// (well over the 262145-byte cap). The script must write a diff.patch of
	// EXACTLY 262145 bytes (judgeDiffCapBytes+1, the truncation marker
	// cmd/judge's MaxDiffBytes+1 read detects), and the uncapped
	// full.patch must NOT exist afterwards (rm -f removes it).
	//
	// Build a second scratch repo for this case (the base repo's diff is too
	// small to exceed the cap).
	bigDir := t.TempDir()
	gitBig := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = bigDir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gitBig %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gitBig("init", "-q")
	gitBig("config", "user.email", "t@example.com")
	gitBig("config", "user.name", "t")
	writeBig := func(rel string, content []byte) {
		t.Helper()
		p := filepath.Join(bigDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeBig("go.mod", []byte("module m\n"))
	gitBig("add", "-A")
	gitBig("commit", "-q", "-m", "base")
	bigBaseSHA := gitBig("rev-parse", "HEAD")
	// The agent's change: add a 310000-byte file (well over the 262145 cap).
	bigContent := make([]byte, 310000)
	for i := range bigContent {
		bigContent[i] = byte('x')
	}
	writeBig("big.bin", bigContent)
	gitBig("add", "-A")
	gitBig("commit", "-q", "-m", "agent-big")
	bigVerifySHA := gitBig("rev-parse", "HEAD")

	// Run the production script against the big repo (only the production
	// paths substituted: /agent-src -> bigDir, /judge -> a fresh temp dir).
	bigScript := strings.ReplaceAll(judgeDiffScript(bigBaseSHA, bigVerifySHA), "/agent-src", bigDir)
	bigJudgeRoot := t.TempDir()
	bigScript = strings.ReplaceAll(bigScript, "/judge", bigJudgeRoot)
	var bigOut bytes.Buffer
	bigCmd := exec.Command("sh", "-c", bigScript)
	bigCmd.Stdout, bigCmd.Stderr = &bigOut, &bigOut
	bigCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	err = bigCmd.Run()
	if err != nil {
		t.Fatalf("the over-cap case must exit 0, got: %v\n%s", err, bigOut.String())
	}
	// diff.patch must be EXACTLY 262145 bytes (judgeDiffCapBytes+1).
	bigDiffPath := filepath.Join(bigJudgeRoot, "diff.patch")
	bigDiff, err := os.ReadFile(bigDiffPath)
	if err != nil {
		t.Fatalf("the over-cap diff.patch must exist at %s: %v", bigDiffPath, err)
	}
	if len(bigDiff) != judgeDiffCapBytes+1 {
		t.Fatalf("the over-cap diff.patch must be EXACTLY %d bytes (judgeDiffCapBytes+1), got %d", judgeDiffCapBytes+1, len(bigDiff))
	}
	// full.patch must NOT exist (rm -f removed it).
	fullPatchPath := filepath.Join(bigJudgeRoot, "full.patch")
	if _, err := os.Stat(fullPatchPath); !os.IsNotExist(err) {
		t.Fatalf("full.patch must NOT exist after the script (rm -f removed it), but it does: %v", err)
	}
}
