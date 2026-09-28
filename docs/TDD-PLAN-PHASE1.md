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
  - **C6b — engine runtime + gate.** The operator is engine-agnostic behind an
    internal `engine.Enforcer` interface (set to a fake in envtest, to the
    KubeArmor emitter in prod): `Apply(ctx, loop, enginePolicy) error` (emit the
    policy object) and `Enforcing(ctx, loop) (bool, reason string)` (D30
    evidence). The KubeArmor emitter translates Coxswain's `EnginePolicy` into a
    `KubeArmorPolicy` (group `security.kubearmor.com/v1`): `selector.matchLabels`
    targets the sandbox pod; `syscalls.matchPaths`/`matchSyscalls`,
    `network.matchDNSQueries` (host:port → DNS name) + `matchProtocols`, and
    `file.matchPaths` carry the allows; `action: Allow` per rule, base default
    the platform minimum. The D30 gate: before `ensureSandbox` sets OperatingMode
    Running, the operator calls `Enforcing()`; on not-enforcing it holds the
    sandbox Suspended + `PolicyEnforced=False` (reason `EngineUnavailable` |
    `NodeNotEnforcing` | `PolicyRejected`) and requeues — never fails the Loop
    (Q5). Seams: (1) pure unit test for the KubeArmorPolicy emitter over an
    `EnginePolicy` (assert selector + syscall/network/file fields); (2) envtest
    with a fake `Enforcer` (not-enforcing → sandbox Suspended + condition;
    enforcing → Running). **Real-engine e2e (KubeArmor installed on kind via a
    `make kind-up` step + a disallowed `exec` actually blocked) is RUN as
    `make kubearmor-e2e` (test/e2e/kubearmor-exec-block.sh) — installed
    KubeArmor v1.7.5 on kind 1.34 (pinned `karmor install --tag v1.7.5`, the
    v-prefix mandatory) and proved the operator's KubeArmorPolicy is accepted +
    loaded on the pod (karmor probe: pod 'Armored Up', Active LSM BPFLSM) and
    the allowed exec runs. Two emitter bugs found + fixed by the e2e: exec
    allows emitted as `/**/<binary>` (KubeArmor's `process.matchPaths[].path`
    requires an absolute-path pattern) and `spec.action: Block` (default-deny;
    without it KubeArmor defaults to Audit and nothing is blocked). **Honest
    result: the disallowed exec (curl) was NOT blocked in that environment**
    (a KubeArmor BPF-LSM process-enforcement gap, not diagnosed); the script
    asserts the block and fails if not enforced, so a green run is the proof
    and it does not currently go green. D30's `Enforcing()` evidence is still
    the agent's telemetry/alert stream (I32 relay), not the policy object: the
    KubeArmorPolicy CRD has no enforcement status field (`status: {}`), so D30's evidence is the agent's telemetry/alert stream
    (I32), a real-runtime dependency; and per-container scoping (D29) is NOT
    expressible in one KubeArmorPolicy (selector is pod-level), so the
    agent=localhost / proxy=model-endpoint split is enforced by the
    NetworkPolicy (C3) with the KubeArmorPolicy as the pod-level fence.
    **R15 P1s fixed (9a951f2):** (1) the D30 gate applies to EVERY Loop (no
    policyRefs = the platform minimum, still translated/emitted/enforced; the old
    no-policyRefs-ungated spec is inverted); (2) the gate has something behind it —
    Reconcile calls `Enforcer.Apply` before the gate, the sandbox pod template
    carries the `coxswain.io/loop` label the selector targets, and a real
    `KubeArmorEnforcer` (unstructured create/update of the KubeArmorPolicy,
    owner-ref'd) is wired in `cmd/main.go`; (3) exec allows are emitted under
    `process.matchPaths` + `action: Allow`, not `syscalls` (monitoring-only).
    `Enforcing` fails closed (NodeNotEnforcing) until the I32 relay is wired.
    **R15 P1s fixed (9a951f2):** (1) the D30 gate applies to EVERY Loop (no
    policyRefs = the platform minimum, still translated/emitted/enforced; the old
    no-policyRefs-ungated spec is inverted); (2) the gate has something behind it —
    Reconcile calls `Enforcer.Apply` before the gate, the sandbox pod template
    carries the `coxswain.io/loop` label the selector targets, and a real
    `KubeArmorEnforcer` (unstructured create/update of the KubeArmorPolicy,
    owner-ref'd) is wired in `cmd/main.go`; (3) exec allows are emitted under
    `process.matchPaths` + `action: Allow`, not `syscalls` (monitoring-only).
    `Enforcing` fails closed (NodeNotEnforcing) until the I32 relay is wired.
    **Remaining:** the real kind e2e (KubeArmor on kind via `make kind-up` + a
    disallowed exec actually blocked) + the D29 per-container proposal (P2, a
    review doc before any C3 code).
    **R15 P1 round 2 (5068c15):** (1) **exec-block posture** — root cause of
    "the disallowed exec ran": KubeArmor v1.7.5's BPF-LSM gates the exec
    allowlist's block-vs-audit on `defaultFilePosture` (NOT `spec.action`; the
    process whitelist's block sentinel is keyed on `defaultPosture.FileAction` in
    `enforcer/bpflsm/rulesHandling.go`), and `karmor install` defaults it to
    `audit`. `make kind-up` now sets `kubearmor-config` to
    `defaultFilePosture/NetworkPosture/CapabilitiesPosture: block` +
    `visibility: process,file,network,capabilities`, then restarts the KubeArmor
    DaemonSet so the live BPF map flips; the e2e asserts `defaultFilePosture=block`
    up front so a misconfigured env fails with a clear posture message. (2)
    **`/**/<name>` spoofing** — each `process.matchPaths` item carries ONLY
    `{path: <absolute location for the real binary>}` (go →
    `/usr/local/go/bin/go` via a `defaultExecPaths` map), so a same-named
    binary in a writable dir does not satisfy the allow. (R16 correction:
    the item must NOT also set `execname` — v1.7.5's BPF-LSM keys the process
    rule on the exec'd file's dentry name when execname is present and IGNORES
    path, so `execname`+`path` is exactly as spoofable as `/**/<name>`;
    reproduced on coxswain-dev: an `execname`+`path` item allowed a copied
    `/tmp/go`, the path-only item denied it.) (3) **base-manifest
    flag** — `--allow-unenforced` removed from `config/manager/manager.yaml` (so
    `make deploy` / `dist/install.yaml` ship fail-closed); added
    `config/manager/allow-unenforced.yaml` as a dev/kind overlay that `make deploy`
    applies. **Remaining:** the real kind e2e run (needs a kind+KubeArmor host to
    go green once the posture is block); the D29 per-container proposal is now
    superseded by the owner's R13 decision (option c: the proxy in its own pod —
    `docs/REVIEW-PHASE1-R13.md`), implemented as D33–D35.

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

