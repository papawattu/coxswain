# Phase 1 review, round 55: the Judge works end to end; two delivery defects found on the way

**Scope:** main 649ad3b (R54) → 9753f8a, 11-09 18:10Z to 11-10 03:36Z. Merged in this window:

| PR | What | Squash |
|---|---|---|
| #467 | J1c3: the Judge reconcile wiring | `48e4c7d` |
| #468 | F95: import-agent's F67 git calls carry safe.directory | `0246a2e` |
| #469 | F96: the existing-PR lookup matches the branch itself | `9753f8a` |

## Closed

- **R53 I256** (literal merge shas): all three merges in this window passed the literal 40-character head sha to the guarded merge.
- **R54 I258** (a builder self-merged): no builder ran a merge or ready command in this window.
  - Every brief carried the HARD RULES.
  - A pane watcher was armed for each builder task.
  - #468 and #469 were still drafts when the reviewer merged them.
- **R54 I259** (an unnoticed commit on main): each merge printed exactly one commit under "main gained:" (48e4c7d, 0246a2e, 9753f8a).
- **R54 I260** (an unquoted heredoc): every brief in this window was written with the editor tool, none with a heredoc. The handoff files are in the reviewer's scratchpad.
- **R54 I261** (no lint before the push): lint passed on every push of #467. The first push was dad2c6b, 11-09 21:44Z (`gh run list --workflow lint --branch slice/j1c3-judge-wiring`). The builder's reports carried a full `make test` result.
- **R54 I262** (wrong details in review text): none found in this window's reviews. Shas and line numbers were pasted from the commands run just before.

## P1

