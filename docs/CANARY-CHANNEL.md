# K0: The canary channel — design note

**The question** (PLAN-PHASE7 K0): can proof that the canary's forced `t.Fatal` actually fired reach the operator over a channel that code running in the test binary (the agent's `init()` and any of its package code) can neither read nor forge?

The open limit (I199, REVIEW-PHASE1-R45): a probe that reads the generated `zzz_canary_test.go`, prints the token and `os.Exit(1)` reads as OK — the canary's `-json` output contains the token (the probe's `stdout` is captured by `test2json` in the `go` command's process and emitted as a JSON event), and the exit is non-zero, so the canary's decision logic (rc≠0 + token-in-output → OK) is satisfied. The canary test never runs.

This note evaluates five candidates, each with a scratch harness that runs the forge.

---

## Candidate 1: A one-shot token from a trusted sidecar (socket)

**Mechanism.** A trusted sidecar process (outside the test binary) serves the canary token exactly once over a unix socket. The canary's `init()` connects, reads the token, and `t.Fatal(token)`s. The agent's `init()` cannot read the token because the socket is closed after the first connection (one-shot).

**Forge attempt.** An `init()` in the agent's package that also connects to the sidecar. Because the canary's `init()` runs first (the canary test file sorts last alphabetically, so its `init()` runs after the agent's — but the harness shows the sidecar is one-shot regardless of which connects first), the second connection is refused.

**Harness** (`/tmp/k0-sidecar/`):

```
$ go test -count=1 -v .
=== RUN   TestForgeSecondConnection
    probe_test.go:14: forge: second connection refused: dial unix /tmp/k0-sidecar-sock: connect: no such file or directory (expected)
--- PASS: TestForgeSecondConnection (0.00s)
=== RUN   TestZZZCanary
    zzz_canary_test.go:37: canary-999
--- FAIL: TestZZZCanary (0.00s)
FAIL
FAIL    sidecar     0.057s
```

The canary test `t.Fatal`s with the token (the honest path). The forge's second connection is refused. The sidecar is one-shot.

**Result: the sidecar channel is forgeable for a different reason.** The sidecar's socket path (`/tmp/k0-sidecar-sock`) is written to the test binary's source (or passed as an env var, or a hard-coded path). An `init()` in the agent's package that runs BEFORE the canary's `init()` (the agent's test file sorts earlier) can connect first and steal the token. The one-shot property only protects against a *second* connection; the *first* connection is whoever's `init()` runs first. The agent controls the sort order of its test files. A file named `aaa_probe_test.go` sorts before `zzz_canary_test.go`, so its `init()` runs first, reads the token from the sidecar, and the canary's `init()` gets connection-refused. The canary then has no token and cannot `t.Fatal` with the correct one — but the probe has already read it and can print it + `os.Exit(1)`. The sidecar channel does not close the limit.

The only fix is to make the sidecar's socket path unpredictable (a random path, like the canary's seed), and to make the sidecar serve the token to the *second* connection (the canary's `init()`), not the first. But then the agent's `init()` (which runs first) can connect and get a connection-refused (the sidecar is waiting for the second connection), and the canary's `init()` (which runs second) connects and gets the token. The agent cannot read the token. But the agent can ALSO connect a second time (from a test, not from `init()`), and the sidecar (which has already served the token to the canary) closes the listener — so the second connection is refused. This works. But it requires the sidecar to be built to serve the token to the *second* connection specifically, not the first. The sidecar must know which connection is the canary's. It cannot (the connections are indistinguishable). So the sidecar must serve the token to the *first* connection that sends a specific handshake (the canary's `init()` sends a handshake string the agent does not know). But the handshake string is written to the test binary's source, so the agent can read it. The sidecar channel is forgeable.

---

## Candidate 2: The exit, observed outside the process (`test2json` in the `go` command's process)

**Mechanism.** The `go` command runs `test2json` in its own process (not the test binary's). `test2json` translates the test binary's `stdout` into JSON events. The canary reads the `-json` output (the events) and checks for the token. The test binary's `init()` cannot forge the `-json` events directly (they are emitted by `test2json` in the `go` command's process, not by the test binary).

**Forge attempt.** The agent's `init()` prints the token to `stdout` and `os.Exit(1)`. `test2json` captures the `stdout` (including the token line) and emits it as a JSON `output` event. The canary greps the `-json` output for the token and finds it. The exit is non-zero. The canary reads it as OK.

**Harness** (`/tmp/k0-forge/`):

```
$ go test -count=1 -run '^TestZZZCanary$' -json . > honest.json 2>&1
rc=1
$ FORGE=1 go test -count=1 -run '^TestZZZCanary$' -json . > forged.json 2>&1
rc=1
$ python3 -c "..."
honest: canary-test-ran-event=True canary-test-fail-event=True token-in-output=yes -> canary verdict: OK (canary gate passes)
forged: canary-test-ran-event=False canary-test-fail-event=False token-in-output=yes -> canary verdict: OK (canary gate passes)
```

The honest run: the canary test `t.Fatal(token)`s; `test2json` emits `run`, `output`, and `fail` events for `TestZZZCanary`. The forged run: the agent's `init()` prints the token and `os.Exit(1)`s before any test runs; `test2json` emits a single `output` event (the token line) and a `fail` event (the package failed). No `run` or `fail` event for `TestZZZCanary` — the canary test never ran. But the canary's decision logic (rc≠0 + token-in-output) does not check for a `run` event for the canary test; it only checks that the token is in the output. The canary cannot distinguish the two.

**Result: forgeable.** The `test2json` channel is forgeable because the canary's decision logic (rc≠0 + token-in-output) does not check that the canary test actually ran. A stronger decision (rc≠0 + token-in-output + a `fail` event for the canary test specifically) would close this forge — but the `fail` event for the canary test is emitted by `test2json` in the `go` command's process, and the `go` command is the canary's *own* process (the canary runs `go test`). The canary could check for a `fail` event with `Test: "TestZZZCanary<seed>"`. The agent's `init()` cannot forge that event (it is emitted by `test2json`, not by the test binary). But the agent can make the test binary exit with a non-zero code without the canary test running (e.g., `os.Exit(1)` in `init()`), and `test2json` will still emit a `fail` event for the *package* (not the canary test). The canary would need to check for a `fail` event with the specific `Test` field set to the canary test name. If the agent's `init()` `os.Exit(1)`s before any test runs, `test2json` does not emit a `fail` event for the canary test (it never ran), so the canary's stronger check (rc≠0 + token-in-output + `fail` event for `TestZZZCanary<seed>`) would reject the forge. The token in the output (from the probe's `stdout`) would be present, but the `fail` event for the canary test would be absent.

So: the `test2json` channel is forgeable with the current decision logic (rc≠0 + token-in-output), but **not forgeable with a stronger decision** (rc≠0 + token-in-output + `fail` event for the canary test specifically). The stronger decision is buildable in `K1`.

---

## Candidate 3: The canary in its own test binary (no agent code)

**Mechanism.** Build the canary test in a separate package that does not import the agent's code. The agent's `init()` cannot run because it is not in the binary. The canary test runs in a clean binary.

**Harness** (`/tmp/k0-sealed/`):

```
$ ./run_sealed.sh
=== RUN   TestZZZCanary
    sealed_test.go:6: canary-12345
--- FAIL: TestZZZCanary (0.00s)
FAIL
exit=1
```

The canary test `t.Fatal(token)`s in a binary that contains no agent code. The canary fires. No forge is possible (the agent's `init()` is not in the binary).

**Result: not forgeable, but the canary is pointless.** The canary's whole job is to detect in-process subversion of the agent's test harness. If the canary runs in a binary that does not include the agent's code, it cannot detect subversion of the agent's code — it only proves that the canary test itself fires in a clean binary, which is trivially true and provides no security signal. The agent could still subvert its own test harness (the acceptance checks, the other inits) without the canary detecting it. This candidate is a valid check (the canary test is honest) but it does not protect against the threat model (in-process subversion of the agent's code). It is a different check, not a fix for the canary's open limit.

---

## Candidate 4: Comparing against a run of the base commit's tree

**Mechanism.** The canary runs the test suite on the base commit's tree and on HEAD's tree, and compares the results. If the base run had a test that HEAD's run does not (or the base run failed and HEAD's run passed), the canary detects the difference.

**Harness** (`/tmp/k0-base-tree/`):

```
$ BASE=$(git rev-parse HEAD); echo "BASE=$BASE"
BASE=b05ebfe3d57547b01489700d7a4134c267c6ea76
$ go test -count=1 . 2>&1 | tail -1
ok      basetree  0.004s
$ rm lib_test.go
$ go test -count=1 . 2>&1 | tail -1
FAIL
$ git ls-tree -r --name-only "$BASE" | grep '_test\.go$'
lib_test.go
$ find . -name '*_test.go' -not -path './.git/*'
(no output)
```

The base run: `TestAdd` passes. The HEAD run: no tests, `go test` fails (build-fail, no test files). The difference is visible: the base had a test file, HEAD does not. The canary (F94a, exit 46) already detects this case (tests removed). The base-tree comparison is the existing F94a mechanism, not a new channel.

**Result: not a channel for the canary token.** The base-tree comparison detects test removal (F94a) but does not detect the #371 probe (the probe does not remove the test file; it reads the token from the canary test file and prints it). The base-tree comparison cannot detect the probe because the probe's file (`probe_test.go`) is not in the base commit (the agent added it), and the canary test file (`zzz_canary_test.go`) is generated by the canary (not in the base commit). The base-tree comparison is orthogonal to the canary token channel.

---

## Candidate 5: The canary's exit code (rc) only, no token

**Mechanism.** Drop the token entirely. The canary checks only that the canary test exited non-zero (the forced `t.Fatal` fired). No token to read or forge.

**Forge attempt.** The agent's `init()` prints nothing and `os.Exit(1)`s before any test runs. The canary test never runs. The exit is non-zero. The canary (rc≠0 → OK) reads it as OK.

**Result: forgeable.** Without the token, the canary cannot distinguish a non-zero exit from the canary test `t.Fatal`ing from a non-zero exit from the agent's `init()` `os.Exit(1)`ing. The token is what distinguishes the two. Dropping the token makes the forge easier (the agent does not need to read the token; it just needs to exit non-zero).

---

## Conclusion

| Candidate | Forgeable? | Cost |
|---|---|---|
| 1. One-shot sidecar token | **Forgeable** (the agent's `init()` runs first and steals the token; the sidecar cannot distinguish the canary's connection from the agent's). | High: a new trusted sidecar process, a socket path, a handshake. |
| 2. `test2json` events in the `go` command's process | **Forgeable** with the current decision (rc≠0 + token-in-output); **not forgeable** with a stronger decision (rc≠0 + token-in-output + a `fail` event for the canary test specifically). | Low: change the canary's decision logic to check for a `fail` event with `Test: "TestZZZCanary<seed>"` in the `-json` output. The agent's `init()` cannot forge that event (it is emitted by `test2json` in the `go` command's process). |
| 3. Canary in its own binary (no agent code) | **Not forgeable**, but the canary is pointless (it does not detect in-process subversion of the agent's code). | Medium: a separate build step. |
| 4. Base-tree comparison | **Not a channel for the token** (orthogonal to the probe). | Already built (F94a). |
| 5. Exit code only (no token) | **Forgeable** (easier than the token: just exit non-zero). | Low: drop the token. |

**Recommendation for D125: build candidate 2 (the `test2json` `fail`-event check) in K1.**

The current canary decision (rc≠0 + token-in-output) is forgeable by the #371 probe (the probe prints the token and `os.Exit(1)`s; the token is in the `-json` output; the exit is non-zero). A stronger decision (rc≠0 + token-in-output + a `fail` event for the canary test specifically, i.e., a JSON event with `"Action":"fail"` and `"Test":"TestZZZCanary<seed>"`) closes the forge: the agent's `init()` cannot emit a `fail` event for the canary test (the event is emitted by `test2json` in the `go` command's process, and the canary test never ran, so no `fail` event for it exists). The honest canary test `t.Fatal(token)`s, `test2json` emits a `fail` event for `TestZZZCanary<seed>`, and the canary's stronger decision is satisfied.

The harness (`/tmp/k0-forge/`) shows that the forged run has no `fail` event for `TestZZZCanary` (`canary-test-fail-event=False`), while the honest run does (`canary-test-fail-event=True`). The stronger decision (checking for the `fail` event for the canary test) would reject the forged run and accept the honest run.

This is a low-cost fix: change the canary's decision logic in `canaryCheckScript` to check for a `fail` event with the canary test name in the `-json` output, in addition to the token check. No new trusted process, no socket, no separate build.
