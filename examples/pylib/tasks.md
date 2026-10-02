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

**Git credential:** each spec's `workspace.gitCredentialSecret` names a Secret
that MUST carry a key named `.git-credentials` with content
`http://<user>:<pass>@<git host>` (for the seeded Gitea:
`http://samples:password@gitea.samples.svc:3000`). The operator mounts ONLY that
key (`items: [{key: '.git-credentials', path: '.git-credentials'}]`) and passes
it to git per command; a Secret without that key makes the init container fail
loud at pod start. See docs/SAMPLES-PLAN.md S3 for the rationale.

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
  policyRefs:
    - pylib-task-1
```

**AgentPolicy (the fenced tools the task needs):**

```yaml
apiVersion: coxswain.wattu.com/v1alpha1
kind: AgentPolicy
metadata:
  name: pylib-task-1
spec:
  exec:
    - /usr/bin/python3        # the runner image carries python3
    - /usr/bin/git
  network: []                     # no external egress; stdlib only
```

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
  policyRefs:
    - pylib-task-2
```

**AgentPolicy:** same as task 1 (`pylib-task-2`, same spec).

**Expected evidence:** `python3 -m compileall` exits 0;
`python3 -m unittest tests.test_clamp` reports all 4 tests passing; the
only change is the `numutils` package (the reference fix is `tasks/2.patch`);
`tests/test_clamp.py` untouched (tamper check 0).
