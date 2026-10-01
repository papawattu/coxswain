# ADR-0006: Coxswain isolates *any* agent; the agent holds no credentials

**Status:** Proposed (revised D26, round 6; supersedes round 5's D26 recommendation)
**Date:** 2026-09-27
**Supersedes:** Round 5's D26 recommendation (grow `runner/` first as the
flagship agent). Round 5 framed the choice as "which agent do we build"; the
owner's direction reframes it.

## Context

Round 5's D26 treated the agent as the product to be built: the
recommendation was to grow the `runner/` tool loop into the default runner
image first, with other agents (Claude Code, pi, Codex, OpenHands) as
adapters added later.

The owner's direction — **"the whole point is the isolation"** — reframes
the value proposition. Coxswain's product is not any particular coding
agent. It is the guarantee that **any** coding agent (Claude Code, pi, Codex,
OpenHands, or our own `runner/`) can be run **untrusted** inside a sandbox,
and its output only counts if the operator's own evidence
(ADR-0005: the verify Job's kubelet-recorded exit codes) says so. The agent
is the *payload*; the isolation and verification are the *product*.

This has a concrete consequence that round 5's plan got backwards: round 5
would have mounted the model API key and a git push token into the sandbox so
the agent could call the model and push its work. But both of those are then
owned by whatever the model does with a shell — a prompt-injected agent could
exfiltrate the push token or read the model key. If the agent holds no
credentials at all, there is nothing to steal, and the operator's
evidence-based gate is the only thing that matters.

## Decision

Coxswain makes the sandbox a **zero-credential, deny-by-default** boundary.
The agent is agent-agnostic and untrusted; every capability it needs is
either a local file or a localhost proxy that holds the real credentials.
Recorded for the plan:

1. **Agent-agnostic contract.** A *runner* is **any image** that:
   - reads its instructions (goal, iteration, previous verify output, memory,
     `.coxswain/desired-phase`) from files under `/workspace/.coxswain/`;
   - edits files under `/workspace`;
   - makes git commits **locally** (no remote, no credentials); and
   - writes `/workspace/.coxswain/result.json` (the claims-only contract of
     ADR-0004).

   Nothing else. Adapters for existing agents are thin entrypoint scripts
   that map the contract to the agent's CLI. `runner/` (the OpenAI-compatible
   tool loop) becomes the **reference/conformance** agent — the one the tests
   drive with the fake model — not the flagship.

2. **Zero credentials in the agent container.**
   - **Model access via a sidecar proxy** in the sandbox pod. The agent talks
     to `http://localhost:<port>` (`COX_MODEL_BASE_URL`); the proxy container
     holds the real model key (mounted **only** into the proxy, never the
     agent), injects the auth header, and forwards only to the configured
     endpoint. The proxy also **meters tokens** — this is the Phase 2
     metering sidecar (CONTEXT.md), built now as the credential boundary.
   - **No push token in the agent.** The agent commits locally. A **trusted
     publish step outside the agent's control** (an operator-created "publish"
     Job, or a sidecar that shares only the workspace volume and reads the
     push token) pushes the Loop branch after each iteration. This is also
     where D11's `verifiedCommit` gets pinned — the operator publishes and
     pins the *same* SHA, so the agent cannot force-push after verify.
   - `automountServiceAccountToken: false` on the sandbox pod; **no
     Kubernetes credentials at all** in the agent container.

3. **Egress deny-by-default.** A NetworkPolicy on the sandbox allows egress
   **only** from the proxy container to the model endpoint (plus DNS). The
   agent container reaches nothing but `localhost`. **Package installs are
   the hard case** — see "Open owner decision 2". General internet egress is
   off by default.

4. **Pod hardening, on by default.** `runAsNonRoot`, drop all capabilities,
   `allowPrivilegeEscalation: false`, seccomp `RuntimeDefault`, read-only root
   filesystem with writable `/workspace` + scratch, CPU/memory limits.
   **`runtimeClassName` (gVisor/kata) is NOT a default (amended by ADR-0007
   Q3):** gVisor intercepts syscalls in user space, so a host eBPF engine does
   not see or enforce inside a gVisor sandbox — with eBPF chosen as the
   enforcement layer (ADR-0007), gVisor is an **opt-in** where the engine
   supports it, and enforcement is then the runtime's, not eBPF's.
   agent-sandbox's `SandboxBlueprint` is the natural place to carry this.

