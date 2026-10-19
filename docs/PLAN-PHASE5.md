# Next phase: Phase 5 — shared memory (self-learning across Loops)

**Status:** proposed. Docs only; the owner approves this plan before any code
(D65). D58 deferred Phase 5 to after v0.1.0 (`docs/PLAN-BETA.md:125`); v0.1.0
is tagged (`v0.1.2`). This plan takes PLAN.md Phase 5
(`docs/PLAN.md:76-84`) and, per D65, folds in R28 I94 and I98
(`docs/REVIEW-PHASE1-R28.md`).

**What the phase is** (PLAN.md:78-84): self-learning across Loops, per repo.
Shared memory is cluster-local — a PVC mounted into sandboxes, keyed by git
URL, read at Loop start into the workspace at `.coxswain/memory/`; the runner
includes it in its prompt. A **static write policy** (operator code) filters
which lessons are eligible; only then does the **Memory Curator** LLM
(ADR-0002) make the single "worth keeping?" judgment. `cox memory
export`/`import` move a tarball of the memory dir for explicit, manual
cross-cluster sharing — never automatic or distributed.

**Done when** (PLAN.md:85): a second Loop on the same repo starts with the
first Loop's approved lessons in its prompt, and the audit trail shows exactly
which lessons were written and why.

## Why it is safe to take now

- v0.1.0 is released and tagged; the beta acceptance holds.
- The one controlled exception to the "deterministic operator" rule — the
  Memory Curator (ADR-0002) — is bounded: the operator never interprets
  content itself; it runs **static rules** in code and only *asks* a model to
  judge, then *records* the answer.
- I94/I98 are small, review-driven corrections to the delivery path that D65
  folded in here rather than fixing as separate slices.

## Slices (in order; one PR each, TDD per AGENTS.md test norms)

### P5a. Memory PVC + read into the prompt (PLAN.md:80)
- **What:**
  - A per-repo memory PVC, **keyed by the git URL** (the Loop's
    `spec.workspace` remote), named `memory-<url-hash>` in the operator
    namespace (or the Loop namespace — see D66). It is shared across all Loops
    on the same repo and is **not** a per-Loop PVC (contrast the workspace
    PVC and the checkpoint snapshots, which are per-Loop).
  - At Loop start (before Planning), the operator mounts the memory PVC read-
    only into the sandbox and the runner copies its contents into
    `<workspace>/.coxswain/memory/` (the operator-owned result dir, ADR-0004).
    The runner includes `.coxswain/memory/` in its prompt (Planning and
    Implementing prompts both).
  - `cox memory <repo-url>` (already named in the Phase 4 CLI list, PLAN.md:72)
    reads the memory dir from the PVC and prints it — inspection only, no
    writes.
- **Tests:**
  - envtest: a Loop on repo R with no memory PVC gets a PVC created (or an
    existing one reused); the PVC is keyed by git URL (two Loops on the same
    URL share it, two on different URLs get distinct PVCs).
  - envtest: the memory PVC is mounted read-only into the sandbox (the sandbox
    spec's volumeMounts carry `readOnly: true`); the operator does not write to
    it.
  - Runner spec: the prompt includes the contents of `.coxswain/memory/` when
    present, and is unchanged when absent (a fresh repo).
  - Mutation (each must make its named spec FAIL):
    - **skip the key-by-URL**: name the PVC by Loop name instead of git URL.
      The "two Loops on the same URL share it" spec FAILS.
    - **drop the read-only mount**: mount the memory PVC read-write. The
      "mounted read-only" spec FAILS.
- **Kind acceptance:** two Loops on the same repo share one memory PVC; a
  second Loop's prompt (visible via the runner's result or the plan) references
  a lesson the first Loop wrote. The PVC is read-only in the sandbox
  (`kubectl get pod -o yaml` shows `readOnly: true`).
- **Done when:** a second Loop on the same repo starts with the first Loop's
  memory in its prompt, and the PVC is keyed by git URL and read-only.

