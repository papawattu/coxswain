# Runner reports progress via the result file only; no Loop-status RBAC

The Phase 1 plan (settled in the design session) said the phase channel was the
Loop **status subresource**: the runner patches `loop.Status.ObservedPhase` via
the API, and the operator reads it. The Phase 0 review (`docs/REVIEW-PHASE0.md`,
D1) flagged this and we are reverting.

## Decision

The runner **writes `result.json` only**. It gets **no Kubernetes credentials**
for Loop objects (the sandbox pod runs with `automountServiceAccountToken: false`,
or a no-permission ServiceAccount). The operator is the only component that
reads and writes Loop `status`.

The phase channel, precisely:
- **Operator → runner:** the operator communicates `desiredPhase` by writing it
  to a file on the workspace (`.coxswain/desired-phase`) or by passing it as an
  argument when it `exec`s into the sandbox. The operator is the writer; the
  runner never writes Loop status.
- **Runner → operator:** the runner does the work and writes `result.json`
  (status, summary, filesChanged, verificationNotes, lessons, and — in Phase 1 —
  the verify outcome + reported file hashes). The operator reads `result.json`
  (via `kubectl exec cat` or a shared PVC, per ADR-0005/D2) and **it** updates
  `loop.Status.ObservedPhase`, `loop.Status.Iteration`, `loop.Status.History`,
  etc. from what the result file reported.

`loop.Status.ObservedPhase` therefore remains a field, but it is the
**operator's record of what the runner reported**, written by the operator — not
a value the runner writes. This keeps the field's meaning while removing the
runner's write access to it.

## Why

1. **A runner with a shell that can patch its own Loop status can lie about its
   own progress.** The model (or a prompt-injected shell command) could set
   `observedPhase=Verifying` without running verify, or bump `iteration`, or
   write a bogus `plan.hash`. That breaks the two properties the whole
   architecture is built on: the operator is a **deterministic state machine**
   that acts on *evidence*, and the **audit trail** is trustworthy. A
   self-reporting agent is neither.
2. **It contradicts CONTEXT.md's own settled contract.** "Result file: the only
   output the operator reads from the runner" and "the runner has no other
   channel to the operator." The status-patching channel *was* another channel.
3. **It is simpler and lighter.** The runner needs no API client, no RBAC, no
   service account. Its only job is: read the prompt/desired-phase, do the work,
   write `result.json`. That is easier to test (the seam stays the in-process
   fake model + result file, exactly as Phase 0) and easier to audit (one file,
   one writer).

## Consequences

- The runner image is credential-free. No `ServiceAccount` token is mounted.
- The operator gains the ability to read `result.json` from the sandbox (D2:
  `pods/exec` to `cat`, a shared PVC the operator also mounts, or a read-only
  sidecar — decided in ADR-0005). This is the operator reading *evidence*,
  which is allowed (the operator may read exit codes, the result file, and
  hashes); it is not the operator *inspecting content to judge quality*.
- The Phase 1 A1 runner seam does **not** include a "patch Loop status" mock.
  The runner test asserts only on `result.json` + workspace files + the fake
  model's request history.

## Considered and rejected

- **Status subresource (the earlier Phase 1 decision).** Cleanest for
  controller-runtime wiring, but gives the agent a write path to its own
  progress. Rejected for reasons 1–3 above.
- **Operator opens the PVC and hashes/reads files directly** (also raised in
  the review, D3). Rejected *as the phase channel* because it couples the
  operator to the workspace filesystem; the operator instead reads the single
  `result.json` the runner produces. (The verify-isolation question — how to
  stop the agent gaming the checks — is D3 and is decided separately; it does
  not require the operator to open arbitrary workspace files.)