5. **Loop API.** `spec.agent: { image, model, endpointSecretRef, env? }`.
   `endpointSecretRef` holds the base URL + API key and is mounted into the
   **proxy**, never the agent. Cluster defaults come from a
   `coxswain-agent-defaults` ConfigMap so the README sample stays short.
   (The `egressAllow?` field was dropped — **network allowlists live in
   `AgentPolicy`, not `spec.agent`, per ADR-0007 Q6.**)

6. **Tests that prove isolation, not just behavior.**
   - envtest asserts the built Sandbox pod spec has **no token automount, no
     secret volume in the agent container**, the hardening fields set, and the
     NetworkPolicy present;
   - an e2e runs an **"evil agent"** image that tries to read a secret, reach
     the API server, `curl` the internet, and push to the remote — **each
     attempt must fail**, and the attempts must not affect the Loop's
     evidence (the verify Job still sees the committed SHA, not the agent's
     in-flight tampering).

## Resequence (see TDD-PLAN-PHASE1)

After D27, the isolation slices come **before B3**:

1. sandbox pod hardening (item 4) + the `spec.agent` CRD fields (item 5);
2. the **model proxy sidecar** (item 2, credential boundary + metering);
3. the **NetworkPolicy** (item 3, deny-by-default egress);
4. the **publish step** (item 2, trusted push + `verifiedCommit` pin);
5. the **evil-agent e2e** (item 6).

Then the reference `runner/` A1–A4 (now driving the conformance agent through
the proxy, with `COX_MODEL_BASE_URL` pointed at `localhost`), and then B3.
The isolation guarantees are what make B2/B3's evidence meaningful; building
verify before isolation would verify an agent that still holds the keys.

## Open owner decisions (NOT picked by the builder)

Per the review, two choices were the owner's. Status at round 8:

1. **First real agent to adapt** — **STILL OPEN.** Which existing agent gets an
   adapter image first, so a real (non-reference) agent can run end to end?
   Candidates: Claude Code, pi, Codex, OpenHands. (The reference `runner/` is
   built either way as the conformance agent.) Q1 (ADR-0007) only fixes the
   agnostic layer; it does not pick the first agent.

2. ~~**Egress policy for dependency installs**~~ — **RESOLVED by ADR-0007 Q6.**
   The owner decided network allowlists live in `AgentPolicy` (allowed
   hosts/CIDRs + ports), default-deny with additive allows, enforced in two
   layers (a generated `NetworkPolicy` + the eBPF engine's network rules). A
   Loop that needs `go mod download` gets it by a policy that allows the
   module proxy; nothing is open by default. This supersedes the three
   candidates listed here.

## Consequences

- The sandbox pod grows from one container to at least two (agent + model
  proxy); the verify Job (ADR-0005) is unchanged — it still runs the agent's
  committed code in a fresh isolated pod.
- The publish step becomes part of the operator's per-iteration sequence
  (commit → publish → pin `verifiedCommit` → create verify Job), which is
  where D11/D27's commit binding is enforced in production.
- `runner/` is re-scoped from "the agent" to "the reference/conformance
  agent"; its tests keep using the fake model, but its runtime config points
  `COX_MODEL_BASE_URL` at the local proxy rather than a remote endpoint.
- The Phase 2 metering sidecar is pulled forward into Phase 1 as the
  credential boundary (CONTEXT.md budgets/metering notes updated).
- The `spec.agent` CRD fields and the `coxswain-agent-defaults` ConfigMap are
  new Phase 1 surfaces (the README sample gets an `agent:` block).
- **CNI requirement (D38, round 16):** the item-3 NetworkPolicy is an
  allowlist, but a NetworkPolicy spec leaves host-network destinations to the
  CNI's implementation. kind's default CNI (kindnet) does **not** police
  pod → host-network egress: on `kind-coxswain-dev` the agent pod can reach
  the apiserver service IP (10.96.0.1:443), the node's :6443, and the
  kubelet's :10250, while pod-IP and external-IP egress is denied (review
  docs/REVIEW-PHASE1-R16.md, D38). The mitigations above (no token mounted,
  403 for anonymous requests, 401 on the kubelet) make the residual
  attack surface credential-gated, but the design assumed the control plane
  was unreachable. **Production requires a CNI that polices
  pod-to-host-network egress** — stated as a property, not a vendor list.
  Verified with: Calico v3.30.1 (`make d38-cni-e2e`, `make verify-cni`).
  To be confirmed (run `make verify-cni`): Cilium, GKE Dataplane V2, EKS
  VPC CNI (with network policy), AKS. kindnet is **dev-only**.
  `make verify-cni` is the preflight check the install docs point at:
  it creates a temp namespace with an agent-shaped NetworkPolicy and a
  probe pod, then TCP-connects to the apiserver service IP, each node's
  :6443/:10250, a kube-dns pod IP, and 1.1.1.1:443; every target must be
  BLOCKED for PASS. It works on any cluster where kubectl is pointed (no
  coxswain install required) and always cleans up. `make kind-calico-up`
  builds a second kind cluster whose CNI is Calico (kindnet disabled), and
  `make d38-cni-e2e` asserts the apiserver/kubelet endpoints are blocked
  from an agent-labelled pod on it (a second BPF-LSM agent is deliberately
  NOT installed there — ADR-0007 F2; the KubeArmor-dependent checks are
  printed as SKIPPED).
