package controller

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/tamper"
)

// s1RunScanPrep executes the REAL scanPrepScript with only the /verify and
// /scan paths substituted, returning the scan dir, exit code and output.
func s1RunScanPrep(t *testing.T, dir, base, verified string, globs []string) (string, int, string) {
	t.Helper()
	scanDir := t.TempDir()
	script := scanPrepScript(base, verified, globs)
	script = strings.ReplaceAll(script, "cd "+verifyScratchPath, "cd "+dir)
	script = strings.ReplaceAll(script, "S="+verifyScanPath, "S="+scanDir)
	cmd := exec.Command("sh", "-c", script)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	err := cmd.Run()
	if err == nil {
		return scanDir, 0, out.String()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return scanDir, ee.ExitCode(), out.String()
	}
	t.Fatalf("run: %v", err)
	return "", -1, ""
}

func s1Files(t *testing.T, root string) []string {
	t.Helper()
	var got []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	return got
}

// TestScanPrepScriptExecutes (S1): the real script copies a changed
// non-protected .go file (both sides), a new file with no base copy, and
// nothing else: a changed *_test.go (protected under preset go) and a non-.go
// file are not copied.
func TestScanPrepScriptExecutes(t *testing.T) {
	globs := tamper.ExpandPreset(tamper.PresetGo, nil, false)
	dir, base, verified := f39Repo(t, func(d string) {
		f39Edit("pkg/x.go", "package pkg\n// work\n")(d)
		f39Edit("pkg/new.go", "package pkg\n// new\n")(d)
		f39Edit("pkg/x_test.go", "package pkg\n// doctored\n")(d)
		f39Edit("README.md", "# changed\n")(d)
	})
	scanDir, code, out := s1RunScanPrep(t, dir, base, verified, globs)
	if code != 0 {
		t.Fatalf("exit=%d output=%q", code, out)
	}
	got := strings.Join(s1Files(t, scanDir), ",")
	want := "base/pkg/x.go,verified/pkg/new.go,verified/pkg/x.go"
	if got != want {
		t.Fatalf("files in /scan = %q, want %q (output %q)", got, want, out)
	}
	for rel, wantBody := range map[string]string{
		"verified/pkg/x.go":   "package pkg\n// work\n",
		"base/pkg/x.go":       "package pkg\n",
		"verified/pkg/new.go": "package pkg\n// new\n",
	} {
		b, err := os.ReadFile(filepath.Join(scanDir, rel))
		if err != nil || string(b) != wantBody {
			t.Errorf("%s = %q (%v), want %q", rel, b, err, wantBody)
		}
	}
}

