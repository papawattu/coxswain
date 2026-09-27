# Phase 1 — TDD plan (DRAFT)

Phase 1 is the loop, minimal: the CRD types get fleshed out, the Runner becomes
the in-sandbox **phase driver**, and the controller drives the **reconcile state
machine** over the phase enum. The trust model (protected paths, deterministic
verify) lands here, not in Phase 6.

Read `docs/PLAN.md` "Phase 1" and `docs/adr/0003-one-shot-loop-fork-not-retask.md`
before working this — the Loop is one-shot and the operator is a deterministic
state machine.

## The two surfaces

### A. Runner as phase driver (`runner/`)
The Phase 0 runner ran once and exited. Phase 1 replaces it with a daemon that:
reads `desiredPhase` (the operator writes it to `.coxswain/desired-phase`), does
the work for that phase, and writes `result.json` (which carries the reported
`observedPhase`). Model context survives across phases within an iteration.

> **Channel (ADR-0004):** the runner writes **`result.json` only** and gets **no
> Loop-status RBAC**. It does not patch the Loop's status. The operator reads
> `result.json` and *it* writes `loop.Status.ObservedPhase`/`Iteration`/`History`.
> `observedPhase` in `result.json` is the runner's *report*; the operator turns
> that report into the Loop status. This is what changed from the earlier
> status-subresource decision.

**Candidate seams (to confirm):**
- **A1** — *Phase contract.* The runner reads `desiredPhase` from
  `.coxswain/desired-phase`, does the work, and writes `result.json` (with the
  reported `observedPhase` + the Phase 0 result fields). The seam is the
  **result file** + the workspace + the fake model's request history — never the
  model internals. A fake model drives the phase work. The runner gets **no**
  Loop-status RBAC (ADR-0004); the test asserts only on `result.json` + workspace
  files. (The earlier "patch Loop status" mock is gone.)
- **A2** — *Planning phase.* Given a goal, the runner writes `PLAN.md` at
  `.coxswain/PLAN.md` with a ≤4KB summary, and sets `observedPhase=Planning`
  complete. Seam: the PLAN.md file + result.
- **A3** — *Implementing phase.* The runner runs the model with a shell tool
  (Phase 0 R3 already proved tool exec); here it additionally must write
  `result.json.status` per the phase outcome and `filesChanged[]`. Seam: workspace
  files + result.
- **A4** — *Model context continuity.* Across Planning→Implementing within one
  iteration, the runner keeps the same conversation (the model is called with the
  accumulated message history, not reset each phase). Seam: the fake model's
  request history across a multi-phase run.

> **A4 (old: Verifying phase) is DROPPED (ADR-0005).** The runner does **not**
> run acceptance checks or report exit codes. Verify evidence is obtained by the
> operator via an isolated Job (B3) and base-commit glob diff (B2); the runner's job
> ends at writing `result.json` (claims) and making local commits (the operator
> publishes them, ADR-0006).

> **Agent-agnostic (ADR-0006, revised D26).** The runner is the **
> reference/conformance agent**, not the flagship. Any image that honours the
> contract (reads `.coxswain/` instructions, edits `/workspace`, commits locally,
> writes `result.json`) is a runner. The reference runner calls the model through
> `COX_MODEL_BASE_URL` — in production that is a **localhost proxy sidecar** in
> the sandbox pod that holds the real model key (the agent holds **zero
> credentials**, ADR-0006); in tests it is the fake model. `spec.agent` names the
> agent image + model + `endpointSecretRef` (mounted into the proxy, never the
> agent).

### C. Isolation (ADR-0006, revised D26 — the product)

The isolation is what makes B2/B3's evidence meaningful, so these slices come
**before B3**. They make the sandbox a zero-credential, deny-by-default boundary
and prove it. Each is a red→green seam; the envtest asserts the built Sandbox pod
spec, the e2e runs an "evil agent" image.

