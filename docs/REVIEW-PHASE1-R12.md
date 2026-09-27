# Phase 1 review, round 12 — branch protection is on

Process round (2026-09-27), since tag `review/phase1-r11`. First review doc
delivered through a pull request.

## What's in force on `main`

Set by the owner's request:

- **Pull request required** for every change — builder, reviewer and owner
  alike (`enforce_admins` on). No direct pushes.
- **Force pushes and branch deletion blocked.**
- **All review conversations must be resolved** before merge. An unresolved
  reviewer comment on a PR blocks the merge — that is how a P1 finding gates.
- **Required approvals: 0.** See I39.
- **Required status checks: none yet.** See I40.

---

## P2 — Process

### I39. Nobody can "Approve" a PR here — use comment reviews + resolved threads

- [ ] Noted in `AGENTS.md`

The builder, the reviewer and the owner all act through the same GitHub
account, and GitHub doesn't let an account approve its own PR, so a required
approval could never be satisfied. Instead:

- The reviewer submits a **Comment** review whose body starts with the
  verdict: `Verdict: OK`, `Verdict: OK + notes`, or `Verdict: CHANGES`.
- Each finding is an inline comment. P1 findings stay unresolved until fixed;
  **required conversation resolution** makes them block the merge.
- The owner merges when the verdict is OK and no threads are open.

Update `AGENTS.md` → *Review protocol* → *Pull requests* to say this (replace
"Approve or Request changes").

### I40. Give each CI job a unique name, then require them

- [ ] Done

**Where:** `.github/workflows/test.yml`, `lint.yml`, `test-e2e.yml`.

**Problem:** all three jobs report the check name **"Run on Ubuntu"**
(kubebuilder scaffold). Required status checks match by name, so requiring
"Run on Ubuntu" couldn't distinguish a passing lint from a failing test.

**Fix:** set `name: test`, `name: lint`, `name: e2e` on the three jobs in a
small PR. After it merges, the owner adds `test` and `lint` as required
checks on `main` (add `e2e` once the kind e2e from I27 is real and stable).

**Acceptance:** the PR's checks list shows `test`, `lint`, `e2e` as separate
entries; after the owner updates protection, a red `test` blocks merge.
