# Phase 1 review, round 51: the relay keeps every KubeArmor line, I222 and I227 are answered, and suggest's input and collapse show their limits

Round 51 was written by the reviewer on 2026-11-08 at ~16:30Z. Its sources are the reviewer's log
(`/tmp/claude-1000/.../scratchpad/overnight-20261009.md`, the entries from 12:51Z to 15:54Z), the
kind-ref evidence under `.samples/run10-kind/` and `.samples/run12-kind/` (local, never committed),
and commands re-run while writing. It follows R50 (`docs/REVIEW-PHASE1-R50.md`, merged as
**`ccf852a`** in PR #421, 2026-11-08 12:51Z, tagged `review/phase1-r50`; its last ID is I235, its last
decision D116).

| PR | Item | Merge |
|---|---|---|
| #422 | I231: the SandboxFailed example no longer contradicts the D76 hold | `a3a7a01` 11-08 13:02:14 |
| #423 | I230: a retry resumes only a known run phase | `36e134f` 11-08 13:08:58 |
| #425 | I232: replayed lines are not counted as unattributed (D117) | `f4faffe` 11-08 15:29:34 |
| #424 | docs(AGENTS): test norms from R49/R50 | `8b141d4` 11-08 15:33:24 |
| #426 | D118: the relay keeps out-of-order KubeArmor lines (replay filter only at a reattach) | `dc388ce` 11-08 15:54:53 |

Merge times and OIDs come from `gh pr list --state merged --json number,title,mergedAt,mergeCommit`,
re-run while writing. The reviewer merged every PR on a reviewer OK, green CI and the evidence the
owner asked for. #424 merged without the owner's read-through, on the owner's word (15:32Z).

**Status:**

- **D118 is kind-proven** (`.samples/run12-kind/lab-run12.log`, raw line numbers):
  - the operator relayed **10143** sandbox lines (:90), counted in the operator's FULL container log
    including rotated files (`run12-operator-full.log`, 34535 lines);
  - the KubeArmor agent on the sandbox's node logged **10143** sandbox lines in the run window (:92);
  - positive control, same counting before the fix: run 10, source 9092, relayed 6904
    (`scratchpad/i227-sim.log`);
  - the Loop Succeeded (:39), `forbidden=0` (:52), and the cluster was restored to the `main` image (:105).
  - The reviewer's mutation (the old per-line filter put back) fails both new specs 3/3.
