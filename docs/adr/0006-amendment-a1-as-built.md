# ADR-0006 Amendment A1: as built

**Status:** Proposed (docs-only; to be reviewed per R31 I107).
**Date:** 2026-10-24
**Code line numbers:** as of `6eb74c2` (main on 2026-10-24); later merges may
move them; the function names are the stable anchor.
**Amends:** ADR-0006 (agent isolation / zero credentials) — rows 15-18 of
`docs/REVIEW-PHASE1-R31.md` (I107, on `origin/review-phase1-r31`) and the
corrections table (`.samples/r31-adr-corrections.md`).
**Supersedes:** the four ADR-0006 claims quoted below. The original ADR is a
decision record and is not edited; this amendment records what was built.

## Context

The ADR-0006 design predates D33 (the model proxy as a separate operator-owned
pod) and D34/D41 (the pod-scoped NetworkPolicy and tool proxies). Four of its
claims no longer describe the code. Per I107, the amendment records the built
behaviour; the ADR text itself is left unchanged.

## Amendment

### Row 15 — publish once, on terminal Succeeded (I107 row 15)

**ADR-0006 said** (`docs/adr/0006-agent-isolation-and-zero-credentials.md:139`):
"the publish step becomes part of the operator's per-iteration sequence
(commit → publish → pin `verifiedCommit` → create verify Job)" (lines 139-141).

**What was built:** delivery is not per-iteration. It runs once, gated on the
**terminal** `Succeeded` phase: `deliveryPhaseActive` at
`internal/controller/loop_deliver_job.go:709` returns true only when
`loop.Status.Phase == LoopPhaseSucceeded` or the Loop holds
Verifying+Delivering for delivery (`loopHoldingForDelivery`, `:694-704`), and
`ensureDeliverJob` (`:775`) creates exactly one Job per verifiedCommit — the
Job is stamped with the `coxswain.io/verified-commit` annotation
(`:898`) and a `backoffLimit` of 0 (`:964-968`), so a failed deliver Job is
terminal and never retried (one Job per verifiedCommit, `:43`, `:59`; a failed
delivery is re-runnable only via the `coxswain.io/redeliver` annotation,
`:449-454`). No per-iteration publish step exists.

**Decision now recorded:** the Loop is one-shot — exactly one goal, ending in
`Succeeded` or `Failed` (ADR-0003) — so publish-once on terminal Succeeded is
the intended behaviour, not a deviation from a per-iteration sequence. The
A's publish is the deliver Job; it pushes the pinned verifiedCommit and opens
the PR, once, at the end.

### Row 16 — the outer fence is pod-scoped (I107 row 16)

**ADR-0006 said** (`docs/adr/0006-agent-isolation-and-zero-credentials.md:68`):
"a NetworkPolicy on the sandbox allows egress only from the proxy container to
the model endpoint (plus DNS); the agent container reaches nothing but
`localhost`" (item 3, lines 68-73).

**What was built:** the NetworkPolicy is **pod-scoped** —
NetworkPolicy has no per-container egress scope — and the model proxy is a
separate pod (D33), so the agent pod's egress cannot be limited "only from the
proxy container". The built policy (`ensureNetworkPolicy`,
`internal/controller/loop_controller.go:4164`) selects the whole agent pod
(`:4308-4317`, the `<loop>-agent-netpol`'s PodSelector matches the agent pod
labels) with deny-all ingress and an egress allowlist of:

- the model proxy pod (one rule, `modelProxyEgress` at `:4153-4162`, added at
  `:4213`), present only when `spec.agent.endpointSecretRef` is set;
- the egress proxy pod (`:4214-4218`), present only when network allows exist
  (`needsEgressProxy`, called at `:4212`, defined `:3142`);
- DNS (`:4220-4223`);
- the repo host for the workspace init container's first clone — a **pod-wide**
  rule, documented in-place as an accepted MVP widening (the comment at
  `:4236-4240` says the agent container can also reach the repo host; the rule
  is at `:4244-4248`, plus an egress-proxy route for external repo hosts,
  `:4257-4261`); and
- one rule per tool proxy (`:4264-4274`), one per tool in the effective
  policy (D41d).

