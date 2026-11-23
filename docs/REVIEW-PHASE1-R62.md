# Phase 1 review, round 62: I276 closed, fence-wait in every Job pod proven on kind, and a probe race after an upgrade

**Scope:** main `3aff005` (R61's main) → `7ab11ec`, 11-22, and two lab runs on kind-ref (Calico) that use R61's script with the Judge, PR delivery and a lesson turned on:
- **c522** (21:39Z–21:46Z), on #522's candidate `fe46e76`;
- **main7ab** (21:56Z–22:13Z), on main `7ab11ec`, which leaves the lab on main.

Evidence is reviewer-local and not committed: `.samples/r4/c522/` and `.samples/r4/main7ab/`. R57's I271 and I272 and D144's ToolTrace capture are not re-checked here and stay open.

Merged in this window:

| PR | What | Squash | Review on the PR |
|---|---|---|---|
| #523 | R61 (owner merge, tag `review/phase1-r61`) | `f21d204` | pi3, read-only |
| #522 | fence-wait as `initContainers[0]` in the verify, memory-writer, Judge and deliver Jobs | `7ab11ec` | reviewer, `Verdict: OK + notes`, 2 P3s |

#522 is the first PR from another session reviewed under D145 = (a). The review was submitted at 21:48:04Z and the PR merged at 21:49:58Z (`gh api …/pulls/522/reviews`, `…/pulls/522 --jq .merged_at`). The owner merged with both P3 threads open, and they are carried here as I283.

## Closed

- **I276** is ticked in R61 (`7ab11ec`). Its acceptance was that #522 is reviewed on the PR before it merges, and it was. The review names three mutations, each run in a scratch worktree, with the FAIL line for each, and the c522 kind run below.

## The Job pods' fence-wait on kind

Both runs give the same result. Each line below is from `fence-summary.txt` in the run's evidence dir: lines 6–11 in both. Each pod's first init container is `fence-wait`, and each exited 0 with `FENCE_UP elapsed=1.10 attempts=2`:

| Pod (main7ab) | Init containers |
|---|---|
| `r4-fence-verify-1-96rdn` | `fence-wait, clone-base, import-agent, tamper, artifact, scan-prep, scan, canary, check-0…2` |
| `r4-fence-judge-1-tgd48` | `fence-wait, judge-diff` |
| `r4-fence-deliver-ltkn7` | `fence-wait, clone-base, import-agent` |
| `r4-fence-memory-write-1-9x7jp` | `fence-wait` |
| `r4-fence-sandbox` (the third of three) | `fence-wait, memory-copy-in, init-phase` |
| `r4-fence-proxy` (model proxy) | `fence-wait` |

- **End state** (`main7ab/r4-lab.log:34-36`): Succeeded at 22:11:41. `JudgeVerdict=True/Judged`, `Delivered=True` (PR 23), `MemoryWriteDone=True/MemoryWritten`, `VerifySuspicious=False`. `SandboxFenceNotReady` was never set (`poll.log`). c522 matches (`c522/r4-lab.log:34-37`, PR 22).
- **Controls** use the sandbox's real fence-wait container with a 10 s budget. With no NetworkPolicy, C0 gives exit 2, `FENCE_TIMEOUT elapsed=10.03 attempts=101`. With a deny-all egress policy, C1 gives exit 0, `FENCE_UP` (`controls.txt:1-2`, both runs).
- **Operator log:** main7ab has 55,985 lines and `forbidden` = 0. The positive control is 2 `CNI probe result changed` lines (`main7ab/r4-lab.log:51`). c522 has 58,913 lines, also 0, with a control of 1 (`c522/r4-lab.log:52`).
- **The lab is on main:** all five images and the operator args are `:7ab11ec`, `--memory-storage-class=local-shared` is unchanged (`main7ab/r4-lab.log:19`), and 0 Loops are left (`:58`).
- **Still not exercised:**
  - the egress, tool and Judge proxies (no jproxy pod appears in `samples-podwatch.json`; the Judge used the Loop's own model proxy);
  - a fence-wait timeout in any real pod;
  - a KubeArmor exec allowlist.

## P2

- [ ] **I281: after an upgrade, the first CNI probe can fail on a terminating probe pod and hold every Loop for a full check interval**

  **Where:** `internal/cni/pod_prober.go:316` (`cleanup` at the start of `Probe`), `:409-420` (`cleanup`), `:379` (`runPod`'s create); `internal/cni/prober.go:126,131` (one probe at start, then one every `--cni-check-interval`, 10 min by default, `cmd/main.go:145`).

  **Problem:**
  - In main7ab the operator was upgraded at 21:56:56Z (`main7ab/r4-lab.log:23`). Its first probe result, at 21:56:58Z, was `ProbeUnavailable` with the detail `control pass: coxswain-cni-probe-control pod create: object is being deleted: pods "coxswain-cni-probe-control" already exists` (`:24`).
  - The check Loop was created at 21:57:12Z and was held fail-closed: `NetworkEnforced=False reason=ProbeUnavailable … sandbox held Suspended` (`main7ab/operator.stream.log:2970`).
  - Nothing retried until the next tick. At 22:07:30Z the result became `CNIEnforced` (`:3489`). The first sandbox pod was created in that same second (`samples-podwatch.json`, creationTimestamp `2026-11-22T22:07:30Z`). So the Loop waited 10 min 18 s with no sandbox.
  - `cleanup()` deletes the probe pods and returns without waiting for them to go. A pod with a grace period is still in the API after its delete is accepted, so the create that follows can hit it. The error text shows that this happened here. Whose delete was pending, this operator's `cleanup` or the old operator's `runPod`, is not shown.
  - In 1 of the 3 upgrades I logged (R4 run 1, c522, main7ab) the first probe was `ProbeUnavailable`. The other two were `CNIEnforced` within 34 s. It fails closed, so nothing ran unfenced. The cost is availability: every Loop waits up to 10 min after a rollout.

  **Fix:** both of these:
  - `cleanup()` waits, within the probe's timeout, until each deleted probe pod is gone;
  - a `ProbeUnavailable` result is retried on a short backoff (for example 15 s, doubling, capped at the interval) instead of waiting for the next tick.

  **Acceptance:**
  - A spec where the control pod still exists with a deletionTimestamp (a finalizer holds it) when `Probe` starts. The probe completes once the finalizer is removed. It FAILS on today's code with the `object is being deleted` detail.
  - A `ProbeRunnable` spec where the first `Probe` returns unavailable and the second returns enforced. The holder is `CNIEnforced` well before `Interval`. It FAILS with the retry removed.
  - One kind upgrade where the first result is `CNIEnforced` or recovers within the backoff, with the log lines cited.

- [ ] **I282: the shared tree was taken off `main` three times in this window, twice under my briefs or by me**

  **Where:** the shared tree `/home/jamie/projects/coxswain`. AGENTS.md (R43 D91(a)) says it stays on `main`.

  **Problem.** `git reflog --date=iso --since=2026-11-19 | grep -v 'pull -q --ff-only'` shows:
  - **11-20 09:00:48:** `checkout: moving from a6ebe62… to origin/main`. That was my command, in a cleanup step: `git checkout -q --detach origin/main` (my session transcript, 2026-11-20T09:00:46Z). The tree stayed detached until the next entry.
  - **11-22 09:26:04 → 11:04:27:** checkouts of `adoption/plan-adoption`, `adoption/plan9-d140-d142`, `adoption/plan9-d141`, `-to-main` and `-to-main-v2`, with four commits made in the shared tree (`857f0a4`, `974e65c`, `9091da6`, `2d5cb58`), then back to `main`. These are the Phase 9 plan PRs from the session I did not brief (R61's I276). Which agent ran them is not in the repo.
  - **11-22 21:30:42:** `checkout: moving from main to origin/main`. That was pi3 while it reviewed #523: `cd /home/jamie/projects/coxswain && git checkout origin/main …` (pi session `01a2052a`, started 21:30:26Z). My brief said "Read-only: do not edit, commit or push" but did not forbid a checkout. I put the tree back on `main` at 21:48:50.
  - No commit landed on `main` from any of these, and no work was lost. But for 2 days (11-20 09:00 to 11-22 09:26) a `git grep` or `make test` in the shared tree answered about `bed9ba7`, not about `main`.

  **Fix:**
  - My own commands read `main` with `git show origin/main:<path>` or `git grep … origin/main`, never with a checkout in the shared tree.
  - Every pi3 brief says: never run `git checkout`, `switch` or `pull` in `/home/jamie/projects/coxswain`; use `git -C … show origin/main:<path>` or a worktree.
  - Sessions I do not brief are asked, through the owner, to work in a worktree.

  **Acceptance:** at R63, the same reflog command prints only `pull` lines for the window.

## P3

- [ ] **I283: #522's two carried P3s**

  **Where:** `internal/controller/loop_verify_outcome_gates.go:577`; `internal/controller/loop_job_fence_wait_test.go:113-131`.

  **Problem:**
  - The doc comment on `trustedInitDecision` still says the trusted inits are "(clone-base, import-agent)". Since #522 the filter at `:591-597` also includes `fence-wait`.
  - `TestVerifyOutcomeFenceWait` tests fence-wait as timed out (`:122`) and as Running (`:128`), but not as Waiting. AGENTS.md's test norms ask for a spec for each in-progress state. The `waiting` state at `:114` is used only for the other inits.

  **Fix:** add `fence-wait` to the comment. Add a Waiting case for fence-wait that asserts no decision and a requeue.

  **Acceptance:** the new case FAILS when `trustedInitDecision` maps a Waiting trusted init to a decision. Name that mutation and paste its output in the PR.

## Still open from R61

Checked on `7ab11ec` just now:
- **I277:** `git grep -c '\bD14[0-3]\b' docs/PLAN-PHASE9-ADOPTION.md` still gives 10.
- **I278:** the "still reachable" detail is unchanged at `pod_prober.go:346-353`.
- **I279:** `applySandboxFenceNotReady` is still only called at `loop_s4_phase.go:1169`, after the early returns at `:700-705`.
- **I280:** `grep -rln 'fence-wait-budget' cmd/*_test.go` still finds 0 files. The control `leader-elect-lease-duration` finds 1.

The next free IDs are I284, and D146, or D150 if I277's renumbering takes D146–D149.

## What I did not verify

- **Whose delete was pending in I281.** The old operator's pod was being replaced at the same moment (no `--leader-elect` on the lab). I did not capture its log before it went.
- **I281 on k3s.** Only kind-ref was upgraded.
- **The other session's agent in I282.** I attribute only the entries I found in a transcript.
