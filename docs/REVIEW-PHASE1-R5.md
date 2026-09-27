# Phase 1 review, round 5 — open issues

Review of `f6ab491` (2026-09-27), since tag `review/phase1-r4`, plus a
design item prompted by the owner: **the runners are LLM agents** — the
agent side is the product, and it's currently the least-built part.

Each issue is self-contained so it can be picked up independently. Work
test-first as in `docs/TDD-PLAN-PHASE1.md`. Tick the box and add the commit
hash when done. Issue IDs continue project-wide.

Priority: **P1** = fix before the next slice builds on it. **P2** = decide
during Phase 1. **P3** = cleanup, batch whenever.

## Verdicts on reviewed commits

| Commit | Issue | Verdict | Follow-ups |
|--------|-------|---------|------------|
| `f6ab491` | D24 tri-state tamper evidence, I28 | OK + notes | **D27 (P1)** |

The shape is right: `*int32` with nil = no evidence, a tri-state
`TamperVerdict` where `Unknown` never advances, `jobName`/`verifiedCommit`
recorded with the evidence, and I28's ignored claim parameter is gone.

Still open from round 4: **D25 (P1)** — `:(glob)` pathspecs and a real-repo
tamper test.

---

## P1 — Blocking (before B3)

### D27. The stale-evidence check can't fire in production

- [ ] Done

**Where:** `internal/controller/loop_controller.go:112`
(`tamperVerdict(v.TamperExitCode, v.VerifiedCommit, v.VerifiedCommit)`) and
`tamperVerdict` (`evidenceCommit != "" && verifiedCommit != "" && …`).

**Problem:**
1. The only production caller passes the **evidence's own**
   `VerifiedCommit` as both arguments, so "evidence names a different commit"
   is always false. The D24.3 staleness guard exists only in the unit test.
2. The guard is skipped when either commit is empty, so evidence that names
   **no** commit is accepted as current — fail-open on a missing binding.

Harmless today (only `Tampered` is acted on), but B3 will use `Clean` to
reach `Succeeded`, at which point leftover evidence from iteration N could
pass iteration N+1.

**Fix:**
- The *current* commit comes from where the operator pinned it on entering
  `Verifying` (D11: `status.history[n].verifiedCommit`, or a
  `status.currentVerify.verifiedCommit` written before the Job is created),
  never from the evidence record.
- `tamperVerdict` returns `Unknown` if either commit is empty or they differ.
- Red: envtest where status carries `tamperExitCode: 0` for commit A while
  the pinned current commit is B → stays `Verifying`. Unit: empty
  `evidenceCommit` → `Unknown`.

---

## P1 — Design (owner-prompted): the runner is the agent

### D26. Make the agent a first-class, pluggable part of the Loop

- [ ] ~~Decided (ADR-0006)~~ — **superseded by R6 D26** (round 6 reframes D26: isolation is the product; the agent holds zero credentials. ADR-0006 is now written against the revised D26.)

**Where:** `runner/` (a library with no `main`, no image), `api/v1alpha1`
(no agent/model fields), `TDD-PLAN-PHASE1.md` (A1–A4 not started; B-slices
first), `docs/PLAN.md` line 36 (still says the runner writes
`status.observedPhase` — stale since ADR-0004).

**Problem:** every B-slice so far judges an agent's output, but no agent can
run: the sandbox runs `sleep infinity`, the runner has only ever talked to a
fake model, and a Loop can't say which agent or model to use. B3's verify
Job needs a commit an agent actually pushed, so the verify path can't be
tested end to end until an agent exists. The build order has drifted from
"agents that need an LLM" to "a state machine with nothing to drive it".

**Decide and record (ADR-0006):**
1. **Agent contract** (already half-defined by ADR-0004): reads the prompt +
   `.coxswain/desired-phase`, works in `/workspace`, commits/pushes to the
   Loop branch, writes `.coxswain/result.json`. Any image that honors it is a
   runner.
2. **Which agent first** — *owner's call*:
   - **(a) Built-in agent**: grow `runner/` (OpenAI-compatible tool loop)
     into the default runner image, pointed at the homelab vLLM. Full
     control, smallest dependency surface, weakest agent.
   - **(b) Wrap an existing coding agent** (e.g. Claude Code, pi, Codex,
     OpenHands) in a thin adapter image that maps the contract to its CLI.
     Much stronger agent on day one; more to isolate (it brings its own tools,
     network needs and credentials).
   - Reviewer recommendation: **(a) first as the reference implementation of
     the contract** (it's already tested and small), with the contract written
     so (b) adapters are a Phase 2 addition, not a redesign.
3. **Loop API**: add `spec.agent` —
   `{ image, model, endpointSecretRef, maxSteps?, timeouts? }` — where
   `endpointSecretRef` holds the base URL + API key. Default from a
   cluster-wide `coxswain-agent-defaults` ConfigMap so the README sample stays
   short.
4. **Credentials & egress**: the operator mounts the model secret into the
   sandbox (the model key is the one credential the agent *must* have — keep it
   separate from the git push token); the sandbox NetworkPolicy allows egress
   to the model endpoint and the git host only. Phase 2's metering sidecar sits
   on that same egress path, so route model traffic through a single
   `COX_MODEL_BASE_URL` now.

**Resequence the plan:** after D25/D27, do **A1 → A2 → A3 → A4 + runner
`main` + image + sandbox wiring** *before* B3, so B3's end-to-end test runs
against a commit a real agent produced. Add an e2e milestone: "a Loop on a
toy repo, agent on vLLM, reaches `Verifying` with a pushed commit".

**Acceptance:** ADR-0006 committed with the owner's choice; `spec.agent` in
the plan; A-slices + wiring ordered before B3; `PLAN.md` line 36 corrected.

---

## Next round will check

- D25, D27 (P1).
- ADR-0006 once the owner picks the first agent.