// TestScanPrepScriptGitFailureMarksPrepFailed (S1): a git failure never
// blocks (exit 0, D17.2) and never reads as clean: the marker is written.
func TestScanPrepScriptGitFailureMarksPrepFailed(t *testing.T) {
	dir, base, _ := f39Repo(t, f39Edit("pkg/x.go", "package pkg\n// w\n"))
	scanDir, code, out := s1RunScanPrep(t, dir, base, strings.Repeat("0", 40), tamper.ExpandPreset(tamper.PresetGo, nil, false))
	if code != 0 {
		t.Fatalf("scan-prep must exit 0, got %d: %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(scanDir, verifyScanPrepFailedMarker)); err != nil {
		t.Fatalf("PREP_FAILED marker missing: %v (output %q)", err, out)
	}
}

// TestBuildVerifyJobSpecScanInits (S1): the init order, the scan's image and
// its read-only /scan.
func TestBuildVerifyJobSpecScanInits(t *testing.T) {
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "s1loop", Namespace: "default"},
		Spec:       s5aLoopSpec(),
	}
	loop.Spec.Verify.AcceptanceChecks = []string{"go test ./..."}
	loop.Status.BaseCommit = s5aBaseCommit
	loop.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: s5aHeadCommit}
	r := &LoopReconciler{MemoryWriterImage: "example.com/operator:test"}
	spec := r.buildVerifyJobSpec(loop)
	names := make([]string, 0, len(spec.Template.Spec.InitContainers))
	by := map[string]corev1.Container{}
	for _, c := range spec.Template.Spec.InitContainers {
		names = append(names, c.Name)
		by[c.Name] = c
	}
	want := "clone-base,import-agent,tamper,artifact,scan-prep,scan,canary,check-0"
	if strings.Join(names, ",") != want {
		t.Fatalf("init order = %v, want %s", names, want)
	}
	scan := by["scan"]
	if scan.Image != "example.com/operator:test" {
		t.Errorf("scan image = %q, want the memory-writer (operator) image", scan.Image)
	}
	if len(scan.Command) != 1 || scan.Command[0] != "/usr/local/bin/coxswain-subvert-scan" || len(scan.Args) != 1 || scan.Args[0] != "/scan" {
		t.Errorf("scan command/args = %v %v", scan.Command, scan.Args)
	}
	var scanMountRO bool
	for _, m := range scan.VolumeMounts {
		if m.Name == "scan" {
			scanMountRO = m.ReadOnly && m.MountPath == "/scan"
		}
		if m.Name == verifyVol {
			t.Errorf("scan must not mount /verify")
		}
	}
	if !scanMountRO {
		t.Errorf("/scan must be mounted read-only in scan: %+v", scan.VolumeMounts)
	}
	if scan.TerminationMessagePath != verifyTerminationLogPath {
		t.Errorf("scan termination path = %q", scan.TerminationMessagePath)
	}
	prep := by["scan-prep"]
	if prep.Image == scan.Image {
		t.Errorf("scan-prep must run on the git image, not the operator image")
	}
	for _, m := range prep.VolumeMounts {
		if m.Name == verifyVol && !m.ReadOnly {
			t.Errorf("scan-prep must mount /verify read-only")
		}
	}
	var scanVol *corev1.Volume
	for i, v := range spec.Template.Spec.Volumes {
		if v.Name == "scan" {
			scanVol = &spec.Template.Spec.Volumes[i]
		}
	}
	if scanVol == nil || scanVol.EmptyDir == nil || scanVol.EmptyDir.SizeLimit == nil {
		t.Fatalf("the scan volume must be a bounded emptyDir: %+v", scanVol)
	}
}

// TestVerifyOutcomeIgnoresScanInits (S1, I49 norm): the scan inits are in no
// decision. While scan-prep/scan are Waiting or Running the checks cannot
// have run (no decision); a terminated scan, even with a non-zero exit, maps to
// no tampered/iterate/failed outcome.
func TestVerifyOutcomeIgnoresScanInits(t *testing.T) {
	term := func(code int32) corev1.ContainerState {
		return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}}
	}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	waiting := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}
	pod := func(prep, scan, check *corev1.ContainerState) *corev1.Pod {
		sts := make([]corev1.ContainerStatus, 0, 8)
		sts = append(sts,
			corev1.ContainerStatus{Name: verifyCloneBaseInit, State: term(0)},
			corev1.ContainerStatus{Name: verifyImportAgentInit, State: term(0)},
			corev1.ContainerStatus{Name: verifyTamperInit, State: term(0)},
			corev1.ContainerStatus{Name: verifyArtifactInit, State: term(0)})
		sts = append(sts, corev1.ContainerStatus{Name: verifyScanPrepInit, State: *prep},
			corev1.ContainerStatus{Name: verifyScanInit, State: *scan},
			corev1.ContainerStatus{Name: verifyCanaryInit, State: term(0)},
			corev1.ContainerStatus{Name: "check-0", State: *check})
		return &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: sts}}
	}
	t0, t1, rn, wt := term(0), term(1), running, waiting
	cases := []struct {
		name        string
		prep, scan  *corev1.ContainerState
		check       *corev1.ContainerState
		wantOutcome int
	}{
		{"scan running, check waiting", &t0, &rn, &wt, verifyNoDecision},
		{"scan waiting, check waiting", &t0, &wt, &wt, verifyNoDecision},
		{"scan-prep running, check waiting", &rn, &wt, &wt, verifyNoDecision},
		{"scan terminated 1, checks passed", &t0, &t1, &t0, verifySucceeded},
		{"scan-prep terminated 1, checks passed", &t1, &t0, &t0, verifySucceeded},
		{"scan terminated 0, checks passed", &t0, &t0, &t0, verifySucceeded},
	}
	for _, tc := range cases {
		got, _, _, _, _ := verifyOutcome(pod(tc.prep, tc.scan, tc.check), 1, false, false)
		if got != tc.wantOutcome {
			t.Errorf("%s: outcome=%d, want %d", tc.name, got, tc.wantOutcome)
		}
	}
}
