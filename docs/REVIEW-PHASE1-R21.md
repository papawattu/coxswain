# Phase 1 review, round 21: follow-ups from I47 and S6 delivery

Round 21, 2026-10-04, since tag `review/phase1-r20`. It covers I47 (#55,
b8cac37) and S6 delivery (#56, a5cec25).

**Status:** delivery works end to end on coxswain-dev with the real vLLM.
Loop `samples/gocli-task1` reached **Succeeded** on iteration 0. The deliver
Job then pushed the verified commit `b1966ff6` to branch
`coxswain/gocli-task1` and opened Gitea PR #1:

- The PR is open, head `b1966ff6` (== `status.currentVerify.verifiedCommit`),
  base `initial`.
- Its `html_url` equals the recorded `status.delivery.prURL`
  (`http://gitea.samples.svc:3000/samples/gocli/pulls/1`). The reviewer
  checked this through the Gitea API.
- A re-delivery reused the PR instead of opening a second one.

The final S6 review ran nine mutations against the delivery gates; each one
fails the suite. I47 is merged; the verify Job's artifact check rejects new
files over 1 MiB or binary.

The items below are what those reviews left open. D47 records an owner
decision. None blocks the next slice (D44); I52 and I53 should land before
the first live GitHub delivery (I54).

---

## Decisions recorded

### D47. Loops may clone from and deliver to GitHub (amends R18)

- [x] **Owner decision, 2026-10-03:** "the github access from the agents
  should not be blocked". The chosen scope is **clone + deliver to GitHub**.
  This overrides R18's "in-cluster Gitea, never GitHub".

What it means:

- `spec.workspace.repo` may be a GitHub repo. S6 delivery pushes the verified
  branch and opens the PR on GitHub.
- The GitHub token stays in a Secret that only the operator's Jobs mount
  (deliver clone-base and push; the workspace init clone). The agent never
  holds it (ADR-0006).
- NetworkPolicy can't name hostnames, so git and API traffic to an external
  repo host goes through the per-Loop egress proxy. Its allowlist is exactly
  the repo host, plus `api.github.com` for a GitHub repo.
- Delivery never pushes to the default branch: only `<branchPrefix><loop>`,
  as a draft PR.
- Live GitHub tests run only against an owner-provided sandbox repo, never
  against papawattu/coxswain.

Why: dogfooding coxswain on its own GitHub repo, without a Gitea mirror.

Implemented in S6 (#56). The live GitHub test is I54.

---

## P2

### I52. A failed delivery can only be retried by hand

- [ ] Open

**Where:** `internal/controller/loop_deliver_job.go`
(`deliverReadbackChanged`, `deliveryRequested`); the deliver Job has
`backoffLimit: 0`.

**Problem:** every delivery failure is terminal: a non-zero push, an invalid
termination message, or a failed init container. The Loop shows
`Delivered=False/DeliveryFailed` and stays there. The condition message tells
the operator to "clear status.delivery + the condition to re-run delivery".
That means a `kubectl patch --subresource=status --type=json` plus deleting
the failed Job. During S6 that manual reset was needed on each kind re-delivery (s6e, s6f, s6g). A
transient failure has no supported way back:

- Gitea or GitHub briefly unreachable;
- an expired token that was then rotated.

**Fix:** a supported re-delivery trigger. The reviewer recommends an
annotation, because the plugin doesn't exist yet: on
`coxswain.io/redeliver=<anything>`, the operator:

1. deletes the failed deliver Job (Background propagation);
2. clears the Delivered condition (status.delivery is already nil for a
   failed delivery);
3. removes the annotation, so the next reconcile creates a fresh Job for
   the same pinned verifiedCommit.

It must never re-deliver a Loop whose status.delivery is already set for the
current verifiedCommit. Re-delivery stays idempotent: the existing PR is
reused. Later, `kubectl cox redeliver` sets the annotation.

**Acceptance:**
- An envtest drives a failed delivery, adds the annotation, and asserts the
  following, re-reading from the API server:
  - the old Job is gone;
  - a new Job exists;
  - the Delivered condition is InProgress;
  - the annotation is removed.
- A second envtest: the annotation on a Loop that is already Delivered for
  its verifiedCommit is removed and does nothing.
- Mutation: skip the "already delivered" check → FAIL.

### I53. The push script parses JSON with `sed`

- [ ] Open

**Where:** `internal/controller/loop_deliver_job.go`, the push container
script. It reads `html_url`, `number` and `default_branch` with
`sed -n 's/.*"field"...'`.

**Problem:** greedy `sed` over a JSON body picks the **last** match, not the
top-level field. On the s6f kind run this recorded the repo's `html_url`
(`…/samples/gocli`) as the PR URL. The operator's exact-path validation
rejected it, which is correct, but the delivery failed. The same pattern
reads `default_branch` from `GET /repos/{owner}/{repo}`. That body contains
nested repository objects (`parent`, `template_repository` on GitHub) with
their own `default_branch`, so a fork can return the **parent's** default
branch. The refusal then compares against the wrong branch. It still fails
closed only if the value is empty.

**Fix:** parse with `python3 -c 'import json,sys; …'` and read the
**top-level** keys only. The push container runs the runner image, which has
`python3` (checked: `coxswain-runner:dev` has `python3`, `curl` and `git`; it
doesn't have `jq`).

**Acceptance:** push-exec test cases where the fake API's responses contain
nested objects with their own `html_url`, `number` and `default_branch`
**after** the top-level ones. The recorded values are the top-level ones.
These cases must FAIL on a5cec25.

### I54. No live GitHub delivery yet

- [ ] Open (blocked on the owner action below)

**Where:** S6's GitHub path, covered today only by:
- the httptest fake-GitHub provider test (draft PR, Bearer auth, idempotent
  GET);
- the proxy-env and allowlist envtests.

**Problem:** these GitHub-only paths have never run against the real
service:
- `api.github.com` through the egress proxy (CONNECT and TLS);
- Bearer auth with a fine-grained token;
- GitHub's `/pull/<n>` `html_url`;
- the `draft: true` create;
- the GitHub default-branch lookup;
- the workspace init clone of a GitHub repo via the proxy.

The S6 kind evidence also ran on `coxswain-controller:s6g` (6399cd2). a5cec25
adds only the fail-closed default-branch path on top. That's covered by tests
but hasn't run live.

**Fix:** once the owner provides a sandbox repo and token, run a Loop whose
`spec.workspace.repo` is that sandbox repo, using a controller image built
from `main`.

**Acceptance:**
- Evidence: the controller digest equals an image built from main's HEAD.
- The Loop reaches Succeeded and `Delivered=True`.
- `status.delivery.prURL` is `https://github.com/<owner>/<repo>/pull/<n>`.
- The PR is a draft, its head is the verified commit, and the base is the
  sandbox's default branch.
- The egress proxy log shows only the repo host and `api.github.com`.
- **Never** papawattu/coxswain.

### I55. Test norms: execute embedded scripts; mutations must be exact

- [ ] Open (AGENTS.md "Test norms", extends I49)

**Problem:**

1. The deliver Job's shell scripts reached kind with **four** bugs that no
   test ran:
   - import-agent re-initialised over the base checkout;
   - the push script used `AUTH` before setting it (twice);
   - the push image had no `curl`.

   Each cost a kind round-trip of about 15 minutes. Execution tests now run
   the real import and push scripts against local repos and a fake API.
2. In round 2 the builder reported a reviewer mutation as "caught". Its
   scratch mutation had deleted more code (the in-memory `setCondition` as
   well as the persist), so a different spec failed. The reviewer's exact
   mutation, which only drops the persist, still passed the suite.

**Fix:** add to AGENTS.md "Test norms":
- Any shell script embedded in Go (Job or init-container scripts) has an
  execution test. It runs the real generated script with only path or host
  constants substituted, before any kind run.
- When a reviewer names a mutation, the builder applies **exactly** that
  diff in a scratch worktree and records the result. A broader mutation
  doesn't count.

**Acceptance:** the AGENTS.md text, committed with the subject `I55: …`.

---

## P3

### I56. Gitea ignores the API draft flag

- [ ] Open

**Where:** the push script's create-PR call for the Gitea provider (49f61da).

**Problem:** Gitea's create-PR API ignores `draft`. A `draft: true` delivery
to Gitea opens a ready PR. The S6 fix prefixes the title with `WIP: `, which
is Gitea's draft convention. That works, but it's undocumented outside the
code, and the existing gocli PR #1 (opened before the fix) isn't marked.

**Fix:** document the provider difference in the delivery section of the
docs, and in the `spec.delivery.draft` field comment so the CRD docs carry it.

**Acceptance:** the doc text, plus a field-comment update (`make manifests`
regenerates the CRD description).

### I57. Housekeeping from R20 and S6

- [ ] Open

- Tick R20's I47 box with b8cac37. PR #55 merged, but the box is still open.
- Builder tree: pi's checkout is still on the deleted branch
  `slice/s6-delivery` (ca1f756). Switch to `main` and pull before the next
  slice.
- Stale scratch worktrees: `/tmp/cox-s6-rev` (5e3f28f) and
  `/tmp/cox-s4-mut` (6a3b22e). Remove them with `git worktree remove`; both
  are scratch.

**Acceptance:** the R20 tick commit, plus `git worktree list` showing only
the main tree.

---

## Carried forward (still open from R20)

D46 acceptance (no exec fencing for agents; proxies stay fenced), I46
(`PolicyEnforced` misreports enforcement), I48 (the stale-iteration guard
exempts iteration-0 claims), I49 (test-norm additions), I50 (demo evidence has
no model-request count), I51 (deploy hygiene). The text is unchanged; see
`docs/REVIEW-PHASE1-R20.md`.

---

## Next (per R18, amended by D47)

1. **D44:** hosted CI runners for `make test` and `make lint`. All
   verification is currently local; the self-hosted runners have 0 capacity.
2. I52 and I53, then I54 (live GitHub delivery) once the owner provides the
   sandbox repo.
3. D46 per the owner's decision; then D41.

## Owner actions (not builder work)

- **I54:** provide a GitHub sandbox repo and a fine-grained token scoped to it
  (contents: write, pull requests: write), as a Secret in the Loop's
  namespace. Never a token that can write papawattu/coxswain.
- Housekeeping from R20 is still pending: the four stale envtest
  `kube-apiserver` processes on the devbox (PIDs 450505, 463475, 483317,
  502079, as of 2026-10-03). Safe to kill.
