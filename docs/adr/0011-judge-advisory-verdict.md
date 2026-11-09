# ADR-0011: the Judge — an advisory verdict after verify

**Status:** Proposed
**Date:** 2026-11-09
**Extends:** ADR-0002 (LLM curation, rule-gated), ADR-0005 (operator
verifies via an isolated Job), ADR-0009 (metering in the model proxy)
**Supersedes:** ADR-0005's `VerifySuspicious` advisory diff scan (D127,
owner 2026-11-09)
**Origin:** `docs/PLAN-PHASE7.md` J0; owner decisions D122–D127
(`docs/PLAN-PHASE7.md:100-107`), all decided 2026-11-09.

## Context

ADR-0005 left the residual in-process test-subversion risk to a layered
answer: a canary test, an advisory static diff scan (`VerifySuspicious`,
designed but never built), and — "Optional (Phase 6)" — "the Judge sidecar
gets the diff and is asked specifically whether the change games the tests"
(`docs/adr/0005-operator-verifies-via-isolated-job.md:257-259`). Phase 6
never built it; Phase 7 does, as a design-first slice (J0 the ADR, J1 the
Job), because it is a second model surface and must respect ADR-0002's
boundary: the operator never interprets content *itself*, but it may *ask*
a model to and *record* the answer
(`docs/adr/0002-llm-curation-rule-gated.md:7`).

The Judge answers the question the deterministic checks cannot: **does this
change meet the goal without gaming the acceptance?** It runs only after
the checks pass, is opt-in per Loop, and — per the owner (D122) — is
advisory: it can never be the only gate (`CONTEXT.md` "Judge";
`docs/PLAN.md:90`).

The owner's decisions this ADR records (`docs/PLAN-PHASE7.md:100-107`):
D122 advisory (a gate can come later as J2, which is dropped unless
revisited); D123 the Loop's model proxy, with its own cap; D124 record
`Unknown` and proceed; D127 the Judge only — ADR-0005's `VerifySuspicious`
is superseded, so there is one advisory surface, not two.

## Decision

### Where it runs

The Judge runs as a **separate Job**, created by the operator **after the
acceptance checks pass** and before `Succeeded` is recorded. It is owned
by the Loop (owner reference, so deletion of the Loop removes it).

It is **not a container in the sandbox pod**. The agent's pod is untrusted
(ADR-0006): a sidecar there sees the agent's filesystem and shares its
trust boundary, and a prompt-injected agent could read or steer it. The
Judge Job sees the agent's code only as text (the diff handed to it) and
executes no agent code.

Its NetworkPolicy denies all egress except the Loop's model proxy and DNS
to resolve it — the same fence pattern as the memory-writer Job
(`internal/controller/loop_memory_writer_job.go:208-210`). No other
egress: the Judge must not phone home a verdict the operator could not
see.

### Input

The Judge is given:

- **the goal, PLAN.md, and the diff `baseCommit..verifiedCommit`** — the
  same pinned evidence the operator already holds (ADR-0005). The diff is
  **capped at 256 KiB**; when it exceeds the cap it is **truncated to the
  first 256 KiB of the unified diff and the Judge input is marked
  `truncated: true`** so the verdict is recorded on that fact (a verdict on
  a truncated diff is a verdict on a diff the reviewer can see was
  truncated). The cap keeps the prompt — and its metered cost — bounded by
  the agent's own diff size;
- **the check names and their exit codes** (the D14 gate values the
  operator already read, one per check container);
- nothing from the agent's `result.json` beyond what the operator already
  reads.

The prompt is operator-composed; the agent's text appears only inside
quoted, bounded sections of it.

### Output

The Judge's reply has a **fixed shape**: a **boolean**, a **score 0–100**,
and a **one-line reason of at most 200 characters** (the same verdict
pattern as the Memory Curator, ADR-0002: a boolean plus a one-line reason,
plus the score). The Job reports it via its **termination message**, the
channel the verify and memory-writer Jobs already use: the operator reads
the kubelet-recorded termination message through pod `get` (no log
dependency), strictly validates the fixed shape, and records the fields.

Anything not in that shape — a parse failure, a longer reason, a missing
boolean — **counts as unavailable** (below). **The operator never parses
prose**: it validates the fixed shape and records the fields, never
interprets free text. That is ADR-0002's boundary applied to the second
model surface.

The verdict is recorded in four places:

- `status.judge` (`{verdict, score, reason, truncated, at}`);
- a **`JudgeVerdict`** condition (`True`/`False`/`Unknown`);
- a history note in `status.history[]`;
- an Event (one `Normal` event on a recorded verdict; see Unavailable).

### Effect (D122, advisory)

**A false verdict delivers the PR as a draft whatever `spec.delivery.draft`
says, and the verdict is shown in the PR description.** Nothing more: it
**never changes `Succeeded`** — the Loop succeeded because the acceptance
checks passed, and the Judge cannot rescind that — and it **never
iterates**: the verdict is not fed back as a failed check and is not
counted by the stall detector. A **true** or **Unknown** verdict leaves
`spec.delivery.draft` exactly as it is (`api/v1alpha1/loop_delivery_types.go:102`).
On Gitea — which has no draft PRs — the draft marker is the `"WIP: "`
title prefix instead (`internal/controller/loop_deliver_job.go:167-170`).
(ADR-0005's `VerifySuspicious` computed
`effectiveReady = spec.pr.ready && ! VerifySuspicious` against the field
PLAN.md designed; the Judge's advisory line is the same idea — the verdict
affects the draft computation, not the phase machine.)