**Candidate seams (to confirm):**
- **C1** — *Sandbox pod hardening + `spec.agent`.* *(DONE — see `main`; the
  operator builds the agent container with `spec.agent.image`, no SA token
  automount, runAsNonRoot, drop-all-caps, allowPrivilegeEscalation false,
  seccomp RuntimeDefault, read-only rootfs + `/workspace` + `scratch` emptyDirs,
  and no secret volume in the agent container; envtest seam
  `loop_c1_hardening_test.go`. Round 10 addenda (I34-I38) landed with it: I34
  `spec.agent.env` is literal-only `[]AgentEnvVar` (no `valueFrom`, CEL rejects
  `COX_*` names — `loop_i34_agentenv_test.go`); I35 pins `runAsUser`/`runAsGroup`/
  `fsGroup`=65532 + `HOME`/`TMPDIR` on `/scratch` so the read-only-rootfs agent
  can start and write (the envtest's RED is the `CreateContainerConfigError`
  that only kind catches — see docs/REVIEW-PHASE1-R10.md); I36 sets CPU/memory/
  ephemeral-storage limits; I37 notes the golang stand-in default in the README
  status line + ADR-0006; I38 moves the `+optional` markers to their own lines so
  `make manifests generate` is self-consistent (verified: zero `+optional` in the
  CRD, fields optional).)* The operator builds the Sandbox with `automountServiceAccountToken: false`, `runAsNonRoot`, drop all
