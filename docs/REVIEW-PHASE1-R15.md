# Phase 1 review, round 15: long-term direction (a general workflow orchestrator)

Design round, 2026-09-28, since tag `review/phase1-r14`. It records the
owner's long-term goal and the gap analysis behind it, so later slices and
ADRs can be judged against it. There are no code findings in this round.

---

## P1: Owner decision

### D36. coxswain's long-term shape: a general orchestrator for gated, isolated agent workflows

- [x] Decided (owner, 2026-09-28): **option (d), an extensible core** (below)
- [ ] ADR written

**Owner decision:**
- **Not SDLC-specific.** coxswain orchestrates **any** workflow that needs
  this kind of orchestration: stages carried out by untrusted agents in
  isolation, human or evidence gates between stages, an approval record, and
  a result handed back. The AI-native SDLC is the first and motivating
  **workflow definition**, not the product's shape. Consistent with the
  earlier owner direction that coxswain must not assume coding agents.
- **Every capability via extensions.** The core defines the extension points,
  contracts and trust boundaries; implementations plug in. Example: memory
  could be Honcho.
- **Simple defaults, solid externals.** Each extension point ships a simple
  built-in default so coxswain works end-to-end with nothing extra installed;
  a solid external implementation plugs in through the same contract.

**What's generic in the core vs. what the SDLC brings:**
- **Generic core:** the workflow/stage graph, gates and approvals, the ledger,
  isolated agent execution (sandbox, proxies, NetworkPolicy, eBPF policy),
  operator-held evidence and checks, the audit stream, and the extension
  contract.
- **The SDLC as one workflow definition:** its stages (intent → spec → plan
  → build → test → deploy → maintain), its artifacts (a repo, a PR), its
  checks (tests, tamper detection on protected paths) and its deliverable (a
  PR or release).
- **Other workflows** plug in their own stages, checks and deliverables: e.g.
  research/report pipelines, data processing, infra changes, document review.
  None of them touch the core.

**Implication for today's code:** Loop/Sandbox/proxies/gates are already
largely workflow-agnostic. The coding-specific parts are the plan →
implement → verify phase names, the repo/PR assumptions and the
git-specific tamper checks. They should end up behind the workflow-definition
and "deliver" extension points, not in the core API. No change is needed
now; it's a constraint on new slices (don't bake more coding assumptions into
the core API).

