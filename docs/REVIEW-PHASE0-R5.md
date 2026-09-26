# Phase 0 review, round 5 — open issues

Review of `40ee0ec`..`5c91a50` (2026-09-26), since tag `review/phase0-r4`.
Baseline at review time: `cd runner && go test ./...` green, runner
golangci-lint 0 issues.

Each issue is self-contained so it can be picked up independently. Work
test-first as in `docs/TDD-PLAN.md`: write the failing test at the named seam,
then fix. Tick the box and add the commit hash when done. Issue IDs continue
from earlier rounds so every ID is unique.

Priority: **P1** = fix before Phase 1 starts. **P2** = decide during Phase 1
(design). **P3** = cleanup, batch whenever.

## Verdicts on reviewed commits

| Commit | Issue | Verdict | Follow-ups |
|--------|-------|---------|------------|
| `40ee0ec` | I8 small follow-ups | OK | — |
| `5c91a50` | I5 + I7 typed structs, robustness | OK + notes | **I10 (P1)**, I11, I12 |

I7 is clean: no `map[string]any` on the wire path, the `jsonKey*` consts are
gone, and the old assertions pass unchanged. I5's four behaviors each have a
test at the right seam (`runner_robustness_test.go`). Tick I5 in
`REVIEW-PHASE0.md`.

Still open: **D10 (P1, before B2)**, D9, D11–D13, I9, and I4 (owner decision).

---

## P1 — Bug

### I10. Shell timeout doesn't fire when the command leaves a child running

- [ ] Done

**Where:** `runner/runner.go` `execShell` (`exec.CommandContext` +
`CombinedOutput`).

**Problem:** on timeout, `CommandContext` kills only `sh`. Any child it
started (a background server `python -m http.server &`, `go test` build
workers, `npm run dev &`) inherits the stdout/stderr pipe, and
`CombinedOutput` waits for EOF on that pipe — so `execShell` blocks until the
child exits, which for a server is **never**. The runner hangs, no
`result.json` is written, and Phase 1's `PhaseTimeout` becomes the only way
out. Verified with a probe: a 1 s context on `sh -c "sleep 6 & wait"`
returned after **6.0 s**. An agent starting a dev server "to test it" is
common behavior, so this will happen.

**Fix:**
1. Make the shell timeout injectable (`runConfig.ShellTimeout`, default 60 s)
   so the test can use ~1 s.
2. Red: a tool call running `sleep 30 & wait` (or `sh -c 'sleep 30' &`) with
   a 1 s timeout must return within ~3 s, and the run must still write
   `result.json` with the timeout visible in the tool output.
3. Green: run the command in its own process group
   (`SysProcAttr{Setpgid: true}`), set `cmd.Cancel` to kill the whole group
   (`syscall.Kill(-pid, SIGKILL)`), and set `cmd.WaitDelay` (e.g. 2 s) so
   `Wait` stops waiting on pipes held by stragglers. Consider the same
   process-group kill after a *successful* command too, so backgrounded
   processes don't outlive the tool call and leak across steps.

**Acceptance:** new test red on current code, green after; existing runner
tests green.

---

## P2 — Design

### I11. HTTP client timeout of 60 s is too short for real models

- [ ] Done

**Where:** `runner/runner.go` `run` (`&http.Client{Timeout: 60 * time.Second}`).

**Problem:** a non-streaming completion from a local 27B-class model (the
homelab vLLM target) or a long-context request can easily exceed 60 s. The
runner then records `blocked` for a request that would have succeeded.

**Fix:** make it configurable (`runConfig.ModelTimeout`, env-driven once the
runner has a `main`), default ~10 min; keep the per-request context so a run
deadline can cancel it later. Test with the fake model delaying past a small
configured timeout → `blocked` with a timeout message; and a delay under it →
success.

---

## P3 — Cleanup

### I12. Runner nits from `5c91a50`

- [ ] Done

- `truncateToolOutput` keeps the **head** of the output. For builds and tests
  the useful part (the failure, the summary line) is usually at the **tail**.
  Keep head + tail (e.g. first 4 KiB + last 12 KiB with a
  `… (N bytes elided) …` marker). Also cut on a UTF-8 rune boundary
  (`utf8.RuneStart`) so a multi-byte character isn't split — same for the
  256-byte `errBody` cut in `callModel`.
- `callModel`: `defer func() { if cerr := resp.Body.Close(); cerr != nil { _ = cerr } }()`
  is a no-op wrapper to appease errcheck. Use `defer func() { _ = resp.Body.Close() }()`.
- `knownToolNames()` builds a map on every `driveModel` call; a package-level
  `var knownTools = map[string]bool{toolNameShell: true}` is simpler.

**Acceptance:** truncation test extended to assert the tail survives; lint
clean.

---

## Next round will check

- I10 (P1) fix.
- ADR-0005 amendment for D10 (P1) before any B2 code, plus D9, D11–D13.
- I4 — still needs the owner's decision. Don't pick silently.
