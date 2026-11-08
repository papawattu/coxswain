# Phase 1 review, round 50: P6c and P6d land, Phase 6's planned slices are done, and the R49 follow-ups close

Round 50 was written by the reviewer on 2026-11-08 at ~13:00Z. Its sources are the reviewer's log
(`/tmp/claude-1000/.../scratchpad/overnight-20261009.md`, lines 1825-1870, the entries after R49),
the kind-ref evidence under `.samples/p6c-kind/` and `.samples/p6b3-kind/run9/` (local, never
committed), and commands re-run while writing. It follows R49 (`docs/REVIEW-PHASE1-R49.md`, merged as
**`35645b5`** in PR #413, 2026-11-07 21:26Z, tagged `review/phase1-r49`, corrected by #415 `db15e28`;
its last ID is I229, its last decision D115).

| PR | Item | Merge |
|---|---|---|
| #415 | review(phase1-r49): correct I223's evidence; I226(g) | `db15e28` 11-07 21:43:54 |
| #414 | D115: the agent KubeArmorPolicy selects the sandbox pod only | `d2aca41` 11-07 21:46:25 |
| #417 | I224: AgentPolicy `files` entries must be canonical absolute paths | `6b49389` 11-08 09:32:19 |
| #416 | P6c: only the operator writes Loop status (VAP, D85(a)/D114) | `0af2e7a` 11-08 09:57:54 |
| #419 | I229: hold the reattach so the relay's Unavailable state is observed | `7317832` 11-08 10:14:24 |
| #420 | I228: suggest collapses busy directory trees recursively | `87bcee1` 11-08 10:50:41 |
| #418 | P6d: `cox retry` for Failed:SandboxFailed (D86/D116) | `1580c6d` 11-08 12:38:56 |

Merge times and OIDs come from `gh pr list --state merged --json number,title,mergedAt,mergeCommit`,
re-run while writing. The reviewer merged every PR, each on a reviewer OK, green CI and the evidence
the owner asked for.

**Status:**

- **Phase 6's planned slices are done:** C2, C3, P6a, P6b, P6c and P6d. C1 (multi-arch release images) is an owner push and is still open.
- **P6c is kind-proven** (`.samples/p6c-kind/p6c-run.log`, line numbers after removing `go: downloading`):
  - the VAP and binding were applied (:22-27);
  - a learn-mode Loop Succeeded with the VAP active (:47), and 0 operator writes were denied (:130);
  - a `system:masters`-grouped status patch was denied by the VAP with its message (:127);
  - the operator ServiceAccount's dry-run patch went through (:129);
  - the VAP was removed and the cluster restored afterwards (:136-145).
- **P6d merged on envtest evidence** (owner decision, 11-08: no kind proof):
  - the CLI reason-gate mutation is caught;
  - all six operator mutations are caught: the reason gate, the episode reset, the `Retried` Event, the annotation removal, the Reconcile wiring and the Sandbox delete;
  - the full suite passed 799/799 at `3d2959f`.
- **Every code PR carried reviewer-run mutations,** each of which failed its spec, plus a full-suite base run:

  | PR | Mutations | Full suite |
  |---|---|---|
  | #414 | 1 | 794/794 |
  | #417 | S, D | 796/796 |
  | #416 | m1, m2, chart | 797/797 |
  | #420 | skipdirs 20/20, nearest 14/20 | (unit) |
  | #418 | M1 + R, C, E, A, W, D | 799/799 |

  #419 is test-only; its proof is a before/after under load, below.
- **No P1 open.**

## Closed

