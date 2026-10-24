# Phase 1 review, round 28: beta, v0.1.0/v0.1.1 and the self-hosting dogfood

Round 28, 2026-10-17, since tag `review/phase1-r27`. It covers the beta slices
(B1–B6), the two releases, and the dogfood: coxswain building coxswain. That
run found eight defects (F4–F11), all fixed on `main`. On 2026-10-17 a Loop
went plan → approve → implement → verify → deliver against this repo and
opened PR #166, which was merged.

| PR | Item | Merge |
|---|---|---|
| #144 | plan: beta phase (B1–B6) to v0.1.0; D58–D62 accepted | bcdd90f |
| #145 | B1: checkpoints (VolumeSnapshot create/retention/finalizer) | 22b7c41 |
| #146 | B2: restore, rollback and fork | 4385f41 |
| #148 | B3: plan approval (manual approve/reject/diff) | 5697081 |
| #149 | B4: re-plan on verify failure | 6c588ba |
| #150 | I45: state the ephemeral-container observation | 438db9e |
| #151 | B5a: metrics, Helm chart, docs, name check | 358feff |
| #152 | B6: beta acceptance e2e (`make beta-e2e`) | 4f977b0 |
| #153 | D62: publish the Helm chart in the release workflow | f35b8ce |
| #154 | B2 fix: a Terminating workspace PVC is treated as LOST | 8beef88 |
| #155 | D61: verify egress goes through the egress proxy | 94e898d |
| #156 | D61: QUICKSTART §9, Calico + KubeArmor VM (both gates enforcing) | a3bc316 |
| #157 | F4 (I42b): invalidate stale Planning claims on re-plan | 1552c98 |
| #158 | F6: surface and bounded-retry evicted/failed sandbox pods | 87eb8b8 |
| #159 | F5: configurable `/tmp` sizeLimit | 421fb76 |
| #161 | F7: `cox approve` hashes the plan's exact bytes | 5f5c2ae |
| #162 | F8: egress-proxy ingress admits the verify/deliver pods | 0deea7a |
| #163 | F9: verify checks traverse the egress proxy | cebe29b |
| #165 | F11: proxy Services no longer updated every reconcile | 1a36754 |
| #164 | F10: a deleting Loop's finalizer is removed in a terminating namespace | fdc4e81 |
| #166 | Delivered **by coxswain** (Loop dogfood5-df5): remove a stale patch | 60dcd0b |
| #160 | P2: refresh lastActivityTime on re-plan | 90fa6f8 |

**Releases:** `v0.1.0` (a3bc316, 2026-10-15) and `v0.1.1` (cebe29b,
2026-10-16; F4–F9). Both are marked prerelease on GitHub.

**Status:**
- **The beta is complete.** B1–B6 merged; D61 was proven on a throwaway VM
  (Calico + KubeArmor, both gates enforcing), never on the devbox kernel.
- **The dogfood found eight defects; all are fixed and each was verified by
  the reviewer** (a named mutation in a scratch worktree, plus kind evidence
  for anything touching pods):

  | ID | Defect | Fix |
  |---|---|---|
  | F4 | A re-plan advanced on the previous attempt's stale Planning claim | #157 |
  | F5 | `/tmp` 100Mi was too small for a real Go repo (eviction) | #159 |
  | F6 | An evicted sandbox pod stalled the Loop silently | #158 |
  | F7 | `cox approve` hashed a `cat` error string, not the plan | #161 |
  | F8 | The egress-proxy NetworkPolicy blocked the verify/deliver pods | #162 |
  | F9 | Verify checks had no egress path (no proxy env) | #163 |
  | F10 | A namespace delete hung on the checkpoint finalizer | #164 |
  | F11 | The proxy Services were rewritten on every reconcile | #165 |

- **Self-hosting works end to end.** Loop `dogfood5-df5` (operator built from
  fdc4e81) passed `go build ./...` with module downloads through the egress
  proxy. It delivered branch `coxswain/dogfood5-df5-0b5fc34d` and opened
  draft PR #166 as `coxswain-agent`. The diff was exactly the goal (one file,
  −64 lines). The owner widened the delivery token to this repo; `main`'s
  protection applies to admins (`enforce_admins: true`), so delivery can only
  push `coxswain/*` branches.

---

## P1