caps, `allowPrivilegeEscalation: false`, seccomp `RuntimeDefault`, read-only root
fs + writable `/workspace` + scratch, CPU/mem limits, and a `runtimeClassName`
when the cluster offers one. CRD: `spec.agent { image, model,
endpointSecretRef, env?, egressAllow? }` + a `coxswain-agent-defaults` ConfigMap
for defaults. Seam: envtest — assert the built Sandbox pod spec carries every
hardening field, no SA token automount, and no secret volume mounted into the
**agent** container.
- **C2** — *Model proxy sidecar.* The sandbox pod gains a second container (the
proxy) that holds the model key (mounted **only** into the proxy), injects auth,
forwards only to the configured endpoint, and meters tokens (the Phase 2
metering sidecar, built now as the credential boundary). The agent talks to
`COX_MODEL_BASE_URL=http://localhost:<port>` and holds no key. Split into two
sub-slices (C2a is the pod wiring; C2b is the proxy binary + e2e — the slice's
box stays open until both are done):

  - **C2a — pod wiring (DONE).** The operator adds the proxy container (when
    `spec.agent.endpointSecretRef` is set; with no ref the pod has no proxy,
    no key, no `COX_MODEL_BASE_URL` — P1: never a half-configured proxy) and the
    model-creds Secret volume (read-only file, mounted only into the proxy —
    not env, P2); the agent's env carries `COX_MODEL_BASE_URL=http://localhost:
    8080`; the proxy runs as its own UID (65533), the pod sets
    `shareProcessNamespace: false`, and the proxy image is a `LoopReconciler`
    `ProxyImage` field (not `os.Getenv` in reconcile, P3). Seam: envtest —
    `internal/controller/loop_c2_proxy_test.go` asserts, with the ref set: the
    proxy container exists; the model-creds volume is mounted into the proxy and
    not the agent; the agent's env has `COX_MODEL_BASE_URL` set to the localhost
    proxy; the key is NOT delivered via env; the proxy has its own UID; the pod
    has `shareProcessNamespace: false`; and, with the ref UNSET: no proxy
    container, no model-creds volume, no `COX_MODEL_BASE_URL`, and no container
    references an empty secret name (the kind `InvalidConfiguration` the pod hit
    when the proxy was unconditional). The proxy container is a `sleep infinity`
    stand-in.

  - **C2b — proxy binary + e2e (remaining).** Build the real proxy: read the key
    + endpoint from the mounted file, inject the auth header, forward
    **only** to the configured endpoint (no other host), and meter tokens
    (Phase 2 metering). e2e — the reference runner, with the fake model behind
    the proxy, reaches `result.json` **without any key in the agent env**; the
    agent's only model path is `localhost:8080`. Until C2b lands, C2's box stays
    open.
- **C3** — *NetworkPolicy generated from `AgentPolicy` (ADR-0007 Q6).* A
`NetworkPolicy` on the sandbox is **generated from the Loop's effective
`AgentPolicy`** egress allows (allowed hosts/CIDRs + ports), default-deny:
with no allows the agent egresses nowhere but `localhost`, and the proxy's own
egress to the model endpoint is platform infrastructure (allowed by the
operator, not the user's policy). This is the coarse pod-level layer; the
process-level egress + its audit records come from the eBPF engine (C6). Seam:
envtest — given a set of egress allows, the generated `NetworkPolicy` for the
sandbox allows exactly those and denies the rest; with no policy the default is
deny (agent egresses only localhost).
- **C4** — *Trusted publish step.* After the agent commits locally, a trusted
component outside the agent's control (an operator-created "publish" Job, or a
sidecar sharing only the workspace volume + the push token) pushes the Loop
branch and **pins `status.currentVerify.verifiedCommit` (D11/D27)** to the same
SHA — the agent cannot force-push after verify. The agent holds no push token.
Seam: envtest/e2e — assert the publish Job's pod has the push token and the
agent container does not; after a publish the operator's pinned commit equals
the published SHA; a force-push attempt by the agent is not reflected in the
pinned commit.
- **C5** — *Evil-agent e2e (the isolation proof).* An "evil agent" image runs
that tries, in order: read a mounted secret, reach the K8s API server,
`curl` the internet, push to the git remote, **run a disallowed command
(eBPF-blocked exec)**, **connect to a disallowed host**, and **call the model
endpoint directly** (not through the proxy — D29: this must be blocked by
**eBPF**, not the pod-level NetworkPolicy). **Every attempt must be blocked**,
and none may affect the Loop's evidence (the verify Job still sees the
committed SHA; the Loop's phase/evidence are unchanged) — a blocked attempt
does **not** fail or pause the Loop (ADR-0007 Q5: block-and-record), but **each
block appears in the activity-audit stream** (Q4), the direct-endpoint call is
**absent from the proxy's metering** (D29), and each increments
`status.policy.blockedCount`. Seam: e2e on kind — assert each
attempt's exit/output shows denial, the Loop keeps running, and each block is
in the audit stream. (The slice that makes ADR-0006 + ADR-0007 concrete.)
- **C6** — *`AgentPolicy` CRD + engine-policy translation (ADR-0007 Q2/Q3/D29).*
  Split into C6a (the CRD + the **pure** effective-allows→engine-policy
  translation, test-first) and C6b (wire the translation into the engine
  runtime + the `PolicyEnforced` gate). The box stays open until both are done.
  - **C6a — CRD + pure translation (DONE).** New `AgentPolicy` CRD
    (group `coxswain.wattu.com`, namespaced; default-deny, additive allows;
    union across policies via `spec.policyRefs[]` on the Loop; optional
    cluster-scoped `ClusterAgentPolicy` is a follow-on). A pure function
    `internal/policy/effective.go: Translate(effective []AllowRule)` returns an
    **engine policy spec** (per-container, D29): the agent container allows
    `localhost` + the policy's allows only; the proxy container allows only the
    model endpoint; default-deny when there are no allows (platform minimum
    only). The translation is engine-agnostic (a Go struct the engine emitter in
    C6b renders) so it is swappable and testable without the engine. Seam:
    **pure unit test** over a set of allows — the generated engine policy carries
    exactly those allows per container, and with no policy the default is deny
    (only the platform minimum); `policy.EffectiveHash` gives a canonical SHA-256
    of the union allows for the decision audit (D32). CRD seam: **envtest** — a
    Loop with `spec.policyRefs: [p1, p2]` records `status.policy.effectiveHash` =
    the union of both policies' allows (a missing referenced AgentPolicy is a
    reconcile error, not a silent narrow policy); a Loop with no policyRefs leaves
    the hash empty (default-deny minimum).
  - **C6b — engine runtime + gate (remaining).** The operator **emits** a
    `KubeArmorPolicy` in BPF-LSM mode selecting the Loop's sandbox pod, owned by
    the Loop (behind an internal interface, swappable for Tetragon), and gates
    the sandbox on **positive evidence** enforcement is active on the node
    (`PolicyEnforced`, D30 — `EngineUnavailable` | `NodeNotEnforcing` |
    `PolicyRejected`; the Loop waits, no terminal reason). `make kind-up` gains
    the engine install step + the disallowed-`exec` smoke. The generated
    `NetworkPolicy` (the coarse outer fence) is C3.

- **C7** — *Activity-audit stream (ADR-0007 Q4).* Coxswain **emits** agent-
activity audit as JSON lines on each trusted source's stdout with the common
envelope `{time, loop, namespace, iteration, source, action, target, verdict,
detail}`; it never stores it. Sources: the model proxy (each request/response
+ token counts), the eBPF engine (exec/file/network, allowed and blocked, with
the Loop's labels), and the operator (its decisions). Seam: the proxy's JSON
lines carry the loop/iteration labels; the engine's alerts carry the Loop
labels; the agent's own traces are not emitted as audit.
- **C8** — *`PolicyBlocked` condition + counter (ADR-0007 Q5).* A blocked
action increments `status.policy.blockedCount` and sets a `PolicyBlocked`
condition carrying the latest blocked target, so a stuck Loop's cause is
visible without reading logs. No terminal reason. Seam: envtest — after a
blocked action the counter increments and the `PolicyBlocked` condition is set
with the target; the Loop is not failed or paused.

### B. Controller reconcile state machine (`internal/controller/`)
The Phase 0 controller only ensured the sandbox + set `Pending`. Phase 1 drives
the phase enum based on `status.observedPhase` (from the runner) and the trust
model.

**Slice status:** B1 ✅ done — `status.desiredPhase`/`status.observedPhase` added
(CRD); `nextPhase(current, reported)` pure transition function handling only the
claim-driven steps (Pending → Planning → Implementing → **Verifying**). **The
runner's report never moves a Loop out of Verifying** (D23): every exit from
Verifying (→ Succeeded, → Implementing, → Failed:*) is evidence-gated and decided
by `verifyOutcome(job)` in B3 from the verify Job's container exit codes — never
by `observedPhase` (ADR-0004: it's a claim; ADR-0005: no gate on a claim). Tests:
pure `nextPhase` table (incl. the D23 stay-put cases) + envtest (claim-driven path
stops at Verifying; a runner report of Succeeded does not exit Verifying).

**Candidate seams (to confirm):**
- **B1** — *Phase transitions.* The controller reads `status.observedPhase` (the
  runner's signal) and advances `status.phase` per the transition table:
  `Pending → Planning → Implementing → Verifying → Succeeded` (happy path), and
  `Verifying → Implementing` (re-plan/implement on failure), `Verifying → Failed`
  on `MaxIterations`. Seam: cluster state via envtest (set observedPhase, reconcile,
  assert phase).
- **B2** — *Protected paths / TamperedVerify via base-commit glob diff
  (ADR-0005 round-4 D10).* The operator pins `status.baseCommit` (resolved from
  `spec.workspace.ref` at Loop start). Protected paths are globs
  (`spec.verify.protectedPaths[]`, per-language default when empty). At
  `Verifying`, the verify Job runs `git diff --name-only <baseCommit>
  <verifiedCommit> -- <globs>` **before any check runs**; non-empty ⇒
  `TamperedVerify` (terminal). The agent has no write path to the evidence
  (the two SHAs are operator-pinned). **Anti-gaming test set (D10 acceptance):**
  (a) edit an existing `*_test.go`; (b) **add a new `*_test.go` with `TestMain`
  → `os.Exit(0)`**; (c) **add a `replace` to `go.mod`** — each ends
  `Failed:TamperedVerify` with **no check run**, even when the fake runner's
  `result.json` claims the protected paths are untouched. **This is the core
  anti-gaming test (D7/D10 acceptance).**
- **B3** — *Deterministic verify via the Job's pod status (ADR-0005 D11/D13).*
  At `Verifying` start the operator resolves the Loop branch head to a SHA and
  records `history[n].verifiedCommit` (D11). The operator creates a short-lived
  verify Job (fresh pod, no shared process namespace, isolated per D12). **Every
  gate value is a kubelet-recorded container exit code, one check per container
  (D14 — the termination message was forgeable):** a clone init container checks
  out the pinned SHA; a tamper-check init container (trusted image, only `git` +
  the glob diff, before any agent code runs) exits non-zero on divergence;
  **each acceptance check is its own init container**. The operator reads
  `status.initContainerStatuses[i].state.terminated.exitCode` (tamper non-zero →
  `TamperedVerify` with no check container run; all checks 0 → `Succeeded`; check
  *k* non-zero → iterate with *k* in history; pod errored → `VerifyError`
  re-run once). Pod **logs** (`pods/log`) are the feed-forward channel for the
  next prompt (claims-grade, never a gate). Seam: **envtest has no Job
  controller**, so the B3 test sets `initContainerStatuses[].state.terminated.exitCode`
  per check directly (not a termination message) and asserts the phase. A minimal
  verify Job image (clone + one tamper init container + one check init container
  per check) is built in Phase 1 and runs isolated (D12).
- **B3a** — *Canary / negative control (ADR-0005 D17).* The code under test can
  subvert the test process in-process (an `init(){ if testing.Testing() {
  os.Exit(0) } }` makes `go test` exit 0 without running tests; no protected file
  changes, so the glob diff is empty). Mitigation: the verify Job includes a
  **canary check container** — the trusted tamper container generates a canary
  test per tested package (random name, `t.Fatal` with a random token) into a
  *separate copy* of the checkout, and a canary check container runs it that
  **must fail** and whose failure must be reported by `go test` (non-zero exit).
  A binary that exits 0 regardless of tests makes the canary *pass* →
  **`Failed:SubvertedVerify`** (a new terminal reason; added to CONTEXT.md
  "Failure reason"). Seam: envtest — assert the verify Job spec the operator
  builds contains a canary check container (trusted image, over a separate
  checkout copy); and the controller decision: canary container exit 0 ⇒
  `Failed:SubvertedVerify`. e2e fixture: the round-8 probe (`init(){ if
  testing.Testing() { os.Exit(0) } }`) as a fixture must end non-`Succeeded`.
- **B3b** — *Restart semantics (ADR-0005 D18).* The operator reads the *single*
  pod of the verify Job; `restartPolicy: OnFailure` / `backoffLimit > 0` would
  restart or recreate pods and break "check *k* failed ⇒ iterate". Fix: the Job
  spec has **`restartPolicy: Never`, `backoffLimit: 0`, `activeDeadlineSeconds`**
  (D12); the operator reads the single pod by the Job's `controller-uid` label —
  **zero or >1 pods ⇒ `VerifyError`**; `VerifyError`'s re-run creates a **new
  Job** (name includes an attempt suffix), never reuses one. Seam: envtest —
  assert the Job spec has `restartPolicy: Never` + `backoffLimit: 0`;
  controller — a second pod for the Job's `controller-uid` ⇒ `VerifyError` (not
  a pass); the re-run creates a new Job with an attempt suffix.
- **B3c** — *Read-only checkout (ADR-0005 D19).* The "otherwise writable"
  checkout branch let agent code in check 1 rewrite `*_test.go` after the
  tamper check, so check 2 could compile doctored tests. Fix: the checkout is
  mounted **read-only into every check container** (unconditional); each check
  gets its own writable scratch `emptyDir` for `HOME`, `GOCACHE`, `GOPATH`,
  `TMPDIR`; a check needing a writable tree gets a fresh copy from a trusted
  init step. Seam: envtest — assert every check container mounts the checkout
  `readOnly: true` and has its own scratch `emptyDir`.
- **B3d** — *Advisory static diff scan (ADR-0005 D17, mitigation 3; D21).* The
  in-process test-subversion canary (B3a) is the automated gate; this is the
  *advisory* layer that runs alongside it. It scans the base→verified diff of
  **non-protected** files for `testing.Testing()`, `os.Exit` inside `init`,
  `//go:linkname`, and `flag.Lookup("test.`. **Advisory only — never a gate, and
  never writes `spec`** (D21: `spec` is user-owned desired state — rewriting it
  fights `kubectl apply`/GitOps, bumps `generation`, and blurs the audit
  trail). Instead a hit records a `VerifySuspicious=True` **condition** (reason
  `SubversionPatternInDiff`, message listing the matched patterns + files) and a
  history note. Phase 6's PR step reads that condition:
  `effectiveReady = spec.pr.ready && !VerifySuspicious`. Reporting channel: the
  scan runs as its own trusted init container **after the tamper check**; hits
  are reported by its **termination message** (acceptable here — no agent code
  runs in that container, per D15) and the container **exits 0 either way** so
  the scan can never block the checks. A missing/garbled message is treated as
  "scan unavailable" (a `Warning` event), **not** as clean. Seam: envtest — a
  Loop whose non-protected change contains `testing.Testing()` in an `init`
  still reaches `Succeeded`, and the operator records the
  `VerifySuspicious=True` condition + a `Warning` event + a history note, with
  **`spec` unchanged (generation stays the same)**; a clean change records no
  such condition/event/note.
