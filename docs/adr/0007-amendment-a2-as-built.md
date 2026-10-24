# ADR-0007 Amendment A2: as built — audit envelope, engine fence, policy hash, fail-closed reasons

**Status:** Proposed (docs-only; to be reviewed per R31 I107).
**Date:** 2026-10-24
**Code line numbers:** as of `6eb74c2` (main on 2026-10-24); later merges may
move them; the function names are the stable anchor.
**Amends:** ADR-0007 rows 24, 26 and 28 of `docs/REVIEW-PHASE1-R31.md`
(I107, on `origin/review-phase1-r31`), **ADR-0007 Amendment A1** rows 29 and
30 of the same table (the rows that cite
`docs/adr/0007-amendment-a1-static-enforcement.md`), and the proxy-policy
passage at `0007:573-582` (the line I107 calls `0007:562-575`).
**Supersedes:** the five passages quoted below (three of ADR-0007 D29/Q4/D32,
two of Amendment A1) and the "will add" / "does not exist yet" clauses at
`0007:573-582`. The ADR and A1 are decision records and are not edited; this
amendment records what was built.

## Context

ADR-0007 was written before the engine translation existed (it specifies
per-container scoping that the emitter does not produce), before the Q4 audit
envelope had a producer, and before the D30 gate had a reason vocabulary.
Amendment A1 (D52) then specified "every unreadable fact reads `Unknown`" and
an optional post-start re-check. R31 (I107) flags five of those claims plus
the proxy-policy line as no longer describing the code. This amendment
records the built behaviour.

## Amendment

### Row 24 — the Q4 envelope has no `iteration`, and no relay fills it (I107 row 24)

**ADR-0007 said**
(`docs/adr/0007-agentpolicy-default-deny-ebpf-audit.md:97-100`): "**JSON
lines** on each source's stdout with a common envelope —
`{time, loop, namespace, iteration, source, action, target, verdict,
detail}` — so any log collector can capture and join them by Loop."

**What was built:** the egress proxy emits one JSON line per connection
attempt that omits `iteration` and says why —
`internal/egress/audit.go:5-12`: "It deliberately carries NO iteration field:
the proxy is a long-lived pod and its env is immutable, so it cannot read
`status.iteration`. The relay (which joins records from all sources into the
Q4 stream) fills iteration by time window … The policy hash in detail
disambiguates if the effective policy changed mid-iteration." The record is
`AuditRecord` (`:17-26`: `time`, `loop`, `namespace`, `source`, `action`,
`target`, `verdict`, `detail`), `Source = "egress-proxy"` (`:30`).

The other two producers in the tree omit it as well: the tool proxy's
`AuditRecord` (`internal/toolproxy/audit.go:9-20`, `Source = "tool-proxy"`
`:23`) has tool-level fields and no iteration, and the model proxy's audit
line is a map literal with no iteration (`internal/proxy/proxy.go:257-268`:
`time, loop, namespace, source, action, model, promptTokens,
completionTokens, status, usagePresent`).
`git grep -n 'json:"iteration' origin/main -- internal/` has exactly **one**
hit, `internal/controller/loop_s4_phase.go:115` — the phase claim's field,
not an audit record.

There is also no relay to fill the field: `git ls-tree --name-only origin/main
cmd/` is `cox`, `egress-proxy`, `kubectl-cox`, `main.go`, `memory-writer`,
`model-proxy`, `proxy-standin`, `runner`, `tool-proxy` — no relay binary — and
`internal/` has no relay package.

**Decision now recorded:** the Q4 envelope as built has no `iteration`. All
three sources cannot carry it (the proxies are long-lived pods whose env is
immutable and which do not read Loop status), and the joiner that would add it
is not built (that absence is I107 row 23, a build row, not this one). The correlation handles
that exist today are `loop`/`namespace` on every record and the
effective-policy hash in `detail`
(`internal/egress/audit.go:15-16`, the same hash as
`status.policy.effectiveHash`).

