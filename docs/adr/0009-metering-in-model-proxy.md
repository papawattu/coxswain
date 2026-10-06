# ADR-0009: Metering in the model proxy — report-only, operator-read

**Status:** Accepted (P2b — the metering model proxy is implemented: the
`cmd/model-proxy` binary + the operator wiring (the proxy pod's usage env,
emptyDir, metering image, the `<loop>-proxy` usage-port netpol ingress, the
`readProxyUsage` seam). The operator's usage CONSUMPTION (the P3 gate
consumption) is the remaining follow-on, not the metering itself.)
**Date:** 2026-10-04
**Supersedes:** —
**Extends:** ADR-0006 (the zero-credential rule), ADR-0007 (two-layer
allowlisted egress, streamed audit)
**Origin:** `docs/TDD-PLAN-PHASE2.md` P2a (metering design point, plan merged
as the Phase 2 plan).

## Context

PLAN.md describes a "metering sidecar (thin HTTP proxy in front of the model
API, **in the sandbox**)". Since D33 the model proxy is a **separate
operator-owned pod** (`<loop>-proxy`, UID 65533) **outside** the agent's
control: the agent container has no SA token, a read-only rootfs, a process +
DNS fence (KubeArmor, D33/C6b), and egress to the model **only through the
proxy** (D34 netpol).

A metering component *inside the sandbox* sits in the agent's own trust
boundary: a prompt-injected agent could edit it, bypass it (point the runner
at another port), or corrupt its counters — so its counts are
**agent-tamperable** and cannot be the basis for a `Fail` decision (a budget
cap applied on tamperable evidence would let the agent *disable its own
guardrail*).

The model proxy already sees **every** model request the Loop makes (the
agent's only path to the model, ADR-0006), it is operator-owned, and it
already holds the credential. Metering there is the **only**
tamper-resistant per-Loop option without new RBAC, a new CRD, or a sidecar
the operator must trust to be unbroken.

## Decision

**Meter in the model proxy, report-only.** The proxy (the `cmd/model-proxy`
binary from P2b) reads each response's `usage` field (`prompt_tokens`,
`completion_tokens`; P2b) and accumulates **per-Loop cumulative** counters
for prompt tokens, completion tokens, and request count. **Tokens are
reported by the model** in the response `usage` object — never estimated by
the proxy. **Cost** is not computed by the proxy either; it is **derived
from the `coxswain-model-prices` ConfigMap** (overridable per Loop via
`spec.budget.modelPrices`) — the operator turns metered tokens into cost at
decision time.

The proxy does **not** decide anything and **never refuses a request because
of a budget**. It is a meter and a report channel, exactly as PLAN.md
requires. The report channel is the one thing the agent must not be able to
steer:

### The operator's read channel: an operator-only HTTP endpoint on the proxy

The operator has **no `pods/log` RBAC** — its pod verbs are
`get;list;watch;create;update;delete` — and envtest has no kubelet, so a
pod-log read was never implementable. The channel is an HTTP endpoint served
by the proxy:

- the proxy serves `GET /coxswain/usage` on a **second port** (`9090`, env
  `PROXY_PORT_USAGE`), distinct from the agent-facing `8080`;
