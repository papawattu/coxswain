package controller

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// I73: the init-workspace script survives an interrupted first run.
//
// The bug: the init-workspace container clones spec.workspace.repo into the
// /workspace volume. The volume is a MOUNT POINT (cannot be rm -rf'd). A
// previous run may leave the workspace in any partial state — most
// dangerously an interrupted first run that already checked out files but
// left no success marker (.coxswain/base-commit), or a stale .git from an
// earlier clone. If the script wipes only .git and .coxswain (the old
// behavior), a restart's `git checkout --detach FETCH_HEAD` fails with
// 'untracked working tree files would be overwritten by checkout' and the
// Loop wedges forever (seen on the live I54 run). The fix wipes ALL contents
// of the mount (including dotfiles) so the clone always starts clean.
//
// This spec is an EXECUTION test (AGENTS.md norm: any shell script embedded
// in Go has an execution test that runs the real generated script, only path
// or host constants substituted). It builds the REAL init-workspace script
// (buildWorkspaceInitContainer), extracts the sh -c payload, substitutes the
// /workspace constant for a temp dir, runs it TWICE against a local bare
// repo: the first run leaves checked-out files + no success marker (an
// interrupted first run), the second run must succeed (a fresh clean clone).
// The mutation check (a scratch worktree with the old partial wipe) makes
// the second run fail with the untracked-files error.

// i73SetupBareRepo creates a local bare repo with one committed file, and
// records its HEAD SHA. The bare repo is the "remote" the script clones from
// (a file path — no network, no credential Secret needed). Package-level so
// the I77 execution spec reuses the same stand-in remote.
func i73SetupBareRepo(root string) (string, string, error) {
	bare := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("I73 seed\n"), 0o644); err != nil {
		return "", "", err
	}
	run := func(dir, args string) error {
		c := exec.Command("git", strings.Fields(args)...)
		c.Dir = dir
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=I73", "GIT_AUTHOR_EMAIL=i73@localhost",
			"GIT_COMMITTER_NAME=I73", "GIT_COMMITTER_EMAIL=i73@localhost")
		if out, e := c.CombinedOutput(); e != nil {
			return fmt.Errorf("git %s: %w: %s", args, e, out)
		}
		return nil
	}
	if e := run(seed, "init"); e != nil {
		return "", "", e
	}
	if e := run(seed, "add README.md"); e != nil {
		return "", "", e
	}
	if e := run(seed, "commit -m seed"); e != nil {
		return "", "", e
	}
	// Clone the seed repo into a bare repo at root (clone runs in root, not
	// seed, so the bare repo is root/origin.git, not seed/origin.git).
	if e := run(root, "clone --bare "+seed+" "+bare); e != nil {
		return "", "", e
	}
	// The HEAD SHA of the bare repo.
	c := exec.Command("git", "-C", bare, "rev-parse", "HEAD")
	headOut, err := c.CombinedOutput()
	if err != nil {
		return "", "", err
	}
	return bare, strings.TrimSpace(string(headOut)), nil
}

