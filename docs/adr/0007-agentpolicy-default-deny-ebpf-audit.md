# ADR-0007: AgentPolicy — default-deny, eBPF-enforced, streamed audit

**Status:** Proposed (records the owner's D28 answers, round 8; to be reviewed
by the reviewer before any C3/C6/C7 code)
**Date:** 2026-09-27
**Supersedes:** ADR-0006's open owner decision 2 (egress/dependency-installs
policy), which Q6 below resolves.
**Amends:** ADR-0006 item 4 (gVisor is no longer a default), per Q3's tension.

## Context

Round 7's D28 introduced the owner's product direction, recorded in
`CONTEXT.md`: *"Agent isolation is the goal, so they can only do what is
allowed by policy. Network isolation, command white lists and auditing."*
Round 8 records the owner's answers to the six questions the reviewer asked.
Those answers are the source of truth here; the items marked *implementation
choice* are the reviewer's recommendations, which the owner can override.

The net effect on ADR-0006: the agent-agnostic, zero-credential, deny-by-
default boundary stays, but the **mechanism** for "what is allowed" becomes an
explicit, owned **Policy** object (default-deny, additive allows) enforced by
an **eBPF/LSM engine outside the agent**, with **streamed** (not stored)
audit.

## Decision

### Q1 — Agent-agnostic, coding is the main (not the only) use case

Isolation, Policy, audit and the runner contract are **agent-agnostic**: no
git, test, or language assumptions in them. The git workspace + acceptance
checks + PR flow stays as the **first (and for v1, the only) Loop workflow**,
but it is written as *one workflow on top of the agnostic layer*, not baked
into it:

- `spec.workspace` and `spec.verify` stay on the **Loop** (they are the
  workflow, not the isolation layer).
- Nothing in `AgentPolicy`, the model proxy, the enforcement engine, or the
  audit stream refers to them.
- `CONTEXT.md` keeps "agents" (not "coding agents") in the definitions of
  Operator, Runner, Policy, and Audit; coding-specific terms (Workspace,
  Acceptance checks, TamperedVerify, PR) are unchanged.

### Q2 — `AgentPolicy`: its own CRD, default-deny, additive allows

A new CRD **`AgentPolicy`** (group `coxswain.wattu.com`).

- **Default deny.** With no policy, an agent may do only the **minimum the
  platform itself needs**: run its entrypoint, read/write its own workspace,
  and talk to the model proxy on `localhost`. Every other capability is
  granted by **adding allow rules**.
- Rules are **only ever allows** — there are no "deny" rules to reason about.
- Multiple policies combine as the **union of allows**.
- *Implementation choice (reviewer rec):* a **namespaced `AgentPolicy`**,
  selected by the Loop (`spec.policyRefs[]`, or a label selector); plus an
  optional **cluster-scoped `ClusterAgentPolicy`** for org-wide allows. Who
  may create policies is plain Kubernetes **RBAC — a different role from who
  may create Loops**, so a Loop author cannot grant themselves more.
- Consequence: the README sample must ship with a **minimal `AgentPolicy`**,
  or the sample agent can do nothing. The C5 evil-agent test's expected
  failures come from the **absence of allows** — which is the property to
  prove.

### Q3 — eBPF enforcement (KubeArmor in BPF-LSM mode)

Command allowlists are enforced by an **eBPF/LSM engine outside the agent**;
in-agent tool allowlists are at most advisory.

- *Implementation choice (reviewer rec):* **KubeArmor** in BPF-LSM mode. Its
  model fits Q2 directly — per-pod `Allow` rules for process, file, and
  network, with a default posture of `block` — and it streams alerts and logs
  (Q4). The operator **translates** each Loop's effective `AgentPolicy` into a
  `KubeArmorPolicy` selecting that Loop's sandbox pod, **owned by the Loop**.
  (Tetragon is the alternative: stronger observability, but allowlists are
  awkward because its enforcement is "kill on match".) Keep the engine behind
  an **internal interface** so it can be swapped.
- **Host check (done at review time):** this dev host runs kernel 6.1 with
  `bpf` in the active LSM list, `CONFIG_BPF_LSM=y`, and BTF present, so
  BPF-LSM enforcement works on kind here. The homelab K3s nodes need the same
  check before production use.
- **Tension to record (amends ADR-0006 item 4):** gVisor
  (`runtimeClassName`) intercepts syscalls in user space, so host eBPF engines
  don't see or enforce inside a gVisor sandbox. With eBPF chosen, **gVisor is
  dropped as a default** (kept as an opt-in where the engine supports it, and
  documented plainly that enforcement is then the runtime's, not eBPF's).

### Q4 — Streamed activity audit (Coxswain emits, never stores)

Coxswain **emits** agent-activity audit as a structured stream; it does **not**
store it. Capture is the cluster's choice (e.g. an existing Alloy/Loki
pipeline).

- **Trusted sources only:** the **model proxy** (each model request/response
  metadata + token counts), the **eBPF engine** (process exec, file access,
  network connects — allowed and blocked), and the **operator** (its own
  decisions, already events + `status.history`). The agent's own traces are
  **not** audit.
- *Implementation choice (reviewer rec):* **JSON lines** on each source's
  stdout with a common envelope —
  `{time, loop, namespace, iteration, source, action, target, verdict,
  detail}` — so any log collector can capture and join them by Loop.
  OpenTelemetry export can come later.