- **B4** — *Iteration + history.* Each transition increments `status.iteration`
  and appends to `status.history[]` (the audit trail, incl. `verifiedCommit`);
  **also records which checks were `NotRun` after the first failing check
  (I14)** — sequential init containers stop at the first non-zero, so the model
  fixes one check per iteration and the next prompt targets the not-run set.
  Seam: assert iteration count + history entries after a multi-iteration run,
  and that checks after the first failing one are recorded as `NotRun`.
- **B5** — *maxIterations.* A Loop that keeps failing stops at `maxIterations`
  with `Failed:MaxIterations` (terminal). Seam: a Loop with `maxIterations: 2`
  that always fails → `Failed:MaxIterations` after 2 tries.
- **B6** — *Foreign-owned sandbox → condition + requeue, not a retry storm or
  a wedge (D8 + D9).* `ensureSandbox` returns `AlreadyOwnedError` when the
  Loop's sandbox is owned by a different controller (I2, `900c72f`); as-is that
  loops `Reconcile` into an exponential-backoff requeue forever, visible only in
  logs. Fix (when the phase machine + conditions exist): on `AlreadyOwnedError`,
  emit a `Warning` event, set condition `SandboxReady=False` reason
  `SandboxNameConflict`, **return nil, and `RequeueAfter` a long interval (e.g.
  5m) while the condition is set** (D9: a plain `return nil` wedges the Loop
  because `Owns(&Sandbox{})` only maps events from sandboxes owned by this Loop,
  so a later deletion of the foreign sandbox enqueues nothing). Alternative:
  a `Watches` on Sandboxes mapping by name (`<loop>-sandbox` → Loop). Seam:
  (1) the foreign-owner test in `loop_adoption_test.go` is updated to assert the
  condition + event and that `Reconcile` returns nil with a requeue (it
  currently asserts an error — deliberately left as a red marker until B6
  lands); (2) **delete the foreign sandbox → the next reconcile creates the
  Loop's own sandbox and clears the condition.** Do not implement before the
  condition/event infrastructure from B1 exists.

