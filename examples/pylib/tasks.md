# pylib — sample tasks

Seed source: `examples/pylib` in the coxswain repo (docs/SAMPLES-PLAN.md §1).
The seeded in-cluster repo (S2) carries the **broken state**: `numutils`
defines `mean` and `mode` but is missing `Median` (and `Clamp`), so the
protected test modules (`tests/test_median.py`, `tests/test_clamp.py`) fail
at import time. The acceptance checks are the ONLY gate to `Succeeded`; the
test modules are protected paths, so weakening them trips the tamper gate
(B2/D24).

Stdlib only — no `pip install`, no network. The `verify.preset` is `none`
(the `go` preset would protect Go files, which pylib has none of); the test
files are named explicitly in `protectedPaths`.

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

## Task 1 — implement `Median`

**Goal (issue-style):**

> `numutils` is missing its `Median` function: the acceptance module
> `tests/test_median.py` imports `numutils.Median` and fails at import.
> Implement `median` in `numutils/stats.py` and expose it as `Median` from
> `numutils/__init__.py` (odd length → the middle value, even length → the
> average of the two middle values, empty sequence → `ValueError`, and the
> input must not be mutated). Do **not** change or weaken the tests —
> `tests/test_median.py` is protected; the acceptance checks must pass on
> the tests as written.

**Loop spec (paste into the Loop manifest):**

```yaml
spec:
  goal: >-
    Implement median in numutils/stats.py and expose it as Median from
    numutils/__init__.py. Odd length returns the middle value, even length
    returns the average of the two middle values, an empty sequence raises
    ValueError, and the input list must not be mutated. The acceptance
    module tests/test_median.py is a protected path: do not change or
    weaken it. Make `python3 -m unittest tests.test_median` pass on the
    tests as written.
  workspace:
    repo: http://gitea.samples.svc:3000/pylib.git
    ref: initial
    gitCredentialSecret: gitea-clone
  verify:
    acceptanceChecks:
      - python3 -m compileall -q .
      - python3 -m unittest tests.test_median
    preset: none
    protectedPaths:
      - tests/test_median.py
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
 system.

**Expected evidence:** `python3 -m compileall` exits 0;
`python3 -m unittest tests.test_median` reports all 5 tests passing; the
only change is the `numutils` package (the reference fix is `tasks/1.patch`);
`tests/test_median.py` untouched (tamper check 0).

---

## Task 2 — implement `Clamp`

**Goal (issue-style):**

> `numutils` is missing its `Clamp` function: the acceptance module
> `tests/test_clamp.py` imports `numutils.Clamp` and fails at import.
> Implement `clamp` in `numutils/stats.py` and expose it as `Clamp` from
> `numutils/__init__.py` (value inside `[low, high]` is returned as-is,
> below `low` returns `low`, above `high` returns `high`, `low > high`
> raises `ValueError`). Do **not** change or weaken the tests —
> `tests/test_clamp.py` is protected; the acceptance checks must pass on
> the tests as written.

**Loop spec:**

```yaml
spec:
  goal: >-
    Implement clamp in numutils/stats.py and expose it as Clamp from
    numutils/__init__.py. clamp(value, low, high) returns value as-is when
    it is inside [low, high], low when below, high when above, and raises
    ValueError when low > high. The acceptance module tests/test_clamp.py
    is a protected path: do not change or weaken it. Make
    `python3 -m unittest tests.test_clamp` pass on the tests as written.
  workspace:
    repo: http://gitea.samples.svc:3000/pylib.git
    ref: initial
    gitCredentialSecret: gitea-clone
  verify:
    acceptanceChecks:
      - python3 -m compileall -q .
      - python3 -m unittest tests.test_clamp
    preset: none
    protectedPaths:
      - tests/test_clamp.py
  agent:
    image: coxswain-runner:latest
    endpointSecretRef: vllm-no-auth
    modelEndpoint: 192.168.1.20:8000
    model: qwen3.8-27b
  # No policyRefs (D46, as task 1).
```

**AgentPolicy:** none (D46, same as task 1).

**Expected evidence:** `python3 -m compileall` exits 0;
`python3 -m unittest tests.test_clamp` reports all 4 tests passing; the
only change is the `numutils` package (the reference fix is `tasks/2.patch`);
`tests/test_clamp.py` untouched (tamper check 0).
