# Phase 1 review, round 52: R51's fix queue lands overnight, suggest becomes usable, D119 starts

Round 52 was written by the reviewer on 2026-11-09 at ~03:30Z, overnight, on the owner's standing
approval ("keep going … I approve your recommendations", 11-08 ~23:47Z). Its sources are the reviewer's
log (`/tmp/claude-1000/.../scratchpad/overnight-20261009.md`, the entries from 23:48Z 11-08), the
kind-ref evidence under `.samples/run12-kind/` (local, never committed), and commands re-run while
writing. It follows R51 (`docs/REVIEW-PHASE1-R51.md`, merged as **`6fd88c5`** in PR #427, 11-08
23:48Z, tagged `review/phase1-r51`; its last ID is I243, its last decision D119).

| PR | Item | Merge |
|---|---|---|
| #428 | I241: PolicyBlocked's latest target is the latest block | `99fca31` 11-09 01:30:44 |
| #431 | I240: docs stop blaming rotation for missing sandbox events | `bde941d` 11-09 01:31:10 |
| #429 | I243/I242: replay spec re-reads a Block; sinceAtMax only at the max | `2478eb8` 11-09 01:40:19 |
| #430 | I236(1): suggest fails on input with no record for the Loop | `10e96cd` 11-09 02:12:55 |
| #432 | I237: suggest collapses a wide, shallow tree | `4e52203` 11-09 02:13:07 |
| #434 | suggest: stable paths for per-run file names (I245) | `b83646c` 11-09 02:44:03 |
| #433 | cli: suggest -f runs offline (I244) | `35fc411` 11-09 02:57:33 |
| #435 | D119(1): per-Loop learn summary in the relay | `a5f7720` 11-09 03:09:26 |

