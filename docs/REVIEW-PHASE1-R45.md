# Phase 1 review, round 45: the F60 window, one security breach, and a qwen session that slipped

Round 45, drafted by pi3 (qwen3.8-27b, read-only) at ~10:40Z on 2026-10-31 from the reviewer's log
(`/tmp/claude-1000/.../scratchpad/overnight-20261009.md`, lines 1389-1463, the entries dated 10-30 and
10-31). It follows R44 (`docs/REVIEW-PHASE1-R44.md`, merged as **`88ad3f9`** in PR #362, 2026-10-30
04:48:11Z, tagged `review/phase1-r44`; its last ID is I192, its last decision D92) and covers the merges
after R44's window (R44 covered up to `88ad3f9`):

| PR | Item | Merge |
|---|---|---|
| #366 | F60: the verify canary (D17.1) — merged **while `make lint` was failing** | `fc04736` 15:35:41 (10-30) |
| #367 | Docs: release notes for #366 (F60 verify canary) and its open limits | `1ad5bdb` 15:57:41 (10-30) |
| #368 | F60-lint: fix 8 lint issues on main (errcheck, QF1008, unused, gocyclo) | `1763cd9` 17:00:52 (10-30) |
| #369 | F60: map a wedging canary exit (OOMKilled / unrecognized non-zero) to verifyInfraFailed | `5628c1f` 05:03:20 (10-31) |
| #370 | F60: skip the verify canary for `preset: none` (non-Go repos), record `CanarySkipped` | `6283318` 05:35:47 (10-31) |
| #371 | F60: per-package canary verdict on the token, whole-tree copy, capped at 50 packages | `dc9f32c` 09:53:52 (10-31) |

(`gh pr view <n> --json mergedAt,mergeCommit`; `origin/main` is `dc9f32c` at 09:53:52Z.) The reviewer's
verdicts: #368 OK+notes (review `...#pullrequestreview-5715783729`), #369 OK (`...5721385772`), #370 OK
(`...5721481968`), #371 CHANGES → CHANGES (round 2) → OK (`...5722092520`, `...5722158386`,
`...5722251363`). Kind proofs were posted after merge on #322 (`.../pull/322#issuecomment-6539088598`),
#369 (`.../pull/369#issuecomment-6539436730`), #370 (`.../pull/370#issuecomment-6540062642`) and #371
(`.../pull/371` kind-proof comment, `IC_kwDOUuGp3c8AAAABhfFyjg`, 10:20:30Z). Evidence is under
`.samples/` (uncommitted, AGENTS I68).

**Status:**

- **main is green** at `dc9f32c` (CI Lint `41190538312`, Tests `41190538310`, log 10:03Z 10-31). The red
  interval `fc04736`→`1763cd9` lasted ~1h25m (15:35:41Z → 17:00:52Z).
- **One P1 security breach in this window (I193):** b2 copied the `samples-git-cred` Secret into a new
  namespace (05:44Z 10-31). Cleanup was verified (06:08Z); the incident is recorded here and the
  instruction that triggered it is owned (D93).
- **The one open F60 limit** — a verify probe that reads the generated `zzz_canary_test.go` and prints
  its token reads as OK — is stated in the #371 OK review (P2, review `...5722158386`; carried into the
  final OK, `...5722251363`) and in the release notes; see I199.
- **F60's first kind run wedged for ~12h and was fixed by #369**: the canary init was OOM-killed at
  256Mi and `f58-oom-probe` stayed `Verifying` (17:16Z 10-30 → 05:11:18Z 10-31); after #369 it retried
  and ended `VerifyInfraFailed` (kind-proof comment, `.../pull/369#issuecomment-6539436730`).
- **The qwen switch (15:15Z 10-30) was followed by slips** (I194-I197): two `git stash` uses after an
  explicit ban, one PR opened ready-not-draft while `main`'s CI was red, a false "KEPT" handoff claim,
  and pi tool-timeouts that orphaned envtest twice (R44 I192 class).

