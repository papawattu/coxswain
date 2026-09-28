# Phase 1 review, round 14 — a dropped port must be visible

Design finding from the PR #8 (C6b) review, 2026-09-27, since tag
`review/phase1-r13`. It moved here from PR #8's port-stripping thread because
the fix belongs to D33/D34, not to a C6b line change.

---

## P2 — Policy translation

### I41. A `host:PORT` allow that loses its port must set `PolicyTranslationLossy` on the Loop

- [ ] Done

**Where:** `internal/engine/kubearmor.go` (`splitNetworkAllows`), and the
Loop status conditions set by the controller.

**Problem:** KubeArmor's egress rules match by DNS name and protocol name
(`matchDNSQueries` + `matchProtocols: tcp`), and they can't express a port.
So an `AgentPolicy` network allow of `pypi.org:443` is emitted as "`pypi.org`,
any TCP port". An allowlist translation must never grant more than its source
rule. C6b only documents this in a code comment, which a policy author never
sees. The Loop reports the policy as enforced while the port restriction
silently isn't.

**Fix:** when the effective policy contains a `host:PORT` network allow that the
engine can't express at that precision, the operator sets a
`PolicyTranslationLossy=True` condition on the Loop. The condition's reason
names the engine, and its message lists the widened allows. Once D34's per-Loop
NetworkPolicy carries the port for that allow, the translation is no longer
lossy (NetworkPolicy + KubeArmor together enforce `host:port`), so the
condition becomes `False` / reason `PortEnforcedByNetworkPolicy`. Keep the
check in one place (the engine's translation result reports what it dropped;
the controller turns that into the condition), not a string match on emitter
output.

**Acceptance:**
- A unit test: `host:443` produces a translation result that reports the
  dropped port, and a bare `host` does not.
- An envtest: a Loop whose AgentPolicy has a `host:PORT` allow gets
  `PolicyTranslationLossy=True` before D34 and `False` once the NetworkPolicy
  carrying the port exists.
- In the D34 kind e2e: from the agent, `host:PORT` succeeds and `host:OTHER`
  fails.
- The `splitNetworkAllows` comment points at this issue instead of "D33
  follow-up".

---

## P1: Owner decision

### I42. How does the agent reach the hosts its AgentPolicy allows?

- [x] Decided (owner, 2026-09-28): **option (b), egress proxy**
- [ ] Done

**Where:** ADR-0007 (the D34 "AgentPolicy network allows" paragraph),
`ensureNetworkPolicy` (agent egress), and D35.

**Problem:** since D34 (PR #13), the agent pod's NetworkPolicy is default-deny
egress: it may reach only its proxy and cluster DNS. An `AgentPolicy`
`network` allow (e.g. `proxy.golang.org:443`) is therefore not enforced as an
allow at all. That fails closed, but the feature is silently absent (no
package downloads). NetworkPolicy can't match hostnames. The obvious
translation, a port-only rule (`ports: [443]`, no `to:`), lets the agent reach
**any** host on that port. That is the evil-agent exfiltration path D34 closes.
KubeArmor's DNS matching doesn't close it either, because an agent can
connect to a hard-coded IP without any lookup. (D34's proposed
`NetworkAllowsNotEnforced` condition was a design comment only — it was never
implemented — so the gap was invisible until this review.)

**Options:**
- **(a) Resolved `ipBlock`s:** the operator resolves each allowed FQDN and
  emits /32 rules, re-resolving on a timer. No new components, but it's
  fragile for CDNs and round-robin DNS, and allows can lag DNS changes.
- **(b) Egress proxy (recommended):** the agent's external traffic goes
  through an operator-owned egress proxy that enforces the hostname
  allowlist (HTTP CONNECT + SNI/Host check), per Loop or shared. The agent's
  NetworkPolicy allows only its model proxy, the egress proxy and DNS. This
  mirrors D29 (the model proxy), works for any CNI, and gives the activity
  audit (ADR-0007 Q4) a single point to record egress.
- **(c) FQDN-aware CNI:** require Cilium (`toFQDNs`) or similar. Precise,
  but it adds a cluster requirement (kind/k3s would need Cilium).

**Until decided:** agent egress stays proxy + DNS only (fail-closed). ADR-0007
records this as an open question, not a "port-only rules" plan.
(`NetworkAllowsNotEnforced`, proposed in D34's design comment, was never
implemented — see the I42 resolution in ADR-0007.)

**Acceptance (after the decision):** in the D34/C5 kind e2e, an allowed host
is reachable from the agent, a non-allowed host on the same port is not, and
a direct IP connection to a non-allowed host on the same port is not.

---

## Owner decisions (2026-09-28)

- **I42 → (b) egress proxy.** The agent's external traffic goes through an
  operator-owned egress proxy that enforces the AgentPolicy hostname
  allowlist (HTTP CONNECT + SNI/Host check). The agent's NetworkPolicy allows
  only its model proxy, the egress proxy and cluster DNS. No port-only
  NetworkPolicy rules, ever. The builder records this as an ADR-0007
  amendment before implementing it.
- **`spec.agent.modelEndpoint` confirmed** (added in D34, PR #13): a non-secret
  `host:port` that is CEL-validated, immutable, and required together with
  `endpointSecretRef`. The operator needs no Secrets RBAC.
