# Phase 1 review, round 24: R23 follow-ups and the first live GitHub delivery

Round 24, 2026-10-10, since tag `review/phase1-r23`. It covers the R23
follow-up PRs and the first live GitHub delivery (R21 I54). That run found
four operator bugs.

| PR | Item | Merge |
|---|---|---|
| #86 | I65 a verify Job that fails before the checks no longer wedges the Loop | d92ef45 |
| #87 | I65 box | c8a03ce |
| #88 | D50 = (a) for I64 | 2bc52f1 |
| #89 | I68 commit hygiene + `make lint` guard; I58 credential rule (AGENTS.md) | 06a670e |
| #90 | I64 documents the DNS-fence gap (D50) | 0df223f |
| #91 | I67 TDD-PLAN-PHASE2 corrections | 73c2c5d |
| #93 | I71 p2-e2e memory: image prune + `P2H_LOOP_COUNT` | 152a9f8 |
| #94 | I69 G1/G2 re-run on 456a209 (closed: empty commits; evidence in the PR body) | — |

**Status:**
- **I54: live GitHub delivery works** (2026-10-09, kind-coxswain-dev).
  - Operator built from main `73c2c5d`; running digest `sha256:d558f2f5…`
    equals the build.
  - Loop `i54/i54-hello`, against the owner's private sandbox
    `papawattu/coxswain-sandbox`, went Planning → Implementing → Verifying →
    Succeeded in ~50 s, first iteration (verified commit `10b11b8`).
  - Draft PR papawattu/coxswain-sandbox#1: head `10b11b8` (the verified
    commit), base `main`, `status.delivery.prURL` the GitHub URL.
  - The egress proxy log shows only `github.com:443` and `api.github.com:443`,
    all allowed.
  - **But** delivery succeeded only after a manual `coxswain.io/redeliver`.
    The first attempt hit I72 below.