- **Residual risk on non-enforcing CNIs (D38 Q2, 2026-10-01):** on a CNI
  that does not police pod → host-network egress, the agent pod **can**
  reach the apiserver service IP, the node's :6443, and the kubelet's
  :10250. The remaining mitigations are: no SA token mounted
  (`automountServiceAccountToken: false`), the apiserver returns 403 for
  anonymous requests (except public-info endpoints), and the kubelet
  requires authentication (401 without credentials). The owner accepts this
  residual risk on clusters where the CNI has been verified to police
  pod → host-network egress, and requires the cluster-hardening checklist
  below on clusters where it has not.
  **Cluster-hardening checklist** (recommended for all clusters; required
  when the CNI does not police pod → host-network egress):
  1. Disable or restrict apiserver anonymous authentication
     (`--anonymous-auth=false`, or an `AuthenticationConfiguration` with
     `anonymous: deny`). Without this, any pod that reaches the apiserver can
     make unauthenticated requests to public-info endpoints.
  2. Turn off the kubelet read-only port (`readOnlyPort: 0`). The read-only
     port (10255) serves `/metrics` and some stats without authentication.
  3. Enable kubelet authentication and authorization webhooks
     (`authenticationTokenWebhook: true`, `authorizationMode: Webhook`, not
     `AlwaysAllow`). This ensures a pod that reaches the kubelet's
     authenticated port cannot act without valid credentials and an
     authorized RBAC binding.
  4. No service-account token automount for agents. Coxswain already sets
     `automountServiceAccountToken: false` on the agent pod spec; verify that
     no other mechanism (a mutating webhook, a ClusterRoleBinding, etc.)
     grants the agent a token.
  `make verify-cni` now includes a **hardening section** (WARN-only, never
  changes the CNI PASS/FAIL verdict) that checks items 2 and 1 from a second
  probe pod with no NetworkPolicy: whether the kubelet read-only port (10255)
  is open (WARN if reachable), and what the apiserver returns for an
  unauthenticated request (401 = anonymous disabled, 403 = anonymous enabled
  but unauthorized, 200 = WARN). These checks measure the cluster, not the
  CNI, and never affect the exit code.
  The next step (D38 next, planned in TDD-PLAN-PHASE1) is an operator-side
  CNI self-test that runs the verify-cni probe at startup and periodically,
  sets a cluster-level condition, and holds Loops Suspended (D30-style) when
  pod-to-host-network egress is not policed.
- **Agent image stand-in (I37, round 10):** when `spec.agent.image` is omitted the
  sandbox runs `docker.io/library/golang:1.26` + `sleep infinity` as a Phase 0
  stand-in. It is hardened (zero credentials, no token automount, read-only
  rootfs, limits) but it is *not* an agent, and it is a general rootless image
  that contradicts the "agent-agnostic, minimal" default the ADR describes. The
  reference runner image lands with the A-slices; until then the stand-in default
  is documented in the README's status line, and `spec.agent.image` should be set
  explicitly by any real run. Revisit once the runner image exists: either make
  the runner image the default (option a) or require `spec.agent.image` via CEL
  (option b).