## Settled design questions (2026-09-26, all confirmed with user)

1. **Phase channel (REVISED, ADR-0004).** ~~Loop status subresource~~ → **result
   file only.** The runner writes `result.json` (which reports the
   `observedPhase`) and gets **no Loop-status RBAC**. The operator reads
   `result.json` and writes `loop.Status.ObservedPhase`/`Iteration`/`History`.
   The operator communicates `desiredPhase` to the runner via a file on the
   workspace (`.coxswain/desired-phase`). `loop.Status.ObservedPhase` stays a
   field, but it is the operator's *record of the runner's report*, written by
   the operator — not a value the runner writes. Rationale: a runner with a
   shell that can patch its own Loop status can lie about its own progress,
   which breaks determinism + auditability and contradicts CONTEXT.md's
   "result file is the only output the operator reads." See ADR-0004.
2. **Tamper check = base-commit glob diff (REVISED, ADR-0005 round-4 D10; round-6
   D14/D16).** ~~Operator computes baseline hashes from the base ref~~ → the
   operator pins **`status.baseCommit`** (resolved from `spec.workspace.ref` at
   Loop start via go-git — D15) and protected paths come from **
   `spec.verify.preset`** (enum, default `go`) + `protectedPaths[]` (+
   `protectedPathsOverride`). At `Verifying`, the verify Job's **tamper-check
   init container** (trusted image, only `git` + the glob diff, before any agent
   code runs) does `git diff --name-only <baseCommit> <verifiedCommit> --
   <globs>`; non-zero exit ⇒ `TamperedVerify` (terminal, before any check
   container runs). This catches **added, modified, deleted, and renamed**
   protected files (a hash list missed added files — D10), needs no stored
   hashes, and keeps the operator content-free (it never clones or holds file
   content). The check *results* are the exit codes of per-check init containers
   (D14 — the termination message was forgeable). See ADR-0005.
