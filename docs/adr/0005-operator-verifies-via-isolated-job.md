# ADR-0005: The operator verifies via a pinned base commit + isolated Job, not from `result.json`

**Date:** 2026-09-26 (Phase 1 design, round-2 review D7; round-4 review D10/D11/D12/D13)
**Supersedes:** D2 and D3 in `docs/REVIEW-PHASE0.md` (how the operator reads
`result.json`, and verify isolation). Amends ADR-0004.
**Revised:** round 4 (D10) replaced the hash-list tamper check with a pinned
`baseCommit` + protected globs; D11 pins the verified commit; D12 isolates the
Job; D13 fixes the Job reporting contract. Round 6 (D14) made every gate value a
kubelet-recorded container exit code, one check per container (the termination
message was forgeable); D15 resolves refs via go-git; D16 makes the protected
preset an explicit `spec.verify.preset` enum. Round 8 (D17) names the residual
in-process test-subversion risk + layered mitigations; D18 pins restart/pod
semantics; D19 makes the checkout read-only into every check container.

## Context

ADR-0004 removed the runner's write access to Loop status (no Loop-status RBAC;
the runner writes `result.json` only). But the Phase 1 plan moved the same
self-reporting into `result.json` itself:

- The runner **computed and reported** sha256 of protected files (baseline at
  iteration start, re-reported after implement).
- The runner **ran** the acceptance checks and **reported** their exit codes.
- The operator made the `TamperedVerify` / `Succeeded` decision
  *deterministically* — but on input the runner supplied.

The runner runs in the same container where the model has a shell. Anything the
runner can write, the model can write. So a model that edits a test file can
also:
1. edit the protected file **and** make the reported hash match the baseline
   (so `TamperedVerify` never fires),
2. report `exitCode: 0` for a check it did not run, or
3. have tampered in iteration N, which becomes iteration N+1's "baseline."

**And (round-4 D10) the hash-list approach itself was gameable even with
operator-computed hashes:** hashing a *list of existing files* misses *added*
files. The agent can pass `go test ./...` without touching any *existing*
protected file by *adding* one — a `zz_test.go` with `func TestMain(m
"testing.M") { os.Exit(0) }`, an `init()` that short-circuits, or a `replace`
in `go.mod` pointing a dependency at a stub. The Job isolation (fresh pod) does
not help because the gaming is in the *committed code the Job runs*. Hashing
also forced the operator to clone the repo and hold file content, against
CONTEXT.md's content-free operator.

The fix is to stop hashing files and instead **compare the protected paths
between two commits** the operator itself pins. The agent has no write path to
that comparison.

## Decision

**`result.json` carries the agent's *claims* — never *evidence* the operator
gates on.**

Claims (operator reads these to build the next prompt / PR description / audit
text, but never as a gate): `summary`, `filesChanged`, `verificationNotes`,
`lessons[]`, `nextIterationPlan`, `needs_input`.

Evidence (the operator gates on these, and obtains them **independently**, not
from `result.json`):
- **The base commit** — resolved by the operator from `spec.workspace.ref` at
  Loop start and pinned in `status.baseCommit`. Immutable for the Loop's life.
- **The verified commit** — resolved by the operator from the Loop branch at
  `Verifying` start and pinned in `status.history[n].verifiedCommit` (D11).
- **The tamper verdict** — `git diff --name-only <baseCommit>
  <verifiedCommit> -- <protectedGlobs>` (run in the Job; non-empty ⇒
  `TamperedVerify`). Catches **added, modified, deleted, and renamed**
  protected files. No stored hashes.
- **The acceptance-check outcome** — per-check exit codes, read from the Job
  pod (D13).

Concretely:

1. **At Loop start** the operator resolves `spec.workspace.ref` to a SHA
   (`git ls-remote`, or a tiny resolve step) and stores it in
   `status.baseCommit`.
