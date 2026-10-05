# Phase 2 — TDD plan: stopping properly (stall detection, token budgets, Paused/resume)

Phase 2 of `docs/PLAN.md` ("Stopping properly") gives the operator the three
mechanisms that stop a Loop which cannot succeed: **stall detection** (N
consecutive identical failing-check outputs), **budgets** (token, wall-clock
and cost caps), and the **`Paused` phase** (with `status.pausedFrom`,
`status.pausedReason`, and resume to the exact phase). Conditions + events
ride on every new transition.

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

**R23 review revisions (2026-10, reviewer Verdict CHANGES on the round-1
plan):** the metering channel is a **cumulative HTTP counter read** (not
pod logs — the operator has no `pods/log` RBAC and envtest has no kubelet),
restart-safe via a **boot ID + cumulative deltas** (item 1–2); the proxy is
**agent-steering-proof** (strips `Accept-Encoding`, forces
`stream_options.include_usage`; item 3); the `Paused` phase records
**`status.pausedReason`** and `suspend=false` resumes **only** `Suspend`
pauses (item 4); a budget pause **re-evaluates the current caps** on resume
(item 5); stall entries are **deduped by verify-Job name** and a stall pause
lands in `Implementing` with the run kept (item 6). The P2/P3 items (7–17)
are incorporated: normaliser run-specific-token rules + golden files (7),
stall-vs-`maxIterations` precedence (8), pause-from-every-phase + the
wall-clock-while-paused case (9), wall-clock accumulated-active-time +
`RequeueAfter` + `>=` semantics (10), slice ownership of the proxy image
wiring + `modelPrices` in P2c + flat price ConfigMap + P2f-before-P2d/P2e
ordering (11), the missing gate mutations (12), per-slice kind evidence for
the pod/suspension changes (13), P2h robustness incl. a real-backend
cross-check (14), the dropped `/metrics` debug endpoint (15), the
reworded isolation claim (16), and the etcd-durability correction (17).

Build conventions (same as D41/I42): each slice is red→green; envtest-first
where the operator is involved, unit tests for binaries and pure functions;
kind acceptance at the end (P2h) on `--context kind-coxswain-dev`, plus
**per-slice kind runs for every slice that changes a pod or the sandbox
suspension** (P2b, P2d, P2f — item 13): each slice's PR records its
kind-run output (the running image digest + the assertions it ran), so the
kind evidence is per-slice, not only P2h. Every gate this plan adds is
mutation-checked per the R20 I49 norm: the named mutation is applied in a
**scratch worktree only**, the affected spec must FAIL, and the result is
recorded in the PR. Same-Loop update/delete specs per the I43 norm;
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
  `defaultMaxIterations = 3`); `verifyJobName` — `<loop>-verify-<iteration>`
  (the Job is **recreated on every re-run**: a B4 re-run creates a NEW Job,
  never mutating the prior one — this is why the stall gate dedups by Job
  name, item 6); `emitVerifyIteratedEvent` (the OS5 Event pattern).
- `internal/controller/loop_controller.go` — `nextPhase` (claim-driven
  transition table, B1); `ensureSandbox` (the `spec.suspend` →
  `OperatingMode=Suspended` branch, ~line 1241); `emitPhaseAdvancedEvent`
  (the OS5 Event pattern); `finalizeLoopStatus` / `setCondition`; the
  spec-hash drift-recreate pattern; `LoopReconciler` config fields (the
  `--verify-image`, `ProxyImage`, `EgressProxyImage`, `ToolProxyImage`
  pattern for new flags — **`ProxyImage` already exists**; P2b's wiring
  slice flips its default from the stand-in to the metering image);
  `readBaseCommitFromTerminationMessage` — the **operator read-back
  pattern**: a real, non-cached `apiReader` (an `APIReader` field, with a
  func-field injection seam on the reconciler for envtest), reading
  **kubelet-recorded pod status** (a termination message), with
  **no exec/kubelet/ReadFile access and no new RBAC** (the operator's pod
  verbs are `get;list;watch;create;update;delete` — **no `pods/log`**).
- `internal/controller/loop_s4_phase.go` — the ADR-0004 claim reader
  (one-shot per phase, termination-message channel, strict-parsed, never a
  gate input). Same seam as above: **termination messages, not pod logs.**
- `internal/controller/loop_suspend_test.go` — the S1 suspend specs
  (reconcile → Sandbox `OperatingMode=Suspended`).
- `cmd/proxy-standin` — the D33 stand-in model proxy (single Go binary,
  distroless, env config, forwards to `MODEL_ENDPOINT`); the pattern for the
  metering binary (P2b).
- `runner/runner.go` — the reference runner. It makes **non-streaming**
  `chat/completions` requests (no `stream`, no `stream_options`) and sets no
  `Accept-Encoding` of its own — the proxy's steering-proof measures (P2b)
  exist for a **future streaming or gzip-negotiating agent**, not the
  reference runner (item 3's L161 correction).
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

- **Meter in the model proxy.** The proxy (the `cmd/model-proxy` binary from
  P2b) reads each response's `usage` field (`prompt_tokens`,
  `completion_tokens`; P2b) and accumulates **per-Loop cumulative** counters
  for prompt tokens, completion tokens, and request count.
- **Report-only, exactly as PLAN.md requires.** The proxy does **not**
  decide anything and never refuses a request because of a budget. The
  report channel is the one thing the agent must not be able to steer:
  - **The operator's read channel is an operator-only HTTP endpoint on the
    proxy** (item 1: the operator has **no `pods/log` RBAC** — its pod
    verbs are `get;list;watch;create;update;delete` — and envtest has no
    kubelet, so a pod-log read was never implementable as the round-1 plan
    claimed). Precisely:
    - the proxy serves `GET /coxswain/usage` on a **second port** (`9090`,
      env `PROXY_PORT_USAGE`), distinct from the agent-facing `8080`;
    - a **proxy-side ingress NetworkPolicy** (owned by the operator,
      selector = the proxy's label set, added in P2b) admits **9090 only
      from the operator pod**. The peer is a **compound selector**: a
      `namespaceSelector: {kubernetes.io/metadata.name: <operator ns>}`
      **and** the controller-manager podSelector (`control-plane: controller-
      manager`, the kubebuilder `config/manager/` manifest label — P2b
      verifies it against the deployed manifest). The netpol lives in the
      **Loop** namespace, and a bare podSelector only matches pods in that
      namespace — the namespaceSelector is required for a cross-namespace
      match. It denies everything else on that port; the agent's existing
      D34 netpol allows the agent → proxy only on **8080**, so the agent
      cannot reach 9090;
    - the operator dials `http://<proxy-pod-ip>:9090/coxswain/usage` with a
      plain in-cluster HTTP client (the pod IP from the pod status the
      operator already reads; **no exec, no log, no new RBAC** — the
      operator already has pod `get`, and the dial is a plain client
      connection, not a Kubernetes API call). **The listener binds
      `0.0.0.0:9090`, not `127.0.0.1:9090`** (P1-A: the operator is a
      **different pod**, so a loopback bind would refuse every pod-IP
      connection — the round-1 text's "localhost-only listener" was
      unreachable by design; the 9090 port is protected **only** by the
      ingress netpol peer, which admits just the operator pod).
      **This is the channel the operator reads; there is no pod-log read in
      the design.**
  - **The endpoint returns cumulative counters, not deltas:**
    `{bootID, promptTokens, completionTokens, requests,
    unmeteredRequests, model, sinceStart}`. The counters are **persisted to
    an emptyDir volume** (a single JSON file the proxy rewrites after each
    metered request, atomic via a temp-file+rename) **along with the
    `bootID`** (P1-B: the `bootID` is generated **only when the file is
    missing** — a fresh boot. A **container restart within the same pod**
    (a crash, an OOM) re-reads the file and **keeps both the counters and
    the same `bootID`**, so the operator's delta logic sees a continuous
    series. A **pod recreate** (the emptyDir is wiped) gets a **new**
    `bootID` with counters from 0 — the operator treats that as a fresh
    start (a delta from 0, P2d's rule). The round-1 flaw was that the
    `bootID` was regenerated on *every process start* while the counters
    persisted — a container restart would then look like a new boot and the
    operator's delta-from-0 would re-add the whole pre-restart total
    (double-count); persisting the `bootID` with the counters closes it.)
  - **The operator's read seam is a func field** on the reconciler (the
    `readBaseCommit` pattern, `loop_controller.go:206`):
    `readProxyUsage func(ctx, loop) (UsageReading, error)` with a default
    that performs the HTTP GET through the non-cached `apiReader` client's
    HTTP path (a direct `http.Client` to
    `http://<proxy-pod-ip>:9090/coxswain/usage` — the pod IP is resolved
    from the pod status the operator already reads; no exec, no log, no new
    RBAC: the operator already has pod `get`, and the HTTP read is a
    plain client dial, not a Kubernetes API call). **Every envtest spec
    injects a fake via this seam** (envtest has neither a proxy pod that
    serves HTTP nor a kubelet).
  - **The audit line stays** (a JSON line on stdout,
    `{time, loop, namespace, source: "model-proxy", action: "usage", model,
    promptTokens, completionTokens, status, usagePresent}`) for
    operator-side observability via `kubectl logs` — it is **not** the
    gate's input.
- **Agent-steering-proof (item 3).** The agent controls the request it
  sends, so the proxy must not let request shaping steer the meter:
  - the proxy **strips `Accept-Encoding`** from the agent's request before
    dialing upstream (a gzip body the proxy cannot parse would otherwise be
    unmetered by the agent's choice);
  - on **streaming** requests the proxy **forces
    `stream_options.include_usage=true`** into the request body before
    dialing (an agent that sets `stream: true` without it would otherwise
    get a stream with no `usage` — again by its own choice);
  - the **reference runner needs neither** (it is non-streaming and sets no
    `Accept-Encoding` — verified in `runner/runner.go`); these measures
    exist for any agent that does.
  - Responses the proxy **cannot** meter (no `usage` object in the body —
    a non-JSON body, an HTTP error with no `usage`, a stream that ends
    without a `usage` chunk despite the forced `include_usage`) are counted
    in a **separate cumulative `unmeteredRequests` count** (item 3: a
    count, not a last-request flag) and reported in the endpoint reading;
    the operator's budget decision is on the **metered** tokens (an
    under-count is visible as `unmeteredRequests` > 0, never an
    over-count).
- **Restart-safe metering (item 2).** A proxy **restart** (container crash,
  OOM-kill, the drift-recreate) must not let the budget be evaded, and must
  not **double-count** (P1-B). The operator's rules, all in P2d:
  - the operator stores `{lastBootID, lastPrompt, lastCompletion,
    lastRequests, lastUnmetered}` on the Loop's **status** (`status.budget`
    carries them) and **adds deltas**: `tokens += reading.promptTokens -
    lastPrompt` (floored at 0). The `bootID` is **stable across container
    restarts** (persisted, above), so a container restart produces a normal
    **same-boot** delta — no rebase, no double-count.
  - **The first read** (empty `lastBootID`): the operator **adopts the
    reading as the baseline** — sets `last*` to the reading's values, adds
    **nothing** to the accumulation (the pre-reading count is unknown, not
    zero, and must not be guessed), and records **no** `MeteringReset`
    warning (an adoption is not an anomaly — P2d spec 13).
  - **A pod recreate** (a **different `bootID`**, counters from 0): the
    operator resets its `last*` to the reading's values, records
    `status.budget.bootIDChanged=true` (sticky, operator-visible), and
    emits a `Warning` Event `Reason: MeteringReset` (a fresh boot, a delta
    from 0). The honest limit: a pod **recreate** loses the cumulative
    count (the emptyDir is per-pod); the loss is visible, not silent.
  - **A counter that drops without a bootID change** (same `bootID`, a
    cumulative value *lower* than `last*` — a corrupted/partial file or a
    torn read): the operator emits a `Warning` Event `Reason:
    MeteringAnomaly`, **rebases** `last*` to the reading's values, and
    **adds nothing** (no negative tokens; the drop is not re-added on the
    next read). Stated once here and in P2d spec 11 (the round-1 text had
    P2a and P2d spec 11 describing this differently — the anomaly is the
    same in both: warn + rebase + add nothing, **not** a `bootIDChanged`
    rebase, which is reserved for a genuine new boot).
  - **A drift-recreate** of the proxy pod (a spec-hash mismatch) is a pod
    recreate: the operator does a **final read** of the endpoint immediately
    before deleting the pod (the P2b drift path), so the last-known
    cumulative count is on `status.budget` and the recreate's `bootID`
    change is the visible, bounded loss.
