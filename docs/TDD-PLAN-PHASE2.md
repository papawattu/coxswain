# Phase 2 — TDD plan: stopping properly (stall detection, token budgets, Paused/resume)

Phase 2 of `docs/PLAN.md` ("Stopping properly") gives the operator the three
mechanisms that stop a Loop which cannot succeed: **stall detection** (N
consecutive identical failing-check outputs), **budgets** (token, wall-clock
and cost caps), and the **`Paused` phase** (with `status.pausedFrom` and
exact-phase resume). Conditions + events ride on every new transition.

Owner direction that bounds the shape (2026-07, handoff):

- **MVP-first rule** (the review docs' standing rule): only **zero-cost
  seams** for anything parked — a field or hook that is inert until a later
  slice, never speculative machinery.
- **Scope is tight.** No oscillation detection (PLAN.md defers it explicitly),
  no UI, no Phase 3 durability.
- **The metering design point is resolved here** (P2a, below): PLAN.md's
  "metering sidecar in the sandbox" is **superseded** — metering happens in
  the **model proxy pod** (D33, an operator-owned pod OUTSIDE the agent's
  control since D33). See "The metering design point" in P2a.

Build conventions (same as D41/I42): each slice is red→green; envtest-first
where the operator is involved, unit tests for binaries and pure functions;
kind acceptance at the end (P2h) on `--context kind-coxswain-dev`. Every gate
this plan adds is mutation-checked per the R20 I49 norm: the named mutation is
applied in a **scratch worktree only**, the affected spec must FAIL, and the
result is recorded in the PR. Same-Loop update/delete specs per the I43 norm;
in-progress (no-decision) specs per the R20 I49 norm for every decision that
reads pod/container/Job status.

Existing seams the slices reuse (verified against `main` at b47ed62):

- `api/v1alpha1/loop_types.go` — `LoopPhase` enum **already includes
  `Paused`** (declared but never entered by any code path today);
  `LoopSettings` (`maxIterations`, default 3 via `defaultMaxIterations`);
  `spec.suspend` (S1: forces `OperatingMode=Suspended` in
  `ensureSandbox` — but the Loop's **phase** today keeps advancing; the S1
  path never records a `pausedFrom`); `LoopStatus.Conditions`;
  `LoopStatus.Progress` (OS1's structured record, where the failing check
  name + exit code already ride as `LastResultStatus`);
  `VerifyStatus` (evidence: `verifiedCommit`, `jobName`, `tamperExitCode`,
  `lastCheckResults`).
- `internal/controller/loop_verify_job.go` — the verify Job builder + the
  `verifyOutcome` evidence-gated decision (B3); the iterate branch
  (`Verifying → Implementing`, `MaxIterationsExceeded` terminal cap at
  `defaultMaxIterations = 3`); `emitVerifyIteratedEvent` (the OS5 Event
  pattern); the verify Job's check-* containers' **logs** are the failing
  output (read via the APIReader log path — same seam as the S4 claim reader).
- `internal/controller/loop_controller.go` — `nextPhase` (claim-driven
  transition table, B1); `ensureSandbox` (the `spec.suspend` →
  `OperatingMode=Suspended` branch, ~line 1241); `emitPhaseAdvancedEvent`
  (the OS5 Event pattern); `finalizeLoopStatus` / `setCondition`; the
  spec-hash drift-recreate pattern; `LoopReconciler` config fields (the
  `--verify-image`, `ProxyImage`, `EgressProxyImage`, `ToolProxyImage`
  pattern for new flags).
- `internal/controller/loop_s4_phase.go` — the ADR-0004 claim reader
  (one-shot per phase, termination-message channel, strict-parsed, never a
  gate input).
- `internal/controller/loop_suspend_test.go` — the S1 suspend specs
  (reconcile → Sandbox `OperatingMode=Suspended`).
- `cmd/proxy-standin` — the D33 stand-in model proxy (single Go binary,
  distroless, env config, forwards to `MODEL_ENDPOINT`); the pattern for the
  metering binary (P2b).
- `test/e2e/d41-e2e.sh` + `Makefile` (`make d41-e2e`) — the kind acceptance
  pattern.

---

## P2a — Metering design point (ADR): meter in the model proxy, not the sandbox

**Scope:** ADR `docs/adr/0009-metering-in-model-proxy.md` recording the
design decision PLAN.md left open, plus the plan-level justification. No code.

**The design point (resolved).** PLAN.md describes a "metering sidecar (thin
HTTP proxy in front of the model API, **in the sandbox**)". Since D33 the
model proxy is a **separate operator-owned pod** (`<loop>-proxy`, UID 65533)
**outside** the agent's control: the agent container has no SA token, a
read-only rootfs, a process + DNS fence (KubeArmor, D33/C6b), and egress to
the model **only through the proxy** (D34 netpol). A metering component
*inside the sandbox* sits in the agent's own trust boundary: a
prompt-injected agent could edit it, bypass it (point the runner at another
port), or corrupt its counters — so its counts are **agent-tamperable** and
cannot be the basis for a `Fail` decision (a budget cap applied on
tamperable evidence would let the agent *disable its own guardrail*).

The model proxy already sees **every** model request the Loop makes (the
agent's only path to the model, ADR-0006), it is operator-owned, and it
already holds the credential. Metering there is the **only** tamper-resistant
per-Loop option without new RBAC, a new CRD, or a sidecar the operator must
trust to be unbroken. Therefore:

- **Meter in the model proxy.** The proxy (the `cmd/proxy-standin` successor
  from P2b) reads each response's `usage` field (`prompt_tokens`,
  `completion_tokens`, P2b) and accumulates **per-Loop** counters for
  prompt tokens, completion tokens, and request count.
- **Report-only, exactly as PLAN.md requires.** The proxy writes a
  JSON-lines audit log to stdout (`source: "model-proxy"`, `action:
  "usage"`, `promptTokens`, `completionTokens`, `model`); it does **not**
  decide anything and never refuses a request because of a budget. **The
  operator** reads the counters into `status.budget` and applies
  `spec.budget.onExceeded` (P2d). The proxy cannot be made to stop the Loop;
  only the operator can. This keeps the "meter reports, operator decides"
  separation PLAN.md asks for — it just moves the meter from the sandbox
  to the proxy, where the report is credible.
- **`usage` is a field on the model's own response.** The proxy does not
  estimate token counts (no BPE, no byte-length heuristic); it copies the
  counts the model reported. Responses without `usage` (or with
  `usage: null`) count the request but **zero tokens** — the operator's
  budget decision on such a Loop is under-counted, never over-counted, and
  the operator records `status.budget.usagePresent=false` so an
  operator/operator-less model is visible. (This is a **derived**, not
  measured, quantity — PLAN.md already says cost is derived, and tokens are
  now derived the same way: reported by the model, trusted at the boundary.)
