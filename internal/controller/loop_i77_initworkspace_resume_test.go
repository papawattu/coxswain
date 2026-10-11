package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// I77 (R24, option (a)): the init-workspace script does not wipe agent work
// on a pod restart of the pod that still carries it.
//
// The bug: init-workspace re-runs whenever the pod that still carries it
// restarts (the operator has not rebuilt the pod without init-workspace yet).
// If that happens after the agent has started writing to the workspace, the
// wipe (introduced by I73 to make an interrupted FIRST run re-runnable)
// removes the agent's work — commits and uncommitted files.
//
// The fix: BEFORE the wipe, the script checks the success marker
// .coxswain/base-commit. The marker is written only AFTER checkout, so if it
// holds a 40-hex SHA that 'git cat-file -e <sha>^{commit}' accepts, the
// workspace holds a COMPLETED clone and the script skips the wipe and the
// clone: it writes the marker SHA to /dev/termination-log, echoes 'workspace
// already initialised at <sha>' and exits 0. An interrupted first run (I73:
// checked-out files, no marker) still gets the full wipe.
//
// This spec is an EXECUTION test (AGENTS.md norm: any shell script embedded
// in Go has an execution test that runs the real generated script, only path
// or host constants substituted — the I73 spec's setup is reused). It builds
// the REAL init-workspace script (buildWorkspaceInitContainer), substitutes
// the /workspace and /dev/termination-log path constants, and runs it TWICE
// against a local bare repo: run 1 clones; an agent commit + a dirty
// untracked file are added to the workspace; run 2 must leave both intact,
// with the termination log holding the ORIGINAL base SHA (not the agent
// commit's SHA). The mutation check (a scratch worktree with the skip block
// deleted) makes run 2 wipe the workspace (the agent commit + dirty file are
// gone and run 2 re-clones to the bare repo's HEAD).

var _ = Describe("I77: init-workspace skips a completed clone and keeps agent work (execution test)", func() {
	var (
		i77BareRepo string
		i77BareHead string
		i77Work     string
		i77Dest     string
	)

	// i77InitScript extracts the init-workspace container's sh -c payload from
	// a real Loop (the operator's buildWorkspaceInitContainer), with the repo
	// pointed at the local bare repo. Only the DEST + /dev/termination-log
	// path constants are substituted; the script body is the real generated
	// text (no hand re-implementation — a hand copy would drift from the
	// production script).
	i77InitScript := func(dest string) string {
		loop := &coxv1alpha1.Loop{
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "I77 execution test",
				Workspace: coxv1alpha1.Workspace{Repo: i77BareRepo},
			},
		}
		scheme := runtime.NewScheme()
		_ = coxv1alpha1.AddToScheme(scheme)
		c := (&LoopReconciler{Scheme: scheme}).buildWorkspaceInitContainer(loop, "git-standin:latest", nil)
		// c.Command == [verifySh, "-c", script].
		if len(c.Command) < 3 || c.Command[1] != "-c" {
			GinkgoT().FailNow()
		}
		script := c.Command[2]
		script = strings.ReplaceAll(script, "DEST=/workspace", "DEST="+dest)
		// The script writes the resolved SHA to /dev/termination-log (the
		// operator's primary read-back, a kubelet thing). In this execution
		// test /dev/termination-log is not writable, so redirect that write to
		// a file in the workspace parent (a pure path substitution — the rest
		// of the script, including the skip block's own termination-log write,
		// is untouched).
		script = strings.ReplaceAll(script, "> /dev/termination-log", "> "+dest+"/.termination-log-test")
		return script
	}

	i77RunSh := func(dir, script string) (string, error) {
		cmd := exec.Command("sh", "-c", script)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// i77Git runs a git command in dir with the test identity.
	i77Git := func(dir string, args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=I77agent", "GIT_AUTHOR_EMAIL=i77agent@localhost",
			"GIT_COMMITTER_NAME=I77agent", "GIT_COMMITTER_EMAIL=i77agent@localhost")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	It("a second run on a workspace holding agent commits leaves them intact (no wipe, no re-clone)", func() {
		var err error
		i77Work, err = os.MkdirTemp("", "i77-initws-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(i77Work) }() //nolint:errcheck // best-effort cleanup
		i77Dest = filepath.Join(i77Work, "workspace")
		Expect(os.MkdirAll(i77Dest, 0o755)).To(Succeed())
		i77BareRepo, i77BareHead, err = i73SetupBareRepo(i77Work)
		Expect(err).NotTo(HaveOccurred())
		Expect(i77BareHead).To(MatchRegexp(`^[0-9a-f]{40}$`))

		script := i77InitScript(i77Dest)
		Expect(script).To(ContainSubstring("DEST=" + i77Dest))
		Expect(script).To(ContainSubstring(i77BareRepo))

		// RUN 1: a clean first run. The clone succeeds; the success marker
		// holds the bare repo's HEAD.
		out1, err := i77RunSh(i77Work, script)
		Expect(err).NotTo(HaveOccurred(), "I77: the first run must succeed (clean clone)\n%s", out1)
		Expect(out1).To(ContainSubstring("workspace initialised at " + i77BareHead))
		marker := filepath.Join(i77Dest, ".coxswain", "base-commit")
		mb, err := os.ReadFile(marker)
		Expect(err).NotTo(HaveOccurred(), "I77: the success marker must exist after run 1")
		Expect(strings.TrimSpace(string(mb))).To(Equal(i77BareHead))

		// The agent now works in the workspace: one COMMIT (the agent
		// iteration's work) plus a DIRTY untracked file (work in flight).
		_, err = i77Git(i77Dest, "commit", "-m", "agent change", "--allow-empty")
		Expect(err).NotTo(HaveOccurred(), "I77: the agent commit must succeed")
		agentSHA, err := i77Git(i77Dest, "rev-parse", "HEAD")
		agentSHA = strings.TrimSpace(agentSHA)
		Expect(err).NotTo(HaveOccurred())
		Expect(agentSHA).NotTo(Equal(i77BareHead), "I77: the agent commit must move HEAD")
		Expect(os.WriteFile(filepath.Join(i77Dest, "dirty.txt"), []byte("work in flight\n"), 0o644)).To(Succeed())

		// RUN 2: a pod restart re-runs init-workspace on the SAME volume.
		// The success marker holds the ORIGINAL base SHA (run 1's checkout
		// SHA — the agent commit is not in the remote, but the marker still
		// exists in the workspace's .git). The script must SKIP: no wipe,
		// no clone, termination log = the original base SHA, exit 0.
		out2, err := i77RunSh(i77Work, script)
		Expect(err).NotTo(HaveOccurred(), "I77: the second run (restart after agent work) must exit 0\n%s", out2)
		Expect(out2).To(ContainSubstring("workspace already initialised at "+i77BareHead),
			"I77: the second run must take the skip branch, not re-clone\n%s", out2)

		// The agent commit is intact (HEAD still the agent commit, not the
		// re-cloned base).
		head, err := i77Git(i77Dest, "rev-parse", "HEAD")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(head)).To(Equal(agentSHA),
			"I77: the agent commit must survive the restart (no re-clone to the base)")
		// The dirty untracked file is intact.
		_, err = os.Stat(filepath.Join(i77Dest, "dirty.txt"))
		Expect(err).NotTo(HaveOccurred(), "I77: the dirty untracked file must survive the restart (no wipe)")

		// The termination log holds the ORIGINAL base SHA (the skip branch
		// writes the marker value, not the current HEAD).
		tl, err := os.ReadFile(filepath.Join(i77Dest, ".termination-log-test"))
		Expect(err).NotTo(HaveOccurred(), "I77: the termination log must be written by the skip branch")
		Expect(strings.TrimSpace(string(tl))).To(Equal(i77BareHead),
			"I77: the termination log must hold the original base SHA (not the agent commit's SHA)")

		// The success marker is untouched (still the original base SHA).
		mb2, err := os.ReadFile(marker)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(string(mb2))).To(Equal(i77BareHead))
	})
})

