# Phase 1 review, round 2 — open issues

Review of `5f42363`..`5dcfaa4` (2026-09-27), since tag `review/phase1-r1`.
Baseline at review time: `internal/controller` envtest green on the pinned
1.34 envtest binaries.

Each issue is self-contained so it can be picked up independently. Work
test-first as in `docs/TDD-PLAN-PHASE1.md`: write the failing test at the
named seam, then fix. Tick the box and add the commit hash when done. Issue
IDs continue project-wide so every ID is unique.

Priority: **P1** = fix before the next slice builds on it. **P2** = decide
during Phase 1. **P3** = cleanup, batch whenever.

## Verdicts on reviewed commits

| Commit | Issue | Verdict | Follow-ups |
|--------|-------|---------|------------|
| `5f42363` | D23 runner report never exits `Verifying` | OK | — |
| `5b098c8` | bookkeeping (I4/D5/D6, D23/I22) | OK | — |
| `5dcfaa4` | D22 agent-sandbox v1.0.4 on kind 1.34 | OK + notes | I23, I24 |

D23 is fixed the way it should be: `nextPhase` only handles the three
claim-driven steps, its doc says it never returns `Succeeded`/`Failed`, and
there's a cluster-seam test that a `Succeeded` report can't leave
`Verifying`. The B1 envtest now stops at `Verifying`.

D22 is a useful result: v1.0.4 reconciles on 1.34 from the **release**
manifest (the source `k8s/controller.yaml` is a `ko://` placeholder, which
explains Phase 0's "no pullable image"), the Service is opt-in via
`spec.service`, and the "≥1.37" claim is corrected in `PLAN.md`.

**No P1 items open. B2 can proceed.**

---

## P2 — Design

### I23. Make the agent-sandbox install reproducible, not a commit-message recipe

- [ ] Done

**Where:** `5dcfaa4` commit message (the only record of the install steps);
`Makefile`; `docs/E2E-PHASE0.md`.

**Problem:** the smoke's install path (release manifest URL for v1.0.4, kind
node image, wait conditions) lives only in a commit message, and the cluster
was torn down. Phase 1's e2e (runner in sandbox, verify Job) and Phase 3's
csi-hostpath setup will need the same bring-up, and the version is pinned
nowhere in code.

**Fix:** add `AGENT_SANDBOX_VERSION ?= v1.0.4` to the `Makefile` and a
`make kind-up` target (or `hack/kind-up.sh` called from it) that:
creates the kind cluster on `$(KIND_NODE_IMAGE)`, applies
`https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$(AGENT_SANDBOX_VERSION)/sandbox.yaml`,
and waits for the controller rollout. Add a `make kind-smoke` that creates a
bare `Sandbox` and waits for `Ready=True` — that's D22's evidence, rerunnable.
Hook `setup-test-e2e` to `kind-up`. Also check the vendored
`config/crd/external/agents.x-k8s.io_sandboxes.yaml` against the release
manifest in CI (or a make target) so the two can't drift silently.

**Acceptance:** `make kind-up kind-smoke` from a clean machine reproduces
D22's result; the version string exists in exactly one place.

---

## P3 — Cleanup

### I24. D20's stated reason is now inaccurate

- [ ] Done

The Loop-name CEL rule's message and the `loop_types.go` comment say the name
"names the Sandbox and its Service". D22 found the Service is opt-in
(`spec.service: true`), and Coxswain doesn't set it. Keep the rule —
DNS-1035 is still the right constraint if a Service is ever enabled (e.g. a
runner health endpoint), and a stricter name costs nothing — but reword to
"it names the Sandbox (and its Service, if one is enabled)" so the admission
error doesn't mislead. Regenerate the CRD.

---

## Next round will check

- B2 (tamper check via base-commit glob diff) as it lands. Watch items for
  B2 from ADR-0005: `baseCommit` resolved by the operator via go-git (D15),
  protected globs from `spec.verify.preset` + `protectedPaths[]` (D16), the
  tamper check in its own trusted init container before any agent code runs
  (D14), and the three anti-gaming fixtures (edit existing `*_test.go`, add a
  new `*_test.go` with `TestMain`, add a `replace` to `go.mod`) each ending
  `Failed:TamperedVerify` even when `result.json` claims otherwise.
- I23, I24.
