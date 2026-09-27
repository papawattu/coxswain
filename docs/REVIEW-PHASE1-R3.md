# Phase 1 review, round 3 — open issues

Review of `3e80a77`..`86c0fc3` (2026-09-27), since tag `review/phase1-r2`.
Covers the builder's I23/I24 commit and the owner-requested README
"Getting started" (`86c0fc3`, written by the reviewer).

Each issue is self-contained so it can be picked up independently. Tick the
box and add the commit hash when done. Issue IDs continue project-wide so
every ID is unique.

Priority: **P1** = fix before the next slice builds on it. **P2** = decide
during Phase 1. **P3** = cleanup, batch whenever.

## Verdicts on reviewed commits

| Commit | Issue | Verdict | Follow-ups |
|--------|-------|---------|------------|
| `3e80a77` | I23 reproducible agent-sandbox install, I24 D20 wording | OK | — |
| `86c0fc3` | README getting started (reviewer, at the owner's request) | — | I25, I26, I27 |

I23 is right: the version lives once in `AGENT_SANDBOX_VERSION`,
`kind-up` / `kind-smoke` / `crd-drift-check` exist, and `setup-test-e2e`
depends on them. (The `kind-smoke` readiness check correctly uses `$$r`, so
the shell variable survives make's expansion.)

**README verified end to end** on a fresh kind 1.34 cluster before commit:
agent-sandbox v1.0.4 from the release manifest → `make docker-build` /
`kind load` / deploy → sample Loop → Loop `Pending`, Sandbox `Ready=True`,
pod `Running`. The README is now the first thing users see, so it must stay
true as Phase 1 lands.

---

## P2 — First impressions

### I25. Rename the leftover `coxscaf` scaffold name everywhere

- [ ] Done

**Where:** `config/default/kustomization.yaml` (`namespace: coxscaf-system`,
`namePrefix: coxscaf-`), `app.kubernetes.io/name: coxscaf` labels across
`config/`, `AGENTS.md` title, `Makefile` buildx builder name,
`test/e2e/e2e_suite_test.go`, RBAC comments
(`grep -rn coxscaf . --exclude-dir=bin --exclude-dir=.git`).

**Problem:** kubebuilder was scaffolded as `coxscaf`. Users following the
README now deploy into `coxscaf-system` and see
`coxscaf-controller-manager` — the README has to print those names, which
reads like a typo on the first page.

**Fix:** rename to `coxswain` / `coxswain-system` /
`coxswain-controller-manager` in one commit, and **update the two
`coxscaf` references in `README.md` step 2 in the same commit**. Also update
`docs/E2E-PHASE0.md` if it names the namespace.

**Acceptance:** `grep -rn coxscaf . --exclude-dir=bin --exclude-dir=.git
--exclude='REVIEW-*'` is empty; README step 2 re-run on kind (or via I27)
reaches the rollout.

### I26. Add a LICENSE file

- [ ] Done

The README and every Go file header say Apache 2.0, but there's no
`LICENSE` file at the repo root, so GitHub shows "no license" and the
project isn't actually licensed for reuse. Add the standard Apache-2.0
`LICENSE` text (copyright line matching `hack/boilerplate.go.txt`).

---

## P3 — Keep the README honest

### I27. Guard the README against drift

- [ ] Done

**Problem:** nothing tests the README's commands. Each Phase 1 slice changes
what a Loop does (B1 phase moves, runner wiring, verify Job), and the
"Watch it" output and the "That's as far as a loop goes today" line will go
stale silently.

**Fix:**
1. Make the round-1 deferred e2e suite (`test/e2e`) follow the README path:
   `kind-up` → build/load/deploy → apply `config/samples/…` → assert Loop
   phase, Sandbox `Ready`, pod `Running`. Then the README and the e2e share
   one script of truth.
2. Add a line to the builder's per-slice checklist: if a slice changes what
   `kubectl get loops` shows for the sample, update README step 4 in the same
   commit.
3. Minor: `make deploy` runs `kustomize edit set image`, which leaves
   `config/manager/kustomization.yaml` modified in the user's clone. Either
   note it in the README or have `deploy` restore the file afterwards.

---

## Next round will check

- B2 (tamper check) as it lands — watch items listed in round 2.
- I25 (with README update), I26, I27.
