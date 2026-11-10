# Release notes: v0.2.0

Entries are added per merged PR, each citing the PR and the squash commit it
landed as, like `RELEASE-NOTES-v0.1.3.md`.

## Summary

- **ClusterAgentPolicy** (P6a): a cluster-scoped `AgentPolicy` companion a `spec.policyRefs` entry can name, so an org-wide allow is one object referenced by Loops in any namespace (namespaced policy wins on a name collision, with a `PolicyNameCollision` Warning Event).
- **Learn mode** (P6b): `spec.auditOnly: true` runs a referenced policy in audit-only posture so the Loop records activity instead of enforcing it; **`cox policy suggest`** (P6b-2) prints a candidate `AgentPolicy` from that recorded activity.
- **The learn summary** (D119): the operator keeps the distinct recorded allows of a learn-mode Loop in a bounded `<loop>-learn-summary` ConfigMap that `cox policy suggest` reads by default.
- **The status VAP** (P6c): a ValidatingAdmissionPolicy that rejects every `loops/status` write except the operator's own ServiceAccount.
- **`cox retry`** (D86, D116): a one-shot annotation that resumes a Loop failed with `SandboxFailed` from its checkpoint without losing its history.
- **Node placement** (C2): `--loop-node-selector` and `--loop-tolerations` operator flags pin every operator-owned Loop workload to a dedicated node pool.
- **The advisory Judge** (ADR-0011): opt-in via `spec.verify.judge`, a second model surface that asks — after the acceptance checks pass — whether the change meets the goal without gaming the acceptance. A false verdict makes delivery a draft; a true or Unknown verdict leaves `spec.delivery.draft` exactly as it is.
- **Multi-arch images** (C1): linux/amd64 + linux/arm64 release images — pending the release workflow push.

## Upgrade

### Known defect in v0.1.3: PR delivery fails on kind (F67) — fixed by #468

- v0.1.3's deliver Job refused its workspace with git "detected dubious ownership", so `spec.delivery` Loops did not open a PR (cause: F67's git calls lacked `safe.directory`; fixed by F95, PR #468).
- PR #469 (F96): the existing-PR lookup now matches the Loop's own branch, so two Loops no longer report the same PR.
- Users of v0.1.3 with `spec.delivery` should upgrade to v0.2.0; there is no v0.1.4 (owner decision D128 = (a)).

### D84b (F77): a completed rollback re-verifies at the restored iteration — #400

**What was wrong:** the phase and iteration survived a rollback: a terminal
Loop restored its tree and nothing ran, and a Loop at maxIterations failed at
once.

**What changed:** when a rollback (`spec.restoreFrom: iter-N`) completes, the
operator sets `status.iteration = N`, `phase = Verifying` and
`desiredPhase = Verifying`, and clears the `Failed` and `Succeeded`
conditions. The restored tree is re-verified, then follows the normal flow
(pass → Succeeded/deliver; fail → iterate). A lost-PVC restore or a fork seed
does NOT reset (spec (f)). The verify pin was already cleared on rollback (the
F77 pin half). Owner decisions: D84(a) and D106.

**On upgrade:** a rollback on a Failed or Succeeded Loop now runs a verify
again.

### The status VAP (P6c, #416): non-manager `loops/status` writes are now rejected by admission

Install from this release's `install.yaml` or chart and any tooling that patched `loops/status` directly (a hand `kubectl patch` on the status subresource, even by a cluster admin) is now denied at admission — including the editor role's old status-write grants. Approve or reject with `cox approve` / `cox reject` (one-shot annotation; the operator writes the status itself). Break-glass hand-edits: delete the binding, edit, re-apply it (`docs/OPERATOR.md`).

### Learn mode: the operator creates `<loop>-learn-summary` ConfigMaps in Loop namespaces

For a learn-mode Loop (every referenced policy `auditOnly`), the operator keeps the `<loop>-learn-summary` ConfigMap in the Loop's namespace (D119). No new RBAC: configmaps `create;get;update` are already in the operator's role. A pre-existing ConfigMap of that name that the Loop does not own is never touched (`LearnSummaryNameTaken` Warning Event).

