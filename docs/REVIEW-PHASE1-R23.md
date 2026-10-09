# Phase 1 review, round 23: D46, D41 and Phase 2

Round 23, 2026-10-07, since tag `review/phase1-r22`. It covers the sixteen PRs
merged from the owner's queue (2026-10-04): D46, the D41 tool proxies, and
the whole of Phase 2.

| PR | Item | Merge |
|---|---|---|
| #69 | D46 agents are not exec-fenced; fail fast on an exec list without the shell | 89f67ad |
| #70 | D41 plan: ADR-0008 tool proxies + TDD-PLAN-D41 | 761db78 |
| #71 | D41a `cmd/tool-proxy`, the generic HTTP tool proxy | d57d998 |
| #72 | D41b `AgentPolicy.spec.tools[]` + validation | cadc90e |
| #73 | D41c the operator provisions, gates and drift-corrects per-tool proxies | fa1d387 |
| #74 | D41d per-tool KubeArmorPolicy, agent DNS allowlist, `COX_TOOL` env, netpol rules | c443ab0 |
| #75 | D41e kind acceptance (`make d41-e2e`) | b47ed62 |
| #76 | Phase 2 plan: TDD-PLAN-PHASE2 | cb9bf40 |
| #77 | P2a ADR-0009: meter in the model proxy | fe988f0 |
| #78 | P2b `cmd/model-proxy` metering binary + proxy image wiring | e474ae0 |
| #79 | P2c API for budgets, stall history and `pausedReason` | 445c60f |
| #80 | P2f Paused phase, `pausedFrom`/`pausedReason`, resume semantics | dfdeede |
| #81 | P2d budget caps + `onExceeded` (wall clock, tokens, cost) | 8f884be |
| #82 | P2e stall detection (normaliser + stall gate) | f1f845b |
| #83 | P2g conditions + events for every P2 transition | 73f4d57 |
| #84 | P2h kind acceptance (the Phase 2 Done-when) | 456a209 |

**Status:**
- **Phase 2's Done-when holds on kind.** `make p2-e2e` run
  `run-20261006221207` (operator digest `770d9f4c`, checked against the
  running pod) passes all five assertions:
  - an impossible goal stops as `Failed:Stalled` at iteration 3, while a
    control Loop runs to its cap of 5;
  - a token cap stops a Loop as `Failed:BudgetExceeded` (400 >= 300);
  - a real-vLLM Loop's metered tokens sit within the summed pi6 + pi8
    Prometheus delta;
  - a suspended Loop resumes where it left off: paused from `Implementing`,
    with the same iteration and verify pin;
  - a budget-paused Loop resumes only through `coxswain.io/resume`, never
    through a `suspend` flip.
- **Gate mutations on kind:**
  - stall gate disabled → assertion 1 fails;
  - budget gate disabled → assertion 2 fails;
  - `pausedReason` check dropped → assertion 5 fails.
