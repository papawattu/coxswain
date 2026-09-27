# Phase 1 review, round 11 — switch to pull requests

Process round (2026-09-27), since tag `review/phase1-r10`. No code reviewed.

**Owner decision:** from now on code changes go through pull requests. `main`
has been pushed to `origin` (it was 25 commits ahead), so GitHub Actions
(test, lint) now run on PRs. The full protocol is in `AGENTS.md` →
*Review protocol*; the short version:

- **One branch + one PR per slice or review item**, opened as a draft early:
  `slice/<id>-<name>` or `fix/<id>-<name>`.
- **Ready for review** when CI is green and the acceptance is met.
- **Code findings are PR review comments**; P1s are *Request changes*.
  Follow-up commits on the branch, no force-push over reviewed commits.
- **The owner merges** (squash, PR title as subject). Builder and reviewer
  don't merge.
- **Review docs + `review/*` tags continue** for design, ADRs, plan changes,
  cross-cutting issues and owner questions.

## What the builder does now

1. **Move the in-progress I34/I35 work onto a branch** without losing it:
   `git switch -c fix/i34-i35-agent-pod` (uncommitted changes come with you),
   commit there with explicit paths, push, and open a **draft PR** titled
   `I34+I35: literal-only agent env; non-root UID for the sandbox pod`. Put the
   kind re-run of README steps 1–4 in the PR description as evidence for I35.
2. The untracked `internal/controller/loop_c2_proxy_test.go` belongs to C2:
   leave it out of the I34/I35 PR (explicit paths), and start
   `slice/c2-model-proxy` from `main` after the I34/I35 PR merges.
3. Stop tagging review items as closed in review docs with a bare commit
   hash; use the **PR number** from now on (e.g. `Done — #3`).

## Owner actions (not the builder's)

- **Protect `main`** on GitHub: require a pull request, and require the
  `test` and `lint` checks to pass before merging. Until then the protocol is
  honor-system.
- **`gh` token:** the `GITHUB_TOKEN` in this environment is missing the
  `read:org` scope; `gh pr create` / `gh pr review` may still work — if they
  fail, refresh the token.