### Row 26 — the engine emits a pod-level union, not a per-container fence (I107 row 26)

**ADR-0007 said** (`0007:131-148`, D29 and the C5 case): "**Per-container
egress is enforced by the eBPF layer**, not the NetworkPolicy … The **eBPF
layer** is what enforces **per-container** egress; the NetworkPolicy is only
the coarse outer fence … The engine policy scopes by **container** in the pod
(KubeArmor can do this)", and C5 ("the evil agent calls the model endpoint
**directly**") is "**blocked by eBPF**, recorded in the activity audit, and
absent from the proxy's metering".

**What was built:** the input *is* scoped per container —
`policy.Translate` builds `agentAllows`
(`internal/policy/effective.go:141-154`: the proxy FQDN, the egress-proxy
FQDN when present, the platform DNS allow, plus the user's exec / network /
file allows) and `proxyAllows` (`:156`:
`{AllowNetwork, modelEndpoint}`) and returns them as two `ContainerPolicy`
entries, both `Default: Deny` (`:158-163`). The emitter then **unions** them:
`internal/engine/kubearmor.go:114-130` ("Union the allows across containers
(pod-level policy)") iterates `ep.Containers` and appends every allow into one
`exec`/`network`/`files` set, which becomes one KubeArmorPolicy per Loop —
`spec.action: Block` plus the unioned allow rules (`:145-150`) — selected by
`coxswain.io/loop: <loop>` (`:147-149`). The emitter's own comment records
the consequence: "Per-container scoping (D29) is NOT expressible in one
KubeArmorPolicy (the selector is pod-level), so this emits ONE pod-level
policy carrying the UNION of the agent and proxy allows"
(`internal/engine/kubearmor.go:65-69`). The sandbox pod carries the loop
label (`internal/controller/loop_controller.go:1900`), and the model proxy
pod does not (`proxyLabels`, `:2667-2672`: `app.kubernetes.io/component` =
`model-proxy` + `coxswain.io/proxy-for`). So the policy applied to the
**agent pod** contains the proxy container's allow as well: the agent
container's effective allow set includes the model endpoint, which is exactly
the bypass D29 exists to prevent. The **per-container fence is NOT built**.

What actually keeps the agent off the model endpoint is the **NetworkPolicy**:
the agent pod's egress allowlist is the model proxy peer
(`loop_controller.go:4213`), the egress proxy peer when network allows exist
(`:4214-4219`), DNS (`:4220-4223`), the repo host for the init clone
(`:4241-4248`) or the egress proxy when the repo host is external
(`:4252-4259`), and one peer per tool proxy (`:4264-4274`) — the model
endpoint is not among them, so a direct connection is dropped by the CNI. No
eBPF audit record exists for such an attempt: nothing translates engine
alerts into the Q4 envelope (I107 row 25, a build row).

The *proxy* side of D35 is built per pod, not per container: each proxy has
its own KubeArmorPolicy with its own selector and a process + network allow
set — egress proxy `internal/engine/kubearmor_i42.go:71-93`, model proxy
`:111-155`, tool proxy `:239-266`, all emitted from
`kubearmor_enforcer.go:121-164`.

**Decision now recorded:** D29's "per-container egress enforced by the eBPF
layer" is **not built**. The engine emits one pod-level union per Loop, so it
cannot distinguish the agent container from the proxy container; the boundary
that holds today is the D34 NetworkPolicy's agent-pod egress allowlist (plus
the proxies' own per-pod engine policies). The consequence for C5 is recorded
plainly: a direct agent → model-endpoint call is **blocked by the
NetworkPolicy, not by eBPF, and is not recorded in the activity audit** (no
producer, row 25) and not in metering (the call is dropped). Restoring D29's
per-container fence — or dropping the claim — is a build/owner decision
tracked under I107 row 26; this record does not assume either.

### Row 28 — the policy hash is current-value; there is no per-iteration record (I107 row 28)

**ADR-0007 said** (`0007:196-199`, D32): "**Policy changes mid-Loop** apply at
the **next iteration boundary**, and each `status.history[]` entry records
the **effective policy's hash/generation**, so the decision audit shows what
the agent was allowed to do in each iteration."

**What was built:** there is no `status.history[]` —
`grep -n History api/v1alpha1/loop_types.go` gives only `stallHistory`
(`:891-896`) and `StallEntry` (`:1185-1211`), and `StallEntry`'s fields are
iteration, job name, output hash, normalisation version, check name and time
— no policy hash. The hash is a **current-value** field:
`status.policy.effectiveHash` (`api/v1alpha1/loop_types.go:400-405`),
recorded by the operator (`internal/controller/loop_controller.go:1283-1286`),
alongside `status.policy.kaptSpecHash` (`:407-413`, the D52 fact-3 hash).
The field's own doc still promises the per-iteration record — "It lets the
decision audit show what the agent was allowed to do in each iteration
(D32)" (`:402-403`) — which the field cannot do: it holds one value, and a
later policy overwrites it. `cox history` renders `status.stallHistory[]`
and says so (`internal/cli/history.go:10-15`).

The first half is not built either: the effective policy is recomputed and
applied on **every reconcile** — `applyEffectivePolicyAndConditions`
(`loop_controller.go:5142-5176`) runs from `Reconcile` (`:869`) and captures
the enforcement observation before applying (`:5161`) — not at an iteration
boundary.

**Decision now recorded:** the ADR's per-iteration policy record does not
exist. The policy hash is a current-value observation
(`status.policy.effectiveHash`), useful for correlating an audit line with
the policy in force *now*, not with the policy of iteration *n*; the only
per-iteration ring in status is `stallHistory`, which is about verify
failures. The `history[]` framing is dropped from the record.

### A1 row 29 — only `EnforcementUnverified` reads `Unknown`; other missing facts read `False` (Amendment A1 `:142-166`)

**Amendment A1 said**
(`docs/adr/0007-amendment-a1-static-enforcement.md:142-166`, "Missing fact or
outage reads `Unknown`, never `True`"): "every fact that **cannot be read**
(the object is absent, the API is unreachable, the field is missing) is
reported as **`Unknown`**, and `Unknown` is **never `True`** … the condition
message names which fact is missing, e.g. `EngineUnavailable` |
`NodeNotEnforcing` | a new `EnforcementUnverified`/`EvidencePending`
reason."

**What was built:** the `PolicyEnforced` condition is `Unknown` for exactly
one reason. `internal/controller/loop_controller.go:5205-5212` starts at
`ConditionFalse` and flips to `ConditionUnknown` only when the reason is
`engine.ReasonEnforcementUnverified` (`:5209-5211`); the deleting-path twin
is `:5301-5305`. Every other reason is `False`, with its reason kept
(`:5184-5201`, messages by reason; the `--allow-unenforced` escape hatch's
naming in the condition message is at `:5192-5196` and `:5202-5204`):

| observation | reason | condition |
|---|---|---|
| no Enforcer wired and `--allow-unenforced` is off | `EngineUnavailable` (`internal/controller/loop_controller.go:1306-1314`) | **False** (default branch, `:5199-5200`) |
| no Enforcer wired and `--allow-unenforced` is set | `EnforcementUnverified` (`:1309-1310`) | **Unknown** (`:5209-5211`) |
| the Loop's KubeArmorPolicy is absent, not controlled by the Loop, missing its spec, has no recorded hash, or its selector does not match | `EnforcementUnverified` (`internal/engine/kubearmor_enforcer.go:406-408`, `:416-417`, `:428-429`, `:431-433`, `:443-447`) | **Unknown** (`:5209-5211`) |
| the node list, the agent-pod list, or the Kapt read fails (API outage / CRD absent) | `EngineUnavailable` (`kubearmor_enforcer.go:368-370`, `:378-381`, `:410-413`) | **False** (`:5197-5198`) |
| no node carries the BPF-LSM label, or a labelled node has no Ready DaemonSet agent pod | `NodeNotEnforcing` (`:372-373`, `:395-396`) | **False** (default branch, `:5199-5200`) |
| the live Kapt spec hash differs from the recorded `kaptSpecHash` | `KaptTampered` (`:434-435`; reason added by owner D55 2026-10-12, `internal/engine/enforcer.go:80-90`) | **False** (default branch) |

So an **API outage** reads `False`/`EngineUnavailable`, not `Unknown`, and
an absent node label reads `False`/`NodeNotEnforcing`. `EvidencePending` was
never added; the reason vocabulary is `internal/engine/enforcer.go:67-91`.

The half of A1 that holds is the load-bearing one: `Unknown` is never
`True`, and **both** `False` and `Unknown` hold the gate. The D30 gate reads
`enforcementStatus` on every reconcile (`loop_controller.go:2023-2025`,
driven from `:1884-1892`, level-based), so a `False` and an `Unknown` both
leave the sandbox Suspended (the `--allow-unenforced` escape hatch applied at
the gate, `:2023`, aside — its reason and condition message are named at
`:5192-5196` and `:5202-5204`).

**Decision now recorded:** A1's "unreadable ⇒ `Unknown`" is replaced by the
actual mapping above — `Unknown` means *the operator has no engine probe, or
the policy fact is not verifiable yet*, and every other missing or failing
fact is `False` with a named reason. Fail-closed is unchanged: only `True`
opens the gate, and both `False` and `Unknown` hold the sandbox Suspended.

### A1 row 30 — no post-start node re-check; the gap is the landed node's label (Amendment A1 `:208-215`)

**Amendment A1 said**
(`docs/adr/0007-amendment-a1-static-enforcement.md:208-215`): "**Optional
post-start re-check (defence in depth):** once the sandbox pod runs, the
operator can re-check its **actual** `spec.nodeName` (the node it landed on)
and flip the gate back to Suspended if that node no longer satisfies the
facts (e.g. the agent pod was deleted, the BPF-LSM label was removed)."

**What was built:** nothing reads the sandbox pod's `spec.nodeName`.
`git grep -n 'Spec.NodeName' origin/main -- internal/ ':!*_test.go'` has one
hit, `internal/engine/kubearmor_enforcer.go:578`, and it selects **KubeArmor
agent** pods on a given node inside the agent-pod lister
(`listAgentPods`, `:562`) — not the sandbox pod. The static check reads the
nodes that **currently** carry the BPF-LSM label (`:367-374`) and requires a
Ready DaemonSet agent pod on each of them (`:376-398`).

That leaves one drift case open: if the BPF-LSM label is **removed from the
node the sandbox actually landed on**, that node leaves the checked set, the
remaining labelled nodes still satisfy the facts, and the running sandbox is
not suspended. The other case A1 names *is* covered without the re-check: an
agent pod deleted on a still-labelled node fails the check for that node
(`:395-396`).

What does exist is the gate being **level-based**: `sandboxSuspendedByGate`
(`loop_controller.go:1998-2025`) runs on every reconcile and drives
`OperatingMode` back to `Suspended` when the gate holds (`:1884-1892`). So a
detected failure re-suspends an already-running sandbox; what is missing is
any check anchored to the pod's landed node.

**Decision now recorded:** the node-identified post-start re-check is not
built. The built equivalent is the every-reconcile re-evaluation of the same
three facts, which catches drift that stays inside the labelled node set and
re-suspends a running sandbox. The uncovered case — the landed node losing
its BPF-LSM label — is recorded here as a known gap (build item under I107
row 30's disposition).

### The proxy KubeArmor policies exist (`0007:573-582`, the line I107 calls `562-575`)

**ADR-0007 said** (`0007:573-582`): "The egress proxy's own KubeArmor policy
(D35 part 2, **when C6b merges**) **will add** a **second layer** … The
KubeArmor policy is per-Loop (owned by the Loop, same shape as the model
proxy's KubeArmor policy under D35 part 2 — **which does not exist yet**;
the model proxy's NetworkPolicy carries that gate today)."

**What was built:** both exist (I42f). The egress proxy's policy is
`EmitEgressProxyKubeArmorPolicy` (`internal/engine/kubearmor_i42.go:71-93`):
`spec.action: Block`, selector `egressProxyKaptSelector(loopName)`
(`:76-78`), a process block allowing only `egressProxyBinaryPath`
(`:79-82`, `:42`), and — when the effective policy has network allows — a
network block of `matchDNSQueries` + `matchProtocols` from those allows
(`:84-86`, via `proxyNetworkBlock`, `:165-173`). It is created only when
`len(p.Network) > 0` and cleaned up when the allows go away
(`internal/engine/kubearmor_enforcer.go:142-149`).

The model proxy's policy is `EmitModelProxyKubeArmorPolicy`
(`kubearmor_i42.go:111-155`): the same shape, selector
`modelProxyKaptSelector(loopName)` (`:118-120`), the proxy binary as the only
exec (`:121-124`, `modelProxyBinaryPath` at `:47`), and the endpoint host in
`matchDNSQueries` (plus its search-expanded form when the host is a bare
single-label in-cluster name, `:129-137`) with `tcp` and the platform DNS
allow in `matchProtocols` (`:125-141`). It is created only when
`spec.agent.endpointSecretRef` is set and cleaned up when it is removed
(`kubearmor_enforcer.go:129-137`). Tool proxy policies are the same shape per
tool (`kubearmor_i42.go:239-266`, upstream host in `matchDNSQueries` plus the
platform DNS allow), emitted per tool at `kubearmor_enforcer.go:155-161` and
cleaned up when the tool leaves the effective policy (`:164`,
`cleanupStaleToolKapt` at `:171`).

Each policy's selector is the proxy's **own** label set, never
`coxswain.io/loop` (`egressProxyKaptSelector`, `kubearmor_i42.go:273-278`;
`modelProxyKaptSelector`, `:283-289` — both comments: "The selector NEVER
carries coxswain.io/loop") — so these fences are per **pod**, and the agent
pod's policy is the unioned one of row 26 above.

**Decision now recorded:** the "when C6b merges … will add" and "which does
not exist yet" clauses are stale. The egress proxy, model proxy and tool
proxy each have their own per-Loop KubeArmorPolicy with a process and network
allow set; the model proxy's inner fence is the engine policy, not only the
NetworkPolicy. What remains unbuilt is the **per-container** scoping inside
the agent pod (row 26), not these proxy policies.

## What this amendment does NOT change

- **The fail-closed gate (D30).** Only `True` opens it; `False` and `Unknown`
  both hold the sandbox Suspended
  (`internal/controller/loop_controller.go:2023-2025`). A1's
  `--allow-unenforced` escape hatch and its naming in the condition message
  are unchanged (`:5192-5196` and `:5202-5204`).
- **The static check's three facts and their in-progress handling (A1/D52).**
  This amendment only corrects the reason→condition mapping (row 29) and the
  post-start claim (row 30); the facts themselves are unchanged
  (`internal/engine/kubearmor_enforcer.go:356-450`).
- **I107 rows 20-23, 25, 27 and 31** — not this amendment: rows 21, 22, 23
  and 25 are build rows (F-items), rows 20 and 27 are owner decisions (D81),
  row 31 is no action. Row 25 (no engine alert producer) is the reason row
  26's C5 case is unaudited; it is a build item, not an amendment.