- a **proxy-side ingress NetworkPolicy** (owned by the operator, selector =
  the proxy's label set, added in P2b) admits **9090 only from the operator
  pod**. The peer is a **compound selector**: a
  `namespaceSelector: {kubernetes.io/metadata.name: <operator ns>}` **and**
  the controller-manager podSelector (`control-plane: controller-manager`,
  the kubebuilder `config/manager/` manifest label — P2b verifies it against
  the deployed manifest). The netpol lives in the **Loop** namespace, and a
  bare podSelector only matches pods in that namespace — the
  namespaceSelector is required for a cross-namespace match. It denies
  everything else on that port; the agent's existing D34 netpol allows the
  agent → proxy only on **8080**, so the agent cannot reach 9090;
- the operator dials `http://<proxy-pod-ip>:9090/coxswain/usage` with a
  plain in-cluster HTTP client (the pod IP from the pod status the operator
  already reads; **no exec, no log, no new RBAC** — the operator already has
  pod `get`, and the dial is a plain client connection, not a Kubernetes API
  call). **The listener binds `0.0.0.0:9090`, not `127.0.0.1:9090`** (the
  operator is a **different pod**, so a loopback bind would refuse every
  pod-IP connection; the 9090 port is protected **only** by the ingress
  netpol peer, which admits just the operator pod). **This is the channel the
  operator reads; there is no pod-log read in the design.**
- **The endpoint returns cumulative counters, not deltas:**
  `{bootID, promptTokens, completionTokens, requests, unmeteredRequests,
  model, sinceStart}`. The counters are **persisted to an emptyDir volume**
  (a single JSON file the proxy rewrites after each metered request, atomic
  via a temp-file+rename) **along with the `bootID`**. The `bootID` is
  generated **only when the file is missing** — a fresh boot. A
  **container restart within the same pod** (a crash, an OOM) re-reads the
  file and **keeps both the counters and the same `bootID`**, so the
  operator's delta logic sees a continuous series. A **pod recreate** (the
  emptyDir is wiped) gets a **new** `bootID` with counters from 0 — the
  operator treats that as a fresh start (a delta from 0, P2d's rule).
- **The operator's read seam is a func field** on the reconciler (the
  `readBaseCommit` pattern):
  `readProxyUsage func(ctx, loop) (UsageReading, error)` with a default that
  performs the HTTP GET (a direct `http.Client` to
  `http://<proxy-pod-ip>:9090/coxswain/usage` — the pod IP is resolved from
  the pod status the operator already reads). **Every envtest spec injects a
  fake via this seam** (envtest has neither a proxy pod that serves HTTP nor
  a kubelet).
- **The audit line stays** (a JSON line on stdout, `{time, loop, namespace,
  source: "model-proxy", action: "usage", model, promptTokens,
  completionTokens, status, usagePresent}`) for operator-side observability
  via `kubectl logs` — it is **not** the gate's input.

### Agent-steering-proof

The agent controls the request it sends, so the proxy must not let request
shaping steer the meter:

- the proxy **strips `Accept-Encoding`** from the agent's request before
  dialing upstream (a gzip body the proxy cannot parse would otherwise be
  unmetered by the agent's choice);
- on **streaming** requests the proxy **forces
  `stream_options.include_usage=true`** into the request body before
  dialing (an agent that sets `stream: true` without it would otherwise get a
  stream with no `usage` — again by its own choice);
- the **reference runner needs neither** (it is non-streaming and sets no
  `Accept-Encoding`); these measures exist for any agent that does.
- Responses the proxy **cannot** meter (no `usage` object in the body — a
  non-JSON body, an HTTP error with no `usage`, a stream that ends without a
  `usage` chunk despite the forced `include_usage`) are counted in a
  **separate cumulative `unmeteredRequests` count** (a count, not a
  last-request flag) and reported in the endpoint reading; the operator's
  budget decision is on the **metered** tokens (an under-count is visible as
  `unmeteredRequests` > 0, never an over-count).

### Restart-safe metering: the operator's delta rules

A proxy **restart** (container crash, OOM-kill, the drift-recreate) must not
let the budget be evaded, and must not **double-count**. The operator's
rules, all in P2d:

- the operator stores `{lastBootID, lastPrompt, lastCompletion,
  lastRequests, lastUnmetered}` on the Loop's **status** (`status.budget`
  carries them) and **adds deltas**: `tokens += reading.promptTokens -
  lastPrompt` (floored at 0). The `bootID` is **stable across container
  restarts** (persisted, above), so a container restart produces a normal
  **same-boot** delta — no rebase, no double-count.
- **The first read** (empty `lastBootID`): the operator **adopts the reading
  as the baseline** — sets `last*` to the reading's values, adds **nothing**
  to the accumulation (the pre-reading count is unknown, not zero, and must
  not be guessed), and records **no** `MeteringReset` warning (an adoption is
  not an anomaly — P2d spec 13).
