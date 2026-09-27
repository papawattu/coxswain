# Phase 1 review, round 1 — open issues

Review of `142a928`..`df4bd7b` (2026-09-27), since tag `review/phase0-r12`.
First Phase 1 round: the owner's Phase 0 sign-off / D5 / D6 decisions, and
slice B1.

Each issue is self-contained so it can be picked up independently. Work
test-first as in `docs/TDD-PLAN-PHASE1.md`: write the failing test at the
named seam, then fix. Tick the box and add the commit hash when done. Issue
IDs continue from the Phase 0 rounds (I22+, D22+) so every ID is unique
across the project.

Priority: **P1** = fix before the next slice builds on it. **P2** = decide
during Phase 1. **P3** = cleanup, batch whenever.

## Verdicts on reviewed commits

| Commit | Issue | Verdict | Follow-ups |
|--------|-------|---------|------------|
| `142a928` | I4 sign-off, D5 k8s 1.34, D6 kind snapshots | OK + notes | D22 |
| `df4bd7b` | Phase 1 B1 phase transitions | **CHANGES** | **D23 (P1)**, I22 |

`142a928` records the owner's decisions faithfully. Tick I4, D5 and D6 in
`REVIEW-PHASE0.md` with `142a928`.

B1's shape is right: a pure `nextPhase(current, reported)`, a table test,
and an envtest slice, with at most one status write per reconcile. One
transition in the table breaks ADR-0005 — see D23.

---

## P1 — Blocking (fix before B2/B3 build on the phase machine)

### D23. `Verifying → Succeeded` is driven by the runner's report

- [x] Done (5f42363: the `Verifying` case is deleted from `nextPhase` — no path
  returns `Succeeded` or `Failed`; the claim-driven path stops at `Verifying` and
  every exit out of `Verifying` is evidence-gated by `verifyOutcome(job)` in B3.
  Pure table + envtest updated red → green.)

**Where:** `internal/controller/loop_controller.go` `nextPhase`
(`case LoopPhaseVerifying: if reported == LoopPhaseSucceeded`);
`loop_phase_transition_test.go` and `loop_phase_transition_envtest_test.go`
("drives the full happy path to Succeeded"); `TDD-PLAN-PHASE1.md` B1.

**Problem:** `observedPhase` is the operator's record of the runner's
*claim* (ADR-0004), and ADR-0005's core rule is that no gate decision is made
on a claim. `Succeeded` is **the** gate: it must follow only from verify-Job
evidence (tamper container exit 0, canary behaving, every check container
exit 0 — B2/B3/B3a). As written, a runner (or a prompt-injected shell) that
reports `Succeeded` while in `Verifying` completes the Loop with no verify
run at all. The envtest locks this in by asserting the happy path reaches
`Succeeded` from reports alone.

The other three transitions are fine as claim-driven: the runner saying
"I've planned" / "I've implemented" only moves the Loop *toward* the
evidence-gated step, and a false claim just triggers verify early.

**Fix:**
1. Red: in the `nextPhase` table, `(Verifying, reported=Succeeded)` →
   `Verifying` (stays put). In envtest, a Loop in `Verifying` with
   `observedPhase: Succeeded` and no verify Job stays `Verifying` after
   reconcile.
2. Green: delete the `Verifying` case from `nextPhase`. Document on
   `nextPhase` that it only handles claim-driven transitions and that every
   exit from `Verifying` (→ `Succeeded`, → `Implementing`, → `Failed:*`) is
   decided by `verifyOutcome(job)` in B3 — never by `observedPhase`.
3. Change the envtest "full happy path" to stop at `Verifying`; the
   end-to-end happy path to `Succeeded` moves into B3's test, where it's
   driven by Job/pod status.
4. B1 in `TDD-PLAN-PHASE1.md`: say explicitly that the runner's report never
   moves a Loop out of `Verifying`.

**Acceptance:** the new red cases fail on `df4bd7b` and pass after; no path
in `nextPhase` returns `Succeeded` or `Failed`.

---

## P2 — Design

### D22. Confirm agent-sandbox v1.0.4 runs on kind 1.34 before Phase 1 depends on it

- [x] Verified (commit 5b098c8 docs; smoke run on kind `coxswain-d22`, node v1.34.0). The v1.0.4 **release** manifest (`releases/download/v1.0.4/sandbox.yaml`) uses the real image `registry.k8s.io/agent-sandbox/agent-sandbox-controller:v1.0.4` (the source `k8s/controller.yaml` is a `ko://` placeholder — the reason Phase 0 thought no image existed). The controller rolls out 1/1 Running, acquires the leader lease, and reconciles a bare `Sandbox` to a Running pod (Sandbox Ready=True). The per-Sandbox Service is **opt-in** (`spec.service: true`) — with it enabled the Service `d22-smoke` + `serviceFQDN` appear. The vendored CRD at `config/crd/external/` already matches v1.0.4 (has `spec.service`/`shutdownPolicy`/`shutdownTime`), so no re-vendoring. Result recorded in `docs/PLAN.md` decision 12 + risk line; the "needs >=1.37" folklore is corrected (it reconciles on 1.34; the floor is a release-tracking decision, not a hard requirement).

**Where:** `docs/PLAN.md` decisions 9–10; `Makefile` `KIND_NODE_IMAGE`,
`ENVTEST_K8S_VERSION`.

**Problem:** the earlier docs say production needs k8s ≥1.37 "for
agent-sandbox"; D5 pins dev to 1.34. Since I4 moved "run the agent-sandbox
controller on kind" into Phase 1, kind now has to *run* agent-sandbox, not
just hold its CRD. agent-sandbox v1.0.4 builds against `k8s.io/api v0.37.0`;
its docs don't state a minimum server version (checked the module's
`README.md` / `docs/`), so whether it installs and reconciles on 1.34 is
unknown. If its CRDs or controller use 1.35+ features, the Phase 1 e2e fails
for reasons unrelated to Coxswain.

**Fix:** as the first Phase 1 infra step, before B2: install agent-sandbox
v1.0.4 on a kind 1.34 cluster and create one bare `Sandbox`; confirm the pod
and Service appear. Record the result in `docs/PLAN.md`. If it fails, raise
it with the owner (bump kind/envtest to 1.37, or pin an older agent-sandbox);
don't pick silently. Also correct or source the "≥1.37 for agent-sandbox"
claim so it isn't folklore.

---

## P3 — Cleanup

### I22. `desiredPhase` is recorded but nothing writes `.coxswain/desired-phase` yet

- [x] Noted in plan (5f42363: field doc now says the `.coxswain/desired-phase`
  copy "will be" implemented in the Phase 1 runner/exec wiring slice, not that
  it is.)

`LoopStatus.desiredPhase`'s doc says it "is also copied to
`.coxswain/desired-phase` for the runner to read", but no code does that yet
(it lands with the runner/exec wiring). Either soften the field doc to
"will be copied (Phase 1, runner wiring slice)", or name the slice that adds
it in `TDD-PLAN-PHASE1.md`, so the API doc doesn't promise behavior that
doesn't exist.

---

## Next round will check

- D23 fix (before B2/B3 code).
- D22 smoke result.
- B2 (tamper check) as it lands.
