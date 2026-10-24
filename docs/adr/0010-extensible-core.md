# ADR-0010: coxswain as a general orchestrator for gated, isolated agent workflows (D36)

**Status:** Accepted (owner, 2026-09-28, D36)
**Date:** 2026-10-24 (this ADR); the decision was made 2026-09-28
**Supersedes:** — (this ADR is the record D36's "ADR written" box refers to)
**Extends:** ADR-0004 (claims vs. evidence), ADR-0005 (operator-held verify
evidence), ADR-0006 (zero-credential agent), ADR-0007 (default-deny
AgentPolicy, eBPF enforcement), ADR-0008 (tool proxies as the first
capability extension)
**Origin:** `docs/REVIEW-PHASE1-R15.md` D36 — the owner's decision (R15:13,
"Decided (owner, 2026-09-28): option (d), an extensible core") and the
decision text (R15:16-49). Line numbers below refer to that file.

## Context

Coxswain began life shaped around one workflow: plan → implement → verify on
a git repo, delivering a PR. The owner's long-term plan is for coxswain to
implement the AI-native SDLC (R15:51, from
[bashebr/ai-native-sdlc](https://github.com/bashebr/ai-native-sdlc)), and the
gap analysis in R15 (R15:74) showed that coxswain already has the two
hardest pieces — trustworthy isolated **Build** and trustworthy
operator-verified **Test** — while everything around them (stages, gates at
every handoff, a durable approval ledger, intake, deploy, maintain) is
missing.

Four options were on the table (R15:98-113): (a) coxswain as a Build/Test
engine driven by an external SDLC; (b) the whole lifecycle inside coxswain;
(c) staged (a)-now-(b)-later; **(d) an extensible core — chosen by the owner**
(R15:110).

Under (d), coxswain defines an **extension point for every capability in the
gap table**, and each capability is implemented by a pluggable extension.
The core keeps what must be trusted: isolation, the evidence gates, and
ledger integrity. Extensions supply everything else (R15:110-113).

## Decision

The owner decided (R15:13, 2026-09-28): **option (d), an extensible core.**
This ADR records that decision. Nothing here is new; every statement of the
decision is taken from R15 D36.

### The three principles (R15:16-28)

1. **Not SDLC-specific.** coxswain orchestrates *any* workflow that needs
   stages carried out by untrusted agents in isolation, human or evidence
   gates between stages, an approval record, and a result handed back
   (R15:17-22). The AI-native SDLC is the first and motivating **workflow
   definition**, not the product's shape — consistent with the earlier owner
   direction that coxswain must not assume coding agents (R15:21-22).
2. **Every capability via extensions.** The core defines the extension
   points, contracts and trust boundaries; implementations plug in.
   "Example: memory could be Honcho" (R15:23-25).
3. **Simple defaults, solid externals.** Each extension point ships a simple
   built-in default so coxswain works end-to-end with nothing extra
   installed; a solid external implementation plugs in through the same
   contract (R15:26-28). The defaults stay deliberately small — the
   reference implementation of each contract, not a competitor to the
   external tools (R15:130-134).

### Generic core vs. workflow definitions (R15:30-49)

(R15:30 introduces the split; the implication for today's code is
R15:43-49.)

- **Generic core:** the workflow/stage graph, gates and approvals, the
  ledger, isolated agent execution (sandbox, proxies, NetworkPolicy, eBPF
  policy), operator-held evidence and checks, the audit stream, and the
  extension contract (R15:31-34).
- **The SDLC as one workflow definition:** its stages (intent → spec → plan
  → build → test → deploy → maintain), its artifacts (a repo, a PR), its
  checks (tests, tamper detection on protected paths) and its deliverable (a
  PR or release) (R15:35-38).
- **Other workflows** (research/report pipelines, data processing, infra
  changes, document review) plug in their own stages, checks and
  deliverables and never touch the core (R15:39-41).

**Implication for today's code (R15:43-49):** Loop/Sandbox/proxies/gates are
already largely workflow-agnostic. The coding-specific parts are the plan →
implement → verify phase names, the repo/PR assumptions and the git-specific
tamper checks. They should end up behind the workflow-definition and
"deliver" extension points, not in the core API. **No change is needed now;
it is a constraint on new slices — don't bake more coding assumptions into
the core API** (R15:47-49).

### Extension points (R15:115-128)

The gap-table capabilities become extension points, each with a built-in
default and candidate solid external implementations (R15:115-128):

| Extension point | Built-in default (per R15) | Solid externals (per R15) |
|---|---|---|
| Memory | notes file in the repo (per-repo `.coxswain/memory.md`), read-only to the agent, curated by the operator | Honcho |

> **Shipped default differs from this sketch (D66).** The built-in memory
> default that actually shipped (Phase 5, complete on `main` at `04472d5`)
> is **not** the per-repo `.coxswain/memory.md` notes file sketched above
> (R15:118). It is a **per-(namespace, git URL) PVC** in the Loop's
> namespace, **`ReadWriteMany`** (copy-in init container as the fallback when
> the storage class lacks RWX support), read-only into the agent's workspace
> at `.coxswain/memory/`, and written only by the operator's memory-writer
> Job after the static screen and the rule-gated Memory Curator
> (`docs/PLAN.md:80-82`, `docs/PLAN-PHASE5.md` D66). The per-namespace PVC
> (not a file in the agent's repo) is the shipped default; the R15 sketch is
> the originally-proposed shape, superseded by D66.
| Intake | `kubectl apply` a Loop, or a label on a GitHub issue | forms, email, hermes kanban |
| Workflow definition | a simple linear graph format, shipped with an SDLC example definition | ai-native-sdlc skill, hermes, Argo Workflows |
| Gate approvers | an annotation or `kubectl coxswain approve` by a human with RBAC | GitHub review, Slack |
| Ledger store | a hash-chained file committed to git (core verifies the chain) | an external append-only log |
| Agent roles | one runner, no extra roles | any runner image (advisory only) |
| Checks / evals | run a command in an isolated verify pod and report pass/fail (today's verify); eval = a list of such runs | ai-native-sdlc evals, custom suites |
| Deliver | open a PR (the SDLC case) or write an artifact | Argo CD/Flux for deploys, object stores, ticket systems |
| Maintain / monitoring | a webhook that turns an alert into a new Loop | Prometheus/Alertmanager with bands |
| Enforcement engine | KubeArmor | Tetragon |
| Model proxy | the stand-in forwarder | LiteLLM, a vendor gateway |

### The extension contract — the invariants (R15:139-158)

(R15:139-141 introduces the extension contract; the five invariants follow.)

The invariants any option must keep (ADR-0004/0006/0007) become the
extension contract under (d); an extension that can't meet them can't be
plugged in (R15:139-141):

- **Agents never decide gates.** Approvals come from humans or
  operator-held evidence, never from an agent's claim. Extensions may advise
  or supply data; the decision stays in the core (R15:142-144).
- **Zero-credential boundaries stay.** Extensions run outside the agent pod,
  and their credentials never reach the agent. An extension the agent must
  talk to (e.g. memory) is reached through an operator-owned,
  policy-enforced path — like the model and egress proxies — never via a
  secret mounted into the agent (R15:145-149).
- **Extension output into the agent is untrusted input.** Something the
  agent writes to memory must not be able to rewrite gates, policy or
  another Loop's context: memory is scoped per Loop, repo or tenant, and
  writes are audited (the Q4 audit stream) (R15:150-154).
- **Fail closed.** A gate whose approver or ledger extension is unavailable
  stays closed; an enforcement extension that can't prove it's enforcing
  keeps the sandbox Suspended (as D30 does today) (R15:155-157).
- **Advisory agents advise, never approve.** Any reviewer/PM/CTO-style
  agents may advise, never approve (R15:158).

### What exists in code today, and what is only intended

R15 (R15:136-137) notes that **two of the seams already exist — the
enforcement `Enforcer` interface and the model proxy image — "which shows
the pattern works."** (Audited against `origin/main` as of
`04472d5`, the branch's merge base.)

**Already built (real seams in code):**
- **Enforcement engine.** `Enforcer` is a Go interface with a KubeArmor
  implementation and a fail-closed `Enforcing` probe:
  `internal/engine/enforcer.go:37` (interface), with the D30
  fail-closed gate driven by it (the comment at
  `internal/engine/enforcer.go:43-50`).
- **Model proxy.** The per-Loop proxy pod's image is overridable
  (`internal/controller/loop_controller.go:2622-2629`: the reconciler's
  `ProxyImage` field / a future `--proxy-image` flag, or the stand-in).
  ADR-0008 (`docs/adr/0008-tool-proxies.md`) generalises the same
  pattern to **tool proxies** — a second, working example of a capability
  extension: a credentialed proxy the agent never holds the secret for,
  reached through an operator-owned policy-enforced path
  (ADR-0008 "Context").
- **Gate approvers (partially).** The Manual plan-approval gate with a
  human approver via annotation/CLI exists today (the B3 `PhaseGate` seam,
  `internal/controller/loop_approval.go:466-490`), matching R15's built-in
  default for the gate-approvers point.

**Built-in default in place, extension point still unformalised:**
- **Checks / evals.** "run a command in an isolated verify pod and report
  pass/fail (today's verify)" is exactly the S3/B3 verify Job — the built-in
  default exists; the *extension contract* (a generic check/eval surface)
  does not, yet.
- **Memory.** **Built (Phase 5, complete on `main` at `04472d5`).** The
  built-in default is a per-(namespace, git URL) PVC, read into the agent's
  workspace at `.coxswain/memory/` and written only by the operator's
  memory-writer Job after the static screen and the rule-gated Memory
  Curator (`docs/PLAN.md:80-82`, `docs/PLAN-PHASE5.md` D66). The R15 table's
  originally-proposed default (a per-repo `.coxswain/memory.md` notes file,
  R15:118) was **superseded by D66** — see the note under the extension-point
  table. The external memory extension (Honcho) is not yet built.

**Intended only — not yet built:**
- **Workflow definition.** There is no workflow/stage-graph resource or
  format on `main`; a Loop is still one plan → implement → verify run. The
  linear graph format and the SDLC example definition are not yet built.
- **Intake.** Not yet built (a Loop is started by `kubectl apply` today).
- **Ledger store.** Not yet built — no hash-chained approval ledger exists;
  Loop status and Events are not hash-chained (R15:82).
- **Agent roles (advisory).** Not yet built — one runner per Loop. The Judge
  sidecar is deferred past v0.1.0 by D60 (`docs/PLAN-BETA.md:127`; also
  listed under "Phase 6" in `docs/PLAN-BETA.md:117`).
- **Deliver (beyond PR).** Delivery opens a PR today (S6); the generic
  deliver extension (release/report/dataset) is not yet built.
- **Maintain / monitoring.** Not yet built.
- **Enforcement / model-proxy externals.** Tetragon and LiteLLM-class
  external implementations are not yet built; only the built-in defaults
  (KubeArmor, the stand-in proxy image) exist.

Plainly: the **extension-point surface itself** (a declared contract + a
registration/plug-in mechanism) is **not yet built** anywhere. Only the two
seams R15 names plus the approval seam exist today, as working Go seams; the
rest of the table is the *intended* shape of the core.

## Consequences

- **The constraint on new slices (R15:43-49) is now an ADR-level rule:**
  don't bake more coding assumptions into the core API; coding-specific
  behaviour belongs behind the workflow-definition and deliver extension
  points.
- **MVP-first still applies.** `docs/PLAN-ALPHA.md:141` parks the D45
  observer and R17 D41–D43 (tool proxies shipped as D41a–D41e; D42 fan-out
  and D43 multi-tenancy remain unplanned). The D37 flow console
  (R15 P2, `docs/REVIEW-PHASE1-R15.md:191-200`) is direction-approved but
  unplanned. None of this contradicts D36; it sequences it.
- **The extension contract is the new review bar for any capability.** A
  proposed extension must satisfy the five invariants above (R15:139-158);
  reviewer/PM/CTO-style agents remain advisory-only.
- **Questions 2-4 of R15 D36 remain open to the owner** (ledger store
  pluggable-vs-core-owned; the advise-only rule; the extension mechanism —
  Kubernetes-native CRDs/webhooks vs in-process Go interfaces vs both).
  R15:160-174 records the reviewer's suggestions (pluggable store with
  core-side verification and git as the default; the advise-only rule as
  written; both mechanisms). They are not yet owner-confirmed, so this ADR
  does not record them as decided.
- **The acceptance (R15:176-190)**: the builder writes the ADR (this one)
  and updates `docs/PLAN.md` to express the post-Phase-1 phases as core +
  extension points + workflow definitions, including a second non-coding
  example workflow definition and the first two external extensions: memory
  via Honcho and gate approvers via GitHub review (R15:183-187). As of this
  ADR, the plan update and the worked example are **not yet done**; they
  follow the owner's answers to questions 2-4.

## Deferred / open (not decided by D36)

1. **The extension mechanism** (R15 Q4, R15:168-173) — CRDs/webhooks,
   in-process Go interfaces, or both. Owner to confirm.
2. **The ledger store** (R15 Q2, R15:163-166) — pluggable store with
   core-side verification vs core-owned. Owner to confirm.
3. **The advisory-agent rule** (R15 Q3, R15:167) — confirm the
   "advise, never approve" contract.
4. **The plan update + second example workflow** (the acceptance, R15:176-190)
   — not yet written.
