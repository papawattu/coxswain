# gocli — sample tasks

Seed source: `examples/gocli` in the coxswain repo (docs/SAMPLES-PLAN.md §1).
The seeded in-cluster repo (S2) carries the **broken state**: `Round`
truncates instead of rounding, so `go test ./...` is red. The acceptance
checks are the ONLY gate to `Succeeded`; the test files are protected paths
(preset `go`), so weakening them trips the tamper gate (B2/D24).

Stdlib only — no `go get`, no network. The `go` preset protects `*.go` files;
the test files below are additionally named in `protectedPaths` to make the
"do not cheat" boundary explicit.

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

## Task 1 — make the tests pass

**Goal (issue-style):**

> `gocli` rounds numbers wrong: `Round(2.5, 0)` returns `2` instead of `3`,
> and the whole test suite is red (`go test ./...` fails). Fix `Round` in
> `round.go` so it rounds half away from zero. Do **not** change or weaken
> the tests — `round_test.go` is protected; the acceptance checks must pass
> on the tests as written.

**Loop spec** (the checked-in manifest is `tasks/1.loop.yaml`, applied by
`make sample-run APP=gocli TASK=1`):

```yaml
spec:
  goal: >-
    Fix Round in round.go so it rounds half away from zero. The test suite
    (round_test.go) is currently red and is a protected path: do not change
    or weaken any test file. Make `go test ./...` pass on the tests as
    written.
  workspace:
    # The seeded repo lives under the 'samples' Gitea user (hack/samples-seed.sh
    # owns it); the git credential Secret is the seeded samples-git-cred in
    # the Loop's namespace (S5b copies it there from config/samples-git).
    repo: http://gitea.samples.svc:3000/samples/gocli.git
    ref: initial
    gitCredentialSecret: samples-git-cred
  verify:
    acceptanceChecks:
      - go build ./...
      - go test -run 'TestRound' ./...
    preset: go
    protectedPaths:
      - round_test.go
      - main_test.go
    # The check-* containers run the user commands and need a Go toolchain
    # (S5a); the trusted git containers (clone-base/import-agent/tamper) use
    # the operator's git image regardless.
    image: docker.io/library/golang:1.26
  agent:
    # The operator's --runner-image flag (set by the dev overlay) selects the
    # real runner entrypoint; image is the stand-in value the flag compares
    # against.
    image: coxswain-runner:latest
    endpointSecretRef: vllm-no-auth
    modelEndpoint: 192.168.1.20:8000
    model: qwen3.8-27b
  # No policyRefs (D46, owner decision (c), 2026-10-03): exec fencing
  # applies to the operator-owned proxies, not the agent — the runner's
  # shell tool calls spawn an open-ended set of binaries. The demo runs
  # the default-deny minimum (no AgentPolicy reference).
  loop:
    maxIterations: 3
```

Option B (owner decision): no approval gate — the bar is
Planning -> Implementing -> Verifying -> Succeeded.

**AgentPolicy: none (D46, owner decision (c), 2026-10-03).** The task-1
example policy (`tasks/1.agentpolicy.yaml`, `exec: [go, git]`) was DELETED:
exec fencing does not apply to the agent in the MVP. The runner runs every
tool call via `/bin/sh -c`, and a shell command spawns an open-ended set of
binaries (the shell's external commands, the Go toolchain's compile/link
helpers), so an exact-path exec allow-list cannot describe an agent shell —
under an enforcing KubeArmor Block policy the agent's shell cannot exec and
the Loop wedges with no output (observed in the S5b demo run). What protects
the system instead: the network fence (egress proxy + NetworkPolicy),
credential isolation (ADR-0006) and the verify Job (ADR-0005). Exec fencing
stays for the operator-owned proxies; per-tool agent proxies (no general
shell) are the D41 follow-on. A Loop whose policyRefs carry an exec list
without the runner's shell (`/bin/sh`) is rejected fail-fast (`PolicyValid=False` reason `ExecListMissingShell`) before the sandbox is created.

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
    repo: http://gitea.samples.svc:3000/samples/gocli.git
    ref: initial
    gitCredentialSecret: samples-git-cred
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
  # No policyRefs (D46, owner decision (c)): exec fencing applies to the
  # proxies, not the agent — see the task-1 AgentPolicy note.
```

**AgentPolicy:** none (D46, same decision as task 1).

**Expected evidence:** plain run still prints the bare number; `-json`
prints the JSON object; `TestMainJSON` green (the protected `main_test.go` is
untouched); reference fix `tasks/2.patch` adds `json.go` and the `-json`
branch in `main.go` only (implementation-only).
