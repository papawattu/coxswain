# SDLC sample apps — plan

Owner decisions (2026-10-02):

1. **Target:** small sample apps plus demo runs — apps that a Loop can
   realistically take from a failing state to `Succeeded` on the local model.
2. **Location:** the app sources live in `examples/` in the coxswain repo
   (seed material). The Loop itself clones from an **in-cluster git server** —
   never from coxswain's GitHub remote, and the sandbox must never be given a
   remote it can push to coxswain's repo (see §2).
3. **Model:** the local vLLM at `http://192.168.1.20:8000` (OpenAI-compatible,
   model id `qwen3.8-27b`). Verified reachable from devbox 2026-10-02:
   `GET /v1/models` lists `qwen3.8-27b` and `qwen3.8-27b-pi8`, and a
   `POST /v1/chat/completions` with `{"model":"qwen3.8-27b", ...,
   "chat_template_kwargs":{"enable_thinking":false}}` returns a normal
   completion (no auth header sent — the server does not require one; we still
   mount a dummy key, §4).
4. **The bar (owner acceptance for the samples build):** at least **one task
   end to end on kind with the real vLLM: Planning → plan approval →
   Implementing → Verifying, with evidence**. Consequence: the operator-side
   gaps that block that bar (§3, GAP 1–5) are **in scope** for the build
   slices (§7, S3–S5); only delivery (branch push + PR to the in-cluster
   Gitea) stays a follow-up.

Everything below is **verified against the code at `d6774cf` (origin/main,
2026-10-02)** unless marked **GAP**. File/function citations are for that
commit.

---

## 1. The sample apps

Three apps, one per language/toolchain, each small enough that
`qwen3.8-27b` can close the gap in a few iterations:

| App | Type | Seed state (the Loop's job) | Acceptance checks (the ONLY gate to `Succeeded`) |
|---|---|---|---|
| `examples/gocli` | Go CLI | A working CLI with **one deliberately failing test** (e.g. `round.go` returns the wrong value; `go test ./...` red) | 1. `go build ./...` 2. `go vet ./...` 3. `go test ./...` |
| `examples/pylib` | Python library | A small `numutils` package; one public function is missing (test suite imports it and fails) | 1. `python3 -m compileall -q .` 2. `python3 -m unittest discover -s tests` |
| `examples/webapi` | Tiny Go web API | `net/http` server with routes `/healthz` (works) and `/api/v1/ping` (**missing**); a smoke test script curls both | 1. `go build ./...` 2. `bash test/smoke.sh` (starts the server on a port, curls `/healthz` and `/api/v1/ping`, expects 200 + the fixed body) |

Design rules:

- **The failing seed is committed, not generated at runtime.** Each app ships
  with the broken/missing state as its initial commit on the in-cluster git
  server, so `status.baseCommit` pins a state where the checks fail.
  `status.baseCommit` is operator-set today only in the B-slice envtest specs;
  from this build it is pinned by the workspace-materialisation init container
  (GAP 1, §3.1), which resolves `spec.workspace.ref` to a SHA after cloning.
  The Loop's goal is the issue-style description ("make `go test ./...` pass;
  do not weaken the tests"), and `verify.acceptanceChecks` +
  `verify.protectedPaths` (the test files) make the "no cheating" boundary
  explicit. `verify.preset` is `go` for gocli/webapi and `none` for pylib
  (the `go` preset would protect go files; pylib has none —
  `docs/REVIEW-PHASE1` D16, `api/v1alpha1/loop_types.go` `VerifyConfig`).
- **Seeded tasks.** Each app carries 2–3 tasks in
  `examples/<app>/tasks.md`: an issue-style goal, the Loop spec fields
  (`goal`, `verify` block, `policyRefs`, `agent` block) to paste, and the
  expected evidence. Task 1 per app is the failing-test fix above; the
  others are one feature each (e.g. a `--json` flag on gocli, a `Median`
  function on pylib, a `/api/v1/echo` route on webapi).
- **No network needed by the app or its tests.** The agent's only external
  dependency is the model (through the proxy); `gocli`/`webapi` use the
  stdlib only, so no `go get`/proxy fetches are needed (the platform minimum
  policy already allows the model endpoint; adding a network allow for
  proxy.golang.org is an optional hardening demo, §5).

## 2. Where the repos live (and how the Loop never touches coxswain's GitHub)

**Verified:** `Workspace` is `repo` + `ref` (+ optional
`gitCredentialSecret`): `api/v1alpha1/loop_types.go` `Workspace`. The repo
URL pattern accepts `https://`, `ssh://`, and scp-style remotes. **The
workspace is the seed state the Loop clones — there is no operator-side
clone/push today (GAP 1, §3.1).**

**Plan: an in-cluster git server in a `samples` namespace.**

- `config/samples/` (new kustomization, deployed by a `make samples-up`
  target on `coxswain-dev`): a Deployment running `gitea/gitea` (single
  container, `GIT_HTTP_ENABLE` + `SERVICE_TCP_LISTEN_PORT=3000`, storage on an
  emptyDir — no PVC needed; the sample is disposable). Gitea over plain HTTP
  (`http://gitea.samples.svc:3000/<repo>.git`) so the sandbox's clone works
  with `gitCredentialSecret` (a basic-auth user, created by an init/seed
  Job) — no SSH keys in kind.
  - *Alternative considered:* `git-http-backend` (the `git-http-backend`
    binary serving bare repos from a Deployment + `git update-server-info`
    seed Job). Fewer moving parts, but no HTTP clone of non-bare repos and
    no easy push-without-auth; Gitea is the safer default, `git-http-backend`
    is the fallback if the Gitea image pull fails on an offline host.
- **Seeding:** `examples/<app>/` in this repo is the seed source. A `make
  samples-seed` target tars each `examples/<app>` and `kubectl cp`s it into a
  Gitea `init` Job (or an admin-API `POST /api/v1/user/repos`) which pushes it
  as repo `<app>` with an initial commit `initial`. The in-cluster repos are
  then the **only** remotes the Loops can see.
- **Never coxswain's GitHub remote.** The seeded repos point nowhere near
  `github.com/papawattu/coxswain`. Defence in depth:
  1. The Loop spec's `workspace.repo` is the `gitea.samples.svc` URL — there
     is nothing in the sandbox that names coxswain's remote.
  2. The agent's egress is allowlisted through the egress proxy (ADR-0007
     I42): with a policy that has **no network allows** (or only the model
     endpoint, which is *not* a network allow), external egress is denied at
     the proxy (`docs/TDD-PLAN-PHASE1.md` I42a–f; `internal/egress`).
     `github.com` is therefore unreachable from the agent even if it tried.
  3. The model proxy's netpol egress is exactly the model endpoint peer +
     DNS (§4) — the *operator's* path to the model is also closed to
     anywhere else.