- **Prometheus `vllm:*_tokens_total` metrics (owner's note) are an
  acceptance cross-check, not the per-Loop source of truth.** They are
  **cluster-wide** (per backend, not per Loop) — on a shared vLLM backend
  several Loops' tokens land in the same counter. They are used in P2h's
  kind acceptance as a consistency assertion (the per-Loop proxy count for a
  Loop is consistent with the backend counter's **delta** over the Loop's
  wall-clock window), never as the input to a budget decision.
- **What this ADR does NOT cover:** metering of tool calls (P2: tools are
  not in the token/cost budget — `maxTokens`/`maxCostUsd` count model
  tokens only; `maxWallClock` is Loop-level), any per-request metering
  beyond `usage`, and any decision by the proxy.

**ADR acceptance:** ADR written and committed; the metering binary
(P2b) and operator decision (P2d) match the ADR's "Decision" section; the
P2h kind acceptance cross-check is present.

**Gate mutation (I49 norm):** N/A (docs-only slice — the mutation checks
live in P2b/P2d, which this ADR's specs name).

**Dependencies:** none (design slice; P2b/P2d consume it).

---

## P2b — `cmd/model-proxy` binary: the metering model proxy

**Scope:** replace `cmd/proxy-standin` with a real metering model proxy
(`cmd/model-proxy/` — the stand-in is kept until P2e switches the default
image so existing e2e pins keep passing). The proxy forwards to
`MODEL_ENDPOINT` exactly as the stand-in does (reverse proxy, single
upstream, the D33/D34 shape) and **adds** per-Loop `usage` metering per the
P2a ADR.

- **Forwarding:** unchanged from the stand-in (origin-form plain HTTP
  in-cluster from the agent; the proxy dials `MODEL_ENDPOINT` as the origin
  of its own request; `https://` with verified TLS, `http://` plain; no
  `CONNECT`/absolute-form — those are `405`; no redirect following; the
  resolved-IP carve-outs are NOT this proxy's job — the D34 netpol owns
  that, unlike the tool proxy).
- **Credential:** the API key header is injected from the mounted
  `model-creds` Secret (as the stand-in does: read every file under
  `/model-creds` at startup). The key is never logged.
- **Metering (P2a):** after each upstream response that the proxy returns
  with a body (any status — a failed completion still reports `usage` when
  the model parsed the prompt), parse the response JSON's `usage` object
  (`prompt_tokens`, `completion_tokens`; the OpenAI-compatible shape the
  reference runner already speaks; a non-JSON or non-object body records
  the request with zero tokens). **Streaming (`SSE`) responses:** the
  standard OpenAI-compatible stream ends with a `data: {...}` chunk carrying
  the final `usage` (with `stream_options.include_usage` the reference
  runner requests this; without it the last chunk's `usage` is used when
  present, else zero — under-counted, recorded, never over-counted, per
  P2a). The counters are **per-Loop, in-memory** (one pod per Loop: no
  cross-Loop contamination) plus the audit log line.
- **Audit (JSON on stdout, one line per metered request):**
  `{time, loop, namespace, source: "model-proxy", action: "usage", model,
  promptTokens, completionTokens, status, usagePresent}`. (`loop`/`namespace`
  from env, `model` from the request body's `model` field or the
  `MODEL_NAME` env.)
- **Counter endpoint (operator read path):** an **in-pod, localhost-only**
  `GET /metrics` (bound to the proxy's own `127.0.0.1`, NOT exposed on a
  Service port — the agent netpol only allows the model port 8080, so the
  agent cannot reach it) returning JSON:
  `{promptTokens: N, completionTokens: N, requests: N, model, sinceStart:
  RFC3339}`. The operator reads it in P2d **through the proxy's own pod**
  (an in-cluster `http://127.0.0.1:<port>` from the operator is impossible —
  the operator runs in a different pod; the read path is the **audit log**,
  re-read from the proxy pod's container logs via the APIReader log seam,
  exactly like the S4 claim reader reads the agent's termination message.
  `/metrics` is kept for an operator's `kubectl exec`/port-forward
  debugging; the **gate** reads the audit log, never the endpoint. This
  keeps the operator's read at the same trust level as every other operator
  observation: kubelet-recorded container logs, not a claim the proxy could
  selectively write.
- **Config via env** (the stand-in's pattern): `MODEL_ENDPOINT`,
  `MODEL_NAME`, `LOOP_NAME`, `LOOP_NAMESPACE`, `PROXY_PORT` (8080).
- **Pod hardening:** identical to the stand-in's (UID 65533, no SA token,
  read-only rootfs, no caps, seccomp RuntimeDefault).

**Unit tests (written first, `cmd/model-proxy/` + `internal/proxy/` for the
shared logic):**
- **Metering (table-driven, `httptest` upstream):** a non-stream response
  with `usage: {prompt_tokens: 120, completion_tokens: 40}` → the audit line
  carries both counts and `usagePresent: true`; a response **without**
  `usage` → zero tokens, `usagePresent: false`; a `4xx`/`5xx` response with
  `usage` → still metered (a failed completion is a consumption); a
  non-JSON body → zero tokens, `usagePresent: false`.
- **Streaming:** a 3-chunk SSE stream whose last chunk carries `usage` →
  the accumulated counts match; a stream with no `usage` chunk → zero,
  `usagePresent: false`.
- **Forwarding invariants (carry the stand-in's tests across):** origin-form
  request → forwarded with the credential header; `CONNECT` / absolute-form
  → `405`, no dial; a non-`2xx` upstream response is returned to the agent
  as-is (no retry); the audit line contains no substring of the test
  credential.
- **Per-Loop isolation:** the counters are in-process only (no shared
  store): a second proxy instance (fresh process) starts at zero — the test
  asserts the module has no package-level mutable state shared across
  "Loops".

**Acceptance:** `go build ./cmd/model-proxy` + unit suite green under
`make test`; the image is buildable (Dockerfile mirroring the
stand-in's) so P2e can reference it.

**Gate mutations (I49 norm, scratch worktree, each must make its spec FAIL):**
- **Skip the `usage` parse** (record zero tokens unconditionally) → the
  metering specs 1/2 FAIL (the counts are zero, `usagePresent` always
  false).
- **Drop the audit log write** → the forwarding-invariant spec that greps
  the audit line FAILS.
- **Retry a non-2xx upstream once** (change the "return as-is" branch) →
  the forwarding spec asserting no retry FAILS.

**Dependencies:** the P2a ADR (the design it implements); the D33/D34
stand-in (the code it replaces in P2e).

---

## P2c — API: `status.budget` + `status.stallHistory` + `spec.loop.stall*` + `spec.budget`

**Scope:** the CRD fields the decision slices (P2d/P2e) read. No controller
behaviour in this slice (P2d/P2e consume it). `make manifests generate`
after the type change; the CRD diff is committed.

- **`spec.budget`** (new, optional, `BudgetConfig`):
  - `maxTokens` (`*int64`, ≥1 when set) — total **prompt + completion**
    tokens across the Loop's life.
  - `maxWallClock` (`*metav1.Duration`-shaped string via a `Duration`
    wrapper field, ≥1s when set) — wall clock from Loop creation
    (`metadata.creationTimestamp`) to now, **including Paused time** (a
    paused Loop still counts toward its wall clock: the budget is on the
    wall, not on running time — documented in the field comment; the owner
    can `Fail` a Loop that sat paused, and that is the intended semantics).
  - `maxCostUsd` (`*string`, a decimal string, ≥0 when set) — derived cost
    (P2d), not measured.
  - `onExceeded` (enum `Pause|Fail`, default `Pause`) — the action when
    **any** cap is hit.
- **`spec.loop`** gains (on `LoopSettings`):
  - `stallAfter` (`*int32`, ≥1, **default 3** when unset — a `nil`/zero
    reads as 3, the PLAN.md default; a Loop can set a different N).
  - `stallAction` (enum `Fail|Pause|Continue`, **default `Fail`** —
    PLAN.md's `Fail → Failed:Stalled` is the conservative default; a Loop
    that wants warn-and-continue sets `Continue`).
- **`status.budget`** (new, optional, `BudgetStatus`, operator-written):
  - `promptTokens`, `completionTokens` (`int64`, from the proxy audit log,
    P2d), `requests` (`int64`), `usagePresent` (`bool` — false when the
    most recent metered request carried no `usage`; P2a), `costUsd`
    (`string` decimal, derived), `exceeded` (`bool` — true once any cap is
    hit; sticky, never cleared, so a resumed Loop still shows it),
    `exceededReason` (enum `Tokens|WallClock|Cost` — the first cap that was
    hit; empty until exceeded).
- **`status.stallHistory`** (new, optional, `[]StallEntry`, atomic list):
  - one entry per **verify-failure** iteration: `iteration` (`int`),
    `hash` (the SHA-256 hex of the normalised failing output, P2e),
    `normalisationVersion` (`string`, the P2e constant the rules ran at —
    PLAN.md's versioning requirement), `check` (the failing check's name,
    as in `LastResultStatus`), `at` (`metav1.Time`, the verify Job's finish
    time, kubelet-recorded). Capped at the last **10** entries (older
    entries dropped — the decision only needs the recent consecutive run;
    the cap is a constant in the code, not a spec field).
- **`status.pausedFrom`** (new, optional, `LoopPhase`): set to the phase
  the Loop left when it entered `Paused` (P2f); cleared on resume.
- **`conditions`:** two new condition types (constants in `loop_types.go`,
  the `DeliveredCondition` pattern): `Stalled` (`True` when the stall
  detector has fired — `stallAction=Fail` then the Loop is `Failed` with
  reason `Stalled`; `Pause` then `Paused`; `Continue` then the condition is
  `True` + the Loop keeps running — the condition records the state, the
  phase does not change) and `BudgetExceeded` (`True` when
  `status.budget.exceeded` is true; the phase then follows
  `onExceeded`).
- **Validation (CEL):** `onExceeded` enum; `stallAction` enum;
  `stallAfter` ≥1; `maxTokens` ≥1; `maxCostUsd` matches `^\d+(\.\d+)?$`.

**Envtest-first tests (`internal/controller/loop_p2c_validation_test.go`):**
1. **Defaults.** A Loop with no `spec.budget` and no `spec.loop.stall*` →
   admitted; the reconciler's effective config (the P2d seam) reads
   `stallAfter=3`, `stallAction=Fail`, `onExceeded=Pause` (assert via the
   exported `effectiveStallConfig`/`effectiveBudget` functions — P2d's
   consumer, unit-tested here).
2. **Bad enum rejected at admission.** `stallAction: "Mangle"` → API server
   rejects (CEL).
3. **`stallAfter: 0` rejected** (CEL minimum).
4. **`maxCostUsd: "abc"` rejected** (CEL pattern).
5. **No budget fields → no change in behaviour.** An existing Loop
   reconciles exactly as before (guards the existing specs: S1 suspend,
   the S5a iterate specs, the B3 decision specs all pass unchanged — a
   compile-level guard that the new fields are additive).

**Gate mutation (I49 norm, scratch worktree):** drop the `stallAfter`
default-of-3 branch (make `nil` read as `0`) → spec 1 FAILS (the effective
config is 0, not 3).

**Acceptance:** `make manifests generate lint test` green; the CRD YAML
diff contains only the P2c additions (no unrelated regeneration drift).

**Dependencies:** none for the types (pure CRD); P2d/P2e consume them.

---

## P2d — Budget decision: the operator reads the proxy audit log and applies `onExceeded`

**Scope:** the operator's budget gate. On each reconcile, the operator
**reads the model proxy pod's container logs** (the P2b audit lines, via
the APIReader log path — the S4 claim reader's seam) and updates
`status.budget`; when any cap is hit it applies `spec.budget.onExceeded`.

- **The read:** the operator reads the proxy pod's logs (the container
  `proxy`, tail the last ~2000 lines), parses the `action: "usage"` JSON
  lines **since the Loop's last reconcile** (dedup by a monotonic `seq` the
  proxy adds to each audit line — P2b's audit line carries it; the operator
  stores `status.budget.lastSeq` and sums `seq > lastSeq`). The sum lands in
  `status.budget.{promptTokens, completionTokens, requests}`. A proxy pod
  that is not Ready or has no readable logs → `status.budget` is unchanged
  (the operator does not reset counters on a log-read failure: an
  under-read is visible as `requests` lagging the verify iterations, and
  the budget decision is on the last *read* count, never on an estimate).
  **A Loop with no model** (`endpointSecretRef` unset, no proxy pod) →
  `status.budget` stays nil; the token/cost caps are inert; `maxWallClock`
  still applies (it needs no proxy).
- **Wall clock:** `now - metadata.creationTimestamp` vs `maxWallClock`
  (computed operator-side; no proxy needed).
- **Cost:** `costUsd = promptTokens * pricePerMtok_prompt/1e6 +
  completionTokens * pricePerMtok_completion/1e6`, the prices read from the
  cluster-wide **`coxswain-model-prices` ConfigMap** (in the operator's
  namespace; keys `prompt`, `completion`, values decimal USD per million
  tokens) **overridable per Loop** via `spec.agent.model`-keyed entries (the
  ConfigMap is the default; a per-Loop `spec.budget.modelPrices` field —
  P2c, added in this slice if the CRD budget allows — carries
  `{prompt, completion}` when set). Cost is **derived** (P2a), not
  measured.
- **The decision (pure function of `status.budget` + `spec.budget` + now):**
  - **No cap set → no decision.** (The default Loop is unaffected.)
  - **A cap is hit** (`exceeded` transitions false→true): set
    `status.budget.exceeded=true`, `exceededReason`, the
    `BudgetExceeded=True` condition (message names the cap and the count),
    and apply `onExceeded`:
    - **`Fail`** → `phase=Failed`, `Failed` condition `True` reason
      `BudgetExceeded`, the sandbox is torn down via the existing
      `Failed`-phase cleanup (the C-series cleanup path), a `Normal`
      Event (`Reason: BudgetExceeded`).
    - **`Pause`** → `phase=Paused`, `status.pausedFrom=<phase-before>`, the
      `Failed`-style teardown does **not** run (the Loop is paused, not
      terminal — P2f owns the Paused phase's mechanics; this slice enters
      it the same way `spec.suspend` would, but with a budget reason), a
      `Warning` Event.
  - **`exceeded` is sticky:** once true it never clears (a resumed Loop
    that re-reads the same counters is already `exceeded`; the operator
    does not re-decide, it holds the phase). **An already-exceeded Loop in
    `Paused` stays `Paused` on resume** (P2f's resume must not clear
    `exceeded` — the cap is still hit; the operator records the resume and
    the Loop re-enters `Failed`/`Paused` on the next reconcile. This is the
    fail-closed semantics: a budget you have already exceeded does not
    un-exceed by pausing and resuming.)
- **In-progress (I49 norm):** the budget decision reads the **proxy pod's
  status** (Ready / not) and the **log read** — both are in-progress states
  that must produce **no decision**: a proxy pod that is `Pending`/`Running`
  (not Ready) → no budget decision this reconcile (requeue), never a
  "not exceeded" decision from a not-yet-readable counter.

**Envtest-first tests (`internal/controller/loop_p2d_budget_test.go`):**
1. **No cap → no decision.** A Loop with a proxy audit log but no
   `spec.budget` → `status.budget` is populated (counts, cost) but the
   phase is unchanged, no `BudgetExceeded` condition. (The read works
   independently of the decision.)
2. **`maxTokens` hit → `Fail`.** `maxTokens: 200`, `onExceeded: Fail`; the
   audit log sums to 250 tokens → `phase=Failed`, `Failed` condition
   `True` reason `BudgetExceeded`, `status.budget.exceeded=true`
   `exceededReason=Tokens`, the Event is recorded.
3. **`maxTokens` hit → `Pause`.** Same, `onExceeded: Pause` →
   `phase=Paused`, `status.pausedFrom=<the phase it left>` (e.g.
   `Implementing`), `BudgetExceeded=True`, **the sandbox is not torn
   down** (still exists; P2f's gate suspends it).
4. **`maxWallClock` hit (no proxy).** A Loop with `maxWallClock` set, no
   model, and `metadata.creationTimestamp` back-dated past the cap →
   `exceeded`, `exceededReason=WallClock`, the `onExceeded` action. (No
   proxy needed — guards the wall-clock path.)
5. **`maxCostUsd` hit (derived).** A `coxswain-model-prices` ConfigMap
   present; `maxCostUsd` set below the derived cost → `exceeded`,
   `exceededReason=Cost`. A missing ConfigMap → the cost is **not computed**
   (`status.budget.costUsd` empty, the cost cap is inert — fail-closed: an
   unpriceable cost is not a $0 cost).
6. **Sticky `exceeded`.** A Loop that hit `maxTokens` (spec 2's shape),
   then the operator *edits the audit log* (a new line with a lower count
   — simulating a partial re-read) → `exceeded` stays true, the phase does
   not un-`Failed`/un-`Paused`.
7. **No model → token/cost caps inert.** A Loop with `maxTokens` set and no
   `endpointSecretRef` → `status.budget` stays nil; the token/cost caps
   never fire; the phase is unaffected by them.
8. **In-progress: proxy pod not Ready → no decision (I49).** The proxy pod
   exists but is not Ready → no budget decision this reconcile (the phase
   is unchanged, no `exceeded`, a requeue — the spec asserts **no**
   `BudgetExceeded` condition and no phase change after the reconcile, and
   that a *second* reconcile with the pod Ready then applies the decision).
9. **In-progress: no log lines yet → no decision (I49).** The proxy pod is
   Ready but the log read returns no `usage` lines (the first reconcile
   after the proxy started) → no decision, no `exceeded` (the operator
   does not decide "not exceeded" from an empty log; it waits for data).
10. **`lastSeq` dedup.** Two reconciles read the same audit lines (the log
    tail is stable) → the second reconcile does not double-count
    (`status.budget` is unchanged between the two reads).
11. **Same-Loop update (I43 norm):** after the Loop is `Paused` on
    `onExceeded=Pause` (spec 3's shape), **update `spec.budget.maxTokens`
    to a higher value and re-reconcile from the API server** → `exceeded`
    is still true (sticky), the phase is still `Paused` (the higher cap
    does not un-exceed — the decision was made; the spec asserts the phase
    and `exceeded` are unchanged, and that the *new* cap is recorded in the
    condition's message on the next exceed decision only).
12. **Per-Loop price override.** A `spec.budget.modelPrices` (P2c) that
    overrides the ConfigMap → the derived cost uses the override (the
    `maxCostUsd` decision flips at the override-derived threshold, not the
    ConfigMap's).

**Gate mutations (I49 norm, scratch worktree, each must make its spec FAIL):**
- **Skip the `exceeded` → phase transition** (set the condition but leave
  the phase) → specs 2 and 3 FAIL (the phase is unchanged).
- **Make `exceeded` non-sticky** (recompute `exceeded` from the current
  sum every reconcile, clearing it when the sum drops) → spec 6 FAILS
  (the phase un-`Failed`s).
- **Treat a missing prices ConfigMap as $0** (cost = 0 when unreadable) →
  spec 5's "missing ConfigMap" half FAILS (the cost cap fires at $0).
- **Decide on an empty log** (an empty `usage` read is "not exceeded") →
  spec 9 FAILS (the decision fires on the first reconcile).
- **Double-count on re-read** (drop the `lastSeq` dedup) → spec 10 FAILS
  (the second reconcile doubles the tokens).

**Acceptance:** envtests green under `make test`; the operator's budget
read uses the APIReader log seam (no new RBAC: `pods/log/get` — confirm the
manager's RBAC has it; add the marker if not, `make manifests`).

**Dependencies:** P2a (the metering design), P2b (the audit log it reads),
P2c (the fields it writes). P2f's Paused phase is the `Pause` action's
target (P2d enters it; P2f owns the phase's full mechanics — the two slices
share the `pausedFrom` write; P2d's spec 3 asserts P2f's contract).

---

## P2e — Stall detection: normaliser + golden files + the operator's stall gate

**Scope:** the stall detector. Two halves: (1) the **normaliser** (a pure
function with golden-file unit tests), (2) the **operator's stall gate**
(envtest, the decision that reads the verify-failure evidence and applies
`stallAction`).

### The normaliser (`internal/stall/`)

- **Input:** the raw **failing verify-check output** — the failed
  check-* container's log tail (the same log the operator already reads for
  `LastCheckResults`'s exit code; P2e's gate reads it via the APIReader log
  path) for the **single failing check** (a verify run with multiple
  failing checks hashes the **first** failing check in declaration order —
  the iterate branch already names `failedCheck`; the normaliser sees that
  check's output only, so a different check failing produces a different
  hash, which is correct: the stall is on *the same* check).
- **Rules (versioned; `normalisationVersion = "v1"` — the constant
  `stall.NormalisationVersionV1`):** applied in order, then the result is
  SHA-256'd:
  1. **Strip timestamps:** any line beginning with (or containing) an
     RFC3339 timestamp, a Go `log`-format `2006/01/02 15:04:05` prefix, or
     a Unix/epoch number in a leading column → the timestamp token is
     removed (the line is kept, the token gone).
  2. **Strip hex addresses:** any `0x`-prefixed hex of ≥4 digits (pointers,
     PC addresses in stack traces) → the hex digits are replaced by a fixed
     `0xADDR` placeholder (the length is not preserved — two addresses of
     different lengths normalise to the same placeholder; that is intended,
     addresses are noise).
  3. **Strip temp paths:** any path containing `/tmp/`, `/var/tmp/`,
     `os.TempDir()`'s shape (a `/tmp/go-build*`, `/tmp/Test*` pattern), or a
     `workdir`-shaped temp (`/tmp/.*-<hex>-<n>`) → the volatile suffix after
     the temp root is replaced by a fixed `TMPDIR/` prefix + a
     `PATH_PLACEHOLDER` (the directory depth is not preserved).
  4. **Strip line numbers:** Go panic/test-format `file.go:123:4` →
     `file.go:LINE:COL` (the file path is kept — a *different* file
     failing is a different failure; the number is noise). Test-framework
     `--- FAIL: TestX (1.23s)` durations → the `(Ns)` is stripped.
  5. **Collapse blank runs:** 3+ consecutive blank lines → 1.
  6. **Trim trailing whitespace per line; drop trailing blank lines.**
- **Why these rules:** they target the noise that makes two *identical*
  failures hash differently (timestamps, addresses, temp paths, line
  numbers are the four PLAN.md names). Anything not stripped is preserved
  — the hash is on the **substance** of the failure. The rules are a
  **versioned constant** (PLAN.md's `normalisationVersion`): a v2 that
  strips more is a new version, and a `stallHistory` entry's
  `normalisationVersion` says which rules produced its hash, so a mixed
  history (v1 then v2) is auditable. **The operator compares consecutive
  hashes for equality WITHIN the same `normalisationVersion`** (a v1→v2
  upgrade resets the consecutive count — a v1 hash and a v2 hash of the
  same output can differ by construction; comparing across versions would
  false-negative a stall at the upgrade).

**Unit tests (written first, `internal/stall/`):**
- **Golden files (table-driven):** a `testdata/` of named inputs + expected
  normalised outputs + expected hashes:
  - `panic-go.txt` (a Go panic with `0x` addresses, `file.go:123:4`, a
    timestamp) → the golden output (addresses → `0xADDR`, line numbers →
    `:LINE:COL`, timestamp stripped) + the golden hash.
  - `test-fail.txt` (a `go test` failure: `--- FAIL: TestX (0.42s)`, a
    `file.go:42:` line, a temp dir `/tmp/TestX123456/`) → the golden
    output + hash.
  - `two-identical-after-normalise.txt` — **two raw inputs that differ only
    in the noise** (one has `2026-07-03T12:00:00Z`, the other
    `2026-07-04T13:33:11Z`; one has `0xdeadbeef`, the other `0xcafebabe`;
    one has `/tmp/foo-1/`, the other `/tmp/foo-2/`) → **the same normalised
    output and the same hash** (the core property: the noise is stripped,
    the substance survives).
  - `two-different-substance.txt` — two inputs whose *substance* differs
    (a different file, a different error message) → **different hashes**
    (the normaliser does not over-collapse: two genuinely different
    failures must not hash equal).
  - `empty.txt` → the empty normalised output + its (defined) hash.
  - `no-noise.txt` — input with none of the four noise types → the output
    is byte-identical to the input (the normaliser is a no-op on clean
    input; guards against over-eager stripping).
- **Rule order:** a timestamp that contains a hex-looking substring
  (e.g. `2026-07-03T0x12:00:00Z` — a malformed timestamp) → the timestamp
  rule runs first (rule 1 before rule 2); the test asserts the timestamp is
  stripped and the surviving `0x12` (if any) is NOT hex-stripped (it's
  <4 digits) — the order is pinned.
- **Version pin:** the `NormalisationVersionV1` constant's value is
  asserted (`"v1"`); a test that constructs a v1 hash and asserts the
  `StallEntry` carries `"v1"`.

**Gate mutation (I49 norm, scratch worktree):** **remove one rule** (e.g.
rule 2, the hex-address strip) from the normaliser → the golden-file spec
`panic-go.txt` FAILS (the output keeps the raw `0x` addresses; the hash
differs from the golden). **Each rule has its own named mutation** (the
PR records all five; the two core property specs — `two-identical-
after-normalise` and `two-different-substance` — must fail on any rule
removal that affects their inputs).

### The operator's stall gate (envtest)

- **Evidence:** on a verify **failure** (the iterate branch, P2d's sibling
  in `loop_verify_job.go`), the operator reads the failing check's log
  (the APIReader log path), normalises it (P2e's normaliser), hashes it,
  and appends a `StallEntry` to `status.stallHistory` (capped at 10).
- **The decision (pure function of `status.stallHistory` + the effective
  stall config):** count the **consecutive** trailing entries with the same
  `hash` **and** the same `normalisationVersion` (a version change resets
  the count, per the normaliser section). Let `k` be that count.
  - **`k < stallAfter` → no decision** (the Loop iterates as today; the
    `Stalled` condition is `False`/absent).
  - **`k == stallAfter` (and not already decided) → fire** (exactly once;
    the decision is sticky like `exceeded` — a Loop that has fired its
    stall does not re-fire on the next identical iteration). Apply
    `stallAction`:
    - **`Fail`** → `phase=Failed`, `Failed` condition `True` reason
      `Stalled`, `Stalled=True`, the existing `Failed` cleanup, a
      `Warning` Event (`Reason: Stalled`, the message names the check and
      the N).
    - **`Pause`** → `phase=Paused`, `status.pausedFrom=<phase>`,
      `Stalled=True`, a `Warning` Event (the sandbox suspension is P2f's
      gate).
    - **`Continue`** → `Stalled=True` + a `Warning` Event, **the phase is
      unchanged** (the Loop iterates on; the condition records the state
      for observability — PLAN.md's "warn and keep going").
  - **Consecutive only (PLAN.md's scope):** an entry sequence
    `[A, B, A, A, A]` with `stallAfter=3` → the trailing run is `A, A, A`
    (3 consecutive `A`s after the `B` broke the earlier `A` run) → fires.
    A sequence `[A, B, A, B, A, B]` (oscillation) → **never fires** (no
    run of 3 consecutive equal hashes) — the oscillation is deferred per
    PLAN.md; the spec asserts the `Stalled` condition stays `False`/absent
    and the Loop keeps iterating (the maxIterations cap is the only stop
    for an oscillator — the existing `MaxIterationsExceeded` path,
    unchanged).
- **Effective config:** the operator reads `spec.loop.stallAfter`/
  `stallAction` (P2c) with the defaults (3 / `Fail`). **The cluster-wide
  default ConfigMap** (`coxswain-stall-defaults`, in the operator's
  namespace; keys `stallAfter`, `stallAction`) is the fallback when the
  Loop's field is unset **and** the PLAN.md default would apply — i.e. the
  precedence is: **Loop field > ConfigMap > built-in default (3 / Fail)**.
  A ConfigMap that is missing or malformed → the built-in default (the
  stall detector is not fail-closed on a missing ConfigMap: a stall that
  goes undetected is worse than one that uses the default; this is the
  opposite of the budget's fail-closed prices, and it is deliberate —
  documented in the field comment).
- **In-progress (I49 norm):** the stall decision reads the **verify Job's
  pod/container status** (the exit codes + the log read). A verify Job that
  is **Waiting** (init not started) or **Running** (a check container not
  terminated), or a Job with neither `Failed` nor `Succeeded` → **no
  decision** (no `StallEntry` appended, no fire, a requeue). The
  `StallEntry` is appended **only** on a terminal verify failure (a
  check-* container `Terminated` with a non-zero exit code, the existing
  B3 evidence).

**Envtest-first tests (`internal/controller/loop_p2e_stall_test.go`):**
1. **Consecutive fires.** `stallAfter=3`; three verify iterations that fail
   the **same check with the same normalised output** (three
   `StallEntry` appends with the same `hash`) → on the third,
   `phase=Failed` (default `stallAction=Fail`), `Failed` reason `Stalled`,
   `Stalled=True`, the Event.
2. **Different output resets the run.** `stallAfter=3`; outputs
   `[X, Y, X, X]` (the same check, but Y's substance differs) → after 4
   iterations **no fire** (the trailing run is `X, X` = 2 < 3); the 5th
   iteration (output X again) fires (the run is `X, X, X`).
3. **`stallAction=Pause`.** Same as spec 1 but `stallAction: Pause` →
   `phase=Paused`, `status.pausedFrom=Verifying` (the phase the iterate
   would have left — actually the phase at fire time is `Verifying`, the
   check just failed; `pausedFrom=Verifying`), `Stalled=True`, the sandbox
   is **not** torn down.
4. **`stallAction=Continue`.** Same as spec 1 but `stallAction: Continue`
   → `phase` is **unchanged** (`Verifying`, the iterate proceeds to
   `Implementing` as today), `Stalled=True`, the Event fired, **the next
   iteration is allowed** (the cap is the maxIterations one, unchanged).
5. **Oscillation never fires.** `stallAfter=3`; outputs `[X, Y, X, Y, X,
   Y]` over 6 iterations → **no fire** after 6 (the `Stalled` condition is
   `False`/absent); the Loop stops only via `maxIterations` (the existing
   `MaxIterationsExceeded` path — the spec asserts the phase reaches
   `Failed` with reason `MaxIterationsExceeded`, **not** `Stalled`, when
   the cap is hit).
6. **Version change resets the count.** Two `StallEntry`s at `v1` with the
   same hash, then the normaliser "upgrades" to `v2` (simulated by a
   `StallEntry` with `normalisationVersion: v2` and the same *raw* output —
   the test writes the entries directly, as the S4/B3 tests do) → the
   consecutive count is 1 (not 3) → no fire.
7. **In-progress: verify Job Running → no decision (I49).** A verify Job
   pod with a check container `Running` (not `Terminated`) → no
   `StallEntry`, no fire, a requeue (the spec asserts `stallHistory` is
   unchanged and the phase is unchanged).
8. **In-progress: verify Job Waiting (init not started) → no decision
   (I49).** A verify Job pod with the init containers not yet terminated
   → same as spec 7.
9. **Sticky fire.** After a fire (spec 1's shape), the operator re-reconciles
   with the same evidence → no re-fire (the condition is already `True`,
   the phase is already `Failed`; the spec asserts no second Event and no
   phase change).
10. **ConfigMap override.** A `coxswain-stall-defaults` ConfigMap with
    `stallAfter: 2`; a Loop with **no** `spec.loop.stallAfter` → fires at
    2 consecutive (the ConfigMap's value, not the built-in 3). A Loop with
    `spec.loop.stallAfter: 5` → the ConfigMap is ignored (the Loop field
    wins).
11. **Same-Loop update (I43 norm):** after a `Continue` fire (spec 4's
    shape), **update `spec.loop.stallAfter` to a lower value and
    re-reconcile from the API server** → the fire is already recorded
    (`Stalled=True` sticky); the phase is unchanged by the edit (the spec
    asserts the edit does not re-fire or clear the condition — the
    condition is a record, not a decision input).
12. **Capped history.** 12 consecutive identical failures →
    `status.stallHistory` has exactly 10 entries (the cap), the fire
    happened at the 3rd (the cap does not delay the fire).

**Gate mutations (I49 norm, scratch worktree, each must make its spec FAIL):**
- **Compare hashes across `normalisationVersion`s** (drop the version
  equality from the consecutive count) → spec 6 FAILS (the count is 3, not
  1, and the spec expects no fire).
- **Fire on non-consecutive repetition** (count total occurrences, not
  trailing consecutive) → spec 5 FAILS (the oscillator fires, the spec
  expects `MaxIterationsExceeded`, not `Stalled`).
- **Append a `StallEntry` on a non-terminal verify** (drop the terminal
  gate) → spec 7 FAILS (a `StallEntry` appears while the check is
  `Running`).
- **Ignore the ConfigMap** (always use the built-in default) → spec 10's
  "ConfigMap override" half FAILS (the fire is at 3, not 2).
- **Re-fire on re-reconcile** (drop the sticky gate) → spec 9 FAILS
  (a second Event is emitted).

**Acceptance:** unit suite (the golden files) + envtests green under
`make test`; the `internal/stall/` package has **no** import of the
controller (the normaliser is pure; the controller imports it, not vice
versa — a `go vet`-style guard the PR records).

**Dependencies:** P2c (the `stallHistory` + `spec.loop.stall*` fields), the
S5a/B3 iterate branch (the stall gate is a sibling decision in the same
evidence path). The normaliser is independent (pure function).

---

## P2f — The `Paused` phase: `pausedFrom`, the sandbox suspension, resume

**Scope:** make `Paused` a real phase the Loop can enter (from P2d's
`onExceeded=Pause`, P2e's `stallAction=Pause`, or `spec.suspend=true`) and
resume from, with the sandbox **suspended** while paused and the resume
returning to **the exact phase** the Loop left.

- **Entering `Paused` (three entry points, one mechanism):**
  - `spec.suspend=true` (the existing S1 path, **upgraded**): today S1
    suspends the sandbox but the **phase** keeps advancing (the claim
    reader still reads; the verify Job still runs). From P2f,
    `spec.suspend=true` **also** sets `phase=Paused`,
    `status.pausedFrom=<current phase>` (the first time only — a
    re-reconcile with `suspend` still true does not overwrite
    `pausedFrom`), the `Paused` condition (a new condition type, the
    `Stalled`/`BudgetExceeded` pattern), and a `Normal` Event
    (`Reason: Paused`, the message names the source: `suspend`, `stall`,
    or `budget`). The sandbox's `OperatingMode` is `Suspended` (the
    existing S1 branch, now the **only** thing that sets it for
    `spec.suspend` — P2f adds the phase + condition).
  - P2d's `onExceeded=Pause` and P2e's `stallAction=Pause` enter the same
    way (they set the phase + `pausedFrom` + their own condition; the
    sandbox suspension is this slice's gate, below — they do not each
    re-implement it).
- **The suspension gate (the core of this slice):** a Loop in `phase=Paused`
  has its sandbox `OperatingMode=Suspended` **regardless of `spec.suspend`**
  (a budget-paused or stall-paused Loop has `spec.suspend=false` — the
  pause is the operator's, not the user's). `ensureSandbox`'s existing
  `spec.suspend` branch is generalised: `desired.OperatingMode =
  Suspended` iff `spec.suspend || phase==Paused` (the D30/D35a gates still
  apply on top — a paused Loop whose proxy is not Ready is Suspended for
  both reasons; the gates are additive, as today). **The verify Job does
  not run while paused:** the verify Job's `ensure` (the B3 path) is gated
  on `phase != Paused` (a paused Loop has no in-flight verify; the Job, if
  one was running at pause time, is left to terminate — the operator does
  **not** delete it on pause; a verify Job that terminates while the Loop
  is paused produces no decision (P2d/P2e's in-progress gate: the phase is
  `Paused`, not `Verifying`, so the iterate/stall/budget decisions are
  inert — the spec asserts this).
- **`pausedFrom` (the exact-phase record):** set on entry (the phase the
  Loop left, e.g. `Implementing` or `Verifying`), **never overwritten while
  paused** (a re-reconcile keeps the original), **cleared on resume** (set
  to empty). The claim reader is **inert while paused** (no claim is read;
  the agent container is stopped, so there is no termination message — but
  the gate is explicit: a claim that *does* arrive while paused, e.g. from
  a pod that was mid-run at pause time, is **ignored** (not advanced) —
  the Loop is paused; a stale claim from before the pause must not advance
  the phase. The existing `nextPhase` is gated on `phase != Paused`).
- **Resume (returns to the exact phase):** resume is via
  **`spec.suspend=false`** (the user's resume of a `suspend`-paused Loop)
  **or an annotation** `coxswain.io/resume: "<phase>"` (the operator's or
  user's explicit resume of a budget/stall-paused Loop — the annotation
  names the phase to return to; it is **cleared by the operator on the
  resume** (a one-shot), and a resume annotation that names a phase the
  Loop is not `pausedFrom` is **ignored** (fail-closed: you can only
  resume to the phase you paused from; a mismatch is a `Warning` Event,
  the annotation is not cleared). On a valid resume:
  - `phase = pausedFrom` (the exact phase), `pausedFrom` cleared, the
    `Paused` condition set `False` (reason `Resumed`), a `Normal` Event
    (`Reason: Resumed`, the message names the phase).
  - **The sandbox `OperatingMode` returns to `Running`** (the suspension
    gate releases: `phase != Paused` and `spec.suspend=false` → `Running`,
    subject to the D30/D35a gates — a proxy that is not Ready re-holds
    Suspended, as today).
  - **The phase machine resumes where it left off:** the claim reader
    re-reads on the next phase recycle (the per-phase pod recycle
    re-creates the agent container with the resumed phase as
    `desiredPhase` — the S4 mechanism, unchanged; the resume does not
    re-iterate, it re-enters the same phase the Loop was in). **A
    resumed-from-`Verifying` Loop re-runs the verify** (the verify Job is
    re-created for the current `currentVerify.verifiedCommit` pin — the
    B3 path, unchanged; the pause did not clear the pin). **A
    resumed-from-`Implementing` Loop re-runs the Implementing phase** (the
    per-phase recycle, unchanged).
  - **A budget-exceeded Loop that resumes stays exceeded** (P2d's sticky
    `exceeded`: the resume re-enters `Implementing`/`Verifying`, and the
    next reconcile re-sees `exceeded=true` → the Loop re-enters
    `Failed`/`Paused` immediately. This is the intended fail-closed
    semantics (P2d): a budget you have already exceeded is not cleared by
    a pause. **The owner's escape is a fork** (ADR-0003: a new Loop from a
    checkpoint), not a resume of an exceeded Loop — documented in the
    `pausedFrom` field comment.)
- **Terminal `Failed` is not paused-from-able:** a Loop that is `Failed`
  (terminal) cannot be `pausedFrom` (the phase is terminal; `pausedFrom`
  is only set from a non-terminal phase). A `spec.suspend=true` on a
  `Failed` Loop → the sandbox is `Suspended` (the S1 branch) but the
  **phase stays `Failed`** and `pausedFrom` is **not** set (a failed Loop
  is not paused; it is already stopped). The spec asserts this.

**Envtest-first tests (`internal/controller/loop_p2f_paused_test.go`):**
1. **`spec.suspend=true` → `Paused`.** A Loop at `Implementing`,
   `spec.suspend` flipped to `true`, re-reconcile → `phase=Paused`,
   `status.pausedFrom=Implementing`, the `Paused` condition `True`, the
   Event, the sandbox `OperatingMode=Suspended`.
2. **`pausedFrom` not overwritten.** Re-reconcile with `suspend` still
   true → `pausedFrom` is still `Implementing` (not re-set), the phase is
   still `Paused` (the spec asserts the value is unchanged across two
   reconciles).
3. **The verify Job does not run while paused.** A paused Loop (spec 1's
   shape) at `pausedFrom=Verifying` with a verify Job that **was** running
   at pause time (the pod exists, a check container `Terminated` non-zero)
   → the operator does **not** create a new verify Job, and the existing
   Job's termination produces **no** iterate/stall/budget decision (the
   phase stays `Paused`, `stallHistory` and `status.budget` are
   unchanged).
4. **A stale claim while paused is ignored.** A paused Loop with an agent
   container that has a termination message (a claim naming the
   immediate-next phase) → the operator does **not** advance the phase
   (the `nextPhase` gate on `phase != Paused`; the phase stays `Paused`).
5. **Resume via `spec.suspend=false` → exact phase.** A paused Loop
   (spec 1) with `suspend` flipped to `false` → `phase=Implementing`
   (the `pausedFrom`), `pausedFrom` cleared, the `Paused` condition
   `False` reason `Resumed`, the Event, the sandbox `OperatingMode`
   returns to `Running` (the D30/D35a gates satisfied in the test
   fixture).
6. **Resume via annotation.** A budget-paused Loop (`spec.suspend=false`,
   `phase=Paused`, `pausedFrom=Verifying`) with annotation
   `coxswain.io/resume: "Verifying"` → resume (as spec 5, the phase
   returns to `Verifying`, the annotation is **cleared** by the operator
   on the resume).
7. **A mismatched resume annotation is ignored.** The same Loop with
   `coxswain.io/resume: "Implementing"` (a phase that is NOT
   `pausedFrom`) → **no resume** (the phase stays `Paused`, the annotation
   is **not** cleared, a `Warning` Event).
8. **A resumed budget-exceeded Loop re-pauses.** A budget-paused Loop
   (P2d spec 3's shape, `exceeded=true`, `pausedFrom=Implementing`),
   resumed via the annotation (spec 6) → the phase re-enters
   `Implementing` **and the next reconcile re-sees `exceeded=true`** →
   the Loop re-enters `Paused` (or `Failed` on `onExceeded=Fail`)
   immediately (the sticky-`exceeded` fail-closed, P2d).
9. **`Failed` is not pausable.** A `Failed` Loop with `spec.suspend=true`
   → the sandbox is `Suspended` (the S1 branch) but `phase` stays
   `Failed`, `pausedFrom` is **not** set (empty).
10. **In-progress: a pod mid-run at pause time (I49).** A Loop at
    `Implementing` with the agent container `Running` (not terminated) and
    `spec.suspend` flipped to `true` → the phase is `Paused`,
    `pausedFrom=Implementing`, and the **in-flight agent run's claim (when
    it terminates) is ignored** (spec 4's gate; the spec asserts the
    phase does not advance on the termination).
11. **Same-Loop update (I43 norm):** after a resume (spec 5), **update
    `spec.suspend` to `true` again and re-reconcile from the API
    server** → the Loop re-enters `Paused` with `pausedFrom=Implementing`
    (the phase it was in, not the stale pre-pause value) — the spec
    asserts the re-pause records the *current* phase, and that the
    sandbox is `Suspended` again.
12. **Resume does not re-iterate.** A paused-from-`Verifying` Loop
    (spec 6) resumed → the `status.iteration` is **unchanged** (the resume
    re-enters `Verifying` at the same iteration; the verify re-runs, but
    the iteration count is not bumped by the resume itself — it is
    bumped by the next *verify failure*, as today).

**Gate mutations (I49 norm, scratch worktree, each must make its spec FAIL):**
- **Drop the `phase != Paused` gate from the claim reader** (a claim
  advances the phase even while paused) → spec 4 FAILS (the phase
  advances).
- **Overwrite `pausedFrom` on every reconcile** (drop the "first time
  only" guard) → spec 2 FAILS (the value changes on the second reconcile).
- **Clear `exceeded` on resume** (drop P2d's sticky guard at the resume
  site) → spec 8 FAILS (the resumed Loop does not re-pause).
- **Allow a mismatched resume annotation** (drop the `pausedFrom` match
  check) → spec 7 FAILS (the resume proceeds).
- **Set `pausedFrom` on a `Failed` Loop** (drop the terminal-phase guard)
  → spec 9 FAILS (`pausedFrom` is set).

**Acceptance:** envtests green under `make test`; the `Paused` phase is
reachable from all three entry points and the resume returns to the exact
phase in every spec; the S1 specs (`loop_suspend_test.go`) are updated to
assert the **phase** is `Paused` (not just the sandbox's `OperatingMode`)
— a mutation of the S1 spec (drop the phase assertion) is recorded as
failing.

**Dependencies:** P2c (the `pausedFrom` field), P2d/P2e (the entry points
that set `phase=Paused` with a budget/stall reason — P2f generalises the
sandbox suspension they rely on). The S4 claim reader (the
`phase != Paused` gate).

---

## P2g — Conditions + events for every transition (the auditability seam)

**Scope:** verify that **every** phase/budget/stall/pause transition in
Phase 2 emits both a **condition** (on `status.conditions`) and an
**Event** (via the existing `Recorder`), and add any missing ones. This is
a **sweep slice** (no new behaviour — it asserts the P2d/P2e/P2f
transitions are all auditable per PLAN.md's "conditions and events for
every transition").

- **The transition inventory (each needs a condition + an Event):**
  - `→ Paused` (from `suspend`, from stall `Pause`, from budget
    `Pause`) — the `Paused` condition (P2f) + the `Paused` Event (the
    message names the source: `suspend`/`stall`/`budget`).
  - `Paused → <phase>` (resume) — the `Paused` condition `False` reason
    `Resumed` + the `Resumed` Event.
  - `→ Failed` reason `Stalled` (stall `Fail`) — the `Failed` condition
    reason `Stalled` + the `Stalled` Event (P2e).
  - `→ Failed` reason `BudgetExceeded` (budget `Fail`) — the `Failed`
    condition reason `BudgetExceeded` + the `BudgetExceeded` Event (P2d).
  - `Stalled=True` with `stallAction=Continue` (no phase change) — the
    `Stalled` condition `True` + the `Stalled` Event (P2e; the condition
    records the state even though the phase is unchanged).
  - `BudgetExceeded=True` (the condition, before the phase action) —
    the `BudgetExceeded` condition `True` + the Event (P2d).
- **The sweep:** a table-driven envtest spec that, for **each** transition
  above, asserts (a) the condition is set with the right type/reason/status
  and a non-empty message, and (b) an Event with the right `Reason` and a
  non-empty message is recorded (the existing `Recorder` is a fake in
  envtest — the spec reads the recorded events, as the OS5 specs do).
  **Missing transitions are added here** (if P2d/P2e/P2f forgot an Event,
  this slice adds it — the sweep is the enforcement, not the
  implementation).

**Envtest-first tests (`internal/controller/loop_p2g_audit_test.go`):**
1. **`→ Paused` (suspend source).** A Loop paused via `spec.suspend` →
   the `Paused` condition `True` + a `Paused` Event whose message contains
   `suspend`.
2. **`→ Paused` (stall source).** A Loop paused via stall `Pause` → the
   `Paused` condition `True` + the `Stalled` condition `True` + a
   `Paused` Event whose message contains `stall`.
3. **`→ Paused` (budget source).** A Loop paused via budget `Pause` → the
   `Paused` condition `True` + the `BudgetExceeded` condition `True` + a
   `Paused` Event whose message contains `budget`.
4. **Resume.** A resumed Loop → the `Paused` condition `False` reason
   `Resumed` + a `Resumed` Event.
5. **`→ Failed:Stalled`.** A stall-`Fail` Loop → the `Failed` condition
   reason `Stalled` + the `Stalled` condition `True` + a `Stalled` Event.
6. **`→ Failed:BudgetExceeded`.** A budget-`Fail` Loop → the `Failed`
   condition reason `BudgetExceeded` + the `BudgetExceeded` condition
   `True` + a `BudgetExceeded` Event.
7. **`Continue` (no phase change).** A stall-`Continue` Loop → the
   `Stalled` condition `True` + a `Stalled` Event, the phase is unchanged.
8. **Every transition has a non-empty message.** The table-driven
   assertion (a structural check: no condition or Event in the above has
   an empty message — PLAN.md's auditability: "all auditability comes from
   `status.history[]` + Kubernetes events, never logs alone").

**Gate mutation (I49 norm, scratch worktree):** **drop one Event** (e.g.
the `Resumed` Event) from its transition site → specs 4 and 8 FAIL (the
Event is absent). (The sweep is the gate: a missing transition is caught
here, not in the implementing slice.)

**Acceptance:** envtests green; the transition inventory is complete
(every P2 transition has a condition + an Event with a non-empty message).

**Dependencies:** P2d, P2e, P2f (the transitions it sweeps).

---

## P2h — Kind acceptance (the PLAN.md "Done when")

**Scope:** `test/e2e/p2-e2e.sh` (+ `make p2-e2e`, pinned to
`--context kind-coxswain-dev`, the d41-e2e pattern). The PLAN.md Done-when:
**"a deliberately impossible goal stops cleanly with the right reason
(`Stalled` or `BudgetExceeded`) instead of spinning, and a paused Loop
resumes where it left off."** Plus the P2a Prometheus cross-check.

**Fixture:** a kind cluster with the operator + a **stub model server**
(an in-kind pod, e.g. `python:3.12` serving an OpenAI-compatible `/v1/chat/
completions` that always returns a fixed `usage: {prompt_tokens: 100,
completion_tokens: 100}` and a completion that says "I cannot do this" —
the deliberately impossible goal: the agent implements, verifies, the
verify fails with the same output every time, and the model burns 200
tokens per iteration). A Loop with `spec.verify.acceptanceChecks` that
**always fails** (a `test -f /nonexistent` check), `spec.loop.stallAfter:
3`, `stallAction: Fail` (the `Stalled` path) in one Loop, and a second
Loop with `spec.budget.maxTokens: 400`, `onExceeded: Fail` (the
`BudgetExceeded` path) with `stallAction: Continue` (so the stall does not
fire first — the budget fires at iteration 3, 600 tokens > 400).

Assertions (script + recorded output in the PR):
1. **The `Stalled` path.** The stall Loop (3 identical verify failures)
   → `kubectl get loop` shows `phase=Failed`, the `Failed` condition
   reason `Stalled`, the `Stalled` condition `True`; **the Loop stopped at
   3 iterations, not spun** (`status.iteration == 3`); the Event
   (`kubectl get events`) shows the `Stalled` reason. **Instead of
   spinning:** without the stall detector (a control Loop with
   `stallAfter` unset to a high value, `maxIterations` high) the same
   fixture would iterate to `maxIterations` — the control Loop is run in
   parallel and its `status.iteration` is asserted to exceed 3 (the stall
   detector is what stopped the first Loop).
2. **The `BudgetExceeded` path.** The budget Loop → `phase=Failed`, the
   `Failed` condition reason `BudgetExceeded`, `status.budget.exceeded=
   true`, `exceededReason=Tokens`, the Event. The per-Loop token count in
   `status.budget` (600 at fire) is asserted.
3. **The Prometheus cross-check (P2a).** The stub model server's pod
   exposes the homelab-Prometheus-style counters (a simple
   `/metrics` with `vllm_prompt_tokens_total` /
   `vllm_generation_tokens_total` incremented per request — a stand-in
   for the real vLLM metrics, cluster-wide by construction). Over the
   budget Loop's wall-clock window, the backend counter's **delta** is
   consistent with the per-Loop `status.budget` count (the per-Loop count
   is ≤ the cluster-wide delta, and within the delta's tolerance given
   the control Loop's concurrent tokens — the assertion is a
   consistency check, not an equality, per P2a). This is the
   **acceptance cross-check** (the cluster-wide metrics are not the
   per-Loop source of truth — the proxy's audit log is).
4. **The paused-Loop resume.** A third Loop: `spec.suspend` flipped
   `false → true → false` at `Implementing` → `phase=Paused`,
   `status.pausedFrom=Implementing`, the sandbox pod is **terminated**
   (`kubectl get pod` shows no running sandbox pod while paused), then
   resume → `phase=Implementing`, `pausedFrom` cleared, the sandbox pod
   is **re-created and Running**, and the Loop **continues from
   Implementing** (the verify Job that eventually runs is for the
   Implementing-produced commit, not a re-plan — the assertion is that
   the resumed Loop's `status.iteration` and `currentVerify.verifiedCommit`
   are consistent with the pre-pause state, not reset).
5. **The budget-`Pause` + resume fail-closed.** A fourth Loop:
   `onExceeded: Pause`, `maxTokens` small → `phase=Paused`,
   `pausedFrom=Verifying`; resume via the annotation → the Loop re-enters
   `Verifying` **and immediately re-pauses** (the sticky `exceeded`, P2d)
   — the assertion is that the re-pause happens on the next reconcile
   (the `Paused` condition is `True` again, `exceeded` still true).

**Gate mutation (I49 norm, scratch worktree):** run the script against a
scratch-build image with the **stall gate disabled** (the P2e mutation:
drop the fire) → assertion 1 FAILS (the stall Loop spins to
`maxIterations` instead of stopping at 3 with `Stalled`). And with the
**budget gate disabled** (the P2d mutation: skip the phase transition) →
assertion 2 FAILS (the budget Loop does not `Failed:BudgetExceeded`).
Record the output.

**Acceptance:** `make p2-e2e` green on `kind-coxswain-dev`; the output
(the `kubectl get loop` outputs, the events, the Prometheus cross-check
numbers, the resume pod lifecycle) committed in the PR description — the
R22 process note: evidence numbers in the PR body are copied from this
artifact, not recalled.

**Dependencies:** P2a–P2g merged; the kind cluster; the stub model server
(a new in-kind fixture, the d41-e2e's in-kind upstream pattern).

---

## Slice order and dependencies

```
P2a (ADR: meter in the model proxy) ──→  P2b (cmd/model-proxy metering binary)
                                              │
P2c (API: budget + stallHistory + stall* + budget spec) ──┤
                                                           ├──→  P2d (budget decision, operator reads the audit log)
                                                           ├──→  P2e (stall detection: normaliser + gate)
                                                           └──→  P2f (the Paused phase: pausedFrom, suspension, resume)
                                                                    │
                                                                    ├──→  P2g (conditions + events sweep)
                                                                    └──→  P2h (kind acceptance: the Done-when)
```

- P2a and P2c are independent (parallel-safe; P2a is docs, P2c is API).
- P2b depends on P2a (the design it implements).
- P2d depends on P2a (the metering seam), P2b (the audit log it reads),
  P2c (the fields it writes).
- P2e depends on P2c (the `stallHistory` + `spec.loop.stall*` fields).
  The normaliser is independent (a pure function; it can be built in
  parallel with P2c).
- P2f depends on P2c (the `pausedFrom` field) and generalises the
  suspension P2d/P2e rely on (it can land before or after P2d/P2e, but its
  specs reference their entry points — land it after at least one of
  P2d/P2e so the entry-point specs have a target).
- P2g depends on P2d, P2e, P2f (the transitions it sweeps).
- P2h depends on all of the above.

**Exit criteria** (the PLAN.md Done-when): the ADR (P2a) written +
reviewed; kind evidence for all five P2h assertions (the `Stalled` stop,
the `BudgetExceeded` stop, the Prometheus cross-check, the paused-Loop
resume, the budget-`Pause` fail-closed resume); a deliberately impossible
goal stops with the right reason instead of spinning; a paused Loop
resumes where it left off.

**Out of scope** (deferred, per PLAN.md + the owner's scope note):
- **Oscillation detection** (non-consecutive repetition; PLAN.md defers it
  explicitly — the maxIterations cap is the only stop for an oscillator).
- **Any metering decision by the proxy** (P2a: the proxy reports only; the
  operator decides).
- **Tool-call metering** (P2a: the token/cost budget counts model tokens
  only).
- **UI** (no dashboard for `status.budget`/`stallHistory`; `kubectl get
  loop` + the conditions are the interface).
- **Phase 3 durability** (a `stallHistory`/`budget` that survives an
  operator restart is Phase 3's job; Phase 2's `status` is the
  operator's record, reset on a status-loss as today).
- **Per-request metering beyond `usage`** (no per-token streaming
  attribution; the final `usage` is the count).
