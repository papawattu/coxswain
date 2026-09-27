# Phase 1 review, round 7 — open issues

Review of `c0942a4`..`0cafed9` (2026-09-27), since tag `review/phase1-r6`,
plus the owner's `CONTEXT.md` update ("agents", "only do what is allowed by
policy — network isolation, command allowlists and auditing").

Each issue is self-contained so it can be picked up independently. Tick the
box and add the commit hash when done. Issue IDs continue project-wide.

Priority: **P1** = fix before the next slice builds on it. **P2** = decide
during Phase 1. **P3** = cleanup, batch whenever.

## Verdicts on reviewed commits

| Commit | Issue | Verdict | Follow-ups |
|--------|-------|---------|------------|
| `c0942a4` | D27 stale-evidence check | OK + note | I31 |
| `0cafed9` | ADR-0006 + C1–C5 resequence | OK + notes | **D28 (P1 for C3+)** |

D27 is fixed: the current commit comes from the operator-pinned
`currentVerify`, and empty or mismatched commits give `Unknown`. ADR-0006
matches round 6, and C1–C5 are ordered ahead of B3.

---

## P1 — Gate on the owner's policy decisions

### D28. The owner's `CONTEXT.md` introduces Policy; ADR-0006 predates it

- [ ] Owner decisions recorded (ADR-0007)

**What changed:** `CONTEXT.md` now says the goal is agent isolation, "so they
can only do what is allowed by policy. Network isolation, command white
lists and auditing," and drops "coding" from "agents". ADR-0006 covers
network isolation and credentials, but **not** command allowlists, agent
activity auditing, or what a Policy *is*. The owner has been asked these
questions directly; **the builder must not pick answers.** Open questions,
with the reviewer's recommendation for each:

1. **Scope of "agents".** Non-coding agents too? *Rec:* isolation layer
   agent-agnostic now; the git + checks Loop stays the only Loop kind in v1.
2. **What a Policy is and who owns it.** *Rec:* a cluster-scoped
   `AgentPolicy` CRD written by admins, referenced by Loops; a Loop may only
   narrow it. Define **Policy** in `CONTEXT.md`.
3. **Where command allowlists are enforced.** In-agent allowlists are
   advisory only (the agent is untrusted and has a shell). *Rec:* enforce at
   the kernel/runtime layer — an eBPF/LSM engine (e.g. KubeArmor or
   Tetragon) and/or gVisor + minimal images — and decide binary-level vs
   argument-level rules.
4. **What auditing covers and where it lives.** Agent-reported traces are
   claims. *Rec:* trusted sources only — model calls from the proxy, exec and
   network events from the enforcement engine, file changes from the git
   diff — shipped to a durable sink (not Loop status, not events alone). Split
   `CONTEXT.md`'s "Audit trail" into decision audit vs agent-activity audit.
5. **What a violation does.** *Rec:* every blocked action is recorded;
   repeated attempts or high-risk classes (API server, secrets) end the Loop
   with a new terminal reason `PolicyViolation`.
6. **Network allowlist = part of Policy** (absorbs ADR-0006 open decision 2 on
   dependency installs).

**What the builder can do now (independent of these answers):**
- **C1** (pod hardening + `spec.agent`) and **C2** (model proxy sidecar) —
  proceed. Keep `spec.agent` free of network/command fields so they can move
  into Policy.
- **C4** (trusted publish step) — proceed.

**What waits for ADR-0007:**
- **C3** (NetworkPolicy) — its allowlist source depends on Q2/Q6.
- A new **C6** (command-allowlist enforcement) and **C7** (trusted activity
  audit) — to be added to the plan once Q3/Q4 are answered.
- **C5** (evil-agent e2e) — extend it to cover a disallowed command and to
  assert the violation appears in the trusted audit, once C6/C7 exist.

**Acceptance:** ADR-0007 records the owner's answers; `CONTEXT.md` defines
Policy and splits the audit terms; C3/C6/C7 are in the plan with seams.

---

## P3 — Cleanup

### I31. Owner's CONTEXT.md edit landed inside the D27 commit

- [x] Done (process note: AGENTS.md Review protocol now requires explicit-path `git add`, never `-A`, I31)

`c0942a4` ("D27: stale-evidence check…") also contains the owner's
`CONTEXT.md` change, because it was staged in the shared tree when the
builder ran `git add -A`. The content is intact; the history just attributes
a product-direction change to a bug fix. Don't rewrite history — but from
now on, stage **explicit paths** (`git add <files>`), never `-A`, so an
owner's or reviewer's in-progress edits aren't swept into builder commits.
Add that rule to `AGENTS.md`'s Review protocol.

---

## Next round will check

- C1, C2, C4 as they land.
- ADR-0007 once the owner answers D28's questions.
