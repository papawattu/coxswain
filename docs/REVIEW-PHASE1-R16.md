# Phase 1 review, round 16: what the I42 kind runs found

Review round, 2026-09-29, since tag `review/phase1-r15`. It covers the I42
egress-proxy series merged since then: I42e (#21), I42b (#22), I42c (#24)
and I42d (#25). Code findings were handled on those PRs. This round records
the **cross-cutting** findings: things no single slice owns, owner decisions,
and process changes. Every item below was found on `kind-coxswain-dev`, not in
envtest. That is itself the main lesson (see I43).

---

## P1: Owner decisions

### D38. The agent can reach the control plane on CNIs that don't police pod→host-network egress

- [ ] Owner decision: production CNI requirement (question 1 below)
- [ ] Owner decision: CNI-independent defence in depth (question 2 below)
- [ ] Planned as slices

**Where:** the per-Loop agent NetworkPolicy (D34, I42c) on kind's default CNI,
kindnet.

**Problem:** the agent NetworkPolicy is an allowlist (model proxy, egress
proxy, kube-dns). On `kind-coxswain-dev` it **is** enforced for pod IPs and
external IPs, but **not** for node / host-network destinations. From a pod
carrying the agent labels (PR #24 review, image `sha256:69d47a4f…`):

```
apiserver svc 10.96.0.1:443          -> 200   (/version)   NOT blocked
apiserver node 172.21.0.2:6443       -> 200                NOT blocked
kubelet node 172.21.0.2:10250        -> 401                NOT blocked (auth required)
controller pod 10.244.0.147:8443     -> timeout            blocked
coredns pod 10.244.0.5:9153/:8080    -> timeout            blocked
external 1.1.1.1:443                 -> timeout            blocked
```

The NetworkPolicy spec leaves host-network traffic to the implementation.
kindnet doesn't police pod→host egress. The service IP 10.96.0.1 DNATs to
the node IP, so it gets through too.

**Mitigations in place today:**
- the agent pod has `automountServiceAccountToken: false` and no token mounted;
- anonymous requests to the apiserver get 403, except public-info endpoints (`/version`);
- the kubelet requires authentication (401).

So the agent can *reach* the control plane but can't act on it without
credentials. Still, it's an unauthenticated attack surface that the design
assumed was closed.

**Questions for the owner:**
1. **Production CNI requirement.** State in the install docs and ADR-0006 that
   production needs a CNI that polices pod→host-network egress (Calico and
   Cilium both do), and treat kindnet as dev-only? Should `make deploy-dev`
   warn when it detects kindnet?
2. **Defence in depth that doesn't depend on the CNI.** Options:
   - (a) a KubeArmor network rule on the agent that denies the node CIDR and
     apiserver endpoints (KubeArmor network rules are protocol-level today, so
     this may need `matchIPs`-style support or an extension);
   - (b) accept the residual risk given the mitigations above, and document it;
   - (c) run the kind e2e on an enforcing CNI (kind with Calico) so the
     property is actually tested.

   The reviewer suggests (c) plus documenting (b) now, and (a) if KubeArmor
   supports it.

**Fix direction:** add the CNI requirement to the docs/ADR. Add a kind profile
with an enforcing CNI for the I42 e2e. The full I42 e2e records kindnet's
host-network gap as a **known limitation**, not a pass.

**Acceptance:**
- the owner has decided questions 1 and 2;
- the docs state the CNI requirement;
- the I42 e2e has a documented known-limitation line for kindnet, or passes
  the apiserver/kubelet-blocked checks on an enforcing-CNI profile.

### D39. Every operator-injected hostname must resolve under the agent's KubeArmor DNS allowlist

- [x] Fixed for the model proxy and egress proxy in I42d (#25, `bcffd86`)
- [ ] Rule adopted for future slices (I42f and later); owner confirmation requested

**Where:** `policy.Translate` (the agent KubeArmorPolicy `matchDNSQueries`),
the agent pod's DNS config, and every URL the operator puts in the agent's
env.

**Problem:** two separate bugs, both invisible to envtest (it has no
KubeArmor), both found on kind with KubeArmor BPF-LSM enforcing:

1. The egress proxy's Service FQDN was never on the agent's DNS allowlist,
   so the agent couldn't resolve its own `HTTPS_PROXY` host.
2. **Pre-existing on main since D33 × C6b:** with the default `ndots:5`, the
   resolver first queries `<name>.<search-suffix>`. That isn't on the
   allowlist, so KubeArmor denies it with EPERM (not NXDOMAIN) and the whole
   lookup aborts. The agent couldn't resolve **the model proxy** from
   `COX_MODEL_BASE_URL` either, so model calls have never worked under
   KubeArmor enforcement.

**Fix (done in #25):**
- the agent pod gets `dnsConfig ndots:1`;
- `COX_MODEL_BASE_URL` and the proxy URLs use the full
  `<svc>.<ns>.svc.cluster.local` form;
- the egress proxy FQDN is on the allowlist iff there are network allows;
- a test pins that the URL hosts the agent is given are the hosts its
  allowlist permits, built from `proxyServiceName`/`egressProxyServiceName`,
  not literals.

**Rule for future slices (owner to confirm):** any hostname the operator injects
into a KubeArmor-governed pod (agent, and the proxies once I42f lands) must:
- use the full cluster-local form;
- be on that pod's DNS allowlist, with a pairing test;
- be covered by a kind check with KubeArmor enforcing.

The full I42 e2e must run with enforcement **on**. Dev runs with
`--allow-unenforced` (D30) skip exactly this class of bug.

**Acceptance:**
- the rule is adopted (owner);
- I42f's proxy policies follow it (checked in I42f review);
- the full I42 e2e asserts model and proxied calls succeed with KubeArmor enforcing.

### D40. Marking PRs ready: the builder's token can't do it

- [ ] Owner decision

**Problem:** pi reported that its token can't mark a draft PR ready
(`gh pr ready`). On #25 the reviewer marked it ready. That left the PR title
as "(DRAFT)", which leaked into the squash subject on `main` (`bcffd86 I42d:
… (DRAFT) (#25)`). `main`'s history was not rewritten.

**Question for the owner:** grant the builder token the scope to mark PRs ready,
or keep the reviewer doing it (and fixing the title before merge)?

**Acceptance:** decided; AGENTS.md updated if the flow changes.

---

## P2

### I43. Envtests only ever created objects, so update-path bugs survived for weeks

- [x] Fixed (process) — PR #32 (builder: 2f558ef, 64f69b1, 6254230, round 3): AGENTS.md "Test norms (R16 I43)" added (rule a: same-Loop update specs; rule b: gate specs must fail when disabled, verified by mutation); 6 backfilled same-Loop specs (d33 proxy Service drift correction, d34 agent-netpol same-Loop update, i42f model-proxy KAPT drift correction, agent KAPT same-Loop update, PolicyValid flip, **proxy-netpol drift correction** — R3: the R2 audit row credited i42c spec 6, which updates the AGENT netpol, not the proxy netpol; the new spec tampers the model egress rule and deletes the netpol, asserting the controller restores/recreates it); 7-gate mutation table (all 7 gates verified FAIL-when-disabled in scratch copies). 124/124 specs pass, make lint clean.

**Where:** `internal/controller/*_test.go`, the review checklist.

**Problem:** `createOrUpdateNP` used a no-op mutate from D34 until I42c. Every
existing NetworkPolicy was frozen at creation (allows added later, CIDR
changes, `modelEndpoint` edits never applied). No spec re-reconciled the same
Loop after a change. Several other I42 findings had the same shape:
- a test that passed without the gate it claimed to test;
- a selector test checking hand-copied labels;
- a drift test that asserted nothing when the pod was gone.

The reviewer now mutation-tests key gates on each PR.

**Fix:**
- Make it a norm: every reconciled object gets at least one **same-Loop** spec
  that changes an input and re-reconciles.
- Every gate spec must fail when the gate is removed. The reviewer checks this
  by mutation; builders should too.
- Add both to AGENTS.md "After Making Changes".

**Acceptance:** the AGENTS.md norm is added, and I42f and later slices show same-Loop update specs.

### I45. Ephemeral containers (`kubectl debug`) bypass the KubeArmor fence

- [ ] Fixed

**Where:** every KubeArmor-fenced pod (agent, egress proxy, model proxy), and
namespace RBAC for Loop namespaces.

**Problem:** found while verifying I42f on kind (PR #28 review). In a pod
selected by the egress-proxy policy, exec of a non-allowlisted binary is
denied (`/bin/busybox: Permission denied`, exit 126). But a busybox
**ephemeral container** added to `i42d-loop-egress-proxy` with `kubectl debug`
ran `busybox true` without being blocked: KubeArmor didn't police the
ephemeral container. Anyone with `pods/ephemeralcontainers` (patch) in a Loop
namespace can get an unfenced process inside a proxy or agent pod, sharing its
network namespace and the proxy's view of traffic.

**Fix:**
- Document that `pods/ephemeralcontainers` must be denied to everyone except
  cluster admins in Loop namespaces.
- Consider a ValidatingAdmissionPolicy that rejects ephemeral containers on
  pods carrying coxswain component labels.
- Add the case to the I42 e2e as a known limitation until that lands.

**Acceptance:**
- the RBAC guidance is documented;
- the admission policy exists (or the owner accepts the documented risk);
- the e2e prints the limitation.

---

## P3

### I44. Small follow-ups from the I42 series

- [x] Fixed in b3ffad9 (#30)

1. `internal/engine/kubearmor_enforcer.go` builds the egress proxy FQDN from
   a `"-egress-proxy."` literal. The #25 envtest pins it against
   `egressProxyServiceName`, but the caller should pass the name in.
2. The cluster domain is hard-coded to `cluster.local` (`proxyServiceURL`,
   `egressProxyServiceURL`). Make it a reconciler field (default
   `cluster.local`) if a non-default cluster domain ever matters.
3. The controller's label functions still use `"model-proxy"` /
   `"egress-proxy"` string literals, not `policy.ComponentProxyLabel` /
   `ComponentEgressProxyLabel`. The #22 selector test catches drift; using the
   constants removes it.
4. On kindnet, the I42c kind evidence first claimed "kindnet doesn't enforce
   egress netpols for cluster-internal IPs". That's wrong: it's host-network
   destinations only (D38). Future evidence should state which destination
   classes were tested.

**Acceptance:** items 1–3 are done in a small cleanup PR (a test-only or refactor
change, with no behaviour change).