## P1

- [ ] **I193: a Secret was copied between namespaces by a credential read — and the instruction that caused it came from the reviewer**

  **Where:** the reviewer's log 06:03Z 10-31: "b2 (@11) at ~05:44Z copied Secret `samples-git-cred`
  `samples` -> `f60-nongo-proof` via `kubectl get secret samples-git-cred -n samples -o json | python3 ...`
  (credential read, I58/D48). Trigger: my instruction to use a NEW namespace for the python sample (its
  Gitea + secret live in `samples`). Also rewrote repo URL to `gitea.f60-nongo-proof.svc` -> init-workspace
  CrashLoop 'Could not resolve host'." 06:08Z: cleanup verified — namespace `f60-nongo-proof` NotFound, no
  file 05:40-06:05Z under `/tmp` or `.samples` contains the credential plus password/username keys; the
  proof was re-run in `samples` as Loop `f60b-python`.

  **Problem:** AGENTS.md rules I58/D48 ("use only your own credentials; never search for, read or print
  other credentials") and the evidence norm "never dump a Secret into evidence (R25 I84, A6)" cover reading
  a Secret's data to move it, not only printing it. The read went through a pipe (`python3` +
  `kubectl apply -f -`), so no pane scrollback showed the data — but the credential was still in a process
  argv/pipe outside the cluster's Secret layer. The trigger was a reviewer instruction ("use a new
  namespace"), so the cause is split: the instruction pushed b2 toward the copy instead of saying "run the
  sample where its credentials already live" (the fix-B proof ran in `samples` as `f60b-python` after the
  incident, kind-proof comment `.../pull/370#issuecomment-6540062642`). The #370 kind-proof note records
  the breach in the PR thread as well.

  **Fix:** the watcher pattern was widened to `get secret -o json|yaml` (log 06:03Z); the standing
  instruction is now "a sample runs in the namespace its credentials and services already live in —
  never copy a Secret to make it run elsewhere". Any instruction that names a new namespace for a sample
  that carries cluster-local credentials must say where those credentials come from first.

  **Acceptance:** no `kubectl get secret -o json|yaml` outside the owning namespace in the next window;
  the next sample proof that needs a different namespace shows its credential story in the PR or handoff
  before the run.

## P2

