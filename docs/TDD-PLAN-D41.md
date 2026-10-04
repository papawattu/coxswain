# D41 — TDD plan: tool proxies

D41 (`docs/REVIEW-PHASE1-R17.md`, owner answers R18) adds credentialed
per-tool, per-Loop proxies so the agent can use authenticated external APIs
without holding credentials — generalising ADR-0006's zero-credential rule to
tools. The ADR is `docs/adr/0008-tool-proxies.md` (the authoritative design);
this plan breaks it into buildable slices.

Owner decisions (R18) that bound the shape:
- tools are declared **on AgentPolicy** (`spec.tools[]`) — no `ToolProxy` kind;
- **a proxy per tool, per Loop** (strongest isolation, matches today);
- a **generic HTTP tool proxy** as the built-in default — typed extensions
  (GitHub path templates, SDK signers) are explicitly deferred.

MVP scope, per the ADR's "Deferred" section: no `ToolProxy` kind, no typed
tools, no shared proxies, no credential hot-reload.

Build conventions (same as the I42 slices): each slice is red→green;
envtest-first where the operator is involved, unit tests for the binary;
kind acceptance on `--context kind-coxswain-dev` at the end (D41e). Every
gate this plan adds is mutation-checked per the R20 I49 norm: the named
mutation (e.g. disable the gate) is applied in a **scratch worktree only**,
the affected spec must FAIL, and the result is recorded in the PR.

Existing seams the slices reuse (verified against `main`):

- `internal/policy` — `ComponentProxyLabel`/`ComponentEgressProxyLabel`,
  `ProxyComponentSelector()` (the I44 pin), the I45 admission-policy YAML pin
  (`policy_yaml_test.go`), the effective-policy union (`effective.go`).
- `internal/controller/loop_controller.go` — `proxyLabels`/`egressProxyLabels`
  (disjoint label sets), `buildProxyPod` (the model proxy pod: UID 65533,
  creds Secret volume, hardening), `egressProxyPod` (UID 65534), the D35a
  owned+Ready sandbox gate, the spec-hash drift-recreate pattern
  (`proxyPodSpecHash` + annotation), `ProxyConflict` foreign-pod gate,
  `foreignNetPols`/`foreignKaptPolicies` order-independent gates, `COX_*`
  agent env + `NO_PROXY` wiring (I42d).
- `internal/engine/kubearmor_i42.go` — `EmitEgressProxyKubeArmorPolicy` /
  `EmitModelProxyKubeArmorPolicy` (process fence + DNS allowlist, `spec.action
  Block`).
- `cmd/egress-proxy` + `cmd/proxy-standin` — the binary and image patterns
  (env config, JSON audit on stdout, read-only rootfs, UID 65534/65533, no SA
  token).
- `test/e2e/i42-e2e.sh` + `Makefile` — the kind acceptance pattern.

---

## D41a — `cmd/tool-proxy` binary (the generic HTTP tool proxy)

**Scope:** a new Go binary (`cmd/tool-proxy/` + `internal/toolproxy/`) that
forwards HTTP requests to a single upstream, gated by request rules, with
credential injection. It is the model proxy generalised from "forward all to
one endpoint" to "rule-check, then forward, injecting the credential".

- Listens on `:8080` (env `TOOL_PROXY_PORT`, default 8080).
- Config via env (Phase 1, as in I42a): `TOOL_NAME`, `TOOL_UPSTREAM`
  (base URL), `TOOL_RULES_JSON` (array of `{methods: [...], paths: [...]}`),
  `TOOL_CREDENTIAL_FILE` (path to the mounted Secret key file; optional),
  `TOOL_POLICY_HASH`, `LOOP_NAME`, `LOOP_NAMESPACE`, `POD_CIDR`, `SERVICE_CIDR`.