3. **Verify = isolated Job, operator reads Job status (REVISED, ADR-0005).**
   ~~Runner runs each acceptance check and reports exit codes in
   `result.json`~~ → at `Verifying` the operator creates a short-lived Job
   (fresh pod, no shared process namespace with the sandbox) that checks out
   the Loop branch at the iteration commit, runs the acceptance checks (from
   the base ref), and reports via container exit code / Job status, which the
   operator reads from the API. `result.json` carries claims only. See
   ADR-0005.

## CRD changes (Phase 1)

`LoopSpec` gains:
- `agent` `{ image, model, endpointSecretRef, env?, egressAllow? }` (ADR-0006) —
  the agent image + model + the secret ref holding the base URL + API key
  (mounted into the **proxy sidecar**, never the agent). Defaults from a
  `coxswain-agent-defaults` ConfigMap so the README sample stays short.
- `workspace.gitCredentialSecret` (string, optional) — secret with the git token
- `verify.protectedPaths[]` (optional, **globs**) + `verify.preset` (enum, default `go`; ADR-0005 round-6 D16) + `verify.protectedPathsOverride` (bool, default false) — the protected paths for the TamperedVerify glob diff. The content-free operator can't detect the repo's language, so the preset is **explicit**: `preset: go` expands to the Go glob set (`**/*_test.go`, `**/testdata/**`, `go.mod`, `go.sum`); `protectedPaths[]` **adds** to it; `protectedPathsOverride: true` (or `preset: none`) **replaces** it. Drop the "files a check references" heuristic; document that a check calling `make` should list `Makefile` in `protectedPaths`. Protecting `go.mod`/`go.sum` means the agent can't add dependencies (documented). (The plan's `acceptanceChecks[]` commands stay as-is.)
- `loop.phaseTimeout` (metav1.Duration, default 30m) — Phase 1 adds the field;
  the timeout *enforcement* (restart from checkpoint) is Phase 3, so Phase 1
  only records it.