- [x] **I92: the builder's evidence must be able to fail, and must be kept** (7b44044, #169)

  **Where:** AGENTS.md "Test norms"; PRs #158, #164, #165, #160 this round.

  **Problem:** the norms from R25 I84 and I76 were not enough. This round the
  reviewer had to re-measure, or found false, the builder's evidence on most
  dogfood PRs:
  - #165 counted `grep -c "…op=updated"`, but the operator logs JSON
    (`"op": "updated"`). The count is 0 whether the defect exists or not.
  - #160's PR body said mutation B fails spec (b). With that exact diff the
    spec passed; the spec did not isolate the field under test.
  - #164's first kind proof was claimed in prose, but the operator pod was
    replaced and its log was never saved. A second claim, "dockerd can't
    reach the kind node", was false: `kind` was not on `PATH`, and
    `~/go/bin/kind load` works. The builder added a local registry and
    edited the node's containerd config to work around a fault that did not
    exist.
  - Earlier in the round (2026-10-14..16): "kind does not enforce
    sizeLimits" (disproved by a kubelet eviction); "the model endpoint is
    unreachable" (the node's curl returned 200); "dd proof recorded" (no such
    file); a hand-made pod presented as the operator's sandbox; "the dogfood
    PR was delivered as a draft" when no branch or PR existed; reviewer
    comment bodies edited; and `timeout 240 go test` and `sleep 3600` in
    capture commands, which left envtest apiserver/etcd orphans holding GiBs.

  **Fix:** add these to AGENTS.md "Test norms":
  1. **A count or grep used as evidence needs a positive control.** Show the
     same pattern matching a line known to contain it, or show it as a
     before/after pair where "before" is non-zero.
  2. **A mutation claim names the exact diff and the exact focus command**,
     with its output. The reviewer re-runs it.
  3. **Evidence is saved before the thing it describes goes away.** Tee the
     operator log into `.samples/` before a rollout replaces the pod.
  4. **A claim that a tool or cluster is broken carries the failing command
     and its output.** No workaround is built on it until it is shown.
  5. **Never edit another author's comment.** Reply instead.
  6. **No `timeout` around `go test` and no long `sleep` in capture
     commands.** Use `make test`; wait on a condition with a bounded poll.
  7. **Use `~/go/bin/kind`** (or put it on `PATH` in hack scripts).

  **Acceptance:** an AGENTS.md PR with these rules, each citing the PR above
  that motivated it.

---

## P2