- **Request handling:** plain-HTTP absolute-form requests and `CONNECT`
  tunnels are the only accepted shapes. The authority must be the upstream's
  host (any other host → 403 + audit). The request path (relative to the
  upstream's base prefix) is checked against the rules:
  - **match** (method in the rule's methods AND path prefix in the rule's
    prefixes) → credential injection (if configured: `Authorization: Bearer
    <file contents>`), forward to the resolved upstream IP.
  - **no match** → `403 Forbidden`, JSON audit record, no upstream dial.
- **Resolved-IP carve-outs (I42a backstop):** the proxy resolves the upstream
  host itself, rejects (403 + audit) if **any** resolved IP is in the
  carve-out set (private ranges, link-local, loopback, ULA, the pod/service
  CIDRs from env), and dials the **same resolved IP** (no re-resolution). A
  tool upstream that resolves into the cluster is therefore blocked even if
  validation (D41b) was bypassed or the DNS record rebinds.
- **Audit (JSON on stdout, one line per request, allowed and blocked):**
  `{time, loop, namespace, source: "tool-proxy", action: "request", tool,
  method, path, status, policy}`. The credential value is **never** in the
  record (no headers are logged).
- **Pod hardening:** no SA token, read-only rootfs, UID 65535 (distinct from
  agent 65532, model proxy 65533, egress proxy 65534), no capabilities,
  seccomp RuntimeDefault.
- **No TLS termination / no MITM** of the upstream (tunnel or forward, never
  decrypt).

**Unit tests (written first, `internal/toolproxy/`):**
- **Rule engine (table-driven):** allowed method+prefix → forwarded; allowed
  prefix wrong method → 403; disallowed prefix → 403; path-prefix semantics
  (`/repos/acme/*` matches `/repos/acme/x/y` but not `/repos/acmer`); a
  request whose authority is not the upstream host → 403; empty rule set →
  everything 403.
- **Credential injection:** with a credential file, the upstream request
  (captured by a test server) carries `Authorization: Bearer <value>`; without
  one, no such header. **Redaction:** the audit line for a credentialed
  request contains no substring of the test credential value.
- **Audit record shape:** allowed record has the right fields (status = the
  upstream's status); blocked record has `status: 403` and the method/path;
  both carry the `policy` hash from env.
- **Resolved-IP rejection:** a resolver returning a private/pod-CIDR IP →
  403 + audit, no dial; a public IP → dial to that exact IP (no
  re-resolution: a second lookup attempt must be observable and rejected or
  the dial uses the first answer — assert which).

**Acceptance:** `go build ./cmd/tool-proxy` + a unit suite green under
`make test`; the image is buildable (Dockerfile mirroring `cmd/egress-proxy`'s,
COPY to `/usr/local/bin/tool-proxy`) so D41c can reference it.

**Gate mutation (I49 norm):** disabling the rule check (forward everything)
in a scratch worktree must make the rule-engine spec FAIL (the 403 cases get
forwarded).

**Dependencies:** none (leaf binary; the same position in the graph as I42a).

---

## D41b — API: `AgentPolicy.spec.tools[]` + validation

**Scope:** the CRD field and its admission + controller-side validation.
No controller behaviour in this slice (D41c consumes it).

- **Shape:** `AgentPolicySpec.Tools []ToolSpec`, `ToolSpec{Name string,
  Upstream string, CredentialSecretRef string, Rules []ToolRule}`,
  `ToolRule{Methods []string, Paths []string}`. JSON tags omitempty;
  `tools` omitempty (policies without tools are unchanged).
- **CEL validation (on AgentPolicy, the I42e pattern):**
  - `name`: matches `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` (DNS-1035; fits the
    derived `<loop>-tool-<name>` name budget with `derivedNameFits`).
  - `upstream`: matches `^https?://` (nothing else — the controller does the
    host checks).
  - `rules`: each entry's `methods` entries are one of
    GET/POST/PATCH/PUT/DELETE (case-sensitive); at least one rule per tool.
  - **In-cluster upstream rejection (first layer, like I42e's network rule):**
    the upstream's host must not end in `.svc` / `.svc.cluster.local` /
    `.cluster.local`, and must not be `localhost` / `127.0.0.1` (the cheap,
    suffix-based CEL layer; keep it within the CRD's CEL cost budget — if the
    network rule plus this push it over, move the suffix check
    controller-side only, as I42e did for the trailing-dot forms).
  - Names unique within the list (`self.all(t, self.filter(o, o.name == t.name).size() == 1)`
    form, or the cheapest equivalent that fits the cost budget).
- **Controller-side validation (in the effective-policy computation, D41c's
  consumer, but the function + its tests land here):** mirrors I42e's
  `FindInClusterNetworkAllow` — reject, for the effective union, upstreams
  whose host is an IP literal in the reconciler's POD_CIDR/SERVICE_CIDR,
  `localhost`, `127/8`, `169.254/16`, `0.0.0.0`, or the full loopback forms.
  Failure mode is the fail-closed `PolicyValid=False` pattern (reason
  `ToolUpstreamInCluster`, naming the offending upstream) — NOT a reconcile
  error loop — and the sandbox is not created.
- `make manifests generate` after the type change; the CRD diff is
  committed.

**Envtest-first tests (`internal/controller/loop_d41_validation_test.go`):**
1. **`.svc` upstream rejected at admission.** Creating an AgentPolicy with
   `upstream: "http://my-svc.default.svc:8080"` → the API server rejects the
   create (CEL fires).
2. **`localhost` / `.cluster.local` upstreams rejected** (same, CEL).
3. **IP in pod CIDR rejected controller-side.** A Loop referencing an
   AgentPolicy with `upstream: "http://10.244.0.5:8080"` (test reconciler
   POD_CIDR `10.244.0.0/16`) → `PolicyValid=False` reason
   `ToolUpstreamInCluster`, no sandbox pod, no reconcile-error loop.
4. **IP in service CIDR rejected** (SERVICE_CIDR `10.96.0.0/12`).
5. **Legitimate upstream passes.** `upstream: "https://api.github.com"` →
   `PolicyValid=True` (or no condition), no rejection.
6. **Bad method / duplicate name / non-http scheme rejected** (CEL, API
   server rejection).
7. **No tools → no change in behaviour.** An existing policy with only
   network allows reconciles exactly as before (guards the union).

**Gate mutation (I49 norm):** in a scratch worktree, comment out the
controller-side in-cluster check — specs 3 and 4 must FAIL (the Loop
reconciles as if the upstream were legitimate).

**Acceptance:** `make manifests generate lint test` green; the CRD YAML diff
contains only the `tools` additions (no unrelated regeneration drift).

**Dependencies:** none for the CEL rules (pure CRD); the controller-side
check uses the same POD/SERVICE CIDR reconciler fields as I42c.

---

## D41c — Operator: per-tool proxy Pod + Service + credential mount + gates

**Scope:** the operator provisions, gates and drift-corrects one proxy pod +
Service per tool in the effective union. This is `ensureProxy`/
`ensureEgressProxy` with the tool loop.

- **Gate:** expected iff the effective policy (union of `spec.policyRefs`)
  has ≥1 tool entry (union semantics: a tool present in any referenced
  policy is expected; union conflicts — same tool name, different specs —
  fail closed via `PolicyValid=False` reason `ToolConflict`, same
  fail-closed pattern as the network union).
- **Pod** (`<loop>-tool-<name>`): built from the model proxy pod shape
  (`buildProxyPod`) with the differences:
  - image from a `ToolProxyImage` field on `LoopReconciler` (stand-in
    `golang:1.26` when unset, as with the egress proxy);
  - UID/GID **65535** (distinct from agent 65532, model proxy 65533, egress
    proxy 65534);
  - env: `TOOL_NAME`, `TOOL_UPSTREAM`, `TOOL_RULES_JSON`,
    `TOOL_POLICY_HASH` (the `status.policy.effectiveHash`), `LOOP_NAME`,
    `LOOP_NAMESPACE`, `POD_CIDR`, `SERVICE_CIDR`;
  - credential: when `credentialSecretRef` is set, the Secret is mounted
    **read-only into this container only** (`/tool-cred`, mode 0444) with
    `TOOL_CREDENTIAL_FILE=/tool-cred/<key>`; **never into the sandbox pod**
    (asserted);
  - liveness/readiness TCP 8080; resources parity (limits 100m/128Mi,
    requests 10m/32Mi); `automountServiceAccountToken: false`.
- **Labels (disjoint, like egressProxyLabels):**
  `app.kubernetes.io/name: coxswain-tool-proxy`,
  `app.kubernetes.io/instance: <loop>`,
  `app.kubernetes.io/component: tool-proxy`,
  `app.kubernetes.io/part-of: coxswain`,
  `coxswain.io/tool-proxy-for: <loop>`, `coxswain.io/tool: <name>`.
  Never `coxswain.io/loop` and never the agent component label.
- **Service** (`<loop>-tool-<name>`), port 8080 TCP, selector = the label
  set, owned by the Loop. Stable across recreates (the agent's env URL
  depends on it).
- **Owned+Ready sandbox gate (D35a pattern, order-independent like the
  I42b/I42c-review gates):** when any tool proxy is expected, the sandbox
  may not be set to Running until **every** expected tool proxy pod exists,
  is `metav1.IsControlledBy` the Loop, and is Ready. Transient read errors
  fail CLOSED (requeue), never skip the gate.
- **Foreign-object gate:** a pod named `<loop>-tool-<name>` not controlled by
  the Loop → `ProxyConflict=True` reason `ForeignToolProxy` + the owned+Ready
  gate holds the sandbox Suspended. The foreign pod is NOT deleted (I2).
- **Drift correction:** `coxswain.io/tool-proxy-spec-hash` annotation
  (hash the full desired pod spec, the `proxyPodSpecHash` pattern); on
  mismatch delete + recreate the pod (the Service name stays stable).
- **Cleanup:** when the tool is no longer in the effective union, delete the
  pod + Service (the `cleanupProxy` direction), and the agent env/netpol
  entries (D41d/D41e) follow via their own gates next reconcile.

**Envtest-first tests (`internal/controller/loop_d41_proxy_test.go`):**
1. **No tools → nothing created.** A Loop whose effective policy has no
   `tools` → no `<loop>-tool-*` pod or Service after reconcile.
2. **One tool → pod + Service.** `tools: [{name: gh, upstream:
   "https://api.github.com", rules: [{methods: [GET], paths: ["/repos/acme/*"]}]}]`
   → pod exists (UID 65535, owned by the Loop, env carries
   `TOOL_UPSTREAM`/`TOOL_RULES_JSON`/`TOOL_POLICY_HASH`, liveness TCP 8080),
   Service exists (port 8080, matching selector), sandbox Suspended
   (pod not Ready yet).
3. **Credential mount.** Same with `credentialSecretRef: gh-cred` → the pod's
   volumes contain the Secret (mode 0444) mounted at `/tool-cred`, and the
   **sandbox pod spec has no volume referencing that Secret** (the
   zero-credential property, envtest-provable half).
4. **Owned+Ready gate: not Ready → Suspended.** Tool proxy pod present, not
   Ready → sandbox Suspended.
5. **Owned+Ready gate: Ready → Running.** Tool proxy pod present, owned,
   Ready (and the model-proxy gate conditions satisfied as elsewhere) →
   sandbox Running.
6. **Foreign pod → ProxyConflict + Suspended.** Pod
   `<loop>-tool-gh` owned by a different controller → `ProxyConflict=True`
   reason `ForeignToolProxy`, sandbox Suspended, foreign pod not deleted.
7. **Drift: spec change → recreate.** Change the tool's upstream (same
   policy generation → new hash via a new AgentPolicy, as in I42b spec 6) →
   the old pod is deleted and a new one created with the updated
   `TOOL_UPSTREAM`; the Service name is unchanged.
8. **Label non-collision.** The tool proxy pod's labels do NOT match the
   agent KubeArmorPolicy selector (`coxswain.io/loop=<loop>`) and do NOT
   include the agent component label (same regression guard as D33/I42b
   spec 7).
9. **Union semantics + conflict fail-closed.** Two referenced policies both
   defining tool `gh` with different upstreams → `PolicyValid=False` reason
   `ToolConflict`, no tool proxy created; identical definitions → one proxy.
10. **Same-Loop update/delete (I43 norm):** after the Running transition,
    (a) update the tool spec (new upstream) and re-reconcile from the API
    server → the pod is updated (spec-hash recreate, new
    `TOOL_UPSTREAM` env); (b) delete the tool from the policy and
    re-reconcile → the pod + Service are deleted and the sandbox can run
    with no tool proxy. Re-read objects from the API server between steps,
    don't trust the in-memory copy.

**Gate mutations (I49 norm, scratch worktree, each must make its spec FAIL):**
- Remove the owned+Ready tool-proxy check from `ensureSandbox` → spec 4 FAILS
  (sandbox goes Running with an unready tool proxy).
- Make the foreign-pod branch not set `ProxyConflict` (or not hold) → spec 6
  FAILS.
- Skip the spec-hash comparison (always treat the pod as matching) → spec 7
  FAILS.

**Acceptance:** envtests green under `make test`; the I44 pin
(`TestProxyComponentSelectorMatches*`) updated in this slice to include the
tool-proxy label set **and fail without it** (mutation: drop
`ComponentToolProxyLabel` from `ProxyComponentSelector` → pin FAILS). The
manager's cache now scopes tool-proxy Pods/Services (no spurious
AlreadyExists create loop on the first reconcile — the I42b kind-acceptance
finding this pin exists for).

**Dependencies:** D41a (image to run), D41b (field + validation), D35a/D33
patterns, C6a union.

---

## D41d — Network + KubeArmor fencing, agent-side wiring

**Scope:** the two-layer egress for the tool proxy (network layer + the
inner fence) and the agent-side egress/DNS/NO_PROXY/env wiring.

- **Tool proxy NetworkPolicy** (`<loop>-tool-<name>-netpol`, per tool;
  `PolicyTypes: [Ingress, Egress]`):
  - **Ingress:** only from this Loop's agent pod (`agentPodLabels(<loop>)`)
    on 8080 TCP.
  - **Egress rule 1:** external `ipBlock {cidr: 0.0.0.0/0, except:
    [10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 0.0.0.0/8, 224.0.0.0/4,
    240.0.0.0/4, 169.254.0.0/16, 127.0.0.0/8, POD_CIDR, SERVICE_CIDR]}` +
    the v6 mirror — the I42c carve-outs, so an upstream that resolves into
    the cluster is blocked at the network layer (agrees with the D41a
    application-layer backstop). No port restriction (the application layer
    owns the request rules; the proxy dials the upstream's port only).
  - **Egress rule 2:** platform DNS (kube-dns 53 UDP+TCP, as I42c).
  - CIDRs from the reconciler's POD_CIDR/SERVICE_CIDR config (not hardcoded;
    envtest spec 5 below pins it).
- **Tool proxy KubeArmor policy** (new emitter
  `EmitToolProxyKubeArmorPolicy`, `internal/engine/kubearmor_i42.go`
  sibling; name `coxswain-<loop>-tool-<name>`):
  - selector = the tool proxy's disjoint label set (component =
    `tool-proxy` + `coxswain.io/tool-proxy-for` + `coxswain.io/tool`);
  - `process.matchPaths` = `[{path: /usr/local/bin/tool-proxy}]`;
  - `network.matchDNSQueries` = the upstream host (+ platform DNS
    `udp`+`tcp`); `spec.action: Block` (the I42f shape).
  - Owned by the Loop; applied through the C6b `Enforcer.Apply` seam under
    the D30 gate (sandbox stays Suspended if the engine is not enforcing);
    a foreign `coxswain-<loop>-tool-<name>` KubeArmorPolicy holds Suspended
    (extend `foreignKaptPolicies`' name set).
- **Agent-side wiring (per tool):**
  - **DNS allowlist:** the agent's KubeArmor `matchDNSQueries` translation
    (C6b's `EmitKubeArmorPolicy` path) gains the tool proxy Service FQDN
    `<loop>-tool-<name>.<ns>.svc.<clusterDomain>` (built from
    `proxyServiceName`-style helpers — **no literals**, R16 I44: the FQDN
    functions are wired at enforcer construction with the cluster domain).
  - **NO_PROXY:** the I42d `NO_PROXY` value gains the tool proxy Service
    name + FQDN per tool (so tool calls go direct, not through the egress
    proxy). No tools → `NO_PROXY` unchanged (guards the I42d specs).
  - **Agent env:** one `COX_TOOL_<NAME>_URL=http://<loop>-tool-<name>.<ns>.svc
    .<domain>:8080` per tool (`COX_` operator prefix, uppercase name).
    `spec.agent.env` may not set a `COX_TOOL_*` name (reserve, like the I42d
    operator proxy env names).
  - **Agent NetworkPolicy** (`<loop>-agent-netpol`): one extra egress rule per
    tool proxy (peer selector = the tool proxy's label set, 8080 TCP).

**Envtest-first tests
(`internal/controller/loop_d41_netpol_test.go`,
`internal/engine/kubearmor_d41_test.go`):**
1. **Tool proxy netpol created.** A Loop with one tool →
   `<loop>-tool-gh-netpol` exists; PolicyTypes include Ingress+Egress;
   ingress peer = agent labels on 8080; egress rule 1 is `ipBlock 0/0`
   with the carve-out list containing the reconciler's POD_CIDR and
   SERVICE_CIDR; egress rule 2 = kube-dns 53.
2. **CIDRs from config.** Change the reconciler's POD_CIDR → the except list
   reflects the new value.
3. **No tools → no tool netpol.** (and the agent netpol has no tool rule).
4. **KubeArmor policy shape.** Given a tool, the emitted KubeArmorPolicy:
   selector matches the tool proxy label set; `process.matchPaths` =
   `[{path: /usr/local/bin/tool-proxy}]`; `matchDNSQueries` includes the
   upstream host; `spec.action: Block`.
5. **Kapt owned by Loop + foreign gate.** After reconcile, the kapt exists
   and is owner-ref'd to the Loop; a kapt with the same name owned by
   something else → sandbox Suspended (`KubeArmorPolicyConflict`-style).
6. **No tools → no tool kapt.** Only the existing agent/model-proxy/egress
   kapt names exist.
7. **Agent DNS allowlist + NO_PROXY + env.** A Loop with one tool → the
   agent KubeArmorPolicy's `matchDNSQueries` includes
   `<loop>-tool-gh.<ns>.svc.<domain>`; the agent container env has
   `COX_TOOL_GH_URL` with the `.svc` URL; `NO_PROXY` includes the tool
   Service name; **the agent env contains no credential** (no env value
   equals or contains the test credential value, and no volume on the agent
   references the tool Secret).
8. **Agent netpol rule.** The agent NetworkPolicy has an egress rule per
   tool proxy with the tool proxy's peer selector on 8080.
9. **FQDN from config, not literals (I44 norm).** Set the reconciler's
   ClusterDomain to `cluster.internal` → the `COX_TOOL_GH_URL` value, the
   `NO_PROXY` FQDN entry and the agent `matchDNSQueries` entry all use
   `cluster.internal`.
10. **Two tools → two of everything** (two netpols, two kapt, two env
    entries, two NO_PROXY entries, two agent-netpol rules; no cross-talk).

**Gate mutations (I49 norm, scratch worktree, each must make its spec FAIL):**
- Drop the tool FQDN from the agent `matchDNSQueries` emission → spec 7
  FAILS.
- Omit the tool rule from the agent NetworkPolicy builder → spec 8 FAILS.
- Skip emitting the tool kapt (gate it off) → specs 4/5/10 FAIL.
- Remove the tool proxy netpol creation → specs 1/2/10 FAIL.

**Acceptance:** envtests green; the I45 admission-policy pin
(`policy_yaml_test.go`) updated to include `tool-proxy` in the component
list — mutation: remove `tool-proxy` from
`config/admission/validating_admission_policy.yaml` → the pin FAILS; the
real manifest gets the value in this slice too (ephemeral containers are
denied on tool proxies).

**Dependencies:** D41c (pods + labels for the selectors), C6b (enforcer
seam), D35a/D30 gates.

---

## D41e — Kind acceptance

**Scope:** `test/e2e/d41-e2e.sh` (+ `make d41-e2e`, pinned to
`--context kind-coxswain-dev`, the i42-e2e pattern). A Loop whose AgentPolicy
declares one tool (an in-kind HTTP "upstream" pod, e.g.
`golang:1.26` serving a fixed `GET /ok` → 200 and everything else 404, with
**no auth of its own** so the injected header is what makes the call
identifiable) + a credential Secret.

Assertions (script + recorded output in the PR):
1. **Allowed path → 2xx.** From the agent pod (or a stand-in pod in the
   agent's network position, if the stand-in agent has no curl):
   `curl http://<loop>-tool-gh.<ns>.svc:8080/ok` with `COX_TOOL_GH_URL`
   → 2xx from the upstream.
2. **Disallowed path → 403 + audit.** `curl .../delete` (a path no rule
   covers) → 403 from the tool proxy; the proxy log contains the audit JSON
   line (`source: "tool-proxy"`, `method`, `path`, `status: 403`, policy
   hash) **and** no credential substring in it.
3. **Credential injected, hidden from the agent.** The upstream pod's log
   shows the request carried `Authorization: Bearer <test-token>`; and
   **the agent pod contains no tool credential**: `kubectl get pod
   <sandbox> -o yaml` (spec: volumes, env, volumeMounts) contains no
   reference to the tool Secret and no env value containing the token;
   `kubectl exec` into the agent and `env | grep -i` for the token value →
   empty; `ls /tool-cred` → does not exist.
4. **The tool proxy can't reach any host but its upstream.** From the tool
   proxy pod: (a) a DNS lookup of a non-upstream external host + connect →
   blocked (KubeArmor `matchDNSQueries` / the network layer); (b) a raw
   connect to the agent pod IP and to the upstream's service on a different
   port → blocked (netpol egress is carve-out + DNS only; assert
   connection-refused/timeout, not success); (c) point the proxy at an
   in-cluster upstream (a scratch policy variant) → the D41a resolved-IP
   check 403s (the netpol carve-out blocks the dial too). Each attempt must
   NOT leave a successful upstream-side log line.
5. **Sandbox gating observable.** Before the tool proxy pod is Ready the
   sandbox is Suspended; after Ready it is Running (the D41c gate, live).
6. **Ephemeral container denied (I45, live).** A
   `kubectl create ephemeralcontainer` on the tool proxy pod is rejected.

**Gate mutation (I49 norm):** run the script against a scratch-build image
with the rule engine disabled (the D41a mutation) → assertions 2 and 3
FAIL (the disallowed path reaches the upstream, and the audit shows an
allowed verdict). Record the output.

**Acceptance:** `make d41-e2e` green on `kind-coxswain-dev`; the output
(log lines, curl statuses, the grep-empty credential checks) committed in
the PR description — the R22 process note: evidence numbers in the PR body
are copied from this artifact, not recalled.

**Dependencies:** D41a–D41d merged; the kind cluster with KubeArmor
(`make kubearmor-e2e`'s profile) for assertion 4a.

---

## Slice order and dependencies

```
D41a (binary) ──┐
                ├──→  D41c (operator: pods, gates, drift)  ──→  D41d (netpol + kapt + agent wiring)
D41b (API) ─────┘                                                        │
                                                                         └──→  D41e (kind acceptance)
```

- D41a and D41b are independent (parallel-safe).
- D41c depends on D41a (image), D41b (field), D35a/D33 + C6a (patterns).
- D41d depends on D41c (labels/selectors) + C6b.
- D41e depends on all of the above.

**Exit criteria** (the ADR's acceptance): ADR written + reviewed; plan
reviewed; kind evidence for all six D41e assertions; the agent pod contains
no tool credential. Out of scope until deferred items are re-opened: a
`ToolProxy` kind, typed tool extensions, shared proxies, credential
hot-reload.
