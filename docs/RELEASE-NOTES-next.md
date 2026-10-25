# Release notes: changes since v0.1.2 (unreleased)

For an operator upgrading from v0.1.2. All items are verified against the
merged code on `main` (as of `1cad554` = PR #254); each claim cites its
merged PR and the squash commit it landed as.

## Upgrade

Must-do items for an operator upgrading from v0.1.2; each points to
`docs/UPGRADE.md` (section "Upgrading from v0.1.2").

- If your RBAC is managed outside `install.yaml`, apply the new
  `serviceaccounts` (create, get, list, watch) rule to the operator's
  ClusterRole (F29, #199). A standard install picks it up automatically.
- Set `--memory-writer-image` to the operator image if your install is not
  `config/default` (PR #182); set `--memory-storage-class` to a
  ReadWriteMany-capable StorageClass if your cluster's default does not
  support ReadWriteMany (PR #191).

Nothing else to do: `spec.workspace.restoreFrom` is now immutable (F35,
#215); the agent NetworkPolicy is always created (F42, #213); the sandbox
waits for the egress proxy for the init clone (F55, #220); the memory
lesson cap is 100 per PVC (F32, #233); the memory-writer audit records the
trimmed lesson text (F33, #234); a stale lessons file no longer
survives into a later iteration (F34, #238); a pinned commit that does not
descend from the base is refused with a new terminal `PinNotDescendant`
reason (F43, #244); and an unmetered request or a stale usage reading is
surfaced, not skipped silently (F44, #248).

## Security fixes

### F37: `shellQuote` escaped an apostrophe wrongly (command injection) — #207

- **What was wrong.** `shellQuote` wrote an apostrophe `'` as three
  apostrophes. One apostrophe left the generated script string open (a syntax
  error); two closed the quote and ran the text between them as shell (merged
  `7dcf262` = PR #207). It was wrong since S3.
- **Who could exploit it, and how.** Call sites hold the git credential
  (deliver Job: `PR_TITLE` from `spec.goal`, plus REPO, BRANCH, BASE), run
  acceptance checks (verify Job) and write agent-influenced plan feedback
  (phase-init). A Loop goal like `Fix the user's login` broke delivery, and a
  goal carrying two apostrophes (e.g. `'; touch PWNED; '`) ran the text
  between them as shell inside the Job (PR #207).
- **What changed.** An apostrophe is now written in the POSIX close-quote,
  backslash-apostrophe, reopen-quote form; one line (merged at
  `7dcf262:internal/controller/loop_controller.go:5647`). The `shellQuote`
  call sites all inherit the fix. Measured on `origin/main` (git grep counting
  lines): 8 in `loop_verify_job.go`, 11 in `loop_deliver_job.go`, 8 in
  `loop_controller.go` (one of them the definition at `:5647`), so 26 call
  lines, matching PR #207's body.
- **On upgrade.** Nothing required. The fix is in the operator; new Jobs are
  generated with the corrected quoting. No kind run was needed because the
  change is script text, covered by execution tests (PR #207).

### F39: tamper check passed all globs as one pathspec and failed open — #210

- **What was wrong.** The tamper check joined all protected-path globs into
  one newline-separated `git diff` pathspec, which git does not split. With 2
  or more globs — including the default `go` preset — nothing matched and the
  gate printed "tamper: clean". A failing `git diff` also read as clean
  (merged `dc628a5` = PR #210, reproduced with real git).
- **Who could exploit it, and how.** A sandbox agent that edits a protected
  path (for example weakens a test file) during implementing passes the
  tamper gate as long as the Loop has 2+ protected-path globs, so a tampered
  verify/deliver goes on unchallenged (merged `dc628a5` = PR #210); the kind
  run in #210 shows an agent appending `// weakened` to `round_test.go` and
  the fix catching it with `reason: TamperedVerify` (PR #210).
- **What changed.** One `shellQuote`'d pathspec argument per glob; no pipe —
  the `git diff` status is checked and a failure exits 2 (fail closed); zero
  globs skip the diff and print `tamper: clean (no protected paths)` (PR
  #210).
- **On upgrade.** Nothing required; the fix is in the operator (PR #210).

### F40: egress proxy followed upstream redirects to unchecked addresses (SSRF) — #212

- **What was wrong.** `handlePlainHTTP` in the egress proxy used an
  `http.Client` with no `CheckRedirect`, so an allowlisted upstream could 302
  the proxy to an address that was never checked against the allowlist or the
  carve-outs (for example 169.254.169.254), and the transport dialed it
  (merged `c9a53f8` = PR #212).
- **Who could exploit it, and how.** A sandbox agent that talks to an
  allowlisted upstream can have that upstream redirect the proxy to a
  non-allowlisted or carved-out address, so the allowlist and the SSRF
  carve-outs are bypassed (merged `c9a53f8` = PR #212).
- **What changed.** `CheckRedirect` returns `http.ErrUseLastResponse`: the
  3xx is passed back to the agent, which re-requests through the proxy and is
  checked like any other request (PR #212).
- **On upgrade.** Nothing required; the fix is in the operator/egress-proxy
  image (PR #212).

### F42: a Loop with no endpoint and no network allows got no NetworkPolicy at all — #213

- **What was wrong.** When `spec.agent.endpointSecretRef` was empty and the
  effective policy had no network allows, the operator created no agent
  NetworkPolicy. The agent netpol carries the ingress deny-all and DNS-only
  egress, so such a Loop's agent ran with open ingress and egress (the
  shipped sample `loop-fix-failing-test.yaml` is this shape). An absent agent
  netpol also did not hold the sandbox Suspended (merged `b90d1db` = PR #213).
- **Who could exploit it, and how.** Any user who could create a Loop with no
  endpoint could run an agent with unrestricted ingress and egress, including
  the cloud metadata address (the kind run showed an unlabelled control pod
  reaching `http://1.1.1.1/` while the labelled one could not; PR #213).
- **What changed.** The agent netpol is always ensured (only the model-proxy
  egress rule is gated on the endpoint); a new `agentNetPolMissing` gate term
  holds the sandbox Suspended while the agent netpol is absent or unreadable
  (`b90d1db:internal/controller/loop_controller.go:4662`); the netpol is
  ensured before the sandbox and the gate releases once the netpol is
  visible. Note: on an upgrade of a pre-F42 Loop, the gate reads the netpol
  through the manager's cache, so the netpol can be missing from the cache
  for a reconcile after it is created — closed by F55 (PR #220), which
  confirms a cached "absent" with the uncached apiReader; a failed ensure
  sets `AgentNetworkPolicyReady=False/NetworkPolicyEnsureFailed`, emits a
  Warning Event, and the condition is persisted before the requeue (merged
  `b90d1db` = PR #213).
- **On upgrade.** Nothing required; the fix is in the operator (PR #213).

### F38: a re-plan after verify-failure could skip the approval gate — #214

- **What was wrong.** With manual approval and `verify.onVerifyFail: Plan`,
  the B4 re-plan (Verifying → Planning) left the stale `PlanApproved`
  condition in place, and the `PlanChanged` clear ran after
  `recordPhaseClaim` — whose approval gate read the stale hash and allowed
  Planning → Implementing. An unapproved re-plan was implemented and
  delivered (merged `daf4cd6` = PR #214). A rejection while Planning had the
  same stale-approval problem and did not bump `planAttempt`.
- **Who could exploit it, and how.** A user with manual approval enabled
  could get an unapproved re-plan implemented and delivered: the gate let the
  re-plan advance to Implementing without a fresh approval (PR #214).
- **What changed.** The `OnVerifyFailPlan` transition now clears
  `PlanApproved` (as the rejection path does); the `PlanChanged` clear now
  runs before `recordPhaseClaim`, and the re-hold is surfaced as a
  `PlanChangedReHold` Event; a Planning-phase rejection clears
  `PlanApproved` and bumps `planAttempt` (PR #214).
- **On upgrade.** Nothing required; the fix is in the operator (PR #214).

### F35: the rollback one-shot re-ran on any spec edit — #215

- **What was wrong.** The rollback one-shot marker was keyed on
  `metadata.generation`, so any spec edit re-armed a completed restore. In
  particular `cox pause` patches `spec.suspend`, so pausing a rolled-back
  Loop deleted the workspace PVC and restored the old snapshot over the work
  since (merged `628fb1a` = PR #215). `restoreFrom` was documented immutable
  but had no CRD rule.
- **Who could exploit it, and how.** A user who paused a Loop after a
  rollback (or edited any other spec field) silently lost all work done since
  the snapshot: the workspace was rolled back again (merged `628fb1a` =
  PR #215).
- **What changed.** The one-shot is now keyed on the restore target (the
  spec), not the generation: the `Restored` condition's message records the
  spec target (e.g. `[spec-target=rollback:iter-3]` or `fork:<loop>/<n>`) and
  the marker is complete only when that key matches the current spec, so spec
  edits (including `cox pause`) no longer re-arm it; a genuinely different
  target does. A marker written before this change counts as complete.
  `restoreFrom` now has a real immutability rule (a CEL `self == oldSelf`
  transition rule; changing it is rejected with `restoreFrom is immutable`)
  (PR #215).
- **On upgrade.** Nothing required; the fix is in the operator and the CRD
  (PR #215).

### F36: the fork-retention label write was Forbidden, and restore progress was wrong — #216

- **What was wrong.** The fork-retention label (which stops the source's GC
  from deleting a snapshot a fork is restoring from) was written and removed
  with `Update`, but the operator's RBAC grants only `patch` on
  volumesnapshots, so the label write was Forbidden on every real cluster and
  only logged at V(1) — and the source's GC could delete a snapshot a fork was
  still restoring from. A rollback whose old PVC was still Terminating was
  reported `Restored` instead of requeued, and a fork whose own PVC was
  already present was not deleted and recreated from the snapshot (merged
  `6eb74c2` = PR #216).
- **Who could exploit it, and how.** Any user forking a Loop: the source
  snapshot's GC could delete it mid-restore, and a fork/rollback restore could
  report success while silently leaving the wrong (or no) workspace contents
  (merged `6eb74c2` = PR #216).
- **What changed.** The label is written and removed with `Patch` (a failure
  is a Warning Event, `ForkRetentionLabelFailed` /
  `ForkRetentionLabelRemoveFailed`); it is written before the fork PVC create
  and removed if the restore does not complete; an AlreadyExists on a
  Terminating PVC sets `DeletingPVC` and requeues instead of `Restored`; the
  fork/lost-PVC branch deletes a present PVC so the restore completes from the
  snapshot (PR #216).
- **On upgrade.** Nothing required; the fix is in the operator (PR #216).

### F55: the sandbox could start before the egress proxy the init clone depends on was ready — #220

- **What was wrong.** A regression in F42's shape: for an external repo with
  no network allows (the shape of the shipped sample
  `config/samples/loop-fix-failing-test.yaml`), the first clone traverses the
  egress proxy, but the sandbox gate did not wait for it — the sandbox pod
  could start before the proxy pod was Ready, and the init clone got
  connection refused (merged `74c56f3` = PR #220). Separately, F42's
  `agentNetPolMissing` gate read the netpol through the manager's cache: on
  an upgrade of a pre-F42 Loop the freshly created netpol could be missing
  from the cache for a reconcile, falsely holding (or releasing) the sandbox
  (PR #220).
- **Who could exploit it, and how.** A Loop with an external repo and no
  network allows could start its first clone before the egress proxy was
  Ready, so the clone failed (connection refused) and the Loop stalled in the
  workspace-init step (merged `74c56f3` = PR #220).
- **What changed.** The sandbox gate now also waits for the egress proxy when
  the init clone must traverse it — gated on the proxy **creation** predicate
  (`workspaceInitProxyCreated`, shared with `ensureEgressProxy`, so the gate
  cannot wait for a proxy that is never created, e.g. a repo URL whose host
  parses empty) rather than the intent predicate (merged `74c56f3` = PR
  #220). The `agentNetPolMissing` gate now confirms a cached "absent" with
  the uncached apiReader before holding (the cache may say "present" but not
  "absent"), and the upgrade path emits one Normal `AgentEgressFenced` Event
  when the netpol is created for a Loop whose sandbox already exists (merged
  `74c56f3` = PR #220).
- **On upgrade.** Nothing required; the fix is in the operator (merged
  `74c56f3` = PR #220). The F42 upgrade-fence note above (stale-cache gate
  read) is closed by this fix.

## Other changes

### F47: checkpoint status entries are validated before the GC deletes or restore uses them — #254

- **What was wrong.** `status.checkpoints` is user-writable (the editor role, since B3), and the operator acted on it without trusting it: the retention GC would delete any VolumeSnapshot a status entry named, even one belonging to another Loop, and the lost-PVC restore could take a forged entry as its restore source (PR #254, review finding review-api P2-1).
- **What changed.** `checkpointEntryTrusted` (in `loop_checkpoint.go`) is the single trust gate: a `status.checkpoints` entry may be acted on only if its name is exactly `checkpoint-<this loop>-iter-<n>` (n >= 0), `n == entry.Iteration`, and the VolumeSnapshot of that name carries this Loop's ownership label. The retention GC **never deletes** a snapshot for a Foreign (unowned or misnamed) entry — it only drops the untrusted entry from status and emits a Warning Event `CheckpointEntryUntrusted`, and only when the GC loop runs (i.e. when the keep-set does not already cover the whole list); a forged entry can stay in status but is never acted on. The lost-PVC restore walks from the newest entry and takes the last Trusted one; if entries exist but none is trusted it takes the visible `NoUsableCheckpoint` fallback (condition + Event), not a silent empty workspace. No RBAC change; the safety property is "never deleted / never restored from", not "always dropped from status" (merged `1cad554` = PR #254).
- **Scope note.** F47 closes the *status* vector only: the fork path still derives its source snapshot from `spec.forkOf` and PATCHes a `fork-of` label onto it with no ownership check, so "an editor cannot read another Loop's checkpoint" is not a property this PR establishes; the `ensureCheckpoint` idempotency guard and the tamper gate's status inputs are also not yet hardened this way (PR #254).
- **On upgrade.** Nothing required; the fix is in the operator (PR #254).

### F46: the model timeout is now wired (`spec.loop.modelTimeoutSeconds`) — #252

- **What was wrong.** The runner's model timeout was documented but never set: the entrypoint left `runConfig.ModelTimeout` at its 120s default, so a slow self-hosted model had no knob to raise it (the docs said so — see the #208 and #232 docs entries above) (PR #252).
- **What changed.** New `spec.loop.modelTimeoutSeconds` (int32, 1–3600, validated by the CRD): the operator sets `COX_MODEL_TIMEOUT=<n>s` on the agent container (only when a model endpoint exists and the field is set), and the runner entrypoint parses it (Go duration or bare seconds; absent/invalid/<=0 keeps 120s). The Sandbox container list is rewritten on every reconcile, so editing the field on a live Loop changes the timeout without a restart. The worst-case figure in the runner's comment is corrected: 4 attempts + 2+4+8s backoff = 4 × timeout + 14s (merged `96d04e6` = PR #252).
- **On upgrade.** Nothing required; the fix is in the operator. To use the knob, set `spec.loop.modelTimeoutSeconds` on the Loop (CRD field; also in the regenerated chart CRD) — e.g. `modelTimeoutSeconds: 300` raises the per-request model timeout from 120s to 300s (PR #252).

### F45: the chart no longer renders a second (inert) VAP binding — #249

- **What was wrong.** Helm installs from v0.1.0–v0.1.2 rendered **two**
  `ValidatingAdmissionPolicyBinding` objects for the one ephemeral-container
  `ValidatingAdmissionPolicy`: the literal
  `coxswain-deny-ephemeral-containers` (correct), plus a templated one named
  after the release's fullname, whose `policyName` named the templated name
  and therefore no existing VAP — the second was **inert** (it bound nothing;
  the gate still worked). At the default release name `coxswain` the two
  templates collide on one object, so it was invisible there; at any other
  release name the stale inert binding was left behind (merged `63679f1` =
  PR #249).
- **What changed.** The templated second binding is removed from the chart, so
  a render produces exactly one effective VAP binding; the guard in
  `hack/chart-check.sh` now renders a second time at a non-default release
  name/namespace and fails if a binding names a policy that is not a VAP in
  that render, or if the render is not exactly one VAP binding
  (merged `63679f1` = PR #249).
- **On upgrade.** At the default release name `coxswain`: nothing to do. At
  any other release name: check for and delete the stale binding per the
  `docs/UPGRADE.md` "The chart no longer renders a second VAP binding" entry
  (it is inert, so deleting it cannot weaken the ephemeral-container gate)
  (PR #249).

### F44: unmetered requests and stale usage readings are surfaced — #248

- **What was wrong.** Two of the budget's inputs could silently stop
  working: (a) a model request the proxy forwarded without a parseable
  usage object was simply not counted, so the token and cost caps ran on
  partial data with no signal; and (b) a usage read that failed (a proxy
  pod mid-restart, a scrape error) was silently ignored — the caps kept
  evaluating the last good reading and nothing recorded how stale it was
  (merged `0f6915a` = PR #248).
- **What changed.** The proxy now counts requests it forwarded without a
  usage object and reports them per read; the operator surfaces both cases
  with conditions and transition-gated Warning Events, and new status
  fields make the staleness inspectable: `MeteringDegraded` (reason
  `UnmeteredRequests`) goes True while a read reports new unmetered
  requests and carries the running total, and the new
  `status.budget.unmeteredRequests` accumulates it; `BudgetReadingStale`
  (reason `UsageReadFailed`) goes True after 3 consecutive failed usage
  reads and the new `status.budget.lastUsageReadAt` /
  `status.budget.consecutiveReadFailures` fields record the last good
  read and the failure run. Both are cleared when the next good read
  arrives, and both carry one Warning Event per episode, not per
  reconcile. Surfacing only: on a stale read the caps stay on the last
  good reading (fail-open) — whether to hold the Loop instead is the open
  owner decision (merged `0f6915a` = PR #248). A new metric,
  `coxswain_unmetered_requests_total`, records the operator-side total.
- **On upgrade.** Nothing required; the fix is in the operator (PR #248).

### F33: memory-writer audit now hashes the trimmed lesson, counts batch duplicates correctly, and creates the deny-all NetworkPolicy before the Job — #234

- **What was wrong.** Three small defects in the memory-writer Job: the audit record's hash was over the raw lesson text, but the file was named by the trimmed text, so a lesson with leading or trailing whitespace had a hash that did not match its name (P3-1); two identical lessons in one batch produced per-lesson lines whose count did not agree with the batch total (P3-2); and the deny-all NetworkPolicy for the Job pod was created after the Job, so the pod's first seconds ran without the fence (P3-6) (PR #234).
- **What changed.** The audit hash is now over the trimmed text; duplicate lessons are counted once per occurrence in the summary; the NetworkPolicy is created before the Job (merged `069a655` = PR #234).
- **On upgrade.** Nothing required; the fix is in the operator (PR #234).

### F43: the verify Job refuses a pinned commit that does not descend from the base — #244

- **What was wrong.** The commit the system verifies and delivers was
  agent-chosen, with no check that it descends from the base commit (the
  finding's P1-1, downgraded to P2 by the reviewer before the fix): the pin
  was shape-checked only (a 40-hex SHA), so a runner (or a corrupted
  `result.json`) could pin any commit reachable in the agent's repo,
  including pre-existing history that predates the Loop — and the tamper
  diff, which is tree-to-tree, cannot catch it (an unchanged pre-existing
  tree reads as clean) (merged `80dac60` = PR #244).
- **What changed.** The verify Job's `import-agent` init now runs
  `git merge-base --is-ancestor <baseCommit> <pin>` before the checkout:
  exit 1 ("not an ancestor") exits with a distinct code that the operator
  maps to a terminal `Failed` reason `PinNotDescendant` (a Warning Event
  naming both commits, a completion metric, no retry — a re-run would
  import the same pin); any other git failure stays an infra failure
  (`VerifyInfraFailed`, retried). A pin equal to the base is allowed ("the
  agent changed nothing") and the checks then decide. In the runner,
  `priorCompletedResult` re-emits a stored Implementing success only if its
  `headCommit` is a commit in the workspace repo — a defence in depth; the
  PR states the trusted gate is the verify Job's ancestry check (a
  per-container-run stamp was shown to be forgeable: the agent shell runs
  as the same user in the same container) (merged `80dac60` = PR #244).
  Also in #244: a blocked Planning no longer truncates `PLAN.md` (the phase
  hook runs only when the model call produced an answer), stale comments
  that claimed a `summary` field / `result.json` reader were corrected (no
  behaviour change), and `commitWorkspace` now returns a reason on each git
  failure instead of an empty string. Lab run: the new import-agent script
  ran in a real verify pod (pass path; the pin equaled the base), and the
  refusal path is covered by the spec that executes the real generated
  script (PR #244).
- **On upgrade.** Nothing required; the fix is in the operator (PR #244).

### F78: the memory-writer audit keeps its counts line, and the 1000-byte cut is line-oriented — #243

- **What was wrong.** The audit text cut at 1000 bytes on a byte/rune
  boundary, and the counts line was last, so a long write lost
  `summary: total=...` (the line `cox describe` and the verdicts metric
  parse) to a mid-line fragment; the `ensureMemoryWriterJobAndNetPolicy`
  doc and the netpol-before-Job comment described the writer's egress fence
  oppositely ("deny-all" / "no network calls" instead of the actual proxy +
  DNS allowlist); and four spec names no longer described what they assert
  (merged `bd4c457` = PR #243).
- **What changed.** `summaryLines` emits the counts line first, then the
  audit lines, then the per-lesson lines; `memoryWriteAuditText` is
  line-oriented — whole lines up to 1000 bytes, then ` … N more lines`,
  never a mid-line fragment; `memoryWriteVerdicts` reads the counts line by
  content, so the metric is unchanged; the fence comments now say proxy +
  DNS egress only; the misnamed specs were renamed (merged `bd4c457` = PR
  #243).
- **On upgrade.** Nothing required; the fix is in the operator (PR #243).

### F34: a stale lessons.json no longer survives into a later iteration, and the restore hold covers StoppingWorkloads — #238

- **What was wrong.** A `lessons.json` left in the workspace from a previous iteration survived into the next one, so a check that found no lessons to propose still saw the old file (P3-3); and the restore hold checked only two of the three workload-stopping conditions the memory-writer Job documents, so a Loop could start a memory write while a restore was in progress (P3-4) (PR #238).
- **What changed.** The stale `lessons.json` is removed when nothing is proposed; the restore hold now also covers `StoppingWorkloads` (merged `5e82c96` = PR #238).
- **On upgrade.** Nothing required; the fix is in the operator (PR #238).

### F31: a no-pod memory-writer Job is decided from its own conditions — #229

- **What was wrong.** `recordMemoryWriterOutcome` requeued whenever the
  Job had no pod, before reading the Job's own conditions. A Job that is
  Failed or Complete with no pod (pod GC, eviction, admission reject)
  requeued every 5 s forever, never set `MemoryWriteDone`, and `cox memory
  delete` refused forever (PR #229).
- **What changed.** With no pod, the outcome is now decided from the Job's
  conditions: Failed sets `MemoryWriteFailed` naming the Job's reason
  (Warning Event, failed metric once); Complete sets `MemoryWritten` with
  no summary; neither means still in progress (requeue) (merged `aedc33d`
  = PR #229). This closes the no-pod half of the F31 item (see Known
  issues: F73 covers a Job that failed while its pod is still stuck).
- **On upgrade.** Nothing required; the fix is in the operator (PR #229).

### F54: the "evil agent" end-to-end test now exists — #222

A test addition, not a product change: the agent must be stopped from
reading its service-account token, reaching the API server or model proxy
directly, reaching the cloud metadata address, pushing code with a
forged credential, and tampering with the verify step (merged `eb6ce98`
= PR #222). It runs two Loop variants (the real runner with a hostile
model stand-in, and the F42 no-endpoint shape) plus a tamper Loop, with
gate conditions and Events added on 2026-10-24. Verified on kind (Calico
+ KubeArmor): the pre-fix build FAILED (variant b escaped seven probes
and the tamper Loop), the fixed build `evil-agent-e2e: OK`, and the
five named mutations (M1–M5) each failed the run (PR #222). Steps the
e2e could not yet prove (probe 6's direct leg — kind has no metadata
service — and a few assertions) are queued as F72 (see Known issues).
No operator or image changed, so no upgrade action (PR #222).

### F30: memory-write metrics — #206

New Prometheus metrics, registered in the operator and documented with PromQL
in `docs/OPERATOR.md` on `main` (merged `dcd79da` = PR #206):

- `coxswain_memory_writes_total{outcome=written|failed}`: one per Loop, on
  the transition that sets `MemoryWriteDone`; a re-reconcile does not bump.
  Counted only after the status update persists, so a conflicting
  status-update no longer double-counts (the kind run found and fixed this;
  PR #206).
- `coxswain_memory_lessons_total{verdict=kept|static-drop|curator-drop}`:
  with a `written` outcome only. `kept` and the drop totals come from the
  summary counts line; per-lesson lines only split the drops
  (`curator: ...` / `curator unavailable` are curator-drop; the rest are
  static-drop).
- Per-cluster kind results: on kind-ref the curator call worked and `kept`
  was observed; on k3d-hermes-sim (k3s) the curator call failed for every
  lesson (`curator unavailable (call failed)`, cause unknown — open
  investigation), so there `curator-drop` dominated and
  `kept`/`static-drop` were not observed. All four outcomes are covered by
  the envtest specs (merged `dcd79da` = PR #206).

### Docs — #208, #209, #224, #225, #227, #228, #230, #231, #232, #236, #237, #239, #240, #247, #250, #251, #253

- #208 (merged `2a3208f`): user docs — corrected a non-existent failure name
  in `docs/QUICKSTART.md` (the deliver credential is only read when
  `spec.workspace.gitCredentialSecret` is set), the `cox logs` flag form
  (`--component`, not a positional), the model-timeout worst case (4 calls of
  120s plus 14s of backoff, about 8 minutes), that `ModelTimeout` is not
  configurable as shipped, a non-existent `make kind-load` target, the
  metrics port (default disabled; manifests bind `:8443` HTTPS), a
  non-existent `--namespace` flag, and the finalizer name
  (`coxswain.wattu.com/checkpoints`). Stale line-number citations in
  `docs/UPGRADE.md` were replaced with function names. Two code gaps were
  flagged instead of hidden: `ModelTimeout` is documented but never wired,
  and the retry worst-case comment disagrees with the code (PR #208).
- #209 (merged `9178e51`): samples, examples, chart docs — the `git-cred`
  "prerequisite" in `config/samples/loop-fix-failing-test.yaml` is only
  relevant when delivery is enabled and a secret is named; the verify image
  `golang:1.26` lacks the `python3` the smoke script uses (now named in a
  comment); the gocli/pylib/webapi example snippets now use the real seeded
  values (`samples/<app>.git`, `samples-git-cred`) instead of the
  non-existent `gitea-clone`; the chart README documents the other
  user-facing value groups, and `rbac.roleNamespaces` and
  `webhook.enabled`/`webhook.port` are documented as dead (read by no
  template). No field value, template, or code changed (PR #209).
- #224 (merged `f3929d8`): a new `docs/TROUBLESHOOTING.md` section covering
  the gate conditions and Events added with this batch of fixes.
- #225 (merged `2f95627`): a new `docs/OPERATOR.md` section on what the
  sandbox can and cannot reach.
- #227 (merged `17a5587`): readability pass on `docs/TROUBLESHOOTING.md`
  and `docs/OPERATOR.md` — mostly rewording for a first-time reader. Two
  substantive changes: TROUBLESHOOTING now says A1–A6 are the alpha
  plan's work items (A6 the acceptance run), and the "Loops stuck in
  Implementing" PromQL example was removed from OPERATOR because its
  `timestamp()` wrap returns the scrape time, not the pod age (PR #227).
- #228 (merged `a32bfc5`): a new `docs/GLOSSARY.md` — a plain-language
  glossary of the terms an operator meets in the docs, each entry linked
  to the section that defines it (PR #228).
- #230 (merged `a0f7fd6`): readability pass on `docs/QUICKSTART.md`
  prose for a first-time reader; touches no commands or code blocks.
  The command defects in §9 it found are queued separately as F71 (see
  Known issues) (PR #230).
- #231 (merged `5454541`): a new `docs/OPERATOR-FAQ.md` — plain-language
  answers to the questions an operator asks first (PR #231).
- #232 (merged `8533a89`): a new `docs/EGRESS-LOG.md` explaining how to read
  the egress proxy's audit log, and a correction to the model-timeout
  worst-case figure in `docs/OPERATOR.md` (PR #232).
- #236 (merged `a6d73c5`): a new `docs/INDEX.md` — a table of contents for
  the docs, each entry linked to the section (PR #236).
- #237 (merged `f9569dc`): a new `docs/UPGRADE.md` section covering the
  changes in this release since v0.1.2 (PR #237).
- #239 (merged `acb40b0`): a new `docs/EVIL-AGENT-E2E.md` — how to run the
  evil-agent e2e, what it proves, and what is still UNPROVEN (F72) (PR
  #239).
- #240 (merged `b850406`): a new `docs/MEMORY-WRITE.md` — how to read the
  memory-write outcome (the `MemoryWritten` / `MemoryWriteFailed` conditions
  and the curator verdicts) (PR #240).
- #242 (merged `23124e7`): cross-doc consistency fixes — the operator FAQ's
  `cox logs` flag form and the hold list (now including
  `NetworkEnforced=False`), the glossary's real `cox` subcommand list (from
  `internal/cli/cli.go`: no `get`/`apply`; adds `diff`/`memory`) plus the
  100-lesson cap and the reader caps, a runnable `cox memory export` line in
  QUICKSTART, "sidecar" reworded to the separate proxy pods in
  TROUBLESHOOTING, and UPGRADE: the F55 entry's condition worded precisely,
  a code line number dropped, the cap wording narrowed, and a
  `MemoryPVCUnbound` entry (that Event isn't in v0.1.2). Docs only (PR #242).
- #245 (merged `4d7b5d2`): docs plus two comment-only code changes —
  TROUBLESHOOTING names the `MemoryWriteFailed` Warning Event's reason and
  says the audit text is now cut on whole lines with "… N more lines" (F78,
  #243), GLOSSARY's "Stall gate" excludes verify infrastructure failures,
  EVIL-AGENT-E2E's probe count now matches the harness `expect` lists, and
  two stale comments that said lessons are written to
  `/memory/lesson-<n>.txt` were corrected to `<sha256>.txt`
  (`cmd/memory-writer/main.go`, `internal/controller/loop_memory_writer_job.go`).
  Comments only; no behaviour change (PR #245).

### F41: the curator call is retried on a transport failure — #241

- **What was wrong.** On the k3d-hermes-sim lab cluster the memory writer's
  curator call failed for every lesson ("curator unavailable (call failed)")
  while the same writer worked on kind-ref. The mechanism was never shown
  (the failure is a start-up transport error that clears by itself,
  bracketed at (0s, 10s] — a writer started immediately against the same
  proxy fails, one delayed 10s succeeds), so the fix makes the writer
  resilient to a transient transport failure instead of failing closed on
  the first one (merged `4b71ff4` = PR #241).
- **What changed.** `pkg/curator` retries a transport failure (no HTTP
  response) up to 3 times on a 1s/2s/4s backoff (7s for one lesson); the
  retry wait is bounded per batch — one Client (one writer batch, up to 20
  lessons) shares a 20s retry budget, so a down proxy costs at most 20s
  instead of 20 × 7s, and once spent a failed call is dropped at once
  ("call failed", fail-closed). An HTTP status or a bad body is never
  retried; each retry logs `retry n/N in <delay>: <err>` to the Job log
  (merged `4b71ff4` = PR #241). One post-fix lab run: `written=2 dropped=1`
  on the cluster that previously failed every call (PR #241).
- **On upgrade.** Nothing required; the fix is in the operator (the curator
  client ships in the operator image, which is the default memory-writer
  image; set `--memory-writer-image` to the new image if your install is not
  `config/default`) (PR #241).

## Known issues still open

The eight P1 fixes above (F37, F39, F40, F42, F38, F35, F36, F55) are all
merged, and F31 (no-pod memory-writer Job), F54 (the evil-agent e2e), F32
(D71 memory lesson cap), F33 (memory-writer audit), F34 (stale
lessons.json), F78 (memory-writer audit counts line), F41 (the unknown
curator failure), F43 (the agent-chosen pin), F44 (surfacing unmetered
requests and stale usage reads), F47 (untrusted checkpoint status entries),
and F46 (the unwired model timeout) were closed by #229, #222, #233, #234,
#238, #243, #241, #244, #248, #254, and #252. In plain words, the items still
open:

**P1 (security):**

- **F58**: the verify step, which runs the agent's own code, has no resource
  limits.
- **F60**: there is no "canary" control check that is required to fail — a
  passing canary would mean the checks were tampered with, but nothing
  detects that.
- **F63**: blocked agent actions are not recorded in the audit log.

**P2:**

- **F82**: F44 follow-ups — two unspecced lines and three stale stream
  comments (PR #248's run).
- **F47 follow-ups** (F47 itself closed by #254): the fork path still
  derives its source snapshot from `spec.forkOf` with no ownership check,
  and the `ensureCheckpoint` idempotency guard and the tamper gate's
  status inputs are not yet hardened this way (PR #254's scope note).
- **F48**: a sandbox pod that cannot start wedges the Loop with no condition
  or timeout.
- **F49**: the "hold the sandbox while awaiting approval" rule and a
  no-pin delivery branch have no automated tests.
- **F51**: the network-fence check reads an unresolvable address as
  "reached"; a missing checker leaves the gate open.
- **F53**: the verify step's code folder is writable by the agent's own code
  between checks, and the verify step has no time limit.
- **F56**: a standard deployment ships no runner image and unpullable proxy
  images, so the agent quietly runs "do nothing".
- **F62**: the "agent action blocked" counter and alert are never written,
  so blocks are invisible.
- **F64**: the port-enforcement guarantee has no automated proof (the specs
  asserting a blocked `host:PORT` is actually blocked were never written).
- **F65**: runtime-pulled images are tag-pinned only; the image that mounts
  the git credential should be pinned by digest.
- **F66**: the verify and deliver Jobs leave the service-account token
  mounted in containers that run the agent's own code.
- **F67**: the "import the agent's commit" step lets git read the agent's own
  repo config, so an agent-chosen program can run inside the trusted import
  step — in the deliver case, the same pod as the git credential.
- **F68**: `spec.workspace.ref` and `spec.verify.acceptanceChecks` are
  unvalidated, so a crafted value can reach the credentialed scripts.
- **F69**: the delivered PR body's facts section carries the agent's own
  claim text presented as an operator fact, with no cap.
- **F70**: a forked Loop with an external repo can wait for an egress proxy
  it never uses; on an install where the proxy image can't be pulled it
  hangs with no condition (PR #222's run).
- **F71**: the from-source build commands in `docs/QUICKSTART.md` §9 are
  wrong (a duplicated `PROXY_IMG`, `build-installer` given only the
  controller tag, and an uncreated `/tmp/d61-loop.yaml`) (PR #230's run).
- **F72**: some of the evil-agent e2e's steps are still UNPROVEN (the
  verify-checkout assertions, the probe 15 control Loop, exec-path
  equivalence; probe 6's direct leg is unproven because kind has no
  metadata service) (PR #222).
- **F73**: a memory-writer Job that failed while its pod is stuck can
  retry forever (the pod-present half of the closed F31 item; PR #229).

**P3 (and P3 bundle):**

- **F74-F77**: four restore/fork edge cases left after F35 and F36 — an
  empty-PVC fallback that leaves `baseCommit` set (F74, waits on owner
  D84), a pre-F35 `Restored` marker with no target key that counts as
  complete for any target (F75), a failed fork-retention label SET that is
  not retried although the doc says it is (F76), and a rollback whose
  post-state is undefined (F77) (PR #238's run).
- **F80**: the memory writer's summary field is not size-bounded by the
  operator: `MemoryWritten.summary` carries the audit text (which can
  include a whole lesson line quoted in a curator reason), and Kubernetes
  truncates Event message strings at 4096 bytes without marking the cut, so
  a summary longer than that silently loses its tail (the operator-side
  1000-byte audit cut, F33/F78, bounds the audit text only — the condition
  message can exceed it when a quoted lesson line is long).
- **F81**: an envtest spec is flaky: a hard-coded ClusterIP it depends on is
  not a stable address across apiservers, so the run intermittently fails.

- **F50/F57/F52**: small and low items — shipped samples pointing at repo
  contents they test, a checkpoint-recovery edge case, and a bundle of misc
  items (a failed delivery counted "in flight" forever, the CLI approval
  command writing status with no conflict retry, and more). (F45 closed by
  #249; F46 closed by #252.)
- **F59**: a code comment claims a "suspicious pattern scan" exists; it
  doesn't.
- **F61**: sample agent-policy files are not shipped.
- **F86**: F47 follow-ups — `ensureCheckpoint`'s idempotency guard
  (`cp.Iteration == iteration`) and the tamper gate's status inputs are not
  yet validated like `checkpointEntryTrusted` (PR #254's scope note).

- **F83**: delivered PR bodies say "no tamper evidence" even when the
  tamper check passed, because the tamper exit code is never recorded
  (tamper failures are still caught and still fail the Loop).
- **F84**: the `BudgetReadingStale` condition can raise a false alarm
  while the model proxy pod is still starting up.
- **F85**: `cox describe`'s budget block misses the new usage-read fields
  (`lastUsageReadAt`, `consecutiveReadFailures`).

**Waiting on owner decisions (not builder tasks):**

- a precondition before the operator mounts a named Secret and presents it
  to a host the same author names (confused deputy);
- whether a stale usage reading should hold the Loop at its cost cap (F44);
- whether to enable the operator NetworkPolicy by default (F45 P3-1);
- narrowing editor write access to the two approval conditions only (F47
  option (a); #254 implemented option (b), validate before acting — the
  editor role is unchanged);
- whether to reject or document as unsupported the `spec.workspace.repo`
  ssh/scp forms the delivery path can never serve.

No P1 fix outstanding from the merged batch remains open; the three P1
security items above (F58, F60, F63) are the only P1 work queued.

### Docs — the six additions after #240

- #247 (merged `3eed62c`): a new `TROUBLESHOOTING.md` section for
  `PinNotDescendant` (F43, #244) — what you see (the terminal Failed
  reason, the Warning Event naming both commits, the import-agent init
  container exiting 97), why the verify Job refuses the pin (the runner's
  `headCommit` claim is not operator evidence; the ancestry check runs
  `git merge-base --is-ancestor` before any checkout), what to do (compare
  `status.baseCommit` and `status.currentVerify.verifiedCommit`, then
  restart the Loop or roll back to a pre-divergence checkpoint — it is
  terminal, no retry), and how it differs from its neighbours
  (`TamperedVerify`, `VerifyInfraFailed`, `Stalled`) (PR #247).
- #250 (merged `5fbc837`): a new `TROUBLESHOOTING.md` entry for F44 — what
  the `MeteringDegraded` and `BudgetReadingStale` conditions and their
  Warning Events (`UnmeteredRequests` / `UsageReadFailed`) mean, what to
  check (the `status.budget` fields, the model proxy pod's port 9090,
  whether `unmeteredRequests` is still growing), and why the caps stay on
  the last good reading (fail-open; D77 hold-vs-surface is still open)
  (PR #250).
- #251 (merged `ced2e2a`): the operator FAQ gains the question "Why did my
  Loop fail with PinNotDescendant?" (as question 8, with the later
  questions renumbered) and the glossary gains a `PinNotDescendant` entry;
  both link to the #247 TROUBLESHOOTING section (PR #251).
- #253 (merged `de44eac`): the `docs/UPGRADE.md` entry for the F45 stale VAP
  binding — what the stale binding is, why the default release name is
  unaffected, and the check-and-delete steps for any other release name
  (PR #253).
- #255: a new `TROUBLESHOOTING.md` entry for the F47 Warning Event
  `CheckpointEntryUntrusted` (#254, merged `1cad554`) — what makes a
  `status.checkpoints` entry trusted (the exact name shape, a matching
  iteration, and this Loop's ownership label), that an untrusted entry is
  never deleted and never restored from, and how the restore falls back
  visibly. (Merged after the notes' "as of" line; included here so the docs
  list is not left short one entry.)
