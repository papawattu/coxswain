package controller

// P5a (Phase 5 shared memory, PLAN-PHASE5.md:34; D66 accepted): the copy-in
// init container's script is a REAL shell script. This execution test runs the
// ACTUAL generated script (memoryCopyInScriptForWorkspace) with /memory and
// /workspace substituted to tmp dirs — not a paraphrased re-implementation
// (AGENTS.md test norm: the test exercises the real script, not a copy of its
// logic).
//
// The semantics tested:
//   - An EMPTY /memory (a fresh repo) leaves NO .coxswain/memory dir at all.
//   - A NON-EMPTY /memory is copied in ADDITIVELY (never deleting a file that
//     is not in the PVC).
//   - Idempotent: a second run on the same contents is a no-op.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

func runCopyInScript(t *testing.T, src, dest string) {
	t.Helper()
	// Substitute the placeholder: the script's DEST is the workspace's
	// .coxswain/memory path; the test runs it against a tmp DEST. The SRC
	// (/memory) is a fixed path in the script — the test can't remount it, so
	// it uses a sed substitution on the generated script (the script's text
	// is the unit under test; the substitution is a path constant, not
	// logic).
	script := memoryCopyInScriptForWorkspace()
	tmpScript := filepath.Join(t.TempDir(), "copy-in.sh")
	patched := script
	patched = replaceAll(patched, "DEST="+memoryCopyDestPath, "DEST="+dest)
	patched = replaceAll(patched, "SRC=/memory", "SRC="+src)
	if err := os.WriteFile(tmpScript, []byte(patched), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	cmd := exec.Command("/bin/sh", tmpScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("copy-in script failed: %v\noutput: %s", err, out)
	}
}

func replaceAll(s, old, new string) string {
	out := s
	for {
		i := indexOf(out, old)
		if i < 0 {
			return out
		}
		out = out[:i] + new + out[i+len(old):]
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestCopyInScriptEmptyMemoryLeavesNoDir: a fresh repo (an empty memory PVC,
// the mount point is an empty dir) leaves NO .coxswain/memory dir at all (the
// runner's prompt is unchanged when the dir is absent).
func TestCopyInScriptEmptyMemoryLeavesNoDir(t *testing.T) {
	src := t.TempDir() // empty (a fresh repo: no lessons yet)
	ws := t.TempDir()
	dest := filepath.Join(ws, ".coxswain", "memory")
	if err := os.MkdirAll(filepath.Join(ws, ".coxswain"), 0o755); err != nil {
		t.Fatalf("mkdir .coxswain: %v", err)
	}
	runCopyInScript(t, src, dest)
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("empty memory must leave NO .coxswain/memory dir (got err=%v)", err)
	}
}

// TestCopyInScriptNonEmptyMemoryCopiesIn: a non-empty memory PVC is copied
// into the workspace's .coxswain/memory/ (the files land in the workspace's
// operator-owned result dir, ADR-0004).
func TestCopyInScriptNonEmptyMemoryCopiesIn(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "abc123def456abcdef"), []byte("lesson one"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "def456abc123def4"), []byte("lesson two"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ws := t.TempDir()
	dest := filepath.Join(ws, ".coxswain", "memory")
	if err := os.MkdirAll(filepath.Join(ws, ".coxswain"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runCopyInScript(t, src, dest)
	for _, name := range []string{"abc123def456abcdef", "def456abc123def4"} {
		data, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil {
			t.Fatalf("copy-in did not create %s: %v", name, err)
		}
		if len(data) == 0 {
			t.Fatalf("copy-in created an empty %s", name)
		}
	}
}

// TestCopyInScriptIsAdditive: a file in the workspace but not in the PVC is
// left alone (the copy-in is an ADDITIVE read of the PVC; it never deletes a
// workspace file). A file in the PVC is added.
func TestCopyInScriptIsAdditive(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "from-pvc"), []byte("lesson"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ws := t.TempDir()
	dest := filepath.Join(ws, ".coxswain", "memory")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A pre-existing workspace memory file (a lesson written by P5b before
	// the PVC was updated, or a manual file): it must survive the copy-in.
	if err := os.WriteFile(filepath.Join(dest, "pre-existing"), []byte("keep me"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runCopyInScript(t, src, dest)
	// The PVC's file is added.
	if _, err := os.Stat(filepath.Join(dest, "from-pvc")); err != nil {
		t.Fatalf("the PVC's file was not copied in: %v", err)
	}
	// The pre-existing workspace file is preserved (additive, never deletes).
	data, err := os.ReadFile(filepath.Join(dest, "pre-existing"))
	if err != nil {
		t.Fatalf("the pre-existing workspace file was deleted by the copy-in: %v", err)
	}
	if string(data) != "keep me" {
		t.Fatalf("the pre-existing workspace file was clobbered: %q", data)
	}
}

// TestCopyInScriptIsIdempotent: a second run on the same contents is a no-op
// (the files are unchanged, no duplicate).
func TestCopyInScriptIsIdempotent(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "abc123def456abcdef"), []byte("lesson one"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ws := t.TempDir()
	dest := filepath.Join(ws, ".coxswain", "memory")
	if err := os.MkdirAll(filepath.Join(ws, ".coxswain"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runCopyInScript(t, src, dest)
	first, err := os.ReadFile(filepath.Join(dest, "abc123def456abcdef"))
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	runCopyInScript(t, src, dest) // a second copy-in (the next phase start)
	second, err := os.ReadFile(filepath.Join(dest, "abc123def456abcdef"))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("the copy-in is not idempotent: %q vs %q", first, second)
	}
	entries, _ := os.ReadDir(dest)
	if len(entries) != 1 {
		t.Fatalf("the copy-in created duplicates: %d entries, want 1", len(entries))
	}
}

// TestSurfaceMemoryPVCUnbound (the visible degradation, R26 I90; re-armed by
// R39 I144): an unbound memory PVC emits a MemoryPVCUnbound Event, and the
// marker is a quiet WINDOW, not a one-shot: the Event repeats once
// memoryPVCUnboundQuiet has passed while the PVC stays unbound, so a
// degradation longer than --event-ttl (1h) is not silent.
//
// A nil Recorder skips the emission without error (most envtests).
func TestSurfaceMemoryPVCUnbound(t *testing.T) {
	ctx := context.Background()
	newLoop := func() *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: "p5a-ev", Namespace: "default"}}
	}
	// A nil Recorder: the emission is skipped without error (the unit's
	// contract: a nil Recorder never panics).
	r := &LoopReconciler{CNIProber: testCNIProber(), Recorder: nil}
	s := runtime.NewScheme()
	if err := coxv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("build scheme: %v", err)
	}
	nilRecorderClient := fake.NewClientBuilder().WithScheme(s).WithObjects(newLoop()).Build()
	r.Client = nilRecorderClient
	r.surfaceMemoryPVCUnbound(ctx, newLoop(), "memory-abc")

	// The window: one Event, then silence inside the window, then a repeat.
	rec := record.NewFakeRecorder(64)
	loop := newLoop()
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(loop).Build()
	clock := metav1.NewTime(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	reconciler := &LoopReconciler{
		CNIProber: testCNIProber(),
		Client:    cl,
		Recorder:  rec,
		now:       func() metav1.Time { return clock },
	}
	drain := func() int {
		n := 0
		for {
			select {
			case e := <-rec.Events:
				if strings.Contains(e, memoryPVCUnboundReason) {
					n++
				}
			default:
				return n
			}
		}
	}

	reconciler.surfaceMemoryPVCUnbound(ctx, loop, "memory-abc")
	if got := drain(); got != 1 {
		t.Fatalf("the first unbound pass must emit one %s Event, got %d", memoryPVCUnboundReason, got)
	}
	marker := loop.Annotations[memoryPVCUnboundNotifiedAnnotation]
	if _, err := time.Parse(time.RFC3339, marker); err != nil {
		t.Fatalf("the marker must be the last Event's time (RFC3339), got %q: %v", marker, err)
	}

	// Inside the window: muted (a per-reconcile Event would be noise).
	clock = metav1.NewTime(clock.Add(memoryPVCUnboundQuiet / 2))
	reconciler.surfaceMemoryPVCUnbound(ctx, loop, "memory-abc")
	if got := drain(); got != 0 {
		t.Fatalf("inside the quiet window the surface must stay silent, got %d Events", got)
	}

	// Past the window: re-announced (the deployment is still degraded, and its
	// Event from the first pass has expired at --event-ttl).
	clock = metav1.NewTime(clock.Add(memoryPVCUnboundQuiet))
	reconciler.surfaceMemoryPVCUnbound(ctx, loop, "memory-abc")
	if got := drain(); got != 1 {
		t.Fatalf("past the quiet window the surface must re-announce exactly once, got %d Events", got)
	}
}