- **D41 on kind:** an allowed tool path returns 2xx, a disallowed one 403 with
  an audit record, and the agent never sees the credential (#75).
- **Mutations:** every gate merged this round was mutation-checked by the
  reviewer, and every mutation failed the suite.
- **Two real bugs found by P2h's kind run,** both fixed in #84 with tests and
  mutations:
  - **A silent failure never stalled:** a check that fails with no output (for
    example `test -f …`) has an empty termination message, which the stall
    gate treated as no evidence.
  - **Every Loop wedged after its first iterate:** the runner's claim carried
    no `iteration`, so the operator's stale-claim guard (R22 I48) discarded
    every claim from iteration 2 on. That hit any Loop that iterated twice,
    not just the test.

Still open from earlier rounds: R21 I54 (owner-blocked) and R22 I58–I63; see
"Carried" below.

The items below are what this round's reviews left open. I64 and I65 should be
settled before Loops run against real tools or for long unattended stretches.

---

## P1

### I64. Tool-proxy egress is not fenced by DNS on clusters without BPF-LSM

- [x] Open (**owner decision**) — fixed in PR #90 (squash `0df223f`): ADR-0008 and README softened to "expected, not yet verified" on BPF-LSM clusters

**Owner decision (2026-10-07), D50 = (a):** accept the gap and document it as a
limitation of clusters without BPF-LSM. No fail-closed hold and no IP pinning
for now. Remaining builder work: document the limitation where operators will
see it (the D41 tool-proxy docs/ADR-0008 and the install/README notes). Say
plainly that on such clusters only the tool proxy's own code keeps its egress
on the configured upstream. Then tick this box with that commit.

**Where:** the D41 tool proxy (`<loop>-tool-<name>`), its NetworkPolicy and
its KubeArmorPolicy (`EmitKubeArmorPolicyWithToolFQDNs`).

**Problem:**
- **The fence is unenforced on kind:** on kind-coxswain-dev, KubeArmor's
  `matchDNSQueries` fence is not enforced (the BPF-LSM `SOCKET_SENDMSG` hook
  isn't active on this kernel/KubeArmor). In #75 the tool-proxy probe, running
  as `/usr/local/bin/tool-proxy`, resolved and connected to `example.com:80`.
- **The tool proxy is exposed:** its NetworkPolicy allows any external IP by
  design, because the upstream's IP isn't known in advance. Where the DNS fence
  is unenforced, only the proxy's own code (it dials the configured upstream
  only) stops it reaching any host.
- **The agent is less exposed:** the agent's DNS allowlist is unenforced too,
  but the agent's netpol is pod-selector-scoped.

**Fix:** choose one:
- **(a)** accept and document it as a limitation of clusters without BPF-LSM;
- **(b)** fail closed: require DNS enforcement through the D30 gate, and hold a
  Loop with tools Suspended when the enforcer can't enforce DNS;
- **(c)** pin the tool proxy's egress: resolve the upstream at reconcile time
  and write an IP-pinned egress rule, re-resolving on a schedule.

**Acceptance:**
- The decision recorded (as D50).
- For (b) or (c): a kind run where the tool proxy can no longer reach a host
  other than its upstream, and a mutation that removes the new fence and makes
  that run fail.

### I65. A verify Job that fails before the checks wedges the Loop in Verifying

- [x] Fixed in d92ef45 (PR #86)

**Where:** `internal/controller/loop_verify_job.go` `verifyOutcome` /
`applyVerifyOutcome`.

**Problem:** `verifyOutcome` reads only the tamper, artifact and `check-*` init
containers. If an earlier init fails (`clone-base` or `import-agent`), those
stay `PodInitializing` for good. The Job goes `Failed` (backoffLimit), nothing
decides, and the Loop sits in `Verifying` forever.
- Seen in P2h's G2 run (`run-20261006174025`, Loop `p2h-real`): `clone-base`
  exited 128, `Could not resolve host: gitea.samples.svc (Timeout while
  contacting DNS servers)`, a transient kind DNS flake. The same Loop
  succeeded in about 4 minutes in each of the three runs before.

**Fix:** treat a `Failed` verify Job, or a non-zero terminated earlier init, as
a decision. Either:
- iterate, naming the failed container; or
- set a distinct reason (`VerifyInfraFailed`) and recreate the Job with a
  bounded retry, then fail the Loop with that reason.

Keep "still running" as no decision (the I49 norm).

**Acceptance:**
- An envtest spec for each case: `clone-base` failed, `import-agent` failed,
  the Job `Failed` with no check run, and each of those still running
  (no decision).
- A mutation that drops the new branch makes the failed-Job spec fail.

## P2

### I66. D49 is not actually in effect

- [ ] Open (**owner action**)

**Where:** `~/.pi/agent/settings.json` (owner's global pi config); the reviewer's
`pi-args.txt`.

**Problem:**
- **Still loaded:** R22 D49 removed `pi-observational-memory`. But it and
  `pi-honcho-memory` are still listed as global `packages` in
  `~/.pi/agent/settings.json`, and the launch args (`-ne -e …`) don't unload
  global packages. So the observer and reflector still run in pi.
- **Not mine to change:** that file is the owner's global configuration, so
  the reviewer hasn't edited it.

**Fix:** the owner removes the two packages from `settings.json` (or moves them
to a project scope that excludes coxswain), then restarts pi.

**Acceptance:** pi's startup lists neither extension.

### I67. TDD-PLAN-PHASE2 corrections found on kind

- [x] Open (docs) — fixed in `73c2c5d` (PR #91, merged)

**Where:** `docs/TDD-PLAN-PHASE2.md` (P2e "The normaliser" input; P2h fixture).

**Problem:** two statements in the merged plan are wrong, and both were found
the hard way on kind.
1. **The check output is in `state`, not `lastState`:** line ~1245 says the
   operator reads `status.initContainerStatuses[].lastState.terminated…`. A
   check init runs once, so its message is in `state.terminated`; `lastState`
   is filled only after a restart. #82 fixed the code (`state` first, then
   `lastState`), but the plan still says `lastState`.
2. **`test -f` prints nothing:** P2h says the check `test -f /nonexistent`
   "fails with the same message". It prints nothing, so the message is empty.
   The operator now counts an empty message as evidence (#84), but the plan
   should say the stall is detected on an *empty* output.

**Fix:** correct both passages and cite #82 / #84.

**Acceptance:** the plan text matches the code; the commit is `I67: …`.

### I68. Builder process: commits that a reviewer had to unwind

- [ ] Open (AGENTS.md + a guard)

**Where:** AGENTS.md; the builder's commit habits.

**Problem:** this round the builder:
- **Amended main's squash commit:** P2g's first commit was
  `git commit --amend` on top of main's P2e squash (`5478c2a`). It was pushed,
  then repaired with a merge of `origin/main`.
- **Committed `.samples` evidence twice:** `edfe93d` (P2g) and `7156bb0`
  (P2h). Each was untracked in a follow-up.
- **Cited a plan line that doesn't exist:** to justify an out-of-scope operator
  change ("item 14: the reference runner is invoked with --max-steps 2"). The
  change was reverted.
- **Committed `.gnosis/` once** (#75), removed in review.

**Fix:**
- AGENTS.md: add `.samples/` and `.gnosis/` to the never-commit list (and
  `git add -f` under them), plus "quote plan text only after grepping it".
- A cheap guard: a `make lint` step that fails when any path under `.samples/`
  or `.gnosis/` is tracked.

**Acceptance:**
- The AGENTS.md text.
- The guard, plus a mutation (force-add a `.samples` file) that turns
  `make lint` red.

### I69. Re-run P2h's G1/G2 gate mutations on the final script

- [x] Open (evidence) — fixed in PR #94 (closed): G1 `1:fail` (digest `25979d90…`), G2 `2:fail` + `5:fail` cascade (digest `33a43223…`), both from `456a209`'s script. Assertion 4 passes under G2 (suspend pause/resume is a separate code path from the budget gate).

**Where:** `.samples/p2h/mutations.md`; `test/e2e/p2-e2e.sh`.

**Problem:** G1 (stall gate disabled) ran on script `7f249f3` and G2 (budget
gate disabled) on `54ae17d`. Both predate the counter-based assertion
accounting (`f76cf8b`) and assertion 5's sub-case (c). Their target assertions
haven't changed since, so the failures stand. But the evidence table mixes
script versions, and G2's extra failures (4 and 5) were never explained in the
record.

**Fix:** re-run G1 and G2 on the merged script, and explain any assertion
beyond the target that fails.

**Acceptance:** G1 `1:fail` and G2 `2:fail` from `456a209`'s script, with
digests, in the record.

## P3

### I70. Upgrade note: tool netpols created before D41d aren't cleaned up

- [x] Open (docs) — fixed in PR #92 (commit `2a87b22`, pending merge): UPGRADE.md rewritten with the correct mechanism (label-in-place, not recreate) + a safe per-netpol classifier command, shown working on kind. The I70 P1 review (jsonpath escaping + label misalignment) is fixed in `2a87b22` (read each netpol's label by name with an escaped jsonpath; kind-verified).

**Where:** `internal/controller/loop_controller.go` (`toolProxyForLabel`, the
label-based stale cleanup).

**Problem:** tool-proxy NetworkPolicies created before #74 carry no
`coxswain.io/tool-proxy-for` label. The stale cleanup lists by that label, so
after an upgrade it won't find them. That doesn't matter on a fresh install.

**Fix:** a line in the upgrade notes (delete pre-D41d tool netpols once), or a
one-time adoption by name.

**Acceptance:** the note, or a spec that adopts an unlabelled
`<loop>-tool-<name>` netpol.

### I71. Housekeeping

- [x] Open (filler) — fixed in PR #93 (squash `152a9f8`): stale `.gnosis` inbox note deleted, `docker image prune` + `P2H_LOOP_COUNT` knob added, if/fi nesting fixed, NOT RUN for reduced runs

- **A stale `.gnosis` inbox note:**
  `.gnosis/inbox/d38-external-positive-control-broken.md` asks how to fix
  D38's EXTERNAL positive control. That's already resolved in
  `internal/cni/pod_prober.go`: EXTERNAL is an expected-BLOCKED target, and
  `DNS_POSITIVE` is the positive control (the note's option (c)). The owner can
  delete the note.
- **Host memory during kind runs:** P2h's seven Loops plus image builds took
  the devbox to about 250 MB free (16 GB total), and Claude Code reaped a
  background watcher. Consider fewer concurrent Loops in `p2-e2e`, or a
  `docker image prune` step between mutation builds.

---

## Carried from R22 (unchanged)

- **I58:** revoke the exposed GitHub token (owner). D48's credential rule is
  still **not** in AGENTS.md; add it with I68's text.
- **I59–I63:** open as written in R22.
- **R21 I54:** owner-blocked (a GitHub sandbox repo + scoped token).

## Next

The owner's 2026-10-04 queue (D46 → D41 → Phase 2) is complete. Proposed
order:
1. **I64** (the owner's decision) and **I65** (the verify-Job wedge).
2. **I68**'s guard and AGENTS.md text, together with **I58**'s text.
3. **I66, I67, I69** as filler; I70 and I71 when convenient.
4. **The next phase** (PLAN.md), plan first, at the owner's direction.

## Owner actions (not builder work)

- **I64:** choose (a), (b) or (c).
- **I66:** remove the two memory packages from `~/.pi/agent/settings.json`.
- **I58:** revoke and regenerate the exposed GitHub token.
- **I54:** a GitHub sandbox repo and a scoped token.
- **Branch protection on `main`:** make `test` and `lint` required.