### Model (D123)

The call goes **through the Loop's model proxy**, the same metered path
the agent and the Memory Curator use (`internal/controller/loop_memory_writer_job.go:381-392`;
ADR-0009). It carries **its own token cap**, `spec.verify.judge.maxTokens`,
enforced at the request: the Judge sets the model request's `max_tokens`
to the cap (bounding the output), and the 256 KiB diff cap bounds the
input; the proxy still meters the call like every other Loop token. **No
new credential**:
the proxy already holds the model key, or the keyless vLLM passthrough
applies, exactly as for the curator.

### Unavailable (D124)

An **error, timeout, or malformed reply** gives
`JudgeVerdict=Unknown`, reason **`JudgeUnavailable`**, a **`Warning`
Event** (per R26 I90: a fallback must surface its failure before it
degrades), and the **Loop proceeds** — `Succeeded` is recorded as if the
Judge had not run. An unavailable Judge is **never read as a pass** and
never as a fail: `Unknown` is its own third state, distinct from the
boolean, and nothing downstream treats it as either verdict.

### Prompt injection

The diff is **agent-authored and can address the Judge**: the agent knows
its code will be shown to a model. Under D122's advisory effect the bound
is plain, in its two cases:

- **(a) steered to `False` on a good change:** the PR is forced to a draft
  and the verdict and one-line reason are shown in the PR description. That
  is **friction only** — the checks passed and `Succeeded` is recorded;
  the PR's human reviewer sees the (wrong) verdict and can ready the PR.
- **(b) steered to `True` on a gaming change:** **no worse than having no
  Judge.** The checks already passed, the PR is delivered per
  `spec.delivery.draft`, and a human reviewing the PR is the final gate
  (ADR-0005, `docs/adr/0005-operator-verifies-via-isolated-job.md:236-237`).

That bound is exactly why D122 is advisory rather than a gate: as a gate,
a steered Judge could flip `Succeeded` or force an iteration.

### Opt-in

The Judge is enabled by **`spec.verify.judge.enabled`**, **never
defaulted**. A Loop without the field behaves exactly as today: no Judge
Job is created, no `JudgeVerdict` condition, no model call, no change to
the phase machine or the PR draft computation. The cap
(`spec.verify.judge.maxTokens`) is set in the same object.

## Consequences

- A second LLM surface exists (after the Memory Curator). ADR-0002's
  boundary holds by construction: the operator asks, the model answers in
  a fixed shape, the operator records — and never interprets prose.
- Cost: every passing, judge-enabled Loop makes **one** extra model call,
  capped by the diff cap (input) and `maxTokens` (output), and metered
  through the proxy like every other Loop token (ADR-0009).
- `VerifySuspicious` is superseded (D127): the Judge subsumes the static
  diff scan's question ("does this change game the tests?"), and there is
  one advisory surface to reason about, spec, and test — not two that
  could disagree. The scan was designed but never built
  (`docs/PLAN-PHASE7.md:91`), so nothing is lost.
- The PR description is the surface where the advisory verdict lives: a
  human reviewer sees the verdict, the score, the reason, and the
  truncated flag in the same place as the goal and the history.
- The phase machine gains one new in-progress state (the Judge Job
  running) that must record **no decision** until it is terminal — the
  I49 norm. `Succeeded` is still recorded only on the checks.

## Considered and rejected

1. **The Judge as a gate** (D122). Rejected by the owner (2026-11-09).
   Advisory keeps "never the only gate" trivially true and bounds what a
   prompt-injected Judge can do (above); a gate (a false verdict
   iterates, its reason fed forward like a failed check, J2) would let
   the agent's own text flip `Succeeded` or force iterations. J2 is
   dropped unless revisited.
2. **A sidecar in the sandbox pod** (CONTEXT.md's original wording).
   Rejected: the agent's pod is untrusted (ADR-0006); a sidecar there is
   inside the agent's trust boundary and could be read or steered by a
   prompt-injected agent. A separate Job owned by the Loop, with a
   proxy+DNS-only NetworkPolicy, is the pattern already proven by the
   verify and memory-writer Jobs.
3. **A separately configured model** (D123). Rejected by the owner
   (2026-11-09). The Loop's model proxy is the one metered path
   (ADR-0009) and holds the credential; a separate model would mean a new
   credential, a new egress rule, and a second metering surface. A
   separate model can be a later field if the verdicts need an
   independent judge.
4. **Keeping `VerifySuspicious` alongside the Judge** (D127). Rejected by
   the owner (2026-11-09): the Judge only. Two advisory surfaces that
   answer the same question and could disagree would blur the audit trail;
   the static scan was designed but never built, so superseding it costs
   no code. Its one contribution — the `effectiveReady` advisory
   computation — is carried into the Judge's draft-holding effect.
