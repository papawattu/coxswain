package cli

// P5a (Phase 5 shared memory): the `cox memory` command's unit specs. The
// MemoryReader is stubbed (the peek pod's kubectl seams are faked); the specs
// cover the command's decision logic (the PVC name from the repo URL, the
// no-memory notes, and the content print).

import (
	"context"
	"strings"
	"testing"

	"github.com/papawattu/coxswain/pkg/memkey"
)

// fakeMemoryReader is the MemoryReader stub (a canned read).
type fakeMemoryReader struct {
	content string
	found   bool
	err     error
}

func (f *fakeMemoryReader) Read(ctx context.Context, pvcName string) (string, bool, error) {
	return f.content, f.found, f.err
}

const memTestRepo = "https://github.com/papawattu/coxswain.git"

// TestMemoryCommandPrintsContent: a memory PVC with lessons prints them
// (the P5a spec: `cox memory <repo-url>`: inspect the memory PVC).
func TestMemoryCommandPrintsContent(t *testing.T) {
	d := &Deps{Memory: &fakeMemoryReader{
		content: "-- abc123def456abcdef:\nlesson one\n",
		found:   true,
	}}
	out := &strings.Builder{}
	err := memoryRepo(context.Background(), *d, "default", memTestRepo, out)
	if err != nil {
		t.Fatalf("memoryRepo: %v", err)
	}
	if !strings.Contains(out.String(), "lesson one") {
		t.Fatalf("the memory content was not printed:\n%s", out.String())
	}
}

// TestMemoryCommandEmptyPVC: a memory PVC that exists but is empty (a fresh
// repo, no lessons yet) prints the "no memory yet" note (not an error).
func TestMemoryCommandEmptyPVC(t *testing.T) {
	d := &Deps{Memory: &fakeMemoryReader{content: "", found: false}}
	out := &strings.Builder{}
	err := memoryRepo(context.Background(), *d, "default", memTestRepo, out)
	if err != nil {
		t.Fatalf("an empty memory PVC is not an error: %v", err)
	}
	if !strings.Contains(out.String(), "No memory yet") {
		t.Fatalf("an empty memory PVC must print the note:\n%s", out.String())
	}
}

// TestMemoryCommandMissingPVC: a memory PVC that does not exist (a repo the
// operator has never run a Loop on) prints the "no memory yet" note (the
// memoryNotFoundErr sentinel — not an error).
func TestMemoryCommandMissingPVC(t *testing.T) {
	d := &Deps{Memory: &fakeMemoryReader{err: memoryNotFoundErr{}}}
	out := &strings.Builder{}
	err := memoryRepo(context.Background(), *d, "default", memTestRepo, out)
	if err != nil {
		t.Fatalf("a missing memory PVC is not an error: %v", err)
	}
	if !strings.Contains(out.String(), "No memory yet") {
		t.Fatalf("a missing memory PVC must print the note:\n%s", out.String())
	}
}

// TestMemoryCommandRealError: a real read failure (the peek pod failed, the
// PVC is not Bound) is an error (the CLI surfaces it, not a note).
func TestMemoryCommandRealError(t *testing.T) {
	d := &Deps{Memory: &fakeMemoryReader{err: errBoom}}
	out := &strings.Builder{}
	err := memoryRepo(context.Background(), *d, "default", memTestRepo, out)
	if err == nil {
		t.Fatal("a real read failure must be an error")
	}
}

// TestMemoryCommandEmptyRepoURL: an empty repo URL has no memory PVC (the
// keying returns ""); the command prints the note (not an error).
func TestMemoryCommandEmptyRepoURL(t *testing.T) {
	d := &Deps{Memory: &fakeMemoryReader{}}
	out := &strings.Builder{}
	err := memoryRepo(context.Background(), *d, "default", "", out)
	if err != nil {
		t.Fatalf("an empty repo URL is not an error: %v", err)
	}
	if !strings.Contains(out.String(), "No memory") {
		t.Fatalf("an empty repo URL must print the note:\n%s", out.String())
	}
}

// TestMemoryCommandUsesMemkeyPVCName: the command computes the PVC name with
// the shared keying (memkey.PVCName — the operator and the CLI agree on the
// name). A mismatch would make the CLI read a PVC that is not the Loop's.
func TestMemoryCommandUsesMemkeyPVCName(t *testing.T) {
	captured := ""
	d := &Deps{Memory: &fakeMemoryReader{}}
	// The fakeMemoryReader does not capture the name; use a capturing stub.
	d.Memory = &captureMemoryReader{name: &captured}
	_ = memoryRepo(context.Background(), *d, "default", memTestRepo, &strings.Builder{})
	want := memkey.PVCName(memTestRepo)
	if captured != want {
		t.Fatalf("the memory command must use memkey.PVCName: got %q, want %q", captured, want)
	}
}

// captureMemoryReader captures the PVC name the command reads (the test
// asserts the name is the shared keying).
type captureMemoryReader struct {
	name *string
}

func (c *captureMemoryReader) Read(ctx context.Context, pvcName string) (string, bool, error) {
	*c.name = pvcName
	return "", false, memoryNotFoundErr{}
}

var errBoom = errBoomT{}

type errBoomT struct{}

func (errBoomT) Error() string { return "boom" }
