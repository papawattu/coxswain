# Phase 1 review, round 20: follow-ups from the samples build (S4, S5a, S5b)

Round 20, 2026-10-03, since tag `review/phase1-r19`. It covers the build slices
merged overnight: S4 (#51, 88dd526), S5a (#52, 3dd1f4f) and S5b (#53,
cd26439).

**Status:** the owner's MVP bar is met. On coxswain-dev, with the real vLLM
(192.168.1.20:8000, qwen3.8-27b), `make sample-run APP=gocli TASK=1` took Loop
`samples/gocli-task1` through Planning → Implementing → Verifying →
**Succeeded** on the first iteration:

- The verify Job ran clone-base, import-agent, tamper (`clean`), check-0
  (`go build`) and check-1 (`go test -run TestRound`). All exited 0.
- The agent's change, read by the reviewer from the workspace PVC (read-only),
  touched only `round.go` (round half away from zero). The tests were
  untouched.

The items below are what that run and the three PR reviews left open. None
blocks the MVP; I46 and I49 should land before the delivery slice.

---

## P1: owner decision

### D46. Exec fencing can't cover an agent shell

- [x] Owner decision (2026-10-03): **(c)**. No exec fencing for agents in the
  MVP. Rely on the network fence, credential isolation and the verify Job.
  Exec fencing stays for the proxies; D41 per-tool proxies come later.
- [ ] Implemented (the Acceptance below)

**Where:** `AgentPolicy.spec.exec` (`api/v1alpha1/agentpolicy_types.go`) and
its KubeArmor translation. Related to D41 (tools on AgentPolicy).

**Problem:** S5b's first demo run used the task's AgentPolicy (`exec:
[/usr/local/go/bin/go, /usr/bin/git]`). The operator translated it into a
KubeArmor policy with `action: Block` that allows only those two paths.
KubeArmor enforced it: `kubectl exec … sh` returned `exec /usr/bin/sh:
permission denied`. The runner runs every tool call through `/bin/sh -c`, and
`go build` spawns `compile`, `link` and `asm`, so the agent wedged. It ran for
30 minutes with no model request.

An exact-path allow-list can't describe an agent shell: the set of binaries a
shell command spawns is open-ended. The demo now runs **without**
`policyRefs` (the example policy is kept but deliberately not referenced).
With no `policyRefs`, the generated KubeArmor policy is a bare `Block` with no
process rules, so exec isn't restricted.

**Options:**

| | Option | Pro | Con |
|---|---|---|---|
| (a) | Directory allow-lists (`matchDirectories`, recursive) in AgentPolicy, e.g. `/usr/local/go/`, `/usr/bin/`, `/bin/` | Small API change; usable today | Coarse: allowing `/usr/bin/` allows most of the image |
| (b) | Allow the shell, deny a list (network tools, package managers, `curl`, `wget`, `nc`) | Practical for agents | Deny-lists are leaky |
| (c) | No exec fencing for agents. Rely on the network fence (egress proxy + NetworkPolicy), credential isolation (ADR-0006) and the verify Job; fence exec only for the proxies, as today | Matches what actually protects the system; no false sense of security | Exec inside the sandbox is unrestricted |
| (d) | D41 per-tool proxies: the agent gets no general shell; tools are proxied | Strongest | Large; it's the D41 plan, not MVP |

**Recommendation:** (c) for the MVP, documented in the threat model. Then (d)
as D41 lands. Avoid (b).

**Acceptance:** the decision is recorded. For (c): AgentPolicy docs say exec
fencing applies to proxies, not agents; the task-1 example policy is either
deleted or rewritten to match; and a Loop with a `policyRefs` exec list
**fails fast** with a clear condition (rather than wedging) if the list doesn't
include the runner's shell.

---

## P2

### I46. `PolicyEnforced` says "NOT enforced" while KubeArmor is enforcing

