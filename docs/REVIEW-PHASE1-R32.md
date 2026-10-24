# Phase 1 review, round 32: the evil-agent e2e as shipped, restore follow-ups, and the docs pass

Round 32, 2026-10-24. It follows R31 (PR #217, not yet merged) and covers the
work merged since R31 was opened. Code line numbers are against main at
`8533a89`. The review was built from these input files, which are not
committed:
- a post-merge integration review of F55, F35 and F36 (`.samples/review-postmerge-2.md`);
- a post-merge review of F54 as shipped (`.samples/review-postmerge-3.md`);
- the pre-reviews of PRs #222 and #224-#233 (`.samples/prereview-*.md`;
  #224 and #225 are `prereview-troubleshoot.md` and `prereview-opsec.md`);
- a comparison of `CONTEXT.md` with `docs/GLOSSARY.md`, checked against the
  code (`.samples/context-vs-glossary.md`, `.samples/context-md-proposal.md`);
- design notes for queued items (`.samples/design-f56-f70.md`, `.samples/design-f72-f73.md`).

| PR | Item | Merge |
|---|---|---|
| #220 | F55: gate the sandbox on the egress proxy for the init clone; stale-cache netpol read | 74c56f3 |
| #222 | F54: evil-agent e2e (ADR-0006:95-99) | eb6ce98 |
| #229 | F31: decide a no-pod memory-writer Job from its own conditions | aedc33d |
| #233 | F32: D71 memory lesson cap with oldest eviction; reader keeps whole files and warns | aeb3d55 |
| #224, #225, #227, #228, #230, #231, #232 | Docs: gate conditions, sandbox reach, readability, glossary, FAQ, egress log, model timeouts | f3929d8, 2f95627, 17a5587, a32bfc5, a0f7fd6, 5454541, 8533a89 |

**Status:**
- **The evil-agent e2e exists and catches the F42 shape.** On kind-ref, the
  pre-F42 build fails it and main passes a full run, and each of five
  security mutations (M1-M5, PR #222's body; M4 is backed by its re-run
  after the first build failed, see I114) fails it. As shipped it can still exit 0 with
  probes that were never measured, and it can't be run from the repo alone
  (I111).
- **F55's gate holds a forked Loop for a proxy the fork never uses** (I112).
  It's queued as F70, to be fixed with F56.
- **Four restore and fork edge cases are unhandled** (I113). One of them
  needs an owner decision (D84).
- **Seven docs PRs landed.** Every one needed fixes after pre-review: more
  than a dozen wrong or unbacked claims were caught before merge (I114).
- **`CONTEXT.md` describes the pre-build design for most core terms** (D83).

---

## P2

- [ ] **I111: the evil-agent e2e can report OK when some probes were never measured**

  **Where:** `test/e2e/evil-agent-e2e.sh` (`expect`, `record`, the final
  verdict lines, the preflight); `Makefile` (the `evil-agent-e2e` target's
  prerequisites).

  **Problem:** three gaps in the harness as merged in #222:
  - (a) `expect` accepts any verdict word, and only `ESCAPED` sets `FAIL`. A
    run whose probes report `N/A`, `UNPROVEN`, `ABSENT` or `RESIDUAL` still
    prints `evil-agent-e2e: OK` and exits 0. Nothing counts how many probes
    were actually measured.
  - (b) The preflight notices missing KubeArmor but keeps going.
    It never checks that the operator, the samples namespace or the host
    tools are present.
  - (c) No target or doc produces the environment the harness defaults to: a
    `kind-ref` context plus the `samples` Gitea seed it reads. QUICKSTART §9
    builds most of it, under a different context name and without the seed.

  The unproven steps (the verify-checkout assertions, probe 15's control
  Loop, the exec-path equivalence run) are labelled honestly. None prints as
  PASS.

  **Fix:** F72 (queued; (a)-(c) above are F72 (c)-(e)). Assert the proved
  set per variant, and print `proved=`/`not-proved=` counts that the exit
  code depends on. Make the preflight stop the run when something is missing. Add a recipe or docs page
  that builds the environment. Design note: `.samples/design-f72-f73.md`.

  **Acceptance:** a run where one required probe reports `N/A` exits
  non-zero; a run without KubeArmor stops in preflight; the execution tests
  cover both; a full kind-ref run is teed into `.samples/`.

- [ ] **I112: the F55 gate holds a forked Loop for a proxy it never uses**

  **Where:** `internal/controller/loop_controller.go:2040` (the gate term
  `needsEgressProxy(ctx, r, loop) || r.workspaceInitProxyCreated(loop)`).

  **Problem:** `workspaceInitProxyCreated` is true for any Loop with an
  external repo and no base commit yet. A fork's workspace is restored from
  the source's snapshot and already carries the init marker, so its clone
  never runs. The gate still holds the fork Suspended until the egress
  proxy is Ready. On a `make deploy` install the proxy image can't be pulled
  (F56), so the fork waits forever, and no condition says the egress proxy is what holds it.

  **Fix:** F70, done with F56 (both queued). Exclude a Loop whose restore
  has completed (`restoreCompletedForSpec`) from the gate's clone term.
  Design note: `.samples/design-f56-f70.md`.

  **Acceptance:** an envtest spec forks a source Loop with an external repo
  and no egress proxy pod, and asserts the fork isn't held, while a fresh
  Loop with the same repo still is. Named mutation: drop the new term, and
  the fork spec fails. One kind-ref run with the proxy image unset shows the
  fork Running, with init-workspace reporting the workspace already
  initialised.

---

## P3

- [ ] **I113: restore and fork edge cases left after F35 and F36**

  **Where:** `internal/controller/loop_restore.go:1021-1036` (the empty-PVC
  fallback), `:645-666` (`restoreCompletedForSpec`, the legacy-marker path),
  `:814-817` and `:860-864` (the fork-retention label, set and remove);
  `docs/TROUBLESHOOTING.md` (the `ForkRetentionLabelFailed` entry).

  **Problem:**
  - (a) **F74.** When the PVC is lost and no checkpoint exists, the operator
    recreates the PVC empty but leaves `status.baseCommit` set, so
    init-workspace never re-clones. The agent runs in an empty workspace.
    The condition and Event say so, but nothing recovers.
  - (b) **F75.** A pre-F35 `Restored` marker has no target key and counts as
    complete for any target, so after an upgrade a later rollback request is
    ignored.
  - (c) **F76.** The doc says `ForkRetentionLabelFailed` is retried on every
    reconcile. That's true for removing the label, but a failed set is not
    retried.
  - (d) **F77.** A rollback doesn't define the state afterwards: phase,
    iteration and the verify pin all survive it. A rollback on a terminal
    Loop restores the tree and nothing runs. A Loop already at
    `maxIterations` fails at once.

  **Fix:** queue F74-F77. Do (b), (c) and the safe half of (d) now: clear
  the verify pin and emit an Event. The rest of (a) and (d) waits on D84.

  **Acceptance:** an envtest spec for each item, with a named mutation per
  spec. The doc entry for (c) matches the code.

- [ ] **I114: the docs pass, what pre-review caught, and process slips**

  **Where:** PRs #224, #225, #227, #228, #230, #231 and #232 (docs), and
  #222 (F54).

  **Problem:** every docs PR needed fixes after pre-review. The wrong or unbacked
  claims caught before merge include:
  - a non-existent `spec.model` field, which the API server would silently
    prune;
  - a GitHub token scope that can't open a PR;
  - proxies described as sidecars (they are separate pods);
  - `Succeeded` called a condition (it's a phase);
  - a `CheckpointPolicy` field that doesn't exist (the field is
    `spec.checkpoints`);
  - a PromQL "stall" query that measured scrape time;
  - `kubectl cox logs <loop> proxy`, which `ExactArgs(1)` rejects. The same
    error was already on main in eight places in `docs/OPERATOR.md`, and
    all are fixed in #232;
  - a claim that an e2e runs a command it doesn't.

  Process slips this round:
  - builder #2 marked #222 ready while the body listed mutation M4 as run;
    its build had failed, so it never ran;
  - builder #2 once ran a scratch mutation in the shared checkout. It
    reverted it, and the tree was clean;
  - the shared checkout was found detached at `2f95627`, and nobody knows
    who did it. Nothing was lost, and it was restored to `main`.

  **Fix:** keep the rule that every docs PR from builder #1 gets a
  pre-review against the code before the reviewer's verdict. The reviewer
  checks every new technical claim in a "readability" PR as if it were code.
  Builders run mutations only in scratch worktrees, and a mutation listed in
  a PR body must have its output in `.samples/`.

  **Acceptance:** no docs PR merges without a pre-review file, and no PR
  body lists a mutation without its output.

---

## Owner decisions

- **D83: bring `CONTEXT.md` in line with the code, or keep it as the design
  record.** For most of the core terms, `CONTEXT.md` describes the pre-build
  design, and the code matches `docs/GLOSSARY.md`. Examples: the runner and
  the agent, who opens the PR, where metering happens, and how memory is
  keyed. Proposed wording for each, with the code lines, and the items that
  need a choice from you are in `.samples/context-md-proposal.md`.
  **Recommendation:** (a) update `CONTEXT.md` to the code for the settled
  items, and decide the rest one at a time. `CONTEXT.md` is your document,
  and nobody else edits it.

- **D84: what a rollback, and the empty-PVC fallback, should leave behind**
  (F74, F77). Options: (a) reset the phase and iteration so the Loop runs
  again from the restored tree; (b) keep them and only document it;
  (c) fail terminally on an empty-PVC fallback instead of running in an
  empty workspace. **Recommendation:** (a) for a rollback, (c) for the
  empty-PVC fallback.