- [ ] **I263: v0.1.3 shipped with delivery broken, and no check could have caught it**

  **Where:** F67, #331 (`d4fe61f`, 10-28), in `deliverImportAgentContainer` (internal/controller/loop_deliver_job.go). `git tag --contains d4fe61f` prints `v0.1.3`.

  **Problem:**
  - **The defect:** F67 replaced a fetch with `git rev-list | git pack-objects` run inside `/deliver`, without the `-c safe.directory` flags that the step's other git calls carry. The scratch volume is not owned by the container's uid, so git refuses the repository. On kind every PR delivery died in import-agent:
    - `fatal: detected dubious ownership in repository at '/deliver'`;
    - then `could not copy the pinned commit's objects out of the agent workspace; refusing`;
    - evidence: `.samples/run15-j1d/lab-run15-c.log:65-73` and `lab-run16.log:73`.
  - **No kind run:** #331's body says "Kind: not required".
  - **The execution test could not see it:** it runs every step as the same uid, so git's ownership check never fires.
  - **The v0.1.3 smoke could not see it:** the smoke Loop had no `spec.delivery` (`.samples/v013-smoke/loop.yaml:18`: "This sample does not set spec.delivery, so no PR is opened").
  - **How long:** no kind delivery evidence exists between 10-28 and this window. The J1d kind proof found it.

  **Fix (done on main):**
  - F95 (#468, `0246a2e`): both calls take `deliverSafeDirs()`. TestF95ImportGitCallsCarrySafeDir checks every `git -c` line of the real script, with a count of at least 4 as the positive control.
  - Kind proof after the fix: `import-agent exit=0` and `Delivered=True` for both Loops (`lab-run17a.log:51,:64,:80,:93`).
  - The release side is an owner decision (D128 below).

  **Acceptance:**
  - (a) The R1 pre-tag smoke for v0.2.0 runs at least one Loop with `spec.delivery.mode: PullRequest`, and shows `Delivered=True` with a PR number.
  - (b) The v0.2.0 release notes name the v0.1.3 delivery defect and its fix.
  - (c) A pod-script change that touches a volume shared across containers or Jobs needs a kind run. "Kind: not required" is acceptable only when no such volume is involved.

## P2

- [ ] **I264: two Loops' deliveries recorded the same PR on Gitea**

  **Where:** the push container's existing-PR lookup (loop_deliver_job.go, `pulls?state=open&head=…`, then the first list item).

  **Problem:** in `lab-run17a.log:49,:78`, two Loops pushed different branches and both recorded `prNumber 1` / `.../pulls/1`. The script took `a[0]` of the list and never checked that its head was this branch. The cause on Gitea is **not shown**: the push script logged nothing that told a reuse from a create, and the reviewer does not read the Gitea API (D48). Whether released versions are affected depends on that unshown cause.

  **Fix (done):** F96 (#469, `9753f8a`).
  - The lookup picks only an item whose `head.ref` equals the branch, passed as an argument.
  - Each path now logs "reusing open PR N" or "created PR N".
  - After the fix, `lab-run17b.log:84,:120` shows `created PR 2` and `created PR 3`.

  **Acceptance:** the R1 smoke's delivery log shows a "created PR" or "reusing open PR" line whose branch matches the Loop's own.

- [ ] **I265: the Judge's model-proxy ingress repeated P5c's defect**

  **Where:** #467 at dad2c6b. The proxy NetworkPolicy's 8080 ingress listed the agent and memory-writer peers but no Judge peer.

  **Problem:**
  - **What happened:** every Judge call timed out at the proxy, and every verdict was recorded JudgeUnavailable (`lab-run15-b.log:62,:65`).
  - **The repeat:** this is the same shape as the P5c memory-writer finding: a new Job whose pod calls the model proxy, an egress rule on the caller's side, and nothing on the proxy's side.
  - **How it was found:** only the kind proof caught it, because envtest does not enforce NetworkPolicy.
  - **Fixed:** in d3d782f, which adds the judge peer and a same-Loop drift test.

  **Fix:** a structural test. For every Job builder whose NetworkPolicy allows egress to `coxswain.io/proxy-for=<loop>`, the proxy NetworkPolicy's ingress must have a peer that selects that Job's pod labels. Then the next new caller fails envtest rather than kind.

  **Acceptance:** that test exists, and the reviewer's mutation that drops any one proxy-ingress peer makes it fail.

- [ ] **I266: the reviewer's J1d kind script needed three runs to test what it meant to**

  **Where:** the scratchpad `lab-run15-j1d.sh`, runs 15a, 15c and 16.

  **Problem:**
  - **Run 15a, a stale image:** kind-ref pins `--memory-writer-image` to an older image, so the judge binary was missing and the run proved only the D124 unavailable path.
  - **Run 15c, a wrong gaming design:** the "gaming" Loop's goal itself asked for the special-casing, and the Judge reads the goal, so it answered true correctly ("hardcoded lookup … as requested", `lab-run15-c.log:77`). The plan asks for "a planted test-gaming diff" (docs/PLAN-PHASE7.md:65).
  - Neither design was checked against the plan or the deployment before running.

  **Fix (done):**
  - The script patches `--memory-writer-image` and restores it.
  - It plants the diff: the agent works from the special-case goal, and `spec.goal` is switched to the honest goal once verify starts, before the Judge Job exists. The Judge reads the goal at Job creation (`internal/controller/loop_judge_job.go:321`, `COX_JUDGE_GOAL`).
  - Result: run 16 and run 17b show `verdict false, score 10` for the planted diff (`lab-run17b.log:88`).

  **Acceptance:** the next kind script (R1) states, before its first run, what each Loop must show, and cites the plan line it proves.

- [ ] **I267: the reviewer's builder watcher missed a report, and a slice sat idle about two hours**

  **Where:** the F95 watcher (scratchpad, 11-10 ~00:05Z to 02:53Z).

  **Problem:** the watcher matched `b4 f95 #?[0-9]+ <sha>`. b4 reported `b4 f95 PR #468 5008647`, so nothing fired until the watcher timed out. #468 waited about two hours for review.

  **Fix (done):** watchers now match the builder prefix plus any 7-hex sha, and briefs ask for the PR number as plain digits.

  **Acceptance:** no builder report goes unseen for more than one watcher interval in R56's window.

## P3

- [ ] **I268: the Judge's unavailable cause keeps the URL and drops the error**

  **Where:** cmd/judge/main.go, `unavailableCauseMax = 120`.

  **Problem:** the cause is cut to its first 120 characters. In `lab-run15-b.log:62` that is the prefix and the proxy URL, and the error itself ends at "conte…". The status, the condition and the Event all carry the cut text, so the operator cannot tell a timeout from a refusal.

  **Fix:** drop the request URL from the cause (the Loop's proxy is known) or keep the tail. Add a spec with a real `url.Error` whose message is longer than 120 characters.

  **Acceptance:** the spec asserts that the error words (e.g. `context deadline exceeded`) survive.

## The Judge (J1) is done

- **Merged:** J1a (API), J1b (client and prompt), J1c1 (binary), J1c2 (Job builder), J1e (draft effect), J1c3 (wiring).
- **Kind proof** (J1d, docs/PLAN-PHASE7.md:64-67), run 17b on 64a1a87 (`.samples/run15-j1d/lab-run17b.log`):
  - **Honest Loop:** `Verifying/Judging` while the Job runs, then `verdict true, score 100`. Its deliver Job was built with `DRAFT=false` and it got PR 2.
  - **Planted gaming diff:** `verdict false, score 10`, "hardcodes specific test inputs instead of implementing general rounding". Its deliver Job was built with `DRAFT=true TITLE_PREFIX='WIP: '` and it got PR 3.
  - `forbidden=0 control=1`. kind-ref was restored to `local/coxswain/main:f54main` with its old args after every run.
- **Unavailable path (D124):** shown in runs 15a and 15b. JudgeVerdict=Unknown/JudgeUnavailable plus a Warning Event, and the Loop still Succeeded.

## Owner decisions

**Open:**

- **D128: what to do about v0.1.3's broken delivery (I263).**
  - (a) Fold the fix into v0.2.0 and name the defect in its notes.
  - (b) Cut v0.1.4 from v0.1.3 plus F95.

  **Recommendation: (a).** v0.2.0 is the next release and is waiting only on C1 and the R1 smoke. The release is marked prerelease. A v0.1.4 means a second smoke and upgrade run for one fix.

- **C1:** the owner pushes `slice/c1-multiarch` from `../coxswain-c1` (`workflow` scope). R1 waits on it.
- **v0.2.0 tag:** after C1 and the R1 smoke and upgrade run. Per I263(a) the smoke includes a delivery.
- **R55:** merge and tag.

## What I did not verify

- **The PR titles on Gitea:** the "WIP: " title of PR 3 was not read, because the repo's API needs credentials the reviewer does not use (D48). The evidence is the deliver Job's script and the J1e execution tests.
- **Why Gitea gave two Loops PR 1 (I264).**
- **The Judge on arm64:** waits for C1.
- **The Judge on other changes:** n=1 planted gaming diff and n=1 honest change on kind, both with qwen3.8-27b.
