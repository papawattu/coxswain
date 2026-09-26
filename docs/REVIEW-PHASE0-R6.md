# Phase 0 review, round 6 — open issues

Review of `23e7f0d`..`21b6e75` (2026-09-26), since tag `review/phase0-r5`. Docs-only
round: ADR-0005 and `TDD-PLAN-PHASE1.md` revised for round-4 D9–D13 and I9.

Each issue is self-contained so it can be picked up independently. Work
test-first as in `docs/TDD-PLAN.md`: write the failing test at the named seam,
then fix. Tick the box and add the commit hash when done. Issue IDs continue
from earlier rounds so every ID is unique.

Priority: **P1** = fix before Phase 1 starts. **P2** = decide during Phase 1
(design). **P3** = cleanup, batch whenever.

## Verdicts on reviewed commits

| Commit | Issue | Verdict | Follow-ups |
|--------|-------|---------|------------|
| `23e7f0d` | D9, D10, D11, D12, D13, I9 | OK + notes | **D14 (P1, before B3)**, D15, D16 |
| `21b6e75` | round-4 bookkeeping, CONTEXT.md to D10 model | OK | — |

D10 is properly closed: pinned `baseCommit` + protected globs + `git diff
--name-only` catches added/modified/deleted/renamed files, the operator
stays content-free, and B2's test set covers the three gaming cases. D9, D11,
D12 and I9 are closed as asked. D13 is closed on the three-outcome split and
RBAC, but its chosen reporting channel reopens self-reporting in one place —
see D14.

Still open: **I10 (P1, runner shell timeout)**, I11, I12, and I4 (owner
decision).

---

## P1 — Blocking (before Phase 1 slice B3)

### D14. The termination message is written from inside the container that runs the agent's code

- [ ] Decided (amend ADR-0005 "Job reporting contract")

**Where:** `docs/adr/0005-operator-verifies-via-isolated-job.md` "Job
reporting contract (D13)"; `docs/TDD-PLAN-PHASE1.md` B3.

**Problem:** per-check exit codes go to `/dev/termination-log`, written by the
verify harness in the **same container** where the acceptance checks execute
the agent's committed code. Anything the harness can write, that code can
write:
- a test's `init()` / `TestMain` writes `{"checks":[0,0,0]}` to
  `/dev/termination-log`, then kills the harness (if the harness isn't PID 1,
  `kill $PPID` works) or leaves a background writer that rewrites the file
  after the harness does, before the container exits;
- the container's exit code is then whatever PID 1 returns, which the agent's
  code can also influence if it can kill or signal PID 1's children.

This is the round-2 problem at a smaller scale: evidence written by a process
the agent's code shares a container with. The kubelet-recorded **exit code of
a container** is the only per-container value the code inside can't forge
after the fact (it can choose its own exit code, but that's just "the check
failed or passed", which is honest by definition for that check).

**Fix:** make every gate value a container exit code recorded by the kubelet,
with one check per container:
- **Tamper check = first init container** — trusted image, only `git` +
  `git diff --name-only <baseCommit> <verifiedCommit> -- <globs>`; runs
  **before any agent code exists in a running process**. Non-zero ⇒
  `TamperedVerify`. Nothing the agent wrote executes in this container.
- **Each acceptance check = its own container**, running sequentially as the
  remaining init containers, or as one pod per check. Its exit code is
  the check's result. The operator reads `status.initContainerStatuses[i]
  .state.terminated.exitCode` — no termination message, no harness to kill.
- Drop the "reserved *tampered* exit code in the main container": with the
  tamper check in its own container, an agent check can't produce a false
  `TamperedVerify` by exiting with the reserved code.
- Pod logs (`pods/log`) stay the feed-forward channel for the next prompt —
  they're claims-grade input to the model, never a gate.
- Share the checkout between containers via an `emptyDir` populated by a
  clone init container (read-only mount for the check containers if the
  checks don't need to write, otherwise a fresh copy per check).

**Acceptance:** ADR-0005 amended; B3's envtest seam sets
`initContainerStatuses[].state.terminated.exitCode` per check (not a
termination message) and asserts: tamper container non-zero → `TamperedVerify`
with no check container run; all checks 0 → `Succeeded`; check k non-zero →
iterate, with k recorded in history.

---

## P2 — Design

### D15. Resolving refs to SHAs needs git access in the operator

- [ ] Decided

**Where:** ADR-0005 Decision §1 and §3 ("`git ls-remote`, or a tiny resolve
step").

**Problem:** the manager image is distroless (no `git` binary), and
`ls-remote` against a private repo needs the git credential in the
**operator**, in every Loop namespace.

**Fix:** pick one explicitly: (a) `go-git`'s `remote.List` in-process (no
binary; the operator reads `spec.workspace.gitCredentialSecret` — needs
`secrets/get` in Loop namespaces, add it to the RBAC list), or (b) a tiny
"resolve" Job that reports the SHA via its termination message — acceptable
here, unlike D14, because no agent code runs in that container.
Recommendation: (a) for simplicity. Record the choice and the RBAC.

### D16. "Per-language default" globs need a language the operator can't detect

- [ ] Decided

**Where:** ADR-0005 Decision §2; `TDD-PLAN-PHASE1.md` CRD field
`verify.protectedPaths[]`.

**Problem:** the default depends on the repo's language, but the operator is
content-free and never sees the repo. "Files a check command references"
also requires parsing shell commands.

**Fix:** make the default explicit in the spec: `spec.verify.preset: go`
(enum, default `go` for Phase 1) → expands to the Go glob set in the CRD
defaulting or controller; `protectedPaths[]` adds to it; a
`protectedPathsOverride: true` (or empty preset `none`) replaces it. Drop the
"files a check references" heuristic; document that a check calling `make`
should list `Makefile` in `protectedPaths`.

---

## Next round will check

- D14 amendment before any B3 code; D15, D16.
- I10 (P1) runner shell-timeout fix.
- I4 — still needs the owner's decision. Don't pick silently.