2. **Protected paths are globs** (`spec.verify.protectedPaths[]`). When empty,
   a per-language default applies — for Go: `**/*_test.go`, `**/testdata/**`,
   `go.mod`, `go.sum`, plus any files a check command references (`Makefile`
   if a check calls `make`). *Documented:* protecting `go.mod`/`go.sum` means
   the agent can't add dependencies; it is overridable (a Loop can list
   specific paths to narrow or widen).
3. **At `Verifying` start** the operator resolves the Loop branch head to a
   SHA and records `status.history[n].verifiedCommit` (D11 — the Job checks out
   *that* SHA, not the branch name, so a force-push after verify can't slip in).
4. **The operator creates a short-lived verify Job** (a fresh pod, **no shared
   process namespace with the sandbox**, the agent never touches it; isolation
   per D12). **Every gate value is a container exit code recorded by the
   kubelet, one check per container (D14):** the termination message is written
   from inside the container that runs the agent's code, so the agent's
   `init()`/`TestMain` could write `{"checks":[0,0]}` and kill the harness. A
   kubelet-recorded exit code is the only per-container value the code inside
   can't forge after the fact (it can choose its own exit code, but that's just
   "this check passed/failed", which is honest for that check). So:
   - **A clone init container** checks out `verifiedCommit` into an `emptyDir`
     mounted **read-only into every check container (D19, unconditional — the
     "otherwise writable" branch let agent code in check 1 rewrite `*_test.go`
     after the tamper check, so check 2 could compile doctored tests)**. Each
     check container gets its **own writable scratch** (`emptyDir` for `HOME`,
     `GOCACHE`, `GOPATH`, `TMPDIR`). If a check genuinely needs a writable tree,
     it gets a **fresh copy** made by a trusted init step, never the shared one.
   - **The tamper check is the next init container** — a trusted image running
     *only* `git` + `git diff --name-only <baseCommit> <verifiedCommit> --
     <protectedGlobs>`. It runs **before any agent code is a running process**.
     Non-zero ⇒ `TamperedVerify`. Nothing the agent wrote executes in this
     container, and there is no *reserved* exit code to collide with (an agent
     check can't produce a false `TamperedVerify` by exiting with a reserved
     code, because the tamper check is its own container).
   - **Each acceptance check is its own init container** (run sequentially),
     its exit code *is* the check's result.
5. **The operator reads `status.initContainerStatuses[i].state.terminated.exitCode`
   from the API** and decides:
   - tamper container non-zero → `TamperedVerify` (terminal), **no check
     container runs**;
   - all check containers exit 0 → `Succeeded` (record the verified SHA);
   - check *k* exits non-zero → re-plan/implement (record *k* in history; feed
     the pod **log** into the next prompt);
   - the pod itself errored (crash, missing status) → `VerifyError` (re-run
     once, then terminal — CONTEXT.md "Failure reason").
   **Pod logs (`pods/log`) are the feed-forward channel for the next prompt —
   claims-grade input to the model, never a gate.**
6. **`result.json` is read by the operator only for the claims** (via `pods/exec`
   `cat`; see I9). It is never the input to a gate decision, and it has no
   verify-evidence fields.

### How the operator reads `result.json` (resolves D2)

The operator `exec`s `cat /workspace/.coxswain/result.json` into the sandbox
pod. The Job's *evidence* never touches this path — the Job reports via its own
pod status. **I9 (noted):** `pods/exec` on the operator's ClusterRole is
namespace-wide (it can exec into any pod in any Loop namespace). Acceptable for
Phase 1; revisit in Phase 7 hardening (a read-only sidecar serving
`result.json` would remove the need for `exec` entirely).

### ADR-0004 amendment

ADR-0004 said the operator reads `result.json` and updates status "from what the
result file reported," including "the verify outcome + reported file hashes."
**That clause is withdrawn.** The operator never gates on, and never reads back
as evidence, any verify field the runner wrote. Specifically:

- **The operator never reads `.coxswain/desired-phase` back.** It writes that
  file as a hint to the runner; `status.desiredPhase` (the API field) is the
  only truth. If the two disagree, the operator trusts its own status.