- **Owner decisions this round:**
  - I64 → D50 = (a): accept and document the DNS-fence gap.
  - I58: the token is not revoked (owner's call). AGENTS.md now carries
    D48's rule.

---

## P1

### I72. Every GitHub delivery fails on its first attempt

- [ ] Open (PR #96: reviewer OK + notes; the live GitHub re-run is still owed, after host memory reaped the first attempt)

**Where:** `internal/controller/loop_deliver_job.go` `ensureDeliverJob`.

**Problem:** the operator creates the deliver Job in the same reconcile that
creates the Loop's egress proxy. The Job's `clone-base` ran 24 ms later and
failed: `Failed to connect to github.com:443 over proxy
i54-hello-egress-proxy… Could not connect to server`. There is one deliver
Job per verified commit and no retry, so the Loop went `Delivered=False
DeliveryFailed`. The S6 kind evidence never hit this because Gitea is
in-cluster and doesn't use the egress proxy.

**Fix:** hold the deliver Job (requeue; no Job and no failure) until the
Loop's egress proxy pod is controlled by the Loop and Ready. This is the same
gate the sandbox uses (`loop_controller.go:1416`).

**Acceptance:**
- Envtests for each in-progress state (no pod, Pending, Running-not-Ready):
  no Job, and Delivered not failed. Ready → Job created.
- Removing the gate fails them.
- A live GitHub re-run delivers first time, with no redeliver.

### I73. init-workspace wedges the Loop if its first run is interrupted

- [ ] Open (PR #97: reviewer OK)

**Where:** `internal/controller/loop_controller.go`, the init-workspace
script.

**Problem:** the script's comment says it wipes a half-cloned workspace, but
it only removes `.git` and `.coxswain`. If the first run is killed after
`checkout` (seen on i54-hello: "Stopping container init-workspace" while the
model proxy was crash-looping), every restart fails with `untracked working
tree files would be overwritten by checkout: README.md`. The Loop can't
recover; only deleting the Loop and its PVC helps.

**Fix:** wipe all of `/workspace`'s contents, dotfiles included, but not the
mount point.

**Acceptance:**
- An execution test runs the real generated script twice against a local
  bare repo, with the first run stopped after checkout; the second run
  succeeds.
- The old wipe fails that test.

## P2

### I74. The sandbox keeps restarting after a Loop finishes

- [x] Open (PR #98: reviewer OK; it also amends TDD-PLAN-PHASE2 item 9/F) — 6be6e16

**Where:** the sandbox lifecycle on terminal phases; the runner.

**Problem:** after `Succeeded`, the agent container restarts repeatedly. The
runner exits with `unknown desired-phase "Succeeded"; the runner executes only
Planning or Implementing`. This is a crash loop for the Loop's lifetime,
using node resources for nothing.

**Fix:** the operator stops the sandbox on terminal phases (as Paused
terminates the sandbox pod).

**Acceptance:** envtests asserting no running sandbox after Succeeded and
after Failed, plus the matching mutation.

### I75. The samples model Secret is pre-P2b

- [x] Open (PR #99: reviewer OK; the live `make sample-run` is still owed) — a4df6a4

**Where:** `hack/sample-run.sh` and the samples fixtures.

**Problem:**
- **Wrong shape:** `samples/vllm-no-auth` still has `api.key`/`model.name`.
  The metering model proxy (P2b) needs `model-key` and `MODEL_BASE_URL`
  (`test/e2e/p2-e2e.sh:558`).
- **Effect:** `samples/gocli-task1-proxy` has crash-looped since P2b (~4
  days), and `make sample-run` copies the stale Secret into new namespaces.

**Fix:** create the P2b shape in the scripts and docs.

**Acceptance:** `make sample-run APP=gocli TASK=1` reaches Succeeded again on
kind.

### Q1. Should the runner's PLAN.md be in the delivered PR? (owner)

The I54 PR adds `PLAN.md` (the runner's planning artifact) next to
`hello.sh`. Either it belongs in the PR as a record of the agent's plan, or
delivery should drop it (or the runner should write it under `.coxswain/`).

**Owner:** choose (a) keep, or (b) exclude from delivery.

### I77. init-workspace can wipe agent work on a pod restart

- [ ] Open (design)

**Where:** the sandbox pod build (`loop_controller.go`, init-workspace
present until the base commit is recorded).

**Problem:** init-workspace re-runs whenever the pod that still carries it
restarts. If that happens after the agent has started writing to the
workspace, and before the operator rebuilds the pod without init-workspace,
the wipe (old or new, see I73) removes the agent's work. This predates I73.

**Fix:** a design item. Options:
- (a) init-workspace skips when `.coxswain/base-commit` matches a completed
  clone (a success marker);
- (b) the operator records the base commit and rebuilds the pod before the
  agent container starts.

**Acceptance:** a spec or execution test where a second init-workspace run on
a workspace holding agent commits leaves them intact.

## P3: process

### I76. Builder habits seen this round

- [x] Open (AGENTS.md) — 8d15503

- **Mutation image left deployed:** after I69, the I69 G2 mutation operator
  (`coxswain-mut-g2-i69`, budget gate disabled) stayed deployed on
  kind-coxswain-dev for two days.
- **Ticks before merge:** the box-ticking PR #95 was opened before the PRs
  it ticked had merged, citing branch hashes instead of squash hashes.
- **Replies stuck as drafts:** review-thread replies were left in a
  **PENDING** review twice (#86), invisible and blocking the reviewer's
  review.
- **Claims without evidence:** "shown working on kind" was posted without the
  output (#92), and a doc comment asserted a kubectl behaviour that a kind
  test disproved.

**Fix:** AGENTS.md:
- after any mutation kind run, redeploy a build of main and say so in the
  PR;
- tick boxes only after merge, with the squash hash;
- after replying, check `reviews[]|select(.state=="PENDING")` is empty;
- paste command output for any "shown working" claim.

---

## Carried

- R22 I59 (design), I60, I61, I62 (workflow edits = owner push), I63: open as
  written in R22.
- R23 I66 (tick pending), I70 (#92: reviewer OK).
- R22 I61 (#100), I63 (#101: reviewer OK), I60 (#102): PRs open from 2026-10-10.

## Next

1. The owner merges #92, #96–#99 and #101. The I72 live re-run and I75's `make sample-run` need host memory.
2. I74, I75.
3. The R22 carry-overs I60, I61, I63.
4. Q1 at the owner's direction.