### D121 (#445): `kubectl cox` returns the kubeconfig error instead of running without a client

When the Kubernetes client setup fails (a missing or broken kubeconfig), `kubectl cox` commands now exit with that kubeconfig error from the CLI pre-run, instead of running without a client and failing later at the first API call.

## New features

### The advisory Judge (ADR-0011) — #455, #459, #460, #461, #462, #463, #465, #467

**What it is:** a second, advisory model surface (after the Memory Curator). After the acceptance checks pass, the operator creates a Judge Job — a separate Job owned by the Loop, with a proxy+DNS-only NetworkPolicy (not a sidecar in the agent's pod) — that asks the model whether the change meets the goal without gaming the acceptance. The Judge gets the goal, PLAN.md, the diff `baseCommit..verifiedCommit` (capped at 256 KiB, marked `truncated: true` when cut), and each acceptance check's command as the user wrote it, with its exit code and an explicit `(passed|failed)` label. It runs through the Loop's model proxy with its own token cap (`spec.verify.judge.maxTokens`), metered like every other Loop token. The verdict is `{verdict, score 0-100, reason (at most 200 chars)}`, recorded in `status.judge`, a `JudgeVerdict` condition (`True`/`False`/`Unknown`), a history note, and an Event. The operator never interprets prose: it strictly validates the fixed shape (ADR-0002's boundary, applied to the second model surface).

**What the verdict does (advisory, D122):** a **false** verdict makes delivery a draft PR whatever `spec.delivery.draft` says (on Gitea — which has no draft PRs — the draft is the `"WIP: "` title prefix; `internal/controller/loop_deliver_job.go:167-170`), and the verdict is shown in the PR description. It **never changes `Succeeded`** and **never iterates**. A **true** or **Unknown** verdict leaves `spec.delivery.draft` exactly as it is. An **unavailable** Judge (error, timeout, malformed reply) is `Unknown`, reason `JudgeUnavailable`, a Warning Event, and the Loop proceeds — never read as a pass or a fail.

**How to use it:** set `spec.verify.judge.enabled: true` and `spec.verify.judge.maxTokens` (required when enabled; `{enabled: true}` without `maxTokens` is rejected at admission) on the Loop. A Loop without `spec.verify.judge` is unchanged: no Judge Job, no `JudgeVerdict` condition, no model call, no change to the phase machine or the PR draft computation.

**On upgrade:** no change for existing Loops. The field is new and unset means no Judge.

### P6b: learn mode — auditOnly policies — #405

**What it is:** `spec.auditOnly: true` on an `AgentPolicy` or a
`ClusterAgentPolicy` (the same spec) is learn mode; the field is never
defaulted. Learn mode applies only when **every** policy the Loop references
is `auditOnly`: the Loop's pod-level KubeArmorPolicy for the agent renders
with `spec.action: Audit` (KubeArmor records, never blocks), the model and
egress proxies' policies stay `Block`, the Loop shows `PolicyEnforced=False`
with reason `AuditOnly`, and the D30 gate is unchanged — a learn-mode Loop
still needs a working engine (KubeArmor installed and the node enforcing),
otherwise the usual hold applies (e.g. `NodeNotEnforcing`). If an enforcing
policy is also referenced, the Loop enforces the union of every policy's
allows (`spec.action: Block`); the `auditOnly` flags have no effect,
`status.policy.auditOnlyIgnored` lists the ignored policies, and one
`AuditOnlyIgnored` Warning Event fires each time that list changes.

**How to use it:** set `auditOnly: true` on every policy a Loop references
(via `spec.policyRefs`), watch the recorded activity (the KubeArmor audit
stream) while the Loop runs, then remove `auditOnly` — or reference an
enforcing policy — to turn the learning into enforcement.

**On upgrade:** no change for existing policies. The field is new and unset
means enforcing, as before.

### P6b-2: cox policy suggest — #406