- **`result.json` has no verify-evidence fields.** The verify outcome and the
  tamper verdict do not appear in `result.json` — they come from the Job.
  `verificationNotes` is a *claim* (free text the agent wrote), not evidence.

### Verified-commit binding (D11)

The agent controls the branch. If the iteration SHA came from `result.json` (a
claim), or the branch head were read at a different moment from when the Job
clones, the agent could have commit A verified and commit B end up as the PR
head (force-push after verify). So: the operator resolves the branch head
*itself* at `Verifying` start (`ls-remote`), records it as
`status.history[n].verifiedCommit`, the Job checks out **that SHA**, `Succeeded`
records the verified SHA, and the PR step (Phase 6) must open the PR at exactly
that SHA or fail if the branch head moved.

### Job isolation (D12)

The verify Job executes the agent's committed code (that's what tests do). A
fresh pod stops process/PATH gaming, but the code can still reach the network,
the API server, or credentials mounted into the Job. So the Job gets:
`automountServiceAccountToken: false`, **read-only clone credentials only**
(never the push token), the same `NetworkPolicy` as the sandbox, resource limits
+ `activeDeadlineSeconds`, and the same runtime class as agent-sandbox pods
(e.g. gVisor) if one is configured. (Consider running it as an agent-sandbox
`Sandbox` rather than a raw `Job` so isolation policy lives in one place.)

**Status-write invariant (I29, round-4):** the verify evidence now lives in
`loops/status` (`status.verify.*`), so its integrity depends on nothing but
who may write `loops/status`. The scaffolded `loop_editor_role` and
`loop_admin_role` grant `loops/status` `get` only — today only the manager
role can write it. **Keep it that way:** only the manager role may write
`loops/status`; it holds the verify evidence. A later RBAC edit that widens
`loops/status` write to a non-manager role would let a non-operator forge the
tamper/check exit codes and must be treated as a break of this invariant.

### Gate evidence = kubelet-recorded exit codes, one check per container (D14, supersedes D13)

The D13 draft sent per-check exit codes to `/dev/termination-log`. **That is
forgeable:** the termination message is written from inside the container that
runs the agent's code, so the agent's `init()`/`TestMain` can write
`{"checks":[0,0]}` and kill the harness (or leave a background writer that
rewrites it). The only per-container value the code inside can't forge after
the fact is the **kubelet-recorded container exit code** — the agent can choose
its own exit code, but that's just "this check passed/failed", which is honest
for that check. So every gate value is an exit code, one check per container
(Decision §4-5 above):
- **Tamper check** = a dedicated init container (trusted image, only `git` +
  the glob diff), before any agent code runs. Non-zero ⇒ `TamperedVerify`.
- **Each check** = its own init container; its exit code is the result.
- **No reserved *tampered* exit code** (the tamper check is its own container,
  so an agent check can't trigger a false `TamperedVerify` by exiting with a
  reserved code). **No termination message as a gate value.**
- `pods/log` stays the feed-forward channel for the next prompt (claims-grade,
  never a gate).
- **B3 test note:** envtest has no Job controller, so the B3 test sets
  `initContainerStatuses[].state.terminated.exitCode` per check directly (not a
  termination message) and asserts: tamper container non-zero → `TamperedVerify`
  with no check container run; all checks 0 → `Succeeded`; check *k* non-zero →
  iterate, with *k* recorded in history.

### Restart / pod-selection semantics (D18)

Pin these or the operator reads the wrong pod and breaks "check *k* failed ⇒
iterate":
- pod **`restartPolicy: Never`** (a failing init container is **not** restarted
  in place — with `OnFailure` a flaky/time-dependent check could re-run and pass);