### I. I42 egress proxy (ADR-0007 I42 resolution — the network enforcement layer)

The I42 slices implement the per-Loop egress proxy that enforces the effective
`AgentPolicy` network allows at the HTTP/HTTPS layer. They sit on top of D35
(proxy gate) and C6b (KubeArmor engine), and before B3 (verify evidence
requires meaningful isolation). Each slice is red→green; envtest-first, then
kind acceptance on `--context kind-coxswain-dev`.

The ADR (docs/adr/0007-*.md, the I42 resolution section) is the authoritative
design; this plan breaks it into buildable slices.

#### I42a — `cmd/egress-proxy/` binary (the stand-in enforcement core)

**Scope:** a new Go binary (`cmd/egress-proxy/main.go` + `internal/egress/`) that
implements the HTTP/HTTPS forward proxy:
- Listens on `:3128` (env `EGRESS_PROXY_PORT`, default 3128).
- Reads the effective policy allows from a mounted file (or env `EGRESS_POLICY_JSON`
for Phase 1 simplification): a JSON array of `{host, port}` pairs.
- **CONNECT handling:** parses the `CONNECT host:port` line. Checks `host:port`
  against the allows (exact host match, exact port match; host match is
  case-insensitive, port is the literal number from the allow). Disallowed →
  `403 Forbidden` + JSON audit record (blocked). Allowed → resolves the host
  **itself** (Go `net.Resolver`, using the pod's DNS), rejects if any resolved
  IP is in a carved-out range (`10/8`, `172.16/12`, `192.168/16`, `169.254/16`,
  `127/8`, `100.64/10`, `::1`, `fc00::/7`, `fe80::/10`, plus the pod CIDR and
  service CIDR passed as env `POD_CIDR` / `SERVICE_CIDR`), then opens a TCP
  dial to **that resolved IP** (same IP — no re-resolution) and relays bytes
  bidirectionally.
- **SNI check:** after the CONNECT tunnel is open (proxy returned 200), the
  proxy reads the first bytes from the client. If they look like a TLS
  ClientHello (record type 0x16), extract the SNI (TLS extension 0x0000). If
  SNI is present and ≠ the CONNECT host → close tunnel + audit (blocked).
  SNI present and = CONNECT host → proceed. SNI absent → close tunnel + audit
  (blocked). (ECH is treated as SNI-absent.)
- **Plain HTTP (non-CONNECT):** parse the request line's `Host` header. Check
  `host:port` against allows. Disallowed → `403` + audit. Allowed → proxy the
  request (forward to the resolved IP).
- **JSON audit on stdout:** every connection attempt (allowed and blocked)
  emits a single JSON line with the Q4 envelope fields: `{time, loop, namespace,
  source: "egress-proxy", action: "connect"|"http", target: "host:port",
  verdict: "allowed"|"blocked", detail: "sni=..., proto=..., ip=..., policy=..."}`.
  The `loop` and `namespace` fields come from env (`LOOP_NAME`, `LOOP_NAMESPACE`).
  The `policy` field is `EGRESS_POLICY_HASH` env. `iteration` is NOT emitted
  by the proxy (the relay fills it by time window).
- No TLS termination. No payload inspection. No MITM.
- No secrets mounted. No SA token. Read-only rootfs. UID 65534.

**envtest-first tests** (none applicable — this is a binary, tested via unit tests
on the internal package + kind e2e):
- **Unit tests** (`internal/egress/`): table-driven test of the CONNECT check
  (allowed host:port passes, disallowed host 403s, disallowed port 403s, exact
  match semantics); resolved-IP rejection (a mock resolver returning a private
  IP → blocked; a public IP → allowed; a rebind to a pod-CIDR IP → blocked);
  SNI extraction (a synthetic ClientHello with SNI = CONNECT host → proceed;
  SNI ≠ CONNECT host → closed; no SNI → closed); audit record shape (allowed
  record has the right fields; blocked record has the right `detail`).

**kind acceptance** (`--context kind-coxswain-dev`):
- A `make egress-proxy-e2e` script (or an addition to the existing e2e)
deploys a minimal egress proxy pod (the `cmd/egress-proxy` binary image) in
  a namespace, with a fixed policy allowing `example.com:443`. A test agent
  pod (curl, `HTTPS_PROXY` pointed at the proxy) sends: (a) `curl https://example.com`
  → succeeds (or at minimum gets a non-403 from the proxy); (b) `curl https://disallowed.com`
  → gets 403 from the proxy; (c) `curl --resolve` or a raw `nc` to a
  cluster-internal IP → connection refused/timeout (NetworkPolicy blocks it).
The script asserts the proxy's stdout contains the expected audit JSON lines
  (one `allowed` for example.com, one `blocked` for disallowed.com).

**Dependencies:** none (this is a leaf binary). The binary must be buildable
  into an image before I42b uses it.

#### I42b — `ensureEgressProxy` controller function (pod + Service + gate)

**Scope:** the operator creates and manages the egress proxy pod + Service,
  gated on the effective policy having network allows.
- **Gate:** the egress proxy pod is created only when the Loop's effective
  `AgentPolicy` (union of `spec.policyRefs`) has at least one `spec.network`
  allow. This gate is **distinct** from the model proxy's gate
  (`spec.agent.endpointSecretRef` is set).
- **Pod spec:** same shape as the model proxy pod (D33) but:
  - Container image: `EgressProxyImage` field on `LoopReconciler` (like
    `ProxyImage` for the model proxy).
  - UID/GID: 65534 (distinct from agent 65532, model proxy 65533).
  - Env: `EGRESS_POLICY_HASH` (the `status.policy.effectiveHash` value),
    `LOOP_NAME`, `LOOP_NAMESPACE`, `POD_CIDR`, `SERVICE_CIDR` (from operator
    config, read once at controller init), `EGRESS_POLICY_JSON` (the effective
    network allows as JSON, for the Phase-1 binary to read; a future version
    mounts a file).
  - No `HTTPS_PROXY`/`HTTP_PROXY` (it doesn't proxy its own traffic).
  - Resources: limits cpu 100m, memory 128Mi; requests cpu 10m, memory 32Mi
    (parity with model proxy).
  - Liveness/readiness: TCP on 3128.
  - `automountServiceAccountToken: false` (pod-level).
  - Labels: `coxswain.io/loop: <loop>` + `coxswain.io/role: egress-proxy`
    (the `role` label distinguishes it from the model proxy's
    `coxswain.io/role: proxy` for NetworkPolicy selectors and the
    `ProxyConflict` gate).
- **Service:** `<loop>-egress-proxy` in the Loop's namespace, port 3128 TCP,
  selector matching the egress proxy pod labels. Stable name (the agent's
  `HTTPS_PROXY` points at it; a policy-change recreate keeps the Service stable).
- **Owner ref:** owned by the Loop (garbage collection + `IsControlledBy` gate).
- **Owned+Ready gate (D35a pattern):** before the sandbox pod is set to
  OperatingMode Running, when the egress proxy is expected (network allows
  present), the operator checks: the egress proxy pod exists AND
  `metav1.IsControlledBy(egressPod, loop)` AND `isPodReady(egressPod)`. If any
  is false → sandbox stays Suspended (not Failed, not OperatingMode Running).
  This is the same gate D35a applies to the model proxy.
- **ProxyConflict:** if a pod named `<loop>-egress-proxy` exists and
  `IsControlledBy` is false → `ProxyConflict=True` reason
  `ForeignEgressProxy` (records the conflict; the owned+Ready gate is what
  actually blocks the sandbox from Running). The foreign pod is NOT deleted (I2).
- **Policy-change recreate:** if `status.policy.effectiveHash` changes and the
  egress proxy pod's `EGRESS_POLICY_HASH` env no longer matches, the operator
  deletes and recreates the pod (the Service name stays stable, so the agent's
  `HTTPS_PROXY` value is unchanged). Same drift-recreate pattern as D33.

**envtest-first tests** (`internal/controller/loop_i42_egress_test.go`):
1. **Gate: no network allows → no egress proxy pod.** A Loop with a policyRef
   to an AgentPolicy that has `spec.network: []` (or no network section) →
   after reconcile, no pod named `<loop>-egress-proxy` exists, no Service, no
   `*_PROXY` env on the agent container.
2. **Gate: network allows present → egress proxy pod + Service created.** A
   Loop with a policyRef allowing `proxy.golang.org:443` → after reconcile, a
   pod named `<loop>-egress-proxy` exists (owned by the Loop, UID 65534, env
   carries `EGRESS_POLICY_HASH` = the effective hash, liveness TCP 3128), a
   Service `<loop>-egress-proxy` exists (port 3128), and the sandbox is
   Suspended (the egress proxy pod is not yet Ready).
3. **Owned+Ready gate: not Ready → sandbox Suspended.** With the egress proxy
   pod present but not Ready (no Ready condition) → sandbox stays Suspended,
   not OperatingMode Running.
4. **Owned+Ready gate: Ready → sandbox Running.** With the egress proxy pod
   present, owned by the Loop, and Ready → sandbox transitions to
   OperatingMode Running.
5. **Foreign pod → ProxyConflict + Suspended.** A pod named
   `<loop>-egress-proxy` exists but is owned by a different controller →
   `ProxyConflict=True` reason `ForeignEgressProxy`, sandbox Suspended.
6. **Policy change → pod recreate.** A Loop whose effective hash changes
   (a new AgentPolicy is referenced) → the old egress proxy pod is deleted and
   a new one created with the updated `EGRESS_POLICY_HASH`; the Service name
   is unchanged.

**kind acceptance** (`--context kind-coxswain-dev`):
- A Loop with a network-allowing AgentPolicy is created; the operator creates
  the egress proxy pod + Service; the sandbox is Suspended until the proxy is
  Ready. Once the proxy image is pulled and the pod is Ready, the sandbox
  transitions to Running. A `kubectl get pods` shows the egress proxy pod
  Ready. The agent container's env shows `HTTPS_PROXY=http://<loop>-egress-proxy.
  <ns>.svc:3128`.

**Dependencies:** I42a (the binary must exist as an image), D35a (the
owned+Ready gate pattern is already implemented for the model proxy — the
egress proxy reuses it), C6a (the effective policy union + hash computation
exists).

#### I42c — NetworkPolicy changes (agent egress + egress-proxy egress)

**Scope:** the agent pod's NetworkPolicy gains an egress rule to the egress
  proxy; a new NetworkPolicy is created for the egress proxy pod.
- **Agent NetworkPolicy** (`<loop>-agent-netpol`, existing from D34): add a
  fourth egress rule — to the egress proxy pod (selector
  `coxswain.io/loop=<loop>`, `coxswain.io/role=egress-proxy`) on port 3128
  TCP. This rule is present only when the egress proxy is expected (network
  allows present); with no network allows the agent's egress rules are
  model-proxy + DNS only (as now).
- **Egress proxy NetworkPolicy** (`<loop>-egress-proxy-netpol`, new):
  - `PolicyTypes: [Ingress, Egress]`.
  - **Ingress:** only from this Loop's agent pod (selector
    `coxswain.io/loop=<loop>`, `coxswain.io/role=agent`) on port 3128 TCP.
  - **Egress rule 1 (external with carve-outs):**
    `ipBlock {cidr: 0.0.0.0/0, except: [10.0.0.0/8, 172.16.0.0/12,
    192.168.0.0/16, 169.254.0.0/16, 127.0.0.0/8, <POD_CIDR>, <SERVICE_CIDR>]}`
    plus the v6 mirror: `ipBlock {cidr: ::/0, except: [fc00::/7, fe80::/10,
    ::1/128]}`. No port restriction (the proxy dials any port the allow
    specifies; the application layer enforces the port).
  - **Egress rule 2 (DNS):** to kube-dns (`kube-system`,
    `k8s-app=kube-dns`) on 53 UDP+TCP.
  - **Where the CIDRs come from:** the `LoopReconciler` reads `POD_CIDR` and
    `SERVICE_CIDR` from its own config (the controller's env, set at
    deployment: kind/k3s exposes these in the kube-system config or as
    `--pod-network-cidr` / `--service-cluster-ip-range` flags; the operator
    reads them once at startup and stores them as fields on the reconciler,
    analogous to `ProxyImage` / `EgressProxyImage`). They are NOT discovered
    per-Loop. In the envtest, the reconciler is constructed with test
    CIDR values.
- **Model proxy NetworkPolicy** (`<loop>-proxy-netpol`): unchanged from D34.

**envtest-first tests** (`internal/controller/loop_i42_netpol_test.go`):
1. **Agent netpol with egress proxy expected:** a Loop with network allows →
   the agent's NetworkPolicy has exactly 4 egress rules: model proxy:8080,
   egress proxy:3128, kube-dns:53 UDP, kube-dns:53 TCP (or a single kube-dns
   rule with both ports). Assert the egress-proxy rule's peer selector
   (`coxswain.io/role=egress-proxy`) and port 3128.
2. **Agent netpol without egress proxy (no network allows):** the agent's
   NetworkPolicy has only 3 egress rules (model proxy + DNS); no egress-proxy
   rule.
3. **Egress proxy netpol created:** a Loop with network allows → a
   NetworkPolicy named `<loop>-egress-proxy-netpol` exists. Assert: PolicyTypes
   include Ingress + Egress; the ingress rule's peer is the agent (role=agent)
   on port 3128; the egress rule 1 has `ipBlock.cidr=0.0.0.0/0` with the
   `except` list containing the expected carve-out CIDRs (including the
   reconciler's configured POD_CIDR and SERVICE_CIDR); the egress rule 2 is
   kube-dns on 53.
4. **Egress proxy netpol NOT created without network allows:** no NetworkPolicy
   named `<loop>-egress-proxy-netpol`.
5. **Carve-out CIDRs are from config:** change the reconciler's POD_CIDR to a
   different value → the egress proxy netpol's `except` list reflects the new
   value (not hardcoded).

**kind acceptance** (`--context kind-coxswain-dev`):
- With a Loop that has network allows, `kubectl get netpol` shows
  `<loop>-egress-proxy-netpol`. `kubectl get netpol <loop>-agent-netpol -o yaml`
  shows the egress-proxy rule. From inside the agent pod, a raw `curl` to a
  cluster-internal IP (e.g. the kube-apiserver service IP) is blocked
  (connection timeout); a `curl` through the proxy to an allowed external host
  succeeds.

**Dependencies:** I42b (the egress proxy pod + its labels must exist for the
NetworkPolicy selectors to target).

#### I42d — `*_PROXY` / `NO_PROXY` env vars on the agent container

**Scope:** when the egress proxy is expected (network allows present), the
  operator sets three env vars on the agent container:
- `HTTPS_PROXY=http://<loop>-egress-proxy.<ns>.svc:3128`
- `HTTP_PROXY=http://<loop>-egress-proxy.<ns>.svc:3128`
- `NO_PROXY=<loop>-proxy.<ns>.svc,<loop>-proxy.<ns>.svc.cluster.local,localhost,127.0.0.1`

  The `COX_MODEL_BASE_URL` must match a `NO_PROXY` entry (the operator sets it
  to the model proxy's `.svc` URL; the `NO_PROXY` list includes it). With no
  network allows, these three vars are NOT set (the agent has no external
  egress; the model proxy is the only proxy and it's reached directly via
  `COX_MODEL_BASE_URL`).

  These vars are added to the agent container's env alongside the existing
  `COX_MODEL_BASE_URL` (D33) and any user `spec.agent.env` vars. They use the
  standard names (not `COX_`-prefixed) so any HTTP client picks them up.

**envtest-first tests** (`internal/controller/loop_i42_env_test.go`):
1. **Network allows present → `*_PROXY` set.** A Loop with a network-allowing
   policy → the agent container's env includes `HTTPS_PROXY`, `HTTP_PROXY`,
   `NO_PROXY` with the expected values (assert the proxy URL contains the
   egress proxy Service name + port 3128; `NO_PROXY` contains the model proxy
   Service name + localhost).
2. **No network allows → `*_PROXY` NOT set.** A Loop with a policy that has no
   network allows → the agent container's env does NOT include `HTTPS_PROXY`,
   `HTTP_PROXY`, or `NO_PROXY`.
3. **`COX_MODEL_BASE_URL` matches `NO_PROXY`.** Assert the `COX_MODEL_BASE_URL`
   value (the model proxy `.svc` URL) appears as a prefix of one of the
   `NO_PROXY` entries (i.e. model calls won't be routed through the egress
   proxy).
4. **Standard names (not `COX_`).** Assert the env var names are exactly
   `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` (not `COX_HTTPS_PROXY` etc.).

**kind acceptance** (`--context kind-coxswain-dev`):
- `kubectl exec` into the agent pod (or inspect the pod spec) shows the three
  env vars with the correct values. A `curl https://<allowed-host>` from the
  agent goes through the egress proxy (the proxy's audit log records it); a
  `curl <model-proxy-url>` does NOT go through the egress proxy (the model
  proxy's audit log records it, not the egress proxy's).

**Dependencies:** I42b (the egress proxy Service must exist for the URL to be
  meaningful).

#### I42e — Phase-1 policy validation: reject in-cluster allows

**Scope:** the `AgentPolicy` CRD's validation (or the controller's
  effective-policy computation) rejects any `spec.network` allow that names an
  in-cluster target. An in-cluster target is:
- A hostname ending in `.svc` or `.svc.cluster.local` (cluster DNS suffix).
- An IP literal that falls within the pod CIDR or service CIDR (the same
  CIDRs from the operator config used in I42c's NetworkPolicy).
- The string `localhost` or `127.0.0.1` (the agent's own loopback; the agent
  should not be "allowing" itself as an external target).

  Rejection is at **policy validation time** (the Loop's reconcile errors with
  a clear message naming the offending allow), not silently dropped. This is
  the first layer of the SSRF defence; the egress proxy's resolved-IP check
  (I42a) is the backstop that catches rebinding/split-horizon even if a
  name passes validation.

  Implementation: a new CEL XValidation rule on `AgentPolicy.spec.network[].host`
  (rejects `*.svc`, `*.svc.cluster.local` suffixes) + a controller-side check
  (rejects IP literals in the pod/service CIDRs and `localhost`/`127.0.0.1`).
  The CEL rule handles the suffix cases (pure, no config needed); the
  controller-side check handles the CIDR cases (needs the operator config).

**envtest-first tests** (`internal/controller/loop_i42_validation_test.go`):
1. **`.svc` suffix rejected.** An AgentPolicy with `spec.network: [{host:
   "my-service.default.svc", port: 443}]` → the Loop's reconcile errors (the
   condition or event names the offending host); the sandbox is NOT created.
2. **IP in pod CIDR rejected.** An AgentPolicy with `spec.network: [{host:
   "10.244.0.5", port: 8080}]` (where the test reconciler's POD_CIDR is
   `10.244.0.0/16`) → reconcile errors.
3. **IP in service CIDR rejected.** An AgentPolicy with `spec.network: [{host:
   "10.96.0.1", port: 443}]` (where SERVICE_CIDR is `10.96.0.0/12`) →
   reconcile errors.
4. **`localhost` rejected.** An AgentPolicy with `spec.network: [{host:
   "localhost", port: 8080}]` → reconcile errors.
5. **Legitimate external host passes.** An AgentPolicy with `spec.network:
   [{host: "proxy.golang.org", port: 443}]` → reconcile succeeds, the egress
   proxy is created.
6. **CEL suffix rejection (CRD-level).** Creating an AgentPolicy directly
   (not via a Loop) with a `.svc` hostname → the API server rejects it
   (the CEL rule fires). This is an envtest that creates the AgentPolicy CR
   and asserts the create fails.

**kind acceptance** (`--context kind-coxswain-dev`):
- `kubectl apply` an AgentPolicy with a `.svc` host → the API server rejects
  it (CEL rule). A Loop referencing it → reconcile error visible in the Loop's
  status/conditions.

**Dependencies:** none for the CEL rule (pure CRD validation); the
  controller-side CIDR check depends on the same operator config as I42c.

#### I42f — D35 part 2: KubeArmor policy for the egress proxy (and model proxy)

**Scope:** extend the `EmitKubeArmorPolicy` function (C6b) to emit KubeArmor
  policies for BOTH proxy pods (the egress proxy AND the model proxy), in
  addition to the existing agent pod policy. The KubeArmor policy is the
  **inner fence** that catches any bug in the proxy's application-level
  enforcement.
- **Egress proxy KubeArmor policy:** selector targets the egress proxy pod
  (labels `coxswain.io/loop=<loop>`, `coxswain.io/role=egress-proxy`);
  `process.matchPaths` allows only the egress proxy binary (its absolute path
  in the image); `network.matchDNSQueries` + `matchProtocols` carry the
  effective `AgentPolicy` network allows (the same host:port pairs, translated
  to KubeArmor's DNS + protocol form — the port is lost in translation, which
  is fine because the egress proxy's NetworkPolicy + application layer carry
  the port precision). `action: Allow` per rule; `spec.action: Block`.
- **Model proxy KubeArmor policy:** selector targets the model proxy pod
  (labels `coxswain.io/loop=<loop>`, `coxswain.io/role=proxy`);
  `process.matchPaths` allows only the model proxy binary; `network` allows
  the model endpoint host (from `spec.agent.endpointSecretRef`'s resolved
  endpoint — or, in Phase 1, the model proxy's configured target). This is
  the "D35 part 2" for the model proxy (it did not have a KubeArmor policy
  before; only the agent pod did).
- **Same emitter:** the `EmitKubeArmorPolicy` function (or a sibling
  `EmitProxyKubeArmorPolicy`) in `internal/engine` takes an `EffectivePolicy`
  (or a new `ProxyPolicy` struct) and emits the KubeArmorPolicy. The egress
  proxy's process allows are `{path: <egress-proxy binary absolute path>}`;
  the model proxy's are `{path: <model-proxy binary absolute path>}`. The
  network allows are the effective `AgentPolicy` network allows for the
  egress proxy; for the model proxy, the network allows are the model
  endpoint (single host:port).
- **Owner ref:** each KubeArmorPolicy is owned by the Loop (garbage
  collection).
- **Gate:** the KubeArmor policies are emitted by the `Enforcer.Apply` call
  (C6b's existing seam) — the operator calls `Enforcer.Apply` for the agent
  pod's policy AND for each proxy pod's policy. The D30 gate
  (`Enforcing()` check) applies to ALL of them: if the engine is not
  enforcing, the sandbox stays Suspended.

**envtest-first tests** (`internal/engine/kubearmor_i42_test.go` +
`internal/controller/loop_i42_kubearmor_test.go`):
1. **Egress proxy KubeArmor policy shape:** given an `EffectivePolicy` with
   network allows `[{host: "proxy.golang.org", port: 443}]`, the emitted
   KubeArmorPolicy for the egress proxy has: selector matching the egress
   proxy pod labels; `process.matchPaths` = `[{path: /usr/local/bin/egress-
   proxy}]` (the binary's absolute path); `network.matchDNSQueries` =
   `[{domain: proxy.golang.org}]`; `spec.action: Block`.
2. **Model proxy KubeArmor policy shape:** the emitted KubeArmorPolicy for the
   model proxy has: selector matching the model proxy pod labels;
   `process.matchPaths` = `[{path: /usr/local/bin/proxy}]`; `network` allows
   the model endpoint.
3. **Both policies owned by the Loop:** envtest — after reconcile with both
   proxies expected, two KubeArmorPolicy objects exist (one per proxy), both
   owner-ref'd to the Loop.
4. **No egress proxy → no egress proxy KubeArmorPolicy:** a Loop with no
   network allows → only the model proxy's KubeArmorPolicy exists (if the
   model proxy is expected), not the egress proxy's.

**kind acceptance** (`--context kind-coxswain-dev`, requires KubeArmor
installed, same as `make kubearmor-e2e`):
- A Loop with network allows + a model endpoint → two KubeArmorPolicy objects
  exist. A disallowed exec in the egress proxy pod (e.g. `curl` when only the
  egress proxy binary is allowed) is blocked. A disallowed network connection
  from the egress proxy pod to a host not in the effective allows is blocked
  by KubeArmor (the inner fence catches the case where the application-level
  CONNECT check is buggy). Assert the KubeArmor policy is loaded on the pod
  (karmor probe or the policy's status).

**Dependencies:** C6b (the `Enforcer` interface + `EmitKubeArmorPolicy`
function + the D30 gate exist), I42b (the egress proxy pod + labels exist for
the selector), I42a (the egress proxy binary's absolute path is known).

#### I42 slice order and dependencies

```
I42a (binary)  ──┐
                 ├──→  I42b (controller: pod+Service+gate)  ──→  I42c (NetworkPolicies)
I42e (validation) ─┘                                              │
                                                                   ├──→  I42d (env vars)
                                                                   │
C6b (KubeArmor) ──────────────────────────────────────────────────┘
                                                                   │
                                                                   └──→  I42f (D35 part 2 KubeArmor for proxies)
```

- I42a and I42e are independent (can be built in parallel).
- I42b depends on I42a (the binary image) + D35a (gate pattern) + C6a (policy
  union).
- I42c depends on I42b (labels for selectors).
- I42d depends on I42b (Service name for the URL).
- I42f depends on C6b + I42b + I42a.
- I42c and I42d can be done in parallel (both depend on I42b, not on each
  other).
- **Full I42 acceptance:** a kind e2e that creates a Loop with a
  network-allowing AgentPolicy + a model endpoint, and asserts: the egress
  proxy pod is Ready; the agent's `HTTPS_PROXY` is set; an allowed external
  host is reachable through the proxy (audit records it); a disallowed host is
  403'd (audit records it); a raw TCP connection to a cluster-internal IP is
  blocked (NetworkPolicy); a `.svc` allow is rejected at validation; the
  KubeArmor policies exist for both proxies. This is the end-to-end I42 proof.

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
+ counter) → C7 (activity-audit stream) → **I42a → I42e (parallel) → I42b →
I42c + I42d (parallel) → I42f** (the egress proxy: binary, validation,
controller, NetworkPolicies, env vars, KubeArmor for proxies) → C5 (evil-agent
e2e, extended with a disallowed command + a disallowed host + a disallowed
external host via the egress proxy), then the reference runner A1 → A2 →
A3 → A4 (now driving the conformance agent through the proxy, `COX_MODEL_BASE_URL`
= localhost, each red→green — note A4 is now *context continuity*; the old A4
"runner runs checks" is dropped per ADR-0005), then B3 → B3a → B3b → B3c → B3d
(the verify path, expanded for the D17 canary + advisory scan, D18 restart
semantics, D19 read-only checkout) → B4 → B5 → B6. B1 and B2 (the claim-driven
phase machine + TamperedVerify via base-commit glob diff) are already done.
B2's anti-gaming guarantee (D7) is the highest-value test; B6 (foreign-owned
sandbox → condition, D8) needs B1's condition/event infrastructure. C5 (the
evil-agent e2e) is the slice that proves ADR-0006 + ADR-0007 and gates B3's
evidence as meaningful. I42 (the egress proxy) sits between C7 and C5: it is
the network enforcement layer that C5's "disallowed host" test case exercises.

**Gate (REVIEW-PHASE1-R8/R9):** no C3/C6/C7 code until the reviewer has
reviewed ADR-0007. C1, C2, C4 (and C8, C5's non-policy parts) may proceed
meanwhile. D29 (per-container egress via eBPF) and D30 (fail-closed when the
engine isn't enforcing) are **P1 ADR-0007 amendments that block C3/C6 code** —
they must be folded in (done in round 9) before those slices build on the
NetworkPolicy-only model.

**Infra:** `make kind-up` gains a step to install the eBPF engine (version
pinned once, like agent-sandbox) and a smoke that proves a disallowed `exec` is
blocked on kind (ADR-0007 Q3).