- `CONTEXT.md` splits **Audit trail** into **decision audit** (operator;
  events + history, as now) and **activity audit** (streamed, above).

### Q5 — Block and record (no terminal reason)

A policy violation is **blocked and recorded** in the activity audit. It does
**not** fail or pause the Loop — there is **no `PolicyViolation` terminal
reason**.

- Consequence to record: a blocked agent will usually fail to make progress
  and hit `maxIterations` or the Phase 2 stall detector — the right outcome.
- Add a count of blocked actions to `status`: **`status.policy.blockedCount`**
  plus a **`PolicyBlocked` condition** carrying the latest target, so a stuck
  Loop's cause is visible without reading logs.

### Q6 — Network allowlist is part of `AgentPolicy`

Egress rules live in **`AgentPolicy`** (allowed hosts/CIDRs + ports), not in
`spec.agent`. This **replaces ADR-0006's open decision 2** (dependency
installs): a Loop that needs `go mod download` gets it by a policy that allows
the module proxy — **nothing is open by default**.

- *Implementation choice:* enforce egress in **two layers** — a Kubernetes
  `NetworkPolicy` generated from the allows (pod-level, coarse) **and** the
  eBPF engine's network rules (process-level, and the source of the audit
  records).
- The model proxy's own egress to the model endpoint is **platform
  infrastructure, allowed by the operator, not by the user's policy**.