### P5b. Static write policy (PLAN.md:81)
- **What:**
  - When a Loop terminates in `Succeeded` or `Failed` with reason `Stalled`,
    the operator reads the runner's `result.json.lessons[]` (each lesson is a
    string the runner proposed).
  - A lesson is **eligible** only if **all** of these hold (static rules,
    operator code, deterministic):
    - the Loop ended in `Succeeded` or `Failed:Stalled` (any other terminal
      reason, or a non-terminal phase, is ineligible);
    - `lessons[]` is non-empty;
    - each lesson is ≤500 chars;
    - each lesson references at least one file path or error signature
      (heuristic: a path-shaped token `foo/bar` or a line matching an error
      signature pattern — see D68 for the exact pattern);
    - each lesson passes a secrets regex-scan (no tokens, passwords, or API
      keys — the regex is the same family the proxy's metering scrubber uses);
    - each lesson is new by hash (dedup against the existing memory dir; a
      lesson whose SHA-256 matches an existing lesson is dropped).
  - Eligible lessons are written to the memory PVC (one file per lesson, named
    by its SHA-256). Each write is recorded in the audit trail
    (`status.history[]` + an Event) with the Loop name, the lesson hash, and
    the reason it was accepted (which rules it passed).
  - Ineligible lessons are **not** written and **not** sent to the curator;
    the drop is recorded in the audit trail with the rule that failed.
- **Tests:**
  - envtest: a Succeeded Loop with a qualifying lesson writes it to the memory
    PVC and records it in `status.history[]` with an Event; a Failed:Stalled
    Loop does the same.
  - envtest (each rule, per AGENTS.md "gate specs must fail when the gate is
    disabled"): a lesson that fails exactly one rule (e.g. 501 chars, no file
    path reference, a secret, or a duplicate hash) is **not** written and the
    drop is recorded. The mutation drops that one rule; the spec FAILS.
  - envtest: a non-terminal phase, or a terminal phase with a reason other than
    Succeeded/Failed:Stalled (e.g. Failed:BudgetExceeded), writes nothing.
  - Mutation (each must make its named spec FAIL):
    - **skip the dedup**: write a lesson whose hash matches an existing one.
      The "new by hash" spec FAILS.
    - **skip the secrets scan**: write a lesson containing a fake API key.
      The "passes a secrets regex-scan" spec FAILS.
    - **drop the ≤500-char cap**: write a 600-char lesson. The "≤500 chars"
      spec FAILS.
- **Kind acceptance:** a Succeeded Loop with a qualifying lesson appears in the
  memory PVC after the Loop terminates; `cox memory <repo-url>` shows it; the
  audit trail (an Event or a `status.history[]` entry) names the Loop, the
  lesson hash, and the rule set it passed.
- **Done when:** a qualifying lesson is written to the memory PVC after a
  Succeeded or Failed:Stalled Loop, and each ineligible lesson is dropped with
  a recorded reason.

### P5c. Memory Curator (ADR-0002)
- **What:**
  - Only when **all** static rules (P5b) pass does the operator invoke the
    **Memory Curator** LLM. The curator's single judgment call is "is this
    lesson worth keeping?" The verdict (boolean + one-line reason) is written
    to the audit trail. The curator **never rewrites** lesson text.
  - The curator is the one controlled exception to the deterministic-operator
    rule (ADR-0002): the operator never interprets content itself, but it may
    *ask* a model to and *record* the answer.
  - A `true` verdict writes the lesson to the memory PVC (P5b's write path);
    a `false` verdict drops it and records the curator's one-line reason in
    the audit trail.
  - A stall can seed a **negative lesson** (configurable; default on for
    `Fail`/`Pause`, off for `Continue` — PLAN.md:82). The negative lesson is
    the stall signature (the repeated failure hash), not the agent's text.
  - The curator call is metered like any other model call (the model-proxy
    sidecar reports it; ADR-0009); a curator-call failure is **not** a Loop
    error (the lesson is dropped, recorded as "curator unavailable", and the
    Loop's terminal state is unchanged).
- **Tests:**
  - envtest: a qualifying lesson (P5b rules pass) is sent to the curator; a
    `true` verdict writes it; a `false` verdict drops it with the reason in
    the audit trail.
  - envtest: the curator never rewrites the lesson text (the bytes written to
    the PVC are byte-identical to the runner's `result.json.lessons[]` entry).
  - envtest: a curator-call failure drops the lesson and records
    "curator unavailable"; the Loop's terminal state is unchanged.
  - envtest: a stall (Failed:Stalled) seeds a negative lesson by default; a
    `Continue` stall does not (the default is off).
  - Mutation (each must make its named spec FAIL):
    - **skip the curator**: write the lesson without asking the curator. The
      "only when all rules pass does the operator invoke the curator" spec
      FAILS (the lesson is written on a `false` verdict).
    - **let the curator rewrite**: the curator returns modified text; the
      "never rewrites lesson text" spec FAILS.
- **Kind acceptance:** a Succeeded Loop with a qualifying lesson is written to
  the memory PVC only after the curator returns `true`; the audit trail shows
  the curator's verdict and one-line reason. A `false` verdict is visible in
  the audit trail with the lesson dropped.
- **Done when:** a qualifying lesson reaches the memory PVC only after the
  curator's `true` verdict, and the audit trail shows exactly which lessons
  were written and why (PLAN.md:85).

### P5d. `cox memory export` / `import` (PLAN.md:83)
- **What:**
  - `cox memory export <repo-url> -o file.tar.gz`: read the memory PVC and
    write a tarball of its contents (the memory dir). Explicit, manual,
    operator-initiated.
  - `cox memory import <repo-url> -f file.tar.gz`: read the tarball and write
    its contents to the memory PVC. A duplicate lesson (same SHA-256) is
    skipped (idempotent import).
  - **Never automatic or distributed** (PLAN.md:83). There is no
    cross-cluster replication, no CRDT, no sync loop. The only path between
    clusters is a human copying a tarball.
- **Tests:**
  - envtest: export writes a tarball whose contents are the memory PVC's
    contents (byte-identical, same SHA-256 set).
  - envtest: import writes the tarball's contents to the memory PVC; a
    duplicate is skipped (the PVC's SHA-256 set is unchanged on a re-import).
  - Mutation (each must make its named spec FAIL):
    - **skip the dedup on import**: import a tarball with a lesson already in
      the PVC. The "idempotent import" spec FAILS (the lesson is duplicated).
- **Kind acceptance:** export a repo's memory to a tarball, delete the PVC,
  import the tarball; the PVC's contents are restored (same SHA-256 set).
- **Done when:** `cox memory export` and `import` move the memory dir as a
  tarball, explicitly and manually, with idempotent import.

### P5e. I94 — delivery PR title and commit subject derive from spec.goal
  (R28 I94; folded per D65)
- **What:**
  - The delivery **PR title** and the **commit subject** both derive from
    `spec.goal`: its **first line**, trimmed and capped (about 72 chars),
    prefixed `coxswain: `.
  - **Today** the title is the branch name and the subject is
    `coxswain: implement` (`runner/phase.go:892`, `git commit -m "coxswain:
    implement"`; the deliver script
    `internal/controller/loop_deliver_job.go:1698` (`deliverPushPayloadPython`),
    `"title": os.environ["TITLE_PREFIX"] + "coxswain: " + os.environ["BRANCH"]`
    — the branch is `coxswain/<loop>-<uid8>`, so the title is
    `coxswain: coxswain/<loop>-<uid8>`). A reviewer cannot tell from the PR
    list or `git log` what the change is (R28 I94: the reviewer had to retitle
    #166 before merging).
  - The full goal stays in the **PR body** (the body already carries the plan
    and facts; the goal is the goal — `deliverPushPayloadPython` in
    `internal/controller/loop_deliver_job.go:1698` already builds the body from
    facts + plan; add the goal).
  - The derivation is a small pure function (first line, trim, cap at ~72
    chars, prefix `coxswain: `) shared by the runner (commit subject) and the
    deliver script (PR title) so the two match.
- **Tests:**
  - envtest/runner spec: the PR title and the commit subject are
    `coxswain: <first line of spec.goal, trimmed, capped>`.
  - spec for the derivation (per R28 I94 acceptance: "long, multi-line and
    special-character goals"):
    - a long first line (>72 chars) is capped at 72 and the rest is dropped
      (or elided with `…` — see D67);
    - a multi-line goal uses only the first line (the second line is not in
      the title);
    - a goal with special chars (quotes, `<`, `>`, `&`) is passed through
      (the PR API and git handle them; no HTML-escaping in the title);
    - a goal whose first line is empty or whitespace falls back to
      `coxswain: implement` (the current behaviour) — or is rejected (see D67);
    - a goal that is exactly 72 chars is not capped (boundary: cap at ≥72).
  - Mutation (each must make its named spec FAIL):
    - **skip the first-line extraction**: use the whole goal (multi-line) in
      the title. The "multi-line goal uses only the first line" spec FAILS.
    - **skip the 72-char cap**: a 120-char first line is not capped. The
      "long first line is capped" spec FAILS.
- **Kind acceptance:** a Loop whose `spec.goal` is a multi-line goal delivers a
  PR whose title is `coxswain: <first line>` and whose commit subject is the
  same; the PR body contains the full goal.
- **Done when:** a delivery PR's title and commit subject derive from
  `spec.goal`'s first line (trimmed, ~72 chars, `coxswain: ` prefix), and the
  full goal is in the PR body (R28 I94 acceptance).

### P5f. I98 — Succeeded is held until Delivered is terminal (R28 I98; D64)
- **What:**
  - The Loop **holds `Succeeded`** until the `Delivered` condition is
    **terminal** (True or False/DeliveryFailed). Today `Succeeded` is set when
    verify passes (`internal/controller/loop_verify_job.go:1276-1277`) and
    delivery is a separate, later step — so a user or script that waits for
    `Succeeded` may read the Loop before its PR exists (R28 I98, dogfood5-df5:
    `phase: Succeeded` while `Delivered=False reason=InProgress`).
  - Per D64: a **failed** delivery is still reported as `Succeeded` with
    `Delivered=False` (as today). The change is only that the phase is not
    *observed* as `Succeeded` until delivery reaches a terminal state.
  - Mechanism: introduce a `Succeeded`-pending state (or a `Delivered` gate on
    the phase) such that `status.phase` is set to `Succeeded` only when the
    `Delivered` condition is terminal. The internal phase (DesiredPhase / the
    verify decision) can still record that verify passed; the **observed**
    phase is held until delivery is terminal.
  - Document the chosen behaviour in OPERATOR.md (R28 I98 acceptance):
    `Succeeded` means "verified **and** delivery is terminal" (a failed
    delivery is `Succeeded` + `Delivered=False`).
- **Tests:**
  - envtest: a Loop that passes verify is **not** observed as `Succeeded`
    until the `Delivered` condition is terminal (True or False).
  - envtest: a Loop that passes verify and delivers successfully is observed
    as `Succeeded` with `Delivered=True`.
  - envtest: a Loop that passes verify and **fails** delivery is observed as
    `Succeeded` with `Delivered=False` (D64: a failed delivery is still
    Succeeded).
  - envtest: a user script that waits for `Succeeded` (a `wait` loop on
    `status.phase`) does not return until the PR exists (or delivery failed).
  - Mutation (each must make its named spec FAIL):
    - **set Succeeded at verify**: observe `Succeeded` the moment verify
      passes (the current behaviour). The "not observed as Succeeded until
      Delivered is terminal" spec FAILS.
- **Kind acceptance:** a Loop that passes verify is held (not observed as
  `Succeeded`) while `Delivered=False/InProgress`; when delivery completes
  (True or False), it is observed as `Succeeded`. `kubectl cox describe` shows
  the hold and the `Delivered` condition.
- **Done when:** the phase is held until `Delivered` is terminal, a failed
  delivery is still `Succeeded` + `Delivered=False` (D64), and the behaviour
  is documented in OPERATOR.md (R28 I98 acceptance).

## Explicitly deferred (keep their PLAN.md homes)
- **Phase 6:** the Judge sidecar (D60) — still deferred.
- **Oscillation detector** (PLAN.md Risks) — still flagged and unscheduled.

## Risks

- **Memory pollution.** A bad lesson, once written, biases every future Loop
  on that repo (PLAN.md:125). Mitigations (all in this phase): the static
  rules (P5b), the curator gate (P5c, ADR-0002), dedup (P5b), and `cox memory`
  inspection + deletion (P5d). No automated forgetting yet — a future concern.
- **Prompt injection via lessons.** A lesson is untrusted content the runner
  includes in its prompt (a Loop's own output). A malicious or buggy lesson
  could steer a later Loop. Mitigation: the static rules (P5b) and the
  curator (P5c) filter before a lesson is written; the lesson is **inspection-
  only** until a curator `true` verdict; `cox memory` lets a human delete a
  bad lesson. A lesson is never executable — it is text in a prompt, not a
  script the runner runs.
- **PVC sharing across namespaces.** The memory PVC is keyed by git URL and
  shared across all Loops on the same repo. If Loops on the same repo run in
  different namespaces, the PVC must be reachable from each (a PVC is
  namespace-scoped). See D66 for where the PVC lives and how cross-namespace
  access works (a per-namespace PVC + a shared storage class, or a
  namespace-scoped read). The default is a single namespace (the operator
  namespace); cross-namespace is a documented limitation until D66.
- **RBAC.** The operator writes to the memory PVC (P5b, P5d import) and reads
  it (P5a, P5d export). The sandbox reads it (P5a). The curator call (P5c)
  needs the model-proxy endpoint (ADR-0009, already in place). No new cluster-
  level RBAC is expected (the PVC is in the operator namespace); confirm in
  the P5a PR that the sandbox's read-only mount needs no extra role.
- **I98 phase-hold complexity.** Holding `Succeeded` until `Delivered` is
  terminal (P5f) adds a phase that was previously a single assignment. The
  risk is a Loop that is observed as neither `Succeeded` nor any other phase
  while delivery is in flight (a user sees a "stuck" Loop). Mitigation: the
  hold is **visible** (a `Delivered=False/InProgress` condition, and
  `kubectl cox describe` shows the hold), and the phase is still `Succeeded`
  internally (DesiredPhase) — only the **observed** phase is delayed.

## Questions for the owner

| ID | Question | Recommendation |
|---|---|---|
| D66 | Where does the memory PVC live? A PVC is namespace-scoped, but Loops on the same repo may run in different namespaces. **(a)** One PVC in the operator namespace, accessed read-only by sandboxes in other namespaces (needs a per-namespace read or a shared storage class). **(b)** A per-namespace PVC keyed by (git URL, namespace) — Loops in different namespaces have distinct memory. **(c)** Default to the operator namespace; cross-namespace is a documented limitation for now. | **(c)** for the first cut (single-namespace, documented); revisit (a) if cross-namespace Loops on the same repo are common. Keeps P5a simple; the PVC is read-only in the sandbox either way. |
| D67 | I94 title derivation edge cases: **(a)** how to cap a >72-char first line — truncate at 72 or elide with `…`? **(b)** what to do with a goal whose first line is empty or whitespace — fall back to `coxswain: implement` (today) or reject the Loop? | **(a)** elide with `…` (the reviewer sees the title was capped, not silently cut). **(b)** fall back to `coxswain: implement` (a Loop with a blank first line still delivers; rejecting a Loop for a cosmetic goal format is too harsh). |
| D68 | P5b "references a file path or error signature" — what is the exact heuristic? **(a)** a path-shaped token (`foo/bar`, `/abs/path`, `file.go:12`) **or** a line matching an error-signature regex (e.g. `panic:`, `error:`, `fatal:`, `Traceback`, `Exception`). **(b)** a stricter rule: must contain a path-shaped token **and** an error signature. | **(a)** (either). A lesson that names a file path is useful even without an error signature; a lesson that names an error signature is useful even without a path. The `and` rule (b) would drop too many valid lessons. |
| D69 | P5f (I98) mechanism: how to hold `Succeeded` until `Delivered` is terminal? **(a)** a new internal phase `Verifying`→`Succeeded` that is observed only when `Delivered` is terminal (the verify decision sets DesiredPhase=Succeeded, the observed phase is gated). **(b)** keep the phase `Succeeded` at verify but add a `Delivered` condition that users/scripts must also check (documented; no phase change). **(c)** a new observed phase `Succeeded` that is set only after delivery (the verify phase is `Verifying`, a new phase). | **(a)** for the first cut (the observed phase is gated on `Delivered` being terminal; the internal DesiredPhase is still `Succeeded` at verify). Keeps the phase machine small and the hold visible. (b) is the "document, don't change" option (R28 I98's alternative) — acceptable if the owner prefers no phase change. (c) adds a phase and is heavier. |
| D70 | P5c curator: is the curator call **required** (a lesson is written only on a `true` verdict) or **best-effort** (a curator-call failure writes the lesson anyway, recorded as "curator unavailable")? ADR-0002 says the verdict is recorded; it does not say a failure blocks the write. | **Required** (a lesson is written only on a `true` verdict; a curator-call failure drops the lesson, recorded as "curator unavailable", and the Loop's terminal state is unchanged). This keeps the curator the gate it is meant to be (ADR-0002: the operator never interprets content itself, but it may *ask* and *record* — a failure to ask is a failure to gate). |