- [x] Fixed in 9ce0ec0 (merge of #62)

**Where:** `internal/controller/loop_controller.go`, `enforcementStatus`
(~L700), and the condition text from the `--allow-unenforced` escape hatch.

**Problem:** on coxswain-dev every Loop reports `PolicyEnforced=False reason
EnforcementDisabled: --allow-unenforced is set: the Loop runs but is NOT
enforced`. During S5b, KubeArmor **was** enforcing that Loop's policy
(the D46 exec denial). `enforcementStatus` returns `EnforcementDisabled`
whenever the Enforcer is nil or reports not-enforcing **and** the flag is set.
So the condition describes the flag, not the cluster. The demo's honesty
requirement (SAMPLES-PLAN §5: gate conditions must be truthful) is violated in
the reassuring-but-wrong direction now. The same code would also report a
genuinely unenforced Loop the same way, so the condition can't distinguish the
two.

**Fix:** separate *what was observed* from *what was allowed*:

- If the Enforcer reports enforcing → `PolicyEnforced=True` (the flag is
  irrelevant).
- If the Enforcer reports not-enforcing → `False / EnforcementDisabled`, with
  the Enforcer's own reason in the message.
- If the Enforcer is nil or can't tell → `Unknown / EnforcementUnverified`
  ("no engine probe; the Loop runs because --allow-unenforced is set").

Find out why the Enforcer reports not-enforcing on coxswain-dev while the
KubeArmor policy is applied. Is it nil (engine not wired in the dev overlay)?
A probe mismatch?

**Acceptance:**
- envtests for all three cases. Mutation: always returning
  `EnforcementDisabled` when the flag is set → FAIL.
- On coxswain-dev, a Loop whose KubeArmor policy is applied reads
  `PolicyEnforced=True`, or `Unknown/EnforcementUnverified` with a reason that
  names why. Never "NOT enforced".

### I47. The runner commits build artifacts into the verified commit

- [x] Fixed in b8cac37 (merge of #55)

**Where:** `runner/phase.go` `commitWorkspace` (`git add -A -- ':(exclude).coxswain'`);
the gocli seed (`examples/gocli`, no `.gitignore` entry for the binary).

**Problem:** in S5b's successful run the verified commit (69b361c → 286c2ec)
contains `round.go` **and a 2.5 MB `gocli` binary**, which the agent built
while testing. Everything the agent leaves in the workspace becomes part of
the commit that is verified and, once delivery exists, pushed. That means
build outputs, caches, and anything else written into the tree.

**Fix:**
- The seeds ignore their build outputs (`.gitignore` per sample app).
- `commitWorkspace` stages only paths that aren't ignored, and refuses (claim
  `blocked`, with a reason) any single new file over a size cap (e.g. 1 MiB)
  or any binary (a NUL byte in the first 8 KiB).
- The claim's summary lists the staged paths.

**Acceptance:** a runner unit test where the workspace contains a built binary
and a source edit: only the source edit is committed. Mutation: `git add -A`
without the filter → FAIL. On kind, `git diff --stat base..verified` for
gocli task 1 shows `round.go` only.

### I49. Test-norm additions (from the S5a regression and the builder's mutation hygiene)

- [x] Fixed in ca6efb1 (merge of #61)

**Where:** AGENTS.md "Test norms (R16 I43)"; `internal/controller/loop_verify_job.go`
`verifyOutcome` (fixed in #53).

**Problem:**
1. `verifyOutcome` mapped a **still-running** check container to "iterate"
   (`check-0 failed (exit 0)`). Every passing verify then failed whenever the
   operator polled mid-check, and a correct fix could never reach `Succeeded`.
   The envtests (and the reviewer's mutations) only modelled terminal states,
   so S5a merged with the bug, and it surfaced in S5b's demo. Fixed in #53
   with a pending-state spec.
2. Process slips during S5a/S5b:
   - a mutation was left applied in the real working tree (it was caught
     before a commit);
   - a commit was amended and force-pushed (67fbf0b → 40aeb31);
   - a destructive `rm -rf` of the evidence plus `> /dev/null` of the run log
     was proposed.

**Fix:** add to AGENTS.md Test norms:
- Every decision that reads pod or container status gets a spec for **each
  in-progress state** (Waiting, Running) as well as each terminal state,
  asserting "no decision".
- Mutations run in a **scratch worktree** (`git worktree add`), never in the
  working tree. Record the result; delete the worktree.
- Kind-run logs and generated evidence are never discarded or redirected to
  `/dev/null`.

**Acceptance:** AGENTS.md updated. A grep across `internal/controller` for
status-reading decisions (`State.Terminated`, `.State.Waiting`) shows each has
a pending-state spec, or this item lists the exceptions.

---

## P3

### I48. The stale-iteration claim guard exempts iteration-0 claims

- [x] Fixed in 3f90cf5 (merge of #63)

**Where:** `internal/controller/loop_s4_phase.go` (~L503): `if claim.Iteration > 0
&& claim.Iteration != loop.Status.Iteration`.

**Problem:** first-cycle claims carry iteration 0 (phase-init writes
`status.iteration`, which starts at 0), and the guard exempts them. After the
first iterate (0 → 2), a stale first-cycle claim on the old pod isn't ignored
by this guard. On kind the desired-phase recycle replaced the pod in time:
every iteration produced a new commit over 10 iterations. But the race isn't
covered by a spec.

**Fix:** start the iteration marker at 1, or treat a 0 claim as stale once
`status.iteration > 0`.

**Acceptance:** an envtest with a stale iteration-0 `{Implementing, success}`
claim present after the first iterate: no re-advance, no verify Job. Mutation:
drop the guard → FAIL.

### I50. Demo evidence has no model-request count

- [x] Fixed in a473449 (merge of #65)

**Where:** the model-proxy dev stand-in (`cmd/proxy-standin`), and
`hack/sample-run.sh` (the EVIDENCE.md "Model proxy" section).

**Problem:** the stand-in logs one line at start-up, not per request, so
EVIDENCE.md can't show that the agent actually called the model, or how often.

**Fix:** log one structured line per forwarded request (method, path, status,
duration; no bodies, no headers). EVIDENCE.md reports the count.

**Acceptance:** EVIDENCE.md for gocli task 1 shows a non-zero forwarded
request count that matches the vLLM `request_success_total` delta over the
run.

### I51. Deploy hygiene: a dev deploy without an image tag strands the rollout

- [x] Fixed in 95d89a8 (merge of #64)

**Where:** `make deploy-dev` / `config/dev`, and `hack/sample-run.sh`.

**Problem:** re-applying the dev overlay without setting the controller image
produced a rollout on `controller:latest`, which doesn't exist in kind. It
stayed Pending while the old pod kept running with a stale `--runner-image`.
The Loop then ran `sleep infinity` instead of the runner (S5b's second wedge).
#53 added a preflight in sample-run, but `make deploy-dev` still allows it.

**Fix:** `make deploy-dev` fails unless `IMG` is set. Fix the shellcheck
warnings in `hack/sample-run.sh` (unused `KUBECTL`, `POLICY_NAMES`) and add
shellcheck for `hack/*.sh` to `make lint`.

**Acceptance:** `make deploy-dev` without `IMG` exits non-zero with a clear
message; `make lint` runs shellcheck clean.

---

## Decisions recorded

### D40 (R16, amended 2026-10-02): marking PRs ready

- [x] **Amended by the owner:** "use the API workaround". The builder marks
  its PRs ready with the GraphQL mutation `markPullRequestReadyForReview` (see
  AGENTS.md); no token change. This supersedes R18's "(a) grant
  `pull_requests:write`". The R18 owner action to issue that token is
  withdrawn.

---

## Next (per R18)

1. **Delivery** to the in-cluster Gitea (branch push + PR from the verified
   commit). This is the slice after S5. Do I47 first, so delivered commits
   carry no build artifacts.
2. **D44:** hosted CI runners for `make test` and `make lint`.
3. D46 per the owner's decision; then D41.

## Owner actions (not builder work)

- ~~D46: choose an option.~~ Decided: (c), 2026-10-03.
- Housekeeping: four stale envtest `kube-apiserver` processes have been
  running on the devbox for 1–5 days (PIDs 450505, 463475, 483317, 502079,
  as of 2026-10-03). Safe to kill.
