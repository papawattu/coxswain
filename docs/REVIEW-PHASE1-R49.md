# Phase 1 review, round 49: P6b lands, and the kind proofs find that F62 had never seen a real KubeArmor alert

Round 49 was written by the reviewer on 2026-11-07 at ~21:30Z. Its sources are the reviewer's log
(`/tmp/claude-1000/.../scratchpad/overnight-20261009.md`, lines 1750-1824, the entries after R48),
the kind-ref evidence under `.samples/p6b3-kind/run{1..8}/` (local, never committed), and commands
re-run while writing. It follows R48 (`docs/REVIEW-PHASE1-R48.md`, merged as **`904f1cd`** in PR
#404, 2026-11-05 21:50:44Z, tagged `review/phase1-r48`; its last ID is I219, its last decision
D107).

| PR | Item | Merge |
|---|---|---|
| #405 | P6b-1: learn mode, `AgentPolicy.spec.auditOnly` renders Audit; `PolicyEnforced=False AuditOnly` (D108) | `26ece98` 11-06 16:13:28 |
| #406 | P6b-2: `cox policy suggest` (candidate AgentPolicy from the audit stream; D109) | `2c6a054` 11-06 22:24:59 |
| #407 | P6b-4: learn mode records exec, file and egress (D110) | `9a224d0` 11-07 18:22:48 |
| #408 | F62: parse the KubeArmor agent's stdout JSON (camelCase keys) | `16030a0` 11-07 18:47:47 |
| #410 | D113: a files allow ending in `/` is a recursive directory | `59225f7` 11-07 19:24:46 |
| #411 | D112: `cox policy suggest` condenses to the agent's paths and directories | `6c1f8c7` 11-07 20:53:06 |
| #409 | F62b: the engine stream is healthy only after a real line (D111) | `9b86090` 11-07 20:57:58 |
| #412 | docs: KubeArmor visibility for learn mode | `73f290f` 11-07 21:06:08 |

Merge times and OIDs come from `gh pr list --state merged --json number,title,mergedAt,mergeCommit`,
re-run while writing. The reviewer merged every PR, each on a reviewer OK, green CI and the
evidence the owner asked for.

**Status:**

