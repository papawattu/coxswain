// I47 (REVIEW-PHASE1-R20) edge cases: the staged-paths evidence must survive
// rename, non-ASCII, and space-bearing paths; a pre-staged artifact (the
// agent's own git add -A) must not ride into the verified commit.
package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommitWorkspacePreStagedArtifactRefused (I47): the agent's own
// `git add -A` during the phase run pre-stages an oversized artifact into
// the index. commitWorkspace must reset the index and refuse — the artifact
// never enters the verified commit.
func TestCommitWorkspacePreStagedArtifactRefused(t *testing.T) {
	ws := t.TempDir()
	initTestRepo(t, ws)

	if err := os.WriteFile(filepath.Join(ws, "blob.bin"), make([]byte, commitFileMaxBytes+512*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "src.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The mutation: the agent pre-stages everything.
	addA, err := exec.Command("git", "-C", ws, "-c", "user.name=t",
		"-c", "user.email=t@t", "add", "-A").CombinedOutput()
	if err != nil {
		t.Fatalf("git add -A: %v: %s", err, addA)
	}

	head, _, refuse := commitWorkspace(ws)
	if head != "" {
		t.Fatalf("a pre-staged oversized artifact must refuse the commit; got head %q", head)
	}
	if refuse == "" || !strings.Contains(refuse, "blob.bin") {
		t.Fatalf("refusal must name the artifact; got %q", refuse)
	}
	// The index is clean again (the runner reset before refusing).
	if d, err := exec.Command("git", "-C", ws, "diff", "--cached", "--name-only").CombinedOutput(); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(d), "blob.bin") {
		t.Fatalf("the artifact is still staged after the refusal: %s", d)
	}
}

// TestCommitWorkspaceRenameNewBinaryRefused (I47): a rename whose NEW path
// is an oversized new file refuses the commit (the rename does not launder
// an artifact into a tracked path).
func TestCommitWorkspaceRenameNewBinaryRefused(t *testing.T) {
	ws := t.TempDir()
	initTestRepo(t, ws)

	// A tracked source file the agent "moves" to a big new name...
	if err := os.WriteFile(filepath.Join(ws, "oldname.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, ws, "add", "oldname.bin")
	gitIn(t, ws, "commit", "-q", "-m", "base2")
	// ...is actually a NEW oversized file at the new path (the old path
	// holds the original): the worktree has an untracked oversized file at
	// the rename destination.
	if err := os.WriteFile(filepath.Join(ws, "big.bin"), make([]byte, commitFileMaxBytes+1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(ws, "oldname.bin"),
		filepath.Join(ws, ".oldname-removed")); err != nil {
		t.Fatal(err)
	}

	head, _, refuse := commitWorkspace(ws)
	if head != "" {
		t.Fatalf("an oversized new file must refuse; got head %q", head)
	}
	if refuse == "" || !strings.Contains(refuse, "big.bin") {
		t.Fatalf("refusal must name the oversized new file; got %q", refuse)
	}
}

// TestCommitWorkspaceNonASCIIAndSpacePaths (I47): a non-ASCII path and a
// space-bearing path round-trip verbatim into the staged-paths list
// (core.quotepath=false + NUL separators on both the status read and the
// staged read).
func TestCommitWorkspaceNonASCIIAndSpacePaths(t *testing.T) {
	ws := t.TempDir()
	initTestRepo(t, ws)

	nonASCII := "röund.go"
	spaced := "spaced name.go"
	if err := os.WriteFile(filepath.Join(ws, nonASCII), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, spaced), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	head, staged, refuse := commitWorkspace(ws)
	if refuse != "" || head == "" {
		t.Fatalf("commit failed: head=%q refuse=%q", head, refuse)
	}
	found := map[string]bool{}
	for _, p := range staged {
		found[p] = true
	}
	if !found[nonASCII] {
		t.Fatalf("the non-ASCII path must be verbatim in the staged list (got %v)", staged)
	}
	if !found[spaced] {
		t.Fatalf("the space-bearing path must be verbatim in the staged list (got %v)", staged)
	}
	// The commit itself carries them verbatim (core.quotepath=false so the
	// non-ASCII name is not C-quoted in the tree listing).
	lsOut, err := exec.Command("git", "-C", ws, "-c", "core.quotepath=false",
		"ls-tree", "--name-only", "-r", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	ls := string(lsOut)
	if !strings.Contains(ls, nonASCII) || !strings.Contains(ls, spaced) {
		t.Fatalf("the commit tree must carry both paths verbatim:\n%s", ls)
	}
}
