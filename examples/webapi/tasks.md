# webapi — sample tasks

Seed source: `examples/webapi` in the coxswain repo (docs/SAMPLES-PLAN.md §1).
The seeded in-cluster repo (S2) carries the **broken state**: `/healthz`
works, but `/api/v1/ping` and `/api/v1/echo` are missing, so
`bash test/smoke.sh` is red. The acceptance checks are the ONLY gate to
`Succeeded`; `test/smoke.sh` is a protected path (preset `go` + explicit
protectedPath), so weakening it trips the tamper gate (B2/D24).

Stdlib only — no `go get`, no network.

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
      - bash test/smoke.sh
    preset: go
    protectedPaths:
      - test/smoke.sh
  agent:
    image: coxswain-runner:latest
    endpointSecretRef: vllm-no-auth
    modelEndpoint: 192.168.1.20:8000
  policyRefs:
    - webapi-task-1
```

**AgentPolicy (the fenced tools the task needs):**

```yaml
apiVersion: coxswain.wattu.com/v1alpha1
kind: AgentPolicy
metadata:
  name: webapi-task-1
spec:
  exec:
    - /usr/local/go/bin/go        # golang:1.26 base image path
    - /usr/bin/git
    - /usr/bin/curl               # the smoke script curls the local server
  network: []                     # no external egress; stdlib only
```

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
      - bash test/smoke.sh
    preset: go
    protectedPaths:
      - test/smoke.sh
  agent:
    image: coxswain-runner:latest
    endpointSecretRef: vllm-no-auth
    modelEndpoint: 192.168.1.20:8000
  policyRefs:
    - webapi-task-2
```

**AgentPolicy:** same as task 1 (`webapi-task-2`, same spec).

**Expected evidence:** `POST /api/v1/echo` with `{"hello":"world"}` returns
the same body with `Content-Type: application/json`; `GET` on the same path
returns 405; `test/smoke.sh` reports `echo: PASS`; reference fix
`tasks/2.patch` (applied on the seed; it also carries task 1's ping route).
