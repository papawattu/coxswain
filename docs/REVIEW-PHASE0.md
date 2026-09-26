# Phase 0 review — open issues

Review of the Phase 0 design and implementation at `558c225` (2026-09-26).
Baseline at review time: `make test` green (controller coverage 68.8%),
`cd runner && go test ./...` green, `make lint` 0 issues.

Each issue is self-contained so it can be picked up independently. Work
test-first as in `docs/TDD-PLAN.md`: write the failing test at the named seam,
then fix. Tick the box and add the commit hash when done.

Priority: **P1** = fix before Phase 1 starts. **P2** = decide during Phase 1
(design). **P3** = cleanup, batch whenever.

---

## P1 — Implementation bugs

### I1. Runner never advertises tools to the model

- [x] Done (I1 test: runner_tools_test.go; fake hardened in testhelper/fakemodel.go)

**Where:** `runner/runner.go:88` (`callModel`).

**Problem:** the request body is only `{"model", "messages"}`. No `tools`
array is sent, so a real OpenAI-compatible model will never emit a `shell`
tool call. R3 (`runner_tool_test.go`) passes only because the fake model
(`runner/testhelper/fakemodel.go`) returns tool calls unconditionally. The fake
already decodes `Tools` from the request, but no test asserts on it.

**Fix:**
1. Red: in a runner test, assert `fake.Requests[0].Tools` is non-empty and
   contains a function named `shell` with a `command` string parameter.
2. Green: send a `tools` array (OpenAI function-calling schema) with the
   `shell` tool on every request.
3. Optional hardening of the fake: only emit a queued tool call if the request
   advertised a tool of that name — so R3 can't pass without tools again.

**Acceptance:** the new test fails on current code and passes after; R1–R4 stay green.

### I2. Adopted Sandbox gets no owner reference

- [x] Done (S2 extended + new foreign-ownership test in loop_adoption_test.go; SetControllerReference moved into the mutate func in loop_controller.go)

**Where:** `internal/controller/loop_controller.go:97-117` (`ensureSandbox`).

**Problem:** `SetControllerReference` is called on `desired` *before*
`CreateOrUpdate`. When the Sandbox already exists, `CreateOrUpdate`'s `Get`
overwrites the object's metadata with the server copy, and the mutate func
never re-sets the owner ref. Verified with a throwaway envtest probe: after
reconciling a Loop against a pre-existing `<loop>-sandbox`, the Sandbox had
**0** owner references. Consequences:
- The Sandbox is not garbage-collected when the Loop is deleted.
- `Owns(&Sandbox{})` never maps its events back to the Loop.
- Any Sandbox that happens to be named `<loop>-sandbox` (even one owned by
  another controller) is silently taken over and its containers overwritten.

S2 (`loop_adoption_test.go`) doesn't catch this because it only asserts count
and the `pre-existing` label.

**Fix:**
1. Red: extend S2 to assert the adopted Sandbox has exactly one controller
   owner ref pointing at the Loop (name + UID).
2. Red: new test — a pre-existing Sandbox with a *different* controller owner
   ref is not taken over; reconcile returns an error (or sets a condition) and
   the Sandbox spec is untouched.
3. Green: move `controllerutil.SetControllerReference(loop, desired, r.Scheme)`
   inside the mutate func (it returns `AlreadyOwnedError` for case 2).

**Acceptance:** both tests red on current code, green after; S0–S3 stay green.

### I3. CI never tests or lints the `runner/` module

- [x] Done (commit 2bc945f: `make test`/`make lint`/`lint-fix` now cover the
      runner module; runner lint findings fixed in the same commit)

**Where:** `Makefile` (`test`, `lint` targets), `.github/workflows/test.yml`,
`.github/workflows/lint.yml`.

**Problem:** `runner/` has its own `go.mod`, so the root `go list ./...` and
`golangci-lint run` skip it. R1–R4 only run if someone `cd runner` manually.

**Fix:** add runner test + lint to the Makefile (e.g. `test` also runs
`cd runner && go test ./...`; `lint` also runs golangci-lint in `runner/`), or
add a `go.work` and adjust targets. Make sure CI goes through those targets.

**Acceptance:** deliberately breaking a runner test makes `make test` fail
locally; CI workflows invoke the updated targets.

