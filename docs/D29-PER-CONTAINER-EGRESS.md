# D29 proposal — per-container egress: agent ↔ proxy split

**Status: PROPOSED (for reviewer evaluation; supersedes ADR-0007's round-9 D29
assumption that "the engine policy scopes by container in the pod (KubeArmor
can do this)").**

## The problem

ADR-0006 item 2 puts the model key in a **proxy sidecar** that the agent
reaches on `localhost:8080` (`COX_MODEL_BASE_URL`); the proxy holds the real
key, injects auth, and meters tokens. ADR-0007 (round 9, D29) then said the
**eBPF layer** enforces *per-container* egress — the **agent container** may
egress only to `localhost` (the proxy) plus the user's `AgentPolicy` allows,
and the **proxy container** only to the model endpoint — so an evil agent that
dials the model endpoint **directly** (bypassing the proxy) is blocked by eBPF,
recorded in the activity audit, and absent from the proxy's metering.

That design rests on an assumption the R15 review (PR #7/PR #8 P2 #4) and the
C6b e2e both invalidated. Two independent facts make per-container egress **not
expressible with a pod-scoped mechanism**:

1. **A Kubernetes `NetworkPolicy` selects pods, not containers.** One NetworkPolicy
   per sandbox pod cannot allow the model endpoint for the proxy container while
   denying it for the agent container — they share one pod, one NetworkPolicy
   identity. Allowing the endpoint for the pod allows it for the agent too.
2. **KubeArmor's `KubeArmorPolicy` selector is pod-level only.** The vendored
   v1.7.5 CRD `selector` has exactly `matchLabels`/`matchExpressions` (verified:
   no `container`/`containerName`/`matchContainers` field anywhere in the selector,
   and no container field in the `process`/`network` blocks). A KubeArmorPolicy
   therefore also cannot scope egress to one container of a pod.
3. **An agent-sandbox `Sandbox` is exactly one pod.** `SandboxBlueprint.PodTemplate`
   is a single `corev1.PodSpec`, and the agent-sandbox controller **refuses to
   reconcile a Sandbox that owns more than one Pod**
   (`multipleSandboxPodsError`: "multiple Pods (N) are controlled by this
   Sandbox; refusing to choose or create a Pod"). So the proxy **must** be a
   second container of the same pod — it cannot be a separate pod *through the
   agent-sandbox API*.

Conclusion: with the agent and proxy in one pod, **no pod-scoped egress mechanism
(NetworkPolicy or KubeArmorPolicy) can split the two containers' egress.** The
round-9 D29 ("KubeArmor can do per-container egress") is not achievable with the
KubeArmor API we have. The mechanism must come from a **process/credential-boundary
argument**, not a network selector.

## Candidate mechanisms

### (a) The proxy is the *only* path to the model endpoint; the agent never
### holds the key and the endpoint is unreachable without it. (Recommended.)

The split is enforced by **credentials + a loopback-only proxy**, not by a
per-container network rule:

- The model key is mounted **only into the proxy container** (already C2: the
  `model-creds` volume is in the proxy, not the agent; the agent has
  `COX_MODEL_BASE_URL=http://localhost:8080`). The agent **cannot forge** a
  request to the real endpoint because it does not hold the key.
- The **proxy binds to the pod's loopback interface only** (`127.0.0.1:8080`),
  so it is reachable **only from within the pod** — i.e. only by the agent
  container (and the proxy itself). It is **not** reachable from outside the pod.
- The **agent's egress** is policed at the pod level (NetworkPolicy + the
  KubeArmorPolicy emitted per C6b) to allow: `localhost` (the proxy, loopback)
  plus the user's `AgentPolicy` allows — and **deny the model endpoint by name**
  so the agent cannot dial it directly even *with* a stolen key (defense in
  depth; see the honest caveat below on key theft).
- The **proxy's egress** to the model endpoint is platform infrastructure
  (allowed by the operator, ADR-0007 item 2), **not** by the user's policy.

The C5 evil-agent case becomes: the agent tries to reach the model endpoint
**directly** (e.g. `curl https://model-endpoint` instead of
`http://localhost:8080`). This is **blocked** because the agent's effective
policy denies that endpoint (the pod-level deny is enough: the agent container
cannot open a socket to the endpoint's DNS name under the default-deny policy),
and is **recorded** in the activity audit. It is also **absent from the proxy's
metering** (the proxy only sees requests that reach it on loopback).

**Why this works where a per-container rule would not:** the agent *could* in
principle open a socket to the model endpoint — the pod-level NetworkPolicy
cannot stop it from *trying*. But the agent **cannot authenticate** (no key),
so a direct call is useless even if it connected. The deny-by-name is the
defense-in-depth layer: it makes the direct call fail *and* auditable, rather
than merely keyless. The **metering/audit guarantee** (the thing D29 exists to
protect) is that **only the proxy holds the key and only the proxy's
loopback-bound listener is the ingress to the model**, so any authenticated
model traffic *must* pass through the proxy — hence metered.

**Honest caveat (key theft):** if the agent could *read* the proxy's key (e.g.
same-UID `/proc/<pid>/environ`, a shared writable volume, or a runc escape),
the key boundary is broken and a direct call would succeed. C2 already
mitigates this: the proxy runs as **UID 65533** (distinct from the agent's
65532), the key is a **read-only file mount** (not env), and
`shareProcessNamespace` is **false** (so the agent cannot
`cat /proc/<proxy-pid>/environ`). The D29 mechanism is the *network* boundary;
the C2 hardening is the *credential* boundary. Together they give the split.
The C5 e2e asserts the direct endpoint call is **blocked or keyless**, not just
keyless.

### (b) Per-container netns (a second network namespace for the proxy container).

Put the proxy container in its own netns (via a sidecar with
`nsenter`/CNI, or a per-container CNI) so the agent pod and the proxy netns are
separate egress identities. A NetworkPolicy can then select each netns.
**Rejected:** non-standard, requires per-container CNI or `nsenter` plumbing the
agent-sandbox API does not expose (it owns the pod spec), fragile across
container runtimes, and far more moving parts than (a). The credential boundary
(a) already provides the guarantee; (b) adds a network boundary that is hard to
make reproducible.

### (c) Proxy in a separate pod (a second `Sandbox`, or a raw pod, the operator
### owns).

The proxy runs as its **own pod** with its own Service; the agent pod's
NetworkPolicy allows only the proxy Service, and the proxy pod's NetworkPolicy
allows only the model endpoint — each pod is a clean NetworkPolicy selector
boundary, so the split is a pure NetworkPolicy (no eBPF needed for the split).
**Rejected for now:** an agent-sandbox `Sandbox` is exactly one pod (the
controller errors on >1 owned pods), so the operator would have to manage a
**second, non-agent-sandbox pod** for the proxy (a plain `Pod` + `Service`, or
a second `Sandbox` not carrying the agent). That splits the agent and proxy
lifecycle (crash-recovery, the D30 gate's single-pod suspension, the
`coxswain.io/loop` label selector, checkpointing) across two objects the
operator must keep in lockstep. It is the *cleanest* network split but the
*most* lifecycle coupling. **Revisit** if (a) proves insufficient — it is the
fallback if the credential boundary is ever found breakable in practice.

## Recommendation

**(a)** — the credential + loopback-boundary split, with a pod-level
deny-by-name on the model endpoint as defense in depth. It reuses the C2 wiring
(key in proxy only, loopback proxy, distinct UID, no shared PID ns) and needs
only the operator to **deny the model endpoint in the agent's effective policy**
(the KubeArmorPolicy/NetworkPolicy emitted per C6b already targets the pod; add
the endpoint to the **deny** set for the agent's container scope, or equivalently
ensure the agent's policy allows only `localhost` + user allows and nothing
else — which, by default-deny, already excludes the endpoint).

The **NetworkPolicy (C3)** generated from the policy is the coarse outer fence
(pod-level: the agent pod may reach `localhost` + user allows; the platform
allows the proxy's model-endpoint egress as infrastructure). The **eBPF layer
(C6b emitter)** is the source of the audit records. Neither needs to scope
*per-container*; the split is the credential boundary.

**Amendments this makes to ADR-0007 / ADR-0006:**

- ADR-0007 D29 (round 9): the sentence "The engine policy scopes by **container**
  in the pod (KubeArmor can do this)" is **wrong** — KubeArmor's selector is
  pod-level. Replace with: *per-container egress is enforced by the credential +
  loopback boundary (the proxy holds the key, binds loopback only, runs as a
  distinct UID, and shares no PID ns with the agent), not by a per-container
  network rule; the NetworkPolicy and the emitted KubeArmorPolicy are
  pod-level fences, with the model endpoint in the agent's deny set as defense
  in depth.*
- ADR-0006 item 2: add that the proxy **binds loopback only** and the agent
  **cannot** hold the key (already implied; make it explicit as the D29
  mechanism).
- C5 evil-agent e2e: the "direct model-endpoint call" case asserts the call is
  **blocked (or keyless) and recorded in the activity audit and absent from the
  proxy metering** — not that a per-container eBPF rule fired.

## Acceptance (for C3 + C5)

- **C3 (NetworkPolicy):** the generated NetworkPolicy for the sandbox pod allows
  egress to `localhost` (loopback) + the user's `AgentPolicy` allows, and the
  platform allows the proxy's model-endpoint egress. The agent pod **cannot**
  reach the model endpoint's DNS name under the default-deny (asserted in the
  C5 e2e, not the NetworkPolicy unit test, since the NetworkPolicy is
  pod-level and cannot distinguish the containers).
- **C5 (evil-agent e2e, the D29 proof):** an evil agent image, with the real
  proxy running, attempts a **direct** model-endpoint call
  (`https://model-endpoint/v1/...`, not `http://localhost:8080`). Assert:
  1. the call is **blocked or returns no authenticated model response** (the
     agent has no key; the endpoint is in its deny set);
  2. the attempt is **recorded in the activity audit** (the eBPF network rule
     for the endpoint deny produces an audit record);
  3. the attempt is **absent from the proxy's metering** (the proxy's loopback
     listener never saw it);
  4. the same request **through the proxy** (`http://localhost:8080/v1/...`)
     **succeeds and is metered** (the positive control proving the proxy is the
     only working path).
- **C6b emitter:** the agent's effective policy denies the model endpoint by
  name (defense in depth) — asserted by the emitter unit test (the endpoint's
  DNS name is in the **deny** set, not the allow set, for the agent scope).

## What this does **not** change

- The agent and proxy stay in **one pod** (the agent-sandbox invariant).
- The D30 fail-closed gate, the `--allow-unenforced` escape hatch, and the I32
  relay evidence source are unchanged.
- The credential boundary (C2: key in proxy only, loopback proxy, UID 65533,
  no shared PID ns) is unchanged — D29 *relies on* it and makes it the named
  mechanism.

## Open question for the owner

Is **(a)** (credential + loopback boundary, pod-level deny-by-name as
defense-in-depth) acceptable as the D29 mechanism, or do you want **(c)** (proxy
as a separate pod) as the primary despite the lifecycle coupling? (a) reuses the
C2 work and is reproducible; (c) is the cleaner network split but splits the
agent/proxy lifecycle across two objects. My recommendation is **(a)**, with
**(c)** recorded as the fallback.
