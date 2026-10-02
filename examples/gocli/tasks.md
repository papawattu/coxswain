# gocli — sample tasks

Seed source: `examples/gocli` in the coxswain repo (docs/SAMPLES-PLAN.md §1).
The seeded in-cluster repo (S2) carries the **broken state**: `Round`
truncates instead of rounding, so `go test ./...` is red. The acceptance
checks are the ONLY gate to `Succeeded`; the test files are protected paths
(preset `go`), so weakening them trips the tamper gate (B2/D24).

Stdlib only — no `go get`, no network. The `go` preset protects `*.go` files;
the test files below are additionally named in `protectedPaths` to make the
"do not cheat" boundary explicit.

## Task 1 — make the tests pass

**Goal (issue-style):**

> `gocli` rounds numbers wrong: `Round(2.5, 0)` returns `2` instead of `3`,
> and the whole test suite is red (`go test ./...` fails). Fix `Round` in
> `round.go` so it rounds half away from zero. Do **not** change or weaken
> the tests — `round_test.go` is protected; the acceptance checks must pass
> on the tests as written.

**Loop spec (paste into the Loop manifest):**

```yaml
spec:
  goal: >-
    Fix Round in round.go so it rounds half away from zero. The test suite
    (round_test.go) is currently red and is a protected path: do not change
    or weaken any test file. Make `go test ./...` pass on the tests as
    written.
  workspace:
    repo: http://gitea.samples.svc:3000/gocli.git
    ref: initial
    gitCredentialSecret: gitea-clone
  verify:
    acceptanceChecks:
      - go build ./...
      - go test -run 'TestRound' ./...
    preset: go
    protectedPaths:
      - round_test.go
      - main_test.go
  agent:
    image: coxswain-runner:latest
    endpointSecretRef: vllm-no-auth
    modelEndpoint: 192.168.1.20:8000
    model: qwen3.8-27b
  policyRefs:
    - gocli-task-1
```

**AgentPolicy (the fenced tools the task needs):**

```yaml
apiVersion: coxswain.wattu.com/v1alpha1
kind: AgentPolicy
metadata:
  name: gocli-task-1
spec:
  exec:
    - /usr/local/go/bin/go        # golang:1.26 base image path
    - /usr/bin/git
  network: []                     # no external egress; stdlib only
```

**Expected evidence:** `go build`/`go vet`/`go test` exit 0; the only change
is `round.go` (the reference fix is `tasks/1.patch`); `round_test.go`
untouched (tamper check 0).

---

## Task 2 — add a `--json` output flag

**Goal (issue-style):**

> `gocli` prints a bare number. Add a `-json` flag: with `-json`, print the
> result as JSON `{"value": <in>, "places": <n>, "rounded": <r>}` on one
> line instead of the bare number. The existing plain output and the
> existing tests must keep working unchanged; the `TestMainJSON` test in
> `main_test.go` (a protected path) is already in the tree and must pass on
> the tests as written.

**Loop spec:**

```yaml
spec:
  goal: >-
    Add a -json flag to main.go: with -json, print the rounded result as a
    single-line JSON object {"value": <input>, "places": <n>, "rounded":
    <r>} instead of the bare number. Plain output without -json is unchanged.
    Add TestMainJSON to main_test.go covering the flag.
  workspace:
    repo: http://gitea.samples.svc:3000/gocli.git
    ref: initial
    gitCredentialSecret: gitea-clone
  verify:
    acceptanceChecks:
      - go build ./...
      - go vet -tags task2 ./...
      - go test -tags task2 -run 'TestMainJSON' ./...
    preset: go
    protectedPaths:
      - round_test.go
      - main_test.go
  agent:
    image: coxswain-runner:latest
    endpointSecretRef: vllm-no-auth
    modelEndpoint: 192.168.1.20:8000
    model: qwen3.8-27b
  policyRefs:
    - gocli-task-2
```

**AgentPolicy:** same as task 1 (`gocli-task-2`, same spec).

**Expected evidence:** plain run still prints the bare number; `-json`
prints the JSON object; `TestMainJSON` green (the protected `main_test.go` is
untouched); reference fix `tasks/2.patch` adds `json.go` and the `-json`
branch in `main.go` only (implementation-only).
