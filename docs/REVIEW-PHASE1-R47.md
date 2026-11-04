# Phase 1 review, round 47: D103 lands slice by slice, v0.1.3 ships, and a Loop with no model fails silently

Round 47 was written by the reviewer on 2026-11-04 at ~19:10Z. Its sources are the reviewer's log
(`/tmp/claude-1000/.../scratchpad/overnight-20261009.md`, lines 1659-1690, the 11-04 entries after
R46) and commands re-run while writing. It follows R46 (`docs/REVIEW-PHASE1-R46.md`, merged as
**`c5007de`** in PR #391, 2026-11-04 11:11:20Z, tagged `review/phase1-r46`; its last ID is I211,
its last decision D103). It covers the merges after R46's window, which ended at `2a2edeb`:

| PR | Item | Merge |
|---|---|---|
| #390 | F51b follow-up: kind-calico-up deploy block in one shell + pull third-party images | `2f33945` 10:57:11 |
| #391 | review(phase1-r46) | `c5007de` 11:11:20 |
| #392 | D103: the D38 probe measures a REJECT-style policy controller (no-policy control pass) | `a6f469b` 16:53:58 |
| #393 | Docs: v0.1.3 release notes (rename RELEASE-NOTES-next.md → RELEASE-NOTES-v0.1.3.md; fresh -next) | `a44c04d` 18:04:47 |

Merge times and OIDs come from `gh pr list --state merged --json number,title,mergedAt,mergeCommit`,
re-run while writing. The reviewer merged all four under the standing rule (OK + green CI +
evidence; owner decision, 2026-11-04).

The reviewer's verdicts on #392, in order:

| Slice | Covers | Verdicts (review IDs) |
|---|---|---|
| A | `combineVerdicts` / `decide` | CHANGES `5767952160` → OK `5768270718` |
| B | the two-pod `PodProber` | CHANGES `5769381433` → OK `5770332315` |
| C1 | `verify-cni.sh` | CHANGES `5770794200` → CHANGES → OK `5771237885` |
| C2 | `d38-cni-e2e.sh` | CHANGES `5771505842` → CHANGES `5771934524` → OK `5771985921` |
| D | docs | CHANGES `5772111522` → final OK + notes for the whole PR `5772135219` |

#393 got OK + notes (`5773283427`). Evidence is under `.samples/` (uncommitted, AGENTS I68).

**Status:**

- **main is green** at `a44c04d`: CI lint and test passed on the #393 head and the #392 head
  (`1e50e1b`).
- **v0.1.3 is released.** The tag `v0.1.3` is on `a44c04d`, and release run `41889467692` passed all
  6 jobs. The GitHub prerelease carries `install.yaml` and `coxswain-0.1.3.tgz`. All 5 ghcr images
  answer 200 at `:v0.1.3` with an anonymous pull token, and `install.yaml` names `:v0.1.3` for each.
  The pre-tag kind smoke passed on kind-coxswain (`main-a6f469b`, helm rev 47): Loop `v013-smoke`
  went Pending → Planning → Implementing → Verifying → Succeeded between 18:36:29Z and 18:40:03Z,
  with `forbidden=0` (control `1`) (`.samples/v013-smoke/smoke-r2.log`).
- **D103 merged with every mutation caught.** Each slice's mutations were re-run by the reviewer at
  the slice head: A1-A5, B1-B6, C1/C2/C5 and M1-M3. Every slice took at least one CHANGES round,
  because a rule was untested or a test did not run the real code (I214).
  - The lab Calico `make d38-cni-e2e` exited 0 on the merged head `91b989b`
    (`.samples/d38-e2e-20261104/d38-e2e-d103-final.log`).
  - kind-ref `verify-cni` passed across 3 nodes (`.samples/verify-cni-20261104/kind-ref-d103-91b989b.txt`).
- **No P1 in this window.**

## Closed from R46

Ticked in `docs/REVIEW-PHASE1-R46.md` by this commit:

- I205 by `2f33945` (#390).
- I209 by `a6f469b` (#392). The selftest's R46 I209 case checks the split FAIL counts.
- I210 by `a6f469b` + `a44c04d` (#392 replaced the "not yet verified" rows; #393 added the
  post-D103 homelab result).

**Not closed:**

- **I206.** Its acceptance asks for a homelab verdict of CNIEnforced or a named ProbeUnavailable
  with a measurable host-network target. The post-D103 run is UNPROVEN because of one node; I213
  carries it.
- **I207** is unchanged.
- **I208** has no fix PR yet.
- **I211.** The v0.1.3 notes do not carry a C1 (multi-arch) line. `grep -n -i 'multi-arch'
  docs/RELEASE-NOTES-v0.1.3.md` finds only `:52`, which is about the third-party image digests.

## P2

- [ ] **I212: a Loop that names no model endpoint gets a sandbox whose agent crash-loops in Planning, and no condition says why**

  **Where:** the v0.1.3 smoke run 1 (log 18:36Z). The reviewer applied
  `config/samples/loop-fix-failing-test.yaml` renamed to `v013-smoke`. The sample ships with
  `spec.agent`'s `endpointSecretRef` / `modelEndpoint` / `model` commented out
  (`config/samples/loop-fix-failing-test.yaml:65-71`). The Loop sat in Planning for 30 minutes,
  and the agent container went into `CrashLoopBackOff` (10 restarts). Its previous log is
  one line, `COX_MODEL_BASE_URL is empty; the runner requires the operator-set model proxy URL`
  (`runner/cmd/main.go:108`; `.samples/v013-smoke/agent-prev.log`). The Loop's conditions were
  RunnerAvailable, SecretsUsable, PolicyTranslationLossy, PolicyEnforced, NetworkEnforced,
  CheckpointsAvailable and PolicyBlocked. None of them names the model, and there was no
  `ModelAvailable` condition (`.samples/v013-smoke/loop-final-r1-nomodel.yaml`). The CRD
  accepts the Loop, because its only model rule is that `endpointSecretRef` and `modelEndpoint`
  are set together (`api/v1alpha1/loop_types.go:678`). The operator's `MissingModelEndpoint`
  check (`internal/controller/loop_controller.go:695-698`) fires only when the Secret is set and
  the endpoint is not.

  **Problem:**
  - This is the AGENTS rule "a fallback must surface its failure before it degrades" (R26 I90) in
    its plainest form. A user who applies the sample exactly as shipped gets a Loop stuck in
    Planning. The only signal is a pod event, and `kubectl cox describe` would not show it.
  - The runner image is the operator default (`--runner-image`) and always needs a model URL.
  - A Loop on the default runner with no model endpoint can never make progress, yet the operator
    creates the sandbox anyway.

  **Fix:** when the agent is the operator's runner and `spec.agent.modelEndpoint` is empty, set a
  condition (e.g. `ModelConfigValid=False reason=MissingModelEndpoint`, or a new reason) and do not
  create the sandbox. Or reject it at admission, if the runner/adapter split allows that to be
  decided there. Also make the sample's comment say plainly that the three fields are required
  before applying it.

  **Acceptance:** an envtest spec applies a Loop with no model fields on the default runner and
  asserts the condition, and asserts that no sandbox pod is created. The spec fails when the check
  is disabled (scratch-worktree mutation, recorded in the PR). The sample's header says the model
  fields must be set.

- [ ] **I213: the homelab k3s verify-cni with D103 is UNPROVEN only because of pi12, and no deployed operator has measured k3s yet**

  **Where:** `.samples/verify-cni-20261104/homelab-k3s-d103.txt` (log 16:56Z; #392
  issuecomment-6636659574), run from merged main `a6f469b` with the owner's approval:
  - **22 BLOCKED:** `APISERVER_SVC`, the 16 kubelet rows other than pi12, the 3 control-plane
    apiserver rows, `CROSS_NS_POD` and `EXTERNAL`;
  - **13 NOT_LISTENING:** the workers' apiserver rows;
  - **2 UNRESOLVED:** `APISERVER_NODE(192.168.1.52)` and `KUBELET_NODE(192.168.1.52)`.

  `192.168.1.52` is pi12, which is `Ready,SchedulingDisabled`. Its policy pass was `REFUSED` (file
  `:61-62`), and its no-policy control pass was `BLOCKED(OSError)` (file `:99-100`).

  **Problem:**
  - The D103 two-pass measurement works on this REJECT-style controller: every other node's
    targets are measured. But `verify-cni.sh` probes every node's host-network targets, so one
    node the control pod cannot reach keeps the cluster verdict UNPROVEN.
  - Why pi12 is unreachable from a pod is **not known**; it was not investigated.
  - The operator's own probe (`internal/cni/pod_prober.go`) runs on one node and has not been run
    by a deployed operator on k3s. "A coxswain run on a healthy node would report CNIEnforced" is
    an expectation, not evidence.

  **Fix:** the homelab side (the homelab Claude's H slices) finds out why a pod on pi3 cannot
  reach pi12's host ports even with no policy. D104 (below) is decided as (a), so pi12's rows stay UNRESOLVED until that is explained.

  **Acceptance:** either a homelab `verify-cni` verdict of PASS, or of a named UNPROVEN whose only
  unmeasurable rows are of a type D104 excludes. And the first deployed-operator run on k3s records
  its `NetworkEnforced` condition with the command and output.

- [ ] **I214: the D103 builder sessions repeated the R45/R46 slip classes in every slice; the reviewer's mutations caught each one before merge**

  **Where:** all from the log, 11-04:
  - **50% stop overrun:** slice B ran to 68.5% and then 68.7% with its work uncommitted (log
    12:57Z, 13:10Z); C2 reached 79% (log).
  - **The full suite run over and over:** slice B ran `make test` four times in a row. The
    builder's tool call times out before the ~10-minute suite finishes, so it restarts it (log
    13:10Z, 13:15Z).
  - **A theater test:** `TestProbeNonZeroExitInEitherPassIsUnavailable` stubbed the error it
    claimed to test (review `5769381433`, log 13:47Z).
  - **A single-node selftest:** slice C1's selftest covered one node, so the multi-node
    duplicate-label regression first showed on kind-ref with EXIT 2
    (`.samples/verify-cni-20261104/kind-ref-d103-45c3426.txt`, log 15:18Z).
  - **The host-network rule untested twice:** mutation A3 survived in Go (log 12:07Z), and C2
    survived in shell (log 15:41Z).
  - **The selftest bypassed the code under test:** C2's first selftest re-typed the verdict instead
    of calling it (mutations M1 and M2 survived, log 16:00Z). Its second forced FAIL on any
    NOT_LISTENING note, which masked `d38_hostnet_verdict` (M2 survived, log 16:29Z).
  - **A doc that contradicted the code:** slice D's `ProbeUnavailable` bullet said "refused by
    both", which the code combines to NOT_LISTENING (review `5772111522`).
  - **A briefed step skipped:** D's D88 grep comment was never posted (log 16:41Z).

  **Problem:**
  - Nothing reached `main`, but only because the reviewer re-ran every mutation at every slice
    head and ran the real scripts on real clusters.
  - Two of these classes were already named:
    - R45/R46 for the overrun and for claiming results;
    - R43 for tests that bypass the code under test.
  - The "a test must call the real code, never a copy, and must not override its result" rule
    (AGENTS, R28 I92 and R25 I84) was broken twice in one slice.

  **Fix:**
  - Briefs carry the lines that now work:
    - run `make test` once, to a file;
    - include multi-node cases;
    - write one isolating case per verdict rule;
    - "the selftest calls the sentinel-extracted block; no `got=` overrides".
  - A docs slice greps each claim against the code before pushing.
  - The reviewer keeps re-running mutations at each head.

  **Acceptance:** in the next code PR, the first push's selftest/spec set catches the reviewer's
  named mutations with no CHANGES round for an untested rule.

## P3

- [ ] **I215: the reviewer's own slips in this window**

  **Where:** log 16:34Z ("MY SLIP: sent /new to b4 while it showed Working"); log 18:36Z (smoke run
  1 applied the sample without its model fields); log 16:56Z (the homelab entry says "BLOCKED
  x21"; the file has 22, which the reviewer corrected in issuecomment-6636659574 after it was
  posted).

  The reviewer also asked the owner whether to deploy the D103 build on kind-coxswain "to show
  kindnet = CNIUnenforced unchanged". But kind-coxswain runs Calico:
  `kubectl --context kind-coxswain -n kube-system get ds` lists `calico-node` and `kube-proxy`.
  The smoke recorded `NetworkEnforced=True CNIEnforced` there. The claim about the cluster's CNI
  was never checked before it was put to the owner.

  **Problem:**
  - Sending keys to a Working pane can drop or garble the builder's turn. Nothing was lost here
    (PENDING `[]`, branch head unchanged).
  - A count and a CNI claim reached the owner and a PR before a command confirmed them: the
    cite-from-command rule (R43 I183).

  **Fix:** send keys only after a pane check shows the pane is not Working (already in the loop
  prompt). Count from the file, not from a scan by eye. Check a cluster's CNI before saying what
  it will show. The pre-tag smoke script sets the sample's model fields.

  **Acceptance:** no corrected-after-posting count or unchecked cluster claim in R48's window.

- [ ] **I216: `cmd/main.go:78` still says the image digests are listed in `RELEASE-NOTES-next.md`**

  **Where:** `cmd/main.go:78`. After #393 the digests are in `docs/RELEASE-NOTES-v0.1.3.md`, which
  has 3 `sha256:` hits; the fresh `-next` has 0 (review `5773283427`).

  **Fix:** repoint the comment to the per-release notes. b4 was briefed with
  `scratchpad/handoff-b4-main78.md` at ~18:50Z.

  **Acceptance:** `git grep -n 'RELEASE-NOTES-next' -- internal cmd` names no file as where the
  digests live.

## Owner decisions

**D104 (decided by the owner, 2026-11-04 ~19:00Z: (a)): how should a target the no-policy control pod cannot reach be counted?** pi12's
two rows are the case: policy `REFUSED`, control `BLOCKED(OSError)` (I213). Both probe pods run
on one node (`pi3` in this run) and target every node's IP. So these rows are the path from a pod
on pi3 to pi12's ports, and that path is closed **even with no policy**. Today the combine table
maps this to UNRESOLVED, and one such row makes the cluster verdict UNPROVEN.

- **(a)** Keep it UNRESOLVED, as now. This is strict, and it never reads an unexplained baseline
  failure as safe. The cost: one cordoned or firewalled node blocks the cluster verdict until
  someone fixes or explains it.
- **(b)** Add a row type `BASELINE_UNREACHABLE`: control `BLOCKED`, policy not `REACHABLE`. It is
  shown and not counted, like NOT_LISTENING. It is the same change in `verify-cni.sh`,
  `d38-cni-e2e.sh` and the operator's `combineVerdicts` (D103's table, `docs/OPERATOR.md`). The
  verdict still needs `APISERVER_SVC` and one host-network target BLOCKED. Nothing reached the
  target in either pass, so no leak is hidden. A *transient* baseline failure would also be
  excluded rather than retried.
- **(c)** Skip `SchedulingDisabled` / `NoSchedule` nodes as targets, printed `SKIPPED
  (unschedulable)` and never PASS (R25 I84). That is wrong for reach: a Loop on another node can
  still target a cordoned node's ports, and this would stop measuring that.

**Decided: (a), with the homelab side explaining pi12 (I213); the owner's answer was "D104 a".** The cause of
`BLOCKED(OSError)` is unknown. Adopting (b) before knowing it would build a rule on an unshown
fault (R28 I92). If pi12 proves to be a deliberate host firewall, (b) is the right general rule,
and it becomes a small D-slice across the three combine tables.

**Decided in this window:**

- **The #392 merge evidence** (owner, log 16:43Z): merge on the evidence in hand (unit tests and
  mutations, selftest and mutations, kind-ref verify-cni, the lab Calico e2e); the homelab k3s
  re-run goes after the merge; no kind-coxswain deploy needed.
- **Removing `../coxswain-d103`** (owner "Yes, rm it", log 18:00Z), after the reviewer checked it
  held no `.samples/` evidence.
- **v0.1.3** went in the order R46 recorded: D103 merged → notes renamed (#393) → the pre-tag
  smoke passed → the reviewer pushed the tag → the release run and artifacts were confirmed.
- **R47 before the I208 / I211 / older queue** (owner, 11-04 ~19:05Z: "R47 first"); then
  **I212 next** (owner, with the D104 answer: "D104 a, then I212 next").

## What I did not verify

- **Why pi12 is unreachable from pods** (I213): not investigated, and the homelab is the owner's.
- **The operator's own D103 probe on k3s:** no deployed operator ran there.
- **The C2 "79%" figure:** it comes from the reviewer's loop-prompt notes, not from a timestamped
  log line.
- **Whether the sample's commented-out model fields were the intended design.** The sample's
  "Expected outcome (with a working model backend)" comment suggests the user is meant to fill
  them in, but nothing checks that.