var _ = Describe("I77: interrupted first run (no success marker) still gets the wipe", func() {
	var (
		i77BareRepo string
		i77BareHead string
		i77Work     string
		i77Dest     string
	)

	i77InitScript := func(dest string) string {
		loop := &coxv1alpha1.Loop{
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "I77 execution test (interrupted)",
				Workspace: coxv1alpha1.Workspace{Repo: i77BareRepo},
			},
		}
		scheme := runtime.NewScheme()
		_ = coxv1alpha1.AddToScheme(scheme)
		c := (&LoopReconciler{Scheme: scheme}).buildWorkspaceInitContainer(loop, "git-standin:latest", nil)
		if len(c.Command) < 3 || c.Command[1] != "-c" {
			GinkgoT().FailNow()
		}
		script := c.Command[2]
		script = strings.ReplaceAll(script, "DEST=/workspace", "DEST="+dest)
		script = strings.ReplaceAll(script, "> /dev/termination-log", "> "+dest+"/.termination-log-test")
		return script
	}

	i77RunSh := func(dir, script string) (string, error) {
		cmd := exec.Command("sh", "-c", script)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	It("a workspace with checked-out files but NO success marker is wiped and re-cloned", func() {
		var err error
		i77Work, err = os.MkdirTemp("", "i77-interrupted-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(i77Work) }() //nolint:errcheck // best-effort cleanup
		i77Dest = filepath.Join(i77Work, "workspace")
		Expect(os.MkdirAll(i77Dest, 0o755)).To(Succeed())
		i77BareRepo, i77BareHead, err = i73SetupBareRepo(i77Work)
		Expect(err).NotTo(HaveOccurred())
		Expect(i77BareHead).To(MatchRegexp(`^[0-9a-f]{40}$`))

		script := i77InitScript(i77Dest)

		// RUN 1: a clean first run (leaves checked-out files + the marker).
		out1, err := i77RunSh(i77Work, script)
		Expect(err).NotTo(HaveOccurred(), "I77: the first run must succeed\n%s", out1)

		// Simulate an interrupted first run the way I73 does: remove the
		// marker AND .git, leaving the checked-out files.
		Expect(os.RemoveAll(filepath.Join(i77Dest, ".coxswain"))).To(Succeed())
		Expect(os.RemoveAll(filepath.Join(i77Dest, ".git"))).To(Succeed())

		// RUN 2: no valid success marker -> the full wipe + re-clone must
		// happen (the I73 behaviour; the I77 skip branch must NOT fire).
		out2, err := i77RunSh(i77Work, script)
		Expect(err).NotTo(HaveOccurred(),
			"I77: the interrupted-first-run restart must still re-clone cleanly\n%s", out2)
		Expect(out2).To(ContainSubstring("workspace initialised at "+i77BareHead),
			"I77: the skip branch must not fire without a success marker\n%s", out2)
		mb, err := os.ReadFile(filepath.Join(i77Dest, ".coxswain", "base-commit"))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(string(mb))).To(Equal(i77BareHead))
	})
})
