# Phase 1 review, round 17: tool proxies, fan-out, multi-tenancy

Design round, 2026-10-01, since tag `review/phase1-r16`. No code findings.
This round turns three capabilities, needed by the use cases discussed with
the owner, into design items. They are judged against D36 (a general,
extensible orchestrator; every capability an extension point with a simple
default).

The motivating use cases:
- dependency/CVE patching across many repos (fan-out);
- infrastructure changes and incident triage (authenticated, scoped tool
  access with no credential in the agent);
- research reports (allowlisted egress with provenance);
- internal "agents as a service" and customer-supplied agents (multi-tenancy).

**Owner direction (2026-10-01):** authenticated tool proxies must support an
**allowlisted egress**, the same model as the I42 egress proxy.

Every item below reuses the I42 machinery. The house rules from R16 apply to
all of them:
- **D39:** any operator-injected hostname must be resolvable under its pod's
  KubeArmor DNS allowlist (`ndots:1`, full names, a pairing test).
- **I43:** same-Loop update specs; gate specs must fail with the gate disabled.
- **I45:** the admission policy covers every coxswain component pod.
- **D38:** the CNI must police pod→host egress (`make verify-cni`).

---

## P1: Design decisions

### D41. Authenticated tool proxies with allowlisted egress

- [x] Owner direction (2026-10-01): tool proxies use allowlisted egress
- [ ] Owner decision: questions below
- [ ] ADR written (generalises ADR-0006's zero-credential rule to tools)
- [ ] Planned as slices

**Where:** a new per-Loop component, alongside the model proxy (D33) and the
egress proxy (I42).

**Problem:** many workflows need authenticated APIs (GitHub, Grafana,
cloud/IaC, ticketing). Today the agent has only the model proxy (credentialed,
single upstream) and the egress proxy (an allowlist, but **no credentials**).
Putting a token in the agent breaks ADR-0006's zero-credential property.

**Design:**
- **A credentialed tool proxy per tool, per Loop.** A pod and Service hold the
  tool's credential; the agent calls the proxy's in-cluster URL and never sees
  the secret. This generalises the model proxy.
- **Spec:** a `tools[]` list on AgentPolicy, or a separate `ToolProxy` kind
  (question 1). Each entry has `name`, `upstream` (a base URL),
  `credentialSecretRef` and **request rules**: allowed HTTP methods plus path
  prefixes. For example, GitHub `GET /repos/acme/*` read-only, and
  `POST /repos/acme/*/pulls` only for a deliver-stage Loop. Anything not matched
  gets a 403, with an audit record.
- **Allowlisted egress, enforced in two layers, as in I42:**
  - **network:** the proxy may connect only to the upstream host:port.
    Enforced by its NetworkPolicy (egress to the upstream plus DNS) and an
    I42f-style KubeArmor policy (process = the proxy binary only;
    `matchDNSQueries` = the upstream host + platform DNS `udp`+`tcp`;
    `ndots:1`). The I42a resolved-IP carve-outs apply: a tool upstream must not
    resolve into the cluster;
  - **application:** the request rules restrict *what* it may do there. Every
    request is audited (method, path, status, policy hash, credential
    redacted).
- **Agent side:** the tool's Service FQDN goes on the agent's DNS allowlist and
  in `NO_PROXY` (D39). The agent NetworkPolicy gains an egress rule to the tool
  proxy's port.
- **Platform plumbing:**
  - a new `component=tool-proxy` label;
  - added to `ProxyComponentSelector` (the I44 pin test must cover it);
  - added to the I45 admission policy's component list;
  - foreign-object gates and drift correction as for the other proxies.

**Questions for the owner:**
1. **Where tools are declared:** on AgentPolicy (one policy object, the
   simplest union semantics) or as a separate `ToolProxy` resource that can be
   reused across Loops and owned by a platform team (recommended if tool
   credentials are managed centrally)?
2. **Granularity:** a proxy per tool per Loop (the strongest isolation,
   matching today), or shared per campaign or tenant (cheaper at scale; see
   D42 #5)?
3. **Built-in tool kinds:** ship a generic HTTP tool proxy as the simple
   default, with typed extensions later (GitHub-aware path templates, cloud SDK
   signers)? Per D36: the default is built in, solid externals plug in.

**Acceptance:**
- the owner has answered the questions; an ADR is written;
- the plan has slices, test-first;
- kind evidence: the agent calls an allowed tool path (2xx) and a disallowed
  path (403, audited); the tool proxy cannot reach any host but its upstream;
- the agent pod contains no tool credential.

### D42. Fan-out: many Loops from one request

- [ ] Owner decision: questions below
- [ ] Planned as slices

**Where:** a new parent resource over Loops; the model and tool proxies; the
controller's concurrency.

**Problem:** use cases like dependency/CVE patching and migrations run the same
workflow across tens or hundreds of repos. Today each Loop is created and
approved by hand, and nothing bounds concurrency, cost or upstream rate limits.

**Design:**
1. **A parent resource** (`Campaign`, or `LoopSet`): a Loop **template** plus
   a **generator** (an explicit list, a label selector over a repo inventory, or
   a matrix). It creates child Loops with owner references and
   **deterministic names** (a hash of the generator entry), so a re-run is
   idempotent.
2. **Flow control:**
   - `maxParallel` and a queue, with priorities;
   - a `failurePolicy` (`FailFast` | `Continue`, with a threshold);
   - retries with backoff for infrastructure failures, not for failed
     evidence gates;
   - an aggregated status (counts per phase, a list of failures).
3. **Gate batching:** a human approves the plan **template** once, for the
   campaign. Each child is then gated on **evidence** (its tests), not on a
   human. The ledger records the campaign approval, and each child links to it.
4. **Shared limits, enforced at the proxies:**
   - model token budgets and rate limits per campaign (the model proxy);
   - upstream API rate limits per credential (a token bucket in the D41 tool
     proxies; e.g. GitHub's limits are per token).
5. **The cost per Loop is the scaling risk; measure it before committing.**
   Today one Loop = an agent pod + 2 proxy pods (+ tool proxies) + 3
   NetworkPolicies + 3 KubeArmorPolicies. At hundreds of Loops that hits:
   - KubeArmor BPF map and policy limits;
   - the cost of evaluating NetworkPolicies;
   - the manager's pod/Service cache (`ProxyComponentSelector`).

   Options: pool the proxies per campaign or tenant (weaker isolation), cap
   Loops per node, or a hybrid (pooled egress proxy, per-Loop model proxy).
   **A load test on kind (and on the Calico profile) is a prerequisite.**
6. **Controller concurrency:** `MaxConcurrentReconciles > 1`. The enforcer is
   already configured once at construction (I44); audit the rest of the
   reconciler for shared mutable state before raising it.
7. **Aggregated delivery:** N child deliverables (PRs) plus one campaign
   summary report (a deliver extension point).

**Questions for the owner:**
1. **Gate batching:** is a single campaign-level plan approval with per-child
   evidence gates acceptable, or must some workflows keep per-child human
   approval?
2. **Isolation vs cost:** may proxies be pooled per campaign or tenant when the
   load test shows the per-Loop cost is prohibitive, or is per-Loop isolation a
   hard requirement?
3. **Generator sources:** which first: an explicit list (simplest), or a repo
   inventory with selectors (needs an inventory extension point)?

**Acceptance:**
- the owner has answered the questions;
- the load-test results are recorded (the max Loops per node before
  KubeArmor/NetworkPolicy/cache limits);
- the plan has slices;
- kind evidence: a 20-child campaign respects `maxParallel`, `FailFast` stops
  scheduling, a re-run is idempotent, and the token budget is enforced at the
  proxy.

### D43. Multi-tenancy

- [ ] Owner decision: questions below
- [ ] Planned as slices

**Where:** namespaces/RBAC, a new cluster-scoped tenant policy, admission,
quotas, scheduling (runtime class, node pools), and the ledger/audit views.

**Problem:** use cases 11 (internal agents-as-a-service) and 12
(customer-supplied agents) need several tenants on one platform, with
guardrails tenants can't loosen, fair resource sharing, and isolation between
tenants. Different tenants warrant different trust levels.

**Design: three tiers, chosen per tenant by trust:**
- **Tier 1, trusted teams (soft tenancy, a namespace per tenant).** Much of
  this works today: AgentPolicy and secrets are namespaced, the proxies are per
  Loop, and I42e rejects in-cluster allows (an agent can't reach another
  tenant's Services). To add:
  - **Tenant policy ceilings:** a cluster-scoped `TenantPolicy` (or
    `ClusterAgentPolicy`) set by the platform team. A tenant's AgentPolicy can
    only **narrow** it; the effective policy is the **intersection**, enforced
    by the controller (authoritative) plus a ValidatingAdmissionPolicy (early
    rejection).
  - **Quotas:** ResourceQuota/LimitRange per tenant namespace; a Loop
    concurrency cap; token budgets per tenant at the model proxy (shares the
    D42 #4 mechanism).
  - **Tenant RBAC templates:** a tenant may create Loops and AgentPolicies but
    may **not** `exec` into, add ephemeral containers to (I45), or
    **update/patch/relabel** agent and proxy pods (the I45 relabel note). A
    default-deny NetworkPolicy per tenant namespace.
  - **Per-tenant audit and ledger views;** credentials per tenant (namespaced
    secrets, already).
- **Tier 2, semi-trusted (hard isolation on a shared cluster):**
  - dedicated **node pools** per tenant (taints/tolerations and a node selector
    on the agent and proxy pods);
  - **sandboxed runtimes** through `runtimeClassName` (gVisor, which
    agent-sandbox supports, or Kata), so a kernel exploit doesn't cross
    tenants. **Interaction to decide:** KubeArmor's BPF-LSM fence doesn't apply
    inside a gVisor sandbox, so pick per tier which layer is primary;
  - D38's CNI requirement is **mandatory** (verified by `make verify-cni` and
    the planned operator self-test).
- **Tier 3, untrusted (a cluster per tenant):** a cluster or virtual cluster
  per tenant, each running a coxswain operator; a thin control plane above
  aggregates workflows, approvals and the ledger. The highest cost, and the
  simplest security argument.

**Recommendation:** build **Tier 1 now**: ceilings, quotas and RBAC templates
are mostly policy and admission work on what exists. Make the agent and proxy
pod specs **runtime-class and node-pool aware** (configurable per tenant) so
Tiers 2 and 3 are configuration, not a redesign.

**Questions for the owner:**
1. **Target tier for the first multi-tenant deployment:** Tier 1 (internal
   teams), or does a customer-facing (Tier 2/3) use case come first?
2. **Ceiling semantics:** strict intersection (a tenant can never exceed the
   ceiling), or a ceiling plus platform-approved exceptions (an exception
   object with its own approval in the ledger)?
3. **gVisor vs KubeArmor in Tier 2:** sandboxed runtime as the primary fence
   (KubeArmor not applicable inside), or KubeArmor on runc with dedicated nodes?

**Acceptance:**
- the owner has answered the questions;
- the plan has slices for Tier 1;
- kind evidence:
  - a tenant AgentPolicy that exceeds its TenantPolicy ceiling is rejected at
    admission and narrowed by the controller;
  - a tenant's Loop can't reach another tenant's namespace (in-cluster allow
    rejected, NetworkPolicy);
  - a tenant user can't exec into, debug or relabel agent pods (RBAC + I45);
  - a quota-exceeding Loop is held, not run;
- the pod spec has `runtimeClassName` / node-selector support behind config.

---

## Dependencies and order

- **D41 → D42:** fan-out's shared API rate limits live in the tool proxies.
- **D43 Tier 1 ↔ D42:** per-tenant quotas and token budgets reuse the
  campaign budget mechanism; build the shared "budget at the proxy" component
  once.
- **D38 next (the operator CNI self-test)** is a prerequisite for any
  multi-tenant claim: tenants can't verify the cluster's CNI themselves.
- **Suggested order:**
  1. D41 (unblocks the infra, triage and research workflows);
  2. the D42 load test (it decides the D42/D43 isolation-vs-cost questions);
  3. D42 campaign + flow control;
  4. D43 Tier 1.