- **Prometheus `vllm:*_tokens_total` metrics (owner's note) are an
  acceptance cross-check, not the per-Loop source of truth.** They are
  **cluster-wide** (per backend, not per Loop) — on a shared vLLM backend
  several Loops' tokens land in the same counter. In P2h the cross-check
  uses the **real vLLM backends** (the pi6 + pi8 `vllm:prompt_tokens_total`
  / `vllm:generation_tokens_total` deltas, read **read-only** via the
  homelab Prometheus at `--context default` — item 14: a stub-emitted
  counter would make the check circular), over the Loop's wall-clock
  window: the per-Loop proxy count is **consistent with** the backend
  delta (≤ it, given the other Loops' concurrent tokens). Never the input
  to a budget decision.
- **What this ADR does NOT cover:** metering of tool calls (tools are not
  in the token/cost budget — `maxTokens`/`maxCostUsd` count model tokens
  only; `maxWallClock` is Loop-level), any per-request metering beyond
  `usage`, and any decision by the proxy. **Durability:** the Loop's
  `status.budget` survives an operator restart (it is in etcd); what does
  **not** survive is the proxy's **in-pod counters** across a pod recreate
  (item 17 — the round-1 text's "status resets on status-loss" framing was
  wrong about the operator and incomplete about the proxy; the honest
  statement is the `bootIDChanged` mechanism above).

**ADR acceptance:** ADR written and committed; the metering binary (P2b)
and operator decision (P2d) match the ADR's "Decision" section; the P2h
kind acceptance cross-check is present.

**Gate mutation (I49 norm):** N/A (docs-only slice — the mutation checks
live in P2b/P2d, which this ADR's specs name).

**Dependencies:** none (design slice; P2b/P2d consume it).

---

## P2b — `cmd/model-proxy` binary: the metering model proxy + operator wiring

**Scope:** a new metering model proxy (`cmd/model-proxy/` — the
`cmd/proxy-standin` pattern) that forwards to `MODEL_ENDPOINT` exactly as
the stand-in does (reverse proxy, single upstream, the D33/D34 shape) and
**adds** per-Loop cumulative `usage` metering + the operator-only
`/coxswain/usage` endpoint per the P2a ADR. **This slice also owns the
operator wiring for the new image** (item 11: the round-1 plan's "P2e
switches the image" was a mislabel — P2e is stall detection): the
`--proxy-image` flag's **default** flips from the stand-in
(`coxswain-proxy:standin`) to the metering image, and the proxy pod's env
gains the metering env (`LOOP_NAME`, `LOOP_NAMESPACE`, `PROXY_PORT_USAGE`
= 9090). The stand-in image is **not removed** (the flag can still name it
for a Loop that does not need metering; the D33/D34 e2e pins keep passing
until P2h runs against the metering image).

- **Forwarding:** unchanged from the stand-in (origin-form plain HTTP
  in-cluster from the agent; the proxy dials `MODEL_ENDPOINT` as the origin
  of its own request; `https://` with verified TLS, `http://` plain; no
  `CONNECT`/absolute-form — those are `405`; no redirect following; the
  resolved-IP carve-outs are NOT this proxy's job — the D34 netpol owns
  that, unlike the tool proxy).
- **Credential:** the API key header is injected from the mounted
  `model-creds` Secret (as the stand-in does: read every file under
  `/model-creds` at startup). The key is never logged.
- **Steering-proof request shaping (item 3, P2a):** the proxy's upstream
  `http.Transport` sets **`DisableCompression: true`** (P3: Go's
  `http.Transport` otherwise adds `Accept-Encoding: gzip` **itself**, so
  "the upstream request has no `Accept-Encoding`" is untestable as long as
  the transport negotiates gzip on its own). With compression disabled the
  proxy strips **any** `Accept-Encoding` the agent sent (request-side)
  and the body is identity-encoded end to end; force
  `stream_options.include_usage=true` on streaming request bodies (a
  non-JSON or non-object body is forwarded unmodified and counted as
  `unmetered`).
- **Metering:** after each upstream response, parse the response body's
  `usage` object (`prompt_tokens`, `completion_tokens` — the
  OpenAI-compatible shape; a non-JSON / non-object body, or a stream with
  no `usage` chunk, counts as `unmetered`). **Streaming:** the standard
  OpenAI stream ends with a `data: {...}` chunk carrying the final `usage`
  (the forced `include_usage` makes this the normal case; a stream that
  ends without it is `unmetered`). Counters are **per-pod cumulative,
  persisted to an emptyDir** (`/coxswain/usage.json`, atomic
  temp+rename) and re-read at boot (item 2).
- **The operator endpoint:** `GET /coxswain/usage` on **`0.0.0.0:9090`**
  (P1-A: the round-1 text's `127.0.0.1:9090` bind was unreachable by the
  operator — a different pod — so the listener must bind the pod's all
  interfaces; access is controlled **only** by the ingress netpol peer,
  below). The proxy's ingress netpol (added in this slice, P2b's pod
  changes include it, owned by the Loop) admits 9090 only from the
  operator pod via a **compound peer**: `namespaceSelector:
  {kubernetes.io/metadata.name: <operator ns>}` **and**
  `podSelector: {control-plane: controller-manager}` (the bare-podSelector-
  in-another-namespace hole — a bare podSelector only matches pods in the
  netpol's own namespace, the Loop namespace; the namespaceSelector makes
  the cross-namespace match). Response:
  `{bootID, promptTokens, completionTokens, requests, unmeteredRequests,
  model, sinceStart}`. **No other endpoint** (item 15: the round-1
  localhost `/metrics` debug endpoint is **dropped** — `/coxswain/usage`
  IS the channel; there is no separate debug surface).
- **Audit (JSON on stdout, one line per metered request):** as in P2a
  (observability only, not the gate's input).
- **Config via env** (the stand-in's pattern + the new keys):
  `MODEL_ENDPOINT`, `MODEL_NAME`, `LOOP_NAME`, `LOOP_NAMESPACE`,
  `PROXY_PORT` (8080), `PROXY_PORT_USAGE` (9090), `COXSWAIN_USAGE_FILE`
  (the emptyDir path; default `/coxswain/usage.json`).
- **Pod changes (this slice's kind evidence, item 13):** the proxy pod
  gains the `usage` emptyDir volume + mount (writable — the only writable
  mount on the proxy; the container's rootfs stays read-only), the
  9090 listener (bound to `0.0.0.0`), the metering env, and the **proxy
  ingress netpol** (`<loop>-proxy-ingress-netpol`: ingress on 9090 only
  from the operator pod via the **compound** `namespaceSelector` +
  `podSelector` peer, above; the existing 8080 agent-ingress rule is
  unchanged). The `usage.json` file persists **both the counters and the
  `bootID`** (P1-B: generated only when the file is missing, so a
  container restart keeps the same `bootID`). Hardening otherwise
  identical (UID 65533, no SA token, no caps, seccomp RuntimeDefault).
- **Operator wiring (this slice, item 11):** `buildProxyPod` gains the new
  env + volume + port; `proxyImage()`'s default flips to the metering
  image; the `LoopReconciler` gains the `readProxyUsage` func-field seam
  (P2a) with the HTTP default (untested here — P2d's envtests exercise it
  through the injection; the default's HTTP path is unit-tested against an
  `httptest` server in this slice).

**Unit tests (written first, `cmd/model-proxy/` + `internal/proxy/` for the
shared logic):**
- **Metering (table-driven, `httptest` upstream):** a non-stream response
  with `usage: {prompt_tokens: 120, completion_tokens: 40}` → the
  cumulative counters advance by exactly those amounts; the endpoint
  reading carries them. A response **without** `usage` →
  `unmeteredRequests` increments, token counts unchanged. A `4xx`/`5xx`
  response **with** `usage` → still metered (a failed completion is a
  consumption). A non-JSON body → `unmetered`.
- **Steering-proof (item 3, P3):** the proxy's transport has
  `DisableCompression: true`, so the test asserts the upstream request
  (captured by the test server) has **no** `Accept-Encoding` header at all
  (the transport no longer adds `gzip` itself) **and** does not forward
  the agent's value (a request carrying `Accept-Encoding: gzip` arrives
  upstream with the header absent). A gzip-encoded response body (the test
  server sets `Content-Encoding: gzip` + a gzipped JSON body with
  `usage`) is **still metered** (with compression disabled the proxy
  gunzips before parsing; the test asserts the counter advanced). A
  `stream: true` request without `include_usage` → the upstream body
  carries `stream_options.include_usage: true` (captured); a
  `stream: true` request **with** it → unchanged. A non-JSON body →
  forwarded unmodified, `unmetered`.
- **Streaming:** a 3-chunk SSE stream whose last chunk carries `usage` →
  the accumulated counts match; a stream with no `usage` chunk →
  `unmetered`.
- **Restart persistence (item 2, P1-B):** the `usage.json` file persists
  **both the counters and the `bootID`**.
  - **Container restart (same pod):** boot the binary with a pre-seeded
    `usage.json` (`promptTokens: 500`, `bootID` B1) → the counters start
    at 500 (not 0) **and the reading's `bootID` is still B1** (the file's
    `bootID` is **reused**, not regenerated — the P1-B fix: the round-1
    binary generated a new UUID every start, so a container restart looked
    like a new boot and the operator's delta-from-0 double-counted the
    pre-restart total). A second metered request advances the counters to
    500+Δ **under the same B1** (the operator's same-boot delta is
    correct, P2d spec 13).
  - **Pod recreate (fresh emptyDir):** a fresh boot with **no** file →
    counters at 0 and a **new** `bootID` (B2) — the operator treats it as
    a fresh start (P2d spec 10).
  - **Mutation (P1-B):** regenerate the `bootID` on every start (ignore the
    file's `bootID`) → the container-restart spec **FAILS** (the reading
    carries B2, not B1; the operator rebases and double-counts).
- **Persistence atomicity:** a write to `usage.json` produces a file that
  is either the old or the new content (never torn): the test reads the
  file mid-write (a concurrent-reader goroutine) and asserts it parses as
  valid JSON every time.
- **Forwarding invariants (carry the stand-in's tests across):** origin-form
  request → forwarded with the credential header; `CONNECT` / absolute-form
  → `405`, no dial; a non-`2xx` upstream response is returned to the agent
  as-is (no retry); the audit line contains no substring of the test
  credential.
- **One pod, one Loop (reworded per item 16):** the metering state lives in
  **pod-scoped storage** (the emptyDir) and in-process memory — the
  module's design is one pod per Loop (the D33 topology), so a second pod
  (a fresh process, a fresh emptyDir) starts at zero **by topology, not by
  a code guard**. (The round-1 "no package-level mutable state shared
  across Loops" test was not testable as written — a fresh process is a
  fresh address space by construction; the claim is reworded as the
  topology statement above, pinned by the "fresh boot, no file → 0" test.)

**Envtest spec for the ingress netpol (item 11's pod-shape change, the I43
same-Loop update norm):** the operator (a) **creates** the
`<loop>-proxy-ingress-netpol` on the Loop (the reconciler's `ensureProxyPod`
path): the spec asserts the netpol exists with the **compound** peer
(`namespaceSelector` on the operator namespace **and** the
`control-plane: controller-manager` podSelector), an ingress rule on 9090
only, and a deny-all posture on that port for non-matching peers. (b)
**I43 update:** after the netpol is created, **update the operator
namespace's label or the Loop's namespace and re-reconcile from the API
server** → the operator re-creates the netpol with the **corrected** peer
selector (the same-Loop update spec proves the netpol tracks the operator
namespace, not a stale snapshot — the round-1 plan had no envtest spec for
the netpol at all).

**Acceptance:** `go build ./cmd/model-proxy` + unit suite + the netpol
envtest green under `make test`; the image is buildable (Dockerfile
mirroring the stand-in's); **a kind run (item 13)** against a dev-overlay
deploy with the new default image: the proxy pod starts (the
`model-creds` check passes — the stand-in's startup check is retained in
the metering binary), the agent's 8080 path is unchanged (the D33/D34
e2e's model-call assertion still passes), **and a dial test (P1-A): a
`kubectl`-exec dial to `http://<proxy-pod-ip>:9090/coxswain/usage` from a
pod labelled `control-plane: controller-manager` in the operator namespace
**succeeds** (returns the JSON reading), while the **same** dial from a
pod in the Loop namespace that is **not** the operator (e.g. a scratch
`netpol-probe` pod) **fails** (connection refused / no route — the
netpol's deny-all on 9090 for non-matching peers). The agent container
itself cannot reach 9090 (its D34 netpol caps it at 8080) — the PR records
the running image digest + the two dial outcomes + the agent-path
assertion.

**Gate mutations (I49 norm, scratch worktree, each must make its spec FAIL):**
- **Skip the `usage` parse** (record `unmetered` unconditionally) → the
  metering specs (the non-stream + streaming cases) FAIL (the counts stay
  0, `unmeteredRequests` climbs).
- **Drop the `Accept-Encoding` strip** → the steering-proof spec FAILS (the
  upstream sees the header; a gzipped body is not parsed).
- **Drop the `include_usage` force** → the streaming steering spec FAILS
  (the upstream body lacks the option; a stream without a `usage` chunk is
  unmetered).
- **Reset the counters to 0 on boot, ignoring the persisted file** (item 2's
  mutation) → the restart-persistence spec FAILS (the counters are 0, not
  500).
- **Drop the audit log write** → the forwarding-invariant spec that greps
  the audit line FAILS.
- **Retry a non-2xx upstream once** → the no-retry spec FAILS.

**Dependencies:** the P2a ADR (the design it implements); the D33/D34
stand-in (the code it generalises). **Before** P2d (the operator decision
reads its endpoint) and P2f (which needs the proxy's pod shape stable for
the suspension gate).

---

## P2c — API: `status.budget` + `status.stallHistory` + `spec.loop.stall*` + `spec.budget`

**Scope:** the CRD fields the decision slices (P2d/P2e) and the `Paused`
slice (P2f) read. No controller behaviour in this slice (P2d/P2e/P2f
consume it). `make manifests generate` after the type change; the CRD diff
is committed.

- **`spec.budget`** (new, optional, `BudgetConfig`):
  - `maxTokens` (`*int64`, ≥1 when set) — total **prompt + completion**
    tokens across the Loop's life.
  - `maxWallClock` (a duration string, ≥1s when set) — **accumulated
    active time** (item 10: the wall clock does **not** count time spent
    `Paused` — a paused Loop's budget clock stops; see P2d's
    `status.budget.activeSeconds` accumulation).
  - `maxCostUsd` (`*string`, a decimal string, ≥0 when set) — derived cost
    (P2d), not measured.
  - `onExceeded` (enum `Pause|Fail`, default `Pause`) — the action when
    **any** cap is hit.
  - `modelPrices` (optional, `{promptUsdPerMtok, completionUsdPerMtok}`
    decimal strings) — the **per-Loop override** of the cluster-wide
    `coxswain-model-prices` ConfigMap (item 11: this field is **P2c's**,
    not "P2d if the CRD budget allows" — the type change lands here so
    P2d's cost derivation has the field to read). Precedence:
    **Loop field > ConfigMap > (no prices → the cost cap is inert,
    fail-closed)**. The ConfigMap shape is **flat** (item 11: one shape,
    chosen — keys `prompt`, `completion`, decimal USD per million tokens,
    in the operator's namespace; **not** model-keyed: the per-Loop
    `modelPrices` override already covers the "different model, different
    price" case without a per-model ConfigMap key scheme).
- **`spec.loop`** gains (on `LoopSettings`):
  - `stallAfter` (`*int32`, ≥1, **default 3** when unset — a `nil`/zero
    reads as 3, the PLAN.md default).
  - `stallAction` (enum `Fail|Pause|Continue`, **default `Fail`**).
- **`status.budget`** (new, optional, `BudgetStatus`, operator-written):
  - `promptTokens`, `completionTokens` (`int64`, the **accumulated** totals
    across boot-ID changes, P2d), `requests` (`int64`),
    `unmeteredRequests` (`int64` — the cumulative unmetered count, item 3:
    a count, not a last-request flag), `costUsd` (`string` decimal,
    derived), `activeSeconds` (`int64` — the accumulated active time,
    item 10, P2d), `exceeded` (`bool` — sticky **until re-evaluated on
    resume**, item 5 — see P2d's resume rule), `exceededReason` (enum
    `Tokens|WallClock|Cost` — the cap that was hit; empty until exceeded),
    `lastBootID` (`string` — the proxy `bootID` the last-read counters
    came from, item 2), `lastPromptTokens`/`lastCompletionTokens`/
    `lastRequests`/`lastUnmeteredRequests` (the last-read **cumulative**
    values the operator's delta logic consumes, item 2), `bootIDChanged`
    (`bool`, sticky — a pod recreate wiped the counters; P2a's
    honest-limit marker), `lastActiveStamp` (`metav1.Time` — the last
    reconcile's active-time accumulation point, item E: set to `now` on
    **every** non-paused reconcile and **reset to `now` on resume**, so the
    first post-resume reconcile does not add the pause's duration — the
    round-1 wall clock leaked the pause into `activeSeconds` because
    `lastActiveStamp` was not reset).
- **`status.stallHistory`** (new, optional, `[]StallEntry`, atomic list):
  - one entry per **verify-failure** iteration: `iteration` (`int`),
    `jobName` (the verify Job's name, `<loop>-verify-<iteration>` — the
    **dedup key**, item 6: a re-read of the same Job appends no entry),
    `hash` (the SHA-256 hex of the normalised failing output, P2e),
    `normalisationVersion` (`string`, the P2e constant), `check` (the
    failing check's name), `at` (`metav1.Time`, the verify Job pod's
    finish time, kubelet-recorded). Capped at the last **10** entries
    (older dropped — the decision only needs the recent consecutive run).
- **`status.pausedFrom`** (new, optional, `LoopPhase`): the phase the Loop
  left on entering `Paused` (P2f); cleared on resume.
- **`status.pausedReason`** (new, optional, enum
  `Suspend|Stall|Budget` — item 4): **why** the Loop is paused. This is
  what makes resume well-defined: `spec.suspend=false` resumes **only** a
  `Suspend` pause; a `Stall` or `Budget` pause resumes via the annotation
  (P2f). (Without it, a budget- or stall-paused Loop has
  `spec.suspend=false` already, and "resume on suspend=false" would
  un-pause it on the next reconcile — the round-1 hole.)
- **`conditions`:** three new condition types (constants in
  `loop_types.go`, the `DeliveredCondition` pattern): `Stalled` (`True`
  when the stall detector has fired — `stallAction=Fail` then the Loop is
  `Failed` reason `Stalled`; `Pause` then `Paused`; `Continue` then the
  condition is `True` + the phase unchanged), `BudgetExceeded` (`True`
  when `status.budget.exceeded` is true), and `Paused` (`True` while the
  phase is `Paused`; the message names the `pausedReason`).
- **Validation (CEL):** `onExceeded`/`stallAction`/`pausedReason` enums;
  `stallAfter` ≥1; `maxTokens` ≥1; `maxCostUsd` + `modelPrices` values
  match `^\d+(\.\d+)?$`.

**Envtest-first tests (`internal/controller/loop_p2c_validation_test.go`):**
1. **Defaults.** A Loop with no `spec.budget` and no `spec.loop.stall*` →
   admitted; the reconciler's effective config (the P2d seam) reads
   `stallAfter=3`, `stallAction=Fail`, `onExceeded=Pause` (asserted via
   the exported `effectiveStallConfig`/`effectiveBudget` functions —
   P2d's consumer, unit-tested here).
2. **Bad enum rejected at admission.** `stallAction: "Mangle"` → API
   server rejects (CEL).
3. **`stallAfter: 0` rejected** (CEL minimum).
4. **`maxCostUsd: "abc"` rejected** (CEL pattern).
5. **No budget fields → no change in behaviour.** An existing Loop
   reconciles exactly as before (guards the existing specs: S1 suspend,
   the S5a iterate specs, the B3 decision specs all pass unchanged — the
   new fields are additive).

**Gate mutation (I49 norm, scratch worktree):** drop the `stallAfter`
default-of-3 branch (make `nil` read as `0`) → spec 1 FAILS (the effective
config is 0, not 3).

**Acceptance:** `make manifests generate lint test` green; the CRD YAML
diff contains only the P2c additions (no unrelated regeneration drift).

**Dependencies:** none for the types (pure CRD); P2d/P2e/P2f consume them.

---

## P2f — The `Paused` phase: `pausedFrom`, `pausedReason`, the suspension gate, resume

**Scope:** make `Paused` a real phase with the suspension mechanics and the
resume semantics. **P2f lands BEFORE P2d and P2e** (item 11's ordering fix):
P2d's `onExceeded=Pause` and P2e's `stallAction=Pause` are **entry points
into** this slice's phase — they set `phase=Paused` + `pausedFrom` +
`pausedReason` + their own condition, and **this slice owns the suspension
gate they rely on** (the round-1 plan had P2d/P2e before P2f with P2f
"generalising" their suspension — inverting it so the pause mechanics exist
first, and the decision slices simply call them).

- **Entering `Paused` (three entry points, one mechanism):**
  - `spec.suspend=true` (the existing S1 path, **upgraded**): S1 today
    suspends the sandbox but the **phase** keeps advancing. From P2f,
    `spec.suspend=true` **also** sets `phase=Paused`,
    `status.pausedFrom=<current phase>` (first time only — a re-reconcile
    with `suspend` still true does not overwrite it),
    `status.pausedReason=Suspend`, the `Paused` condition `True` (message:
    `paused (suspend)`), a `Normal` Event (`Reason: Paused`, the message
    names the source: `suspend`).
  - P2d's `onExceeded=Pause`: `pausedReason=Budget`.
  - P2e's `stallAction=Pause`: `pausedReason=Stall` (and, per P2e's
    resume semantics, the entry lands in `Implementing` — the stall pause
    happens **after** the iterate bookkeeping, so `pausedFrom=Implementing`
    and the iteration has already advanced; item 6).
  - **Pause from every phase (item 9):** the pause entry is defined for
    **every non-terminal phase** — `Pending`, `Planning`, `AwaitingApproval`,
    `Implementing`, `Verifying` (and — **item F: delivery runs in
    `Succeeded`, not `CleaningUp`** (`loop_deliver_job.go:493`; the
    controller never sets a `CleaningUp` phase, and the round-1 text's
    "CleaningUp, where the deliver Job may be in flight" referenced a
    phase that does not exist) — **`Succeeded` with a deliver Job in
    flight**: `Succeeded` is normally **not pausable** (terminal-success),
    but if `spec.delivery.mode == PullRequest` and the deliver Job has not
    yet recorded `status.delivery`, a `spec.suspend=true` is **refused** —
    the operator keeps the phase `Succeeded`, sets the `Paused` condition
    `False` with a message naming the reason (`suspend` refused while a
    deliver Job is in flight), and leaves the sandbox running so delivery
    completes; the spec asserts the refusal + the deliver Job is undisturbed.
    Once `status.delivery` is recorded (delivery done), `suspend=true` is
    again refused by the terminal-phase rule (spec 9). **The owner must pick
    one** — the plan chooses **refuse** (let delivery finish, do not suspend
    a terminal-success Loop mid-delivery; the alternative, suspending and
    leaving the deliver Job to terminate, would strand a half-pushed PR).) **A wall-clock cap hit while already
    `Paused`** (item 9): the budget decision is **inert in `Paused`** —
    the phase is already paused; the operator updates
    `status.budget` (`exceeded`, `exceededReason`) and, if the pause's
    `pausedReason` is not already `Budget`, **keeps the original reason**
    (the first pause wins; the spec asserts a `Suspend`-paused Loop that
    then hits its wall clock stays `pausedReason=Suspend` with
    `exceeded=true` recorded).
- **The suspension gate (the core of this slice):** a Loop in
  `phase=Paused` has its sandbox `OperatingMode=Suspended` **regardless of
  `spec.suspend`** (a budget- or stall-paused Loop has
  `spec.suspend=false`). `ensureSandbox`'s existing `spec.suspend` branch
  is generalised: `desired.OperatingMode = Suspended` iff
  `spec.suspend || phase==Paused` (the D30/D35a gates still apply on top —
  additive, as today). **No Jobs run while paused:** the verify Job's
  `ensure` is gated on `phase != Paused` (a paused Loop has no in-flight
  verify; a Job that was running at pause time is **not deleted** — it is
  left to terminate; a verify Job that terminates while the Loop is paused
  produces **no** iterate/stall/budget decision — the phase is `Paused`,
  not `Verifying`, so P2d/P2e's decisions are inert; the spec asserts
  this).
- **`pausedFrom` / `pausedReason` (the exact-phase + why record):** set on
  entry (the phase the Loop left + the reason), **never overwritten while
  paused** (a re-reconcile keeps both), **cleared on resume** (both set to
  empty). The claim reader is **inert while paused** (the agent container
  is stopped; the gate is explicit — a claim that *does* arrive while
  paused, e.g. from a pod mid-run at pause time, is **ignored**: the
  existing `nextPhase` is gated on `phase != Paused`).
- **Resume (returns to the exact phase):**
  - **`spec.suspend=false` resumes ONLY a `Suspend` pause** (item 4): when
    the operator sees `suspend=false` and `phase=Paused` and
    `pausedReason=Suspend` → resume. A `Stall` or `Budget` pause is
    **untouched** by `suspend=false` (the Loop stays paused — the spec
    asserts this; the round-1 hole where "suspend=false un-pauses a
    budget pause" is closed).
  - **A `Stall` or `Budget` pause resumes via the annotation**
    `coxswain.io/resume: "true"` (item 4's simplification: the annotation
    is a one-shot flag, **not** an exact-phase name — the phase to resume
    to is always `status.pausedFrom`; naming it in the annotation would
    let a mismatched value be an attack surface for no benefit). The
    operator **clears the annotation on the resume** (a leftover
    annotation on a non-paused Loop is ignored — it only applies when
    `phase==Paused`).
  - **On a valid resume:** `phase = pausedFrom` (the exact phase),
    `pausedFrom` + `pausedReason` cleared, the `Paused` condition `False`
    reason `Resumed`, a `Normal` Event (`Reason: Resumed`, the message
    names the phase + the resumed-from reason). **`lastActiveStamp` is
    reset to `now`** (item E: without this, the first post-resume reconcile
    adds `now - lastActiveStamp` **including the entire pause**, leaking
    the pause into `activeSeconds` — the round-1 wall clock counted the
    pause). The sandbox's
    `OperatingMode` returns to `Running` (the suspension gate releases:
    `phase != Paused` and `spec.suspend=false` → `Running`, subject to
    the D30/D35a gates — a proxy that is not Ready re-holds Suspended, as
    today).
  - **A resume while a cap is still exceeded is refused** (P3: the round-1
    text let the resume re-enter the phase and then re-pause, **bouncing**
    the sandbox `Running → Suspended`). The operator checks the current
    `spec.budget` caps against the current counts **before** releasing the
    sandbox: if still exceeded (a `Budget` pause with un-raised caps, or a
    `Stall`/`Suspend` pause whose wall clock has since elapsed), it
    **refuses the resume** — the `Paused` condition stays `True` with a
    message naming the still-exceeded cap, the sandbox stays `Suspended`, the
    annotation is **not** cleared (the operator did not act on it), and a
    `Warning` Event `Reason: ResumeRefused` is emitted. The owner raises the
    cap (or, for a `Suspend` pause, keeps `suspend=true`) and retries. The
    bounce is gone: the sandbox does not leave `Suspended` on a refused
    resume. (The re-evaluation that **clears** `exceeded` on a raised cap —
    item 5 — is unchanged; this rule only governs the still-exceeded case.)
  - **The phase machine resumes where it left off:** the per-phase pod
    recycle re-creates the agent container with the resumed phase as
    `desiredPhase` (the S4 mechanism, unchanged). **A
    resumed-from-`Verifying` Loop re-runs the verify** — and the stall
    gate's dedup (P2e, item 6) means the **same failed verify Job is not
    re-counted**: the re-run creates a NEW Job (`<loop>-verify-<iteration>`
    with the advanced iteration — the B4 re-run pattern; the resume does
    not re-read the old Job's evidence as a fresh failure). **A
    resumed-from-`Implementing` Loop re-runs the Implementing phase** (the
    per-phase recycle, unchanged).
  - **A budget-exceeded resume re-evaluates the current caps** (item 5 —
    the round-1 sticky-until-forever `exceeded` made `Pause` equivalent to
    `Fail` with the sandbox kept, contradicting PLAN.md's "a paused Loop
    resumes where it left off"): on a resume of a `Budget` pause, the
    operator **recomputes** `exceeded` against the **current**
    `spec.budget` caps and the **current** `status.budget` counts. If the
    caps were **raised** (or the counts are now under them — the
    `activeSeconds` accumulation stopped during the pause), `exceeded`
    **clears** and the Loop proceeds (the `BudgetExceeded` condition
    flips `False` reason `ClearedOnResume`, a `Normal` Event). If the caps
    were **not** raised (still exceeded), the Loop **re-enters `Paused`
    immediately** on the next reconcile (the decision re-fires — the
    fail-closed behaviour the round-1 plan had, but now **conditional on
    the caps still being hit**, not sticky by construction). **The owner's
    escape for an un-raisable budget is a fork** (ADR-0003), as before —
    documented in the `pausedReason` field comment.
  - **A stall pause's resume semantics** are P2e's (item 6): the stall
    pause enters from `Implementing` (post-iterate), the run of
    consecutive identical hashes is **kept** (not reset), and the resume
    re-enters `Implementing` — the next verify failure appends a
    **new** `StallEntry` (a new Job, a new hash) and the consecutive run
    continues (if the agent produced the **same** output again, the run
    extends and the stall fires **again** — the detector is not
    exhausted by one pause; if the agent changed the output, the run
    resets). Both cases are spec'd in P2e.
  - **Terminal phases are not pausable** (`Succeeded`/`Failed` above).
- **Conditions/events:** the `Paused` condition + the `Paused`/`Resumed`
  Events are P2f's (P2g's sweep asserts them).

**Envtest-first tests (`internal/controller/loop_p2f_paused_test.go`):**
1. **`spec.suspend=true` → `Paused`.** A Loop at `Implementing`,
   `suspend` flipped to `true`, re-reconcile → `phase=Paused`,
   `status.pausedFrom=Implementing`, `pausedReason=Suspend`, the
   `Paused` condition `True`, the Event (message contains `suspend`), the
   sandbox `OperatingMode=Suspended`.
2. **`pausedFrom`/`pausedReason` not overwritten.** Re-reconcile with
   `suspend` still true → both unchanged across two reconciles.
3. **The verify Job does not run while paused.** A paused Loop at
   `pausedFrom=Verifying` with a verify Job that **was** running at pause
   time (the pod exists, a check container `Terminated` non-zero) → no new
   verify Job is created, and the existing Job's termination produces
   **no** iterate/stall/budget decision (the phase stays `Paused`,
   `stallHistory` and `status.budget` unchanged).
4. **A stale claim while paused is ignored.** A paused Loop with an agent
   container carrying a termination-message claim naming the
   immediate-next phase → the operator does **not** advance the phase
   (the `nextPhase` gate on `phase != Paused`).
5. **Resume via `suspend=false` (a `Suspend` pause) → exact phase.** A
   paused Loop (spec 1) with `suspend` flipped to `false` →
   `phase=Implementing` (the `pausedFrom`), `pausedFrom` +
   `pausedReason` cleared, the `Paused` condition `False` reason
   `Resumed`, the Event, the sandbox `OperatingMode` back to `Running`
   (the D30/D35a gates satisfied in the fixture).
6. **`suspend=false` does NOT resume a `Budget` or `Stall` pause (item 4).**
   A Loop paused with `pausedReason=Budget` (and `spec.suspend=false`
   already) → a reconcile with `suspend` still `false` → **no resume**
   (the phase stays `Paused`, `pausedFrom`/`pausedReason` intact). Same
   for `pausedReason=Stall`. (The round-1 hole: this spec is the one that
   fails without `pausedReason`.)
7. **Resume via the annotation (a `Budget`/`Stall` pause).** A
   `pausedReason=Budget`, `pausedFrom=Verifying` Loop with annotation
   `coxswain.io/resume: "true"` → resume (the phase returns to
   `Verifying`, the annotation is **cleared**, the `Paused` condition
   `False` reason `Resumed`, the Event). A leftover annotation on a
   **non-paused** Loop → ignored (the phase is unchanged, the annotation
   is **not** cleared by a non-paused reconcile — it is left for the next
   pause/resume cycle; the spec asserts no side effect).
8. **A resumed budget-exceeded Loop re-evaluates (item 5).** A
   budget-paused Loop (`exceeded=true`, `exceededReason=Tokens`,
   `pausedFrom=Implementing`, `maxTokens: 200`, counts 250):
   - **(a) caps not raised:** resume via the annotation → the operator
     **refuses the resume** (P3: the caps are still hit, so it does not
     release the sandbox — the round-1 "re-enter the phase and bounce
     `Running → Suspended`" is gone). The `Paused` condition stays `True`
     with a message naming the still-exceeded cap, the sandbox stays
     `Suspended`, the annotation is **not** cleared, and a `Warning`
     `ResumeRefused` Event fires. (The spec asserts **no** `OperatingMode`
     transition on the resume.)
   - **(b) caps raised:** before the resume, update
     `spec.budget.maxTokens` to 500 (an I43 same-Loop update), then resume
     via the annotation → `exceeded` **clears** (250 < 500), the
     `BudgetExceeded` condition `False` reason `ClearedOnResume`, the
     `Paused` condition `False` reason `Resumed`, and the Loop
     **proceeds** (the phase is `Implementing` and stays there on the next
     reconcile — the spec asserts two reconciles after the resume with the
     phase unchanged, proving the re-evaluation cleared the exceedance).
9. **`Failed`/`Succeeded` are not pausable; delivery-in-flight refused
   (item F).** A `Failed` Loop with `suspend=true` → the sandbox is
   `Suspended` (the S1 branch) but `phase` stays `Failed`, `pausedFrom` +
   `pausedReason` are **not** set. A `Succeeded` Loop **with no deliver Job
   in flight** (`status.delivery` recorded or no delivery mode) with
   `suspend=true` → the same (not pausable, spec-9 terminal rule). A
   **`Succeeded` Loop with a deliver Job in flight** (mode `PullRequest`,
   `status.delivery` nil, the deliver Job running) with `suspend=true` →
   the operator **refuses** (item F): the phase stays `Succeeded`, the
   `Paused` condition stays `False` with a message naming the refusal, the
   deliver Job is **undisturbed** (not deleted, not suspended), and the
   sandbox is left running so delivery completes.
10. **In-progress: a pod mid-run at pause time (I49).** A Loop at
    `Implementing` with the agent container `Running` (not terminated) and
    `suspend` flipped to `true` → `phase=Paused`,
    `pausedFrom=Implementing`; the in-flight run's claim (when it
    terminates) is ignored (spec 4's gate; the spec asserts the phase does
    not advance on the termination).
11. **Same-Loop update (I43 norm):** after a resume (spec 5), **update
    `spec.suspend` to `true` again and re-reconcile from the API
    server** → the Loop re-enters `Paused` with `pausedFrom=Implementing`
    (the phase it was in, not a stale pre-pause value); the sandbox is
    `Suspended` again.
12. **Resume does not re-iterate (item C — consistent with the stall model).**
    A paused-from-`Verifying` Loop (spec 7) resumed → `status.iteration`
    is **unchanged** by the resume itself (the resume re-enters `Verifying`
    at the same iteration; the verify re-run — a NEW Job for the current
    pin — bumps the iteration only on its **failure**, as today). **A
    paused-from-`Implementing` Loop (the stall-pause case, spec 7's shape
    with `pausedReason=Stall`) resumed** → `status.iteration` is **the
    post-iterate value** (the iterate bookkeeping advanced it *before* the
    pause, item 6) and the next verify is a **new** Job
    (`<loop>-verify-<iteration+1>`) — item C's model: a stall resume
    records `pausedFrom=Implementing` *after* the iterate, so the next
    iteration is a new Job, and the fire rule (`k >= stallAfter`, per new
    Job, P2e) applies to that new Job. (The round-1 spec said "a
    resumed-from-Verifying Loop re-runs the verify — the re-run creates a
    NEW Job with the advanced iteration" while also saying the iteration
    is unchanged — the contradiction is resolved: the iterate advances the
    iteration *before* the pause for a stall pause, so the post-resume Job
    is new and the iteration is the post-iterate value; for a *budget/suspend*
    pause from `Verifying` the iteration is unchanged because no iterate
    ran.)
13. **Pause from `Planning`/`Pending`/`AwaitingApproval` (item 9).** A
    Loop at each of those phases with `suspend=true` → `phase=Paused`,
    `pausedFrom=<that phase>`, the sandbox `Suspended`; resume returns to
    the same phase. (The table-driven form: one sub-spec per phase.)
14. **A wall-clock cap hit while already `Paused` (item 9).** A
    `Suspend`-paused Loop whose `maxWallClock` elapses → the phase stays
    `Paused`, `pausedReason` stays `Suspend` (the first pause wins),
    `status.budget.exceeded=true` + `exceededReason=WallClock` are
    recorded; resume (suspend=false) proceeds with the wall clock
    re-evaluated per spec 8's rule (the `activeSeconds` accumulation
    stopped during the pause, so the cap may no longer be hit — the spec
    asserts the re-evaluation, in both the hit and not-hit sub-cases).
15. **A long pause does not leak into `activeSeconds` (item E).** A Loop
    at `Implementing`, `maxWallClock: 1h`, `activeSeconds: 30m`, with
    `suspend` flipped `true` → `Paused`; the test advances the clock by
    **2h** (a long pause) and re-reconciles → `activeSeconds` is **still
    30m** (the pause is not counted — the `lastActiveStamp` was frozen at
    the pause). Then `suspend=false` → resume; the test advances the clock
    by **10s** and re-reconciles → `activeSeconds` is **30m + 10s** (the
    reset `lastActiveStamp` means the resume adds only the post-resume
    10s, not the 2h pause + 10s). Without the `lastActiveStamp` reset
    (the round-1 bug) the second assertion would be 30m + 2h10s → the cap
    would fire spuriously. (The spec asserts `activeSeconds` ≈ 30m10s, not
    3h10m.)

**Gate mutations (I49 norm, scratch worktree, each must make its spec FAIL):**
- **Drop the `pausedReason` check from the `suspend=false` resume** (resume
  any pause on `suspend=false`) → spec 6 FAILS (a `Budget` pause resumes).
- **Drop the `phase != Paused` gate from the claim reader** → spec 4 FAILS
  (the phase advances).
- **Overwrite `pausedFrom`/`pausedReason` on every reconcile** (drop the
  "first time only" guard) → spec 2 FAILS.
- **Treat the resume annotation as an exact-phase name and require it to
  match `pausedFrom`** (re-add the round-1's dropped requirement) → spec 7
  FAILS (the one-shot `"true"` is not accepted).
- **Make `exceeded` sticky-through-resume** (drop P2f's re-evaluation —
  the round-1 semantics) → spec 8(b) FAILS (raising the cap does not
  clear the exceedance).
- **Resume a still-exceeded pause anyway** (drop the P3 refuse-while-
  exceeded gate — the round-1 bounce) → spec 8(a) FAILS (the sandbox
  transitions `Suspended → Running` on the resume and re-pauses, the
  `ResumeRefused` Event is absent).
- **Allow pause from `Succeeded`/`Failed`** (drop the terminal-phase
  guard) → spec 9 FAILS (`pausedFrom` is set on a terminal phase).
- **Suspend a `Succeeded` Loop with a deliver Job in flight** (drop the
  item-F refusal, let the sandbox suspend) → spec 9's delivery-in-flight
  half FAILS (the deliver Job is suspended/deleted, delivery is stranded).
- **Resume to a phase other than `pausedFrom`** (drop the exact-phase
  resume — resume to a hard-coded `Implementing`) → spec 5 FAILS (a Loop
  paused from `Verifying` resumes to `Implementing`, not `Verifying`).
- **Reset `lastActiveStamp` to `zero` on resume** (drop the item-E reset —
  leave it at the pre-pause value) → spec 15 FAILS (the first post-resume
  reconcile adds the whole pause, `activeSeconds` ≈ 3h10m, the cap
  fires spuriously).
- **Do not reset `lastActiveStamp` at all on resume** (the round-1 bug) →
  spec 15 FAILS (same as above).

**Acceptance:** envtests green under `make test`; the `Paused` phase is
reachable from all three entry points at every non-terminal phase, the
resume returns to the exact phase in every spec, and `suspend=false`
resumes **only** `Suspend` pauses. The S1 specs
(`loop_suspend_test.go`) are updated to assert the **phase** is `Paused`
(not just the sandbox's `OperatingMode`) — the round-1 S1-only specs are
superseded. **Kind evidence (item 13):** the suspension gate changes the
sandbox's `OperatingMode`, so this slice's PR records a kind run: a
kind-deployed Loop paused via `suspend=true` shows its sandbox pod
**terminated** (the agent-sandbox controller tears down the pod on
`OperatingMode=Suspended`) and re-created `Running` on resume; the digest
+ the `kubectl get pod` output before/during/after are in the PR.

**Dependencies:** P2c (the `pausedFrom`/`pausedReason` fields). **Before**
P2d and P2e (their `Pause` actions enter this slice's phase). The S4 claim
reader (the `phase != Paused` gate).

---

## P2d — Budget decision: the operator reads the proxy endpoint and applies `onExceeded`

**Scope:** the operator's budget gate. On each reconcile, the operator
**reads the proxy's `/coxswain/usage` endpoint** (the P2a channel, through
the P2b `readProxyUsage` seam) and updates `status.budget`; when a cap is
hit it applies `spec.budget.onExceeded` — entering the **P2f `Paused`
phase** (P2f landed first) or `Failed`.

- **The read + delta (item 2, P1-B):** the reading is
  `{bootID, promptTokens, completionTokens, requests, unmeteredRequests}`.
  The operator stores `lastBootID` + the last-read cumulative values on
  `status.budget` (P2c) and applies **four rules** (stated once here, in
  P2a, and pinned by the four specs):
  - **Same `bootID` (the normal + container-restart case):**
    `accumulated += reading.cumulative - lastCumulative` (floored at 0).
    A **container restart keeps the same `bootID`** (the file persists it,
    P2b), so a container restart produces an ordinary same-boot delta — **no
    rebase, no double-count** (the P1-B fix: the round-1 binary regenerated
    the `bootID` every start, so a container restart looked like a new boot
    and the delta-from-0 re-added the pre-restart total).
  - **The first read** (empty `lastBootID`): **adopt the reading as the
    baseline** — set `last*` to the reading's values, add **nothing**, and
    record **no** `MeteringReset` warning (an adoption is not an anomaly;
    the pre-reading count is unknown, not zero).
  - **A different `bootID` (a pod recreate, the emptyDir wiped):** the
    operator resets its `last*` to the reading's values, records
    `bootIDChanged=true` (sticky) + a `Warning` Event `Reason:
    MeteringReset` (a fresh boot, a delta from 0). The honest limit: a pod
    **recreate** loses the cumulative count (the emptyDir is per-pod); the
    loss is visible, not silent.
  - **A counter that drops without a bootID change** (same `bootID`, a
    cumulative value *lower* than `last*` — a corrupted/partial file or a
    torn read): the operator emits a `Warning` Event `Reason:
    MeteringAnomaly`, **rebases** `last*` to the reading's values, and
    **adds nothing** (no negative tokens; the drop is not re-added on the
    next read). This is **not** a `bootIDChanged` rebase (that is reserved
    for a genuine new boot) — the round-1 text had P2a and P2d spec 11
    describing this differently; it is the same rule in both now.
  A read **failure** (the proxy pod not Ready, the dial refused, the pod
  absent) → `status.budget` is **unchanged** (no reset, no delta — the
  operator does not guess; an under-read is visible as `requests` lagging
  the iterations, and the decision is on the last **successful** read,
  never an estimate).
  **Before a drift-recreate of the proxy pod** (a spec-hash mismatch), the
  operator does a **final read** of the endpoint (the P2b drift path), so
  the last-known cumulative count is on `status.budget` and the recreate's
  `bootID` change is the visible, bounded loss (not an unrecorded one).
- **Wall clock (item 10, item E):** `activeSeconds` accumulates
  **active** time: each reconcile adds `now - lastActiveStamp` to
  `status.budget.activeSeconds` when the Loop is **not** `Paused` (the
  pause stops the clock — a paused Loop's wall clock does not advance;
  `lastActiveStamp` is a status field, P2c). **`lastActiveStamp` is set to
  `now` on every non-paused reconcile AND reset to `now` on resume** (item
  E: the round-1 wall clock counted the pause — the first reconcile after
  resume added `now - lastActiveStamp` where `lastActiveStamp` was the
  pre-pause stamp, so the entire pause duration leaked into
  `activeSeconds`; resetting it on resume means the first post-resume
  reconcile adds only the post-resume interval). The cap
  comparison is `activeSeconds >= maxWallClock` (**`>=`** — item 10's
  boundary rule, applied to **every** cap: `tokens >= maxTokens`,
  `costUsd >= maxCostUsd`, `activeSeconds >= maxWallClock` — a cap hit
  **at** the value fires, not above it). **A quiet Loop (item 10):** the
  reconcile that computes `activeSeconds` also sets
  `RequeueAfter = maxWallClock - activeSeconds` (when the cap is set and
  not yet hit) so a Loop with **no other activity** still trips the wall
  clock — a wall-clock-only budget does not wait for the next claim/verify
  event.
- **Cost:** `costUsd = promptTokens * promptPrice/1e6 +
  completionTokens * completionPrice/1e6`, the prices from
  `spec.budget.modelPrices` (P2c) **or**, absent, the cluster-wide
  **`coxswain-model-prices` ConfigMap** (flat keys `prompt`/`completion`,
  operator namespace, P2c's chosen shape — item 11). Cost is **derived**
  (P2a), not measured. **A missing/unreadable ConfigMap and no
  `modelPrices`** → the cost is **not computed** (`costUsd` empty, the
  cost cap **inert** — fail-closed: an unpriceable cost is not a $0
  cost).
- **The decision (pure function of `status.budget` + `spec.budget` +
  `activeSeconds` + now), gated on `phase != Paused`** (P2f: the budget
  decision is inert in `Paused` — a wall-clock hit while paused records
  `exceeded` but does not change the phase/reason, P2f spec 14):
  - **No cap set → no decision.** (The default Loop is unaffected.)
  - **A cap is hit** (`exceeded` transitions false→true): set
    `status.budget.exceeded=true`, `exceededReason`, the
    `BudgetExceeded=True` condition (message names the cap + the value +
    the cap), and apply `onExceeded`:
    - **`Fail`** → `phase=Failed`, `Failed` condition `True` reason
      `BudgetExceeded`, the existing `Failed`-phase cleanup, a `Normal`
      Event (`Reason: BudgetExceeded`).
    - **`Pause`** → the **P2f pause entry** with `pausedReason=Budget`
      (`phase=Paused`, `pausedFrom=<current phase>`, the `Paused`
      condition + the `Paused` Event message containing `budget`), the
      sandbox suspension via P2f's gate. **The budget decision fires
      once per exceedance:** after the pause, the decision is inert (the
      phase is `Paused`); on a resume it **re-evaluates** (P2f item 5),
      so a raised cap clears it and an un-raised cap re-fires.
  - **Stall/budget precedence (item 8):** a verify failure that **both**
    hits the token cap **and** reaches the stall threshold in the same
    reconcile → **the stall decision wins** (it is evaluated first — it
    is on the verify evidence, the more specific signal; the budget
    decision sees the phase already changed and is inert). Both default
    to 3 on bare reconcilers (`defaultMaxIterations = 3`,
    `stallAfter` default 3), so on the 3rd identical failure the stall
    fires with reason `Stalled`, **not** `BudgetExceeded`. The spec
    asserts this ordering (a Loop with both a small `maxTokens` and
    `stallAfter=3` that hits both on the 3rd failure → `Failed:Stalled`,
    `Stalled=True`, `BudgetExceeded` **also** `True` — the budget
    condition is recorded, the stall is the **decision**).

**Envtest-first tests (`internal/controller/loop_p2d_budget_test.go`):**
1. **No cap → no decision.** A Loop with a proxy reading (injected via the
   `readProxyUsage` seam) but no `spec.budget` → `status.budget` is
   populated (counts, cost, `activeSeconds`) but the phase is unchanged,
   no `BudgetExceeded` condition. (The read works independently of the
   decision.)
2. **`maxTokens` hit → `Fail`.** `maxTokens: 200`, `onExceeded: Fail`; the
   injected reading sums to 250 tokens → `phase=Failed`, `Failed`
   condition `True` reason `BudgetExceeded`,
   `status.budget.exceeded=true` `exceededReason=Tokens`, the Event.
3. **`maxTokens` hit → `Pause`.** Same, `onExceeded: Pause` →
   `phase=Paused`, `status.pausedFrom=<the phase it left>` (e.g.
   `Implementing`), `pausedReason=Budget` (the P2f contract),
   `BudgetExceeded=True`, the sandbox suspended (P2f's gate), the `Paused`
   Event message contains `budget`.
4. **`maxWallClock` hit (no proxy).** A Loop with `maxWallClock` set, no
   model, `activeSeconds` back-dated past the cap (the status field is
   operator-written; the test seeds it) → `exceeded`,
   `exceededReason=WallClock`, the `onExceeded` action. **No proxy
   needed** (guards the wall-clock path independently of the read).
5. **`RequeueAfter` for a quiet wall-clock Loop (item 10).** A Loop with
   `maxWallClock: 1h`, `activeSeconds: 30m`, no other pending work → the
   reconcile returns `RequeueAfter ≈ 30m` (the spec asserts the
   `ctrl.Result.RequeueAfter` is set to the remaining time, within a
   tolerance).
6. **`maxCostUsd` hit (derived).** A `coxswain-model-prices` ConfigMap
   present; `maxCostUsd` set below the derived cost → `exceeded`,
   `exceededReason=Cost`. A missing ConfigMap → the cost is **not
   computed** (`costUsd` empty, the cost cap inert — fail-closed).
7. **`>=` boundary (item 10).** A reading that lands the token sum
   **exactly** at `maxTokens` → `exceeded` (the cap fires **at** the
   value). One token below → not exceeded.
8. **Sticky until re-evaluated; re-evaluate on resume (item 5).** A Loop
   that hit `maxTokens` (spec 3's shape, paused): a reconcile **while
   paused** with the same counts → `exceeded` stays true, the phase stays
   `Paused` (no re-fire — the decision is inert in `Paused`). Then:
   - **(a)** a resume with the caps **unchanged** (P2f spec 8(a)) → the
     re-evaluation keeps `exceeded` → the re-pause;
   - **(b)** a resume with `maxTokens` **raised** (P2f spec 8(b)) →
     `exceeded` clears, the Loop proceeds.
   (The round-1 "sticky forever" is replaced: sticky **while paused /
   while the caps still hit**, re-evaluated on resume.)
9. **No model → token/cost caps inert.** A Loop with `maxTokens` set and
   no `endpointSecretRef` → `status.budget` stays nil (no proxy pod, no
   read); the token/cost caps never fire; the phase is unaffected by them
   (the wall clock still applies — spec 4).
10. **Boot-ID change — pod recreate (item 2, P1-B).** A Loop whose stored
    `lastBootID` is `B1` with `lastPromptTokens: 100`; the injected reading
    is `bootID B2` (a **different** boot — the pod was recreated, the
    emptyDir wiped), `promptTokens: 50` → the operator records
    `bootIDChanged=true`, the `MeteringReset` Event, and the **accumulated**
    count becomes 100 + 50 = 150 (the delta from the new boot is from 0,
    added to the prior accumulation — the pre-recreate count is not lost,
    only the in-pod counter was). A second reading at `B2` with
    `promptTokens: 80` → the delta is 80 - 50 = 30 (the `last*` was reset
    to the B2 reading), accumulated 150 + 30 = 180 (no double-count of the
    B2 baseline).
    **Container restart (same bootID, the P1-B case):** a Loop whose stored
    `lastBootID` is `B1` with `lastPromptTokens: 100`; a **container**
    restart (the pod is the same, the emptyDir persists the `bootID`) → the
    injected reading is `bootID B1` (**the same** bootID),
    `promptTokens: 130` → the operator applies the **same-boot** delta (130
    - 100 = 30, **not** a rebase-to-0), `bootIDChanged` stays **false**, no
    `MeteringReset` Event, accumulated 100 + 30 = 130. (The round-1 flaw:
    the binary regenerated the `bootID` on every start, so this reading
    would have been `B2` and the operator would have rebased to 0 + 130,
    double-counting the 100.)
11. **A cumulative decrease without a bootID change is an anomaly (item 2,
    P1-B, stated once here and in P2a).** `lastPromptTokens: 200` (boot
    B1); a reading at the **same** boot B1 with `promptTokens: 50` (a
    corrupted/partial file or a torn read) → the operator emits a
    `Warning` Event `Reason: MeteringAnomaly`, **rebases** `last*` to 50,
    and **adds nothing** (no negative tokens; the drop is not re-added on
    the next read). `bootIDChanged` is **not** set (the boot ID matched —
    this is an anomaly, not a recreate). The accumulation is **unchanged**
    by the drop. (The round-1 text had P2a and this spec describing the
    anomaly differently — the rule is now the same in both: warn + rebase +
    add nothing, **not** a `bootIDChanged` rebase.)
12. **In-progress: proxy pod not Ready → no decision (I49).** The proxy
    pod exists but is not Ready (the `readProxyUsage` seam returns an
    error) → no budget decision this reconcile (the phase is unchanged, no
    `exceeded`, a requeue — the spec asserts **no** `BudgetExceeded`
    condition and no phase change after the reconcile, and that a
    *second* reconcile with a successful read then applies the decision).
13. **The first read adopts the baseline (item 2, P1-B).** A Loop with
    `lastBootID` **empty** (the first reconcile after the proxy started);
    the injected reading is `bootID B1`, `promptTokens: 400` → the operator
    adopts the reading as the baseline (`last*` = the reading's values),
    adds **nothing** (the accumulated count is **0**, not 400 — the
    pre-reading count is unknown, not zero), and records **no**
    `MeteringReset` warning (an adoption is not an anomaly; the spec
    asserts `exceeded` is false and no `Warning` Event fired). A second
    reading at B1 with `promptTokens: 450` → the delta is 450 - 400 = 50,
    accumulated 0 + 50 = 50 (the adoption did not count the pre-reading 400).
    **In-progress: read not yet available → no decision (I49).** The seam
    returns "not ready" (the proxy pod not Ready) → no decision, no
    `exceeded` (the operator does not decide "not exceeded" from an absent
    reading; it waits for data).
14. **Same-Loop update (I43 norm):** after the Loop is `Paused` on
    `onExceeded=Pause` (spec 3's shape), **update
    `spec.budget.maxTokens` to a higher value and re-reconcile from the
    API server** → while **paused**, `exceeded` stays true (the decision
    is inert in `Paused` — the raise takes effect on the **resume**, P2f
    spec 8(b)); the phase is still `Paused`. (The round-1 spec 11's
    "the new cap is recorded in the condition's message on the next
    exceed decision only" is dropped — the message is a snapshot of the
    fire, not a live cap display.)
15. **Per-Loop price override (item 11's moved field).** A
    `spec.budget.modelPrices` (P2c) that overrides the ConfigMap → the
    derived cost uses the override (the `maxCostUsd` decision flips at
    the override-derived threshold, not the ConfigMap's).
16. **Stall/budget precedence (item 8).** A 3rd identical verify failure
    that also pushes the token sum past `maxTokens` (both hit on the same
    reconcile) → `phase=Failed` reason **`Stalled`** (the stall decision
    wins), `Stalled=True`, `BudgetExceeded=True` (recorded, not the
    decision). (The cross-slice spec lives here because the budget side
    is P2d's; P2e's spec asserts the stall side.)
    **Stall `Pause` + `maxIterations` together (P3):** when a
    `stallAction=Pause` fire and the `maxIterations` cap are hit on the
    **same** verify failure, **the stall `Pause` wins** (the Loop enters
    `Paused`, not `Failed:MaxIterationsExceeded`) — the stall is the more
    specific signal and a paused Loop is resumable, whereas
    `MaxIterationsExceeded` is terminal. The `maxIterations` cap still
    fires for the **non-stalled** cap case (three different failures). The
    spec asserts both: identical → `Paused` (stall wins); different →
    `Failed:MaxIterationsExceeded`.
17. **Wall clock stops while paused (item 10, item E).** A Loop paused (P2f)
    for a reconcile: `activeSeconds` does **not** advance during the pause
    (the spec asserts the `activeSeconds` is unchanged across a paused
    reconcile, and advances again after resume — and, per item E, the first
    post-resume reconcile adds only the post-resume interval, not the
    pause; see P2f spec 15 for the long-pause arithmetic).

**Gate mutations (I49 norm, scratch worktree, each must make its spec FAIL):**
- **Skip the `exceeded` → phase transition** (set the condition but leave
  the phase) → specs 2 and 3 FAIL (the phase is unchanged).
- **Drop the `phase != Paused` gate from the budget decision** → spec 8
  FAILS (the decision re-fires while paused, changing the phase/reason).
- **Treat a missing prices ConfigMap as $0** (cost = 0 when unreadable) →
  spec 6's "missing ConfigMap" half FAILS (the cost cap fires at $0).
- **Decide on an absent reading** (a failed read is "not exceeded") →
  specs 12/13 FAIL (the decision fires on the first reconcile).
- **Ignore the `bootID`** (drop the boot-ID check — item 2's named
  mutation) → spec 10 FAILS (a B2 reading after B1 is treated as a
  continuation of B1's cumulative: 50 - 100 floors to 0, the 50 tokens
  are lost; the `bootIDChanged`/Event are absent).
- **Drop the `RequeueAfter`** (item 10) → spec 5 FAILS (the quiet Loop
  never re-reconciles against the wall clock).
- **Use `>` instead of `>=`** (item 10) → spec 7 FAILS (the exact-boundary
  reading does not fire).
- **Evaluate the budget before the stall** (item 8) → spec 16 FAILS (the
  phase is `Failed:BudgetExceeded`, not `Failed:Stalled`).
- **No-model: compute a budget from a nil proxy** (drop the no-model guard
  — item 12's missing mutation) → spec 9 FAILS (the token cap fires with
  no proxy).
- **Price override: ignore `modelPrices`** (item 12's missing mutation) →
  spec 15 FAILS (the cost uses the ConfigMap, not the override).
- **Wall clock counts paused time** (drop the `activeSeconds` pause-stop)
  → spec 17 FAILS (`activeSeconds` advances during the pause).
- **Do not reset `lastActiveStamp` on resume** (drop the item-E reset) →
  spec 17 / P2f spec 15 FAIL (the first post-resume reconcile adds the whole
  pause, the cap fires spuriously).
- **Wall clock never fires** (skip the `activeSeconds >= maxWallClock`
  comparison — item G's missing mutation) → spec 4 FAILS (the
  `maxWallClock` Loop is never `exceeded`, `exceededReason=WallClock` is
  absent).
- **Adopt the first read as a baseline of 0 and add the reading** (drop
  the P1-B first-read adoption — count the pre-reading 400 as tokens) →
  spec 13 FAILS (the accumulated count is 400 + 50 = 450, not 50;
  `exceeded` may fire spuriously).

**Acceptance:** envtests green under `make test`; the read goes through
the `readProxyUsage` seam (the `readBaseCommit` pattern — a func field with
a non-cached-client default); **no new RBAC** (the HTTP read is a plain
client dial to the pod IP; the operator's existing pod `get` resolves the
IP — the round-1 plan's "confirm `pods/log` RBAC" is dropped: there is no
log read). **Kind evidence (item 13):** the budget slice's PR records a
kind run with the metering proxy (the P2b image): a kind-deployed Loop
with a small `maxTokens` hits the cap live — the PR shows the
`kubectl get loop` output (`phase=Failed`/`Paused`, `status.budget`
populated from the **real** endpoint read, `bootID` stable across the
reconciles) + the proxy pod's `kubectl logs` audit lines, and the running
image digest.

**Dependencies:** P2a (the metering design), P2b (the endpoint it reads +
the `readProxyUsage` seam), P2c (the fields it writes), **P2f** (the
`Pause` action's target — landed before P2d, item 11's ordering).

---

## P2e — Stall detection: normaliser + golden files + the operator's stall gate

**Scope:** the stall detector. Two halves: (1) the **normaliser** (a pure
function with golden-file unit tests), (2) the **operator's stall gate**
(envtest, the decision that reads the verify-failure evidence and applies
`stallAction`).

### The normaliser (`internal/stall/`)

- **Input:** the raw **failing verify-check output** — the failed
  check-* container's output for the **single failing check** (the
  iterate branch already names `failedCheck`; the normaliser sees that
  check's output only, so a different check failing produces a different
  hash — the stall is on *the same* check). **The source (item D — the
  round-1 text's "S5a artifact seam" does not exist: `artifact` in
  `loop_verify_job.go` is the I47 build-artifact *init container* check, and
  the check-* containers are `Command`-driven with **no**
  `TerminationMessagePath`, so they write no termination message today):**
  the operator adds a **`TerminationMessagePath`** (e.g.
  `/tmp/check-out.txt`) to each check-* container's
  `TerminationMessage` field, with the check script wrapper
  (`verifySh -c <cmd> > /tmp/check-out.txt 2>&1; exit $?`) redirecting the
  check's stdout+stderr there. Kubernetes then writes the **last 4 KB** of
  the container's output to that file on termination (the
  `terminationMessagePath` + `terminationMessagePolicy: File` shape; the
  file lives on the check-tmp emptyDir the check containers already mount
  at `/tmp`), and the operator reads it from the pod's
  `status.initContainerStatuses[].lastState.terminated.terminationMessage`
  (kubelet-recorded — the **same** evidence channel as `lastCheckResults`'
  exit codes; **no pod-log read** — the round-1 text's "APIReader log
  path" was never implementable, item 1's correction applies here too).
  The normaliser's input is the 4 KB termination-message tail (a check
  that writes more than 4 KB loses the head — acceptable: the stall is on
  the *repetition* of a bounded failure signature, not the full transcript;
  a check that needs more signal can write its failing summary to the
  **last** lines, which the termination message captures). A **read seam**
  for envtest: a func field `readCheckOutput func(ctx, loop, jobName,
  checkIdx) (string, error)` on the reconciler (the `readProxyUsage`
  pattern), defaulted to the pod-status read; envtest specs inject a fake.
  The **script execution test** (the test norms' requirement for any shell
  embedded in Go) runs the real generated wrapper against a scratch check
  command and asserts the termination message carries the expected output
  tail (and the 4 KB truncation on a >4 KB output).
- **Rules (versioned; `normalisationVersion = "v1"` — the constant
  `stall.NormalisationVersionV1`). Seven rules** (item 7: the round-1
  plan listed six and said "five" — the count is corrected and two
  run-specific rules added), applied in order, then SHA-256'd:
  1. **Strip timestamps:** any line beginning with (or containing) an
     RFC3339 timestamp, a Go `log`-format `2006/01/02 15:04:05` prefix, or
     a leading Unix/epoch number → the timestamp token is removed (the
     line is kept).
  2. **Strip hex addresses:** any `0x`-prefixed hex of ≥4 digits (pointers,
     PC addresses) → a fixed `0xADDR` placeholder.
  3. **Strip temp paths:** any path containing `/tmp/`, `/var/tmp/`, or a
     `workdir`-shaped temp (`/tmp/.*-<hex>-<n>`) → the volatile suffix is
     replaced by a fixed `TMPDIR/PATH_PLACEHOLDER`.
  4. **Strip line numbers:** Go panic/test-format `file.go:123:4` →
     `file.go:LINE:COL`; test-framework `--- FAIL: TestX (1.23s)` → the
     `(Ns)` duration is stripped.
  5. **Strip 40-hex commit SHAs (item 7 — run-specific):** any 40-hex
     string (a per-iteration commit SHA — the check output names the
     commit it ran against, which **differs every iteration** and would
     otherwise defeat the hash) → a fixed `COMMIT` placeholder. (The
     normaliser's **most important** rule for the real fixture: without
     it, two identical failures on different commits hash differently and
     the stall never fires.)
  6. **Strip the verify-Job name (item 7 — run-specific):** any
     `<name>-verify-<n>` token (the `verifyJobName` shape — the check
     output / artifact names the Job, which carries the **iteration** and
     differs every re-run) → a fixed `VERIFYJOB` placeholder.
  7. **Collapse blank runs:** 3+ consecutive blank lines → 1. (Plus trim
     trailing whitespace per line and drop trailing blank lines — a
     sub-step of rule 7, not a separate rule.)
- **Why these rules:** they target the noise that makes two *identical*
  failures hash differently — the four PLAN.md names (timestamps, hex
  addresses, temp paths, line numbers) **plus the two run-specific tokens
  the round-1 review flagged** (commit SHAs, Job names — item 7: check
  output "likely contains the per-iteration commit SHA or the Job name,
  so hashes never match"). Anything not stripped is preserved — the hash
  is on the **substance**. The rules are a **versioned constant**
  (PLAN.md's `normalisationVersion`): a v2 that strips more is a new
  version, and a `stallHistory` entry's `normalisationVersion` says which
  rules produced its hash. **The operator compares consecutive hashes for
  equality WITHIN the same `normalisationVersion`** (a v1→v2 upgrade
  resets the consecutive count — a v1 hash and a v2 hash of the same
  output can differ by construction).

**Unit tests (written first, `internal/stall/`):**
- **Golden files (table-driven):** a `testdata/` of named inputs +
  expected normalised outputs + expected hashes:
  - `panic-go.txt` (a Go panic with `0x` addresses, `file.go:123:4`, a
    timestamp) → the golden output + hash.
  - `test-fail.txt` (a `go test` failure: `--- FAIL: TestX (0.42s)`, a
    `file.go:42:` line, a temp dir `/tmp/TestX123456/`) → the golden
    output + hash.
  - `commit-sha.txt` (a check output naming a 40-hex commit SHA, e.g. a
    `git diff <sha>` header) → the SHA → `COMMIT`; **a second input with
    the same substance and a DIFFERENT 40-hex SHA → the same normalised
    output and the same hash** (the item-7 property: two iterations on
    different commits hash equal).
  - `verify-job-name.txt` (a check output naming
    `<loop>-verify-3`, e.g. in a log line) → `VERIFYJOB`; **a second
    input with the same substance and `<loop>-verify-4` → the same
    output and hash** (the item-7 property: two iterations' Job names
    hash equal).
  - `blank-run.txt` (a failure with 4 consecutive blank lines in the
    middle) → the golden output (the blank run collapsed to 1) + hash.
    **This golden FAILS without rule 7** (the round-1 plan noted the
    blank-run rule had no golden that fails without it — it now does).
  - `two-identical-after-normalise.txt` — two raw inputs differing only
    in the noise (timestamps, addresses, temp paths, line numbers) →
    **the same output and hash** (the core property).
  - `two-different-substance.txt` — two inputs whose *substance* differs
    (a different file, a different error message) → **different hashes**
    (the normaliser does not over-collapse).
  - `empty.txt` → the empty output + its (defined) hash.
  - `no-noise.txt` — input with none of the noise types → byte-identical
    output (the normaliser is a no-op on clean input; guards against
    over-eager stripping).
  - `rule-order.txt` — an input where the **order of rules 1 and 2
    genuinely matters** (item H: the round-1 input had no hex inside the
    timestamp, so swapping the rules could not change the output and the
    mutation could not fail). The input is a **single line** whose
    timestamp field itself contains a `0x` hex token:
    `2026-07-03T0xde:00:00Z crash` — the `T0xde:` is the start of the
    RFC3339 time (the `0xde` is a 2-hex-digit hour, which is invalid time
    but a valid *string* the rule-1 timestamp regex matches on the
    `T[0-9]{2}` shape... precisely, the input is crafted so **rule 1's
    timestamp regex matches a span that includes `0xde`**: the line is
    `2026-07-03T0xde:00:00Z`, and rule 1's regex
    `\d{4}-\d{2}-\d{2}T[0-9]{2}:[0-9]{2}:` matches `2026-07-03T0d` (the
    `0d` where `d` is matched by a relaxed `[0-9a-f]` hour pattern the
    test pins) — **no**, the honest statement: the input is
    `2026-07-03T0x1e:00:00Z` and the **test asserts the output of the
    rules-as-ordered** and a **mutation spec (swap rules 1 and 2)**
    produces a **different** output. The mechanism: rule 1 (timestamps,
    first) matches the whole `2026-07-03T0x1e:00:00Z` token (the test's
    rule-1 regex is deliberately permissive enough to swallow a hex hour,
    which is the point — a real timestamp parser would reject it, but the
    normaliser's rule is a *regex strip*, not a parse) and removes it
    entirely, so the `0x1e` hex is gone and rule 2 has nothing to strip.
    If rule 2 runs **first** (the swapped order), it strips the `0x1e` →
    `0xADDR` **before** rule 1 sees the line, leaving
    `2026-07-03T0xADDR:00:00Z`, which rule 1 then does **not** match
    (the `0xADDR` breaks the timestamp shape) — so the line **survives**
    with the `0xADDR` placeholder. The two orderings produce **different
    outputs** (one empty, one `2026-07-03T0xADDR:00:00Z`), so the golden
    pins the order and the swap mutation **fails** on this input. (The
    round-1 input `2026-07-03T12:00:00Z panic at 0xdeadbeef` had the hex
    *after* the timestamp on a separate word, so both orders stripped both
    and produced the same output — the mutation could not fail.)
- **Version pin:** the `NormalisationVersionV1` constant's value is
  asserted (`"v1"`); a test that constructs a v1 hash and asserts the
  `StallEntry` carries `"v1"`.

**Gate mutations (I49 norm, scratch worktree, one per rule — the PR
records all seven; the two core property specs must fail on any rule
removal that affects their inputs):**
- **Remove rule 1** (timestamps) → `two-identical-after-normalise` (the
  timestamp-differing pair) FAILS.
- **Remove rule 2** (hex addresses) → `panic-go.txt` + the same pair FAIL.
- **Remove rule 3** (temp paths) → `test-fail.txt` + the pair FAIL.
- **Remove rule 4** (line numbers) → `panic-go.txt` + the pair FAIL.
- **Remove rule 5** (commit SHAs) → `commit-sha.txt`'s same-hash pair
  FAILS (the two SHAs produce different hashes — the item-7 property).
- **Remove rule 6** (verify-Job names) → `verify-job-name.txt`'s
  same-hash pair FAILS (the item-7 property).
- **Remove rule 7** (blank runs) → `blank-run.txt` FAILS (its golden
  requires the collapse — the round-1 gap, closed).
- **Swap the order of rules 1 and 2** → `rule-order.txt` FAILS (the order
  pin).

### The operator's stall gate (envtest)

- **Evidence:** on a verify **failure** (the iterate branch's sibling in
  `loop_verify_job.go`), the operator takes the failing check's output
  (the operator-collected channel above), normalises it (P2e's
  normaliser), hashes it, and **appends a `StallEntry` to
  `status.stallHistory` — deduped by `jobName`** (item 6: the entry's
  `jobName` is the verify Job's name, `<loop>-verify-<iteration>`; a
  reconcile that re-reads the **same** Job (a requeue, a stale read after
  a phase recycle that did not advance the iteration) finds the entry
  already present by `jobName` and **appends nothing** — the round-1 hole
  where resuming to `Verifying` re-read the same failed Job and appended a
  duplicate `StallEntry`). Capped at 10 entries.
- **The decision (pure function of `status.stallHistory` + the effective
  stall config), gated on `phase != Paused`** (P2f), evaluated **before
  the budget decision** (item 8's precedence — P2d spec 16):
  - **Dedup + count:** count the **consecutive** trailing entries with the
    same `hash` **and** the same `normalisationVersion` **and**
    **distinct `jobName`s** (a Job name appears at most once in the
    history — the dedup guarantees it — so a consecutive run is a run of
    *distinct Jobs* with the same hash; the `k` count is the trailing
    run's length).
  - **The fire rule (item C — one consistent model, replacing the
    round-1 "k == stallAfter and not already decided" which contradicted
    spec 3's re-fire at k=4 and spec 11's per-run-sticky mutation):**
    **`k >= stallAfter`, evaluated once per new verify Job.** The `k`
    count is the trailing run's length (consecutive distinct-Job entries
    with the same hash + same `normalisationVersion`). When a **new**
    `StallEntry` is appended (a **new** verify Job — the dedup guarantees a
    Job name appears at most once, so a new entry is always a new Job), the
    operator evaluates the fire **once for that Job**: if `k >=
    stallAfter`, fire; if `k < stallAfter`, no fire. The key is **per new
    Job, not per run**: a run of 3 with `stallAfter=3` fires on the 3rd
    Job; if the Loop is paused and resumes and the agent produces the same
    output on a **4th** new Job, `k` is 4 (≥3) and the stall **re-fires on
    the 4th Job** (spec 3) — the detector is not exhausted by one fire, but
    it does not fire *within* a single Job (one entry per Job, so there is
    nothing to re-evaluate until the next Job). The round-1 "not already
    decided for this run" is dropped: it implied the detector fired once
    per *run* and was then exhausted, which contradicted both spec 3 (re-fire
    at k=4) and the per-run-sticky mutation (spec 11) — the model is now
    *per Job*, which makes all three consistent. Apply `stallAction`:
    - **`Fail`** → `phase=Failed`, `Failed` condition `True` reason
      `Stalled`, `Stalled=True`, the existing `Failed` cleanup, a
      `Warning` Event (`Reason: Stalled`, the message names the check +
      N). (A terminal `Failed` Loop has no further Jobs — the per-run-sticky
      property is trivial: there is no 4th Job to re-fire on.)
    - **`Pause`** → the **P2f pause entry** with `pausedReason=Stall`,
      **entered AFTER the iterate bookkeeping** (item 6): the operator
      first does the iterate's normal work (advance
      `status.iteration`, clear `currentVerify`, record the failing check
      in `progress` — the B3 iterate branch), **then** enters
      `Paused` — so `pausedFrom=Implementing` (the phase the iterate
      would have set), the iteration has **already advanced**, and the
      consecutive run is **kept** (not reset — the `Stalled=True`
      condition + the history entries are intact). The resume (P2f, via
      the annotation) re-enters `Implementing` with the run kept; the
      iterate bookkeeping advancing the iteration means the **next**
      verify is a **new** Job (`<loop>-verify-<iteration+1>`) — item C's
      model: a stall resume records `pausedFrom=Implementing` *after* the
      iterate, so the next iteration is a new Job, and the fire rule is
      `k >= stallAfter` evaluated on that new Job. If the agent produces
      the **same** output, the new Job appends a `StallEntry`, `k` extends
      to 4 (≥3) and the stall **re-fires** on the new Job (spec 3); if the
      output **changed**, the run resets to 1 and no fire (spec 4).
    - **`Continue`** → `Stalled=True` + a `Warning` Event, **the phase is
      unchanged** (the Loop iterates on; the condition records the state
      — PLAN.md's "warn and keep going").
  - **Consecutive only (PLAN.md's scope):** `[A, B, A, A, A]` with
    `stallAfter=3` → the trailing run is 3 `A`s (the `B` broke the earlier
    `A` run) → fires. `[A, B, A, B, A, B]` (oscillation) → **never
    fires** — the maxIterations cap is the only stop (the existing
    `MaxIterationsExceeded` path, unchanged).
  - **Stall vs `maxIterations` precedence (item 8):** both default to 3.
    On the 3rd identical failure, **the stall fires** (the stall decision
    is evaluated in the iterate branch **before** the
    `MaxIterationsExceeded` cap check — the stall is the more specific
    signal, and its reason (`Stalled`) is more informative). The
    `MaxIterationsExceeded` cap still fires for the **non-stalled**
    3rd-failure case (three *different* failures — the stall run never
    reaches 3 — hit the cap). The spec asserts both: identical →
    `Stalled`; different → `MaxIterationsExceeded`.
- **Effective config:** `spec.loop.stallAfter`/`stallAction` (P2c) with
  the built-in defaults (3 / `Fail`). **The cluster-wide default
  ConfigMap** (`coxswain-stall-defaults`, operator namespace; keys
  `stallAfter`, `stallAction`) is the fallback when the Loop's field is
  unset: precedence **Loop field > ConfigMap > built-in (3 / Fail)**. A
  missing/malformed ConfigMap → the built-in default (the stall detector
  is **not** fail-closed on a missing ConfigMap: an undetected stall is
  worse than a default — the opposite of the budget's fail-closed prices,
  deliberate, documented in the field comment).
- **In-progress (I49 norm):** the stall decision reads the **verify Job's
  pod/container status** (the exit codes + the operator-collected output).
  A verify Job **Waiting** (init not started) or **Running** (a check
  container not terminated), or a Job with neither `Failed` nor
  `Succeeded` → **no decision** (no `StallEntry` appended, no fire, a
  requeue). The `StallEntry` is appended **only** on a terminal verify
  failure (a check-* container `Terminated` non-zero, the existing B3
  evidence).

**Envtest-first tests (`internal/controller/loop_p2e_stall_test.go`):**
1. **Consecutive fires.** `stallAfter=3`; three verify iterations that fail
   the **same check with the same normalised output** (three distinct
   Jobs — `<loop>-verify-1/2/3` — three `StallEntry` appends with the same
   `hash`) → on the third, `phase=Failed` (default
   `stallAction=Fail`), `Failed` reason `Stalled`, `Stalled=True`, the
   Event.
2. **Dedup by Job name (item 6).** The same Job is re-read twice (a
   requeue before the iterate advances the iteration) → **one**
   `StallEntry` (the second read appends nothing — the spec asserts
   `stallHistory` length is 1 after two reads of the same Job name).
3. **`stallAction=Pause` + resume re-fires on identical output (item 6).**
   `stallAction: Pause`, `stallAfter=3` → the fire enters `Paused` with
   `pausedFrom=Implementing`, `pausedReason=Stall`, the iteration
   **advanced** (the iterate bookkeeping ran first). Resume via the
   annotation (P2f) → `Implementing`; the agent produces the **same**
   failing output again → a new Job, a new `StallEntry`, the run extends
   to 4 (≥3) → the stall **re-fires** → `Paused` again (the spec asserts
   the re-fire: the detector is not exhausted by one pause).
4. **`stallAction=Pause` + resume resets on changed output (item 6).**
   Same as spec 3, but the resumed agent produces a **different** output →
   the run **resets** to 1 → no re-fire (the Loop iterates on; the spec
   asserts the `Stalled` condition returns to `False` and the phase
   advances normally).
5. **`stallAction=Continue`.** Same as spec 1 but `stallAction: Continue`
   → `phase` **unchanged** (the iterate proceeds to `Implementing` as
   today), `Stalled=True`, the Event fired, **the next iteration is
   allowed** (the cap is the maxIterations one, unchanged).
6. **Oscillation never fires.** `stallAfter=3`; outputs `[X, Y, X, Y, X,
   Y]` over 6 iterations → **no fire** after 6 (the `Stalled` condition
   is `False`/absent); the Loop stops only via `maxIterations` (the
   spec asserts the phase reaches `Failed` with reason
   `MaxIterationsExceeded`, **not** `Stalled`, when the cap is hit).
7. **Stall vs maxIterations precedence (item 8).** Three **identical**
   failures → `Failed:Stalled` (the stall wins, spec 1's shape). Three
   **different** failures (no run of 3) at `maxIterations=3` →
   `Failed:MaxIterationsExceeded` (the cap fires for the non-stalled
   case). Both in one table-driven spec.
8. **Version change resets the count.** Two `StallEntry`s at `v1` with the
   same hash, then a `StallEntry` at `v2` (simulated by writing the
   entries directly, as the S4/B3 tests do) with the same *raw* output →
   the consecutive count is 1 (not 3) → no fire.
9. **In-progress: verify Job Running → no decision (I49).** A verify Job
   pod with a check container `Running` (not `Terminated`) → no
   `StallEntry`, no fire, a requeue (`stallHistory` + phase unchanged).
10. **In-progress: verify Job Waiting (init not started) → no decision
    (I49).** Same as spec 9 with the init containers not terminated.
11. **Per-Job sticky (no re-fire within a Job; item C's consistent model).**
    A `stallAction=Fail` fire (spec 1's shape, `Failed`) → a re-reconcile
    with the **same** evidence (the same Job, no new entry) → no re-fire
    (the condition is already `True`, the phase already `Failed`, and the
    per-Job rule means there is no *new* Job to evaluate → the spec
    asserts no second Event, no phase change). For a `stallAction=Pause`
    fire, the same holds until the resume produces a **new** Job (spec 3):
    the operator re-reconciles a paused Loop with the **same** Job → no
    re-fire (the phase is `Paused`, the decision is inert in `Paused` —
    P2f's gate). (The round-1 "per-run sticky" wording is replaced by the
    **per-Job** rule, item C: the detector fires once *per new Job*, so
    "no re-fire without a reset" is really "no re-fire without a **new
    Job**" — and a terminal `Failed` Loop simply has no new Job.)
12. **ConfigMap override.** A `coxswain-stall-defaults` ConfigMap with
    `stallAfter: 2`; a Loop with **no** `spec.loop.stallAfter` → fires at
    2 consecutive (the ConfigMap's value, not the built-in 3). A Loop
    with `spec.loop.stallAfter: 5` → the ConfigMap is ignored (the Loop
    field wins).
13. **Same-Loop update (I43 norm):** after a `Continue` fire (spec 5's
    shape), **update `spec.loop.stallAfter` to a lower value and
    re-reconcile from the API server** → the fire is already recorded
    (`Stalled=True`); the phase is unchanged by the edit (the condition
    is a record, not a decision input — the spec asserts the edit does not
    re-fire or clear the condition).
14. **Capped history.** 12 consecutive identical failures →
    `status.stallHistory` has exactly 10 entries (the cap), the fire
    happened at the 3rd (the cap does not delay the fire).
15. **Stall/budget precedence (item 8, the stall side).** The 3rd
    identical failure also pushes the tokens past `maxTokens` (P2d's
    injected reading) → `phase=Failed` reason **`Stalled`** (the stall
    decision, evaluated first, wins) — P2d's spec 16 asserts the budget
    side (`BudgetExceeded=True` recorded); this spec asserts the stall
    side (the `Stalled` condition `True` is the **decision**).

**Gate mutations (I49 norm, scratch worktree, each must make its spec FAIL):**
- **Drop the `jobName` dedup** (item 6) → spec 2 FAILS (two `StallEntry`s
  for the same Job name).
- **Compare hashes across `normalisationVersion`s** (drop the version
  equality from the consecutive count) → spec 8 FAILS (the count is 3,
  not 1).
- **Fire on non-consecutive repetition** (count total occurrences, not
  trailing consecutive) → spec 6 FAILS (the oscillator fires `Stalled`
  instead of `MaxIterationsExceeded`).
- **Append a `StallEntry` on a non-terminal verify** (drop the terminal
  gate) → spec 9 FAILS (a `StallEntry` appears while the check is
  `Running`).
- **Ignore the ConfigMap** (always the built-in default) → spec 12's
  "ConfigMap override" half FAILS (the fire is at 3, not 2).
- **Re-fire within the same Job** (drop the per-Job rule — item C: fire
  whenever `k >= stallAfter` on **any** reconcile, not only on a **new**
  Job) → spec 11 FAILS (the paused/failed Loop re-fires on every reconcile
  with the same evidence — a second Event, a phase bounce).
- **Fire only when `k == stallAfter` exactly** (drop the `>=` — a run of 4
  with `stallAfter=3` does not fire) → spec 3 FAILS (after resume, `k` is
  4, the exact-`==` rule never fires, the identical output does not
  re-fire).
- **Evaluate the budget before the stall** (item 8) → specs 15 (and P2d
  16) FAIL (the phase is `Failed:BudgetExceeded`).
- **Reset the consecutive run on a stall pause** (drop the "run kept"
  semantics — item 6) → spec 3 FAILS (after resume, the run is 1, not
  extended, so the identical output does not re-fire at the same
  threshold — the spec's re-fire assertion fails).
- **The `Pause` branch: enter `Paused` BEFORE the iterate bookkeeping**
  (drop the "after the iterate" ordering — item 6) → spec 3 FAILS
  (`pausedFrom=Verifying`, not `Implementing`; the iteration is not
  advanced).
- **The `Continue` branch: set the phase to `Paused`** (corrupt the
  Continue action — item 12's missing mutation) → spec 5 FAILS (the phase
  changes when it must not).

**Acceptance:** unit suite (the golden files, all seven rules mutation-
checked) + envtests green under `make test`; the `internal/stall/` package
has **no** import of the controller (the normaliser is pure; the
controller imports it, not vice versa — a `go vet`-style guard the PR
records). The normaliser's input is the **operator-collected** check
output (termination message + S5a artifact) — no pod-log read.

**Dependencies:** P2c (the `stallHistory` + `spec.loop.stall*` fields),
**P2f** (the `Pause` action enters P2f's phase — P2f lands before P2e,
item 11), the S5a/B3 iterate branch (the stall gate is a sibling decision
in the same evidence path, evaluated before the budget, item 8). The
normaliser is independent (a pure function; buildable in parallel with
P2c).

---

## P2g — Conditions + events for every transition (the auditability seam)

**Scope:** verify that **every** phase/budget/stall/pause transition in
Phase 2 emits both a **condition** (on `status.conditions`) and an
**Event** (via the existing `Recorder`), and add any missing ones. A
**sweep slice** (no new behaviour — it asserts the P2d/P2e/P2f
transitions are all auditable per PLAN.md's "conditions and events for
every transition").

- **The transition inventory (each needs a condition + an Event):**
  - `→ Paused` (from `suspend` — `pausedReason=Suspend`; from stall
    `Pause` — `Stall`; from budget `Pause` — `Budget`) — the `Paused`
    condition `True` (message names the reason) + the `Paused` Event
    (message contains the source: `suspend`/`stall`/`budget`).
  - `Paused → <phase>` (resume, via `suspend=false` for `Suspend` or the
    annotation for `Stall`/`Budget`) — the `Paused` condition `False`
    reason `Resumed` + the `Resumed` Event (message names the phase +
    the reason).
  - `→ Failed` reason `Stalled` (stall `Fail`) — the `Failed` condition
    reason `Stalled` + the `Stalled` condition `True` + the `Stalled`
    Event (P2e).
  - `→ Failed` reason `BudgetExceeded` (budget `Fail`) — the `Failed`
    condition reason `BudgetExceeded` + the `BudgetExceeded` condition
    `True` + the `BudgetExceeded` Event (P2d).
  - `Stalled=True` with `stallAction=Continue` (no phase change) — the
    `Stalled` condition `True` + the `Stalled` Event (the condition
    records the state even though the phase is unchanged).
  - `BudgetExceeded=True` (the condition, before the phase action) — the
    `BudgetExceeded` condition `True` + the Event (P2d).
  - `exceeded` cleared on a raised-cap resume — the `BudgetExceeded`
    condition `False` reason `ClearedOnResume` + a `Normal` Event (P2f
    item 5 / P2d spec 8(b)).
  - `bootIDChanged` (the metering reset) — a `Warning` Event
    `Reason: MeteringReset` (no condition — it is a metering anomaly, not
    a Loop state; the `status.budget.bootIDChanged` field is the record).
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
   the `Paused` condition `True` (message contains `Suspend`) + a
   `Paused` Event whose message contains `suspend`.
2. **`→ Paused` (stall source).** A Loop paused via stall `Pause` → the
   `Paused` condition `True` (message contains `Stall`) + the `Stalled`
   condition `True` + a `Paused` Event whose message contains `stall`.
3. **`→ Paused` (budget source).** A Loop paused via budget `Pause` → the
   `Paused` condition `True` (message contains `Budget`) + the
   `BudgetExceeded` condition `True` + a `Paused` Event whose message
   contains `budget`.
4. **Resume.** A resumed Loop → the `Paused` condition `False` reason
   `Resumed` + a `Resumed` Event (message names the phase + reason).
5. **`→ Failed:Stalled`.** A stall-`Fail` Loop → the `Failed` condition
   reason `Stalled` + the `Stalled` condition `True` + a `Stalled` Event.
6. **`→ Failed:BudgetExceeded`.** A budget-`Fail` Loop → the `Failed`
   condition reason `BudgetExceeded` + the `BudgetExceeded` condition
   `True` + a `BudgetExceeded` Event.
7. **`Continue` (no phase change).** A stall-`Continue` Loop → the
   `Stalled` condition `True` + a `Stalled` Event, the phase is unchanged.
8. **`ClearedOnResume`.** A raised-cap budget resume (P2f spec 8(b)) → the
   `BudgetExceeded` condition `False` reason `ClearedOnResume` + the
   Event.
9. **`MeteringReset`.** A boot-ID change (P2d spec 10) → the `Warning`
   Event `Reason: MeteringReset` (no condition; the
   `status.budget.bootIDChanged` field is `true`).
10. **Every transition has a non-empty message.** The table-driven
    structural assertion (no condition or Event in the above has an empty
    message — PLAN.md's auditability: "all auditability comes from
    `status.history[]` + Kubernetes events, never logs alone").

**Gate mutation (I49 norm, scratch worktree):** **drop one Event** (e.g.
the `Resumed` Event) from its transition site → specs 4 and 10 FAIL (the
Event is absent). (The sweep is the gate: a missing transition is caught
here, not in the implementing slice.)

**Acceptance:** envtests green; the transition inventory is complete
(every P2 transition has a condition + an Event with a non-empty message,
or a named no-condition anomaly like `MeteringReset`).

**Dependencies:** P2d, P2e, P2f (the transitions it sweeps).

---

## P2h — Kind acceptance (the PLAN.md "Done when")

**Scope:** `test/e2e/p2-e2e.sh` (+ `make p2-e2e`, pinned to
`--context kind-coxswain-dev`, the d41-e2e pattern). The PLAN.md Done-when:
**"a deliberately impossible goal stops cleanly with the right reason
(`Stalled` or `BudgetExceeded`) instead of spinning, and a paused Loop
resumes where it left off."** Plus the P2a real-backend cross-check.

**Fixture:** a kind cluster with the operator + a **stub model server**
(an in-kind pod, e.g. `python:3.12`, serving an OpenAI-compatible
`/v1/chat/completions`). **The stub's behaviour is pinned (item 14: a stub
that writes no code may never produce a new commit, so the stall never
advances):** the stub's completion response contains a **fixed
implement-instruction** the reference runner's tooling executes — a
one-liner that commits a **one-character change to a non-protected file**
(e.g. appends a newline to `README.md`) every time, so **every iteration
produces a new commit** (the verify Job re-runs, the iteration advances,
and the failing check's output is **identical every iteration** — the
check is `test -f /nonexistent`, which fails with the same message
regardless of the commit). The stub returns a fixed
`usage: {prompt_tokens: 100, completion_tokens: 100}` **per request**
(**200 tokens total per request** — item I's arithmetic fix: the round-1
text said "100 tokens per request" but the `usage` is 100 prompt + 100
completion = **200**; the cap math below uses 200) and `stream: false`
(non-streaming — the reference runner's shape; the steering-proof measures
are not exercised here, they are P2b's unit coverage). **Cross-check Loop
(item I):** the fixture runs **one additional Loop** whose `modelEndpoint`
is the **real vLLM LB** (`192.168.1.20:8000`, the homelab backend), so the
cross-check in assertion 3 reads **that Loop's** per-Loop `status.budget`
counts against the real vLLM delta — the other Loops point at the stub and
are **not** part of the cross-check (the round-1 text pointed all Loops at
the stub, so the real-vLLM delta could never see their tokens — the check
was vacuous). Loops:
- **the stall Loop:** `stallAfter: 3`, `stallAction: Fail`, `maxIterations`
  high (10) — so the stall (at 3) stops it, not the cap.
- **the budget Loop:** `maxTokens: 300` (so the cap is hit on the 2nd
  request: **400 tokens ≥ 300** — the arithmetic uses **200 tokens per
  request**, item I: request 1 → 200 (< 300, no fire), request 2 → 400
  (≥ 300, fire). The round-1 text said `maxTokens: 250` hit on the 3rd
  request at 300 tokens, which assumed 100/request — with the correct
  200/request, 250 would fire on request 2 at 400; the cap is set to 300
  so the fire is unambiguous at request 2. The assertion is **`>= cap`**,
  item 14: the runner may make ≥1 request per iteration, so the exact total
  is not pinned; `onExceeded: Fail`, `stallAction: Continue` (so the stall
  does not fire first — the budget fires first on request 2, while the stall
  run is only 1–2 on the 1st–2nd *failure*; the budget is on the **request**,
  the stall on the **failure**, so the budget can fire before the 3rd verify
  failure; the spec asserts the reason is `BudgetExceeded` and
  `status.budget.promptTokens + completionTokens >= 300`).
- **the control Loop:** `stallAfter: 10`, `maxIterations: 5`, the same
  failing check — it spins to 5 (the cap) without a stall, proving the
  stall Loop stopped **because of the detector**, not the cap.
- **the resume Loop:** `spec.suspend` driven `false → true → false` at
  `Implementing`.
- **the budget-`Pause` Loop:** `onExceeded: Pause`, `maxTokens` small.

Assertions (script + recorded output in the PR):
1. **The `Stalled` path.** The stall Loop → `kubectl get loop` shows
   `phase=Failed`, the `Failed` condition reason `Stalled`, the `Stalled`
   condition `True`; **the Loop stopped at 3 iterations, not spun**
   (`status.iteration == 3`); the Event shows the `Stalled` reason.
   **Instead of spinning:** the control Loop (parallel) reaches
   `status.iteration == 5` (the cap) — the control's higher iteration
   proves the stall Loop's stop was the detector's. (The round-1
   "control with `stallAfter` unset to a high value" is made concrete:
   `stallAfter: 10` > the 3 failures that occur.)
2. **The `BudgetExceeded` path.** The budget Loop → `phase=Failed`, the
   `Failed` condition reason `BudgetExceeded`, `status.budget.exceeded=
   true`, `exceededReason=Tokens`, the Event; **the token total is
   `>= 300`** (the `>= cap` assertion, item 14 — with 200 tokens per request,
   the total is 200 after request 1 and 400 after request 2, so `>= 300`
   is satisfied at request 2; not an exact number).
3. **The real-backend cross-check (P2a / item I).** The **cross-check
   Loop** (the one Loop pointed at the real vLLM LB, `192.168.1.20:8000`,
   item I: the other Loops point at the stub and cannot be cross-checked
   against the real vLLM delta — the round-1 text pointed *all* Loops at the
   stub, so the assertion was vacuous) has its per-Loop `status.budget`
   token counts read against the **real** `vllm:prompt_tokens_total` /
   `vllm:generation_tokens_total` deltas from the **homelab Prometheus**
   (`--context default`, read-only) over that Loop's wall-clock window:
   the **cross-check Loop's** per-Loop `status.budget` count is
   **consistent with** (≤, given the other Loops' concurrent tokens on the
   shared backend) the pi6+pi8 backend delta. A stub-emitted counter is
   **not** used (item 14: it would be circular — the stub would be both the
   meter's input and the cross-check's oracle). (If the homelab Prometheus
   is unreachable from the kind runner, the cross-check is **dropped** with a
   note — it is a consistency check, not a gate; the per-Loop counts are the
   source of truth.)
4. **The paused-Loop resume.** The resume Loop: `suspend` flipped
   `false → true` **while `phase == Implementing`** (the script **waits
   on the phase** — `kubectl wait`/a poll on `status.phase`, item 14's
   racy-"suspend at Implementing" fix — before flipping `suspend`, and
   waits for the sandbox pod to be **terminated** before flipping
   `false`) → `phase=Paused`, `status.pausedFrom=Implementing`,
   `pausedReason=Suspend`, the sandbox pod is **terminated**
   (`kubectl get pod` shows no running sandbox pod while paused); then
   resume (`suspend=false`) → `phase=Implementing`, `pausedFrom` +
   `pausedReason` cleared, the sandbox pod **re-created and Running**, and
   the Loop **continues from Implementing** (the assertion: the resumed
   Loop's `status.iteration` and `currentVerify.verifiedCommit` are
   consistent with the pre-pause state, not reset — the verify Job that
   eventually runs is a NEW Job for the pre-pause pin, per P2f's resume
   semantics).
5. **The budget-`Pause` + raised-cap resume (item 5).** The budget-`Pause`
   Loop: `onExceeded: Pause`, `maxTokens` small → `phase=Paused`,
   `pausedFrom=Verifying` (or `Implementing`, per the entry point),
   `pausedReason=Budget`, `exceeded=true`. Then the script **raises
   `spec.budget.maxTokens`** (a `kubectl patch` — an I43 live update) and
   resumes via the annotation (`kubectl annotate ... coxswain.io/resume=
   "true"`) → the Loop **proceeds** (the re-evaluation clears
   `exceeded` — `phase` leaves `Paused` and does not re-pause on the next
   reconcile; the assertion: two reconciles after the resume with
   `phase != Paused` and `exceeded=false`). A **second** budget-`Pause`
   Loop (control, caps **not** raised) resumed the same way → **re-pauses
   immediately** (the fail-closed re-fire) — both sub-cases asserted.

**Gate mutation (I49 norm, scratch worktree):** run the script against a
scratch-build image with the **stall gate disabled** (the P2e mutation:
drop the fire) → assertion 1 FAILS (the stall Loop spins to its
`maxIterations` instead of stopping at 3 with `Stalled`). And with the
**budget gate disabled** (the P2d mutation: skip the phase transition) →
assertion 2 FAILS (the budget Loop does not `Failed:BudgetExceeded`).
And with the **`pausedReason` check dropped** (the P2f mutation) →
assertion 4's resume via `suspend=false` **un-pauses a budget pause
early** (the budget-`Pause` control Loop in assertion 5, resumed by
`suspend=false` instead of the annotation, resumes — which it must not;
the spec asserts it stays `Paused`). Record the output.

**Acceptance:** `make p2-e2e` green on `kind-coxswain-dev`; the output
(the `kubectl get loop` outputs, the events, the Prometheus cross-check
numbers, the resume pod lifecycle, the raised-cap resume) committed in the
PR description — the R22 process note: evidence numbers in the PR body are
copied from this artifact, not recalled.

**Dependencies:** P2a–P2g merged; the kind cluster; the stub model server
(a new in-kind fixture, the d41-e2e's in-kind upstream pattern, with the
pinned commit-producing behaviour); the homelab Prometheus (read-only,
`--context default`) for assertion 3.

---

## Slice order and dependencies

```
P2a (ADR: meter in the model proxy, operator-only endpoint channel)
   │
   ├──→  P2b (cmd/model-proxy metering binary + the proxy image wiring + readProxyUsage seam)
   │
P2c (API: budget + stallHistory + stall* + budget spec + pausedReason)
   │
   └──→  P2f (the Paused phase: pausedFrom, pausedReason, the suspension gate, resume)  [BEFORE P2d/P2e — item 11]
              │
              ├──→  P2d (budget decision: read the endpoint, apply onExceeded, RequeueAfter, boot-ID deltas)
              ├──→  P2e (stall detection: normaliser + golden files + the operator's stall gate, Job-name dedup)
              │
              └──→  P2g (conditions + events sweep)  [after P2d + P2e]
                           │
                           └──→  P2h (kind acceptance: the Done-when + the real-backend cross-check)
```

- P2a and P2c are independent (parallel-safe; P2a is docs, P2c is API).
- P2b depends on P2a (the design it implements); **it owns the proxy image
  wiring** (the `--proxy-image` default flip + the pod env/volume/netpol
  changes + the `readProxyUsage` seam) — item 11's ownership fix. **P2b
  lands before P2d** (the decision reads its endpoint) and its pod changes
  need a stable shape for P2f's suspension gate.
- P2c depends on nothing (pure CRD). **`spec.budget.modelPrices` is
  P2c's** (item 11) — P2d reads it.
- **P2f lands before P2d and P2e** (item 11's ordering fix): its
  `Paused` phase + suspension gate + resume semantics are the target of
  P2d's `onExceeded=Pause` and P2e's `stallAction=Pause`.
- P2d depends on P2a, P2b, P2c, **P2f**.
- P2e depends on P2c, **P2f**, the S5a/B3 iterate branch. Its normaliser
  (a pure function) is independent and buildable in parallel with P2c.
- P2g depends on P2d, P2e, P2f (the transitions it sweeps).
- P2h depends on all of the above.
- **Per-slice kind evidence (item 13):** P2b (proxy pod changes), P2d
  (the live budget decision), and P2f (the sandbox suspension) each record
  their own kind run + running image digest in their PRs — not only P2h.

**Exit criteria** (the PLAN.md Done-when): the ADR (P2a) written +
reviewed; kind evidence for all five P2h assertions (the `Stalled` stop
with the control-Loop contrast, the `BudgetExceeded` stop at `>= cap`, the
real-backend cross-check, the paused-Loop resume, the raised-cap
budget-`Pause` resume + the un-raised re-pause); a deliberately impossible
goal stops with the right reason instead of spinning; a paused Loop
resumes where it left off (a `Suspend` pause via `suspend=false`, a
`Stall`/`Budget` pause via the annotation, and a raised-cap budget pause
proceeds on resume).

**Out of scope** (deferred, per PLAN.md + the owner's scope note):
- **Oscillation detection** (non-consecutive repetition; PLAN.md defers it
  explicitly — the maxIterations cap is the only stop for an oscillator).
- **Any metering decision by the proxy** (P2a: the proxy reports only; the
  operator decides).
- **Tool-call metering** (P2a: the token/cost budget counts model tokens
  only).
- **UI** (no dashboard for `status.budget`/`stallHistory`; `kubectl get
  loop` + the conditions are the interface).
- **Phase 3 durability** (a `stallHistory`/`budget` that survives a **pod
  recreate** of the proxy is the `bootIDChanged` honest-limit's job — the
  operator's `status` is in etcd and survives an operator restart; the
  in-pod counters do not survive a pod recreate, and that is recorded, not
  hidden — item 17).
- **Per-request metering beyond `usage`** (no per-token streaming
  attribution; the final `usage` is the count).