### I4. Phase 0 "done when" is inconsistent with what was delivered

- [ ] Done

**Where:** `docs/PLAN.md` (Phase 0), `docs/E2E-PHASE0.md`, `cmd/main.go`,
`internal/controller/loop_controller.go`.

**Problem:** `docs/PLAN.md` says Phase 0 is done when "the smoke-test runner
runs inside [the Sandbox], calls the model, and writes a `result.json` the
controller logs". What was proven (`E2E-PHASE0.md`) is the narrower original
criterion: the Sandbox *object* is created and logged; the agent-sandbox
controller wasn't run, so no pod exists. Gaps against the written plan:
- `runner/` has no `main` package, no image, no Dockerfile.
- Runner has only a `shell` tool — no `fs` / `git` tools; prompt isn't read
  from an env var.
- Controller runs `docker.io/library/golang:1.26` with `sleep infinity`;
  `LoopReconciler.SandboxImage` is never set from a flag in `cmd/main.go`.
- No `SandboxTemplate` exists.
- Plan says "Claude API"; runner speaks OpenAI chat-completions.

**Fix (needs a decision from the owner — don't pick silently):** either
(a) amend `docs/PLAN.md` Phase 0 "done when" to match what was proven and move
the listed gaps into Phase 1 as explicit items, or (b) close the gaps now
(runner `main` + image, `--sandbox-image` flag, run agent-sandbox controller in
kind, e2e proves `result.json` written). Also make the docs say which model API
is targeted (OpenAI-compatible fits the homelab vLLM stack).

**Acceptance:** PLAN.md and E2E-PHASE0.md agree on what Phase 0 delivered.

### I5. Runner robustness

- [x] Done — commit `5c91a50` (I5+I7 together)

  - HTTP status checked (401/500 → blocked with "HTTP <n>: <body>" in notes);
    `io.ReadAll` error checked; unknown tool names rejected (not executed);
    shell output capped to 16 KiB; `maxSteps` configurable via
    `runConfig.MaxSteps`; `itoa` wrapper dropped.

**Where:** `runner/runner.go`.

- HTTP status isn't checked; a 401/500 surfaces as "no choices in response".
  Return an error containing status + body snippet. `io.ReadAll` error is ignored.
- Any tool-call name is executed as shell. Reject unknown tool names with a
  tool-result error message.
- Tool output is unbounded; truncate (e.g. last 16 KiB) before feeding back.
- `maxSteps = 5` (line 174) is too low for real work; make it configurable
  with a higher default.
- `itoa` wrapper is unnecessary; use `strconv.Itoa`.

**Acceptance:** one test per behavior change at the result-file / fake-model seam.

---

## P2 — Design decisions (resolve during Phase 1, record as ADRs)

### D1. Runner must not write Loop status

- [x] Decided (ADR: 0004, commit e19c5a2)

`CONTEXT.md` ("Runner") says the runner writes `status.observedPhase`. That
needs RBAC for the sandbox to patch Loop status, and the runner's credentials
are effectively the model's (it has a shell). The model could forge
phase/iteration. It also contradicts CONTEXT.md's own "Result file is the only
output the operator reads" and "the runner has no other channel".
**Recommendation:** sandbox gets no Kubernetes credentials for Loop objects
(`automountServiceAccountToken: false` or a no-perms SA); operator observes
progress only via the result file.

### D2. How the operator reads `result.json`

- [ ] Decided (ADR: 0005, folded into D7)

Undesigned. Options: operator `exec`s `cat` into the sandbox (needs
`pods/exec` RBAC), a shared PVC the operator mounts, or a small read-only
sidecar. Choice affects RBAC and D1.

> Round 2 (REVIEW-PHASE0-R2): D2 and D3 are being decided together by
> ADR-0005 (D7) — how the operator obtains verify *evidence*. result.json
> carries the agent's claims only, never evidence the operator gates on.

Undesigned. Options: operator `exec`s `cat` into the sandbox (needs
`pods/exec` RBAC), a shared PVC the operator mounts, or a small read-only
sidecar. Choice affects RBAC and D1.

### D3. Verify isolation / what "protected paths" means

- [ ] Decided (ADR: 0005, folded into D7)

Verify runs in the same sandbox where the agent has a shell. For a check like
`go test ./...` there is no single "source file" to hash, and the agent can
pass verify without touching tests: `replace` directives in `go.mod`, edited
Makefile/conftest/test helpers, a shadowed binary on `PATH`, a background
process that persists into verify. **Recommendation:** run acceptance checks in
a fresh pod from a clean checkout of the agent's commit, with the check
definitions (and any declared protected paths) taken from the base ref, not
the workspace. Otherwise specify exactly which paths are hashed.

### D4. API group is `cox.cox.dev` under a domain we don't own

- [x] Decided — renamed to `coxswain.wattu.com` (commit e5027e9). Leftover
      `cox.dev` references in cmd/main.go + docs are tracked as I6 in
      REVIEW-PHASE0-R2.

Kubebuilder produced `cox.cox.dev` (domain `cox.dev` + group `cox`); the plan
says `cox.dev`. ADR-0001 records that `cox.dev` is taken. Owning the group's
domain is the convention. Rename now (e.g. `coxswain.wattu.com`) — cheapest
before any Loop objects exist. Touches `PROJECT`, `groupversion_info.go`, CRD
file names, RBAC markers, samples, docs.

### D5. Pin one Kubernetes version for dev + CI

- [ ] Decided

Plan says dev = 1.32, the kind cluster ran 1.34, envtest uses 1.37,
production needs ≥1.37 (agent-sandbox). Pick one for kind + envtest + CI and
update `CONTEXT.md` ("Version target") and `docs/PLAN.md`.

### D6. Mandatory checkpoints vs kind

- [ ] Decided

Checkpointing is mandatory (Phase 3), but kind has no VolumeSnapshot support
by default. Plan for csi-hostpath-driver + snapshot CRDs in the kind setup, or
run Phase 3 e2e on the homelab Ceph cluster (RBD supports snapshots).

---

## P3 — Cleanup

- [ ] **License headers corrupted** — `api/v1alpha1/loop_types.go`,
  `internal/controller/loop_controller.go`,
  `internal/controller/loop_controller_test.go` read `" IS" BASIS` with a
  missing newline (and a duplicated line in `loop_types.go`). Restore from
  `hack/boilerplate.go.txt`.
- [x] **Sample CR is a stub** — `config/samples/cox_v1alpha1_loop.yaml` still
  has `TODO(user)` and label `app.kubernetes.io/name: coxscaf`. Replace with
  the smoke Loop from `E2E-PHASE0.md`.  (Done: `5762374` renamed it to
  `coxswain_v1alpha1_loop.yaml` and replaced the stub with the real smoke Loop.)
- [ ] **`maxIterations` default is fragile** — `LoopSpec.Loop`
  (`loop_types.go:102`) has no `+kubebuilder:default={}`, so a YAML Loop that
  omits `loop:` gets no default (Go clients happen to send `loop: {}`). Add
  the marker, regenerate, add an envtest creating the Loop via unstructured
  without `loop`.
- [ ] **`Workspace.Ref`** (`loop_types.go:56`) — `+kubebuilder:default=""` is
  a no-op; remove. `Repo` has `Format=uri` (line 50) which rejects SSH remotes
  (`git@github.com:...`); decide whether SSH is supported and adjust.
- [ ] **`go.mod`** marks `sigs.k8s.io/agent-sandbox` as `// indirect` though
  it's imported directly — run `go mod tidy`.
- [ ] **e2e suite is still scaffold** — `test/e2e/e2e_test.go` only checks
  manager + metrics. Automate the manual steps in `E2E-PHASE0.md` (apply Loop
  → Sandbox exists with owner ref → log line). Use `make deploy`
  (`config/default` overlay) instead of the hand-patched namespace workaround.
- [ ] **Scratch file committed** — delete
  `docs/e2e-prereqs-scout-2026-09-26T21-02-17.md`.
- [ ] **Controller tidy-ups** (`loop_controller.go`): type the logger param as
  `logr.Logger`; combine the two `Status().Update` calls into one; the
  "ensured loop sandbox" log fires on every reconcile — log at V(1) when
  `op == unchanged`; `<loop>-sandbox` can exceed 63 chars — cap Loop name
  length via CRD validation or hash-truncate the Sandbox name.