var _ = Describe("I73: init-workspace survives an interrupted first run (execution test)", func() {
	// i73BareRepo is a local bare repo the script clones from. Created once
	// per spec run (a file-backed git remote — no network).
	var i73BareRepo string
	var i73BareHead string

	// i73InitScript extracts the init-workspace container's sh -c payload from
	// a real Loop (the operator's buildWorkspaceInitContainer), with the repo
	// pointed at the local bare repo. Only the DEST + /dev/termination-log
	// path constants are substituted; the script body is the real generated
	// text (no hand re-implementation — a hand copy would drift from the
	// production script).
	i73InitScript := func(dest string) string {
		loop := &coxv1alpha1.Loop{
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "I73 execution test",
				Workspace: coxv1alpha1.Workspace{Repo: i73BareRepo},
			},
		}
		scheme := runtime.NewScheme()
		_ = coxv1alpha1.AddToScheme(scheme)
		_ = loop
		c := (&LoopReconciler{Scheme: scheme}).buildWorkspaceInitContainer(loop, "git-standin:latest", nil)
		// c.Command == [verifySh, "-c", script].
		if len(c.Command) < 3 || c.Command[1] != "-c" {
			GinkgoT().FailNow()
		}
		script := c.Command[2]
		// Substitute the DEST host constant (/workspace) for the temp dir. The
		// repo/ref host constants are already the local bare repo (shellQuote).
		script = strings.ReplaceAll(script, "DEST=/workspace", "DEST="+dest)
		// The script writes the resolved SHA to /dev/termination-log (the
		// operator's primary read-back, a kubelet thing). In this execution
		// test /dev/termination-log is not writable, so redirect that one line
		// to a file in the workspace (a pure path substitution — the rest of
		// the script, including the success-marker write, is untouched).
		script = strings.ReplaceAll(script, "> /dev/termination-log", "> "+dest+"/.termination-log-test")
		return script
	}

	// i73RunSh runs a script with sh in dir, capturing stdout+stderr. It
	// returns the combined output and the exit error.
	i73RunSh := func(dir, script string) (string, error) {
		cmd := exec.Command("sh", "-c", script)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// i73SimulateInterrupt clears the success marker + the .git dir, leaving
	// the checked-out files in the workspace (the state an interrupted first
	// run leaves: files present, but no .coxswain/base-commit and no .git —
	// the run was killed after checkout but before the marker write; or the
	// .git was removed by a previous partial wipe). This is the precondition
	// for the second run.
	i73SimulateInterrupt := func(dest string) error {
		if err := os.RemoveAll(filepath.Join(dest, ".coxswain")); err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(dest, ".git")); err != nil {
			return err
		}
		return nil
	}

	var (
		i73Work string
		i73Dest string
	)

	BeforeEach(func() {
		var err error
		i73Work, err = os.MkdirTemp("", "i73-initws-*")
		Expect(err).NotTo(HaveOccurred())
		i73Dest = filepath.Join(i73Work, "workspace")
		Expect(os.MkdirAll(i73Dest, 0o755)).To(Succeed())
		i73BareRepo, i73BareHead, err = i73SetupBareRepo(i73Work)
		Expect(err).NotTo(HaveOccurred())
		Expect(i73BareHead).To(MatchRegexp(`^[0-9a-f]{40}$`))
	})

	AfterEach(func() {
		_ = os.RemoveAll(i73Work)
	})

	It("the second run succeeds when the first run was interrupted (checked-out files, no success marker)", func() {
		// Extract the REAL init-workspace script for this local bare repo.
		script := i73InitScript(i73Dest)
		Expect(script).To(ContainSubstring("DEST=" + i73Dest))
		Expect(script).To(ContainSubstring(i73BareRepo))
		Expect(script).To(ContainSubstring("checkout --detach FETCH_HEAD"),
			"I73: the script must checkout the fetched ref (the real generated script)")

		// RUN 1: a clean first run. The clone succeeds; the success marker
		// (.coxswain/base-commit) holds the bare repo's HEAD.
		out1, err1 := i73RunSh(i73Work, script)
		Expect(err1).NotTo(HaveOccurred(), "I73: the first run must succeed (clean clone)\n%s", out1)
		sha1File := filepath.Join(i73Dest, ".coxswain", "base-commit")
		sha1Bytes, err := os.ReadFile(sha1File)
		Expect(err).NotTo(HaveOccurred(), "I73: the success marker must exist after run 1")
		Expect(strings.TrimSpace(string(sha1Bytes))).To(Equal(i73BareHead),
			"I73: the success marker must hold the bare repo's HEAD")
		// The workspace has checked-out files (README.md from the seed commit).
		_, err = os.Stat(filepath.Join(i73Dest, "README.md"))
		Expect(err).NotTo(HaveOccurred(), "I73: the checked-out file must exist after run 1")

		// Simulate an interrupted first run: the run was killed AFTER checkout
		// (files present) but BEFORE the success-marker write (no .coxswain),
		// or a previous partial wipe removed .git. Either way the workspace
		// has checked-out files and NO success marker.
		Expect(i73SimulateInterrupt(i73Dest)).To(Succeed())
		_, err = os.Stat(filepath.Join(i73Dest, ".coxswain"))
		Expect(os.IsNotExist(err)).To(BeTrue(), "I73: the success marker must be gone (interrupted)")
		_, err = os.Stat(filepath.Join(i73Dest, "README.md"))
		Expect(err).NotTo(HaveOccurred(), "I73: the checked-out file must still be present (interrupted mid-checkout)")

		// RUN 2: the restart. The script must wipe ALL workspace contents
		// (including the checked-out files) and re-clone cleanly. With the old
		// partial wipe (only .git + .coxswain) this run FAILS with 'untracked
		// working tree files would be overwritten by checkout' — the
		// checked-out README.md is still there when git checks out FETCH_HEAD.
		out2, err2 := i73RunSh(i73Work, script)
		Expect(err2).NotTo(HaveOccurred(),
			"I73: the second run (restart after an interrupted first run) must succeed; "+
				"if it failed, the workspace wipe is partial (checked-out files survive the wipe)\n%s", out2)
		Expect(out2).NotTo(ContainSubstring("untracked working tree files would be overwritten"),
			"I73: the restart must not hit the untracked-files checkout error")
		// The success marker holds the bare repo's HEAD again.
		sha2Bytes, err := os.ReadFile(sha1File)
		Expect(err).NotTo(HaveOccurred(), "I73: the success marker must exist after run 2")
		Expect(strings.TrimSpace(string(sha2Bytes))).To(Equal(i73BareHead))
	})
})
