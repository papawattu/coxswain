# Phase 0 review, round 3 — open issues

Review of `2bc945f` (2026-09-26), since tag `review/phase0-r2`. Baseline at
review time: `cd runner && go test ./...` green, runner `golangci-lint` 0
issues.

Each issue is self-contained so it can be picked up independently. Work
test-first as in `docs/TDD-PLAN.md`: write the failing test at the named seam,
then fix. Tick the box and add the commit hash when done. Issue IDs continue
from earlier rounds so every ID is unique.

Priority: **P1** = fix before Phase 1 starts. **P2** = decide during Phase 1
(design). **P3** = cleanup, batch whenever.

## Verdicts on reviewed commits

| Commit | Issue | Verdict | Follow-ups |
|--------|-------|---------|------------|
| `2bc945f` | I3 runner in `make test` / `make lint` | OK + notes | I7, I8 |

I3 meets its acceptance: `test` and `lint` both cover the runner, and CI goes
through those targets. Tick I3 in `REVIEW-PHASE0.md`.

Still open from round 2: **D7 (P1, blocks Phase 1 B2/B3)**, D8, I6.

---

## P3 — Cleanup

### I7. Replace `map[string]any` + JSON-key consts with typed request structs

- [x] Done

  - Typed wire structs in `runner/runner.go`: `chatRequest` (model/messages/
    tools), `chatMessage` (role/content/tool_calls/tool_call_id), `toolDef`,
    `fnDef`, `paramSchema`, `propSchema`. `callModel`/`driveModel`/`assistant
    message` build from these — no `map[string]any` in the request/response
    path; the `jsonKey*` consts are gone. `jsonRole*` + `jsonToolFunction` are
    kept as protocol *value* consts the tests assert against (I7 allows this).
  - **Done together with I5** as the review asked: the rewrite of
    `callModel`/`driveModel` is the single pass.
  - All runner tests green with unchanged assertions (R1–R4, R3, R5, I1 tools
    test) — verified.

**Where:** `runner/runner.go` — the `jsonKey*` / `jsonRole*` /
`jsonToolFunction` const block, `callModel`, `shellToolSchema`,
`assistantMessageFor`, the tool message built in `driveModel`.

**Problem:** the goconst findings were fixed by naming ~20 JSON field-name
strings (`jsonKeyRole = "role"`, …). That satisfies the linter, but the root
cause is building the chat-completions wire format from `map[string]any`:
there's no compile-time schema, a key typo is a silent runtime bug, and the
consts make the code harder to read than the literals were.

**Fix:** define typed wire structs (e.g. `chatRequest{Model, Messages,
Tools}`, `chatMessage{Role, Content, ToolCalls, ToolCallID}`,
`toolDef`/`functionDef`, reusing the existing `toolCall`/`fnCall`) with json
tags, marshal those, and delete the `jsonKey*` consts. Keep `jsonRole*` only
if goconst still requires it. This is a pure refactor: R1–R4 and the I1 tools
test must pass unchanged — they're the safety net. **Do this together with
I5** (runner robustness), since I5 rewrites `callModel`/`driveModel` anyway;
doing I5 first on the map-based code means touching it twice.

**Acceptance:** no `map[string]any` in the request/response path; `jsonKey*`
consts gone; all runner tests green without edits to their assertions;
`make lint` 0 issues.

### I8. Small follow-ups from `2bc945f`

- [x] Done

  - `fakeModelName` ("fake-model") moved from `runner.go` to `runner_test.go`
    (test fixture). `runner.go` no longer references it.
  - Makefile `lint`/`lint-fix` use `$(GOLANGCI_LINT)` (absolute path) for the
    runner too, so an overridden `GOLANGCI_LINT` applies to both modules
    (verified with a wrapper that logged its invocation path).
  - Makefile `runner-test` now runs `go vet ./...` on the runner module as well.

- `runner/runner.go` const block: `fakeModelName = "fake-model"` is a test
  fixture in production code. Move it to a `_test.go` file (or the
  `testhelper` package).
- `Makefile` `lint` / `lint-fix`: `cd runner && ../bin/golangci-lint run`
  hard-codes the path; use `"$(GOLANGCI_LINT)"` like the root invocation so
  an overridden `GOLANGCI_LINT` applies to both modules.
- `Makefile` `test`: `fmt` and `vet` still run only on the root module. Fine
  while golangci-lint (with `govet`) covers the runner; note it, or add
  `cd runner && go vet ./...` to `runner-test`.

**Acceptance:** `grep -n fake-model runner/runner.go` returns nothing;
`make lint GOLANGCI_LINT=<other path>` uses that path for both modules.

---

## Next round will check

- I5 + I7 together (runner robustness on typed structs).
- D7 — ADR-0005 before any Phase 1 B2/B3 work.
- I4 — still needs the owner's decision. Don't pick silently.
