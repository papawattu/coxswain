# Phase 1 review, round 25: the alpha phase, A1 to A3

Round 25, 2026-10-11, since tag `review/phase1-r24`. It covers the first
half of the alpha phase (`docs/PLAN-ALPHA.md`): static enforcement (A1),
restart durability (A2) and `kubectl cox` (A3).

| PR | Item | Merge |
|---|---|---|
| #108 | I79 sample evidence counts the metering proxy usage lines | 5ce9e98 |
| #109 | PLAN-ALPHA (A0–A6) | 845709f |
| #110 | D52–D54 recorded | 4ebd404 |
| #111 | I80 untracked root PLAN.md stays out of the agent commit (D51) | 6f5fb97 |
| #112 | A1 ADR-0007 amendment: static enforcement evidence (D52) | ffc48d9 |
| #113 | A1 static enforcement evidence | 0ea752c |
| #114 | A2 restart durability e2e (measure) | a12ccbe |
| #115 | I77 init-workspace skips a completed clone | 00c0a09 |
| #116 | A2 close-out: sandbox-after-write fires from the script | 183c535 |
| #117 | A3 `kubectl cox` minimal | 2ee7a1b |

**Status:**
- **A1 done.** On kind-coxswain-dev, `PolicyEnforced=True` holds without
  `--allow-unenforced`.
- **A2 done.** Every kill case (operator in each phase and with a deliver
  Job in flight; sandbox mid-Implementing and after the agent wrote; verify
  Job) ends in the uninterrupted outcome. The PVC UID and `baseCommit` are
  unchanged, and the post-kill HEAD builds on the pre-kill HEAD.
- **A3 done.** All seven commands have fake-client unit tests. The kind e2e
  runs pause → resume → describe → redeliver, with the redeliver proven on a
  staged failed first delivery (new deliver Job UID, then `Delivered=True`).
- **R24 I72's live GitHub re-run** (2026-10-10, kind-coxswain-dev) delivered
  on the first attempt with no redeliver: `Delivered=True`, PR 2 open on
  `papawattu/coxswain-sandbox`.

**Owner decisions this round:**
- **D55: a sanctioned GitHub token file** (a narrow exception to D48).
  - The owner's fine-grained token, scoped to `papawattu/coxswain-sandbox`
    only, lives at `~/.config/coxswain/github-token` (mode 600).
  - Scripts may read **only** this file, and **only** to pipe it into a
    `kubernetes.io/basic-auth` Secret (`username=x-access-token`) through a
    helper, `hack/github-secret.sh <namespace>`.
  - The builder never prints or `cat`s it, and never looks for any other
    credential. The rest of D48 stands.
  - The point is repeatability: e2e runs and A6's fresh cluster need no
    manual step.
- **A4 merges without its live GitHub run.** That run moves into A6's
  acceptance, where D55 supplies the Secret.

---

## P2

### I81. A Loop whose name was used before can never deliver

- [x] Open (design) — 213dacf

**Where:** the deliver Job's push (`internal/controller/loop_deliver_job.go`),
which pushes to the branch `coxswain/<loop>`.

**Problem:**
- An earlier Loop of the same name, in this or another cluster, leaves
  `coxswain/<loop>` on the remote with unrelated history.
- The new Loop's push is then rejected (`! [rejected] ... (fetch first)`):
  `Delivered=False DeliveryFailed`.
- `coxswain.io/redeliver` hits the same rejection, so the Loop can't recover
  without someone deleting the remote branch.
- Seen in the #117 e2e, which now stages it deliberately. A6 runs on a fresh
  cluster, but its sandbox repo keeps branches from earlier runs.

**Fix (recommendation, b):**
- (a) Push with `--force-with-lease` to the Loop's own branch. It's simple,
  but it overwrites a branch a human may have built on.
- (b) Name the branch with the Loop UID, e.g. `coxswain/<loop>-<uid[:8]>`.
  Branches never collide, and nothing is overwritten.

**Acceptance:**
- A spec or execution test: a second Loop with an earlier Loop's name
  delivers first time while the earlier branch still exists.
- The kind e2e's stale-branch staging stops causing a failure (move the
  redeliver proof to another staged failure).

## P3

### I82. `cox redeliver` mislabels a failed delivery

- [ ] Open

**Where:** `internal/cli/redeliver.go`.

**Problem:** for a Loop whose delivery failed, the command prints
`note: status.delivery is nil` (#117 run.log:104). That reads as "never
delivered".

**Fix:** print the `Delivered` condition's status and reason, e.g.
`previous delivery failed: DeliveryFailed`.

**Acceptance:** a unit test for the failed-delivery message.

### I83. A duplicate PhaseAdvanced event

- [ ] Open (cause unverified)

**Where:** the phase-advance event emission (`loop_controller.go`
`emitPhaseAdvancedEvent`, and a second site on the verify path).

**Problem:** the A2 control run (`.samples/a2-restart-20261010191544`)
recorded `phase advanced Implementing -> Verifying` twice, about 1 s apart,
for the same Loop. Other runs show it once. The cause is not established.

**Fix:** reproduce, find the cause, then emit once per transition.

**Acceptance:** an envtest spec asserting exactly one event per transition
under a re-reconcile.

## P3: process

### I84. e2e claims ahead of their evidence

- [ ] Open (AGENTS.md)

Three times this round, an e2e result or PR reply said more than its
evidence:
- **#114:** `events.jsonl` was field-flattened by `jq -c '.[]'`, and its
  validation passed anyway.
- **#115:** the reply said the script fired the kill in Implementing. kill.txt
  said Verifying; the later run was a hand-issued kill, with the Loop still
  stuck.
- **#117:** the script printed `PASS ... -> redeliver` after logging
  `skipping redeliver`.

**Fix:** AGENTS.md test norms:
- an e2e never prints PASS for a skipped or unproven step;
- a validation checks the content's meaning (e.g. `.kind == "Event"`), not
  just that it parses;
- an evidence claim names the file and line it comes from.

**Acceptance:** the AGENTS.md change; reviewers cite it.

---

## Carried

- R24 Q1 is answered (D51, I80 #111).
- R22 I59 (design) and I62 (workflow edits are an owner push): open as
  written.

## Next

1. A4, the self-explaining draft PR (in progress), then D55's
   `hack/github-secret.sh`.
2. A5 packaging. Needs the owner for the ghcr push (D48; D55 covers only the
   sandbox repo).
3. A6 alpha acceptance, including the live GitHub run.
4. I81 before or with A6 (the sandbox repo keeps old branches).