**Ticked by this commit:**
- R49 I223 by `d2aca41` (#414); its evidence correction is #415.
- R49 I224 by `6b49389` (#417).
- R49 I229 by `7317832` (#419). Under 6 CPU burners with `GOMAXPROCS=1 -count=200`, the old spec failed 19/200 (`timed out waiting for the unavailable state`), and the new one passed 200/200 (`scratchpad/i229-old-full.log`).

**Closed by analysis, not by a code change:**
- **R49 I221** (unattributed alerts): the operator's audit stream already writes them with `loop: ""` and `detail: unattributed: workload <ns>/<pod>`. Grouping runs 8 and 9 by pod and timestamp shows two sources:
  1. The PREVIOUS runs' verify-pod lines, replayed when the relay first attaches after their Loop was deleted. Run 8 started at 20:58:14Z, but its 753 verify-pod lines carry 20:07Z timestamps, from run 7's pod `qjt2n`. Run 9's lines are from 20:07Z and 21:05Z.
  2. The live `gitea` pod in the visibility-annotated `samples` namespace (170 and 86 lines).

  Neither is a defect. The tidy-up is I232. Ticked when R50 merges.

**Still open:**
- **R49 I220:** its rule ("a feature consuming a third-party component's output is proven by a REAL captured line and one kind run") is not yet in the AGENTS.md test norms.
- **R49 I222** (egress-proxy records) and **I227** (log rotation): these need investigation, not a builder slice.
- **R49 I228:** partly met. Run 8 now yields **25** files entries, against the acceptance of 20 or fewer, with 0 overflow and 0 entries covered by a listed `dir/` (`scratchpad/run8-suggest-i228c.yaml`). The bar is not moved. The rest is the four `/memory/*.txt` lesson files, under the 5-entry collapse threshold, and the git-credential mount paths.
- **R49 I225 and I226:** continued as I234 and I235.
- **R47 I213; R46 I206, I207 and I211:** unchanged.

## P2

- [x] **I231: the docs' "missing gitCredentialSecret ends at SandboxFailed" example has been stale since D76** (`a3a7a01`)

  **Where:** `docs/UPGRADE.md` ~515 ("A sandbox volume the kubelet cannot mount (a `gitCredentialSecret` that does not exist, say) now ends at `Failed:SandboxFailed`"), and #418's new `docs/OPERATOR.md` `### 8. Retry` ("e.g. a missing `gitCredentialSecret`"). The opposite is in `internal/controller/loop_secrets.go:104`: the D76 gate covers `spec.workspace.gitCredentialSecret`, and a missing one holds the Loop with `SecretsUsable=False SecretNotFound` before any sandbox exists (TROUBLESHOOTING's D76 section says the same).

  **Problem:** the docs point users at `cox retry` for a case that never reaches `SandboxFailed`. The reviewer's own P6d kind-proof plan used the same stale premise.

  **Fix:** reword both examples to a cause that DOES reach the bound, for example a Secret deleted or changed after the Loop started, or a volume the kubelet cannot mount for another reason. Grep the docs for the old example first.

  **Acceptance:** `grep -rn "gitCredentialSecret. that does not exist\|missing .gitCredentialSecret" docs/` returns only text consistent with the D76 hold.

- [ ] **I234: the builders' slips this window (continues R49 I225)**

  **Where:** reviews on #414, #416, #417, #418 and #420; log 11-07 21:3xZ to 11-08 12:xxZ.
  - **pi3:**
    - (a) `/new` was blocked by uncommitted changes in its cwd (`Warning: Commit your changes first`), so the I224 brief was never received. The reviewer re-sent it;
    - (b) skipped two step-5 test items on #411/D112 without saying so (carried from R49);
    - (c) used non-standard report lines on P6d step 2, so the watcher missed the push;
    - (d) its D115 upgrade answer claimed a `KaptTampered` pass that the cited code does not produce (live and recorded hashes are both old);
    - (e) refactored Reconcile's checkpoint-gate block for gocyclo (#418) beyond the brief. Equivalent, traced by the reviewer.
  - **b4:**
    - (f) diagnosed P6c's envtest failure as "envtest does not enforce `request.*` VAPs" and proposed a dedicated envtest cluster. The failing log showed RBAC (`User "p6c-editor" cannot update resource "loops/status"`), which runs before admission;
    - (g) its first I228 subsumption skipped `dir/` entries; caught by the reviewer's real-data check.

  **Fix (in force):**
  - after `/new` plus a brief, confirm the pane shows `Working`;
  - every report line is pinned in the brief, and the reviewer reads the pane when a watcher times out;
  - no root-cause claim is accepted without the failing output;
  - every evidence number is re-run against the pushed head.

  **Acceptance:** none of (a)-(g) recurs in R51's window.

- [ ] **I235: the reviewer's own slips in this window**

  **Where:** the log entries named below.
  - **(a) A mutation on the wrong clause:** the first I224 mutation D removed the EXEC rule's `!e.contains('/../')`. The CRD has that clause twice, and the replace hit the first one. That run is void; the redo targets the files rule (`i224-muts2.py`) and is caught. Rule: a mutation of a duplicated clause checks the occurrence count and targets the exact rule.
  - **(b) A parent-PID kill:** `pkill -P <parent>` was used to stop CPU-load generators during I229's proof. It is PID-targeted, but against the "kill by PID only" rule. The re-run recorded each PID and killed those.
  - **(c) An unverified claim in a release note:** the I224 release-note brief claimed CRD-ratcheting behaviour ("the edit is then rejected") that was never verified. Fixed in `1be8308` before merge.
  - **(d) A stale premise:** the P6d kind-proof plan assumed a missing Secret reaches `SandboxFailed`, which is stale since D76 (I231). The owner chose envtest-only.

  **Fix:** the rules above (memory and the loop prompt).

  **Acceptance:** none of (a)-(d) recurs in R51's window.

## P3

- [x] **I230: `retryPhaseFromEpisode` accepts any string after the `/`** (`36e134f`)

  **Where:** `internal/controller/loop_retry.go` (`retryPhaseFromEpisode`).

  **Problem:** the phase a retry resumes is whatever follows the first `/` in `status.agent.infraEpisode`, as long as it is not empty and not `Failed`. A corrupted or unexpected value (`Paused`, or a typo) would be written to `Phase`/`DesiredPhase` unchecked.

  **Fix:** accept only the phases a sandbox run can fail in (Planning, Implementing, Verifying). Anything else is ineligible, with a log line, and the annotation is removed.

  **Acceptance:** a spec with `infraEpisode: "0/Paused"` leaves the Loop `Failed`.

- [x] **I232: replayed previous-run lines are counted as "unattributed" (from R49 I221)** (`f4faffe`)

  **Where:** `internal/alerts/relay.go` (`accept`, and the first attach's `firstAttachTailLines`).

  **Problem:** the first attach re-reads up to 2000 agent lines, including events from Loops that no longer exist. Each one bumps the unattributed count that the `PolicyBlocked` message reports, which reads as a live problem.

  **Fix:** an option, owner's call: do not count, or do not emit, a line older than the relay's start for a pod that no longer resolves; or report it as "replayed" separately.

  **Acceptance:** run 8's shape (old verify-pod lines) is no longer reported as live unattributed alerts.

## Owner decisions

**Decided in this window:**
- **D114**, 11-07: P6c's VAP denies every `loops/status` write except the operator's ServiceAccount, cluster admins included. Break-glass is to remove the binding.
- **D115**, 11-07: the agent KubeArmorPolicy selects the sandbox pod only. Made defensive by the #415 correction.
- **D116**, 11-08: `cox retry` sets a one-shot `coxswain.io/retry` annotation (SandboxFailed only), and the Loop resumes from its current checkpoint in the phase it failed in.
- **P6c kind proof:** approved (11-08), after the permission classifier blocked the first attempt.
- **P6d:** merge on envtest evidence, with no kind proof (11-08).
- **R49:** merge and tag approved (11-07).

**Open:**
- **I232:** the replayed-line option.
- **C1:** multi-arch, an owner push.
- **R47 I213:** waiting on the homelab side.

## What I did not verify

- **Why run 3's ref-worker2 agent logged no Loop events** until 16:57:44Z (R49); it has not recurred since.
- **The egress proxy** (R49 I222): whether the agent made any external connect.
- **The relay's behaviour across a kubelet log rotation** (R49 I227).
- **CRD validation ratcheting** for already-stored `files` entries (I224's release note says it is cluster-dependent).