**Per-container egress is enforced by the eBPF layer, not the NetworkPolicy
(D29, round 9).** A Kubernetes `NetworkPolicy` selects **pods**, not
containers. The proxy container must reach the model endpoint, so a pod-scoped
NetworkPolicy has to allow it for the **whole sandbox pod** — and the
**agent** container then reaches it too, bypassing the proxy (and, if the
endpoint needs no key — the homelab vLLM is the likely first target — the agent
gets unmetered, unaudited model access). The **eBPF layer** is what enforces
**per-container** egress; the NetworkPolicy is only the coarse outer fence
(the union of both containers' allows). The engine policy scopes by
**container** in the pod (KubeArmor can do this):
- the **agent container's** policy allows network connects only to
  `localhost` (the proxy) **plus the user's `AgentPolicy` allows** — nothing
  else;
- the **proxy container's** policy allows only the model endpoint.

C5 gains a case: the evil agent calls the model endpoint **directly** (not
through the proxy) → **blocked by eBPF**, recorded in the activity audit, and
**absent from the proxy's metering**.

**Fail closed when the engine isn't enforcing (D30, round 9).** If the eBPF
engine isn't installed, isn't running on the node the sandbox lands on, lacks
BPF-LSM there (e.g. a K3s node without it), or the translated policy was
rejected, the agent would run **unrestricted** and nothing would say so
(KubeArmor can fall back to AppArmor or audit-only depending on the node). The
operator lets the sandbox run (`OperatingMode: Running` / the agent container
started) **only once it has positive evidence that enforcement is active on
*that node* for *that Loop's policy*** — e.g. the engine policy's status is
applied **and** the node reports BPF-LSM enforcement. Otherwise it sets a
**`PolicyEnforced=False` condition** with a reason — `EngineUnavailable` |
`NodeNotEnforcing` | `PolicyRejected` — and the Loop **waits** (no terminal
reason, consistent with Q5's "never fail the Loop"; *the owner may prefer a
terminal reason — flag, don't pick*). Seam: envtest with the engine's status
objects faked; e2e on kind with the engine removed → the sandbox **never
runs**.

## Learn mode (D31, round 9)

Default-deny exec is hard to author by hand: real agents (Claude Code, pi,
Codex) exec a lot (`node`, `bash`, `git`, toolchains, their own helpers), so a
working per-agent-image policy is trial and error. Add a **learn mode**:

- `AgentPolicy` (or the Loop) can request an **audit-only posture** — the
  engine records instead of blocking;
- a small tool (`kubectl cox policy suggest <loop>` later) turns the recorded
  activity into a candidate `AgentPolicy` for a human to review;
- **reference policies** per supported agent image ship under
  `config/samples/policies/`.

Learn mode must be **explicit**, **visible in status** (`PolicyEnforced=False`,
reason `AuditOnly`), and **never the default**.

## Which pods an `AgentPolicy` covers (D32, round 9)

The user's `AgentPolicy` selects **the Loop's sandbox pod** (agent + proxy
containers, per D29). It does **not** cover:

- **Verify Job pods** (ADR-0005 D12) run the agent's committed code. They get a
  **fixed, operator-owned *verify* policy** (checkout tools + the check
  commands' toolchain, no egress) — **not** the user's `AgentPolicy`.
- **Publish step / trusted sidecars** (C4) are **operator-owned, not
  user-policed** — kept out of the agent container's policy selector.
- **Policy changes mid-Loop** apply at the **next iteration boundary**, and each
  `status.history[]` entry records the **effective policy's hash/generation**,
  so the decision audit shows what the agent was allowed to do in each
  iteration.

## Consequences

- New CRD `AgentPolicy` (+ optional `ClusterAgentPolicy`) and a
  `spec.policyRefs[]` on the Loop. The operator translates effective allows →
  `KubeArmorPolicy` (owned by the Loop) + `NetworkPolicy`.
- The eBPF engine is a new dependency installed with the cluster (`make
  kind-up`, version pinned once, like agent-sandbox), behind an internal
  interface.
- `status` gains `policy.blockedCount` + a `PolicyBlocked` condition.
- **`blockedCount` source (I32, round 9):** the operator derives it by
  **consuming the engine's alert stream** (the KubeArmor relay) — a new runtime
  dependency and a new **trust edge** (the operator reads engine alerts). C8's
  seam names this mechanism; a **relay outage must show "count unknown", not
  zero** (so a stuck Loop's cause is never masked as "nothing was blocked").
- The README sample ships a minimal `AgentPolicy`. The C5 evil-agent test's
  expected failures are the **absence of allows**.
- ADR-0006: gVisor opt-in (not default); open decision 2 resolved by Q6.
- Plan slices: C3 = NetworkPolicy generated from `AgentPolicy`; new
  **C6** (AgentPolicy CRD + engine-policy translation), **C7** (activity-audit
  stream), **C8** (`PolicyBlocked` condition + counter); C5 extended with a
  disallowed command and a disallowed host **and a direct model-endpoint call
  (blocked by eBPF, per D29)**.
- Production: the homelab K3s nodes must pass the same BPF-LSM/BTF host check
  before eBPF enforcement is relied on there.

## Findings (observed, 2026-09-27, dev host kernel 6.1.0-53, KubeArmor v1.7.5)

These are runtime observations that shape how the engine is installed and
operated. They amend the Q3 install story, not the ADR's decisions.

- **F1 — the exec allowlist's block posture is `defaultFilePosture`, not
  `spec.action`.** KubeArmor v1.7.5's BPF-LSM gates the process allowlist's
  block-vs-audit choice on the default posture (the block sentinel is keyed on
  `defaultPosture.FileAction` in the agent's `rulesHandling.go`); the policy's
  top-level `action: Block` is not what does it. `karmor install` defaults
  `defaultFilePosture: audit`, so a disallowed exec is *evaluated and logged*
  ("Armored Up" + an audit alert) but **allowed**. Implication: the posture
  must be `block` (plus `process` visibility) **before the node agent first
  starts** — hence `make kind-up` passes `-b all --viz process,file,network`
  to `karmor install` rather than editing the config afterwards (karmor v1.4.9
  rejects the `-viz` shorthand; only `-b`/`-a`/`-i` have shorthands).
- **F2 — a KubeArmor agent stop/restart can wedge the node's BPF subsystem
  (kernel 6.1).** Observed: an agent process exiting through SIGKILL hung in
  D state at `bpf_trampoline_unlink_prog → unregister_ftrace_direct_multi →
  ftrace_shutdown → synchronize_rcu_tasks` (with `rcu_tasks_kthread` also D),
  holding the BPF trampoline lock; every subsequent `bpf_prog_load` on the
  host then blocked (a fresh cluster's snitch `bpf_check` sat in
  `bpf_trampoline_get`), so no new BPF-LSM program could load until the host
  rebooted. Consequences:
  - `make kind-up` **must not** `rollout restart` the KubeArmor agent after
    changing the posture (the old edit-config + restart flow is both
    unnecessary per F1 and host-disruptive per F2).
  - **Production installs must treat KubeArmor agent restarts/upgrades as
    node-disruptive operations: drain the node first** (a hung agent
    teardown takes the node's BPF subsystem down with it — not just the
    agent). Same applies to `kind delete` of a cluster whose agent has hung
    (it can hang the control-plane container's teardown); the only reliable
    recovery is a host reboot.

## Open owner decisions (NOT picked by the builder)

- **ADR-0006's open decision 1** (first real agent to adapt) is still open and
  is *not* answered by Q1 (Q1 only fixes the agnostic layer). It is listed,
  not picked. **D31's reference policies should start with whichever agent the
  owner picks** (I33, round 9).
- Q2/Q3/Q4/Q6's *implementation choices* (namespaced `AgentPolicy` +
  `spec.policyRefs[]`; KubeArmor over Tetragon; JSON-lines envelope; two-layer
  egress) are the **reviewer's** recommendations and are overridable by the
  owner — recorded here as recommendations, not settled.

## Known limit: NetworkPolicy cannot match DNS names (D34, alongside I41)

Kubernetes `NetworkPolicy` selects by pod/namespace labels or IP blocks, not
by DNS name. For the proxy's model egress rule (D34), this means:

- An **in-cluster** endpoint (a same-namespace Service name like `vllm:8000`)
  is expressed as a same-namespace podSelector — which admits **any** pod in
  that namespace on the model port, not just the target Service. The
  hostname-level precision (only `vllm`, not `other-service`) is enforced by
  the proxy's **KubeArmor** policy (D35), which CAN match the DNS query
  domain.
- An **external** endpoint (FQDN that does not resolve to an in-cluster IP)
  cannot be expressed in a NetworkPolicy at all. The model egress rule is
  omitted (fail-closed) and the hostname-level allow belongs to the
  KubeArmor proxy policy.

This is recorded here so that the D34 NetworkPolicy's coarser-grained
model-egress peer is understood as the outer fence (port-level), with the
D35 KubeArmor policy providing the inner fence (hostname-level).

**AgentPolicy network allows (D34, R17 P2):** the agent's `AgentPolicy`
`spec.network` allows (e.g. `proxy.golang.org:443`) are NOT translated into
the agent NetworkPolicy's egress rules. AgentPolicy (C6a, PR #7) is not
merged on the D34 branch, so `spec.policyRefs` and the AgentPolicy type are
unavailable. Until then, the agent's egress is proxy + DNS only (fail-closed).

**Resolved — I42 (docs/REVIEW-PHASE1-R14.md, owner decision 2026-09-28):** the egress proxy (option b). See the "I42 resolution" section below. The original question — how to express AgentPolicy network allows in a NetworkPolicy — is answered by the egress proxy: NetworkPolicy remains the coarse outer fence (default-deny on the agent pod's egress), while the operator-owned egress proxy enforces the hostname:port allowlist at the HTTP CONNECT / SNI / Host layer. Until the egress proxy is implemented, agent egress stays proxy + DNS (fail-closed).

## I42 resolution: the egress proxy (owner decision, 2026-09-28)

**Status:** Owner-decided (option b from I42, docs/REVIEW-PHASE1-R14.md).
This section amends the D34 "Open design question — I42" paragraph above (now retired by this
resolution) and records the design for the implementation that follows.

**Problem restated:** since D34, the agent pod's NetworkPolicy is
default-deny egress (proxy + DNS only). An `AgentPolicy` `spec.network` allow
(e.g. `proxy.golang.org:443`) is not enforced as an allow. NetworkPolicy
cannot match hostnames; a port-only rule admits any host on that port
(exfiltration). KubeArmor's DNS matching doesn't close the gap because an
agent can dial a hard-coded IP without a DNS lookup. The owner chose **option
(b): an operator-owned egress proxy** that enforces the hostname allowlist at
the HTTP/HTTPS layer.

### Per-Loop vs shared egress proxy

**Decision: per-Loop egress proxy**, mirroring the D33 model proxy.

Reason: the effective `AgentPolicy` is the union of the Loop's
`spec.policyRefs` and is Loop-specific. A shared proxy would need to look up
the effective policy per connection (adding a policy-lookup dependency to the
hot path), and a compromised or misconfigured shared proxy would affect all
Loops. Per-Loop keeps the blast radius contained, reuses the D33 pattern
(operator creates a proxy pod + Service owned by the Loop, with the
`ProxyConflict` / `IsControlledBy` gate from D35), and makes the
KubeArmor policy per-pod (D35 part 2). The cost is one extra pod per Loop
that has network allows — acceptable for Phase 1 (a small number of
concurrent Loops on a single node).

A shared proxy with per-connection policy lookup is the natural Phase 2
optimisation (one proxy, an `AgentPolicy` watcher, a small in-memory policy
cache keyed by Loop UID). The design below keeps that door open: the
enforcement logic (the CONNECT/SNI/Host check against the effective allows)
is independent of the proxy's topology. (The `Enforcer` seam in
`internal/engine` covers the eBPF engine, not the egress proxy; the proxy's
enforcement seam is new.)

### Enforcement point: HTTP CONNECT + SNI/Host check

The egress proxy is an **HTTP/HTTPS forward proxy** (the same shape as
`CONNECT` handling in a corporate egress proxy). It enforces the effective
`AgentPolicy` network allows at two points:

1. **HTTP CONNECT:** for HTTPS, the client sends
   `CONNECT host:port`. The proxy checks `host:port` against the effective
   allows. Disallowed → `403 Forbidden` + audit record (blocked). Allowed →
   the proxy resolves the host **itself**, checks the resolved IP (below),
   and opens a TCP tunnel to **that IP** (the same one it checked — no
   re-resolution at dial time, so no TOCTOU re-resolution) and relays bytes.
2. **SNI / Host header:** for plain HTTP, the `Host` header is checked.
   For HTTPS CONNECT, the SNI (from the TLS ClientHello) is checked as a
   second gate. The rule is strict: **the SNI must equal the CONNECT host**
   and both must be in the effective allows. SNI ≠ CONNECT host → tunnel
   closed + audit record (blocked). **No SNI** (ECH, or a client that omits
   it) → tunnel closed + audit record (blocked). Disallowed in any case →
   audit record.

**Resolved-IP check (SSRF defence):** the CONNECT/SNI/Host check is a
**hostname** check. The proxy therefore resolves the hostname itself (via the
cluster DNS the egress proxy's NetworkPolicy allows) and, **before dialing**,
rejects the connection if any resolved IP is in a non-allowlisted private or
cluster-internal range: `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`,
`169.254.0.0/16` (link-local, including the cloud metadata endpoint),
`127.0.0.0/8`, `100.64.0.0/10`, `::1`, `fc00::/7`, `fe80::/10`, and the
cluster **pod** and **service** CIDRs. In **Phase 1, allows must not name
in-cluster targets** (a `*.svc` name, a cluster DNS suffix, or an in-CIDR IP
literal is rejected at policy-validation time, alongside the existing
`COX_`-name validation); the resolved-IP check is the backstop that makes
"in-cluster target" unexploitable even if an allow slips through (DNS
rebinding, an attacker-controlled domain in the allowlist, a split-horizon
name, or a name that changes resolution between validation and dial).
The resolved IP is recorded in the audit `detail` (below) so a rebind is
visible after the fact.

**Why this resolves I41's port loss:** the egress proxy enforces **host AND
port** — the full `host:port` pair from the `AgentPolicy` allow. A KubeArmor
`matchDNSQueries` rule can match the DNS name but cannot carry a port (the
D34/D35 port-loss that produced I41's `PolicyTranslationLossy` condition).
With the egress proxy as the enforcement point, the port is checked at
CONNECT time, so the `PolicyTranslationLossy` condition becomes unnecessary
for network allows (the port is enforced). The KubeArmor agent policy's
network rules remain as a **coarse outer fence** (default-deny on the agent
pod's egress, allowing only the proxy pod + DNS), not the hostname-level
enforcement layer.

**What the proxy does NOT do:** it does not terminate TLS (it tunnels bytes
after the CONNECT handshake). It does not inspect the payload. It does not
MITM. The SNI check reads the SNI field from the first TLS record the client
sends over the tunnel (the proxy sees the ClientHello because the client
sends it through the tunnel); if the SNI is absent, disagrees with the
CONNECT host, or is not itself in the effective allows, the proxy closes the
tunnel and records the block.

**Known limits of tunnel-only mode (recorded, not solved):**

- **Domain fronting:** the proxy sees the SNI but **not** the inner HTTP
  `Host` header (TLS is opaque). An allowlisted CDN hostname can therefore
  front a disallowed origin that shares the CDN's TLS certificate. A
  tunnel-only proxy cannot close this; only a TLS-terminating proxy (out of
  scope, below) could. Phase 1 records this as a policy-authoring concern:
  authors of CDN allows should understand that the CDN's fronted origins
  are effectively co-allowlisted.
- **No SNI:** denied (see the SNI rule above), rather than allowed on the
  CONNECT host alone — denying is the fail-closed choice.
- **ECH (Encrypted Client Hello):** the SNI is encrypted; the proxy treats
  the connection as SNI-absent and denies it (Phase 1 does not decrypt ECH).

**Tools that ignore the proxy fail closed:** the operator sets
`HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY` on the agent container (below).
Tools that honour these env vars (curl, wget, Go's `net/http`, Python's
`requests`, most HTTP clients) route through the proxy and are enforced.
Tools that **ignore** the proxy env vars (raw TCP sockets, `nc`,
`/dev/tcp`, hardcoded IP dials, some DNS-over-HTTPS clients) **do not go
through the proxy** and are **blocked by the NetworkPolicy** (the agent pod's
egress is default-deny except to the proxy pods + DNS). This is the
fail-closed guarantee: the NetworkPolicy is the outer fence (no raw egress),
the proxy is the inner enforcement point (hostname + port allowlist).

### How the agent is pointed at the egress proxy

The operator sets three env vars on the **agent container** (not the model
proxy, not the egress proxy):

| Env var | Value |
|---|---|
| `HTTPS_PROXY` | `http://<loop>-egress-proxy.<ns>.svc:3128` |
| `HTTP_PROXY` | `http://<loop>-egress-proxy.<ns>.svc:3128` |
| `NO_PROXY` | `<loop>-proxy.<ns>.svc,<loop>-proxy.<ns>.svc.cluster.local,localhost,127.0.0.1` |

- **Port 3128** (the standard Squid/forward-proxy port) is the egress proxy's
  listen port. The model proxy (D33) listens on 8080.
- **`NO_PROXY`** excludes the model proxy Service — **`<loop>-proxy`**
  (D33's Service name; not `<loop>-model-proxy`) — in both its short
  (`.svc`) and FQDN (`.svc.cluster.local`) forms, plus localhost. DNS is not
  HTTP, so the cluster DNS IP deliberately does **not** appear in `NO_PROXY`
  (an entry there would be inert). The agent's NetworkPolicy already allows
  DNS egress to kube-dns.
- **`COX_MODEL_BASE_URL` must match a `NO_PROXY` entry.** The operator sets
  `COX_MODEL_BASE_URL` to the model proxy's `.svc` URL (D33). If it did not
  match a `NO_PROXY` entry, the agent's model calls would be routed through
  the egress proxy, which would **deny** them (the model endpoint is not in
  the agent's effective network allows). The operator keeps the two in
  sync when it sets them.
- **`COX_`-style reservation:** the `COX_` env var prefix is reserved for
  operator-managed variables (I34: `COX_MODEL_BASE_URL`,
  `COX_WORKSPACE_PATH`, etc.). The proxy env vars are **not** `COX_`-prefixed
  because they use the standard names that all HTTP clients recognise
  (`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`). The `COX_` reservation does not
  apply to them — the standard names let every HTTP client pick them up
  without coxswain-specific client support. The `COX_` prefix remains for
  coxswain-specific variables (`COX_MODEL_BASE_URL`).
- The egress proxy pod's env vars are **not** `HTTPS_PROXY`/`HTTP_PROXY`
  (it doesn't proxy its own traffic). Its only env var is `EGRESS_POLICY_HASH`
  (the effective policy's hash, for audit correlation with
  `status.policy.effectiveHash`). It sets no `*_PROXY` variables.

### NetworkPolicies

The per-Loop NetworkPolicy set grows from 2 (agent + model proxy) to 3
(agent + model proxy + **egress proxy**). The egress proxy is created only when the Loop's effective `AgentPolicy`
has `spec.network` allows. That gate is distinct from the model proxy's
(`endpointSecretRef` is set), so a Loop with model calls but no external
egress runs its model proxy without an egress proxy, and vice versa.

**Agent pod NetworkPolicy** (`<loop>-agent-netpol`):
- `PolicyTypes: [Ingress, Egress]`
- **Ingress:** empty (deny all, as now).
- **Egress:** three rules:
  1. To the **model proxy** pod (`<loop>-proxy`, D33 labels) on port 8080
     TCP.
  2. To the **egress proxy** pod (`<loop>-egress-proxy`, D35-egress labels)
     on port 3128 TCP.
  3. To **kube-dns** (`kube-system` namespace, `k8s-app=kube-dns`) on port
     53 UDP+TCP.
- No other egress. No port-only rules. No `ipBlock` rules for external
  hosts. The agent's raw egress to the internet is **not possible** — it must
  go through the egress proxy (enforced by the proxy) or be blocked (raw
  TCP to a non-proxy IP).

**Model proxy pod NetworkPolicy** (`<loop>-proxy-netpol`): unchanged from
D34 — ingress from agent on 8080, egress to model endpoint peer + DNS.

**Egress proxy pod NetworkPolicy** (`<loop>-egress-proxy-netpol`):
- `PolicyTypes: [Ingress, Egress]`
- **Ingress:** only from this Loop's agent pod on port 3128 TCP. (Per-Loop
  labels, same as the model proxy's ingress rule.)
- **Egress:** two rules:
  1. **External, with carve-outs:** `ipBlock {cidr: 0.0.0.0/0, except: [10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16, 127.0.0.0/8, <pod CIDR>, <service CIDR>]}` plus the v6 mirror (`::/0` except `fc00::/7`, `fe80::/10`, `::1/128`). The carve-outs cover every cluster-internal and link-local range the proxy could otherwise pivot into (kube-apiserver, other Loops' model proxies **and their mounted keys**, node/kubelet ports, other namespaces' services, `169.254.169.254`). The pod and service CIDRs come from the operator's config (the controller reads them at install; kind/k3s expose them in the `kube-system` config / `--pod-network-cidr` / `--service-cluster-ip-range`), not auto-discovery per Loop.
  2. **kube-dns** on 53 UDP+TCP (the proxy resolves CONNECT hosts itself —
     see the resolved-IP check in the enforcement section).

**Why the egress proxy's egress is this wide (and no wider):** the egress
proxy is the **enforcement point**, but enforcement is layered —

1. the **NetworkPolicy** above is the **outer fence**: the pod can never
   reach cluster-internal IPs at all (they are carved out of the `ipBlock`),
   so even a fully compromised proxy is not an SSRF pivot into the apiserver,
   other Loops' model proxies/keys, node ports, or the metadata endpoint;
2. the **proxy's resolved-IP check** is the **middle layer**: an
   allowlisted hostname whose resolution lands in a carved-out range is
   rejected **before** the dial (and the dial is to the same checked IP —
   no TOCTOU re-resolution), so DNS rebinding and split-horizon names cannot
   reach a private address even on the external path;
3. the **CONNECT/SNI/Host + port check** is the **inner enforcement**: the
   hostname itself must be in the effective `AgentPolicy` allows.

The `ipBlock` carve-outs make the NetworkPolicy "wide" in the sense that it
allows the public internet — that is what a public egress proxy needs — but
"wide" no longer includes the cluster. This is the same trust model as the
model proxy (D33): the model proxy pod has egress only to the model endpoint
peer + DNS, and the enforcement is at the proxy's application layer (it
forwards only to the configured `MODEL_ENDPOINT`).

The egress proxy's own KubeArmor policy (D35 part 2, when C6b merges) will
add a **second layer**: the proxy's process is allowed to exec only its own
binary, and its network is allowed only to the hosts:ports in the effective
`AgentPolicy` allows (the KubeArmor `matchDNSQueries` + `matchProtocols`
rules). This is the inner fence that catches any bug in the proxy's
application-level enforcement (e.g. a deserialisation error that skips the
CONNECT check). The KubeArmor policy is **per-Loop** (owned by the Loop,
same shape as the model proxy's KubeArmor policy under D35 part 2 — which
does not exist yet; the model proxy's NetworkPolicy carries that gate
today).

### Hardening parity

The egress proxy pod gets the **same hardening as the model proxy pod**
(D33):

| Setting | Value |
|---|---|
| `runAsUser` / `runAsGroup` | 65534 (distinct from agent 65532 and model proxy 65533; the D33 implementation pins these as `nonRoot` constants) |
| `runAsNonRoot` | true |
| `readOnlyRootFilesystem` | true |
| `allowPrivilegeEscalation` | false |
| `capabilities.drop` | `["ALL"]` |
| `seccompProfile.type` | `RuntimeDefault` |
| `automountServiceAccountToken` | false (pod field; no container-level row — the proxy needs no SA token) |
| Resources | limits: cpu 100m, memory 128Mi; requests: cpu 10m, memory 32Mi (CPU parity with the D33 model proxy: 100m limit / 10m request) |
| Liveness/readiness probes | TCP on port 3128 |

The egress proxy is a **non-root, read-only, no-privilege-escalation** pod
that listens on one port and tunnels bytes. It has no access to the model
credentials (those are mounted only into the model proxy pod). It has no
access to the agent's workspace (those volumes are mounted only into the
agent container).

### Audit record per connection (ADR-0007 Q4)

The egress proxy emits a **JSON-lines audit record on stdout** for every
connection attempt (allowed and blocked), using the Q4 envelope:

```json
{
  "time": "2026-09-28T12:00:00Z",
  "loop": "<loop-name>",
  "namespace": "<ns>",
  "iteration": 3,
  "source": "egress-proxy",
  "action": "connect",
  "target": "proxy.golang.org:443",
  "verdict": "allowed",
  "detail": "sni=proxy.golang.org, proto=https, ip=151.101.0.223, policy=<hash>"
}
```

- `source`: `"egress-proxy"` (distinct from `"model-proxy"` and
  `"kubearmor"`).
- `action`: `"connect"` (CONNECT tunnel) or `"http"` (plain HTTP request).
- `target`: the `host:port` from the CONNECT line or Host header.
- `verdict`: `"allowed"` (tunnel opened) or `"blocked"` (403 / tunnel
  closed).
- `detail`: the SNI (if present), the protocol (http/https), the **resolved
  IP the tunnel dials** (the SSRF defence — a rebind to a different IP is
  visible after the fact), and the policy hash (correlation with
  `status.policy.effectiveHash`).

**`iteration`:** the Q4 envelope carries `iteration` so records can be joined
per plan→implement→verify cycle. The egress proxy is a long-lived pod and its
env is immutable, so it **cannot** read `status.iteration` itself. The proxy
emits the record **without** `iteration`; the **relay** (the thing that joins
records from all sources into the Q4 stream) fills `iteration` by looking up
the Loop's current `status.iteration` at capture time. This is the same
joining the relay already does for the model proxy and eBPF engine sources
(they too stream without a per-record iteration, which the relay attaches by
time window). A record captured between two iterations is attributed to the
iteration in effect at capture time; the `policy` hash in `detail`
disambiguates if the effective policy changed mid-iteration.

This gives the activity audit (Q4) a **single point** to record all egress:
every external connection the agent makes goes through the egress proxy, so
every connection is audited. The KubeArmor audit records (blocked raw TCP
connections that bypass the proxy) are the **second source** — they catch
the tools-that-ignore-the-proxy case (raw TCP to a non-proxy IP, blocked by
the NetworkPolicy, recorded by KubeArmor).

The `status.policy.blockedCount` (Q5, C8) counts **both** sources: the
egress proxy's blocked records **and** the KubeArmor blocked-network records.
The relay outage rule (I32) applies: a relay outage shows "count unknown,"
not zero.

### Failure modes

| Failure | Behaviour |
|---|---|
| Egress proxy pod not Ready (crash, ImagePullBackOff) | **Gated, like the model proxy (D35a):** the sandbox stays **Suspended** (not Running) until the egress proxy pod is owned by the Loop **and** Ready — the same `IsControlledBy` + `PodReady` gate D35a applies to the model proxy. The agent never starts with `HTTPS_PROXY` pointing at nothing or at a foreign pod. A proxy outage is an infrastructure problem, not a policy violation, so the Loop is **not** failed (Q5); `PolicyEnforced` is unaffected. The operator should alert on the egress proxy pod's `Ready` condition (same as the model proxy pod under D35a). |
| Egress proxy pod is a **foreign** pod (not owned by the Loop) | Same gate as above: `IsControlledBy` is false, so the sandbox stays Suspended, and `ProxyConflict=True` reason `ForeignEgressProxy` records the conflict. The foreign pod is **not** deleted (I2). (The `ProxyConflict` condition is the *record*; the *gate* is the owned+Ready check — consistent with the not-Ready row.) |
| AgentPolicy network allows are empty (no `spec.network` entries) | The egress proxy pod is **not created** — the gate for the egress proxy is "the effective policy has `spec.network` allows", **distinct from** the model proxy's gate (`endpointSecretRef` is set). The agent's NetworkPolicy has only the model-proxy + DNS egress rules. No `*_PROXY` env vars are set. The agent has no external egress (fail-closed). |
| AgentPolicy network allows change mid-Loop | The effective policy is carried on the egress proxy pod's env (immutable after creation). The operator deletes and recreates the pod when the effective policy changes (same drift-recreate pattern as D33's model proxy). The sandbox stays Running during the recreate: in-flight agent connections to the old pod are dropped, and the agent retries against the new pod via the Service (whose stable name keeps the agent's `HTTPS_PROXY` value unchanged). The `status.policy.effectiveHash` is updated on the next reconcile. |
| Egress proxy bug: CONNECT check skipped (e.g. a deserialisation error) | The egress proxy's NetworkPolicy egress is the `ipBlock` with cluster-internal carve-outs (above), so the NetworkPolicy layer stops a skipped check from reaching the cluster but not from reaching the public internet — the D35 part 2 KubeArmor policy for the egress proxy (above) is the backstop for the public-internet case. Until D35 part 2 lands, the observable signal is the per-connection audit record: a `verdict: allowed` for a host outside the effective allows is anomalous. Once D35 part 2 lands, a bypassed CONNECT check that connects to a disallowed host:port is blocked by KubeArmor and recorded in the audit. This is the "inner fence catches the outer fence's bug" pattern. |
| Agent uses a tool that ignores `HTTPS_PROXY` (raw TCP, `/dev/tcp`, `nc`) | The NetworkPolicy blocks the raw egress (the agent pod's egress is default-deny except to the proxy pods + DNS). KubeArmor records the blocked connection. The tool gets a connection timeout (exit 28) or connection refused (exit 7, if the target is a cluster-internal pod on a non-allowed port). |
| DNS resolution for an allowed host fails (the host doesn't exist) | The agent's HTTP client (honouring `HTTPS_PROXY`) sends a CONNECT; the egress proxy resolves the host via kube-dns, gets NXDOMAIN, and returns an error to the client. No tunnel is opened, and a **blocked** audit record is emitted (`verdict: blocked`, `detail: dns=nxdomain`). This is correct behaviour: the host is allowed but doesn't exist. |
| **DNS rebinding:** an allowlisted host's resolution flips to a private/cluster IP between the allowlist check and the dial (an attacker-controlled or rebind-prone name) | The proxy resolves **once**, rejects private/link-local/cluster-CIDR IPs **before** the dial, and dials **the same IP it checked** (no TOCTOU re-resolution). A rebind that lands in a carved-out range is **blocked** and audited (`verdict: blocked`, `detail` records the rejected IP). A rebind that lands in the **public** range is dialled to the new IP — but that is within the allowlist's intent (the hostname is allowed); the `detail` IP makes any rebind visible after the fact. The NetworkPolicy `ipBlock` carve-outs are the backstop for the in-cluster/private cases at the pod level. |
| Egress proxy's egress is abused by a compromised egress proxy pod | The egress proxy runs as UID 65534, read-only rootfs, no privilege escalation, no SA token. Its NetworkPolicy egress is the `ipBlock` with cluster-internal + link-local carve-outs (above), so a compromised proxy **cannot** reach the apiserver, other Loops' model proxies **and their keys**, node/kubelet ports, or `169.254.169.254` — the SSRF pivot is closed at the NetworkPolicy layer. It **cannot** read the model credentials (not mounted), **cannot** write to the agent's workspace (not mounted), and **cannot** exec any binary other than its own (KubeArmor D35 part 2). Its blast radius is public-internet exfiltration of data the agent has already sent through the proxy (the proxy sees the CONNECT/Host/SNI and the resolved IP but not the TLS payload, since it tunnels bytes). This is **less** privileged than the model proxy (no secrets mounted, no cluster-internal reach). |

### Consequences for existing design decisions

- **I41 (`PolicyTranslationLossy`):** the port-loss that produced I41 is
  resolved for network allows by the egress proxy (it enforces host AND
  port). The `PolicyTranslationLossy` condition remains for the KubeArmor
  translation of the **model proxy's** egress (the model endpoint's port is
  still translated to a KubeArmor `matchProtocols` rule, which is
  protocol-name-only). For the agent's network allows, the KubeArmor policy
  does not need to carry the port (the egress proxy does), so the
  `PolicyTranslationLossy` condition is **not set** for network allows once
  the egress proxy is implemented.
- **`NetworkAllowsNotEnforced` condition (D34, I42):** it was only proposed
  in D34's design comment, never implemented. With I42 resolved by the
  egress proxy it is dropped: the I42 slice adds nothing named
  `NetworkAllowsNotEnforced`. Until that slice lands, agent egress stays
  proxy + DNS (fail-closed) and the gap is recorded here, in ADR-0007.
- **D35 part 2 (KubeArmor per-container policies):** the egress proxy pod
  gets its own KubeArmor policy (process = egress proxy binary only,
  network = effective AgentPolicy allows, same as the model proxy's KubeArmor
  policy). This is the inner fence.
- **C6b (KubeArmor engine):** the egress proxy's KubeArmor policy is emitted
  by the same `EmitKubeArmorPolicy` function (the engine is
  agent-agnostic; it takes an `EffectivePolicy` and emits a KubeArmorPolicy).
  The egress proxy's `EffectivePolicy` is the **same** as the agent's (the
  effective AgentPolicy), but the process allows are different (egress proxy
  binary, not agent binaries).
- **Q4 audit:** the egress proxy is a new trusted audit source (alongside the
  model proxy and the eBPF engine). The `source` field value is
  `"egress-proxy"`.
- **Plan slices:** a new slice **I42** (egress proxy pod + Service +
  NetworkPolicy + env vars + owned-and-Ready gate + audit) is added. It sits
  on top of D35 (the gate) and C6b (the KubeArmor engine). The implementation
  is a new `ensureEgressProxy` function in the controller (same shape as
  `ensureProxy` for the model proxy), a new `cmd/egress-proxy/` stand-in (same
  shape as `cmd/proxy-standin/`), a new NetworkPolicy
  (`<loop>-egress-proxy-netpol`), and the D35a-style owned+Ready gate extended
  to the egress proxy.

### What is NOT in scope for this ADR amendment

- The egress proxy's **TLS inspection** (it tunnels bytes, it does not
  terminate TLS). A future ADR can decide whether to add a
  TLS-terminating mode (for plain-HTTP inspection) or keep the tunnel-only
  mode.
- The egress proxy's **connection pooling** and **performance** (a single
  Go `net/http` reverse proxy is sufficient for Phase 1; a load-balanced
  egress proxy is a Phase 2 concern).
- The **shared egress proxy** optimisation (Phase 2, above).
- **DNS-over-HTTPS:** the agent has **no raw egress** (its NetworkPolicy
  allows only the model proxy, the egress proxy, and kube-dns). So DoH
  (HTTPS to e.g. `1.1.1.1:443` or `dns.google:443`) has to go through the
  egress proxy as a CONNECT, and it is **enforced like any other host**:
  blocked unless that DoH host is in the allowlist. It does **not** bypass
  the NetworkPolicy, and it is not caught by KubeArmor `matchDNSQueries`
  (there is no separate DNS channel to match). DoH is only possible to an
  allowlisted DoH host, and is then indistinguishable from other allowed
  HTTPS — a policy-authoring concern, not an enforcement gap.