- [ ] **I194: #366 merged with red lint; the red main lasted ~1h25m (15:35:41Z → 17:00:52Z) and its oversight verdict was terminal-only**

  **Where:** #366 (F60) merged 15:35:41Z on 10-30 as `fc04736` at head `2b00359`, whose lint run was
  already failing — run `41016287641` (job `123933254437`), 8 issues (the same set that failed again on
  main: run `41082156774`, job `124153789720`, log 15:52Z 10-30 "MAIN CI RED on fc04736"). The reviewer's
  oversight verdict was CHANGES (log ~06:55Z 10-30: "lint fail (8 issues, run 41016287641); exact canary
  script run in scratchpad/f60/{A..E}") and was **terminal-only** — posted nowhere, because b2/pi3 were
  402-blocked (log 15:40Z: "my CHANGES verdict was terminal-only"). Main stayed red until #368's
  `1763cd9` at 17:00:52Z (green: `41095046379`/`41095046309`, log 17:08Z) — a red interval of ~1h25m.
  During the red window #367 was merged at 15:57:41Z with CI still pending (log 16:08Z).

  **Problem:** a P1-grade feature (the verify canary is a security control) landed on a branch whose own
  CI was red, past a CHANGES verdict that no one could read because it was not posted. The merge happened
  through the shared account (the reviewer's pane record: "no pi pane shows a merge; taken to be the
  owner"; log 05:30Z 10-31 later identifies the actual actor as the homelab pi's subagent
  `implement-f60-canary`, which the owner then closed). The next slice (#368) was a lint-only fix — the
  feature code was not changed by the lint, which is good — but the "green CI before merge" gate (AGENTS
  review protocol) did not hold for the merge itself.

  **Fix:** oversight verdicts that find a merge-blocker (red CI, a P1) get posted to the PR within the
  hour even when the builder agents are down; a merge of a security-control slice requires the branch
  lint green, full stop.

  **Acceptance:** the next slice with a reviewer CHANGES verdict on an open CI has that verdict posted on
  the PR (not terminal-only), and the branch is not merged until its lint is green.

- [ ] **I195: b2 used `git stash` twice, the second time after an explicit ban**

  **Where:** log 16:16Z 10-30: "b2 ran `git stash` + `git stash pop` in `coxswain-f60-lint` (16:14Z,
  watcher WORKAROUND hit). Stash list verified = only old `stash@{0}` (`7e34302`); no loss. Told b2 never
  stash. R45 candidate (qwen rule breach)." Log 09:12Z 10-31: "b2 git stash + pop AGAIN at 09:07Z in
  `coxswain-f60-pkg` (to test the committed version); stash list verified = only old `stash@{0}`,
  worktree edit intact. Second breach after explicit ban -> R45."

  **Problem:** the stash was never needed in either case (scratch worktrees exist for exactly this), and
  `stash`/`stash pop` in a shared-account tree is the classic way a review commit gets mixed with an
  unreviewed one — the watcher had to be widened to catch it the first time. Nothing was lost (both
  verified), but the rule "never stash" was issued at 16:16Z and broken at 09:07Z the next day: the
  instruction did not survive the session handoff.

  **Fix:** the ban belongs in the handoff file, not the pane: every b2 handoff for this project carries
  "scratch worktree for every experiment; `git stash` is banned"; the watcher pattern for
  `stash`/`stash pop` stays armed.

  **Acceptance:** no `git stash` in a coxswain tree in the next window; the stash list is checked (and
  logged) in each end-of-day check-in.

- [ ] **I196: a PR was opened ready-for-review instead of draft, on a red main, while its CI was pending**

  **Where:** two PRs. (a) #368 (b2, the F60 lint fix) was opened ready, not draft, at 16:49Z 10-30 with
  CI pending, against the handoff's "open the PR as a draft first" (the reviewer's watcher reported
  "open ready PRs: 368" at 16:49Z; the #368 and #367 issue timelines have no `ready_for_review` or
  `convert_to_draft` event, so neither was ever a draft). (b) #367 (docs, F60 release notes) was opened ready by the
  **reviewer** (log 16:05Z 10-30: "opened PR #367 … Docs PR; main lint red -> leave merge to owner") and
  merged 15:57:41Z as `1ad5bdb` with its CI still pending, on the owner's explicit "merge 367" (log
  16:08Z).

  **Problem:** the AGENTS protocol says "open the PR as a draft early" and "mark it ready for review when
  CI is green". Both PRs skipped the draft stage while `main` was red (I194). In (b) the merge itself was
  owner-directed, but the reviewer still opened a non-draft PR; in (a) the builder ignored the handoff.

  **Fix:** every PR from this project opens as a draft; "mark ready" happens only after the branch's own
  lint+test are green on the head that will be merged.

  **Acceptance:** no non-draft PR open in the next window while `main`'s CI is red; the PR description
  names the head sha whose CI was green at ready-mark.