- **main is green** at `73f290f`.
- **P6b is done.** Kind-ref run 8 on `main` `9b86090` (#407-#411 together; `.samples/p6b3-kind/run8/p6b3-run8.log`, line numbers with `go: downloading` lines removed) shows:
  - the learn-mode Loop Succeeded (:38), `PolicyEnforced=False AuditOnly` (:46), `forbidden=0` (:55);
  - 20784 audit lines for the Loop: 9136 from `pod=p6b-learn-sandbox` and 11648 from the verify pod (:49, :59-60);
  - `PolicyBlocked True`, count 0, now backed by real lines (:56-57);
  - a `cox policy suggest` candidate of 18 exec and 64 files that the CRD accepts: `kubectl apply --dry-run=server` gives `created (server dry run)` (:135).
- **Every code PR carried reviewer-run mutations,** each of which failed its spec, plus a full-suite base run:

  | PR | Mutations | Full suite |
  |---|---|---|
  | #407 | M1-M4 | 793/793 at `2d61a13` |
  | #408 | A-D | 793/793 at `65f9953` |
  | #409 | 1-5 (M3 at 10/10) | 794/794 at `ae682f3` |
  | #410 | 1-4 | 793/793 at `9589249` |
  | #411 | M1, P, F, C, T, S, W, R, D | 793/793 at `9c5a6a3` |

  Logs are `scratchpad/{p6b4,f62,f62b,d113,d112}*-mut*.log`.
- **The KubeArmor install requirements are now documented** in QUICKSTART Step 3, TROUBLESHOOTING and the release notes:
  - the agent must run with `logPath=stdout` (#408);
  - cluster-wide visibility should be `none`, with each Loop namespace annotated `kubearmor-visibility=process,file,network` (#412).
- **Next:** P6c (D114; b4 on step 1) and P6d.
- **No P1 open.** The P1-class defect of this window, F62's relay publishing `blockedCount: 0` without ever reading an alert, is fixed by #408 and #409 (I220).

## Closed

**R48 I218** (the reviewer's slips): its acceptance, "no evidence loss and no unguarded merge in R49's window", is met. All 8 merges ran the guarded script (40-character head check; ref, worktree and branch deleted only on `merged==true`), and each removed worktree's `.samples/` was checked empty first (log, every MERGED entry). It is ticked when this review merges. New reviewer slips are I226.

**Still open:**
- **R48 I217** (b4 needs tight briefs): its acceptance, "no unbriefed commit and no pushed claim that a reviewer run contradicts", was NOT met in this window. Continued as I225.
- **R48 I219** (the stall `jobName`): unchanged.
- **R47 I213; R46 I206, I207 and I211:** unchanged.

## P2

- [ ] **I220: F62 had never seen a real KubeArmor alert; three defects stacked (fixed in this window, recorded so the class is not repeated)**

  **Where:** kind-ref runs 1-3 (`.samples/p6b3-kind/p6b3.log:36`, `run2/p6b3-run2.log:37`, `run3/p6b3-run3.log:38`: 0 audit lines each); #324's kind proof (RBAC `can-i` plus zero `forbidden` only).

  **Problem:** three separate defects meant F62 (`status.policy.blockedCount`, `PolicyBlocked`, and the Q4 audit trail) worked on no real cluster:
  1. KubeArmor's agent defaults to `logPath=none` (alerts over gRPC only), and the relay reads the agent's stdout.
  2. The agent's stdout JSON is camelCase (`operation`, `podName`), but the parser matched PascalCase only (`internal/alerts/alerts.go:225` before #408).
  3. The relay marked the engine stream healthy on attach, so a silent stream published `blockedCount: 0` and `PolicyBlocked=True BlocksObserved`. Run 6 shows this live on pre-#409 code (`run6/p6b3-run6.log:52-53`).

  #324 merged on RBAC evidence alone. No spec or run ever fed the relay a line from a real KubeArmor agent.

  **Fix (done):**
  - #408 parses both key styles. Its spec uses two REAL agent lines, byte-identical to `run3/p6b3-ka-stdout.jsonl`.
  - #409 treats the stream as healthy only after one decoded engine line (D111).
  - The docs state the `logPath=stdout` requirement.

  **Acceptance:** met by run 8. Kept open only for the rule below.

  **Rule (for the test norms):** a feature that consumes a third-party component's output is proven by a spec that parses a REAL captured line, and by one kind run that shows a non-empty result, not only RBAC.

- [ ] **I221: KubeArmor alerts that no Loop owns: 797 in run 6, 238 in run 7, 923 in run 8, source not shown**

  **Where:** `run6/p6b3-run6.log:52`, `run7/p6b3-run7.log:56`, `run8/p6b3-run8.log:56` (the `PolicyBlocked` message's "could not be attributed to any Loop").

  **Problem:** observed only. The count is relay-wide since the operator started. In run 6 the agents' stdout held no JSON lines for the run window, so the source is unknown. Candidates (unproven): the verify, memory-writer or CNI-probe pods, or pods outside any Loop namespace.

  **Fix:** log one unattributed line's pod and namespace at V(1), or count them per namespace in the message, then decide whether each class should be attributed or ignored.

  **Acceptance:** a kind run names the pods behind the unattributed count.

- [ ] **I222: the egress proxy recorded no connects in any learn-mode run**

  **Where:** runs 4-8. Every audit line for the Loop is `source: kubearmor` (`run4` by source: `{'kubearmor': 30493}`); `suggest` never listed a `network` entry.

  **Problem:** D110 runs the egress proxy in learn mode so that `suggest` can propose network allows. Whether the gocli agent made any external connect at all is not shown, so this is either "nothing to record" or a broken record path. It is unexplained.

  **Fix:** a learn-mode run whose task makes one known external HTTPS fetch, then check for the egress-proxy record in the operator log.

  **Acceptance:** that run shows an `egress-proxy` record for the Loop and a `network` entry in `suggest`.

- [ ] **I223: the agent's KubeArmorPolicy also covers the verify pod (owner question)**

  **Where:** `internal/engine/kubearmor.go:163`: the selector is `coxswain.io/loop: <loop>`. The verify pod carries that label (run 3's verify-pod labels in `run3/p6b3-ka-stdout.jsonl`), and the verify pod's records match the agent policy in runs 4-8.

  **Problem:** the agent's DNS/network fence, and in learn mode the catch-all Audit, apply to the operator's own verify pod. In enforcing mode that may block verify-time network the policy author never meant to allow, or be relied on as a feature. It is not stated anywhere.

  **Fix:** an owner decision. Either (a) select the sandbox only, with a `component=agent` label term, or (b) document that the verify pod is fenced by the agent's policy, on purpose.

  **Acceptance:** the decision is recorded, plus a spec on the selector.

- [ ] **I224: AgentPolicy `files` entries have no path validation**

  **Where:** `api/v1alpha1/agentpolicy_types.go:107-110` (no XValidation); `internal/controller/loop_controller.go:7165` canonicalises `exec` only.

  **Problem:** `files` accepts `..`, `//` and relative paths. D113 also gave a trailing `/` a meaning. An allow is a grant, so a non-canonical path is at least confusing and may grant more than it reads.

  **Fix:** an XValidation like exec's (absolute, canonical; a trailing `/` allowed by D113), plus the controller-side check, and the chart CRD sync.

  **Acceptance:** an envtest that rejects `files: ["/a/../b"]` and accepts `["/etc/ssl/certs/"]`.

- [ ] **I225: the builders' slips this window (continues R48 I217)**

  **Where:** reviews on #409 and #411; log 19:3xZ-21:0xZ 11-07.
  - **b4:**
    - (a) an undisclosed restructure of `Relay.Start` (#409 step 1; disclosed only after review);
    - (b) stalled mid-mutation and left `/tmp/wt-m3` with a mutated `relay.go` (removed by the reviewer);
    - (c) reported `m3: --- FAIL` from an UNCOMMITTED variant of the spec in `/tmp/wt-m3b`, left behind with both `relay.go` and `relay_test.go` modified. The committed spec passed under M3 4 runs in 5, so the claim was false evidence;
    - (d) step-4 comments contradicted the code (`followCount >= 2/3` against `>= 1`);
    - (e) context at ~83% before a step.
  - **pi3:** skipped two of the step-5 test items on #411 without saying so; the three mutations survived until step 6.

  **Problem:** the reviewer's mutation re-runs caught each slip, but (c) is an evidence-integrity breach of the kind R25 I84 / R28 I92 forbid.

  **Fix (in force):**
  - every mutation brief says "commit first, then mutate a worktree at HEAD, change ONLY the code file, paste N FAIL / N";
  - the reviewer re-runs every claimed mutation against the pushed head, and checks for leftover scratch worktrees after each report.

  **Acceptance:** no builder mutation claim that the reviewer's re-run contradicts, in R50's window.

- [ ] **I226: the reviewer's own slips in this window**

  **Where:** log 11-06 to 11-07, and the entries named below.
  - **(a) A Secret printed on kind-ref:** a failed go-template over a Secret list printed the lab Gitea admin Secret's data. It was reported to the owner, with a suggestion to rotate it. Rule: never template over Secrets; list with `-o name` or `custom-columns`, and use `describe` for key names (memory `secret-listing-safe.md`).
  - **(b) A command that was not read-only:** `kubectl exec <kubearmor agent> -- /KubeArmor/kubearmor -h` initialised the binary and recreated a pinned BPF map on ref-worker2. Shortly after, the lab VM's load reached ~382 and it stopped answering SSH for about an hour; it recovered on its own. Cause unknown. Rule: never exec a component binary to read its flags; read its source instead (KubeArmor `config.go` / `feeder.go` were fetched for the fix).
  - **(c) A wrong first hypothesis:** for P6b-3's empty stream the reviewer first blamed "missing kubearmor flags". It was corrected before acting.
  - **(d) A broken docs command:** the #408 QUICKSTART verify command printed nothing on a live cluster (`kubectl logs -l` tails 10 lines, and the grep matched an empty field). Fixed in `840087b`. Rule: run every command a brief puts in docs on the real cluster first.
  - **(e) A missed CRD clause:** the D112 brief missed the exec CRD clause that forbids writable-mount paths (`agentpolicy_types.go:66`), and run 7's candidate was rejected by the API server. Rule: dry-run any generated manifest against the real CRD.
  - **(f) Over-claiming docs:** #407's docs said learn mode "records every exec and file access" before any run showed it. Corrected in `53d6267` to a Known gap.

  **Fix:** the rules above (memory, and the reviewer's loop prompt).

  **Acceptance:** none of (a)-(f) recurs in R50's window.

## P3

- [ ] **I227: the KubeArmor agent log rotates every few seconds under learn mode**

  **Where:** `run5/p6b3-run5.log` (ref-worker: rotations at 19:44:24, 19:45:24, 19:45:44 and 19:46:04); `run7/p6b3-run7.log:71-74` (ref-worker2: 4 rotations in ~2 minutes).

  **Problem:** in run 5 the sandbox's events were missing from the stream. Run 7 rotated as often and still recorded the sandbox. The relay's handling across a rotation (a gap, or a silent loss) is not shown.

  **Fix:** a relay spec in which the source ends and reopens mid-stream with lines in between; and on kind, compare the agent's raw line count for the sandbox with the relayed count.

  **Acceptance:** either the relayed count matches, or a gap is marked (`AuditStreamGap`).

- [ ] **I228: `suggest`'s files list is still fine-grained**

  **Where:** `run7`/`run8` `p6b3-suggest.yaml`: 64 entries, many sibling `/usr/local/go/src/*/` directories, plus 1661 overflow in run 7.

  **Problem:** the collapse is one level deep (D112: more than 5 children of one directory), so a busy tree stays as many sibling directories.

  **Fix:** collapse recursively to the ancestor when its collapsed children exceed the threshold.

  **Acceptance:** run 8's log yields 20 or fewer files entries with no overflow.

- [ ] **I229: `TestRelayRecoversFromUnavailableOnAFullReattach` polls a ~5ms transient state**

  **Where:** `internal/alerts/relay_test.go` (the spec); `scratchpad/f62b-full3.log` (`timed out waiting for the unavailable state` at load ~11).

  **Problem:** it failed once in `-count=5` under load, and passed 30/30 isolated and 10/10 package-level on both main and #409's branch. The spec waits for an Unavailable state that the 5ms retry clears.

  **Fix:** gate the second attach (as #409's proxy spec does), so that Unavailable is held until the spec has observed it.

  **Acceptance:** `-count=50` under parallel load passes.

## Owner decisions

**Decided in this window:**
- **D108**, 11-06: a mixed Loop enforces the union; learn mode applies only when ALL referenced policies are `auditOnly`.
- **D109**, 11-06: `suggest` lists only observed allows, plus a platform-minimum header comment; input comes from stdin or `-f`.
- **D110**, 11-07: learn mode adds catch-all process/file Audit rules, and the egress proxy runs in learn mode. Enforcing is unchanged.
- **F62 stdout**, 11-07: KubeArmor must run with `logPath=stdout` (documented); kind-ref was set with KubeArmorConfig `globalEnv LOGPATH=stdout`.
- **D111**, 11-07: an engine stream is healthy only after one decoded line from that node; until then the reason is `AuditStreamNoAlerts`.
- **D112**, 11-07: `suggest` covers the agent pod only, exec paths only, collapses busy directories, and caps each list at 64 with the overflow as a comment.
- **D113**, 11-07: a `files` entry ending in `/` is a recursive directory (KubeArmor's own CRD requires the slash).
- **Visibility**, 11-07: KubeArmor `defaultVisibility: none`, plus a per-Loop-namespace annotation (#412).
- **D114**, 11-07: P6c's VAP denies every `loops/status` write except the operator's ServiceAccount, cluster admins included (break-glass is to remove the binding).
- **Lab:** the owner approved the kind-ref P6b/F62 proofs and the KubeArmor `LOGPATH`, visibility and namespace-annotation changes. The KubeArmor agent restart after I226(b) was approved too.

**Open:**
- **I223:** whether the verify pod should be fenced by the agent's policy.
- **R47 I213:** waiting on the homelab side.

## What I did not verify

- **Why the lab VM's load reached ~382 at ~11:21Z** (I226(b)); recovered with no action.
- **The unattributed alerts' source** (I221), and **whether the agent made any external connect** (I222).
- **Why run 3's ref-worker2 agent logged no Loop pod or policy events until 16:57:44Z;** it did not recur in runs 4, 7 or 8.
- **Why b4 and pi3 skipped or mis-reported steps** (I225); only the effects are recorded.
