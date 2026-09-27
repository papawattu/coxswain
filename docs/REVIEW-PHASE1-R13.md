# Phase 1 review, round 13 — D29 decided: the proxy runs in its own pod

Owner decision (2026-09-27), recorded by the reviewer. Supersedes ADR-0007's
round-9 D29 text ("the engine policy scopes by container in the pod") and the
recommendation (a) in the builder's `docs/D29-PER-CONTAINER-EGRESS.md`
proposal on PR #8.

## Decision

**Option (c): the model proxy runs in its own operator-owned pod, not as a
sidecar in the agent's sandbox pod.**

Why (from the PR #8 review): NetworkPolicy and KubeArmorPolicy are both
pod-scoped, and agent-sandbox allows exactly one pod per Sandbox, so nothing
can split egress between two containers of one pod. Option (a) relied on "the
agent has no key", which fails for keyless model endpoints (the homelab vLLM,
Ollama) — the exact case D29 exists for. A separate pod makes the split a plain
pod-to-pod boundary that works for keyed and keyless endpoints alike.

## What this requires (builder: turn into ADR-0007/ADR-0006 amendments + plan slices)

### D33. Proxy pod + Service per Loop (replaces C2a's sidecar)

- [ ] Done

The operator creates, per Loop, a proxy **Pod** (or single-replica
Deployment) and a **Service** `<loop>-proxy`, owner-referenced to the Loop
(garbage-collected with it). The model-creds Secret is mounted **only** into
the proxy pod. The agent container gets
`COX_MODEL_BASE_URL=http://<loop>-proxy.<namespace>.svc:8080` and no proxy
container remains in the sandbox pod. The proxy pod gets the same hardening as
the agent (non-root own UID, read-only rootfs, drop ALL, seccomp, no SA token,
limits). C2a's sidecar code and tests are replaced, not kept alongside.

**Acceptance:** envtest — the sandbox pod has one container (`agent`), no
model-creds volume; a proxy pod + Service exist, owner-ref'd to the Loop, with
the Secret mounted read-only; the agent's `COX_MODEL_BASE_URL` points at the
Service. Kind — README flow still reaches `Running` (with and without an
endpoint secret).

### D34. NetworkPolicies do the split

- [ ] Done

Generated per Loop, default-deny both ways:
- **Agent pod:** egress to the `<loop>-proxy` pod on 8080, DNS, and the
  `AgentPolicy` network allows — nothing else. Ingress: none.
- **Proxy pod:** ingress **only** from this Loop's agent pod on 8080 (so no
  other pod can use this Loop's key); egress only to the model endpoint + DNS.
This absorbs C3's per-pod NetworkPolicy and is where D29 is actually enforced.

**Acceptance (the C5 evil-agent case):** on kind, from the agent container, a
direct call to the model endpoint fails (keyless endpoint included — use an
unauthenticated in-cluster fake), a call through the proxy Service succeeds, a
call from an unrelated pod to the proxy Service fails.

### D35. Gate, audit and enforcement cover both pods

- [ ] Done

- The D30 gate requires the proxy pod Ready (and its policy enforced) before
  the sandbox is `Running`.
- The proxy pod gets its own operator-owned engine policy (process allow =
  the proxy binary only; network = model endpoint), separate from the user's
  `AgentPolicy`, which applies to the agent pod only.
- The proxy is the activity-audit source for model calls (ADR-0007 Q4); the
  per-Loop Service identity makes those records attributable.

## Process

- Move `docs/D29-PER-CONTAINER-EGRESS.md` off PR #8: keep its analysis (the
  pod-scope facts are right) and add a note that the owner chose (c). PR #8
  stays C6b code.
- Sequence: finish C6b's open P1s on #8 (enforcement actually blocking,
  `/**/` spoofing, base-manifest flag), then D33 → D34 → D35 as slices.