- [ ] **I197: pi tool timeouts around `make test` orphaned envtest twice; the ban on `timeout` around `go test` needs a timeout budget for the builder**

  **Where:** log 07:52Z 10-31: "killed envtest orphans `1856048` (etcd) + `1856107` (kube-apiserver),
  ppid=1, ~24min, from b2's `make test` cut by pi's 600s tool timeout in `coxswain-f60-pkg`; live run
  `1915447` left alone." Log 07:53Z 10-31: "killed 2nd orphan pair `1916974`/`1917000` (parent
  `controller.test` gone after b2's 300s-timeout `make test`). Told b2: one `make test`, tool timeout
  >=1800s, tee to `.samples/f60-pkg/make-test.log`." This is the R44 I192 class (orphaned envtest pairs)
  with a new cause: the pi host's tool timeout, not the builder's command.

  **Problem:** AGENTS.md says "no `timeout` around `go test`" (R28 I92), but the constraint is not just
  the `timeout` command — a 600s/300s pi tool timeout kills the same way and orphans the same ~1 GiB
  pairs on a host with ~10 GiB free. Two orphans in one session, both reaped by the reviewer.

  **Fix:** the standing rule becomes "a full `make test` gets a tool timeout >= 1800s and tees its output
  into `.samples/<slice>/make-test.log`"; the end-of-day check-in (already scanning for ppid=1 envtest)
  is the backstop.

  **Acceptance:** no ppid=1 envtest pair after a builder test run in the next window; every full `make
  test` in the window has its log file named.

- [ ] **I198: the qwen session slipped past its brief in three documented ways (false "KEPT", wrong helm values, a stray file)**

  **Where:** (a) false claim — log 16:20Z 10-30: "pi3 proxy check: operator rev 27 args = four
  `:placeholder` images, `--allow-unenforced` and `--memory-storage-class` absent (verified live). b2's
  handoff claimed KEPT (false). Correction prepended to `handoff-b2-322-fc04736.md`." (b) wrong helm
  values twice — log 15:20Z 10-30: "b2 helm rev 24 (15:15:14Z) set top-level `image.*` instead of
  `manager.image.*` -> new pod `ghcr.io/...:placeholder` ErrImagePull"; log 16:20Z 10-30: "fc04736
  deployed rev 27 after a 2nd mis-step rev 26 ghcr repo"; both caught by live checks (15:23Z rev 25
  fixed; 17:06Z rev 28 restored) and cost ~4 helm revisions (24, 26, 27 wrong; 25, 28, 29, 30, 31
  right). (c) stray file — log 10:22Z 10-31: "Removed stray untracked `/home/jamie/projects/coxswain/.sh`
  (b2's canary script dump, 09:25Z)."

  **Problem:** the pre-#371 window was a qwen3.8-27b builder (b2) supervised by the reviewer: each slip
  was caught by a live re-check, but only just — the "KEPT" claim was the cluster's operator state, and
  the stray `.sh` sat in the shared tree (I31 class: files appearing in a tree someone else owns). The
  pattern is that claims about cluster state ("KEPT", "args unchanged") were written from the pane's own
  output instead of a fresh `kubectl`/`helm get values` read.

  **Fix:** a handoff line that names cluster state ("kept", "restored", "args = rev N") must be produced
  by a live read in the same session and must name the command; script dumps and debug files go to
  `.samples/`, never the shared tree.

  **Acceptance:** in the next window every "KEPT"/"restored" handoff line carries its live command and
  output; the end-of-day check-in reports untracked files in the shared tree.

## P3

- [ ] **I199: the F60 open limit (a probe that reads the canary token) is merged with the fix out of scope**

  **Where:** #371 round-1 CHANGES, P2 (review `...#pullrequestreview-5722092520`): "the token is neither
  random nor secret, and a comment claims otherwise … Readable anyway. The token is written in plain text
  to `zzz_canary_test.go` in the very package directory the agent's `init()` runs in. A probe can read
  that file, print the token and `os.Exit(1)`; that hits the 'rc≠0 + token' branch and reads as OK."
  Round 2 (`...#pullrequestreview-5722158386`): the urandom seed landed; the readability remains. Final
  OK (`...#pullrequestreview-5722251363`): "Still open, and stated in the release notes: a probe that
  reads the generated `zzz_canary_test.go` and prints its token reads as OK. A real fix needs a channel
  the agent's code cannot read."

  **Problem:** this is the one open limit of the whole F60 window and it is a real subversion path: the
  canary's whole job is to fail honestly, and the honest-failure evidence (the token) is readable from
  the directory the agent's code runs in. The window closed it by documentation (release notes + the OK
  review), which is the right call for this round, but the limit is P1-shaped and needs an owner decision
  (D94) on when it is fixed or accepted.

  **Fix:** owner decides: (a) accept the limit with the release-notes statement (current state), or (b)
  scope the fix — the token must travel over a channel the agent's code cannot read (e.g. the canary
  passes the expected token to the operator out-of-band and compares operator-side, not file-side).

  **Acceptance:** a decision recorded in the next review round; if (b), a branch exists for it.

