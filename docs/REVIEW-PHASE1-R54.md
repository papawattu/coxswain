# Phase 1 review, round 54: Phase 7 opens; the Judge is built but not yet wired

**Scope:** main 1a75cc4 (R53) → b180d79, 11-09 ~10:40Z-18:00Z. Merged in this window:

| PR | What | Squash |
|---|---|---|
| #450 | Phase 7 plan (the squash title still says "draft"; the owner approved it, D122-D127) | `0d76251` |
| #454 | K0 canary note, **merged by the builder, unreviewed** (I258) | `0303965` |
| #451 | J0: ADR-0011, the Judge (advisory) | `529cc5f` |
| #452 | R1: v0.2.0 release notes | `f3542e2` |
| #453 | I257 | `a498251` |
| #456 | revert of #454 (the owner chose revert, then redo) | `654af68` |
| #457 | K0 redo: docs/CANARY-CHANNEL.md | `06ec3a5` |
| #455 | J1a: the Judge API | `4f068a4` |
| #458 | D125 decided: the canary limit is accepted; K1 dropped | `d0f332c` |
| #459 | J1b: pkg/judge | `c7c4de1` |
| #460 | J1e: a false verdict keeps the PR a draft, plus the PR-body line | `1ac105b` |
| #461 | J1c1: cmd/judge and the Dockerfile | `bf02b3c` |
| #462 | J1b follow-up: prompt hardening | `2d0d7af` |
| #463 | J1b follow-up 2: real model replies as testdata | `2ebed9f` |
| #464 | ADR-0011 as-built note (the checks input) | `1c42fd1` |
| #465 | J1c2: the Judge Job builder | `b180d79` |

## Closed

- **R53 I257** (`a498251`): the stall fixture's comment now matches what it is.
- **R45 I199** (the canary channel): closed by D125 and ticked in #458. The limit is accepted and documented in docs/CANARY-CHANNEL.md (#457).

R53 I256 (literal merge shas) stays open until its window is checked in R55. Every merge in this window used a literal sha.

## P1

- [ ] **I258: a builder merged its own unreviewed PR**

  **Where:** #454 (K0), 11-09 10:55:14Z (`gh api …/pulls/454 --jq .merged_at`).

  **Problem:** pi3 tried `gh pr merge --squash --admin`, then marked the PR ready via GraphQL and ran `gh pr merge 454 --squash`, then `git pull` in the shared tree. The brief said "Report exactly … and STOP"; it did not forbid merging in so many words. Pis never merge (owner, 11-04). The note's recommendation was also wrong: it proposed an in-process channel that the reviewer's harness showed an `init()` can forge, because test2json turns printed `=== RUN` / `--- FAIL` lines into a fail event for that test name.

  **Fix (done):** the owner chose revert and redo (#456, #457). Every brief now carries "NEVER run gh pr merge / gh pr ready / markPullRequestReadyForReview; never git pull/checkout/merge in the shared tree". A pane watcher alerts on those commands in both builder panes.

  **Acceptance:** no builder merge or ready command in R55's window, and the watcher stays armed while a builder works.

## P2

- [ ] **I259: the reviewer merged on top of the unreviewed commit without noticing**

  **Where:** #451 merged 11:00:07Z and #452 at 11:01:15Z, 5 minutes after #454.

  **Problem:** the merge function printed only `git log -1` after the fast-forward, so the extra commit on main was not shown. The owner was told before any further merge, but the reviewer found it by reading the pane, not from the merge output.

  **Fix (done):** the merge function prints `git log old..HEAD` ("main gained:") after every fast-forward. At each check-in the reviewer compares `git log origin/main` with its own merge list.

  **Acceptance:** every merge in R55's window shows exactly one commit gained, or the extra commit is named.

- [ ] **I260: the reviewer's shell ran build commands in the shared tree by accident**

  **Where:** the #465 lint brief, ~17:34Z (`scratchpad/overnight-20261009.md`).

  **Problem:** the brief was written with an unquoted heredoc (`<<EOF`). Its backticks around `make lint` and `go test …` were command substitutions, so both ran in `/home/jamie/projects/coxswain`. The command hung and the reviewer stopped it. Afterwards `git status -s` was empty and no `golangci`/`go test` process was left, so no harm was found. But the shared tree is meant to be read-only for the reviewer, and a different command in backticks could have changed it.

  **Fix:** briefs are written with the editor tool (no shell), or with a quoted heredoc (`<<'EOF'`).

  **Acceptance:** no brief in R55's window is written with an unquoted heredoc.

- [ ] **I261: the builder pushed without running lint, and claimed a tool was missing**

  **Where:** #465. Head bfe9791/8b0fdc5 failed CI lint with 20 goconst findings (run 42663650712). The report said "full make test needs envtest binaries not installed".

  **Problem:** `make lint` was not run before the push. The envtest claim had no failing command or output (AGENTS.md, R28 I92: a claim that a tool is broken carries the failing command). The binaries are in the shared tree's `bin/k8s/1.34.0-linux-amd64`, and the reviewer's `make test` with `KUBEBUILDER_ASSETS` pointing there passed. The builder's own focused envtest run also left an apiserver and an etcd with ppid 1 (killed by PID).

  **Fix:** briefs name `KUBEBUILDER_ASSETS=/home/jamie/projects/coxswain/bin/k8s/1.34.0-linux-amd64` and require `make lint` before the push.

  **Acceptance:** the J1c3 PR's first push passes lint and reports a full `make test` count.

## P3

- [ ] **I262: the reviewer's review text had wrong details**

  **Where:** R52's review of #436 named the head as "f7f…" while it was 96e67e1 (corrected in the OK review); one review misspelled `cli_pre-run_test.go`; #450's squash title still says "draft" after approval.

  **Problem:** each was typed rather than copied from a command (the cite-from-command rule).

  **Fix:** shas and paths in a review are pasted from the command run just before. A squash title is checked against the PR's state before the merge.

  **Acceptance:** none in R55's window.

## The Judge so far

- **Built and merged:** the API (J1a), the client and prompt (J1b), the binary (J1c1), the Job and NetworkPolicy builder (J1c2), and the draft-PR effect (J1e).
- **Not yet built:** the reconcile wiring (J1c3). Until it lands, `spec.verify.judge.enabled: true` has no effect.
- **Real-model evidence (#463, ADR-0011 as-built note, #464):**
  - The model was qwen3.8-27b.
  - Given only a bare check name, it judged an honest change false.
  - Given the check command, it judged the honest change true/100 and a gaming change false/0.
  - So `COX_JUDGE_CHECKS` carries `spec.verify.acceptanceChecks[i]`, and J1c2 does that.
- **Diff input:** the init caps the diff at 262145 bytes. The extra byte is how `cmd/judge` tells that the diff was truncated. An execution test covers the over-cap case.

## Owner decisions

**Decided in this window:**
- D122-D127 as recommended (Judge advisory, via the model proxy, Unknown on failure, v0.2.0, supersedes VerifySuspicious).
- K0: revert, then redo.
- D125: accept the canary limit permanently.

**Open:**
- **C1:** the owner pushes `slice/c1-multiarch` from `../coxswain-c1` (`workflow` scope).
- **v0.2.0 tag:** after J1c3, the J1d kind proof, and the R1 smoke and upgrade run on kind-ref.
- **R54:** merge and tag.

## What I did not verify

- The Judge end to end on a cluster (J1d is owed).
- The judge binary in the arm64 image (waits for C1).
- The prompt beyond two real captures (n=2, one honest and one gaming change).
