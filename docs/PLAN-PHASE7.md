# Next phase: Phase 7 — the Judge, the canary's out-of-band channel, and the v0.2.0 release

**Status:** DRAFT for the owner (reviewer, 2026-11-09). Nothing here is
approved. The open questions are D122–D127 under Owner decisions.

**What carries in:**
- **D95** (owner, 2026-11-04): the Judge sidecar (`docs/PLAN.md:90`, D60) is deferred to Phase 7.
- **D97** (owner, 2026-11-04): the canary's out-of-band channel (F60's open limit, R45 I199) goes to Phase 7, **with a design note first**.
- Phase 6 is merged except **C1** (multi-arch images), which waits on the owner's push. Nothing since v0.1.3 is released.

**What the phase is:** two design-first items and one release.
1. **The Judge.** An optional model verdict after the acceptance checks pass. It asks the question ADR-0005 left open for it (`docs/adr/0005-operator-verifies-via-isolated-job.md:258`): does this change game the tests? Like the Memory Curator (ADR-0002), it is a rule-bounded exception to the deterministic operator, so an ADR comes before any code.
2. **The canary channel.** A design note that either finds a channel the agent's code cannot read, or shows that none exists inside the test process and records the limit as permanent.
3. **v0.2.0.** Release what Phase 6 built: ClusterAgentPolicy, learn mode and `suggest`, the status VAP, `cox retry`, node placement and multi-arch (C1).

**Done when:**
- v0.2.0 is tagged, with linux/amd64 and linux/arm64 images and upgrade notes.
- ADR-0011 (the Judge) is accepted. A Loop with `spec.verify.judge` set records the Judge's verdict in its status and the PR. A Loop without it behaves exactly as today.
- The canary design note is merged and D125 is decided. If a channel exists, the #371 reviewer probe (read the token file, print it, exit 1) ends non-`Succeeded` on kind.

**Naming:** `docs/PLAN.md:95` calls Phase 7 "Hardening and release". Most of that list is built: the metrics (`coxswain_*` in `internal/controller/loop_controller.go:93-151`), the Helm chart, the docs and the example Loops (`config/samples/`). What remains of it is the release (R1) and the CNCF landscape check (`docs/PLAN.md` "Before Phase 0"), which this plan absorbs.

## Why it is safe to take now

- The review backlog is clear (R53). The open boxes are slip records and the homelab items (I206, I213, I211/C1).
- Neither design item changes the phase machine until its ADR or note is accepted. Both start as docs-only PRs.
- The Judge is opt-in per Loop (`spec.verify.judge`). It is never the only gate (`CONTEXT.md` "Judge"; `docs/PLAN.md:90`), so a Loop that does not set it cannot change behaviour.

## Slices (in order; one PR each, TDD per the AGENTS.md test norms)

### R1. v0.2.0 (release; owner tags)
- **Why first:** Phase 6 is unreleased, and nothing in J or K depends on it.
- **Content:** `docs/RELEASE-NOTES-next.md` becomes `RELEASE-NOTES-v0.2.0.md`, with an **Upgrade** section that covers:
  - the status VAP (P6c), which rejects non-manager status writes;
  - learn mode's ConfigMap and its RBAC (none new);
  - the D84b rollback re-verify.
- **Proof:**
  - the pre-tag smoke on kind, with the sample's model fields set (the v0.1.x lesson);
  - an upgrade from v0.1.3 on kind-ref (scripted, restored), with one Loop living across the upgrade;
  - the arm64 manifests, once C1 is pushed.
