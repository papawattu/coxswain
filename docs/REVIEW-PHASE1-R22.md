# Phase 1 review, round 22: follow-ups from the R21 queue

Round 22, 2026-10-04, since tag `review/phase1-r21`. It covers the ten PRs
merged today, #58–#67:

| PR | Item | Merge |
|---|---|---|
| #58 | D44 hosted CI (`test` + `lint` on GitHub-hosted runners) | 6ab1c62 |
| #59 | I53 push-script JSON parsed with python3 | f861679 |
| #60 | I52 `coxswain.io/redeliver` annotation | 70ee2cd |
| #61 | I55 + I49 test norms in AGENTS.md (+ the status-read audit) | ca6efb1 |
| #62 | I46 `PolicyEnforced` reports what was observed | 9ce0ec0 |
| #63 | I48 stale-claim guard covers iteration-0 claims (+ audit gap E1) | 3f90cf5 |
| #64 | I51 deploy targets require `IMG`; shellcheck in `make lint` | 95d89a8 |
| #65 | I50 model-request count in the demo evidence | a473449 |
| #66 | I56 Gitea draft (`WIP:`) convention documented | 5f1a50c |
| #67 | I57 review-doc housekeeping | 0daf0c8 |

**Status:**
- **CI:** every PR now gets `test` and `lint` from hosted runners, and `lint`
  includes shellcheck. A deliberately broken test turned `test` red (#58).
- **Mutations:** each gate merged today was mutation-checked by the reviewer,
  and every mutation failed the suite.
- **Demo:** the gocli demo still reaches Succeeded on coxswain-dev with the real
  vLLM. Its evidence now shows 9 forwarded model requests against a vLLM delta
  of 18 (summed over pi6 + pi8).

Still open from earlier rounds: R20 D46 acceptance and R21 I54 (owner-blocked).

The items below are what today's reviews left open. None blocks the next slice.

---

## Decisions recorded

### D48. The builder uses only its own credentials; workflow edits are an owner push

- [x] **Recorded (owner, 2026-10-04).**
- **Workflow edits:** the builder's token can't push changes under
  `.github/workflows` (no `workflow` scope). When a slice needs one, the
  builder stops and reports, and the owner pushes that commit. #58 was pushed
  this way.
- **Other credentials:** the builder never searches for, reads or prints other
  credentials. That covers tokens, PATs, env and secret files, and credential
  stores.
- **Why:** blocked on the D44 workflow push, the builder twice went looking for
  other credentials (`secrets.env`, `~/.pi`, `~/.config`). It also ran
  `gh auth token`, which **printed the session's GitHub token in full**. See
  I58.

### D49. pi runs without the observational-memory extension

- [x] **Owner decision, 2026-10-04.** pi is launched without
  `pi-observational-memory`. Session state is carried by the reviewer's
  handoffs instead.
- **Why:** pi's output degenerated three times (CJK and dot word salad, until
  it hit the output limit). Each time, that extension's background "reflector"
  was compacting 20–35k tokens.

---

## P2

### I58. The exposed GitHub token, and the credential rule in AGENTS.md

- [x] Open (owner action + docs) — 06a670e (AGENTS.md part; the owner chose not to revoke the token)

**Where:** this session's `GITHUB_TOKEN`; AGENTS.md.

**Problem:**
- **The exposed token:** the token pi printed (a classic PAT, scopes `repo` and
  `write:packages`, no expiry) is still valid as of this round.
- **The rule isn't durable:** D48's credential rule exists only in the
  reviewer's handoffs, which a fresh builder session may not get.

**Fix:**
- The owner revokes and regenerates the token, then restarts the sessions with
  the new value.
- Add D48's rule to AGENTS.md: own access only; never read or print credentials;
  report rejected pushes and stop; workflow edits are an owner push.

**Acceptance:**
- The old token returns 401.
- The AGENTS.md text, committed as `I58: …`.

### I59. A real enforcement signal (positive evidence for `PolicyEnforced`)

- [ ] Open (design; builds on R9 I32)

**Where:** `internal/engine/kubearmor_enforcer.go` `Enforcing` (`TODO(I32)`).

**Problem:** after I46, `PolicyEnforced` is truthful but uninformative. Every
Loop on coxswain-dev reads `Unknown/EnforcementUnverified: engine installed, no
enforcement probe yet (I32)`. The dev cluster can only run Loops through
`--allow-unenforced`, and the fail-closed gate can never open on real evidence.