**What it is:** `kubectl cox policy suggest <loop>` reads the Loop's learn
summary — the `<loop>-learn-summary` ConfigMap the operator keeps for a
learn-mode Loop (D119 = (b)) — and prints a candidate `AgentPolicy` built
from what the agent actually used in learn mode. It warns on stderr when the
summary's `dropped` count is above 0. With `-f <file|->` it reads the
operator's Q4 audit lines from a log instead (e.g. `kubectl logs
-n coxswain-system deploy/coxswain-controller-manager | kubectl cox policy
suggest <loop> -f -`). `--name` defaults to `<loop>-suggested`; the namespace
is the CLI's `--namespace`. Lines that are not audit records, and other Loops'
records, are skipped.

**How to use it:** run the Loop in learn mode, run `cox policy suggest` once
it has the learn summary (the `-f <file|->` form works on a captured log
too), review the printed candidate, apply it (still in learn mode to
confirm), then drop `auditOnly` to enforce. The candidate holds
only the allows the agent used — deduplicated and sorted — and never a deny:
KubeArmor process records with an absolute path become `exec`, KubeArmor file
records become `files`, egress-proxy connects (`host:port`) become `network`,
and KubeArmor network records are skipped (the egress proxy's record carries
the `host:port` an `AgentPolicy` needs). The candidate has no `auditOnly`; a
header comment lists the platform minimum the operator adds itself (the
model-proxy and egress-proxy Service FQDNs, DNS, the agent entrypoint), which
is not written as allows (D109).

**On upgrade:** the audit record `detail` for KubeArmor lines now starts with
`operation=…` (`operation=Process|File|Network` first, before the rest of the
detail). Any parser that matched the old `detail` text exactly must allow the
new prefix.

### P6b-4: learn mode records exec, file and egress — #407

**What was wrong:** a kind-ref run of a learn-mode Loop got 0 records. The
agent's policy is a DNS/network fence (D46), so the learn-mode
KubeArmorPolicy had no process/file rules, and `Audit` alone recorded
nothing.

**What changed:** in learn mode (every referenced policy `auditOnly`) the
agent's KubeArmorPolicy carries catch-all **Audit** rules for process and
file — every directory, recursive — so KubeArmor is asked to record every
exec and file access. The egress proxy runs in learn mode even when no policy has network
allows: its allowlist is then empty, so it blocks every host and records each
connect, and `cox policy suggest` turns those connects into network allows.
Enforcing mode is unchanged: no catch-all rules, and no egress proxy without
network allows.

**Known gap:** KubeArmor must run with
`logPath=stdout` (QUICKSTART Step 3) for the relay to see any alert, and
#408 makes the relay parse those lines. On kind-ref, KubeArmor reported no
events from the agent's sandbox pod in one run (cause under investigation),
so `cox policy suggest` may still come back empty.

**On upgrade:** a learn-mode Loop now also gets an egress proxy pod; network
for the agent in learn mode goes through it and is blocked unless allowed, as
before.

### D113: a files allow ending in / grants a directory recursively — #410

**What changed:** an AgentPolicy `files` entry ending in `/` renders as KubeArmor `matchDirectories {dir, recursive: true}`; other entries stay exact paths. KubeArmor itself requires a directory to end in `/`.

**On upgrade:** nothing; existing entries without a trailing `/` are unchanged.

### P6d: `cox retry` for a Loop that failed with SandboxFailed (D86, D116) — #418

**What was wrong:** `SandboxFailed` was terminal; the remedy was delete-and-re-apply, which loses history.

**What changed:** `kubectl cox retry <loop>` sets the one-shot `coxswain.io/retry` annotation. The operator resumes the phase the Loop failed in, from its current checkpoint, resets the sandbox-attempt count, deletes the failed Sandbox, and emits a Normal `Retried` Event. It applies only to `SandboxFailed`; every other Failed reason is refused (the CLI names it; the operator ignores and removes an annotation it did not accept).

**On upgrade:** nothing; a new CLI command and annotation.

### D112: cox policy suggest condenses to the agent's paths and directories — #411

**What was wrong:** a kind-ref learn-mode run printed 1242 exec and 6260 files entries. Each exec entry was a full command line, which the AgentPolicy CRD rejects (absolute paths only, at most 64 entries).

**What changed:** `cox policy suggest` now counts only the agent sandbox pod's records (init containers still count), keeps only the executable's absolute path for `exec`, collapses directories with more than 5 distinct file paths into one recursive `dir/` entry, and caps each list at 64 most-used entries with the rest printed as a comment.

**On upgrade:** a KubeArmor audit line's `detail` now has `pod=<pod>` after `operation=…`.

### D119: the learn summary — `<loop>-learn-summary` ConfigMap (D119 = (b)) — #435, #439

**What it is:** for a learn-mode Loop, the operator keeps a bounded summary of the distinct allows the Loop's agent recorded (sandbox process/file, egress connects) in the ConfigMap `<loop>-learn-summary`, in the Loop's namespace. Data keys: `records.jsonl` (one record per distinct allow, each with its `count`) and `dropped` (a count, monotonically non-decreasing). It is owned by the Loop (controller owner reference) and is garbage-collected with it. The operator needs no new RBAC: configmaps `create;get;update` are already in the operator's role.

**Bound:** before the size check, the file records are compacted the way `suggest` compacts them: per-run atomic-writer paths become their directory, and a busy directory tree (more than 5 immediate children) becomes one `dir/` entry, with the counts summed. If `records.jsonl` is still over 900 KiB, records are evicted by class: file records first (lowest count first), then process records, and egress connects last. Each evicted record is added to `dropped`. Evidence: kind-ref run 14, 13007 relayed records → 359 summary records, dropped 0; the ConfigMap candidate's exec/files/network sets equal the full-log candidate's.

**Events:** a write error emits a Warning Event `LearnSummaryWriteFailed` on the Loop. If a ConfigMap of the name exists that the Loop does not own, the operator leaves it alone and emits a Warning Event `LearnSummaryNameTaken` on the Loop instead of overwriting it.

**How to use it:** `kubectl cox policy suggest <loop>` reads it by default (P6b-2 above); the operator log is still available via `-f <file|->`. `kubectl get cm <loop>-learn-summary -n <ns>` shows the object.

**On upgrade:** no migration; existing learn-mode Loops get their summary on their next recorded record. A ConfigMap named `<loop>-learn-summary` that the Loop does not own is never touched (see `LearnSummaryNameTaken`).

## Security fixes

### P6c: only the operator writes Loop status (D85(a), D114) — #416

**What was wrong:** D85 made the operator the sole writer of Loop status at the CLI level (approve/reject now annotate the Loop, not the status subresource), but the admission chain had no hard boundary: a user with the editor role's old grants, or a cluster admin, could still `kubectl patch` `loops/status` directly and move a Loop's phase or approval state by hand.

**What changed:** a new cluster-scoped ValidatingAdmissionPolicy (`coxswain-deny-loop-status-writes`) plus binding denies every `loops/status` write except the operator's own ServiceAccount — including cluster admins. Approve or reject with `cox approve` / `cox reject` (which write a one-shot annotation on the Loop); the operator consumes the annotation and writes the status itself. Break-glass hand-edits are documented in `docs/OPERATOR.md` (delete the binding, edit, re-apply it).

**On upgrade:** install.yaml and the chart ship the new VAP and binding; any tooling that patched Loop status directly is now denied.

### I224: AgentPolicy files entries must be canonical absolute paths — #417
- **What was wrong:** `spec.files` (AgentPolicy and ClusterAgentPolicy) had no validation. It accepted relative paths and `..`, `//` or `.` segments, and a files entry is a grant.
- **What changed:** the CRD rejects such an entry at admission, with the message `files entries must be absolute canonical paths …`, and limits the list to 64 entries of at most 512 characters each. A trailing `/` is still allowed: it means the directory, recursively (D113).
- **On upgrade:** a policy already stored with a bad entry is NOT re-checked until it is next edited. Whether a later edit that leaves that entry unchanged is accepted depends on the cluster's CRD validation ratcheting; fix the entry when you next edit the policy. List policies to check with `kubectl get agentpolicies,clusteragentpolicies -A -o yaml | grep -A3 'files:'`.