- **Blocks on:** C1 (the owner's push) for arm64; D126 for the version number.

### J0. ADR-0011: the Judge (docs only)
The ADR fixes six things:
- **Where it runs.** A **separate Job** after the acceptance checks pass. It is not a container in the sandbox pod, because the agent's pod is untrusted (ADR-0006). It sees the agent's code only as text.
- **Input.**
  - the goal, PLAN.md and the diff `baseCommit..verifiedCommit` (bounded; the cap is stated);
  - the check names and exit codes;
  - nothing from the agent's `result.json` beyond what the operator already reads.
- **Output.** A boolean, a score from 0 to 100, and a one-line reason. These go into `status.judge` and a `JudgeVerdict` condition, plus a history note and an Event. The operator never parses free text: the shape is fixed and anything else is "unavailable".
- **Effect.** This is D122. The recommendation is advisory: a negative verdict keeps the PR a draft and is shown in its description. The verdict never changes `Succeeded`.
- **Model and budget** (D123): the call goes through the Loop's model proxy (metered, ADR-0009), with its own token cap.
- **Failure** (D124): an error, timeout or malformed answer gives `JudgeVerdict=Unknown`, reason `JudgeUnavailable`, which surfaces per R26 I90 and is never read as a pass.
- **Prompt injection.** The diff is agent-authored and can address the Judge. The ADR states the bound: under D122's advisory effect, a steered Judge can at worst mislabel a Loop the checks already passed, or keep its PR a draft.
- **Acceptance:** the ADR is merged with D122–D124 recorded.

### J1. The Judge Job (opt-in, advisory)
- **API:** `spec.verify.judge: {enabled, maxTokens}`, never defaulted. Run `make manifests`, `make generate`, `hack/chart-crd-sync.sh`.
- **Operator:** after a verify passes and before Succeeded is recorded, create the Judge Job (owned by the Loop, its NetworkPolicy reaching only the model proxy) and read its termination message.
- **Specs** (envtest):
  - the in-progress states: no decision while the Job runs (the I49 norm);
  - pass, fail and malformed each record the right condition;
  - a Loop without `judge` never creates the Job;
  - a mutation that ignores `enabled` must fail a spec.
- **Kind:**
  - one Loop with a planted test-gaming diff, where the Judge says false and the PR stays a draft;
  - one honest Loop, where the Judge says true;
  - `can-i` and forbidden 0 for every new rule.
- **Real-output norm (R49 I220):** the parser's testdata is a captured real model reply, byte for byte.

### J2. The Judge as a gate (only if D122 = gate)
A false verdict iterates the Loop, with the Judge's reason fed forward like a failed check. It counts toward the stall detector. This slice is dropped if D122 stays advisory.

### K0. The canary channel: design note (docs only; D97)
- **The question:** can the expected token, or the fact that the forced `t.Fatal` fired, reach the operator over a channel that code running inside the test binary cannot read or forge?
- **Candidates the note must evaluate,** each against the #371 probe:
  - a token served once over a socket by a trusted sidecar;
  - reading the test's exit through `test2json` in a separate process;
  - running the canary in a binary built without the agent's package;
  - comparing against a run of the base tree.
- **For each candidate,** the note shows a concrete forge, or why none exists, with a harness that runs the real script (the AGENTS.md hack-script norm).
- **Acceptance:** the note is merged, and D125 is decided on what it shows.

### K1. The canary channel (only if K0 finds one)
- **Fix:** the chosen channel.
- **Acceptance:** the #371 probe as a fixture ends non-`Succeeded` on kind, and the honest-canary path still passes.

## Explicitly deferred (keep their homes)
- **The oscillation detector** (D96): unchanged.
- **The per-node CNI probe** (D80(b)): unchanged, until multi-node clusters are a support target.
- **Agent roles** (ADR-0010 "Agent roles (advisory)"): the Judge is a single verdict, not a role system.
- **VerifySuspicious** (ADR-0005's advisory diff scan): designed, not built. J0 should say whether the Judge replaces it or sits beside it (the D127 question).

## Risks
- **A second LLM surface.** ADR-0002's boundary ("the operator never interprets content itself, but it may ask a model to and record the answer") must hold. The verdict's shape is fixed and the operator never parses prose.
- **Cost.** Every passing Loop makes one extra model call. The call is opt-in, capped and metered.
- **The canary may have no fix.** If the note shows that every in-process channel is forgeable, the right outcome is a permanent, documented limit, not a weaker check.

## Owner decisions

| ID | Question | Recommendation |
|---|---|---|
| D122 | **The Judge's effect:** advisory (a negative verdict keeps the PR a draft and is recorded) or a gate (`Succeeded` also needs a true verdict)? | **Advisory.** It keeps "never the only gate" trivially true, and it bounds what a prompt-injected Judge can do. A gate can come later as J2. |
| D123 | **The Judge's model:** the Loop's model via its model proxy (metered, with its own cap), or a separately configured model? | **The Loop's model proxy.** One metered path (ADR-0009) and no new credential. A separate model can be a later field. |
| D124 | **The Judge unavailable:** record `Unknown` and proceed, or hold the Loop? | **Record `Unknown` and proceed** (advisory), with an Event per R26 I90. Holding would make an optional check block success. |
| D125 | **After K0:** build the channel the note finds, or accept the limit permanently? | Decide on the note's evidence. Until then, D94's accepted limit stands. |
| D126 | **The release version:** v0.2.0, or v0.1.4? | **v0.2.0.** There are new CRD fields (auditOnly, ClusterAgentPolicy) and a new admission policy that rejects writes v0.1.3 allowed. |
| D127 | **VerifySuspicious:** does the Judge replace ADR-0005's advisory diff scan, or do both land? | Decide in J0. The recommendation leans toward the Judge only, to keep one advisory surface. |

## Estimate
- R1 is about a day of release work, plus the owner's C1 push and tag.
- J0 and K0 are docs and can run in parallel: a day each, plus review.
- J1 is 2–3 days: API, Job, conditions and the kind proof with a real model reply.
- K1 (if any) is 1–2 days.
- **Total:** about 5–7 builder days, or 2–3 weeks of wall-clock with review rounds and owner decisions.