- **D117 is kind-observed** (run 12): the message's 123 unattributed alerts are exactly the live `gitea`
  lines logged after the relay started at 15:50:39Z. The 1877 replayed earlier-run lines (1508 from
  run 11's verify pod `dhqvz`, 369 from `gitea`) were emitted but not counted.
- **No P1 open.** C1 (multi-arch images) is still an owner push.

## Closed

**Ticked by this commit:**
- R50 I230 by `36e134f` (#423); I231 by `a3a7a01` (#422); I232 by `f4faffe` (#425).
- R49 I220 by `8b141d4` (#424): its rule is now in the AGENTS.md test norms.
- R49 I227 by `dc388ce` (#426), see below.

**Answered, closed by this round:**
- **R49 I222** (egress-proxy records): run 10's task made one external fetch. The egress proxy recorded
  `connect proxy.golang.org:443 blocked reason=not-allowed` (`run10-kind/lab-run10.log:122`), and
  `suggest` listed `network: "proxy.golang.org:443"` (`run10-kind/p6b3-suggest.yaml:217-218`). The
  earlier runs' agents made no external connect. Acceptance met; no code change.
- **R49 I227** (rotation): the cause of the missing sandbox lines was not rotation. It was the relay's
  per-line replay filter (`if !al.Time.After(*since) { continue }` on every line), and KubeArmor's
  timestamps are not monotonic (30% of the lines in run 10's agent log are earlier than a line before
  them). Simulating that filter over run 10's 9092 source lines keeps exactly the 6904 relayed. Fixed
  by D118 (#426); acceptance (relayed == source) met by run 12.

**Still open:**
- **R49 I228:** worse than R50 recorded, now that the relay keeps every line. See I237.
- **R49 I221, I225, I226; R50 I234, I235:** ticked when this round merges (answered or continued below).
- **R47 I213; R46 I206, I207, I211:** unchanged.

## P2

- [ ] **I236: `cox policy suggest`'s documented input loses everything before the operator's last log rotation**

  **Where:** `docs/OPERATOR.md` ~948 (`kubectl logs -n coxswain-system deploy/coxswain-controller-manager | kubectl cox policy suggest <loop> -f -`), and the same line in `docs/RELEASE-NOTES-next.md` ~55. Evidence: run 12.

  **Problem:** the operator writes every audit record to its own stdout. In a learn-mode run that is
  thousands of lines a minute: run 12's operator container log reached 34535 lines (11 MB) in about four
  minutes and rotated during the run. `kubectl logs` returns only the current file. Run 12's `suggest`,
  fed that way, printed an empty `spec:` with exit 0 (`lab-run12.log:80-81`). Fed the full rotated log,
  it printed 17 exec, 64 files and a network entry (`run12-kind/run12-suggest-full.yaml`). A user
  following the docs gets an empty or partial candidate and no warning.

  **Fix:** two parts.
  1. `suggest` exits non-zero with a message when no audit record for the Loop is in the input
     (the "a fallback surfaces its failure" norm).
  2. The documented input must cover the whole learn run. That is an owner decision, D119.

  **Acceptance:** (1) a spec with input holding no record for the Loop exits non-zero; (2) per D119.

- [ ] **I237: `suggest` does not collapse a wide, shallow tree (continues R49 I228)**

  **Where:** `internal/policy/suggest.go`, the files collapse (#411/D112, #420/I228). Evidence:
  `run12-kind/run12-suggest-full.yaml`.

  **Problem:** with the relay now keeping every line, run 12's candidate has 64 files entries plus
  "418 more files entries were observed but not listed" (:97). 462 of the observed files are under
  `/scratch/.cache/go-build/<xx>/`, spread over 224 two-hex subdirectories holding 1 to 5 files each
  (87 with 1, 69 with 2, 40 with 3, 23 with 4, 5 with 5). The collapse fires only when one directory has
  more than 5 children, so no subdirectory collapses, and the recursive step (#420) climbs only from
  already-collapsed directories, so `/scratch/.cache/go-build/` never does.

  **Fix:** decide a collapse on the number of observed files under an ancestor (descendants), not only
  its direct children, so a directory with many small subdirectories collapses to one recursive entry.

  **Acceptance:** `run12-suggest-full.yaml`'s input (the full log, CRI prefix stripped) yields 20 or
  fewer files entries with no overflow; the run-8 fixture still passes; the candidate dry-runs against
  the real CRD on kind-ref.

- [ ] **I238: the builders' slips this window (continues R50 I234)**

  **Where:** log 13:04Z to 15:40Z.
  - **pi3:** (a) its C1 audit concluded that, without `--platform=$BUILDPLATFORM`, the proxies' builds
    would produce amd64 binaries. They run the arm64 build stage under emulation. Caught before the patch.
  - **b4:** none recorded. The D118 report line, scope and testdata (byte-identical to the captured
    lines) matched the brief.

  **Acceptance:** (a) does not recur in R52's window.

- [ ] **I239: the reviewer's own slips in this window**

  **Where:** the log entries named below.
  - **(a) An inconclusive acceptance run:** run 11 counted "relayed" from `kubectl logs` of the operator
    (only the 30 s after its last rotation, first line 15:48:24Z for a 15:40:50Z start) and "source" over
    every run still in the agent's log (32k lines). It read 0 against 32194 and proved nothing. Run 12
    counts relayed from the full container log, including rotated files, and source only in the run
    window. Rule: a count from a container log names the files it read and the window.
  - **(b) An unverified cause in user docs:** `docs/QUICKSTART.md` ~484-486 says that with the agent on
    stdout the log rotated every ~20 s "and the Loop's own sandbox events were missing from the
    operator's stream". The reviewer accepted that wording in #412; the shown cause is D118's filter
    (I227 above), not rotation. See I240.
  - **(c) A missed empty result:** run 12's `suggest lines: 10 exec: 0` (:81) was not read as a failure
    until this round was written (I236).
  - **(d) A test diff not read before merge:** #426 was reviewed from its `relay.go` diff, the mutation
    and the kind run. Its rewrite of `TestRelayReplayedLineIsNotCountedTwice` was not read and
    turned out vacuous (I243). Rule: read every changed test before the verdict; a changed spec's
    stated mutation is run.

  **Acceptance:** none of (a)-(d) recurs in R52's window.

## P3

- [ ] **I240: QUICKSTART blames rotation for the missing sandbox events**

  **Where:** `docs/QUICKSTART.md` ~484-486.

  **Problem:** see I239(b). The visibility advice that follows stays right (cluster-wide visibility
  floods the agent log), but the stated cause of the missing events is disproved by run 10's simulation
  and run 12.

  **Fix:** drop the "missing from the operator's stream" clause, or say the volume is the reason to
  scope visibility. Grep `docs/` for "rotat" first and fix every copy.

  **Acceptance:** no doc names log rotation as the cause of missing relayed events.

- [ ] **I241: `PolicyBlocked`'s "latest target" is the latest alert of any verdict**

  **Where:** `internal/alerts/relay.go` ~127-131 and ~194 (`LatestTarget`, set for every accepted
  alert), and `internal/controller/loop_audit.go` ~136-142 (`blockedActionsMessage`).

  **Problem:** run 12's message reads "the engine blocked 2 action(s) for this Loop; latest target
  /tmp/termination.log" (`lab-run12.log:53`). The two blocks were the egress proxy's
  `proxy.golang.org:443` connects. `/tmp/termination.log` is an ALLOWED KubeArmor file event from the
  verify pod. Its field comment says the target tells "WHAT was bounced", but in learn mode thousands of
  allowed lines overwrite it. "The engine blocked" is also wrong when the blocks are the proxy's.

  **Fix:** set `LatestTarget`/`LatestAt` only from blocked alerts; word the message as "blocked N
  action(s)" (or name the source).

  **Acceptance:** a spec with a blocked alert followed by allowed ones reports the blocked target.

- [ ] **I242: the relay's `sinceAtMax` grows with out-of-order lines (from #426's review)**

  **Where:** `internal/alerts/relay.go`, `follow()`, the `else` branch of the max update.

  **Problem:** every line with `al.Time <= since` is added, including lines strictly earlier than the
  max. They are harmless (lines before the boundary are skipped by time) but the set grows until the
  max advances.

  **Fix:** add only lines whose time equals the max.

  **Acceptance:** the two D118 specs still pass, and the D118 mutation still fails them.

- [ ] **I243: `TestRelayReplayedLineIsNotCountedTwice` no longer tests a replayed Block (#426)**

  **Where:** `internal/alerts/relay_test.go`, `TestRelayReplayedLineIsNotCountedTwice` (rewritten by
  #426, `dc388ce`).

  **Problem:** the second attach re-reads only the Allow line, and Allow lines never change `Count`.
  The spec's comment says "Mutation: remove the dedup/replay check → the count is 2". With exactly that
  mutation (the three replay `continue`s disabled, `scratchpad/d118-nodedup.py`), the spec PASSES 3/3,
  and `TestRelayReattachSkipsOnlyReplayedLines` FAILS 3/3. Coverage holds through the new spec; the old
  one is vacuous and its comment is false.

  **Fix:** make the Block the boundary line: attach 1 delivers the Allow (t1) then the Block (t2);
  attach 2 re-reads the Block. Keep the comment's mutation claim only if it is then true.

  **Acceptance:** the `d118-nodedup.py` mutation fails the spec with `count = 2`.

## Owner decisions

**Decided in this window:**
- **D117**, 11-08: replayed relay lines (older than the relay's start, pod no longer resolves) are
  emitted but not counted as unattributed. Merged in #425; observed on kind in run 12.
- **D118**, 11-08 (P1, fix now): the replay filter applies only at a reattach boundary, with an
  exact-duplicate dedupe; spec from a real captured sequence; kind acceptance relayed == source.
  Merged in #426.
- **Kind-ref investigations for R49 I222 and I227:** approved (11-08); runs 10-12.
- **AGENTS.md test norms:** approved, then merged without the owner's read-through on the owner's word.
- **C1:** a builder prepared the patch locally; the owner asked the reviewer to push it. The push was
  rejected (`refusing to allow a Personal Access Token to create or update workflow
  .github/workflows/release.yml without workflow scope`), and the reviewer stopped there.

- **D119**, 11-08 ~16:40Z: option **(b)**, recorded below with the question as asked.

**Open:**
- **D119 (I236), decided (b):** where should `suggest`'s input come from, given the operator log rotates during a
  learn run? Options: (a) document reading the node's rotated files, or a log pipeline, as the input;
  (b) the operator also keeps a bounded per-Loop summary of distinct allows (for example in a
  ConfigMap or the Loop's status) for `suggest` to read; (c) the operator stops writing ALLOWED
  KubeArmor lines to its stdout in learn mode and keeps only the summary. Reviewer recommendation: (b),
  so `suggest` no longer depends on log retention, keeping the audit lines as they are.
- **C1:** the owner pushes `slice/c1-multiarch` from `../coxswain-c1`; the arm64 proof is the next
  release run's `docker buildx imagetools inspect`.
- **R47 I213:** waiting on the homelab side.

## What I did not verify

- **Why run 3's ref-worker2 agent logged no Loop events** until 16:57:44Z (R49); not recurred since.
- **CRD validation ratcheting** for already-stored `files` entries (I224's release note says it is
  cluster-dependent).
- **The C1 images on arm64:** only the amd64 local builds were run.