- Job **`backoffLimit: 0`** (no new pods after a failure, so "read
  `initContainerStatuses`" is unambiguous);
- `activeDeadlineSeconds` set (D12).
- The operator reads the **single pod** selected by the Job's `controller-uid`
  label; **zero or >1 pods ⇒ `VerifyError`**.
- `VerifyError`'s "re-run once" creates a **new Job**, never relies on Job
  retries.
- B3 test: a second pod appearing is a `VerifyError`, not a pass.

### Residual risk: in-process test subversion (D17)

The tamper check guarantees the test *definitions* are unchanged. It **cannot**
guarantee the *code under test* doesn't subvert the test process, because that
code runs inside it. Verified with a probe — a non-test, unprotected file:

```go
// add.go (Add is deliberately broken)
func init() { if testing.Testing() { os.Exit(0) } }
```

`go test ./...` prints `ok` and exits 0; `TestAdd` never runs. No protected file
changed, so the glob diff is empty and the check container exits 0 →
`Succeeded`. Variants: `//go:linkname` into `testing`, a fake `--- PASS`
stream, monkeypatching the runner in interpreted languages. Any output-based
detection is forgeable by the same process. So:

- **Acceptance checks are the only *automated* gate. A human reviewing the draft
  PR (Phase 6, draft by default) is the final gate.**
- **Layered cheap mitigations** (none complete alone):
  1. **Negative control (canary).** In the trusted tamper container, generate a
     canary test per tested package (random name, `t.Fatal` with a random
     token) into a *separate copy* of the checkout and run one extra check
     container on it that **must fail** and whose failure must be reported by
     `go test` (non-zero exit). A binary that exits 0 regardless of tests makes
     the canary pass → `TamperedVerify` (or a new `SubvertedVerify` reason).
     Cheap, and the always-exit-0 trick is the obvious one a model stumbles into.
  2. **Static flag on the diff (advisory, not a gate — D21).** A dedicated
     trusted init container (after the tamper check) greps the base→verified
     diff of *non-protected* files for `testing.Testing()`, `os.Exit` inside
     `init`, `//go:linkname`, `flag.Lookup("test.` and reports hits by its
     **termination message** (acceptable — no agent code runs in that container,
     per D15). It **exits 0 either way** so the scan can never block the checks;
     a missing/garbled message is "scan unavailable" (a `Warning` event), not
     clean. A hit records a `VerifySuspicious=True` **condition** (reason
     `SubversionPatternInDiff`) + a history note. It does **not** write `spec`
     (D21: `spec` is user-owned; rewriting it fights GitOps and blurs the audit
     trail) — Phase 6's PR step computes
     `effectiveReady = spec.pr.ready && !VerifySuspicious`.
  3. **Optional (Phase 6):** the Judge sidecar gets the diff and is asked
     specifically whether the change games the tests.
- **Acceptance:** B3 gains a slice for the canary (the probe above as a fixture
  must end non-`Succeeded`) and a slice for the advisory scan (D21: a subversion
  pattern in the diff of a non-protected file records a `VerifySuspicious=True`
  condition + `Warning` event + history note, with `spec` unchanged); CONTEXT.md
  wording updated.

### Resolving refs to SHAs (D15)

`ls-remote` needs `git` in the operator, but the manager image is distroless
(no `git` binary), and `ls-remote` against a private repo needs the git
credential in the **operator** in every Loop namespace. **Choice: (a) go-git's
`remote.List` in-process** (no binary; the operator reads
`spec.workspace.gitCredentialSecret`). **RBAC: add `secrets/get` in the Loop's
namespace.** (Option (b), a tiny "resolve" Job that reports the SHA via its
termination message, is acceptable *here* — unlike D14 — because no agent code
runs in that container; but (a) is simpler and we choose it.)

### Protected-path preset (D16)

"Per-language default" globs need a language the content-free operator can't
detect (it never sees the repo), and "files a check command references" needs
shell parsing. So the preset is **explicit in the spec**: `spec.verify.preset`
(enum, default `go` for Phase 1) expands to the language's glob set (the Go set
for `go`); `spec.verify.protectedPaths[]` **adds** to it; `protectedPathsOverride:
true` (or `preset: none`) **replaces** it. Drop the "files a check references"
heuristic; document that a check calling `make` should list `Makefile` in
`protectedPaths`.

