# Next phase: Phase 6 — policy breadth, defence in depth, and the retry path

**Status:** approved by the owner, 2026-11-04 (decisions D95–D101 recorded
under Owner decisions). The open decisions D95–D101 are decided (owner, 2026-11-01 and
2026-11-04); see Owner decisions. Built from the owner's 2026-11-01 decisions: the **homelab readiness
direction** (running on the homelab is the point of the next phase; the
homelab side is planned in the homelab repo — `papawattu/homelab` PR #149,
`docs/coxswain-homelab-readiness-plan.md` — so only the coxswain-side C1–C3
slices land here, first in the order), **D81(b)** (plan both the
`ClusterAgentPolicy` CRD and learn mode as slices), **D85(b)→(a)** (the
annotation slice is b2's in-progress work; the ValidatingAdmissionPolicy
defence in depth is this phase's slice), and **D86** (queue the retry path
for `Failed:SandboxFailed`). Also carries forward from PLAN.md: the Judge
sidecar (D60), deferred to Phase 7 (owner, 2026-11-04), and the
oscillation detector, deferred (owner, 2026-11-04); neither lands in this
phase's slice list.

**What the phase is:** the homelab prerequisites (C1 multi-arch release
images — the homelab has schedulable arm64 nodes; C2 operator-level node
placement for a dedicated node pool; C3 the k3s/flannel + SMB profile in
OPERATOR.md, written after the homelab's H3/H4 results). On that base, the
alpha's enforcement and human-gate machinery gets widened: (1) a
cluster-scoped `ClusterAgentPolicy` so org-wide allows stop requiring one
`AgentPolicy` per namespace (ADR-0007 Q2); (2) an explicit audit-only
**learn mode** plus `cox policy suggest` so default-deny policies can be
authored from observed activity instead of by trial and error (ADR-0007
learn mode, D31); (3) a ValidatingAdmissionPolicy that makes the editor
role's residual `loops/status` write grant non-forgeable — D85's "then (a)
later"; and (4) a `cox retry` path for `Failed: SandboxFailed` Loops, so a
mount wedge stops costing a delete-and-re-apply.

**Done when:** the release workflow publishes linux/amd64 + linux/arm64
manifests for all five images, Loops land on a dedicated node pool, and
OPERATOR.md documents the k3s/flannel + SMB profile with evidence (C1–C3);
a `ClusterAgentPolicy` applies to a Loop in another namespace via
`policyRefs`; a Loop run in audit-only posture records its activity and
`cox policy suggest` prints a candidate `AgentPolicy` a human can review; a
non-manager status PATCH that touches anything other than
`PlanApproved`/`PlanRejected` is rejected by admission; and a
`Failed:SandboxFailed` Loop with the cause fixed can be retried without
losing its history.

## Why it is safe to take now

- v0.1.x is released and the Phase 5 shared memory (P5a–P5f) is on `main`.
- The homelab side is planned and tracked separately (`papawattu/homelab`
  PR #149); C1–C3 are the coxswain-side prerequisites, and none of them
  changes the operator's decisions on kind — they add image arches,
  placement, and a documented profile.
- The D85 annotation slice (b2, in progress) removes the editor's need for a
  broad status write first; the VAP slice here then closes what remains. The
  two are ordered, not parallel.
- Every slice rides an existing decision: D81(b) (owner, 2026-11-01) for
  ClusterAgentPolicy + learn mode; D85 (b2's in-progress (b) slice + (a)
  later); D86 (R40 records (a)'s acceptance met — the runbook remedy,
  `docs/UPGRADE.md:383-394` — but the (a)/(b) choice itself is still open,
  and the owner's 2026-11-01 direction queues (b) as this slice); and the
  ADR-0007 rows for the policy items.
- No slice touches the phase machine, delivery, or the tamper gate.

## Slices (in order; one PR each, TDD per AGENTS.md test norms)

### C1. Multi-arch release images (linux/amd64 + linux/arm64)

- **Source:** the owner's 2026-11-01 homelab-readiness direction (the
  homelab has schedulable arm64 nodes; the homelab side is
  `papawattu/homelab` PR #149, `docs/coxswain-homelab-readiness-plan.md`,
  and this is the coxswain-side C1 slice). Today
  `.github/workflows/release.yml` builds the five images (controller,
  runner, model-proxy, egress-proxy, tool-proxy — the matrix at
  `.github/workflows/release.yml:23-43`) with `docker/build-push-action`
  and no `platforms`, so each release image is single-arch
  (`.github/workflows/release.yml:60-71`).
- **What:** add `platforms: linux/amd64,linux/arm64` to the build job
  (buildx is already set up, `release.yml:50`); the release job's
  `make build-installer` and chart stamping (`release.yml:73-96`) consume
  the tags unchanged — the tag becomes the multi-arch manifest.
- **Acceptance:**
  - `docker buildx imagetools inspect` on the next v* tag shows both
    linux/amd64 and linux/arm64 for all five images; paste the output in
    the PR (evidence norm, R28 I92).
  - An arm64 node pulls and runs the controller + a Loop end-to-end; if no
    arm64 hardware is available for the proof, say so explicitly in the PR
    rather than claim it (an e2e never prints PASS for a skipped step,
    R25 I84).
- **Decisions needed:** none. **This is a `.github/workflows` edit, so it is
  an OWNER push — the builder stops and reports, the owner pushes that
  commit (AGENTS.md credential rules).**
- **Done when:** a v* tag produces images that a homelab arm64 node can pull
  and run.

### C2. Loop node placement (operator flags for nodeSelector + tolerations)

- **Source:** the owner's 2026-11-01 homelab-readiness direction (Loops land
  on a dedicated node pool). No placement control exists today: a grep over
  `internal/controller/` finds no nodeSelector or toleration in operator
  code, and `cmd/main.go`'s flag set (`:99-149`) carries no placement flag.
  The flag + ManagerOption pattern to follow is the existing
  `--memory-storage-class` (`cmd/main.go:138`).
- **What:**
  - Operator flags (recommended over a Loop spec field for the first cut —
    placement is cluster-ops, not per-Loop): `--loop-node-selector`
    (comma-separated key=value pairs) and `--loop-tolerations` (comma-
    separated `key:op:effect[:value[:seconds]]` entries), both defaulting
    to empty (no placement — today's behaviour).
  - Applied to every operator-owned object that runs Loop work: the
    sandbox (the Sandbox spec), the verify Job, the deliver Job, and the
    memory-writer Job. The per-Loop model-proxy pod joins the dedicated pool
    (decided, owner 2026-11-01, D101): it is Loop-scoped infrastructure, and
    leaving it on the default pool is an invisible surprise.
  - OPERATOR.md's deployment section: set the flags + a node-pool label +
    a taint, so Loops and only Loops land on the dedicated pool.
- **Acceptance:**
  - envtest: with the flags set, the sandbox and the verify/deliver/
    memory-writer Jobs carry the nodeSelector + tolerations; with them
    unset, none of the objects carry any. A malformed flag value surfaces
    at startup with the bad token named (the spec must FAIL when a
    malformed selector is silently dropped).
  - unit: the flag-parsing helpers with malformed inputs → a startup error.
  - kind (or the homelab once H3/H4 land): a node labelled + tainted with
    the matching toleration; a Loop's sandbox pod and verify Job pod land
    on the dedicated node (`kubectl get pod -o wide`, pasted in the PR); the
    unschedulable case (flags set, no matching node) surfaces
    `SandboxNotStarting` (the F48 surfacing,
    `internal/controller/loop_s4_phase.go:1044-1049`) and ends
    `Failed:SandboxFailed`, not a silent wedge; operator log shows zero
    `forbidden` errors (positive control).
  - Mutation: drop the tolerations from the Job specs (keeping the
    nodeSelector) → the "lands on the dedicated node" proof fails on the
    tainted node.
- **Decisions needed:** (decided, owner 2026-11-01, D101) the proxy pod
  joins the pool; flags vs a Loop spec field (recommendation: flags for the
  first cut — a per-Loop override can follow without breaking the flags).
- **Done when:** setting the flags pins every operator-owned Loop workload
to the node pool.

### C3. The k3s/flannel + SMB profile in docs/OPERATOR.md

- **Source:** the owner's 2026-11-01 homelab-readiness direction (the
  homelab runs k3s with flannel and SMB-backed storage). OPERATOR.md's
  tested profiles today are kind + the D38 Calico test profile
  (`config/calico/kustomization.yaml`, `docs/TROUBLESHOOTING.md:220-232`).
  **This slice is written after the homelab H3 (SMB storage) and H4
  (flannel under the D38 CNI gate) results come back** (`papawattu/homelab` PR
  #149), so its contents carry that evidence, not assumptions.
- **What:** a new section in `docs/OPERATOR.md`: the k3s profile — install
  (install.yaml/chart on k3s), the D38 network gate under flannel (the H4
  evidence says how; if H4 shows a gap, the section documents the gap + the
  flag that works around it, and the gap becomes a queue item, not a silent
  workaround in the docs, R26 I90); the SMB storage profile — the
  StorageClass for workspace PVCs and the memory PVC (P5a's
  `ReadWriteMany`; `--memory-storage-class`, `cmd/main.go:138`), checkpoint
  behaviour under the SMB VolumeSnapshot class (or the
  `CheckpointsAvailable=False` fallback, which is the documented, visible
  path — D59); and the k3s deltas the H results name (agent-sandbox under
  k3s, flannel and the CNI probe).
- **Acceptance:**
  - Every claim names its evidence: the homelab H3/H4 result (PR #149) or a
    k3s run with the command + output in the PR (evidence norm, R28 I92 —
    no "should work" statements).
  - A reader can follow the section on a fresh k3s node to a running Loop
    (proven by the homelab run or a k3s kind-adjacent run; the PR says which
    one).
- **Decisions needed:** none at draft time; the H3/H4 results may create
  queue items (e.g. a flannel gap in the CNI probe) — reported, not worked
  around.
- **Done when:** OPERATOR.md's supported-profiles story names
  k3s/flannel + SMB with evidence, and the homelab plan (PR #149) can point
  at it.

### P6a. ClusterAgentPolicy (ADR-0007 Q2, D81(b))

- **Source:** ADR-0007 Q2 — "a namespaced `AgentPolicy`, selected by the
  Loop (`spec.policyRefs[]`, …); plus an optional cluster-scoped
  `ClusterAgentPolicy` for org-wide allows"
  (`docs/adr/0007-agentpolicy-default-deny-ebpf-audit.md:50-56`); Consequences
  — "New CRD `AgentPolicy` (+ optional `ClusterAgentPolicy`)"
  (`:243-245`). The shipped CRD is namespaced only
  (`config/crd/bases/coxswain.wattu.com_agentpolicies.yaml:15`, scope
  Namespaced; no `clusteragentpolicies` CRD in `config/crd/bases/`), and
  `spec.policyRefs` is a list of *names in the Loop's namespace*
  (`api/v1alpha1/loop_types.go:804-808`).
- **What:**
  - New CRD `ClusterAgentPolicy` (cluster-scoped, same schema as
    `AgentPolicy` — reuse the types, second Kind). A `Loop.spec.policyRefs`
    entry may name a `ClusterAgentPolicy` (a disambiguation rule is needed
    where a namespace and a cluster policy share a name — decided, owner
    2026-11-04, D98: namespaced wins, and the collision is a Warning Event,
    not a reject).
  - The effective-policy union (ADR-0007 Q2) takes the union across
    both scopes; `status.policy.effectiveHash`
    (`api/v1alpha1/loop_types.go:897-905`) hashes the union of both.
  - RBAC: policy creators' roles gain `clusteragentpolicies` get/list/watch
    (a *different role from who may create Loops* — ADR-0007 Q2).
- **Acceptance:**
  - envtest: a Loop in namespace A references a `ClusterAgentPolicy` by
    name; the effective policy (visible in `status.policy.effectiveHash`
    and the rendered KubeArmorPolicy) includes its allows.
  - envtest: namespaced and cluster policies with overlapping allows union
    (not one shadowing the other).
  - envtest: a `policyRefs` entry that names neither a namespaced nor a
    cluster policy fails with the existing unknown-policy gate behaviour.
  - RBAC coverage test updated (AGENTS.md norm: each resource's group is
    checked against a trusted table) + kind `kubectl auth can-i
    clusteragentpolicies --as=<policy-creator role>`.
  - Mutation: make the union intersection-only → the union spec FAILS.
  - kind: a Loop in one namespace picks up allows from a
    `ClusterAgentPolicy`; operator log shows zero `forbidden` errors
    (positive control per the R28 I92 norm).
- **Decisions needed:** (decided, owner 2026-11-04, D98) the name-collision
  rule — namespaced wins + Warning Event.
- **Done when:** an org-wide allow exists once and applies to Loops in any
  namespace that reference it.

### P6b. Learn mode + `cox policy suggest` (ADR-0007 learn mode, D31; D81(b))

- **Source:** ADR-0007 "Learn mode (D31, round 9)"
  (`docs/adr/0007-agentpolicy-default-deny-ebpf-audit.md:172-183`): an
  audit-only posture, a `kubectl cox policy suggest <loop>` later, reference
  policies under `config/samples/policies/` (two already ship:
  `default-deny.yaml`, `go-toolchain.yaml`); "Learn mode must be **explicit**,
  **visible in status** (`PolicyEnforced=False`, reason `AuditOnly`), and
  **never the default**." No audit-only posture exists in code today (no
  `AuditOnly` reason anywhere in `api/` or `internal/`).
- **What:**
  - An explicit opt-in field on the `AgentPolicy` (and so the
    `ClusterAgentPolicy`) — `spec.auditOnly: true`, never defaulted — that
    makes the engine
    **record instead of block** for that policy's allows. The Loop's
    KubeArmorPolicy renders with the audit-only rules; status shows
    `PolicyEnforced=False` reason `AuditOnly` (a new condition/reason if the
    existing `PolicyEnforced` shape cannot carry a per-policy audit subset —
    see the condition-shape decision below).
  - The activity-audit stream (already shipped for the eBPF engine, ADR-0007
    Q4) is the recorded source; the suggestion tool reads the Loop's audit
    records (the trusted sources that the operator can read) plus the
    platform minimum (run entrypoint, workspace RW, model proxy on
    localhost) and prints a candidate `AgentPolicy` YAML to stdout — human
    review, no auto-apply.
  - `cox policy suggest <loop>` CLI command (new command group `policy`).
- **Acceptance:**
  - envtest: a Loop with an audit-only policy ref gets `PolicyEnforced=False`
    reason `AuditOnly`; a Loop with an enforcing policy is unchanged
    (spec must FAIL when the audit-only flag is removed from the test
    fixture — the gate-spec norm).
  - envtest: audit-only never defaults — a policy with the field unset
    renders block-mode rules.
  - unit: the suggest tool's candidate generation from a fixture audit
    stream (each recorded allow appears once; the platform minimum is
    always present; no deny rules are ever emitted — allows only).
  - kind: run a Loop in audit-only posture, capture the audit stream, run
    `cox policy suggest`, and show the candidate YAML in the PR (evidence
    saved per the R28 I92 norm).
  - Mutation: make the renderer emit block rules when audit-only is set →
    the `AuditOnly` spec FAILS.
- **Decisions needed:** (decided, owner 2026-11-04, D99) the audit-only
  posture is a field on the `AgentPolicy` (and so the `ClusterAgentPolicy`),
  not on the Loop; open: the condition shape if a Loop references both an
  enforcing and an audit-only policy (recommendation: the condition reflects
  the *enforcing* policies; the audit subset is recorded in
  `status.policy` + an Event).
- **Done when:** a human can run an agent image once in audit-only and get a
  reviewable starting policy instead of trial and error.

### P6c. D85(a): the status-write VAP (defence in depth behind b2's D85(b) slice)

- **Source:** D85 — "the Loop editor role can write Loop status, which makes
  every status-driven decision forgeable"
  (`docs/REVIEW-PHASE1-R34.md:386-398`); owner decision: (b) annotation
  slice first, "(a) an admission policy as defence in depth can follow
  later" (`.samples/decide-in-5-minutes.md:46` reply
  `D85: (b), then (a) later`; the digest row
  `.samples/owner-decisions-digest.md:227`). b2's (b) annotation slice is
  **in progress** — this slice follows it.
- **What:**
  - A ValidatingAdmissionPolicy (CEL, matching the shipped I85 VAP + binding
    pattern, `config/validation/`) that applies to
    `loops/status` `update`/`patch` and allows a non-manager status write to
    change **only** the `PlanApproved` and `PlanRejected` conditions; any
    other status change (phase, budget, stall history, verify pin,
    `lastCheckResults`, `lastFailureSummary`, …) is rejected with a message
    naming the offending field. The manager's own ServiceAccount is
    exempt (identified by SA, so the operator is never gated by its own
    VAP).
  - After b2's (b) lands, the editor role's status write grant exists only
    for the pre-annotation migration window (or not at all) — the VAP is
    the backstop that makes *either* state safe: even a user holding the
    grant cannot forge beyond the two conditions.
  - Document in OPERATOR.md (the conditions table) and in
    `config/rbac/loop_editor_role.yaml`'s header comment (the current
    comment at `:35` says the editor writes the two conditions — the VAP
    makes that the *enforced* truth).
- **Acceptance:**
  - envtest (admission runs in envtest with a VAP binding): a status PATCH
    as the editor SA changing only `PlanApproved` is **allowed**; the same
    PATCH additionally touching `status.iteration` is **rejected** with the
    field named.
  - envtest: a PATCH of `status.phase` as the editor SA is rejected; as the
    manager SA it is allowed (the exemption works — the spec must FAIL when
    the exemption is removed).
  - kind: `kubectl patch` a Loop's status as an editor-bound user (test
    user) and show the rejection; operator log shows the operator's own
    status writes succeeding (positive control).
  - Mutation: widen the CEL to allow `status.phase` → the phase-rejection
    spec FAILS.
  - `make install.yaml` regeneration + chart sync (the VAP rides the release
    manifests, as I85's does).
- **Decisions needed:** none beyond confirming the VAP may key on Service
  Account identity (the I85 precedent already uses admission context; the
  exact exempt mechanism — SA name vs label — is an implementation detail to
  confirm against the I85 VAP in the implementing slice).
- **Done when:** no credential that is not the manager's can make a
  status-driven decision (the 17 reads in
  `.samples/status-driven-decisions.md`) fire from a forged status write.

### P6d. D86: the retry path for `Failed: SandboxFailed`

- **Source:** D86 — "a mount failure is now terminal, and the remedy is
  delete-and-re-apply" (`docs/REVIEW-PHASE1-R35.md:233-259`):
  `loop_s4_phase.go:1597-1615` (exhausted → `Failed:SandboxFailed`), no
  `retry` command in `internal/cli/` (the shipped set is
  `describe, history, logs, plan, pause, resume, redeliver, fork, approve,
  reject, diff, memory` — `internal/cli/cli.go:120-134`), and
  `Failed:SandboxFailed has no retry`
  (`internal/controller/loop_f48g_episode_boundaries_envtest_test.go:18`).
  (`docs/REVIEW-PHASE1-R40.md:247-255`) while the (a)/(b) choice itself stays
  open; the runbook remedy is `docs/UPGRADE.md:383-394` (the F48g section,
  `Failed:SandboxFailed`, "delete and re-apply") and
  `docs/TROUBLESHOOTING.md` (the F48b entry).
- **What:**
  - `cox retry <loop>`: sets a one-shot `coxswain.io/retry` annotation on a
    Loop that is `Failed` with reason `SandboxFailed` (the I52
    `coxswain.io/redeliver` shape — `loop_deliver_job.go:524-531`), refused
    for every other failed reason (a `TamperedVerify` retry would be a gate
    bypass; `PhaseTimeout` already has its resume path).
  - The operator, on seeing the annotation: resets the sandbox-attempt
    episode (the F48 run-key machinery — `loop_s4_phase.go:1630`-region
    attempt counter + the F48g run key) to zero, clears `Failed`, and
    re-enters the sandbox lifecycle from the current checkpoint/branch state
    (no phase reset: the Loop resumes the phase it died in). Clears the
    annotation after acting (idempotent, like `redeliver`).
  - Document in TROUBLESHOOTING.md (the F48b entry gains the retry remedy,
    replacing the delete-and-re-apply as the first step) and UPGRADE.md.
- **Acceptance:**
  - envtest: a `Failed:SandboxFailed` Loop + the annotation re-enters the
    sandbox lifecycle; the attempt counter restarts from zero (a second
    wedge of the same class still reaches the bound — the bound is per-
    retry-episode, not lifetime).
  - envtest: the annotation on a `Failed:TamperedVerify` Loop is refused
    (the CLI refuses; the operator ignores an annotation it did not accept);
    the spec must FAIL when the reason gate is removed.
  - envtest: the annotation clears after acting; re-setting it retries again
    (unbounded and audited, decided owner 2026-11-04, D100 — the retry is
    explicit human intent, recorded in the decision audit).
  - CLI unit: `cox retry` refuses non-`SandboxFailed` reasons with the
    reason named in the error.
  - kind: wedge a Loop with a missing git-cred Secret, retry after creating
    it, show it resumes and reaches its verify; log + `describe` saved as
    evidence.
  - Mutation: skip the attempt-counter reset → the "second wedge still
    reaches the bound" spec FAILS.
- **Decisions needed:** (i) annotation vs `cox retry` — recommendation:
  `cox retry` that *sets* the annotation (matches `redeliver`); (ii)
  retry bound — (decided, owner 2026-11-04, D100) unbounded and audited,
  only for `SandboxFailed`; the per-episode attempt bound still applies; (iii)
  confirm the
  resume-from-checkpoint state is the current checkpoint, not the last
  passed verify's (recommendation: same restore path a fresh sandbox uses).
- **Done when:** the runbook remedy for a mount wedge is "fix the Secret,
  `cox retry <loop>`", with the Loop's history preserved.

## Explicitly deferred (keep their homes)
- **The Judge sidecar** (PLAN.md Phase 6, `docs/PLAN.md:90`; D60) — deferred
  to Phase 7 (owner, 2026-11-04, D95).
- **The oscillation detector** (PLAN.md Risks, `docs/PLAN.md:124` — "An
  agent that alternates between two failure modes never trips the
  consecutive-hash stall detector. Deferred; needs its own detector.") —
  deferred (owner, 2026-11-04, D96).
- **D80(b): the per-node CNI probe** (owner, 2026-11-01). The CNI gate's
  evidence is ONE probe pod on ONE node (`internal/cni/pod_prober.go`); a
  multi-node cluster's non-enforcing node would not be detected. Documented
  as the D80 one-node limit in `docs/TROUBLESHOOTING.md` (D83 PR). The
  per-node probe (one probe pod per node; the gate satisfied only when every
  node passes) is deferred until multi-node clusters are a support target.
- **The D94 revisit** (F60's out-of-band canary-token channel) — decided
  2026-11-04 (D97): the out-of-band channel goes to Phase 7, with a design
  note first; D94's accepted limit stands through Phase 6. Draft #375
  (file-side comparison) was closed because the token still reached the
  agent's package dir.
- **The homelab H slices** (`papawattu/homelab` PR #149) — tracked in the
  homelab repo, not this plan; C1–C3 are the coxswain-side prerequisites,
  and C3 blocks on the H3/H4 results.

## Risks

- **The homelab is a second tested topology (C1–C3).** Every kind-verified
  claim (CNI gate, KubeArmor gate, storage, agent-sandbox) has to hold on
  k3s/flannel/SMB too, and a gap found there is a queue item with a repro,
  not a docs footnote. Mitigation: C3 is written *after* H3/H4, so the docs
  carry evidence; C2's kind proof uses the same dedicated-pool shape the
  homelab uses; C1's arm64 proof names what was actually run (no skipped-
  step PASS, R25 I84).
- **The multi-arch build widens the release surface (C1).** Five images ×
  two arches: a broken arm64 build fails the tag, and a silently wrong
  manifest (amd64-only under a multi-arch tag) ships to the homelab.
  Mitigation: the acceptance is the `imagetools inspect` output pasted in
  the PR for the tag, and the first homelab run is the arm64 proof.
- **Node placement can hide scheduling failures (C2).** A Loop whose
  sandbox cannot land on the pool would wedge silently if nothing
  surfaced it. Mitigation: the existing `SandboxNotStarting` surfacing
  (the F48 condition,
  `internal/controller/loop_s4_phase.go:1044-1049`) fires and the episode
  ends `Failed:SandboxFailed`, for which P6d's retry path is the remedy
  once the pool is fixed — the C2 acceptance proves the unschedulable case
  surfaces, not just the happy path.- **Audit-only posture leaks enforcement.** A Loop in learn mode is
  *recorded*, not blocked (P6b) — a user who forgets the posture is running
  an agent with no eBPF fence. Mitigation: it is explicit and non-default
  (a policy field, never defaulted), visible in status
  (`PolicyEnforced=False` reason `AuditOnly`) and in `kubectl cox
  describe`, and the operator emits a Warning Event on each audit-only
  iteration. The KubeArmor policy gate (A1) still requires the engine to be
  *installed*; the audit posture is a mode within an installed engine.
- **The VAP's exemption is a new trust edge (P6c).** The manager SA is
  exempt from its own admission policy. Mitigation: the exemption keys on
  the SA identity only (no label a Loop author can set), and the envtest
  specs assert both the rejection and the exemption, so widening the
  exempt set is a spec failure. A user with *any* cluster write on
  `loops/status` who also controls the manager SA already owns the cluster.
- **The VAP and the annotation slice race (P6c).** b2's D85(b) slice changes
  who writes status when; this slice changes what a residual write may do.
  They are ordered (b2 first), and the VAP is written to be correct under
  *both* states (with and without the editor grant), so landing either first
  is safe.
- **A retry path invites retry abuse (P6d).** An explicit `cox retry`
  against `Failed:SandboxFailed` could mask a persistently broken workspace
  Secret or PVC. Mitigation: the retry is refused for every other failed
  reason (no gate bypass — the only retryable reason is one that says
  nothing about the agent's work, by the F48b design), every retry is an
  operator decision recorded in the decision audit + an Event, and the
  per-episode attempt bound still applies inside a retried episode.
- **Cluster-scoped policy widens the blast radius (P6a).** A
  `ClusterAgentPolicy` visible to (and referenceable by) Loops in every
  namespace means one bad policy grants allows cluster-wide. Mitigation:
  the *creating* role is separate from the Loop-creating role (ADR-0007
  Q2), the union is additive and visible in `status.policy.effectiveHash`
  per Loop (D32), and policy changes apply at the iteration boundary with
  the hash recorded per iteration.

## Owner decisions

| ID | Question | Recommendation | Decision |
|---|---|---|---|
| D95 | **Judge sidecar (PLAN.md:90, D60):** include it in this phase, or keep it deferred to Phase 7? | Keep deferred: it is the one other LLM-judgment surface (like the Memory Curator, ADR-0002) and this phase is already four slices of security-adjacent machinery. Phase 7's hardening theme is its natural home. | The judge sidecar is **deferred to Phase 7** (owner, 2026-11-04). |
| D96 | **Oscillation detector (PLAN.md:124):** include it in this phase, or keep it deferred? | Keep deferred: it is a stall-detector extension (a new normalisation + a new detection rule over `status.stallHistory`), independent of every slice here, and the stall gate it complements is working. | The oscillation detector is **deferred** (owner, 2026-11-04). |
| D97 | **D94 revisit (F60's open limit, I199):** a real out-of-band canary-token channel — the token travels over a channel the agent's code cannot read (e.g. the canary passes the expected token to the operator out-of-band and the comparison happens operator-side, not file-side). Draft #375 was closed because the token still reached the agent's package dir. Include it in this phase? | **Question only, no slice drafted.** The canary is the negative control for the acceptance checks and the token read is a second-order subversion (R45 D94's own framing); if the owner treats the canary as load-bearing for release, it becomes the next slice after this phase. No design is in the slice list. | The canary out-of-band channel goes to **Phase 7, with a design note first** (owner, 2026-11-04). D94's accepted limit stands through Phase 6. |
| D98 | **P6a name-collision rule:** a `policyRefs` entry that names both a namespaced `AgentPolicy` and a `ClusterAgentPolicy` — namespaced wins + Warning Event (recommended), or reject the Loop at admission? | Namespaced wins + Warning Event: a cluster policy is org-wide boilerplate; a namespaced one is the Loop author's explicit choice. Rejecting breaks Loops when an org policy happens to share a name. | (already decided 2026-11-04) Namespaced wins, plus a Warning Event. Shipped in P6a / #389. |
| D99 | **P6b opt-in location:** the audit-only posture on the `AgentPolicy` (recommended — the engine mode is per-policy) or on the Loop (`spec.policyAuditOnly`)? | Policy field: the posture describes what the *policy* does to its allows, and a Loop that references it inherits the decision in one place. | The P6b audit-only posture is a **field on the `AgentPolicy`** (and so the `ClusterAgentPolicy`), not on the Loop (owner, 2026-11-04). |
| D100 | **P6d retry bound:** unbounded explicit retries (recommended — human intent, audited) or a per-Loop cap (e.g. 3, then the Loop needs a fork)? | Unbounded + audited: the retry refuses every reason except `SandboxFailed`, which by the F48b design says nothing about the agent's work; the per-episode attempt bound still applies. A cap would force a fork (losing status) for a transient infra class the owner can fix. | The P6d explicit retry is **unbounded and audited**, only for `SandboxFailed`; the per-episode attempt bound still applies (owner, 2026-11-04). |
| D101 | **C2 proxy pod placement:** the per-Loop model-proxy pod is operator-owned (its usage endpoint is read by the operator, `loop_controller.go:577`) — does it join the dedicated pool (recommended), or stay on the default pool as operator-side infrastructure? | Join the pool: a reader who sets the placement flags expects every object the Loop creates to land there; a proxy pod silently on the default pool is the same class of surprise the D38 NetworkPolicy carve-outs exist to document. | (decided 2026-11-01) The C2 placement flags pin every Loop pod (owner, 2026-11-01). Shipped in #381. |

## Estimate

b2 alone, at about 2 slices a day including review rounds (the Phase 5 pace
the owner set): C1 (a small workflow edit, but an **owner push** — it gates
on the owner's availability and the next v* tag's arm64 proof, not on
builder time), C2 (1 day — flag plumbing + the object sites + the kind
proof including the unschedulable case), C3 (a few hours of writing, but
it **blocks on the homelab H3/H4 results** from PR #149). Then the
policy/gate slices: P6a (1 day — second Kind, union extension, RBAC, kind
proof), P6b (1–2 days — renderer mode + new condition + CLI tool + kind
capture), P6c (1 day — CEL VAP + binding + manifest/chart regen; blocked
until b2's D85(b) annotation slice merges), P6d (1 day — annotation handler
+ run-key reset + CLI + kind proof). **Total: 6–7 working days of builder
time**, i.e. about 3–4 weeks of wall-clock including review rounds, the b2
dependency, the owner push for C1, and the homelab H3/H4 results for C3.