## 3. The SDLC flow as it exists TODAY (verified, with gaps)

The phase enum (`api/v1alpha1/loop_types.go` `LoopPhase`):
`Pending;Planning;AwaitingApproval;Implementing;Verifying;Succeeded;Failed;
Paused;CleaningUp`. What actually moves a Loop today, and the gaps the
samples build must close to meet the owner's bar:

1. **Admission/creation.** `Reconcile` (`internal/controller/loop_controller.go:191`)
   validates the referenced `AgentPolicy` objects (`validateAgentPolicies`),
   applies the D30/D38 gates (`applyEffectivePolicyAndConditions`), requires
   the `endpointSecretRef` + `modelEndpoint` pair (`ModelConfigValid` check),
   creates the Sandbox (`ensureSandbox`), the per-Loop model proxy pod +
   Service + NetworkPolicies (`ensureProxyAndNetPolicies`, D33/D34), and the
   egress proxy when network allows exist (`ensureEgressProxy`). A fresh Loop
   is set to `Pending` (`loop_controller.go:268`).
   **GAP 1 — nothing materialises the workspace.** `ensureSandbox` builds the
   sandbox pod with an **empty** `workspace` emptyDir volume and an agent
   container whose `Command` is `["sh", "-c", "sleep infinity"]`
   (`loop_controller.go:703`, comment: "Keep the container alive until the
   phase driver (Phase 1) takes over. sleep infinity is a stand-in."):
   - nothing clones `workspace.repo`@`ref` into the sandbox (no init
     container, no clone code; `workspace.repo`/`ref` have **no reader** in
     `internal/`/`cmd/` either, not just `gitCredentialSecret`);
   - the goal is never passed to the agent (no operator-set goal env);
   - a runner image set via `spec.agent.image` would have its entrypoint
     **overridden** by `sleep infinity`, so "the agent actually called the
     model" cannot happen;
   - `status.baseCommit` is never set by any non-test code, so §1's
     "baseCommit pins a state where the checks fail" is unsupported today.
2. **Phase machine.** The operator is claim-driven: `nextPhase(current,
   observed)` (`loop_controller.go:812`) advances **exactly one step** —
   `Pending→Planning→Implementing→Verifying` — when
   `status.observedPhase` is the immediate next phase. It **never returns
   `Succeeded`/`Failed`**; exits out of `Verifying` are evidence-gated (see 4).
   `AwaitingApproval` is in the enum but **nothing drives it, and
   `nextPhase` cannot even land there** — the switch only returns
   `Planning`/`Implementing`/`Verifying`, so an `observedPhase` of
   `AwaitingApproval` is ignored (`loop_controller.go:812`).
   **GAP 2 — the runner does not drive the phase machine.** The runner
   (`runner/runner.go`) is a one-shot conformance agent: it takes a prompt,
   drives the model via an OpenAI-compatible `/chat/completions` endpoint
   with a single `shell` tool, and writes `result.json` (schema:
   `status success|blocked`, `summary`, `filesChanged`, `lessons`, …).
   **It does not read `.coxswain/desired-phase`, and its `result.json` schema
   (`runner/runner.go` `Result`, line ~87) carries no `observedPhase` field.**
   The TDD-PLAN contract ("A. Runner as phase driver", A1–A4) is un-built:
   the runner is supposed to read `desiredPhase` from
   `.coxswain/desired-phase`, do that phase's work, and write `result.json`
   carrying the reported `observedPhase` — **and the operator side of that
   channel is missing too**: ADR-0004 says the operator reads `result.json`
   (via `pods/exec`) and **it** writes `loop.Status.ObservedPhase`, but there
   is no code in `internal/controller/` that reads `result.json` (grep for
   `result.json` in non-test controller code finds only comments);
   `status.observedPhase` is written today only by envtest specs. So the
   ADR-0004 reader must be **added**, not merely cited. Until that lands, no
   sample run can drive the phase machine past `Pending` on its own;
   `nextPhase` is proven only by envtest specs that set
   `status.observedPhase` directly.
   **GAP 3 — no plan-approval gate.** `AwaitingApproval` is un-reachable
   (see 2); the approval gate is Phase 4 in `docs/TDD-PLAN-PHASE1.md`
   ("Out of scope for Phase 1"): *the approval gate is Phase 4; Phase 1 uses
   `spec.approval.mode: Auto` only*. There is no `spec.approval` field on
   `LoopSpec` today. The build adds the **minimum gate** that makes
   `AwaitingApproval` real (§7 S4); because it is Phase-4 scope, this is
   flagged as an **owner question** in the PR body (option A: minimal gate
   now — the default; option B: Auto approval, bar lowered to
   "Planning → Implementing → Verifying").
3. **Verify (the evidence path).** At `Verifying`, the operator's
   `tamperVerdict` (`loop_controller.go:873`, B2/D24) is a tri-state over the
   operator's OWN evidence — the verify Job's tamper init-container exit code
   bound to `status.currentVerify.verifiedCommit` (D11/D27): `Tampered` →
   terminal `Failed:TamperedVerify` **before any check runs**. `Clean`/
   `Unknown` is not a decision — B3 (the check-container exit codes →
   `Succeeded`/iterate/`Failed`) is the gate to `Succeeded`, and **the
   verify Job itself is not created by the operator today (GAP 4, B3)** —
   envtest specs set `status.verify.*` directly (stated in the
   `VerifyStatus` doc comment: "in envtest (no Job controller) the B-slice
   tests set them directly"). The build brings the B3 verify Job into scope
   (§7 S5) — it is the **preferred, trusted evidence path** for the
   `Verifying` evidence the owner's bar requires; the operator-side check Job
   in §6 remains as the *collection* mechanism for `EVIDENCE.md`, not as a
   substitute for B3.
4. **Delivery — what it does TODAY: nothing (GAP 5).** A Loop ends in
   `Succeeded` with a `status.iteration` count; **the operator never pushes a
   branch, never opens a PR, and never writes delivery state (GAP).**
   `docs/PLAN.md` Phase 1 promises "hands back a draft PR";
   `docs/TDD-PLAN-PHASE1.md` puts "PR + Judge" in **Phase 6**.
   `spec.workspace.gitCredentialSecret` is declared (`Workspace` in
   `api/v1alpha1/loop_types.go`) but has **no reader in `internal/` or `cmd/`
   today (GAP)** — no code clones, commits, or pushes. For the samples,
   "delivery" therefore means: the acceptance checks pass **inside the
   sandbox** (via the B3 verify Job, §3.3) and the evidence (phase,
   iteration, verify exit codes, result.json) is collected from the cluster
   (§6). **Delivery (branch push + PR to the in-cluster Gitea) is the
   follow-up, out of scope for the build slices.**

**Implication for the sample demo, after the build (stated plainly):** the
build closes GAP 1–4, so a sample run proves (a) the sandbox+proxy+netpol+
policy plumbing comes up correctly around a *real* agent run against the
*real* local model, (b) the phase machine moves **Planning → plan approval →
Implementing → Verifying** with the runner as driver and the B3 verify Job as
evidence, and (c) the acceptance checks pass on the final state. It does
**not** prove delivery (GAP 5 stays: no push, no PR) — the demo is framed
accordingly: "a Loop on a sample repo, real model, isolated network, checks
pass through plan approval" — not "an SDLC that ships a PR."

## 4. Model wiring (verified)

- **The pair, together and immutable.** `AgentConfig`
  (`api/v1alpha1/loop_types.go`): CEL `has(self.endpointSecretRef) ==
  has(self.modelEndpoint)` (set together); `endpointSecretRef` is
  `self == oldSelf` (immutable, proxy `spec.volumes`); `modelEndpoint` is
  `self == oldSelf || self == ''` + host:port pattern + port 1–65535.
  The controller additionally rejects a Secret without an endpoint
  (`ModelConfigValid` / `MissingModelEndpoint`, `loop_controller.go:246`).
- **The model proxy pod (D33).** `buildProxyPod`
  (`loop_controller.go:1064`): a bare Pod, `MODEL_ENDPOINT` env =
  `modelEndpointValue(loop)` (`loop_controller.go:2137` — a bare
  `host:port` becomes `http://host:port`, so `192.168.1.20:8000` →
  `http://192.168.1.20:8000`, which is exactly the vLLM address). The
  `endpointSecretRef` Secret is mounted read-only at `/model-creds`; the
  stand-in proxy (`cmd/proxy-standin/main.go`) **fatals at startup if no
  readable key file exists**, so the Secret must carry at least one entry —
  vLLM needs no key, so the Secret holds `apiKey: "vllm-no-auth"` (dummy).
  The proxy forwards `:8080 → MODEL_ENDPOINT` (reverse proxy).
- **The agent side.** `COX_MODEL_BASE_URL` is set by the operator to the
  proxy Service URL (`ensureSandbox`, `loop_controller.go:653`); a Loop
  cannot override it (`COX_*` names rejected, I34). The agent talks to
  `http://<loop>-proxy.<ns>.svc:8080`; the key never reaches the agent.
  **Path convention (S3, 2026-10-02):** the base URL carries the **bare**
  `http://<proxy>:8080` (no `/v1`), and the runner appends
  `/v1/chat/completions` itself (`runner/runner.go`, pinned by
  `TestRunnerPostsV1ChatCompletionsPath`). vLLM serves its OpenAI-compatible
  API under `/v1`; the stand-in proxy is a transparent reverse proxy that
  does NOT rewrite the path, so the full path must reach the proxy — posting
  to `<base>/chat/completions` 404s on vLLM. The model name
  (`spec.agent.model`, sent as the request's `model`) must be one the
  upstream actually serves: `qwen3.8-27b` on the local vLLM
  (`curl http://192.168.1.20:8000/v1/models`). The local Qwen vLLM serves a
  thinking model: the runner sends
  `chat_template_kwargs: {"enable_thinking": false}` (via the runner's
  `-extra-body` flag) or the model's tool calls land in the reasoning output
  and the loop cannot make progress.
- **NetworkPolicy reachability to `192.168.1.20:8000` — yes, it is allowed,
  and it is an ipBlock rule (verified).** `ensureNetworkPolicy`
  (`loop_controller.go:1669`) builds the proxy netpol egress as
  `dnsPeer()` (kube-dns) **plus**, when `spec.agent.modelEndpoint != ""`,
  one rule via `modelPeer(endpoint)` (`loop_controller.go:2082`) + the
  endpoint port: `modelPeer` returns an `IPBlock` of `192.168.1.20/32` for a
  bare IP host. The peer is an `IPBlock` with **no `except` list** — the
  carve-outs (POD_CIDR/SERVICE_CIDR, `egressCarveOutCIDRs`,
  `loop_controller.go:2058`) apply to the **egress proxy**'s `0.0.0.0/0`
  external rule, not to the model-endpoint rule. So the proxy pod may reach
  `192.168.1.20:8000` (off-cluster RFC1918) and nothing else.
- **Which cluster: `coxswain-dev`** (kindnet; `make kind-up` +
  `make deploy-dev` = `config/dev` = base + `--allow-unenforced` +
  `--allow-unenforced-network` + `POD_CIDR`/`SERVICE_CIDR` env —
  `config/dev/kustomization.yaml`). Requirements for the run:
  1. **Routability.** The kind node must route to `192.168.1.20`. kind
     containers have default routing via the host (kindnet bridges), so
     `192.168.1.20` is reachable from the nodes as long as the host
     reaches it — verified from devbox 2026-10-02. If kind's host network
     changes, the run fails loudly at the proxy (connection timeout).
  2. **The runner as agent image.** `spec.agent.image` is honoured
     (`loop_controller.go:639`); when empty the default is
     `golang:1.26` (`sandboxImage`, `loop_controller.go:2332`). But as of
     today the container `Command` is overridden to `sleep infinity`
     (GAP 1) — so merely setting `spec.agent.image` to a runner image does
     **not** run the runner; the build's agent-execution change (§7 S3)
     makes the operator run the image's entrypoint when the agent image is
     the runner. No Dockerfile/manager flag selects the runner today
     (GAP) — a build PR ships a **runner agent image** (small Dockerfile:
     `golang:1.26` base + the compiled runner binary + git + the sample
     toolchains) and sets `spec.agent.image` on the sample Loops. The runner
     needs `GIT_TERMINAL_PROMPT=0`-style hygiene, but no credentials
     (push is not done by the agent, §3.4).

## 5. Gates a sample run passes (on `coxswain-dev`)

| Gate | Status on dev | Meaning for the demo |
|---|---|---|
| D30 eBPF/PolicyEnforced (`--allow-unenforced`) | `PolicyEnforced=False`, reason `EnforcementDisabled` (`enforcementStatus`, `loop_controller.go:368`) | The agent fence is **not** enforced; the demo must not claim eBPF isolation. The plumbing (KubeArmorPolicy emission etc.) is not exercised on dev — it is exercised on the `coxswain-calico`/KubeArmor profiles. |
| D38 CNI NetworkEnforced (`--allow-unenforced-network`) | `NetworkEnforced=False`, reason `EnforcementDisabled` (`cniNetworkStatus`, `loop_controller.go:389`) | kindnet does not police pod→host-network egress; the escape hatch lets Loops run. The demo must not claim CNI enforcement. |
| NetworkPolicy (D34) | **LIVE on dev** (built regardless of the flags) | The real, demonstrated isolation: agent egress = this Loop's proxy `:8080` + DNS (+ egress proxy `:3128` only when network allows exist); proxy egress = `192.168.1.20/32:8000` + DNS. This is the strongest network claim the demo can make. |
| AgentPolicy (C6a/C8) | Union computed, `status.policy.effectiveHash` recorded; a missing referenced policy **errors and holds the sandbox** | Each sample task pins an `AgentPolicy`; the hash in `status` is the audit evidence (D32). |
| Verify tamper gate (B2/D24) | Wired; `TamperedVerify` is terminal | The protected-path (test-file) boundary is real and demo-able: tampering a protected path ends `Failed:TamperedVerify` before any check runs, even once B3 lands. |

## 6. `make sample-run APP=<app> TASK=<n>` design

```
sample-run:        ## samples/<app> task <n> on coxswain-dev; collect evidence to .samples/<app>-<n>/
  @ samples-up (idempotent: cluster+gitea+operator)
  @ samples-seed APP=<app>            # push examples/<app> to gitea repo <app> (fresh initial commit)
  @ kubectl -n samples create ... Loop <app>-task<n>  # spec from examples/<app>/tasks.md task <n>
  @ watch loop phase/status/conditions + sandbox pod logs (timeout, e.g. 30m)
  @ plan-approval gate (option A, the default): when the Loop reaches
    AwaitingApproval, surface .coxswain/PLAN.md and the pending approval to
    the operator; the operator applies the approval (the gate's mechanism,
    §3.2 GAP 3) and the Loop proceeds to Implementing — the approval itself
    is evidence (who/what/when, recorded in the run log)
  @ collect evidence:
       - loop object (status.phase/iteration/verify/conditions/policy)
       - proxy + egress-proxy pod status + netpols (kubectl get netpol -o yaml)
       - sandbox pod logs (the agent transcript), result.json (kubectl cp / exec cat)
       - the B3 verify Job's container exit codes (tamper + each check)
       - the acceptance checks re-run IN THE CLUSTER against the final state:
         a small Job in the samples namespace mounting the same workspace
         clone (the seed Job image, git at the loop's branch head) that runs
         verify.acceptanceChecks and records exit codes to a mounted emptyDir
       - the model-proxy audit (forwarded request count) from its logs
  @ write .samples/<app>-<n>/EVIDENCE.md (a machine-generated summary:
    phase reached (incl. AwaitingApproval + approval), iterations, per-check
    exit codes, policy hash, netpol rules, gate conditions with reasons)
```

Evidence collection is **operator-side** (kubectl/Job), never agent-side —
matching the trust model (the agent's `result.json` is a claim; the B3
verify Job's exit codes and the acceptance-check exit codes re-run by a
trusted Job are the evidence, exactly the B3 contract the verify Job
implements).

## 7. Slicing for the build PR(s)

The owner's bar (2026-10-02): **one task end to end on kind with the real
vLLM: Planning → plan approval → Implementing → Verifying, with evidence.**
Slices S3–S5 bring the operator-side gaps (§3 GAP 1–4) into scope; only
delivery (GAP 5) stays a follow-up.

| Slice | Contents | Acceptance |
|---|---|---|
| **S1 — sample app sources** | `examples/gocli`, `examples/pylib`, `examples/webapi` with the failing seed states, tests, `tasks.md` (2–3 tasks each with goal + `verify`/`agent`/`policyRefs` spec blocks) | `make test`-style sanity in CI: each app's acceptance checks **fail** on the seed state and **pass** after applying the task's reference patch (a CI job runs the checks twice: seed state, then `git apply` the reference fix). No cluster needed. |
| **S2 — in-cluster git + seed** | `config/samples/` (Gitea Deployment + Service + seed Job), `make samples-up` / `make samples-seed` (coxswain-dev only, `--context kind-coxswain-dev`) | On `coxswain-dev`: `make samples-up` leaves Gitea Ready and all three repos cloneable over HTTP with the seeded credential; a `git clone http://gitea.samples.svc:3000/gocli.git` from a throwaway pod succeeds. |
| **S3 — workspace materialisation + agent execution + runner image** (GAP 1, §3.1) | (a) `ensureSandbox` gains a workspace **init container** (trusted seed image, git + toolchains) that clones `workspace.repo`@`ref` into the `workspace` volume using `gitCredentialSecret` (mounted into the init container only — never into the agent container, ADR-0006), and the operator records `status.baseCommit` from the resolved clone; (b) **agent execution:** the operator sets a `COX_GOAL` env (the I34-protected `COX_*` namespace) carrying `spec.goal`, and when the agent image is the runner the container `Command` is the runner's entrypoint — **not** `sleep infinity`; `sleep infinity` stays only as the **explicit opt-out** for agent images that are not the runner (e.g. a manual debugging pod or the `golang:1.26` default dev image) — anything else that depends on the stand-in today (no specs in-tree do; verified) is called out in the change PR; (c) the **runner agent image** (`cmd/runner/Dockerfile`: `golang:1.26` + compiled `runner` + `git` + `python3` for pylib) and `make runner-build` + `kind load` in `samples-up`. **Credential Secret shape:** the Secret named by `workspace.gitCredentialSecret` MUST be a standard `kubernetes.io/basic-auth` Secret carrying the keys `username` and `password` (the same shape as the samples `samples-git-cred`, `config/samples-git/secret.yaml`). The operator mounts the Secret volume with `items: [{key: 'username', ...}, {key: 'password', ...}]` (the two keys at `/workspace-creds/username` and `/workspace-creds/password`, read-only) and passes the credential to git via `http.extraHeader`: only the `fetch` invocation carries `-c http.extraHeader="Authorization: Basic $AUTH"`, where the script builds `AUTH` from the mounted files (`printf '%s:%s' username password | base64`). Nothing is written and nothing is persisted — a `-c` flag lives only for that one command (the git credential helpers are not usable here: `store` writes a lock and, on success, ERASES the entry in its file, so the read-only mount fails it with 'unable to get credential storage lock: Read-only file system'; the inline `!sh -c` helper form is not parsed by busybox ash; and `store` must NEVER be pointed at the agent's workspace, where it would persist the credential). A SubPath mount of a missing key makes the kubelet mount an empty DIRECTORY — the items mapping names the keys explicitly and fails loud at pod start instead | Envtest: the built Sandbox spec carries the init container (with the credential Secret, and the Secret is NOT mounted into the agent container), the `COX_GOAL` env = `spec.goal`, and the agent `Command` is the runner entrypoint when `spec.agent.image` is the runner; a non-runner image still gets `sleep infinity`. Kind: with a seeded Gitea repo, a runner sandbox pod clones the repo (the init container log shows the clone; `status.baseCommit` = the seeded `initial` commit SHA) and the runner executes the goal (sandbox log shows the model loop; `result.json` appears). |
| **S4 — runner as phase driver + ADR-0004 reader + plan-approval gate** (GAP 2 + 3, §3.2) | (a) **Runner side (TDD-PLAN A1–A4):** the runner becomes a phase driver — reads `desiredPhase` from `.coxswain/desired-phase` (operator-written), does that phase's work, writes `result.json` carrying the reported `observedPhase` (the `Result` schema gains the field); Planning writes `.coxswain/PLAN.md` (≤4KB summary) and reports `observedPhase=Planning`; Implementing runs the model+shell loop and reports `observedPhase=Implementing`; model context survives across phases within an iteration (A4). (b) **Operator side (ADR-0004):** the operator **gains the reader** — on each reconcile at a phase boundary it reads `result.json` from the sandbox (the `pods/exec` read channel ADR-0004 specifies) and writes `loop.Status.ObservedPhase` from the claim (never from anything else; a gate never runs on a claim, ADR-0005). This code does not exist today (GAP 2) — it is added here, not cited. (c) **Plan-approval gate (the minimum, Phase-4 scope — owner question, option A):** the phase table gains the gate: the runner reports `observedPhase=AwaitingApproval` after Planning (the plan is written, the operator has read it back and recorded it), the operator parks the Loop in `AwaitingApproval`, and only an **approval** applied by the operator (annotation `coxswain.io/plan-approved` on the Loop, set by `make sample-run`'s approval step after surfacing `PLAN.md`) advances it to `Implementing`. No `spec.approval` field in this build (that is the Phase-4 shape); the annotation is the minimum mechanism and is documented as such. | Envtest: (a) runner reads `desiredPhase` + writes `result.json.observedPhase` (fake model, file seam, per A1); (b) a reconcile with `result.json` reporting the next phase writes `status.observedPhase` and `nextPhase` advances one step (the existing `nextPhase` table is reused unchanged); (c) a Loop in Planning with a written plan + operator-read result reaches `AwaitingApproval`; without the approval annotation it stays there (no auto-advance); with it, it advances to `Implementing`. Fake model + envtest only; no cluster. |
| **S5 — `make sample-run` + B3 verify Job + end-to-end demo** (GAP 4, §3.3) | (a) **B3 verify Job:** the operator creates the short-lived verify Job at `Verifying` (clone init container at the pinned SHA → tamper-check init container → one check init container per acceptance check; `restartPolicy: Never`, `backoffLimit: 0` per B3b; canary per B3a, advisory scan per B3d), and reads `status.initContainerStatuses[].state.terminated.exitCode` for the outcome — all checks 0 → `Succeeded`, check *k* non-zero → iterate, tamper non-zero → `Failed:TamperedVerify` (B2, already wired). This is the **`Verifying` evidence** the owner's bar names. (b) `make sample-run` per §6 (Loop manifests for the three tasks, the approval step, the `EVIDENCE.md` generator, the in-cluster check-re-run Job). | **The demo run (the owner's bar):** at least one end-to-end run on kind with the **real vLLM** (`192.168.1.20:8000`): `make sample-run APP=gocli TASK=1` produces `EVIDENCE.md` showing, with evidence for each step — (i) the sandbox came up with the workspace cloned + proxy + netpols (§4/§5 shapes), (ii) the phase machine moved `Planning → AwaitingApproval → Implementing → Verifying` (the plan + the approval recorded), (iii) the agent actually called the model (proxy logs + request count), (iv) the B3 verify Job's exit codes (tamper 0 + each check 0) → the Loop reaches `Succeeded`, and (v) the loop's gate conditions read `EnforcementDisabled` (not True) — the demo's honesty requirement. The run is appended to the PR body. |
| **S6 — follow-up (post-merge, not this build)** | **Delivery (GAP 5):** branch push + draft PR to the **in-cluster** Gitea (never GitHub) — the Phase-6 "PR + Judge" work; plus the `spec.approval` field shape if the owner wants to move beyond the annotation gate (option B of the owner question, if chosen, lands here instead of in S4) | Out of scope for the build PRs; listed so the demo's framing (§3) stays honest. |