Merge times and OIDs come from `gh pr list --state merged --json number,title,mergedAt,mergeCommit`,
re-run while writing. Every code PR was merged on a reviewer comment review, green CI, the reviewer's
own mutation run, and a full `make test`. For #433 and #434, `make test` ran on the PR head merged
locally with the then-current main (they were cut before #430/#432 landed).

**Status:**

- **`cox policy suggest` on real data,** main at `b83646c`:
  - run 12's full operator log gives **20** files entries, **0** overflow
    (`.samples/run12-kind/run12-suggest-main-b83646c.yaml`), against 64 + 418 overflow at R51;
  - it is `created (server dry run)` on kind-ref;
  - run 8's log gives **21**, 0 overflow.
- **D119(b):** step 1 (the relay's in-memory per-Loop learn summary) is merged (`a5f7720`, #435;
  reviewer mutations m1-m3 caught, `make test` ok). Step 2 is with b4. Steps 2 (an owned ConfigMap), 3 (suggest reads it) and 4 (docs + kind run 13) are briefed or designed
  (`scratchpad/d119-design.md`).
- **No P1 open.** C1 (multi-arch images) is still an owner push.

## Closed

**Ticked by this commit:**
- R51 I240 by `bde941d` (#431), I241 by `99fca31` (#428), I242 and I243 by `2478eb8` (#429).
- R51 I237 by `4e52203` (#432; plus `b83646c` #434 for the last entries). Acceptance: run 12 at ≤ 20
  with no overflow, met at `b83646c` (20, 0).

**Still open:**
- **R51 I236:** part 1 is merged (`10e96cd`, #430: suggest exits non-zero, naming log rotation, when the
  input holds nothing for the Loop); part 2 is D119, in progress.
- **R49 I228:** run 8 yields 21 files entries against its ≤ 20. See D120.
- **R51 I238, I239:** continued below as I246 and I247.

## P2

- [ ] **I244: a kubeconfig error masked every offline CLI error (fixed in #433)**

  **Where:** `internal/cli/cli.go` ~103, the root `PersistentPreRun`.

  **Problem:** the pre-run builds client-go deps for every command. When that fails, it prints the
  error and sets `cmd.SilenceErrors = true`, so the command's own error is never shown. On this devbox
  `cox policy suggest -f <file>` with input holding no records printed only `Error: invalid
  configuration: [context was not found for specified context: homelab, …]`, never #430's message.

  **Fix (done, #433):** `suggest` is annotated offline and skips the deps. Other commands keep the
  pre-run, and the masking stays for them; their own errors are usually caused by the missing
  kubeconfig, but not always.

  **Acceptance:** met for `suggest` (mutation: `policy_test.go:77: kubeconfig error leaked into the
  output`). For the other commands, the owner's call (D121).

- [ ] **I245: `suggest` listed per-run file names (fixed in #434)**

  **Where:** `internal/policy/suggest.go`. Evidence: run 8 and run 12 candidates.

  **Problem:** two kinds of entry are different on every run:
  - `/workspace-creds/..2026_11_07_21_02_45.2174268029/{username,password}` (a Secret volume's
    atomic-writer directory);
  - `/memory/<sha256>.txt` (memory files named by content hash).

  An enforced candidate would deny the next run's paths.

  **Fix (done, #434):** `stableFilePath` maps them to `/workspace-creds/` and `/memory/`.

  **Acceptance:** met (`TestSuggestFromRecordsStablePaths`; run 8's candidate dry-runs on kind-ref).

- [ ] **I246: the builders' slips this window (continues R51 I238)**

  **Where:** log 11-09 00:5xZ to 03:00Z.
  - **b4:**
    - (a) edited `internal/policy/suggest.go` in the SHARED tree (cwd `/home/jamie/projects/coxswain`,
      on main) instead of its `../coxswain-i237` worktree. The uncommitted change blocked pi3's `/new`
      ("Commit your changes first"). The reviewer interrupted b4, saved the diff
      (`scratchpad/b4-i237-sharedtree.patch`), applied it to the worktree and restored the shared tree;
    - (b) its first I237 implementation double-counted collapsed directories (`/s/c/` = 28 for 14
      files). Caught by the reviewer's scratch count test, then fixed in `c24c46e`;
    - (c) folded a test addition into a merge commit when the brief asked for its own commit (#433).
  - **pi3:**
    - (d) left a duplicated doc comment in `relay.go` (#435). Fixed in a follow-up.

  **Fix (in force):** every brief says "Never edit /home/jamie/projects/coxswain itself", and the
  reviewer checks `git status` of the shared tree after each `/new`.

  **Acceptance:** none of (a)-(d) recurs in R53's window.

- [ ] **I247: the reviewer's own slips in this window**

  **Where:** the log entries named below.
  - **(a) A full suite without the suite's timeout:** the base run for #428 was a bare
    `go test ./internal/... -count=1`, without `make test`'s `-timeout 20m`. It hit `panic: test timed
    out after 10m0s` under load, after every spec had passed, and left 6 envtest orphans (3
    apiserver + 3 etcd with ppid 1; 3 more were older, from builders' runs). All 6 were killed by PID,
    and every later base run used `make test`.
  - **(b) A mutation that did not compile:** the first m1 for #435 removed the only use of `slices`,
    so it built nothing and printed nothing. Redone so the import stays used (`|| true`), which failed
    `TestLearnRelevant/verify-pod_File`. Rule: a mutation's output must show a FAIL line, never empty.
  - **(c) A void base run:** #433's first `make test` was meant to run on the head merged with main.
    The merge conflicted and was aborted, and the script still ran `make test` on plain main and
    reported rc=0. The script now skips the test on a conflict.

  **Acceptance:** none of (a)-(c) recurs in R53's window.

## P3

- [ ] **I248: `stableFilePath` would map an atomic-writer directory directly under `/` to `/`**

  **Where:** `internal/policy/suggest.go`, `stableFilePath`.

  **Problem:** `/..2026_…/x` returns `/`, a whole-filesystem recursive allow. A Secret volume cannot
  mount at `/`, so this cannot happen today.

  **Fix:** return `p` unchanged when the prefix before the segment is empty.

  **Acceptance:** a table row `/..2026_01_01/x` → unchanged.

- [ ] **I249: the relay never forgets a deleted Loop's state**

  **Where:** `internal/alerts/relay.go`: `Holder.loops`, and (since #435) `Holder.learn`.

  **Problem:** neither map is pruned when a Loop is deleted. `loops` holds a small Snapshot per Loop;
  `learn` holds up to 20000 records per Loop, so a long-lived operator that runs many learn-mode
  Loops grows without bound.

  **Fix:** D119 step 2's brief adds `Holder.Forget(uid)` (both maps) on the Loop's deleting path.

  **Acceptance:** an envtest spec deletes a Loop and asserts its summary and snapshot are gone.

## Addendum (11-09 ~06:45Z): D119 step 2 lands; two more fixes

**Merged after the round was written:**
- #440 (I250, 7edb4e8), #441 (I251, 991373b), #439 (D119 step 2, 1d73ff9).
- I249 is addressed by #439: `Holder.Forget(uid)` runs on the deleting path (`internal/controller/loop_learn_summary.go:513`). It is ticked in R53.

**D119 step 2 on kind:**
- **Run 13** (`.samples/run13-kind/lab-run13.log`, head faa4571): the ConfigMap mechanics held (owner, GC, can-i, forbidden 0). But the 900 KiB bound kept 2684 records and dropped 2771. Lowest-count-first eviction dropped the one egress connect (`proxy.golang.org:443`) and `/bin/base64`, so the ConfigMap candidate differed from the full-log candidate. CHANGES.
- **Fix:** compact the File records the way `suggest` does, then evict by class (file, process, connect).
- **Run 14** (`.samples/run14-kind/lab-run14.log`, head ef0f153, CLI 991373b):
  - 13007 relayed records → 359 summary records, dropped 0 (:81, :86);
  - **the exec 19/19, files 26/26 and network 1/1 sets are equal** (:97-99);
  - the dry-run was created (:100), and the ConfigMap is gone after the Loop is deleted (:103).
  - The eviction order was **not** exercised on kind (dropped 0). Unit specs and mutations m1/m4 prove it.

### P2

- [ ] **I250: the chart wiring check flaked on SIGPIPE (fixed in #440)**

  **Where:** `hack/chart-drift-check-test.sh`, the F87 wiring check.

  **Problem:** `make -n | grep -q` under `pipefail`: grep exits at its first match, make gets SIGPIPE (rc 141), and the check prints a false FAIL. Repro 1/30. It failed one `make test` (#437).

  **Fix:** read `make -n` once into a variable. Under 6 CPU burners it went from 43/200 FAIL to 0/200.

  **Acceptance:** met in #440.

- [ ] **I251: `suggest` trusted any ConfigMap named `<loop>-learn-summary` (fixed in #441)**

  **Where:** `internal/cli/client.go`, `ReadLearnSummary`.

  **Problem:** a planted ConfigMap would become a candidate AgentPolicy handed to a human.

  **Fix:** require a controller ownerReference to the Loop (kind, name, uid). Otherwise refuse:
  "… is not owned by Loop …; refusing to use it (it was not written by the operator)".

  **Acceptance:** met in #441 (table test plus the uid mutation; run 13 and run 14 accepted the real ConfigMap).

- [ ] **I252: the builders' slips after the round (continues I246)**
  - **(a) pi3, #439 d119-2c:** the brief said "Count = the summed count", but the code counted records (`targets[t]++`). A stored count of 300 plus one new path became 2 on the next write. Process records and connects also shared a rank, so a count-1 connect was evicted before count-5 Process records. Both were shown by a reviewer probe that calls the functions directly, and fixed in ef0f153 (mutations m3/m4).
  - **(b)** The brief's spec for (a) had no test for the stored-count path, and the builder added none. A test fixture must include a record whose `Count` is not 1.

  **Acceptance:** none recurs in R53's window.

- [ ] **I253: the reviewer's slips after the round (I247 recurred)**
  - **(a) A mutation that did not compile, again (I247(b)):** my m2 for #439 (`if !changed`) left `changed` unused. VET/BUILD FAIL, not a spec failure. Redone with `|| true`.
  - **(b) A focus that ran zero specs:** my `-ginkgo.focus "UPDATE"` did not match (the focus is case-sensitive). Re-run with "updates it on the second". Rule: a focused mutation's output must show `Ran 1 of`, not `Ran 0 of`.
  - **(c) A brief command with the wrong base:** the d119-2c brief said `git worktree add /tmp/wt-d1192c HEAD` without a `cd`. Run from the shared tree, HEAD was main. pi3 caught it and re-ran it from the PR worktree. Briefs now `cd` into the PR worktree first.

  **Acceptance:** none of (a)-(c) recurs in R53's window.

### P3

- [ ] **I254: an ownerReference is forgeable by anyone who can write ConfigMaps in the namespace**

  **Where:** `internal/cli/client.go`, `learnSummaryOwnedBy` (I251).

  **Problem:** a Loop's uid is readable by anyone who can get Loops. A namespace writer can create `<loop>-learn-summary` with a matching controller ownerReference, and the check accepts it. I251 stops stray and foreign ConfigMaps, not a deliberate namespace writer.

  **Fix:** document the trust boundary (OPERATOR.md and TROUBLESHOOTING.md): the summary is as trustworthy as the namespace's ConfigMap writers, and the candidate is always reviewed by a human before it is applied.

  **Acceptance:** the docs name the boundary.

- [ ] **I255: #439's small leftovers**
  - `loop_controller.go:305` says CompareAndDelete; the code uses `Load`/`Store` (`loop_learn_summary.go:476-477`).
  - `loopNameTakenSummaryEvents` is keyed by UID and never pruned on delete: a small leak per deleted Loop that hit `LearnSummaryNameTaken`.
  - `compactLearnFileRecords`' `rawByEntry` loop is O(raw × compacted) per write.

  **Fix:** correct the comment, delete the UID in the Forget path, and build `rawByEntry` by walking each raw path's ancestors.

  **Acceptance:** a spec deletes a Loop that hit `LearnSummaryNameTaken` and asserts its UID is gone from the map.

## Owner decisions

**Decided in this window:**
- **Overnight standing approval** (11-08 ~23:47Z): continue without asking; the reviewer's
  recommendations are approved. It covered the R51 merge and tag, I236-I243, and D119(b).

**Open (reviewer recommendations, approved overnight, for the owner to confirm or reverse):**
- **D120 (R49 I228):** close I228 on "no overflow, stable paths, dry-run valid" rather than "≤ 20
  entries". Run 8's 21 entries (`scratchpad/main-run8.yaml`) are 8 `dir/` entries and 13 exact
  paths. 3 of the exact paths are directory-self entries next to their `dir/` (`/memory`, `/workspace`,
  `…/telemetry/local`), and 10 are single files the agent read. Recommendation: close.
- **D121 (I244):** the CLI's other commands keep the masking pre-run. Recommendation: make the
  pre-run return its error (a `PersistentPreRunE`), so a command never runs with nil deps and never
  silences its own error.
- **C1:** the owner pushes `slice/c1-multiarch` from `../coxswain-c1`; the push needs the `workflow`
  scope, which the reviewer's token lacks.

## What I did not verify

- **Whether KubeArmor's `matchDirectories {dir: X/, recursive: true}` also covers the directory `X`
  itself.** `suggest` lists both `/workspace` and `/workspace/` (and `…/telemetry/local` with
  `…/local/`). If it does, the exact entry is redundant.
- **D119's eviction order on kind:** run 14 never hit the bound (dropped 0). Only unit specs and mutations show it.
- **The C1 images on arm64.**