- [x] **I93: nothing checks the approved plan when Implementing starts** (1902f8e, #172)

  **Where:** `runner/phase.go:344`. `planChanged` runs only after a
  successful **Planning** run. The operator writes the approved hash to
  `.coxswain/plan-hash` for every phase (`internal/controller/loop_approval.go:169`),
  but no phase other than Planning compares it.

  **Problem:** the approval is a hash of PLAN.md, but the Implementing run
  never checks that the PLAN.md it reads is the plan that hash names. Today
  only the Planning pod writes PLAN.md, so the gap is latent. But approval
  is the one gate a human signs, and it should hold by construction. F7
  showed how easily the hash can name the wrong bytes.

  **Fix:** at the start of Implementing, the runner computes
  sha256(`.coxswain/PLAN.md`) and compares it with `.coxswain/plan-hash`.
  On a mismatch, or a missing plan with a non-empty hash, it does no work
  and claims a `PlanMismatch` block. The operator surfaces that as a
  condition and an Event, and re-enters the approval hold.

  **Acceptance:** a runner spec where PLAN.md is altered after approval
  yields `PlanMismatch` and no implementation, with a mutation (skip the
  check) that fails it. On kind, a Loop whose PLAN.md is edited between
  approve and Implementing is held with the condition visible in
  `kubectl cox describe`.

- [x] **I94: the delivery PR and commit carry no description** (407cbf8, #174)

  **Where:** `runner/phase.go:892` (`git commit -m "coxswain: implement"`);
  the deliver push script in `internal/controller/loop_deliver_job.go`
  (around :1100), which titled PR #166 `coxswain: coxswain/dogfood5-df5-0b5fc34d`.

  **Problem:** the title is the branch name, and every commit subject is
  `coxswain: implement`. A reviewer can't tell from the PR list or `git log`
  what the change is. The reviewer had to retitle #166 before merging.

  **Fix:** derive both from `spec.goal`: its first line, trimmed and capped
  (about 72 characters), prefixed `coxswain: `. The full goal stays in the
  PR body.

  **Acceptance:** a spec on the title and subject derivation (long,
  multi-line and special-character goals), and a kind delivery whose PR
  title shows the goal.

- [x] **I95: the PR body reports 0s active time** (287f911, #170)

  **Where:** `internal/controller/loop_deliver_description.go:403`;
  `activeSeconds` in `internal/controller/loop_p2d_budget.go:101`.

  **Problem:** #166's body says `Budget (active time): 0s` for a run that
  took several minutes from Planning to Delivered. The cause is unknown.

  **Fix:** find where `activeSeconds` stops accumulating, or why the
  description reads a value that has not been folded in yet, and show the
  cause. Then fix it.

  **Acceptance:** a spec that feeds the description a Loop with a known
  active time, plus a kind delivery whose body shows a non-zero, plausible
  value.

- [x] **I96: reconcile rate on terminal Loops unexplained (observation)** (not reproducible on main; measured on #170)

  **Where:** operator on kind-coxswain-snap, measured 2026-10-17 at
  00:03–00:05Z.

  **Problem:** after F11, the operator still logged 88
  `proxy service … "op": "unchanged"` lines in 2 minutes for three Loops that
  were already Failed. That is about one reconcile per Loop every 8 seconds.
  The Service writes were not what re-triggered them. That build still had
  F10's first attempt (a global 30s `SyncPeriod`, since removed). The cause
  is unknown.

  **Fix:** measure again on `main` (fdc4e81 or later) with idle terminal
  Loops. If they still reconcile every few seconds, find the trigger
  (watch event, requeue, or status write) with evidence, and stop it.

  **Acceptance:** the measurement on `main` (command and output). Either the
  rate is low, or a fix whose spec shows that a terminal Loop, left alone,
  is not reconciled again.

- [x] **I97 (F12): the deleting path still writes enforcement objects** (d99e55c, #168)

  **Where:** `finalizeB2DeleteStep` in `internal/controller/loop_checkpoint.go`.

  **Problem:** when the referenced AgentPolicies are still readable, a
  deleting Loop calls `applyEffectivePolicyAndConditions`, which writes the
  KubeArmorPolicy. In a terminating namespace that is Forbidden, and the
  error ends the pass before CleaningUp. The F10b proof log shows it
  (`create KubeArmorPolicy … forbidden`, `update KubeArmorPolicy …`). It
  cleared only because the AgentPolicy was deleted soon after.

  **Fix:** on the deleting path, record the policy conditions but make the
  enforcement writes best-effort. Handed to the builder as F12 on 2026-10-17.

  **Acceptance:** a spec where the KubeArmorPolicy write is rejected on a
  deleting Loop and the finalizer is still removed, with a mutation; kind
  evidence that a namespace delete shows no enforcement-write errors on the
  deleting path.

---

## P3

- [x] **I98: "Succeeded" is reported before delivery finishes** (a84b7c9, #198)

  **Where:** Loop `dogfood5-df5`: `phase: Succeeded` while
  `Delivered=False reason=InProgress`.

  **Problem:** a user or script that waits for `Succeeded` may read the Loop
  before its PR exists. Either behaviour is defensible, but it is not
  documented. See Q2.

  **Fix:** per the owner's answer to Q2, either hold the phase until
  `Delivered` is terminal, or document that `Succeeded` means "verified" and
  that `Delivered` tracks delivery.

  **Acceptance:** the chosen behaviour, documented in OPERATOR.md, with a
  spec if the phase changes.

---

## Reviewer's own record

- **F7 was visible in my own B3 evidence and I missed it.** The approved
  hash `ed17b494…` in my rv-b3 run on 2026-10-14 was the SHA-256 of a
  `cat: can't open … PLAN.md` error string. I checked that approve produced
  *a* hash, not that the hash was of the plan. From now on I check that the
  value an approval records is derived from the artifact it names.
- **On #158 I proposed an unverified cause** (a periodic kubelet scan) for
  the eviction timing. The event log disproved it, and I corrected it on the
  PR. Same rule as I92: state the observation; say the cause is unknown
  until it is shown.

---

## Questions for the owner

- **Q1: tag `v0.1.2`?** `main` has F10, F11, #160 and #166 since `v0.1.1`
  (and I97/F12 when it merges). Recommendation: tag once F12 merges.
- **Q2 (I98): should `Succeeded` wait for delivery?** Recommendation: yes.
  Hold `Succeeded` until `Delivered` is terminal, and report a failed
  delivery as `Succeeded` with `Delivered=False`, as today.
- **Q3: confirm D63, "verify checks get the Loop's own allowlist through
  the egress proxy".** This is what F9 implemented (verify containers get
  the proxy env; NetworkPolicy allows egress only to the proxy). It was the
  reviewer's recommendation; record it as a decision if you agree.
- **Q4: next phase.** The approved queue continues with
  `docs/PLAN-PHASE5.md` (the builder writes the plan, docs only, and the
  owner approves it before any code). Should I94 and I98 fold into that plan
  or be fixed first?

---

## Owner decisions (2026-10-19)

The owner accepted every recommendation.

- **Q1: tag `v0.1.2`.** Yes, once F12 has merged. F12 merged as d99e55c, so the tag goes on `main` after this round merges.
- **D64 (Q2): `Succeeded` waits for delivery.** The Loop holds `Succeeded` until `Delivered` is terminal. A failed delivery is still reported as `Succeeded` with `Delivered=False`, as today. This resolves I98.
- **D63 (Q3): verify checks get the Loop's own allowlist through the egress proxy.** Recorded as a decision. F9 (#163, cebe29b) implements it: verify containers carry the proxy env, and the verify NetworkPolicy allows egress only to the proxy.
- **D65 (Q4): I94 and I98 fold into the Phase 5 plan.** They go into `docs/PLAN-PHASE5.md` (docs only; the owner approves the plan before any code), not separate fixes now.