### D78(b): a credential key followed by ':' and any value is dropped from lessons — #401

**What was wrong:** the credential-assignment rule only dropped a credential key (`password|passwd|secret|token|api_key`) when followed by `:` and an 8+ character token-shaped value, so short values such as `password: hunter2` were written to memory.

**What changed:** a credential key followed by `:` and any non-space value is now dropped, whatever its length. The accepted cost is that prose such as `the token: parser lives in …` is dropped too.

**On upgrade:** existing lessons are re-screened against the current rules on the first memory write after the upgrade and any that fail are removed (see D73(b)); a short colon credential stored before the upgrade is removed at that point.

### D73(b): the memory writer re-screens existing lessons on every write — #401

**What was wrong:** lessons that failed the current static rules (for example D78(b)'s credential rule or D68's path-or-error rule) could stay on the memory PVC forever: a lesson was only screened when it was proposed.

**What changed:** on every memory write, the writer re-screens the existing `<sha256>.txt` lessons against the CURRENT static rules (all of them, including D68's path-or-error rule) and removes those that fail. Each removal is one audit line in the write summary — `rescreened <sha12> <reason>`, with no lesson content. A re-screen error is non-fatal (logged; the new lessons are still written).

**On upgrade:** the first write after the upgrade removes stored lessons that fail the current rules (e.g. a lesson with no file path or error signature, or a short colon credential). Each removal is listed in that Loop's memory-write summary.

## Other changes

### I268: the Judge's unavailable message keeps the error, not the proxy URL — #471

**What was wrong:** the Judge's unavailable cause was cut to its first 120 characters, and a Go `url.Error` cause embeds the full proxy URL, which for a cluster-internal service uses up the budget and loses the actual error.

**What changed:** `unavailableLine` now replaces the quoted URL with the bare token `<proxy>` before the cap, so the cause keeps the error, not the URL.

**On upgrade:** no action; the unavailable message is shorter and carries the error.

### F62: the relay parses the KubeArmor agent's stdout; KubeArmor needs logPath=stdout — #408

**What was wrong:** the KubeArmor agent's stdout lines use camelCase keys, so the relay never decoded them and the blocked-action count and the audit lines were always empty; and the agent's default `logPath` is `none`, which writes alerts over gRPC only, so the relay's stream had no KubeArmor line at all.

**What changed:** the relay parses both key styles, so the camelCase stdout lines are decoded.

**On upgrade:** set `LOGPATH=stdout` with `kubectl -n kubearmor patch kubearmorconfig kubearmorconfig-default --type merge -p '{"spec":{"globalEnv":[{"name":"LOGPATH","value":"stdout"}]}}'` and check the agent log's `Final Configuration` line shows `LogPath:stdout`. For learn mode, also set KubeArmor defaultVisibility none and annotate each Loop namespace kubearmor-visibility=process,file,network (QUICKSTART Step 3).

### F62b: no false zero before the first KubeArmor line (D111) — #409

**What was wrong:** the relay called the engine stream healthy on attach, so with `logPath=none` every Loop showed `blockedCount: 0`.

**What changed:** healthy only after one decoded engine line from that node; until then `PolicyBlocked=Unknown`, reason `AuditStreamNoAlerts`.

**On upgrade:** a Loop on a node whose KubeArmor writes no stdout now shows Unknown instead of 0; set `logPath=stdout`.

### D79(b) part 1: the wall clock pauses while a Loop waits at AwaitingApproval — #402

**What was wrong:** a Manual-mode Loop waiting for `cox approve` at `AwaitingApproval` kept using up its `spec.budget.maxWallClock`, so a slow approver could exhaust the budget before any work ran.

**What changed:** `applyBudget` adds no active time while the phase is `AwaitingApproval`, and keeps `status.budget.lastActiveStamp` fresh so the wait is not counted after the approval either (the same stop-the-clock treatment as Paused). Owner decision D79(b). The no-real-diff stall rule (D79(b) part 2) follows in its own PR.

**On upgrade:** no action. A Loop already at `AwaitingApproval` stops accumulating on the first reconcile after the upgrade.

### D79(b) part 2: stall on N failures with no real diff — #403

**What was wrong:** the stall detector hashed only the check output, which the agent influences, so varying the failure text dodged it; `maxIterations` was the only bound.

**What changed:** the verify Job's tamper init now prints `diffhash: <64 hex>` on every clean run — the SHA-256 of the agent's whole diff `baseCommit..verifiedCommit` — and writes it to its termination message (`/tmp/termination.log`, on its own `tamper-tmp` emptyDir). The operator records it as `status.stallHistory[].diffHash`. The stall gate now fires on either rule, with the same `stallAfter` (N, default 3) and the same `stallAction`: N consecutive identical failing outputs (the old rule), or N consecutive failures with the same agent diff, even when the failing output differs (D79(b), "no real diff"). The Stalled condition and the StallDetected Event name the rule: "identical verify failures" or "verify failures with no real diff (the same agent diff)".

**On upgrade:** a verify Job created before the upgrade reports no diffhash, so the new rule starts counting from the first post-upgrade verify. `status.stallHistory[].diffHash` is new and optional.

### D84a (F74): a lost workspace PVC with no usable checkpoint fails the Loop — #399

**What was wrong:** When the workspace PVC was lost and no usable checkpoint
existed (the checkpoint's snapshot was gone, every `status.checkpoints`
entry was untrusted, or the VolumeSnapshot CRD was not installed), the
operator's A2 fallback recreated the workspace PVC **empty** and the Loop
carried on. Because `status.baseCommit` stayed set, init-workspace never
re-cloned, and the agent ran in an empty workspace — the content was lost
but the Loop never said so in a way that stopped it (the F74 finding,
`REVIEW-PHASE1-R32.md:102-110`).

**What changed:** The operator now fails the Loop terminally (the F93a
pattern): the `Failed` condition with reason `WorkspaceLost`, a message
naming the lost checkpoint and the fix (a fresh Loop clones from scratch —
delete and re-apply the Loop), the phase flipped to `Failed`, and one
Warning Event. The `Restore` condition keeps the `NoUsableCheckpoint` cause
record (the three-variant message: the missing snapshot, the all-untrusted
entries, or the missing CRD). No empty PVC is created (the F36
fork-reject precedent: the bootstrap hold prevents
`ensureWorkspacePVC` from seeding the failed Loop in the same pass). A
usable checkpoint still restores as before (the A2 restore path is
untouched).

### I208: PolicyValid's message names both policy kinds — #398

A Loop whose `spec.policyRefs` resolves to a `ClusterAgentPolicy` (P6a) was
left with `PolicyValid=True` and the message "all referenced AgentPolicies
are valid" — the message named the wrong kind for a cluster-only ref. The
True message is now kind-neutral ("all referenced policies (AgentPolicy /
ClusterAgentPolicy) are valid"); the reason stays `Valid`.

### I212: a runner Loop with no model endpoint is held — #396

**What was wrong:** A Loop on the operator's runner (`spec.agent.image` empty
or equal to `--runner-image`) with no `spec.agent.endpointSecretRef` and no
`spec.agent.modelEndpoint` was admitted, the sandbox was created anyway, and
the agent crash-looped in Planning with `COX_MODEL_BASE_URL is empty; the
runner requires the operator-set model proxy URL`, with no condition naming
the cause (the v0.1.3 smoke run hit this applying the shipped sample as-is;
the three samples ship the model fields commented out).

**What changed:** The operator now holds such a Loop pre-sandbox with
`ModelConfigValid=False reason=NoModelEndpoint` (the D34 pair check's
`MissingModelEndpoint` is unchanged); no sandbox, no model-proxy, no egress-proxy objects are created. A non-runner `spec.agent.image` is not held.
The three sample comments now say the model fields are required before
applying. `docs/OPERATOR.md` lists the new reason under the key conditions.

**On upgrade:** a runner Loop without model fields that used to crash-loop is
now held with `ModelConfigValid=False NoModelEndpoint`. Recreate it with
`spec.agent.endpointSecretRef` + `spec.agent.modelEndpoint` (+ `model`) set —
`modelEndpoint` is immutable, so an existing Loop cannot be edited in place.