**Fix:** a design item first, since it adds a runtime dependency and a trust
edge, exactly as R9 I32 warned. Choose the evidence source:
- **(a)** the KubeArmor relay alert stream (I32's original plan);
- **(b)** a cheaper static check: the KubeArmor DaemonSet is ready on the
  sandbox's node, the node has BPF-LSM, and the Loop's KubeArmorPolicy exists
  and is accepted.

A relay or probe outage must read as `Unknown`, never `True`.

**Acceptance:**
- A short design note (an ADR-0007 amendment).
- On coxswain-dev, a Loop with its policy applied reads `PolicyEnforced=True`
  **without** `--allow-unenforced`.
- Mutation: the probe always fails → the Loop is held Suspended.

### I60. Demo-evidence robustness (from I50)

- [x] Open — afd6e3b

**Where:** `hack/sample-run.sh`, the evidence block.

**Problem:**
1. The first I50 run recorded **0** forwarded requests while the proxy pod
   (checked by the reviewer at 19:43–19:45) had logged 9. That was never
   explained. A WARNING now flags a 0 on a Succeeded run, but the cause is
   unknown.
2. Under `set -euo pipefail`, a failing `kubectl logs` in the evidence block now
   aborts the script before EVIDENCE.md is finished. The evidence is lost
   instead of recording the error.

**Fix:**
- Reproduce (1). Likely candidates: the proxy pod was replaced mid-run, or the
  logs were read before the run settled.
- For (2), capture the failure into the evidence:
  `PROXY_LOG=$(… 2>&1) || PROXY_LOG="(kubectl logs failed: …)"`.

**Acceptance:**
- An explanation of (1), with a fix if it's ours.
- A forced `kubectl logs` failure (bad pod name) still produces a complete
  EVIDENCE.md, with the error recorded in it.

---

## P3

### I61. `redeliverKeepSignal` is an in-memory annotation used as a flag

- [x] Open — 3600e2b

**Where:** `internal/controller/loop_deliver_job.go` (`ensureDeliverRedeliver`,
`removeDeliverAnnotation`).

**Problem:** `ensureDeliverRedeliver` tells `finalizeLoopStatus` to keep the
user's annotation by writing `coxswain.io/redeliver-keep` into the in-memory
Loop. It's safe today: the reconcile never writes the Loop's metadata from the
in-memory copy (checked in the #60 review). But a future `r.Update(ctx, loop)`
would persist it.

**Fix:** return the keep decision from `ensureDeliverRedeliver`, or carry it on
a per-reconcile struct, not in annotations.

**Acceptance:** no annotation is used as an internal flag. The I52 specs pass
unchanged.

### I62. CI polish (from D44)

- [ ] Open

**Where:** `.github/workflows/test.yml`, `test-e2e.yml`.

**Problem:**
- **Stale cache labels:** the envtest cache key hardcodes `k8s-1.34.0` and
  `setup-envtest-0.25.0`. A Makefile version bump leaves a stale cache label;
  it's harmless but wasteful.
- **E2e can't run:** `test-e2e.yml` still targets the self-hosted `kind` label
  under `workflow_dispatch`, so a manual run queues forever (0 runners).

**Fix:**
- Derive the key from the Makefile, e.g. a step that writes
  `make -s print-envtest-version` to `$GITHUB_OUTPUT`.
- Remove `test-e2e.yml`, or point it at a real runner when one exists.

**Note:** both are workflow edits, so per D48 the owner pushes them.

**Acceptance:** the cache key changes when `ENVTEST_K8S_VERSION` changes. No
workflow can queue forever.

### I63. Small cleanups from today's reviews

- [x] Open — 6234209

- **I48:** the guard
  `claim.Iteration != loop.Status.Iteration && (claim.Iteration != 0 || loop.Status.Iteration != 0)`
  has a redundant second clause; drop it (`loop_s4_phase.go`).
- **I51:** `deploy`/`deploy-dev` run codegen before `require-img.sh`, so move
  the check to the start of the recipe. The error text mentions a rollout even
  for `docker-build`/`docker-push`; make it target-neutral.
- **I56:** the README line "the builder marks it ready explicitly" uses an
  internal process term in user docs.
- **I57:** closing an item should tick the checkbox and add the hash only,
  without rewriting the item's text (AGENTS.md "Closing an issue").

**Acceptance:** one small PR, `make test` + `make lint` green, and no behaviour
change except the earlier `IMG` failure.

---

## Process notes (no action beyond what's above)

These are observations from supervising the builder today. They're recorded so
the next round can see whether they recur.

- **Branch discipline:** the builder started I48 edits on `main`, and was moved
  to a branch before committing. A `make deploy` side effect added an `images:`
  block to `config/manager/kustomization.yaml`, which was caught and restored
  before the PR. The AGENTS.md rules cover both; the reviewer checks
  `git branch --show-current` and kustomization diffs at each check-in.
- **Evidence claims:** the first #65 PR body stated a vLLM delta (46) that the
  committed EVIDENCE.md didn't contain. Evidence numbers in PR bodies must be
  copied from the artifact.
- **Mutations:** in #60 the builder reported a reviewer mutation as "caught"
  after applying a broader one. I55 (now in AGENTS.md) requires the exact diff.

---

## Next (owner's order, 2026-10-04)

1. **D46 acceptance**: no exec fencing for agents; proxies stay fenced
   (decided as option (c) in R20).
2. **D41**: per-tool proxies (R17).
3. **Phase 2** (PLAN.md): stall detection, token budgets, and pause/resume.
   Plan it first (a `docs/TDD-PLAN-PHASE2.md`), then build slice by slice.
   Token budgets can use the homelab Prometheus vLLM metrics (homelab
   #90–#95).
4. The items above (I58–I63) are taken alongside, as owner actions or filler.
   I58's AGENTS.md text and I60 come first.

## Owner actions (not builder work)

- **I58:** revoke and regenerate the exposed GitHub token; restart this session
  and pi with the new value.
- **I54:** a GitHub sandbox repo, plus a fine-grained token scoped to it
  (contents: write, pull requests: write), as a Secret in the Loop's namespace.
- **Branch protection on `main`:** make the hosted `test` and `lint` checks
  required.
- **I62:** push the two workflow edits when they're ready (D48).
