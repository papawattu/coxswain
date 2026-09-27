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