## Minimum acceptable alternative (recorded as accepted risk)

If the Job is too heavy for the Phase 1 schedule, the **minimum** variant is:
the operator `exec`s a **fixed verify command** directly into the sandbox (no
runner involvement) and takes the tamper check as a `git diff --name-only`
between the pinned base and verified commit. This still closes the
"runner reports the exit code" and "added protected file" gaps (the diff is
operator-pinned), but **leaves open** the PATH-shadowing / persistent background
process / `go.mod replace` *runtime* gaming (the checks run in the agent's
sandbox). We choose the Job; this is the fallback only if the Job proves
impractical, and it is recorded here so the residual risk is explicit.

## Why

1. **The operator's determinism is real only if its input is evidence it
   obtained, not a claim the agent made.** The pinned commits + the Job's
   status satisfy that; `result.json` verify fields do not.
2. **The base-commit + glob diff closes the gaps the reviews named** — edited
   test (modified), *added* test with `TestMain` short-circuit (added), `go.mod
   replace` (modified), deleted/renamed protected files (D10) — by
   construction, and needs **no stored hashes** and **no operator cloning**.
3. **It keeps the runner simple** (ADR-0004 reason 3): the runner only writes
   `result.json` (claims) and pushes its commit; it never computes or reports
   verify evidence.
4. **It is auditable**: every gate decision traces to two pinned SHAs
   (`baseCommit`, `verifiedCommit`) and a Job object the operator created (with
   a full pod log + termination message) — all retrievable from the cluster, not
   from the agent's word.

## Consequences

- The operator gains `jobs` create/delete/list/get, `pods/get` (Job pod status,
  `initContainerStatuses` exit codes — D14), `pods/log` (feed-forward, D14),
  `pods/exec` (read claims), and `secrets/get` (go-git ref resolution, D15) RBAC
  in the Loop's namespace. `pods/exec` is namespace-wide (I9); revisit in Phase 7.
- A verify Job image is needed (a small image with a clone init container,
  a tamper-check init container, and one check init container per check — D14).
  Phase 1 builds a minimal one. It runs isolated (D12): no SA token, read-only
  clone creds, sandbox NetworkPolicy, limits, runtime class; restart semantics
  pinned (D18: `restartPolicy: Never`, `backoffLimit: 0`, single pod by
  `controller-uid`). The checkout is mounted read-only into every check
  container (D19).
- `loop.Status` changes: `verify.baselineHashes` is **removed** and replaced by
  `baseCommit` (string, set at Loop start). `history[]` entries gain
  `verifiedCommit` (string, set at `Verifying` start) and record which checks
  were **not run** (I14: sequential init containers stop at the first failing
  check — the model fixes one check per iteration; record the not-run set so the
  next prompt can target them; revisit with one pod per check if iteration
  counts suffer). The runner writes none of these.
- `spec.verify.acceptanceCheckPaths[]` is **replaced** by
  `spec.verify.protectedPaths[]` + `spec.verify.preset` (enum, default `go`)
  + `spec.verify.protectedPathsOverride` (D16).
- The Phase 1 B-slices change: **B2** (TamperedVerify) becomes "Job/Operator
  compares the base-commit glob diff to the verified commit" (the anti-gaming
  test set is a: edit an existing `*_test.go`; **b: add a new `*_test.go` with
  `TestMain` → `os.Exit(0)`; c: add a `replace` to `go.mod`; each ends
  `Failed:TamperedVerify` with no check run, even when the fake `result.json`
  claims otherwise); **B3** (deterministic verify) becomes "operator reads the
  Job's `initContainerStatuses` exit codes, one check per container" (D14), with
  a canary slice for in-process subversion (D17) and a restart-semantics case
  (D18: a second pod ⇒ `VerifyError`). **A4** (runner runs checks + reports exit
  codes) is **dropped** — the runner does not run or report checks.
