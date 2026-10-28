# Phase 1 review, round 34: a dead tamper gate, three short surfaces, and the review record

Round 34, 2026-10-25. It follows R33 (PR #246, not yet merged) and covers the
work merged since R33 was opened (#247 to #260). Code line numbers are against
main at `c57a255` (the merge of #259) unless an item says otherwise; I122 cites
`7f8074b` (the merge of #260). The review was built from these input
files, which are not committed:
- a post-merge review of F48 and F83 (`.samples/review-postmerge-7.md`) and of
  F84 as merged (`.samples/postmerge-f84.md`);
- the pre-reviews of PRs #247-#259 (`.samples/prereview-*.md`) and of the open
  PR #260 (`.samples/prereview-260.md`, merged during the round);
- the reviewer's mutation runs for #256, #258 and #259 (`.samples/mut-256-r2.md`,
  `.samples/mut-258.md`, `.samples/mut-259.md`);
- the citation check of the previous draft (`.samples/r34-cite-check.md`);
- the design notes for the owner decision and the queued fixes
  (`.samples/design-d85.md`, `.samples/status-driven-decisions.md`,
  `.samples/design-f84-f85.md`, `.samples/design-f86.md`, `.samples/design-f87.md`).

A bare `loop_*.go` or `loop_*_test.go` name is under `internal/controller/`;
`describe.go` and `commands_test.go` are under `internal/cli/`.

| PR | Item | Merge |
|---|---|---|
| #247 | Docs: TROUBLESHOOTING section for PinNotDescendant (F43) | 3eed62c |
| #248 | F44: surface unmetered requests and stale usage reads | 0f6915a |
| #249 | F45: one effective VAPBinding and a guard that renders at a non-default release | 63679f1 |
| #250 | Docs: TROUBLESHOOTING entry for the F44 metering and budget conditions | 5fbc837 |
| #251 | Docs: PinNotDescendant FAQ entry and glossary term | ced2e2a |
| #252 | F46: wire the model timeout (`spec.loop.modelTimeoutSeconds`) | 96d04e6b |
| #253 | Docs: UPGRADE note for the F45 stale VAP binding | de44eac |
| #254 | F47: validate checkpoint status entries before the GC or a restore uses them | 1cad554 |
| #255 | Docs: TROUBLESHOOTING entry for CheckpointEntryUntrusted (F47) | 67bde03 |
| #256 | F48: surface a sandbox pod that never starts; bound CreateContainerConfigError; validate Secret-name fields | 4451ade |
| #257 | Docs: UPGRADE pointers for the memory-writer image and the RWX storage class | 61d12f1 |
| #258 | F83: record the tamper evidence; the tamper gate acts only on operator-only evidence | 434216f |
| #259 | F84: a proxy pod that is not up yet is no reading (ErrNotReady + start grace), an honest stale message, no-endpoint conditions cleared | c57a255 |
| #260 | F85: cox describe reads the real budget fields, shows the F44 facts and the F43 commit pin | 7f8074b |

**Status:**
- **The tamper gate is dead, and F83 killed it a second way.** Before #258
  nothing wrote `status.verify.tamperExitCode`, so the gate always saw no
  evidence and only a forged status could make it fire (D85). #258 added the
  writer and the verify Job UID binding, and a forged status can no longer fail a
  Loop. But the writer also rewrites the gate's inputs from the same pod in
  exactly the reconciles where the gate has one, so the gate cannot decide
  anything (I119).
- **F83's user-visible win landed**: a delivered PR body now says "Tamper check:
  clean" when the check ran clean, instead of "no tamper evidence".
- **F48's surfaces work and each stops short.** A Pending sandbox pod is visible,
  but the condition can be dropped at `Verifying` and then fire an Event every
  reconcile, it outlives its pod, one grace has no slack and one kubelet reason
  is still unbounded (I120).
- **F84 is merged** (#259, `c57a255`), after a CHANGES round and an OK round. The
  rounds caught three things before merge: a **false evidence claim** (the "no
  pod IP" spec reached the start grace, not the branch it was named for, and
  disabling that branch left the suite green); an **unbounded silent fallback**
  (`ErrNotReady` returned before the age test for the two shapes that never
  resolve, so a proxy that never comes up showed nothing at all — the reason
  `ModelProxyNotReady` exists); and a **write storm** (the failure count inside
  the stale message re-stamped `lastTransitionTime` every pass). I121 is closed;
  what is left is F84b (I127).
- **F85 is merged** (#260, `7f8074b`) after one CHANGES round: the review
  caught the delivery block beside the fix reading `prNumber` as a `float64` and
  the wrong `prUrl` key, the same bug class the PR fixed (I122, closed).
- **The queued follow-ups**: F86 (I123), F87 (I124), F84b (I127).
- **Neither F48's nor F83's new surface reaches the runbook** (I125). F84's did
  land (`docs/TROUBLESHOOTING.md:781-793`), which is the pattern the other two
  need.
- **The review record has four slips** (I126).
- **D85 is this round's owner decision.** F83 turned it into a documentation
  problem as well as a code one: the ADR states an invariant the manifests break,
  while F83's own field doc states the opposite as fact.
- **No P1s this round.** The security property at stake (a forged status must not
  fail a Loop) holds at `c57a255`.
- **Eight issues are open** — I119, I120, I123-I127; I121 is closed by
  #259 and I122 by #260.

---

## P2

- [x] **I119: the tamper gate is dead, and F83 made it unreachable** (`b729cf4` (#264), `347c07c` (#274))

  **Where:** `internal/controller/loop_controller.go:1241-1276`
  (`applyTamperGate`), `:1278-1293` (`tamperPodConfirms`), its call order
  (`:963` then `:1010`), the comments at `:1238-1240`, `:1248-1252`;
  `internal/controller/loop_verify_job.go:1266-1304` (`recordTamperEvidence`,
  called at `:1352`), `:1164-1172` (`verifyOutcome`'s tamper mapping),
  `:1571-1585` (the phase set), `:1323-1326` (the delivery-hold early return),
  `:1500` (the pin clear, which touches nothing in `status.verify`);
  `internal/controller/loop_s4_phase.go:569-586` (the per-pin reset);
  `loop_checkpoint_envtest_test.go:564-572`; `api/v1alpha1/loop_types.go:297-301`
  (`jobName`); `internal/controller/loop_deliver_description.go:378-385`;
  `loop_f83_tamper_evidence_envtest_test.go:186-197`.

  **Problem:** Four parts; (c) and (d) are P3 but ride with F83c because they
  touch the same code.
  (a) **the gate cannot fire.** The writer sets `tamperExitCode`,
  `verifiedCommit` and `tamperJobUID` from the same `job`/`pod` the gate is then
  handed, so the guard at `:1267-1269` reduces to "the tamper init terminated
  non-zero" — which is `verifyTampered`, and that has already set `phase=Failed`
  at `:1576-1585`. Every path that supplies no pair hands the gate `nil, nil`
  and its `job == nil` term bails. The "fail-closed second line" comment is
  false, and the forge spec's "positive control" (`:186-197`) asserts
  `verifyTampered`, not the gate. The unreachability is load-bearing: a gate with
  its own read would make a forged status actionable again in the delivery-hold
  window.
  (b) **A kubelet kill is recorded as tampering.** `verifyOutcome` maps any
  non-zero tamper exit other than the git-error code to `verifyTampered`, so an
  `OOMKilled` init (exit 137) or an evicted pod ends the Loop terminally as "a
  protected path changed", and F83 now records and prints it.
  (c) **The record's lifecycle is incomplete.** The writer allocates
  `status.verify` with no evidence (`loop_verify_job.go:1266-1269`); the record survives the pin clear (`:1500`), so the
  next `Implementing` run renders the previous iteration's tamper code;
  `status.verify.jobName` is a published field nothing writes; and F77 (rollback)
  will clear the pin and leave `tamperJobUID`.
  (d) **Two claims are untrue.** `:1239` names a function that does not exist,
  and `:1248-1252` promises a commit-mismatch guard the writer makes
  unproducible. The delivered-body tamper bullet is now status text and belongs
  in F69.

  **Fix:** F83c: delete the gate and its plumbing and keep the writer and the
  bullet (the unreachability must not be "fixed" by giving the gate its own
  read); map the kubelet kill and an evicted pod to `verifyInfraFailed`; clear
  the three fields with the pin and add `tamperJobUID` to F77; allocate the
  object only when a field is written; decide `jobName`; correct the comments and
  the spec; scope the bullet into F69.

  **Acceptance:** the gate is gone with no spec deleted except the gate's own;
  the forge spec still shows a status-only verdict cannot fail a Loop; the
  `OOMKilled` mapping has a spec and a named mutation; `git grep -n 'fail-closed second line'` returns nothing;
  F77's queue line names `tamperJobUID`.

- [ ] **I120: F48's three sandbox surfaces each stop short**

  **Where:** `internal/controller/loop_s4_phase.go:723` and `:727-737` (the
  `Verifying` return), `:1124-1126` (the early return that skips the clear) and
  `:1147-1150` (the clear), `:1207-1208` (the grace), `:1203-1223` (the
  config-error-only bound), `:695-700` (no call at Paused/Succeeded/Failed),
  `:1096-1105` (`configErrorWaiting`); `docs/UPGRADE.md:263-270`; the F48 queue
  item.

  **Problem:** Five parts; (b), (c) and (e) are P3.

  (a) **The `Verifying` hold drops the write.** `sandboxFailureStep`
  reports that it changed the status, and `:737` returns `false, false, ""`. So
  at `Verifying`, `SandboxNotStarting` is set, its Warning Event is emitted, and
  the write is thrown away unless something else changes the status in the same
  reconcile: one Event per reconcile, unbounded, and the condition never
  persists. The F6 `clearRecoveredCondition` at `:711-713` is dropped the same
  way, although it is deliberately placed before the hold. No spec covers it —
  the F48 specs plant the Loop at `Planning` and call the helper directly.
  (b) **The condition outlives its pod.** The clear lives inside the success
  branch, so a deleted Sandbox or a terminal Loop never revisits it and
  `cox describe` points at a pod that no longer exists.
  (c) **One grace has no slack.** The config-error grace is measured from
  `CreationTimestamp`, so a sandbox pod whose init containers legitimately run
  for a minute is already past it when the error first appears.
  (d) **One kubelet reason is bounded.** A typo'd image gives
  `InvalidImageName`/`ErrImageNeverPull`, which never clears and never fails the
  Loop; the motivating F48 case (a missing `gitCredentialSecret` volume, reported
  as `FailedMount`) is F48b.
  (e) **The docs.** The UPGRADE note (the reviewer's fix `1ea7eab`) says
  ratcheting is why "the operator's own status writes still work". That is the
  wrong reason: a `/status` update validates only the status schema, and
  `PrepareForUpdate` restores the stored spec first
  (`k8s.io/apiextensions-apiserver` v0.37.0, `registry/customresource/status_strategy.go:66-90`,
  `validator.go:103-116`). Status writes are never blocked by a spec value on any
  version; ratcheting matters only for spec and metadata writes (a `kubectl
  edit`, the operator's finalizer removal). The tick must also say "surfacing only; the
  bound is F48b" rather than claim a bound F48 does not have.

  **Fix:** F48d for (a) and (d) (return `changed`, bound `InvalidImageName`);
  F48c with F48b for (b); record the config error's first sighting and measure
  from it for (c); rewrite the UPGRADE paragraph with the two paths separated;
  write the tick against `main` at merge time, naming F48b.

  **Acceptance:** a Loop at `Verifying` with a Pending pod past the grace,
  reconciled twice, has one Event and the condition persisted after the first
  pass (named mutation: restore `false`; the spec fails); a spec clears the
  condition when the Sandbox is deleted; a just-appeared error on an old pod
  decides nothing; `InvalidImageName` is bounded; the UPGRADE note names the spec
  path and the status path separately.

- [x] **I121: `BudgetReadingStale` counted reconciles — closed by #259 (`c57a255`)**

  **What it was:** a proxy pod that was absent or had no pod IP counted as a
  failed usage read, so an ordinary pod start raised a stale alarm.

  **What the review rounds caught before merge:** (a) a false evidence claim:
  the "no pod IP" spec's fixture set `PodIP = "127.0.0.1"`
  (`loop_p2d_budget_test.go:206`), so it passed through the start grace, and
  disabling the `PodIP == ""` branch left the suite green; fixed with a table
  spec, one row per `ErrNotReady` shape. (b) An unbounded silent fallback
  (R26 I90): the two shapes that never resolve showed nothing at all; fixed by
  surfacing the D35a hold as `ModelProxyNotReady` with one Event and a runbook
  entry (`docs/TROUBLESHOOTING.md:781-793`). The agent cannot spend meanwhile:
  its only model egress is this Loop's proxy pod. (c) A write storm: the
  failure count in the condition message re-stamped `lastTransitionTime` every
  pass; the count now lives in the Event only. The reviewer's mutations A and B
  fail the new specs (`.samples/mut-259.md`).

  **Residual:** F84b (I127).

- [x] **I122: `cox describe` read fields that do not resolve — closed by #260 (`7f8074b`)**

  **What it was:** the budget block read `status.budget.tokens`, which is not in
  the API, and showed neither F44 field; the delivery block read `prNumber` as a
  `float64` (it is `int64`) and the key `prUrl` (the tag is `prURL`), so neither
  the `pr:` nor the `url:` line printed for a real Loop, and the spec's `"PR #1"`
  assertion matched the condition message instead.

  **What landed:** `internal/cli/describe.go:86-117` reads the real budget
  fields; `:128` reads `prNumber` through `numberField` and `:131` reads
  `prURL`; the spec asserts both rendered delivery lines
  (`internal/cli/commands_test.go:77`) and a phantom budget key
  (`TestDescribe_BogusBudgetKeyPrintsNoTokens`). The review round caught the
  delivery block; the reviewer's two mutations (`.(float64)` restored, `prUrl`
  restored) each fail the new assertions (`.samples/mut-260.md`).
  `docs/TROUBLESHOOTING.md` now describes the one `PinNotDescendant` Event as
  one.

- [ ] **I123: F47 closed the status vector only; the fork/restore-spec vector is still open**

  **Where:** `internal/controller/loop_restore.go:245-247` (the fork target
  builds `checkpointName(… ForkOf.Loop, ForkOf.Iteration)` with no
  `checkpointEntryTrusted`), `:912` (`resolveCheckpointSource` with another
  Loop's name), `:890-894` and `:1171-1197` (`removeForkRetentionLabel` gets and
  patches the source's snapshot); the hardened status path it bypasses is
  `:281-309`; `loop_checkpoint.go:170-186` (`checkpointEntryTrusted`).

  **Problem:** an entry named in **spec** is trusted. The ownership check F47
  added applies to `status.checkpoints` only, so a fork can read, and write the
  retention label on, another Loop's snapshot. Plus the F47 P3s:
  `IsNoMatchError` → `Gone` is shared by call sites with opposite safe
  directions, `checkpointUnknown` is silent, and the GC delete has no UID
  precondition.

  **Fix:** F86 (queued, design `.samples/design-f86.md`).

  **Acceptance:** a spec for a cross-Loop fork target (it must not take another
  Loop's snapshot), the three P3s each with a spec or an accepted residual, and a
  decision recorded for the fork-ownership rule.

- [x] **I124: F87 — three guards that do not run, and one status consumer F47 left behind** (`058529c` (#284), `defc770` (#285), `98db918` (#286), `23c0d23` (#289))

  **Where:** `hack/chart-check.sh` (not wired into `make test`; `Makefile:74` is
  the `test` target, `:100` the separate `chart-check` target),
  `dist/chart/templates/NOTES.txt` (approval RBAC, which disagrees with
  `dist/chart/README.md:42-44,51-53,72-74`),
  `internal/controller/loop_checkpoint.go:502-506` (the
  `cp.Iteration == iteration` early return).

  **Problem:** the F45 chart guard never runs in CI; the chart's post-install
  notes still teach the approval RBAC that #249 corrected in the README — the two
  shipped surfaces disagree, so an operator following NOTES.txt gets a different
  story from one following the README; a forged `status.checkpoints` entry at the
  current iteration suppresses the real checkpoint, so a later lost-PVC restore
  has nothing trustworthy (F47's property holds, the checkpointing it protects
  does not); and the CLI item paired with F47 is still open.

  **Fix:** F87 (queued, design `.samples/design-f87.md`).

  **Acceptance:** `make test` fails when the F45 guard's invariant is broken;
  `NOTES.txt` corrected; `ensureCheckpoint` trusts only an entry
  `checkpointEntryTrusted` accepts, with a spec for a forged entry.

---

## P3

- [ ] **I125: neither F48's nor F83's surface reaches the runbook**

  **Where:** `docs/TROUBLESHOOTING.md` (per-symptom sections for F36 (`:562`),
  F38 (`:536`), F39 (`:580`), F42 (`:482`), F43 (`:599`), F44 (`:658`), F47
  (`:795`) — none for F48 or F83); `docs/OPERATOR.md:437-444` (the conditions
  table).

  **Problem:** (a) `git grep SandboxNotStarting -- docs/ dist/` returns nothing,
  so the condition tells the user to read pod events and the runbook never
  mentions it. The two `AgentFailed` faces of a never-started container ("has not
  started (waiting: …)") have no entry, and the conditions table has no row for
  any of `SandboxNotStarting` (F48), `MeteringDegraded`/`BudgetReadingStale`
  (F44) or `ModelProxyNotReady` (F84).
  (b) The delivered PR body's tamper bullets are undocumented — what "clean"
  means, and why a missing record prints "no tamper evidence (fail-closed
  reading)" rather than "clean".

  **Fix:** one section for `SandboxNotStarting`, one for the config-error
  `AgentFailed` variant, one paragraph on the tamper bullets, and one
  conditions-table row. F84's `ModelProxyNotReady` entry
  (`docs/TROUBLESHOOTING.md:781-793`) is the model to follow.

  **Acceptance:** every string above is greppable in `docs/`; no code change.

- [ ] **I126: process slips**

  **Where:** `.samples/review-postmerge-6.md` (six residual items);
  PR #258's body; `/home/jamie/projects/coxswain-f43ts/.samples/`;
  `gh pr list` (eleven review/doc PRs open).

  **Problem:** (a) postmerge-6's six residuals are still unqueued and untracked —
  a merged PR body's "not hardened" sentence is not a tracking system.
  (b) #258 was resumed by a different model mid-issue, and neither its body nor
  the queue says so, so the reviewer's "check this one extra carefully" control
  cannot be audited.
  (c) Evidence was written into a worktree's `.samples` for the third time; it is
  gitignored, invisible to the reviewer, and deleted with the worktree (the R28
  I92 rule inverted).
  (d) Twelve review/doc PRs are open, so R30-R33's ticks are not on `main` and
  R34 lands behind an unmerged R33.
  (e) **The reviewer's own misses.** The reviewer posted `Verdict: OK + notes` on
  #258 without seeing that the gate it kept is unreachable; post-merge review 7
  found it (I119). On #259 the reviewer's first M1 mutation removed the
  `errors.Is` call, which left the `errors` import unused and failed the build;
  it was re-run with the expression intact before being cited. A reviewer
  mutation that fails to build is not evidence.

  **Fix:** (a) queue or accept each residual, as postmerge-7's were routed this
  round (I119-I125, D85) and postmerge-f84's were routed to F84b; (b) name the
  model and session in a resumed PR's body; (c) move the stray file into the main
  `.samples/` and add the check to the worktree cleanup step; (d) merge the
  review stack oldest first, or have R34's intro name the dependency; (e) the
  reviewer reads the post-merge path of any gate a PR keeps, and records a
  mutation only after it builds.

  **Acceptance:** no item closes by silence; the next resumed PR's body names the
  switch; `find ~/projects -maxdepth 2 -name .samples -type d` returns only
  `coxswain/.samples`; the round record states the open review/doc count.

- [x] **I127 (F84b): what F84 left behind — one hold at a time, no terminal clear, and five stale sentences** (`74d4a03` (#265), `01b7b28` (#277))

  **Where:** `docs/TROUBLESHOOTING.md:712-715` (the "surfaced as
  `ModelProxyNotReady`" sentence), `:755-759` ("a restarted or crashed proxy pod
  is the usual cause" for `BudgetReadingStale`);
  `internal/controller/loop_controller.go:2024-2037` (`modelProxyHold` — it
  runs after the restore, approval and D30 gates, so only the first hold is named),
  `:2042-2045` (`modelProxyNotReady` says "it does not exist yet" for **any** read
  error), `:2061` and `:2064` (`surfaceModelProxyNotReady`'s `pod` parameter,
  `_ = pod`), `:2072-2078` (the only clear path; there is no terminal-phase
  clear), `:541-542` (the `readProxyUsage` seam doc); `loop_p2d_budget.go:79-82`
  (the threshold comment); `docs/RELEASE-NOTES-next.md:230` (branch
  `docs/release-notes-next`: "cleared on recovery"); `.samples/postmerge-f84.md`.

  **Problem:** six small residues, one of them a behaviour gap:
  (a) **one hold is named at a time.** `sandboxSuspendedByGate` returns at the
  first hold that fires, and `modelProxyHold` is behind restore, approval and
  D30, so a Loop held by an earlier gate does not show `ModelProxyNotReady` yet —
  the runbook sentence promises it unconditionally.
  (b) **the condition has no terminal clear.** The only clears are
  `clearModelProxyNotReady` (`:2074-2076`) and the no-endpoint block
  (`loop_p2d_budget.go:210-213`); a Loop that reaches `Failed` while its proxy is
  not Ready keeps `ModelProxyNotReady=True` describing a hold that no longer
  means anything.
  (c) **any read error says "it does not exist yet"** (`:2044-2045`), including an
  apiserver error, which sends the operator to look for a missing pod that may be
  there.
  (d) **the threshold comment still argues from the old shape**
  (`loop_p2d_budget.go:79-82`, "a single miss (a pod mid-restart)" — a pod
  mid-restart is `ErrNotReady` and never counts now).
  (e) **the seam doc is stale since F44/F84** (`:541-542`, "an error means NO
  reading … status.budget is left unchanged" — a counted failure increments the
  counter and may set the condition).
  (f) **two runbook/release-notes sentences still describe the old model**
  (`docs/TROUBLESHOOTING.md:755-759`; `docs/RELEASE-NOTES-next.md:230`).

  **Fix:** (b) clear the condition when the Loop reaches a terminal phase, with a
  spec and a named mutation (drop the clear; the spec fails); the rest are text:
  add "once the earlier holds (restore, approval, D30) clear" to `:712-715`, say
  "could not be read" for a non-`NotFound` error and keep "it does not exist yet"
  for `NotFound`, drop `surfaceModelProxyNotReady`'s unused `pod` parameter, and
  rewrite (d), (e) and (f) as in `.samples/postmerge-f84.md` P3-1 to P3-4.

  **Acceptance:** a Failed Loop whose proxy is not Ready ends with
  `ModelProxyNotReady=False`; the message distinguishes a `NotFound` from a read
  error; `surfaceModelProxyNotReady` takes no `pod`; the three sentences match the
  merged code.

---

## Owner decisions

**D85** below is new. D72-D84 remain open (D72-D74 in R29, D75-D84 in R30-R32;
those PRs are still open, I126(d)).

- [ ] **D85: the Loop editor role can write Loop status, which makes every status-driven decision forgeable**

  **Where:** `config/rbac/loop_editor_role.yaml:31-43` (`loops/status`
  `get;update;patch`, added for `cox approve`/`reject` in #148) against
  `docs/adr/0005-operator-verifies-via-isolated-job.md:170-177` ("only the manager
  role may write `loops/status` … **Keep it that way**"). The 17 status reads are
  listed in `.samples/status-driven-decisions.md`; the options are in
  `.samples/design-d85.md`.

  **Problem:** an editor-bound user can forge every status input of a terminal
  decision: the tamper gate (F83 narrowed this one), the budget caps, the stall
  history, the iteration cap, the two retry bounds, the verify pin, the stale-Job
  deletions, and the phase reads (`status.phase` → sandbox suspension,
  `Delivered` → Succeeded, `status.pausedFrom`). The repo asserts the invariant
  twice, in opposite directions, both as fact: the ADR says the grant is a break,
  while the manifests grant it and F83's field doc
  (`api/v1alpha1/loop_types.go:278-287`, the phrase "user-writable" at `:316`)
  says so as the premise of its hardening. Two forgeable inputs a reader still
  acts on after F83: `lastCheckResults` → the checkpoint's `Milestone` flag
  (`loop_checkpoint.go:1113-1130`) and `lastFailureSummary` → the next planning
  prompt's feedback file (`loop_approval.go:231-244`).

  **Decision asked:** (a) a ValidatingAdmissionPolicy allowing a non-manager
  status write to change only `PlanApproved`/`PlanRejected`; (b) move
  approve/reject to a one-shot annotation and drop the editor's status write;
  (c) keep the grant and fix the sites one by one. Recommendation: **(b)**, with
  (a) as later defence-in-depth.

  **Acceptance (if (b)):** the editor role grants `loops/status` `get` only (both
  manifests) with a test that fails on a widened rule; `cox approve`/`reject`
  write the annotation and the operator turns it into the condition and clears it
  after the status write, with the "a refused action does not clear it" spec; an
  UPGRADE note; a kind run showing `auth can-i update loops/status` = no, a
  forged status patch = 403, and zero `forbidden` in the operator log. ADR-0005
  §I29 gets a dated amendment naming the break, the F83 mitigation and the two
  residuals.