- **A pod recreate** (a **different `bootID`**, counters from 0): the
  operator **adds the new boot's reading in full** to the accumulation (a
  fresh boot is a **delta from 0**), then resets its `last*` to the reading's
  values, records `status.budget.bootIDChanged=true` (sticky, operator-visible),
  and emits a `Warning` Event `Reason: MeteringReset` (a fresh boot, a
  **delta from 0**). The honest limit: a pod **recreate** loses the old pod's
  usage since the last read (the emptyDir is per-pod); the loss is visible,
  not silent.
- **A counter that drops without a bootID change** (same `bootID`, a
  cumulative value *lower* than `last*` — a corrupted/partial file or a torn
  read): the operator emits a `Warning` Event `Reason: MeteringAnomaly`,
  **rebases** `last*` to the reading's values, and **adds nothing** (no
  negative tokens; the drop is not re-added on the next read). This is
  **not** a `bootIDChanged` rebase, which is reserved for a genuine new
  boot. Stated once here and in P2d spec 11.
- **A drift-recreate** of the proxy pod (a spec-hash mismatch) is a pod
  recreate: the operator does a **final read** of the endpoint immediately
  before deleting the pod (the P2b drift path), so the last-known cumulative
  count is on `status.budget` and the recreate's `bootID` change is the
  visible, bounded loss.

### Cross-check only: Prometheus `vllm:*_tokens_total`

The homelab vLLM Prometheus metrics (`vllm:prompt_tokens_total`,
`vllm:generation_tokens_total`) are an **acceptance cross-check, never a
decision input**. They are **cluster-wide** (per backend, not per Loop) — on
a shared vLLM backend several Loops' tokens land in the same counter. In P2h
the cross-check uses the **real vLLM backends** (the pi6 + pi8 counter
deltas, read **read-only** via the homelab Prometheus at `--context default`
— a stub-emitted counter would make the check circular), over the Loop's
wall-clock window: the per-Loop proxy count is **consistent with** the
backend delta (≤ it, given the other Loops' concurrent tokens).

## Consequences

- Metering lives entirely in the operator-owned proxy; the agent can read,
  edit, bypass, or crash nothing that changes what the operator measures.
  The budget `Fail` decision rests on counters the agent cannot steer.
- The proxy grows one more responsibility (persisting counters to a
  per-pod emptyDir, serving the second port, rewriting request bodies); all
  of it is report-only — no new denial path, no new agent-visible behavior.
- A proxy **pod recreate** loses the cumulative count (emptyDir is
  per-pod); the loss is made visible, bounded, and operator-auditable by the
  `bootID`/`bootIDChanged`/`MeteringReset` mechanism and the final read
  before a drift-recreate. A container restart within the pod loses
  nothing (counters and `bootID` are re-read from the file).
- Durability is asymmetric and deliberate: the Loop's `status.budget`
  survives an **operator** restart (it is in etcd); the proxy's **in-pod
  counters** do not survive a **proxy pod** recreate.
- The operator's read is one HTTP dial per Loop per reconcile, through a
  seam (`readProxyUsage`) that envtest fakes — no new RBAC, no pod-log
  dependency, no exec.

## Alternatives considered

1. **The in-sandbox metering sidecar (PLAN.md's original design).** Rejected:
   it sits inside the agent's trust boundary — a prompt-injected agent can
   edit it, bypass it, or corrupt its counters, so its counts are
   **agent-tamperable** and cannot ground a `Fail` decision.
2. **Operator log scraping** (the operator reading the proxy's stdout
   audit lines and summing them). Rejected: the operator has **no
   `pods/log` RBAC** (pod verbs are `get;list;watch;create;update;delete`),
   and envtest has no kubelet, so pod-log reads are not implementable on
   the test path at all; even where available, log collection is **lossy**
   and **restart-unsafe** (log offsets, rotation, and pod replaces make a
   cumulative sum unreliable).
3. **Prometheus as the source of truth** (`vllm:*_tokens_total`). Rejected:
   the counters are **cluster-wide per backend, not per Loop** — on a shared
   backend several Loops' tokens land in the same counter, so they cannot
   attribute spend to a Loop. They remain an acceptance cross-check only
   (P2h), never the input to a budget decision.

**Acceptance** (mirrors the P2a plan): this ADR is written and committed;
the metering binary (P2b) and the operator decision (P2d) match the
"Decision" section above; the P2h kind acceptance cross-check is present.
