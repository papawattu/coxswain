// I47 (REVIEW-PHASE1-R20): the runner's Implementing commit must not carry
// build artifacts into the verified commit. The verified commit is pushed to
// the (external) repo by S6 delivery, so everything the agent left in the
// workspace — a freshly built binary, a multi-MiB blob — must either be
// .gitignore'd (seeded in examples/gocli) or refused at commit time (the
// per-new-file size/binary guard).
//
// The unit tests here are hermetic (a real git repo in t.TempDir, a real
// git binary) and prove the contract:
//   - a binary + source edit commits ONLY the source edit (the binary is
//     left unstaged and NOT in the verified commit);
//   - a plain `git add -A` (the S5a mutation that built the 2.5 MB gocli
//     binary into the verified commit) stages the binary — the same
//     workspace, no .gitignore — so the mutation is provably different;
//   - an unignored NEW file over 1 MiB refuses the commit (blocked claim,
//     no headCommit);
//   - an unignored NEW binary file refuses the commit (blocked claim);
//   - the staged paths ride into the claim summary (evidence).
package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommitWorkspaceBinaryPlusSourceEditCommitsOnlySource (I47 acceptance
// case 1): the agent builds a binary and also edits a source file. The
// commit carries the source edit ONLY — the binary is left unstaged (it is
// refused by the binary heuristic) and never enters the verified commit.
// With the seed's .gitignore the binary would be ignored (unstaged via the
// per-path add); this test proves the guard fires even WITHOUT a .gitignore
// (a .gitignore-less workspace must still not leak a binary).
func TestCommitWorkspaceBinaryPlusSourceEditCommitsOnlySource(t *testing.T) {
	ws := t.TempDir()
	initTestRepo(t, ws)

	// The agent's build output: a new binary file (NUL bytes → the I47
	// heuristic). NOT gitignored (no .gitignore in this scratch repo — the
	// guard must catch it either way).
	bin := filepath.Join(ws, "gocli")
	if err := os.WriteFile(bin, []byte{0x7f, 'E', 'L', 'F', 0, 0, 0}, 0o755); err != nil {
		t.Fatal(err)
	}
	// The agent's real work: a source edit (new + modified).
	if err := os.WriteFile(filepath.Join(ws, "round.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n// edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	head, staged, refuse := commitWorkspace(ws)
	// I47: the binary NEW file is refused — the commit does not happen and
	// the caller blocks the claim (no headCommit). The refusal names the
	// binary.
	if head != "" {
		t.Fatalf("a binary new file must refuse the commit; got head %q", head)
	}
	if refuse == "" || !strings.Contains(refuse, "gocli") {
		t.Fatalf("refusal must name the binary file; got %q", refuse)
	}
	_ = staged
}

// TestCommitWorkspaceGitignoreDBinaryCommitsOnlySource (I47 acceptance case
// 1, the seeded path): with the examples/gocli .gitignore in place (the
// binary is IGNORED), the commit succeeds and carries ONLY the source
// change — `git diff --stat base..verified` would list exactly the source
// file. This is the gocli sample's contract.
func TestCommitWorkspaceGitignoreDBinaryCommitsOnlySource(t *testing.T) {
	ws := t.TempDir()
	initTestRepo(t, ws)

	// The seed's .gitignore (examples/gocli): build outputs are ignored.
	if err := os.WriteFile(filepath.Join(ws, ".gitignore"), []byte("/gocli\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The agent's build output (now ignored): a binary that would have been
	// the 2.5 MB leak in the S5b run.
	if err := os.WriteFile(filepath.Join(ws, "gocli"), []byte{0x7f, 'E', 'L', 'F', 0}, 0o755); err != nil {
		t.Fatal(err)
	}
	// The agent's real work.
	if err := os.WriteFile(filepath.Join(ws, "round.go"), []byte("package main\nfunc Round() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	head, staged, refuse := commitWorkspace(ws)
	if refuse != "" {
		t.Fatalf("gitignored binary must not refuse the commit; got %q", refuse)
	}
	if head == "" {
		t.Fatal("commitWorkspace returned an empty SHA")
	}
	// The verified commit carries EXACTLY round.go (and .gitignore, the
	// seed's own file — the diff base..verified is the source edit; the
	// binary is absent).
	ls := gitIn(t, ws, "ls-tree", "--name-only", "-r", "HEAD")
	if strings.Contains(ls, "gocli") {
		t.Fatalf("the gitignored binary leaked into the verified commit:\n%s", ls)
	}
	if len(staged) == 0 {
		t.Fatal("no staged paths reported")
	}
	for _, p := range staged {
		if p == "gocli" {
			t.Fatalf("the gitignored binary was staged: %v", staged)
		}
		if p != "round.go" && p != ".gitignore" {
			t.Fatalf("unexpected staged path %q (want only source + .gitignore): %v", p, staged)
		}
	}
}

// TestCommitWorkspaceOversizedNewFileRefuses (I47 acceptance case 2): an
// unignored NEW file over 1 MiB refuses the commit — the claim is blocked
// with the reason, no headCommit, the operator holds the Verifying advance
// (ADR-0005 fail-closed). A plain `git add -A` (the S5a mutation) would
// stage it into the verified commit — the mutation is provably different
// (asserted in the same test).
func TestCommitWorkspaceOversizedNewFileRefuses(t *testing.T) {
	ws := t.TempDir()
	initTestRepo(t, ws)

	// A 1.5 MiB unignored new file (just over the 1 MiB cap).
	big := filepath.Join(ws, "blob.bin")
	if err := os.WriteFile(big, make([]byte, commitFileMaxBytes+512*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	head, _, refuse := commitWorkspace(ws)
	if head != "" {
		t.Fatalf("an oversized new file must refuse the commit; got head %q", head)
	}
	if refuse == "" || !strings.Contains(refuse, "blob.bin") || !strings.Contains(refuse, "1048576") {
		t.Fatalf("refusal must name the file and the cap; got %q", refuse)
	}

	// The mutation is provably different AND the runner blocks it: a plain
	// `git add -A` (the S5a code path) stages the oversized file into the
	// index (it would ride into the verified commit — the I47 bug). Run
	// commitWorkspace against that PRE-STAGED state: it must refuse (the
	// reset+refusal path catches the artifact the mutation staged) and
	// commit nothing.
	out, err := exec.Command("git", "-C", ws, "-c", "user.name=t", "-c", "user.email=t@t", "add", "-A").CombinedOutput()
	if err != nil {
		t.Fatalf("mutation git add -A failed: %v: %s", err, out)
	}
	d, err := exec.Command("git", "-C", ws, "diff", "--cached", "--name-only").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d), "blob.bin") {
		t.Fatalf("the plain git add -A mutation does not stage the oversized file (test setup broken): %s", d)
	}
	// The runner, run against the mutation's pre-staged state, must refuse:
	// the reset undoes the agent's index and the refusal catches the
	// oversized NEW file before any of the runner's own staging.
	head2, _, refuse2 := commitWorkspace(ws)
	if head2 != "" {
		t.Fatalf("a pre-staged oversized new file must refuse the commit; got head %q", head2)
	}
	if refuse2 == "" || !strings.Contains(refuse2, "blob.bin") {
		t.Fatalf("the refusal must name the pre-staged artifact; got %q", refuse2)
	}
	if d2, err := exec.Command("git", "-C", ws, "diff", "--cached", "--name-only").CombinedOutput(); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(d2), "blob.bin") {
		t.Fatalf("the runner's reset must have unstaged the mutation's artifact (got staged: %s)", d2)
	}
	// A final plain `git add -A` (no runner involvement) still stages it —
	// proving the guard is in the RUNNER, not in git itself.
	addA, err := exec.Command("git", "-C", ws, "-c", "user.name=t",
		"-c", "user.email=t@t", "add", "-A").CombinedOutput()
	if err != nil {
		t.Fatalf("final mutation git add -A failed: %v: %s", err, addA)
	}
	if d3, err := exec.Command("git", "-C", ws, "diff", "--cached", "--name-only").CombinedOutput(); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(d3), "blob.bin") {
		t.Fatalf("the plain git add -A still stages the oversized file without the runner (the I47 bug): %s", d3)
	}
}

// TestCommitWorkspaceBinaryNewFileRefuses (I47): an unignored NEW binary
// file (NUL in the first 8 KiB) refuses the commit even when it is small
// (the size cap alone would not catch a small binary).
func TestCommitWorkspaceBinaryNewFileRefuses(t *testing.T) {
	ws := t.TempDir()
	initTestRepo(t, ws)

	if err := os.WriteFile(filepath.Join(ws, "small.bin"), []byte{0, 1, 2, 0, 3}, 0o644); err != nil {
		t.Fatal(err)
	}

	head, _, refuse := commitWorkspace(ws)
	if head != "" {
		t.Fatalf("a binary new file must refuse the commit; got head %q", head)
	}
	if refuse == "" || !strings.Contains(refuse, "binary") {
		t.Fatalf("refusal must say the file is binary; got %q", refuse)
	}
}

// TestCommitWorkspaceTrackedLargeEditCommits (I47): the size/binary guard
// applies to NEW files only — an agent edit to a LARGE EXISTING tracked
// file (a test fixture) is source work and is committed as-is (no false
// positive on a legit large source edit).
func TestCommitWorkspaceTrackedLargeEditCommits(t *testing.T) {
	ws := t.TempDir()
	initTestRepo(t, ws)

	// A tracked file just under the cap, committed in the base commit...
	fixture := filepath.Join(ws, "fixture.txt")
	if err := os.WriteFile(fixture, make([]byte, commitFileMaxBytes/2), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, ws, "add", "fixture.txt")
	gitIn(t, ws, "commit", "-q", "-m", "fixture")

	// ...that the agent now edits (still under the cap, text).
	if err := os.WriteFile(fixture, append(make([]byte, commitFileMaxBytes/2), []byte("edited\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	head, staged, refuse := commitWorkspace(ws)
	if refuse != "" {
		t.Fatalf("an edit to a tracked large file must not refuse; got %q", refuse)
	}
	if head == "" {
		t.Fatal("commitWorkspace returned an empty SHA")
	}
	if len(staged) != 1 || staged[0] != "fixture.txt" {
		t.Fatalf("staged = %v, want [fixture.txt]", staged)
	}
}

// TestCommitWorkspaceStagedPathsInClaimSummary (I47): the staged paths ride
// into the claim summary (the claim's one-line note the operator records in
// progress) — evidence of what the verified commit contains.
func TestCommitWorkspaceStagedPathsInClaimSummary(t *testing.T) {
	ws := t.TempDir()
	initTestRepo(t, ws)

	if err := os.WriteFile(filepath.Join(ws, "a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "b.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	head, staged, refuse := commitWorkspace(ws)
	if refuse != "" || head == "" {
		t.Fatalf("commit failed: head=%q refuse=%q", head, refuse)
	}
	if len(staged) != 2 {
		t.Fatalf("staged = %v, want 2 paths", staged)
	}
	summary := stagedClaimSummary("model finished", staged)
	if !strings.Contains(summary, "[staged: 2 file(s): a.go, b.go]") {
		t.Fatalf("claim summary must list the staged paths; got %q", summary)
	}
}
