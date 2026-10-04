# ADR-0008: Tool proxies — credentialed per-tool, per-Loop credential boundary

**Status:** Proposed (records the owner's D41 decisions, round 18; to be
reviewed before any D41 code)
**Date:** 2026-10-04
**Supersedes:** — (extends ADR-0006)
**Extends:** ADR-0006 (the zero-credential rule), ADR-0007 (two-layer
allowlisted egress, streamed audit)
**Origin:** `docs/REVIEW-PHASE1-R17.md` D41 (design + questions) and
`docs/REVIEW-PHASE1-R18.md` "D41 (R17): tool proxies" (the owner's answers).

## Context

ADR-0006 makes the sandbox a **zero-credential** boundary: the model's API key
is held by the per-Loop model proxy, never the agent. Many workflows need
*more* than the model: authenticated external APIs (GitHub, Grafana,
cloud/IaC, ticketing). Today the agent has:

- the **model proxy** (credentialed, single upstream, D33) — the working
  credential-boundary pattern;
- the **egress proxy** (I42) — an allowlist, but **no credentials**; it can
  limit *where* the agent talks to, not *who it is*.

Putting a tool token in the agent (env var, mounted file, or git credential)
breaks ADR-0006's core property: a prompt-injected agent could read and
exfiltrate it through any path the egress proxy allows. The egress proxy is
the wrong place to fix this — it holds no secrets by design and cannot
express per-request auth or per-path scoping.

The model proxy shows the way: a **per-Loop, operator-owned proxy pod holds
the credential, the agent calls a localhost/in-cluster URL, and never sees
the secret.** This ADR generalises that to external tools.

The owner decided (R18) the three open questions from R17:

1. **Where tools are declared: on AgentPolicy.** `spec.tools[]` joins the
   other allows in one policy object, with the existing union semantics
   (`spec.policyRefs`). A separate `ToolProxy` kind comes later, when tool
   credentials are managed centrally.
2. **A proxy per tool, per Loop.** Strongest isolation, matching the model
   and egress proxies today. Sharing (per campaign/tenant, D42 #5) is
   deferred.
3. **A generic HTTP tool proxy as the built-in default** (D36: the default
   is built in, solid externals plug in). Typed extensions (GitHub-aware
   path templates, cloud SDK signers) come later.

## Decision

A **credentialed tool proxy per tool, per Loop.** The agent policy declares
tools; the operator provisions one proxy pod + Service per tool; the agent
reaches the tool through the proxy and never holds the credential.

### API — `AgentPolicy.spec.tools[]`

```yaml
tools:
  - name: github
    upstream: https://api.github.com
    credentialSecretRef:
      name: my-github-cred   # Secret in the policy's namespace
      key: token
    rules:
      - methods: [GET]
        paths: ["/repos/acme/*"]
```

- `name` (required): DNS-1035 label; unique within the effective policy
  union; forms the proxy pod/Service name (`<loop>-tool-<name>`).
- `upstream` (required): a base URL (`scheme://host[:port][/prefix]`). The
  proxy dials only this host, as the **origin** of its own request: it is a
  **reverse proxy**, not a forward proxy (see the binary section — the
  opaque-TLS argument). `https://` means the proxy originates its own TLS
  connection to the upstream and verifies its certificate; `http://` is
  plain.
- `credentialSecretRef` (optional): a `{name, key}` pair — the Secret and
  the key within it — mounted read-only into the proxy pod **only**.
  The proxy injects the value into requests (header form is the typed
  extension's job; the Phase 1 generic proxy uses one
  `Authorization: Bearer <value>` header, replacing any agent-supplied
  `Authorization` / `Proxy-Authorization`). A tool entry without it is a
  credential-less passthrough.
- `rules[]` (required, ≥1): each rule is a set of allowed HTTP **methods**
  (GET/POST/PATCH/PUT/DELETE) plus allowed **path prefixes** (matched
  against the request path *after* the upstream's base prefix). Anything
  matching no rule → **403 + an audit record**.
- Validation (CEL + controller, I42e pattern): upstream must be
  `http(s)`; an in-cluster upstream (`.svc` / cluster.local suffixes,
  IP literals in the pod/service CIDRs, loopback) is rejected with
  `PolicyValid=False` reason `ToolUpstreamInCluster` (an in-cluster tool
  upstream is the SSRF/exfiltration path, and it defeats the point —
  the agent can already name in-cluster Services directly through no proxy).

### The generic HTTP tool proxy (`cmd/tool-proxy`)

One binary, one upstream, one credential file, one rule set — the model
proxy generalised from "single endpoint, forward all" to "rule-checked
forwarding with credential injection":

- Listens on `:8080` (env `TOOL_PROXY_PORT`, default 8080). Reads rules +
  upstream + policy hash from a mounted file (`TOOL_POLICY_JSON`) or env
  (Phase 1 simplification, as in I42a).
- **Reverse proxy, not a forward proxy.** A forward proxy's `CONNECT` tunnel
  to an `https://` upstream is opaque TLS: the proxy cannot see the method
  or path and cannot inject the credential, so the request rules and the
  credential boundary would not exist for the common case. Instead the
  agent speaks **plain HTTP, origin-form** in-cluster to
  `COX_TOOL_<NAME>_URL + path` (no `Host` override of the upstream, no
  absolute-form, no `CONNECT`); the proxy rule-checks, strips
  agent-supplied `Authorization` / `Proxy-Authorization`, injects the
  credential (read from the mounted Secret file; **never logged, never
  echoed into an audit record, never returned to the agent**), and
  **originates its own request** to the fixed upstream — TLS with the
  upstream's certificate **verified** for `https://` (a MITM of the
  upstream is the attacker's goal, not the proxy's). The upstream is fixed
  by config: there is no per-request authority to validate, so the rules
  engine sees the real method and path of every upstream request.
  - `CONNECT` and absolute-form requests → `405 Method Not Allowed`, no
    upstream dial (they would bypass the rules or redirect the dial).
  - The path is **normalised before matching** (rejects/`400`s `..`,
    `%2e%2e`, `%2F`, `//`, backslash and NUL encodings) and matched by
    **segment prefix** — `/repos/acme/` covers `/repos/acme/x` but not
    `/repos/acmer` — so no encoding trick can route a request past a rule.
    A rule prefix without a trailing `/` matches only the **exact** path
    (e.g. `/repos/acme` matches only `/repos/acme`, not `/repos/acme/x`);
    use a trailing `/` for prefix semantics.
  - method + path match a rule → forward to the resolved upstream IP
    (same resolved-IP carve-outs as I42a: a tool upstream must not resolve
    into the cluster — the check applies before dialing, and the dial is to
    the resolved IP with no re-resolution);
  - no match → `403 Forbidden`, no upstream dial.
- **No redirect following.** A `3xx` response from the upstream is returned
  to the agent **as-is**; the proxy does not re-resolve, re-check or dial
  the `Location` target. (A redirect to another server is that server's
  problem to receive *nothing*: the agent holds no credential and its own
  egress is fenced, but the proxy itself never dials a second host.)
- **Audit on stdout, one JSON line per request (allowed and blocked)**, with
  the `405` and `400` (normalisation) rejections audited like 403s,
  the Q4 envelope fields: `{time, loop, namespace, source:
  "tool-proxy", action: "request", tool: <name>, method, path, status,
  policy: <hash>}`. The credential is **redacted** (absent from the record
  by construction — the proxy logs method/path/status only, never
  headers or the credential).
- The proxy is the **TLS client** to the upstream (it originates and
  verifies; it never terminates or MITM the upstream's cipher). No secrets
  other than the one tool credential. No SA token. Read-only rootfs.
  UID 65535 (distinct from agent 65532, model proxy 65533, egress proxy
  65534).

### Two-layer allowlisted egress (per I42, per tool proxy)

- **Network layer.** A NetworkPolicy for the tool proxy pod: ingress only
  from this Loop's agent pod (on 8080); egress only to (a) the upstream
  host's resolved address space — i.e. the standard external `ipBlock`
  0/0 with the I42c carve-outs (private ranges + pod/service CIDRs) so an
  upstream that resolves into the cluster is blocked at the network layer
  too, plus (b) platform DNS. An I42f-style **KubeArmor policy** is the
  inner fence: process block allows only the tool proxy binary;
  `matchDNSQueries` = the upstream host + platform DNS `udp`+`tcp`;
  `spec.action: Block`. The I42a resolved-IP carve-outs are the
  application-layer backstop.
- **Application layer.** The request rules restrict *what* the proxy may do
  at the upstream. Every request is audited (method, path, status, policy
  hash, credential redacted) — the streamed-audit property of ADR-0007.

### Agent side

- The tool proxy Service FQDN (`<loop>-tool-<name>.<ns>.svc`) goes on the
  agent's **DNS allowlist** (the KubeArmor `matchDNSQueries` translation
  gains one entry per tool) and into **`NO_PROXY`** (D39/I42d) so the agent
  talks to the tool proxy directly, not through the egress proxy.
- The agent container env gains one entry per tool:
  `COX_TOOL_<NAME>_URL=http://<loop>-tool-<name>.<ns>.svc.<domain>:8080`
  (the `COX_` prefix is reserved for operator-set env; the name is the
  uppercased tool name). The agent has no other way to learn the tool's
  credential.
- The agent NetworkPolicy gains an egress rule per tool proxy (peer
  selector = the tool proxy's disjoint label set, port 8080 TCP).

### Platform plumbing (same as the other proxies)

- New **`component=tool-proxy`** label value (`policy.ComponentToolProxyLabel`),
  in the tool proxy's **disjoint label set**:
  `app.kubernetes.io/component: tool-proxy` +
  `coxswain.io/tool-proxy-for: <loop>` +
  `coxswain.io/tool: <name>` — never `coxswain.io/loop` (the agent
  KubeArmorPolicy selector) and never the agent's component label.
- Added to **`policy.ProxyComponentSelector`** so the manager's scoped Pod/
  Service cache includes tool proxies (the I44 pin test,
  `TestProxyComponentSelector*`, gains a tool-proxy label set).
- Added to the **I45** ValidatingAdmissionPolicy component list (ephemeral
  containers are denied on tool proxies too), pinned by the I45 YAML test.
- **Foreign-object gates and drift correction** exactly as for the other
  proxies: a foreign `<loop>-tool-<name>` pod → `ProxyConflict=True`
  reason `ForeignToolProxy` (Sandbox holds Suspended via the
  owned+Ready gate, never Running); the owned+Ready gate (D35a pattern)
  blocks the sandbox from Running until every expected tool proxy pod is
  owned by the Loop and Ready; a spec-hash annotation
  (`coxswain.io/tool-proxy-spec-hash`) drives delete-and-recreate on
  drift (I42b review P3 pattern: hash the full desired pod spec);
  foreign tool-proxy netpols / KubeArmorPolicies hold Suspended via the
  existing order-independent gates extended to the new name sets.
- `PolicyTranslationLossy` is **not** set on the grounds of tool upstreams
  (the proxy enforces host at DNS + application layer, as in I42d/I41).

### Audit

Tool proxy audit records are streamed to the proxy pod's stdout like the
egress proxy's (collected by the same log path); the `policy` field is the
effective policy hash so a record is attributable to the exact policy
generation that produced it. The credential value never appears in any
record or log (unit-tested: the audit line for a credentialed request
contains no substring of the test credential).

## Consequences

- The agent can use authenticated external tools **without holding a
  credential**, extending ADR-0006's zero-credential property to tools:
  "every capability the agent needs is a local file or a localhost proxy
  that holds the real credentials" now covers tool APIs, not just the
  model.
- Pod fan-out: N tools → N proxy pods + Services + netpols + KubeArmor
  policies per Loop. The MVP cost is real but bounded by the same union
  semantics as network allows; sharing (D42 #5) is the scale-out path.
- D46's agent exec fencing stays off (owner decision (c)); per-tool proxies
  are the follow-on that, when the agent is restricted to tools-only
  (`COX_TOOL_*_URL` calls, no general shell), makes exec fencing
  meaningful again. The tool proxy does not require that — it is safe under
  today's unrestricted-shell agent too.
- The agent env surface grows (`COX_TOOL_*_URL`) and the NO_PROXY / DNS
  allowlist grow with tool count; both are derived from the effective
  policy, so drift is caught by the same spec-hash pattern.
- `tools[]` on AgentPolicy is a Phase 2-adjacent surface shipped in the
  D41 slices; the `ToolProxy` kind (central credential management, reuse
  across Loops) is deferred (below).

## Deferred (explicitly NOT in D41)

1. **`ToolProxy` kind** — a separate resource owned by a platform team for
   centrally managed tool credentials, reusable across Loops.
2. **Typed tool extensions** — GitHub-aware path templates, OAuth/token
   refresh, cloud SDK request signers. The generic HTTP proxy is the
   default; typed proxies plug in as additional images behind the same
   plumbing.
3. **Shared proxies** (per campaign / per tenant, D42 #5) — revisit with
   the D42 load-test numbers.
4. **Credential rotation semantics** beyond delete-and-recreate (a hot
   reload of the mounted Secret file is not guaranteed; the pod-spec hash
   recreates on Secret change only when the operator observes it — the MVP
   recreates on policy change and on manual pod deletion).

**Acceptance** (mirrors R17 D41, restated as the plan's exit criteria):
an ADR (this one) is written and reviewed; the plan has test-first slices;
kind evidence shows an allowed tool path → 2xx, a disallowed path → 403 with
an audit line, the tool proxy can't reach any host but its upstream, and the
agent pod contains no tool credential (grep the pod spec + env).