**Ordering:** S1 → S2 (parallel with S3) → S4 (needs S3's `COX_GOAL` +
execution) → S5. One branch + one PR per slice (AGENTS.md), explicit `git
add` paths, no force-push. S3's (a)+(b) touch `ensureSandbox` and
`loop_controller.go`; S4 (b) adds the `result.json` reader to the same
file — keep them in separate PRs so the reviewer can see each seam.

## Verification ledger (claims verified against code at `d6774cf`, origin/main)

| Claim | Where verified |
|---|---|
| Phase enum values; `AwaitingApproval` exists but nothing drives it | `api/v1alpha1/loop_types.go:29-41`; `internal/controller/loop_controller.go` (no `AwaitingApproval` outside the `nextPhase` doc comment) |
| `nextPhase` one-step table, never Succeeded/Failed, and cannot land on `AwaitingApproval` | `internal/controller/loop_controller.go:812` |
| No `spec.approval` field; approval gate is Phase 4, `mode: Auto` only in Phase 1 | `LoopSpec` (no such field); `docs/TDD-PLAN-PHASE1.md` "Out of scope for Phase 1" (line 1034) |
| **GAP 1:** `ensureSandbox` sets the agent `Command` to `["sh","-c","sleep infinity"]`; empty `workspace` emptyDir; no init container, no clone code; `workspace.repo`/`ref` have no reader in `internal/`/`cmd/`; `status.baseCommit` set only by tests | `internal/controller/loop_controller.go:703` (+ comment at :702); volumes block in `ensureSandbox`; grep of `internal/`, `cmd/` for `workspace.repo`/`BaseCommit` writers (only `api` type + tests) |
| **GAP 2:** runner does not read `desired-phase`; `result.json` schema carries no `observedPhase`; no operator code reads `result.json` (the ADR-0004 reader is un-built); `status.observedPhase` written only by envtest specs | `runner/runner.go` `Result` struct (~line 87; grep `desired`/`observed` → no matches); grep `result.json` in non-test `internal/controller/` (comments only); `docs/adr/0004-runner-result-file-only.md` (the operator reads `result.json` and writes `status.observedPhase` — the contract) |
| **GAP 3:** no approval gate; `AwaitingApproval` un-reachable via `nextPhase` | `loop_controller.go:812` (switch only returns Planning/Implementing/Verifying); `docs/TDD-PLAN-PHASE1.md:1034` |
| B2 `tamperVerdict` tri-state; B3 check-gate to `Succeeded`; verify Job not created by operator (envtest sets `status.verify.*`) | `loop_controller.go:873`, `:290-315` (Verifying block), `api/v1alpha1/loop_types.go` `VerifyStatus` doc comment |
| B3 verify Job shape (clone init + tamper init + check inits, exit codes, `restartPolicy: Never`/`backoffLimit: 0`, canary, advisory scan) | `docs/TDD-PLAN-PHASE1.md` B3/B3a/B3b/B3c/B3d (lines ~832–900) |
| **GAP 5:** no delivery: no push/PR/branch code; `gitCredentialSecret` has no reader in `internal/`/`cmd/` | grep of `internal/`, `cmd/` (no go-git/clone/push); `docs/TDD-PLAN-PHASE1.md` ("PR + Judge" = Phase 6, line 1034) |
| `endpointSecretRef`+`modelEndpoint` together + immutability CELs; pair check at reconcile | `api/v1alpha1/loop_types.go` `AgentConfig` markers; `loop_controller.go:246` (`MissingModelEndpoint`) |
| Proxy pod: `MODEL_ENDPOINT` = `http://`+endpoint; Secret mounted at `/model-creds`; stand-in fatals without a key; forwards `:8080` | `loop_controller.go:1064` (`buildProxyPod`), `:2137` (`modelEndpointValue`); `cmd/proxy-standin/main.go` |
| Proxy netpol egress = model endpoint peer + DNS; `modelPeer` → `IPBlock 192.168.1.20/32` (no except) for an IP endpoint; carve-outs belong to the egress-proxy rule | `loop_controller.go:1669` (`ensureNetworkPolicy`, proxyNP block), `:2082` (`modelPeer`), `:2058` (`egressCarveOutCIDRs`), egress-proxy netpol block |
| `COX_MODEL_BASE_URL` set by operator, `COX_*` env names rejected (I34) — the namespace the build uses for `COX_GOAL` | `loop_controller.go:653`; `AgentEnvVar` CEL in `api/v1alpha1/loop_types.go:256` (`!self.startsWith('COX_')`) |
| D30 `--allow-unenforced` → `EnforcementDisabled` (runs, not enforced); D38 `--allow-unenforced-network` → `EnforcementDisabled` | `loop_controller.go:368` (`enforcementStatus`), `:389` (`cniNetworkStatus`) |
| Dev overlay = both flags + `POD_CIDR`/`SERVICE_CIDR` | `config/dev/kustomization.yaml` |
| `spec.agent.image` honoured; default `golang:1.26`; proxy default `coxswain-proxy:standin` | `loop_controller.go:639`, `:2332` (`sandboxImage`), `:2345` (`proxyImage`) |
| vLLM at `192.168.1.20:8000`: model ids `qwen3.8-27b`, `qwen3.8-27b-pi8`; unauthenticated completion with `enable_thinking:false` | `curl` against the live endpoint, 2026-10-02 (this session) |
| GAP: runner agent image (no Dockerfile/flag) | `cmd/main.go` flags (no agent-image flag); `Makefile` (no runner build target) |
| GAP: no operator-side clone/push, no verify Job | grep results above (GAP 1 / GAP 4) |