The per-container fence the ADR describes is **not built**: the engine emits a
pod-level union of every container's allows
(`internal/engine/kubearmor.go:114-130` — "Union the allows across containers
(pod-level policy)") and the proxy's allow set is the model endpoint itself
(`internal/policy/effective.go:156-162`), so the engine cannot distinguish
the agent container from the proxy (I107 row 26, marked `[BROKEN] [sec]` in
the same review round: the fence is the D34 NetworkPolicy, not per-container
eBPF). Today the agent's inability to reach the model endpoint directly comes
from the NetworkPolicy, which does not name the endpoint in the agent pod's
allowlist (the agent's peers are the proxies, DNS, the repo host and the tool
proxies, `:4213-4274`), and because the proxy holds the key (D33): recording
a per-container engine as the built inner fence would overstate the fence.

**Decision now recorded:** the built mechanism is D33/D34/D41 — the pod-scoped
NetworkPolicy is the egress fence (the agent pod's allowlist names the proxies,
DNS, the repo host and the tool proxies, never the model endpoint), and the
proxy pod holds the key (D33). The per-container eBPF fence the ADR describes
is not built (the engine policy is a pod-level union, I107 row 26). The ADR's
"egress only from the proxy container" wording is over-precise (NetworkPolicy
cannot express it) and is superseded by this record.

### Row 17 — the model proxy is a separate pod (I107 row 17)

**ADR-0006 said** (`docs/adr/0006-agent-isolation-and-zero-credentials.md:53`):
"Model access via a sidecar proxy in the sandbox pod. The agent talks to
`http://localhost:<port>` (`COX_MODEL_BASE_URL`); the proxy container holds the
real model key (mounted only into the proxy, never the agent)" (item 2a,
lines 53-58).

**What was built:** superseded by D33 — the model proxy is a **separate
operator-owned pod** (`<loop>-proxy`, `ensureProxy`,
`internal/controller/loop_controller.go:2410`), not a sidecar in the sandbox
pod. The agent's `COX_MODEL_BASE_URL` is the proxy's in-cluster Service FQDN
(`proxyServiceURL`, `:2694-2696`; set at `:1542`), not `localhost`. The model
Secret is mounted into the proxy pod only (D33 comment at `:1948-1949`). The
only Secret volume the sandbox pod ever carries is the git credential for the
workspace init container's first clone (S3a, `:1740-1755`, mounted read-only
into the init container only, `:5475`).

**Decision now recorded:** D33 (separate model proxy pod, ADR-0007/0009)
supersedes item 2a's sidecar wording; the zero-credential property (the key
lives only on the proxy, the agent container holds no model key) is unchanged.
ADR-0006 carries no D33 pointer; this row is that pointer.

### Row 18 — defaults come from CRD markers (I107 row 18)

**ADR-0006 said** (`docs/adr/0006-agent-isolation-and-zero-credentials.md:87`):
"Cluster defaults come from a `coxswain-agent-defaults` ConfigMap so the README
sample stays short" (item 5, line 87; repeated at lines 147-148: "the
`spec.agent` CRD fields and the `coxswain-agent-defaults` ConfigMap are new
Phase 1 surfaces (the README sample gets an `agent:` block)").

**What was built:** no such ConfigMap exists. `grep -rn 'agent-defaults'
--include=*.go --include=*.yaml .` outside `docs/` finds 0 hits. The `spec.agent`
CRD fields were built (`api/v1alpha1/loop_types.go:545-580`, `AgentConfig`), but
there are no kubebuilder `default` markers on the agent fields (`Image`,
`Model`, `EndpointSecretRef`, `ModelEndpoint` are all `+optional` with no
default; a grep for `+kubebuilder:default` in the file finds defaults only on
unrelated fields, e.g. `:89`, `:98`, `:224`). The samples set the agent fields
explicitly (`config/samples/loop-*.yaml`); the README's inline sample does not
(`README.md:54-69` carries no `agent:` block), so the ADR's parenthetical
"the README sample gets an `agent:` block" was not built either — and there is
no ConfigMap to fall back to. (The operator does
have a different, real ConfigMap, `coxswain-model-prices`, for model pricing —
`api/v1alpha1/loop_types.go:530-535`; that is not the agent-defaults ConfigMap.)

**Decision now recorded:** the `coxswain-agent-defaults` ConfigMap is dropped
from the record. Defaults come from the CRD markers (none on the agent
fields) plus the operator's flags; the samples, not the README, set the agent
fields explicitly.

## What this amendment does NOT change

- **The zero-credential property** (the ADR's core) — the agent container still
  holds no model key, no SA token, and no git credential at runtime (the git
  credential volume is removed once the base SHA is pinned, `:1740-1755`).
- **Row 19** (the "evil agent" e2e, `0006:91-99`) — that row's disposition is
  *build* (F54), not amend; it is not part of this amendment.
- **ADR-0007's matching rows** (D33, D34, the pod-level engine union) will be
  amended under ADR-0007's own amendment (per I107, still open); this
  amendment only records the ADR-0006 side.
- **`CONTEXT.md`** — I107 names no CONTEXT.md line for ADR-0006; no change.