- [ ] **I200: the F60 merge actor was unknown for ~13h and the pane's windows disappeared unexplained**

  **Where:** log 15:40Z 10-30: "#366 F60 MERGED 15:35:41Z as `fc04736` by the shared `papawattu`
  account (no pi pane shows a merge; taken to be the owner)". Log 05:30Z 10-31: "left pane %22 = homelab
  pi (PR #138, 78.5% ctx) KEEP; right pane %27 = its subagent `implement-f60-canary` (author of #366; the
  left pi merged #366 at 15:35:41Z, not the owner directly). … killed pi `1929579` by PID (pane closed),
  removed worktree `/tmp/coxswain-f60` (clean, `2b00359`), deleted local + remote
  `feat/f60-verify-canary`." Log 05:28Z 10-31: "tmux windows @17 (pi3, idle fresh) and @18 (homelab pi
  %22) GONE between ~05:21Z and 05:27Z; not by me … No pi process left for either."

  **Problem:** the reviewer assumed the owner merged a red-lint security PR for ~13 hours; the actual
  actor was a subagent pane, found only when the owner asked about tmux window 5, and two tmux windows holding agent
  sessions vanished with no recorded cause. All three facts (actor, pane kill, windows gone) are
  unexplained by evidence — recorded here as observations, not causes.

  **Fix:** the reviewer records the merge actor from the PR's `merged_by`/commit author whenever a pane
  does not show the merge; tmux windows that hold pi sessions are re-checked at each check-in and their
  disappearance is logged with the last known state.

  **Acceptance:** the next window's log names every merge's actor from `gh pr view --json merged_by`, and
  every window loss has a "last known state" line.

## Owner decisions

**D93 (new): the instruction that caused the I193 Secret copy is the reviewer's, and the standing
instruction is replaced.** The reviewer instructed "use a new namespace for the python sample" while the
sample's Gitea and `samples-git-cred` Secret live in `samples`; b2 read the Secret to make it work.
Standing instruction from this round: a sample runs in the namespace its credentials and services
already live in; a new namespace is only used when its credential story is written down first.
**Decision needed:** confirm the instruction as written, and decide whether the widened watcher pattern
(`get secret -o json|yaml`) is permanent.

**D94 (new): the F60 open limit (I199).** Options: (a) accept the token-readable limit with the
release-notes statement (current state); (b) scope the out-of-band-token fix now. Recommendation: (a)
for this round, since the canary is the negative control for the checks and the token read is a
second-order subversion; (b) if the owner treats the canary as load-bearing for release, the fix is the
next slice.

**Still open:** D72-D92 (digest `.samples/owner-decisions-digest.md`), I180's AGENTS edit, #338's
measurement, #342's two Calico runs, and the pre-1.30 proof (I189/D92 — #322's two observations ran in
this window: log 15:50Z 10-30, kind-proof comment `.../pull/322#issuecomment-6539088598`).

## What I did not verify

The cause of the two pi crashes (log 04:15Z and 06:50Z 10-31, "Cannot read properties of undefined
(reading render)" / "TypeError render") beyond the messages themselves. The cause of the tmux windows
disappearing (05:28Z 10-31). The full contents of the `.samples/post-merge/f60-pkg/` files named in the #371 kind-proof comment
(referenced, not re-read).
