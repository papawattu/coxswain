# Release notes: changes since v0.1.2 (unreleased)

For an operator upgrading from v0.1.2. All items are verified against the
merged code on `main` (as of `a0f7fd6`); each claim cites its merged PR and
the squash commit it landed as.

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

### F42: a Loop with no endpoint got no NetworkPolicy at all — #213

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

### Docs — #208, #209, #224, #225, #227, #228, #230

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

## Known issues still open

The eight P1 fixes above (F37, F39, F40, F42, F38, F35, F36, F55) are all
merged, and F31 (no-pod memory-writer Job) and F54 (the evil-agent e2e)
were closed by #229 and #222. In plain words, the items still open:

**P1 (security):**

- **F58**: the verify step, which runs the agent's own code, has no resource
  limits.
- **F60**: there is no "canary" control check that is required to fail — a
  passing canary would mean the checks were tampered with, but nothing
  detects that.
- **F63**: blocked agent actions are not recorded in the audit log.

**P2:**

- **F32**: saved lessons have no count or size cap; oversized memory silently
  disappears from the agent's context.
- **F43**: the commit the system verifies and delivers is agent-chosen, with
  no check that it descends from the base commit.
- **F44**: a broken or stale usage reading silently switches off the token
  and cost caps (surface only; whether to hold the Loop is an open owner
  decision).
- **F47**: "editor"-level users can write status fields the operator later
  uses to delete or restore checkpoints.
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

- **F33/F34**: audit-record inconsistencies and stale-file leftovers (audit
  hash/file-name mismatch, duplicate-lesson counts, deny-all NetworkPolicy
  created after the Job; stale lessons file surviving restore).
- **F50/F45/F46/F57/F52**: small and low items — shipped samples pointing at
  repo contents they test, an inert second admission-policy binding, the
  runner model-timeout never wired, a checkpoint-recovery edge case, and a
  bundle of misc items (a failed delivery counted "in flight" forever, the
  CLI approval command writing status with no conflict retry, and more).
- **F59**: a code comment claims a "suspicious pattern scan" exists; it
  doesn't.
- **F61**: sample agent-policy files are not shipped.

**Investigation open:**

- **F41**: the memory-writer's curator call fails on one lab cluster and
  works on another; the cause is unknown and the investigation is open.

**Waiting on owner decisions (not builder tasks):**

- a precondition before the operator mounts a named Secret and presents it
  to a host the same author names (confused deputy);
- whether a stale usage reading should hold the Loop at its cost cap (F44);
- narrowing editor write access to the two approval conditions only (F47);
- whether to reject or document as unsupported the `spec.workspace.repo`
  ssh/scp forms the delivery path can never serve.

No P1 fix outstanding from the merged batch remains open; the three P1
security items above (F58, F60, F63) are the only P1 work queued.
