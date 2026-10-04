# webapi — sample tasks

Seed source: `examples/webapi` in the coxswain repo (docs/SAMPLES-PLAN.md §1).
The seeded in-cluster repo (S2) carries the **broken state**: `/healthz`
works, but `/api/v1/ping` and `/api/v1/echo` are missing, so
`bash test/smoke.sh` is red. The acceptance checks are the ONLY gate to
`Succeeded`; `test/smoke.sh` is a protected path (preset `go` + explicit
protectedPath), so weakening it trips the tamper gate (B2/D24).

Stdlib only — no `go get`, no network.

**Git credential:** each spec's `workspace.gitCredentialSecret` names a standard
`kubernetes.io/basic-auth` Secret with keys `username` and `password`
(the same shape as the seeded Gitea's `samples-git-cred`). The operator mounts
ONLY those two keys (`items: [{key: 'username', ...}, {key: 'password', ...}]`)
at `/workspace-creds/` (read-only); the init container's `fetch` passes them
to git as a Basic-auth `http.extraHeader` built from the files (a `-c` flag
for that one command only — nothing is written or persisted; the git
credential helpers refuse the read-only mount). A Secret missing either key
makes the init container fail loud at pod start. See docs/SAMPLES-PLAN.md S3
for the rationale.

## Task 1 — add `/api/v1/ping`

**Goal (issue-style):**

> `webapi` only serves `/healthz`. Add a `GET /api/v1/ping` route that
> returns HTTP 200 with the exact body `pong` (no trailing newline). The
> existing `/healthz` route and the smoke test script must keep working
> unchanged — `test/smoke.sh` is protected; do not weaken or modify it.

**Loop spec (paste into the Loop manifest):**

```yaml
spec:
  goal: >-
    Add a GET /api/v1/ping route to main.go that returns HTTP 200 with the
    exact body "pong" (no trailing newline). The existing /healthz route is
    unchanged. The smoke test script test/smoke.sh is a protected path: do
    not modify or weaken it; make it pass by adding the route.
  workspace:
    repo: http://gitea.samples.svc:3000/webapi.git
    ref: initial
    gitCredentialSecret: gitea-clone
  verify:
    acceptanceChecks:
      - go build ./...
      - bash test/smoke.sh ping
    preset: go
    protectedPaths:
      - test/smoke.sh
  agent:
    image: coxswain-runner:latest
    endpointSecretRef: vllm-no-auth
    modelEndpoint: 192.168.1.20:8000
    model: qwen3.8-27b
  # No policyRefs (D46, owner decision (c), 2026-10-03): exec fencing
  # applies to the operator-owned proxies, not the agent — an agent exec
  # list that omits the runner's shell (/bin/sh) fails fast (reason
  # ExecListMissingShell); without a list exec is unrestricted.
```

**AgentPolicy: none (D46, owner decision (c)): exec fencing does not apply
 to the agent in the MVP (see the no-policyRefs note above); the network
 fence, credential isolation (ADR-0006) and the verify Job protect the
 system.**

**Expected evidence:** `/healthz` still 200 `ok`; `/api/v1/ping` 200 `pong`;
`test/smoke.sh` reports `ping: PASS`; reference fix `tasks/1.patch`.

---

## Task 2 — add `/api/v1/echo`

**Goal (issue-style):**

> Add a `POST /api/v1/echo` route that mirrors the request body back to the
> caller with `Content-Type: application/json`. Non-POST requests get 405.
> The existing `/healthz` and `/api/v1/ping` routes keep working. The smoke
> test script is protected — extend it **only if necessary** (the reference
> fix adds the echo assertion to `test/smoke.sh` as a new check; the
> protected-path rule applies to weakening, not to extending with a new
> assertion).

**Loop spec:**

```yaml
spec:
  goal: >-
    Add a POST /api/v1/echo route to main.go that mirrors the request body
    back with Content-Type application/json. Non-POST requests return 405.
    The existing /healthz and /api/v1/ping routes are unchanged. Add the
    echo assertion to test/smoke.sh (a new check, not a weakening of the
    existing ping check).
  workspace:
    repo: http://gitea.samples.svc:3000/webapi.git
    ref: initial
    gitCredentialSecret: gitea-clone
  verify:
    acceptanceChecks:
      - go build ./...
      - bash test/smoke.sh echo
    preset: go
    protectedPaths:
      - test/smoke.sh
  agent:
    image: coxswain-runner:latest
    endpointSecretRef: vllm-no-auth
    modelEndpoint: 192.168.1.20:8000
    model: qwen3.8-27b
  # No policyRefs (D46, as task 1)
```

**AgentPolicy:** none (D46, same as task 1).

**Expected evidence:** `POST /api/v1/echo` with `{"hello":"world"}` returns
the same body with `Content-Type: application/json`; `GET` on the same path
returns 405; `test/smoke.sh` reports `echo: PASS`; reference fix
`tasks/2.patch` (applied on the seed; it also carries task 1's ping route).