- `approval` (already in the enum; `mode: Auto|Manual`, `onReject: Replan|Fail`)
  — Phase 1 uses `mode: Auto` only; the Manual gate is Phase 4.

`LoopStatus` gains (all **operator-written**; ADR-0004 — the runner never writes these, it only reports them in `result.json`):
- `desiredPhase` / `observedPhase` (Phase) — the operator records the phase it asked for and the phase the runner reported. `desiredPhase` is also copied to `.coxswain/desired-phase` for the runner to read.
- `baseCommit` (string) — the SHA resolved from `spec.workspace.ref` at Loop start, pinned for the Loop's life (ADR-0005 round-4 D10). **Replaces** the old `verify.baselineHashes`.
- `currentVerify` `{ verifiedCommit string }` (ADR-0006/D27) — the operator's pin of the **current** iteration's verified commit, written on entering `Verifying` (D11) and used to bind the verify evidence to the commit being verified. Distinct from `verify.verifiedCommit` (what the *evidence* names); a mismatch or empty pin makes the evidence Unknown (fail-closed).
- `plan` `{ summary string (≤4KB), hash string (sha256 of PLAN.md) }`
- `history []HistoryEntry` — the audit trail: `{ iteration, phase, reason, message, timestamp, verifiedCommit string (the SHA the verify Job checks out, set at `Verifying` start — ADR-0005 D11) }`
- `iteration` (int) — already present from Phase 0
- `verify` `{ lastCheckResults []string }` — the last verify outcome (from the verify Job's `initContainerStatuses` exit codes, one check per container — ADR-0005 D14) to feed forward. (The old `baselineHashes` is gone — replaced by `baseCommit` + the Job's glob diff.)

