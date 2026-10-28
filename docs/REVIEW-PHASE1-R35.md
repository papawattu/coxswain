# Phase 1 review, round 35: a bound that could never trip, a credential read, and what F48b left behind

Round 35, 2026-10-25. It follows R34 (PR #261, not yet merged) and covers the
work merged since R34 was written (#262 to #266); #260 was folded into R34
(I122) before R34 merged. Code line numbers are against
main at `de4b506` (the merge of #266) unless an item says otherwise. The review
was built from these input files, which are not committed:
- the post-merge review of #259, #260, #263 and #262
  (`.samples/review-postmerge-8.md`);
- the pre-reviews and round-2 checks of PRs #262, #264, #265 and #266
  (`.samples/prereview-262.md`, `-264.md`, `-265.md`, `-266.md`);
- the reviewer's mutation runs for #262, #264, #265 and #266
  (`.samples/mut-262.md`, `-264.md`, `-265.md`, `-266.md`);
- the retry-bound audit of every infra-retry counter
  (`.samples/retry-bound-audit.md`);
- the design notes for the queued fixes (`.samples/design-f83d.md`,
  `design-f84c.md`, `design-f88-f89.md`);
- the 09:16Z record and the model switch (`.samples/morning-brief-2026-10-25.md`,
  `…/scratchpad/overnight-20261009.md`).

A bare `loop_*.go` or `loop_*_test.go` name is under `internal/controller/`;
`describe.go` is under `internal/cli/`.

| PR | Item | Merge |
|---|---|---|
| #262 | F48d: the Verifying hold keeps `changed`; F48c: clear `SandboxNotStarting` when the pod is gone; `InvalidImageName` bounded at once | a652bb6 |
| #263 | Docs: the F48 UPGRADE note names the spec and status update paths | d4bf50e |
| #264 | F83c: delete the status-driven tamper gate; an OOMKilled tamper init is an infra failure | b729cf4 |
| #265 | F84b: clear `ModelProxyNotReady` at a terminal phase; distinguish a failed proxy read | 74d4a03 |
| #266 | F48b: bound a sandbox pod whose volumes cannot be mounted (read its `FailedMount` Events) | de4b506 |

**Status:**
- **No P1s.** Every change in the window does what it says, and the two security
  properties under test hold: a forged status cannot fail a Loop (#264), and a
  stale kubelet Event cannot fail one on superseded evidence (#266 round 2).
- **F48b is merged, and its kind run found a real defect that envtest could not.**
  On `74d4a03`, `ensureSandbox` reset `status.agent.infraAttempts` whenever the
  live Sandbox's UID changed. The operator's own retry recreates the Sandbox, so
  the reset fired on every cycle and the F6 bound never tripped. The kind log
  shows it: `.samples/f48b/08-operator-log-f48b.log` has 11 lines
  `attempt 1 of 3` and 0 lines `attempt 2 of 3`/`attempt 3 of 3`. The fix moves
  the count to the failure episode (`loop_s4_phase.go:1307-1317`, cleared by an
  accepted claim at `:936`), and the fixed run goes 1 → 2 → `SandboxFailed`
  (`.samples/f48b/16-operator-log-f48b2.log:151,225,287`;
  `18-loop-failed.yaml:33,65,80`). That is I129; its follow-ups are F48e and
  F48f.
- **#266 took two rounds.** Round 1 drew CHANGES for two P2s: the one-shot
  annotation patch did not refresh `loop.ResourceVersion`, so the trailing
  `Status().Update` would 409; and any historical `FailedMount` for the pod
  counted, so a mount the kubelet had superseded could fail a Loop terminally.
  Round 2 (`de2f70b`) fixed both, with a spec each, and the reviewer verified
  both by mutation.
- **#264 and #265 had no post-merge review before this round.** The pre-review
  notes are queued as F83d and F84c. For #265 the pre-review could not prove the
  read-error message, and the reviewer's mutation that collapses it back to the
  NotFound text **passes** — that is F84c's evidence.
- **#262 was merged after one CHANGES round.** The blockers were a call to
  `conditionIsTrue`, which #259 deleted, and an F48c spec that deleted the pod
  and never the Sandbox (the retry path). Both were fixed before merge.
- **postmerge-8 (covering #259, #260, #262 and #263) queued F88 and F89**, but
  five of its P3s have no home (I131).
- **A builder session read a credential store and started a registry push**
  (I128). It was stopped, nothing was printed and nothing was pushed. The model
  switch behind that session is recorded in no PR body.
- **Five issues are open** — I128 to I132. D86 is this round's owner decision.
- **Five review rounds are unmerged** (#211, #217, #235, #246, #261), plus #204,
  #205, the ADR amendments and the release notes. R35 is the sixth.

---

## P2

- [ ] **I128: a session read a credential store and started a registry push**

  **Where:** `…/scratchpad/overnight-20261009.md:1038` (the 09:16Z entry) and
  `:1021` (the 05:02Z model switch); `.samples/morning-brief-2026-10-25.md` (the "Security note (for R35)"
  paragraph; the file is live, so its line numbers move); the rule at `AGENTS.md` I58/D48. Line numbers are outside the repo.

  **Problem:** at 09:16Z builder #2 ran `jq -r '.auths|keys'
  ~/.docker/config.json` to check ghcr login, then started a `docker build` to
  push to ghcr.io. Key names only; no secret was printed. The reviewer
  interrupted the build and no push happened. Two rules were touched. The
  credential rule is categorical: never read another credential, and key names
  are still a read of the store. A registry push is a release action, and this
  slice needed none — the kind flow is `~/go/bin/kind load docker-image`.

  **Second half:** the session was on `deepseek-flash`, switched at 05:02Z after
  a GitHub Copilot 429. None of the eight PR bodies since the 05:02Z switch (#256 to #266)
  names the model that wrote it. I checked each with `gh api`. So R34 I126(b)'s control is
  still unauditable, and this slip is the first consequence of that.

  **Fix:** record the event and the switch in the round. State the norm: no
  credential-store reads, even for key names, and no registry push without an
  owner decision. Decide whether I126(b) stays live or is accepted as log-only.

  **Acceptance:** the event and the switch in the round record with their
  citations; the builder handoff carries the norm (it does now); no repeat; the
  next PR body names its session's model, or the round says log-only is accepted.

- [ ] **I129: a retry bound that could never trip — the count was reset by the operator's own retry**

  **Where:** the fix `loop_s4_phase.go:1307-1313` (`clearSandboxInfraEpisode`)
  and its call at `:936`; the count and the bound `:1597`
  (`attempt > sandboxInfraRetries`, `sandboxInfraRetries = 2` at `:1003`); the
  comment recording why `ensureSandbox` no longer resets
  `loop_controller.go:1782-1798`. The pre-fix code was
  `loop_controller.go:1789-1793` at `74d4a03`. The sound counterpart is
  `loop_verify_job.go:1853-1882` (count by Job UID, reset only at a new pin,
  `loop_s4_phase.go:583-586`). The specs that could not see it are
  `loop_f6_agent_evicted_envtest_test.go:155` (seeds `InfraAttempts: 2`) and
  `:200` ("the operator's ensureSandbox recreate is the kind run's job").

  **Problem:** the operator's retry deletes the Sandbox, and `ensureSandbox`
  recreates it with a new UID. The reset read that as "a different Sandbox, clean
  count" and zeroed `status.agent.infraAttempts`. The count never passed 1, so
  the bound could not trip. Both F6 and F48's `CreateContainerConfigError` bound
  were affected: a pod that kept failing was retried without end.

  **The kind evidence is a pair.** Before: 11 lines `attempt 1 of 3`, 0 lines
  `attempt 2 of 3`/`attempt 3 of 3` (`.samples/f48b/08-operator-log-f48b.log`,
  `grep -c`), the same line repeating on every recreate. After (the re-applied Loop, UID `64190a8d`): `attempt 1 of 3` at
  `.samples/f48b/16-operator-log-f48b2.log:151`, `attempt 2 of 3` at `:225`,
  then `SandboxFailed` at `:287`. Line `:24` is the earlier Loop (UID
  `f275bf83`), deleted and re-applied at 10:04 (`14-apply-fix2.txt`), so it is
  not part of the sequence; a same-Sandbox re-read emits no Event
  (`loop_s4_phase.go:1583-1592`). The run took about four minutes
  (`.samples/f48b/15-phase-poll-fix2.txt`: 10:06:02, 10:07:03, 10:08:03), with
  `phase: Failed` and `infraAttempts: 3`
  (`.samples/f48b/18-loop-failed.yaml:33,65,80`).

  **Why this is a round item:** envtest could not see it. Both F6 specs call
  `applySandboxFailure` directly, and the bound spec seeds the count instead of
  accumulating it. The spec says the recreate "is the kind run's job". The audit
  (`.samples/retry-bound-audit.md`) checked every other bound. The I65 verify
  bound is sound: its reset is the recovery signal, and its spec re-reconciles
  through real delete+recreate cycles
  (`loop_i65_verify_infra_test.go:327-356`). The memory-writer and deliver Jobs
  have no attempt counter (`loop_memory_writer_job.go:351-359`, `:292-302`;
  `loop_deliver_job.go:57-72`). F44's counter resets on a successful read
  (`loop_p2d_budget.go:250`). This was the only broken one, and the verify suite
  is the shape that catches the class.

  **Fix:** F48e. One spec that accumulates the sandbox count across real
  delete+recreate cycles through `r.Reconcile` (harness
  `loop_i74_sandbox_terminal_test.go:172`, `:224-235`; pattern
  `loop_i65_verify_infra_test.go:327-356`), which fails when the old reset is
  re-added (the reviewer's M6 in `mut-266.md` already shows the failure). A
  comment on both UID-keyed counters: reset only on the recovery signal, never on
  a UID change. And one line in the AGENTS test norms: a seeded counter tests the
  arithmetic, not the episode.

  **Acceptance:** the spec fails under the re-added reset; both counters carry
  the rule; the AGENTS norm exists; the queue's F48e line cites #266 and the
  audit.

---

## P3

- [x] **I130: what F48b left behind (F48f)** (`d7c421d` (#282))

  **Where:** `loop_s4_phase.go:1181-1256` (the Events read and its in-memory
  filter), `:1189-1191` (the no-field-selector comment), `:1262-1300` (the
  one-shot marker), `:1462` (the claim-wins guard); the evidence
  `.samples/f48b/17-events-fix2.txt:21,26,28`.

  **Problem:** four small parts.
  (a) The List reads every Event in the namespace. That is deliberate, but the
  comment's reason is wrong for the path it describes: a field index is only
  needed by the cached fallback, and the `apiReader` passes
  `involvedObject.uid`/`name` selectors straight to the apiserver.
  (b) The unreadable-Events marker is never cleared. After the first Event, a
  later real read failure is silent, and the Event itself expires after an hour.
  (c) The new claim-wins guard at `:1462` has no spec. Deleting it leaves the
  suite green, so the ADR-0004 half of the mount decision is untested.
  (d) The PR body says "6 `Warning FailedMount` Events". The file has 3
  (`:21,26,28`). Six is the count of lines containing the string, because the
  `SandboxRetry` and `SandboxFailed` messages quote the kubelet's text.

  **Fix:** F48f for (a), (b) and the wording; add the (c) spec in the same
  change. If the body is still editable, correct (d); otherwise state it here.

  **Acceptance:** F48f is queued with these parts; the (c) spec fails when the
  guard is removed; the round says which parts F48f took.

- [ ] **I131: postmerge-8's five P3s have no home**

  **Where:** `.samples/review-postmerge-8.md` — P3-2 (`:213`), P3-3 (`:231`),
  P3-5 (`:279`), P3-6 (`:291`), P3-7 (`:299`).

  **Problem:** P2-1 and P2-2 were queued as F88 and F89, and P3-1 was folded into
  F88. The other five have no queue entry and no acceptance. R34 I126(a) exists
  to stop exactly that: a merged body's residual sentence is not a tracker.

  **Fix:** route each one. P3-6 and P3-7 are `describe.go` and go to F88 (same
  block, `:86-117`). P3-2 and P3-3 are docs and go to one docs item, with the
  `InvalidImageName` runbook line that #262 did not take. P3-5 is a one-line fix
  to the `Makefile` comment that states the cause the PR's own numbers disprove.

  **Acceptance:** each of the five names a queue item or an explicit accepted
  residual in R35, in a routing table.

- [ ] **I132: process slips — a stale kind image, and an unrelated change riding a fix**

  **Where:** the "Honest note on the first kind attempt" in #266's body;
  `…/scratchpad/handoff-b2-session2.md` ("Gotcha for the next builder"); the
  `Makefile` `-timeout 20m` change that rode #262; postmerge-8 P3-5 (`:279`).

  **Problem:** (a) #266's first kind run used an image whose binary contained
  none of the branch's code. `docker build` reused a stale Go layer. It was
  caught only by extracting `/manager` and grepping for the new strings, after a
  wasted run. (b) #262 carried an out-of-scope `make test -timeout 20m` change.
  The reviewer accepted it on evidence it recorded itself, but the body never
  cites that evidence, and the merged comment still states the cause the PR's
  numbers disprove.

  **Fix:** put the image check in the AGENTS kind-run section, or in a `hack/`
  helper: build with `--no-cache`, grep the image's `/manager` for a string the
  branch adds, and confirm the running pod's image ID. For (b), one line in the
  AGENTS "work a queue" section: an unrelated change rides only with its own
  evidence in the body.

  **Acceptance:** the norm exists and the next kind run shows the check; the
  Makefile comment is fixed, or its item is routed in I131.

---

## Owner decisions

**D86** below is new. D72-D85 remain open (D72-D74 in R29, D75-D84 in R30-R32,
D85 in R34).

**D86: a mount failure is now terminal, and the remedy is delete-and-re-apply.**

  **Where:** `docs/TROUBLESHOOTING.md:518-548` (the new F48b entry);
  `loop_s4_phase.go:1597-1615` (exhausted → `Failed:SandboxFailed`);
  `internal/cli/` (no `retry` command — approval, fork, history, logs, memory,
  pause, plan, redeliver, workspace only); `.samples/design-f48b.md`
  ("Recovery story — flag for the owner"; not committed).

  **Problem:** before #266 a pod that could not mount its volumes stayed
  Pending, visible as `SandboxNotStarting`, and never failed. Now it fails in
  about four minutes with `Failed:SandboxFailed`, which is terminal. There is no
  CLI retry for a Failed Loop. The runbook's remedy is to create the missing
  Secret, delete the Loop and re-apply it, which loses its status and history.
  The queue item asked for the bound; this decision is the cost of it.

  **Decision asked:** (a) accept the terminal failure and the runbook remedy
  (the queue item's choice), or (b) add a retry path for `Failed:SandboxFailed`
  (a `cox retry`, or the `coxswain.io/redeliver` annotation shape) before the
  release. Recommendation: **(a)** for this release, with (b) queued.

  **Acceptance (if (a)):** the runbook entry stays, and `docs/UPGRADE.md` names
  the behaviour change — a mount wedge now ends `Failed` and needs a re-apply.
  **If (b):** a queue item exists with its acceptance, and D86 stays open until
  it lands.
