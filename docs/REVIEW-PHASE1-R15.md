# Phase 1 review, round 15: long-term direction (AI-native SDLC)

Design round, 2026-09-28, since tag `review/phase1-r14`. It records the
owner's long-term goal and the gap analysis behind it, so later slices and
ADRs can be judged against it. There are no code findings in this round.

---

## P1: Owner decision

### D36. coxswain and the AI-native SDLC: build engine, or the whole lifecycle?

- [ ] Decided (owner)
- [ ] ADR written

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

**Invariants any option must keep** (ADR-0004/0006/0007):
- agents never decide gates, and approvals come from humans or operator-held
  evidence, never from an agent's claim;
- the zero-credential agent and proxy boundaries stay;
- fail closed;
- any reviewer/PM/CTO-style agents may **advise**, never approve.

**Questions for the owner:**
1. Is coxswain the Build/Test engine inside an SDLC run elsewhere (a, or c),
   or does it own the whole lifecycle (b)?
2. Where does the approval ledger live: the Kubernetes API (CRDs/status, with
   the retention problem after deletion) or git (as the methodology does),
   or git as the record and Kubernetes as the cache?
3. Is the "agents advise, humans or operator evidence decide" rule for
   multi-role agents acceptable?

**Acceptance:** the owner picks an option and answers the three questions.
The builder then writes an ADR (ADR-0008) recording the direction, and
updates `docs/PLAN.md` so the phases after Phase 1 reflect it. That means
naming which of the capabilities above are coxswain's and which belong to the
external SDLC.