**`result.json` (the runner's claims file) carries NO verify-evidence fields**
(ADR-0005). Its schema for Phase 1 is: `status` (success|blocked|needs_input),
`summary`, `filesChanged[]`, `verificationNotes` (a *claim*, free text),
`nextIterationPlan?`, `lessons[]`, and the reported `observedPhase`. The verify
outcome and the tamper verdict are **not** in `result.json` — they come from
the verify Job (pod `initContainerStatuses` exit codes, one check per container — D14).

RBAC: the runner gets **no** Loop RBAC (ADR-0004) — it is credential-free and
only writes `result.json`. The operator keeps full CRUD on loops + sandboxes
and, per ADR-0005, gains:
- `pods/exec` — to `cat` the runner's claims `result.json` out of the sandbox
  (namespace-wide; I9 — revisit in Phase 7).
- `jobs` create/delete/list/get (in the Loop's namespace) — to create the verify
  Job and read its status (the verify evidence).
- `pods/get` (in the Loop's namespace) — to read the verify Job's pod status
  (`initContainerStatuses[].state.terminated.exitCode`, one check per container;
  D14).
- `pods/log` (in the Loop's namespace) — to fetch the raw verify Job log to feed
  the next implement prompt (claims-grade, never a gate; D14).
- `secrets/get` (in the Loop's namespace) — to read
  `spec.workspace.gitCredentialSecret` for go-git ref resolution (D15).


## Out of scope for Phase 1

Budgets/stall (Phase 2), checkpoints/fork (Phase 3), approval gate +
kubectl-cox (Phase 4), shared memory (Phase 5), PR + Judge (Phase 6), hardening
(Phase 7). The `AwaitingApproval` phase is in the enum but the approval *gate*
is Phase 4 — Phase 1 uses `spec.approval.mode: Auto` only.

## Suggested slice order (once seams are confirmed)

The isolation slices (C1–C8, ADR-0006 + ADR-0007) come **before B3** — they are
what make B2/B3's evidence meaningful. After D27:

C1 (sandbox hardening + `spec.agent`) → C2 (model proxy sidecar) → C6
(`AgentPolicy` CRD + engine-policy translation) → C3 (NetworkPolicy generated
from `AgentPolicy`) → C4 (trusted publish step) → C8 (`PolicyBlocked` condition
+ counter) → C7 (activity-audit stream) → C5 (evil-agent e2e, extended with a
disallowed command + a disallowed host), then the reference runner A1 → A2 →
A3 → A4 (now driving the conformance agent through the proxy, `COX_MODEL_BASE_URL`
= localhost, each red→green — note A4 is now *context continuity*; the old A4
"runner runs checks" is dropped per ADR-0005), then B3 → B3a → B3b → B3c → B3d
(the verify path, expanded for the D17 canary + advisory scan, D18 restart
semantics, D19 read-only checkout) → B4 → B5 → B6. B1 and B2 (the claim-driven
phase machine + TamperedVerify via base-commit glob diff) are already done.
B2's anti-gaming guarantee (D7) is the highest-value test; B6 (foreign-owned
sandbox → condition, D8) needs B1's condition/event infrastructure. C5 (the
evil-agent e2e) is the slice that proves ADR-0006 + ADR-0007 and gates B3's
evidence as meaningful.

**Gate (REVIEW-PHASE1-R8/R9):** no C3/C6/C7 code until the reviewer has
reviewed ADR-0007. C1, C2, C4 (and C8, C5's non-policy parts) may proceed
meanwhile. D29 (per-container egress via eBPF) and D30 (fail-closed when the
engine isn't enforcing) are **P1 ADR-0007 amendments that block C3/C6 code** —
they must be folded in (done in round 9) before those slices build on the
NetworkPolicy-only model.

**Infra:** `make kind-up` gains a step to install the eBPF engine (version
pinned once, like agent-sandbox) and a smoke that proves a disallowed `exec` is
blocked on kind (ADR-0007 Q3).
