# Release notes: changes since v0.1.3 (unreleased)

Entries are added per merged PR, each citing the PR and the squash commit it
landed as, like `RELEASE-NOTES-v0.1.3.md`.

## Upgrade

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

## New features

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

**What it is:** `kubectl cox policy suggest <loop> -f <file|->` reads the
operator's Q4 audit lines (e.g. `kubectl logs -n coxswain-system
deploy/coxswain-controller-manager | kubectl cox policy suggest <loop> -f -`)
and prints a candidate `AgentPolicy` built from what the agent actually used
in learn mode. `--name` defaults to `<loop>-suggested`; the namespace is the
CLI's `--namespace`. Lines that are not audit records, and other Loops'
records, are skipped.

**How to use it:** run the Loop in learn mode, capture the audit lines, pipe
them to `cox policy suggest`, review the printed candidate, apply it (still in
learn mode to confirm), then drop `auditOnly` to enforce. The candidate holds
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
file — every directory, recursive — so KubeArmor records every exec and file
access. The egress proxy runs in learn mode even when no policy has network
allows: its allowlist is then empty, so it blocks every host and records each
connect, and `cox policy suggest` turns those connects into network allows.
Enforcing mode is unchanged: no catch-all rules, and no egress proxy without
network allows.

**On upgrade:** a learn-mode Loop now also gets an egress proxy pod; network
for the agent in learn mode goes through it and is blocked unless allowed, as
before.

## Security fixes

### D78(b): a credential key followed by ':' and any value is dropped from lessons — #401

**What was wrong:** the credential-assignment rule only dropped a credential key (`password|passwd|secret|token|api_key`) when followed by `:` and an 8+ character token-shaped value, so short values such as `password: hunter2` were written to memory.

**What changed:** a credential key followed by `:` and any non-space value is now dropped, whatever its length. The accepted cost is that prose such as `the token: parser lives in …` is dropped too.

**On upgrade:** existing lessons are re-screened against the current rules on the first memory write after the upgrade and any that fail are removed (see D73(b)); a short colon credential stored before the upgrade is removed at that point.

### D73(b): the memory writer re-screens existing lessons on every write — #401

**What was wrong:** lessons that failed the current static rules (for example D78(b)'s credential rule or D68's path-or-error rule) could stay on the memory PVC forever: a lesson was only screened when it was proposed.

**What changed:** on every memory write, the writer re-screens the existing `<sha256>.txt` lessons against the CURRENT static rules (all of them, including D68's path-or-error rule) and removes those that fail. Each removal is one audit line in the write summary — `rescreened <sha12> <reason>`, with no lesson content. A re-screen error is non-fatal (logged; the new lessons are still written).

**On upgrade:** the first write after the upgrade removes stored lessons that fail the current rules (e.g. a lesson with no file path or error signature, or a short colon credential). Each removal is listed in that Loop's memory-write summary.

## Other changes

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