**Context:** the owner's long-term plan is for coxswain to implement the
AI-native SDLC as packaged in
[bashebr/ai-native-sdlc](https://github.com/bashebr/ai-native-sdlc): six
stages, Plan → Design → Build → Test → Deploy → Maintain, with a human
approval gate at every handoff. That repo is a Claude Code / Codex skill (templates,
prompts and a few scripts in a git repo), not a platform. Its core mechanisms:

- **Stages and artifacts:** each stage commits an artifact (intent → spec →
  plan → code/PR → eval results → release). Maintain writes findings back as
  a new intent.
- **Gates and a ledger:** every handoff is approved by a human and recorded in
  a hash-chained, append-only gate ledger. The workflow only advances when a
  matching ledger record exists.
- **Enforced vs. convention:** a production-deploy gate, a plan-vs-code sync
  check and ledger integrity are enforced. Most other rules are prompt
  conventions.
- **Evals:** 20–50 real tasks with machine-checkable pass conditions, re-run
  whenever agent configuration changes and after incidents.
- **Maintain bands:** monitoring thresholds that escalate from logging, to an
  agent diagnosis, to a proposed fix or runbook through the gates.
- **Optional agent org:** PM/CTO/engineer/reviewer agents with a human "CEO",
  peer review between agents, and intake from issues, forms and email.

**Gap analysis (capability level):**

| Capability | coxswain today |
|---|---|
| Isolated, zero-credential agent execution | **Have.** Sandbox, model proxy, egress proxy (I42), NetworkPolicies, eBPF policy. |
| Verification the agent can't fake | **Have.** Operator-run checks, pinned commits, tamper detection (ADR-0004/0005). |
| Plan approval gate | **Partly.** Designed (hash-pinned `PLAN.md`), not built. |
| Gates at every stage (intent, spec, merge, release) | **Missing.** Only the plan gate is designed. |
| Tamper-evident approval record | **Partly.** Loop status and events exist, but they aren't hash-chained and are lost with the Loop. |
| Multi-stage workflow (intent → … → maintain) | **Missing.** A Loop is one plan → implement → verify run; stages need a layer above Loops. |
| Intake (issues, forms, email) | **Missing.** |
| Multiple agent roles and peer review | **Missing.** One runner per Loop; a judge is planned. |
| Eval harness for agent configuration | **Missing.** |
| Deploy and production gate | **Missing.** coxswain stops at a PR (itself not built yet). |
| Maintain / incident loop | **Missing.** |
| Lessons carried across runs | **Partly.** Shared memory with a curator is planned for a later phase. |

**Reviewer's read:** coxswain already has the two hardest pieces, a
trustworthy **Build** stage (isolation) and a trustworthy **Test** stage
(evidence the agent can't fake). Everything missing is orchestration around
them: stages, gates, a ledger, intake, deploy and monitoring.

**Options:**
- **(a) Build/Test engine inside an external SDLC (recommended as the
  near-term shape):** coxswain stays focused on secure, verifiable Loops and
  exposes clean hooks. Something outside coxswain (the owner's hermes/kanban
  flow, or the ai-native-sdlc skill itself) drives stages and gates:
  - a Loop starts from an approved plan or spec;
  - approvals are recorded against a Loop;
  - the evidence and PR are emitted for downstream gates.
- **(b) The whole lifecycle in coxswain:** add a Workflow-level resource
  above Loops (the stages and graph), a gate/approval model with a durable
  ledger, intake, an eval harness, a deploy/production gate and a maintain
  loop. This is effectively a second product on top of the current one.
- **(c) Staged:** (a) now, with (b) later, keeping the Loop contract stable so
  a future Workflow layer can drive Loops without rework.
- **(d) Extensible core (CHOSEN):** coxswain defines an **extension point**
  for every capability in the gap table, and each is implemented by a
  pluggable extension. The core keeps what must be trusted: isolation, the
  evidence gates and the ledger's integrity. Extensions supply everything
  else. Candidate extension points, one per capability:

  | Extension point | What it supplies | Built-in default (ships with coxswain) | Solid external implementations |
  |---|---|---|---|
  | Memory | lessons and context carried across runs | notes file in the repo (per-repo `.coxswain/memory.md`), read-only to the agent, curated by the operator | Honcho |
  | Intake | turning outside demand into work items | `kubectl apply` a Loop, or a label on a GitHub issue | forms, email, hermes kanban |
  | Workflow definition | the stages, gates and checks of a workflow (the SDLC is one) | a simple linear graph format, shipped with an **SDLC example definition** | the ai-native-sdlc skill, hermes, Argo Workflows |
  | Gate approvers | who may approve a gate, and how they're notified | an annotation or `kubectl coxswain approve` by a human with RBAC | GitHub review, Slack |
  | Ledger store | where approvals are recorded | a hash-chained file committed to git (core verifies the chain) | an external append-only log |
  | Agent roles | extra advisory agents (reviewer, PM, judge) | one runner, no extra roles | any runner image (advisory only) |
  | Checks / evals | stage checks (evidence) and the harness that scores agent configurations | run a command in an isolated verify pod and report pass/fail (today's verify); eval = a list of such runs | ai-native-sdlc evals, custom suites |
  | Deliver | where a workflow's result goes (a PR, a release, a report, a dataset) behind a gate | open a PR (the SDLC case) or write an artifact | Argo CD/Flux for deploys, object stores, ticket systems |
  | Maintain / monitoring | signals that open new Loops | a webhook that turns an alert into a new Loop | Prometheus/Alertmanager with bands |
  | Enforcement engine | the eBPF policy engine (already a seam: C6b `Enforcer`) | KubeArmor (today) | Tetragon |
  | Model proxy | the credential-holding model gateway (already a seam: D33) | the stand-in forwarder (today) | LiteLLM, a vendor gateway |

  **Every extension point ships a simple built-in default**, so coxswain
  works end-to-end with nothing extra installed. A solid external
  implementation replaces the default through the same contract. The
  defaults stay deliberately small, since they're the reference
  implementation of each contract, not a competitor to the external tools.

  Two of these seams already exist (the enforcement `Enforcer` interface and
  the model proxy image), which shows the pattern works.

**Invariants any option must keep** (ADR-0004/0006/0007). Under (d) these
become the **extension contract**; an extension that can't meet them can't be
plugged in:
- agents never decide gates, and approvals come from humans or operator-held
  evidence, never from an agent's claim. **Extensions may advise or supply
  data; the decision stays in the core.**
- the zero-credential agent and proxy boundaries stay. **Extensions run
  outside the agent pod, and their credentials never reach the agent.** An
  extension the agent must talk to (e.g. memory) is reached through an
  operator-owned, policy-enforced path, like the model and egress proxies,
  and never via a secret mounted into the agent.
- **anything an extension returns into the agent (memory, intake text,
  review comments) is untrusted input.** Something the agent writes to memory
  must not be able to rewrite gates, policy or another Loop's context: memory
  is scoped per Loop, repo or tenant, and writes are audited (the Q4 audit
  stream).
- **fail closed:** a gate whose approver or ledger extension is unavailable
  stays closed, and an enforcement extension that can't prove it's enforcing
  keeps the sandbox Suspended (as D30 does today).
- any reviewer/PM/CTO-style agents may **advise**, never approve.

**Questions for the owner** (question 1 is answered by (d)):
1. ~~Build/Test engine, or the whole lifecycle?~~ **Answered: an extensible
   core; the whole lifecycle is reachable through extensions.**
2. Is the ledger itself an extension point (the core verifies the hash chain,
   and the store is pluggable), or does the core own one store? The
   reviewer's suggestion is a pluggable store with core-side verification,
   with git as the default.
3. Is the "agents advise, humans or operator evidence decide" rule for
   multi-role agents acceptable? (It's written into the extension contract
   above.)
4. What's the extension mechanism? Kubernetes-native (a CRD per extension
   point, with implementations as controllers or webhooks the operator
   calls), in-process Go interfaces like `Enforcer`, or both (interfaces for
   the trusted core seams, CRDs and webhooks for the rest)? The reviewer's
   suggestion is both.

**Acceptance:** the owner answers questions 2–4. The builder then writes
ADR-0008, "coxswain as a general, extensible orchestrator for gated, isolated
agent workflows", recording:
- the generic core vs. workflow definitions;
- the extension points with their built-in defaults;
- the extension contract (the invariants);
- the mechanism;
- the SDLC as the first shipped workflow definition, plus a second,
  non-coding example definition sketched to prove the core isn't
  SDLC-shaped;
- the first two external extensions: memory via Honcho, and gate approvers
  via GitHub review.

It also updates `docs/PLAN.md` so the phases after Phase 1 are expressed as
core + extension points + workflow definitions.
