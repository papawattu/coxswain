# Phase 1 review, round 57: v0.2.0 is released for amd64 and arm64; the owner's grill sets Phase 8

**Scope:** main a3c5324 (R56's scope end) → a7eed6b, 11-10 16:44Z to 22:13Z. The v0.2.0 release ran on 11-11, 18:13Z to 18:19Z. The owner's grill of the Phase 7 decisions was on 11-11.

Merged in this window:

| PR | What | Squash |
|---|---|---|
| #476 | R56 (owner merge and tag `review/phase1-r56`) | `0257039` |
| #477 | C1: the release workflow builds linux/amd64 + linux/arm64 (the owner pushed the commit) | `55b064b` |
| #478 | R1: the v0.2.0 notes, multi-arch images are no longer pending | `a7eed6b` |

## Closed

R56's boxes are ticked in `docs/REVIEW-PHASE1-R56.md` in this commit:

- **I269** (a cause stated before it was shown): this window states no unshown cause. One output stays unexplained and is listed under "What I did not verify": `docker manifest inspect` said "no such manifest" while the registry serves the index.
- **I270** (the reviewer's slips): this window has no wrong API path in a brief, no FETCH_HEAD and no lines-for-matches count. It does have a new slip (I271).

## v0.2.0 (D129 = (a))

- **The workflow push:**
  - The owner's first push was rejected, because the `GITHUB_TOKEN` environment variable lacks the `workflow` scope.
  - The owner then logged gh in with that scope and pushed with the variable unset. The reviewer did not look for or read any credential (D48).
  - #477's diff is `release.yml` only, 4 lines added (`git diff --stat origin/main...origin/slice/c1-workflow`).
- **The tag:** the owner said "tag v0.2.0". It is a lightweight tag on `a7eed6b`, like v0.1.3.
- **Release run 43075627762: success.**
  - controller 4m51s; runner, model-proxy, egress-proxy and tool-proxy 1m08s–1m32s each; install.yaml + GitHub release 1m08s.
  - The controller job built with `--platform linux/amd64,linux/arm64 … --tag ghcr.io/papawattu/coxswain-controller:v0.2.0` (from its job log).
- **The images:** the registry API was read with an anonymous pull token.
  - All five `ghcr.io/papawattu/coxswain-*:v0.2.0` are OCI indexes listing `linux/amd64` and `linux/arm64`.
  - Positive control: `coxswain-controller:v0.1.3` lists `linux/amd64` only.
- **The release page:**
  - The release has the assets `install.yaml` and `coxswain-0.2.0.tgz`.
  - The workflow published it as a prerelease. On the owner's word it is now `--prerelease=false` and latest; `releases/latest` returns `v0.2.0`.
  - A note at the top of the page says the arm64 images were not run on arm64 hardware (D137).
- **v0.1.3's release page:** a known-defect notice at the top (D136). The original body is unchanged below it.

## The owner's grill (11-11)

The owner asked to be grilled on the last few decisions. Each answer is recorded here. D131–D137 change Phase 7's D122–D129 as shown. Every Judge change stays advisory: no Loop's `Succeeded` depends on it.

**Answered:**

- **D131 (on D122):** the Judge stays advisory, and its verdict is shown in `kubectl get loop`.
  - The gap it closes: with `spec.delivery.mode: None` (`api/v1alpha1/loop_delivery_types.go:43`), a false verdict had no visible effect.
- **D132 (on D123):** an optional `spec.verify.judge.model`, plus its own endpoint and Secret. It defaults to the Loop's model.
  - The gap it closes: the Judge was the same model as the author, and its kind proof was n=1 with qwen3.8-27b (R55).
- **D133 (on D124):** an Unknown or truncated verdict forces a draft PR, like a false verdict. `Succeeded` is unchanged.
  - The gap it closes: an agent that wants to dodge the Judge can more easily make it fail than make it pass. Unknown delivered like True (`internal/controller/loop_deliver_job.go:446`).
  - Also, a diff over 256 KiB is judged on its first 256 KiB (`cmd/judge/main.go:137,172`).
- **D134 (on D125):** the canary limit stays accepted, and one kind run plants the #371 probe and records what the Judge does.
  - The canary note's only possible catch for the probe was "the Judge … whether it flags it is unshown" (`docs/CANARY-CHANNEL.md:349-350`).
  - The Judge's one kind proof was a hardcoded-inputs trick, not the probe (R55).
- **D135 (on D127):** ADR-0005's diff scan (VerifySuspicious) is built beside the Judge, not replaced by it.
  - It is deterministic, runs on every Loop, and its patterns include `os.Exit` inside `init`, which the #371 probe uses.
  - A hit sets the same draft flag.
- **D136 (on D128):** v0.1.3's release page carries a known-defect notice (done). AGENTS.md gets a rule: a defect found after a release gets a notice on that release's page (Phase 8 H0).
- **D137 (on D129):** arm64 is built, not run. The release page says so (done), and so do the v0.2.0 notes in this commit (`docs/RELEASE-NOTES-v0.2.0.md:15`). There is no arm64 smoke and no arm64 build in PR CI.
- **D130:** Phase 8 hardens verification (D131–D136) and ships v0.3.0. Adoption follows: dogfooding the owner's repos, a QUICKSTART a stranger can follow, and the CNCF landscape. The plan is `docs/PLAN-PHASE8.md`, in its own PR for the owner's approval.

**Answered with the Phase 8 plan (owner, 11-12: "approve the plan, D138 and D139 as recommended"):**

- **D138:** the `Judge` column shows one value, the most serious first: `Suspicious` > `False` > `Truncated` > `Unknown` > `True`.
- **D139:** drop `--prerelease` from `release.yml:117`. This is an owner push.

## P3

- [ ] **I271: the reviewer hid `make lint`'s exit status**

  **Where:** the reviewer's local check before pushing #478 (11-10 ~22:00Z).

  **Problem:**
  - The command was `make lint 2>&1 | tail -2`. The pipeline's status is `tail`'s, and the two lines it printed were shellcheck lines, not a result. So the push went out without a local lint result.
  - CI lint passed on #478 (run 42910497694), so nothing broke.
  - The same pipe would have hidden a failure.

  **Fix:** a local check is run without a pipe that drops its status, or with `set -o pipefail` and the status printed (`; echo rc=$?`).

  **Acceptance:** none in R58's window.

- [ ] **I272: CONTEXT.md describes the Judge and VerifySuspicious as they were planned, not as built or decided**

  **Where:**
  - `CONTEXT.md:36-38` ("An optional sidecar … a boolean plus a score"). The Judge is a Job through the model proxy (ADR-0011).
  - `CONTEXT.md:60-62` ("Planned … not implemented"). D135 now schedules it (S1).

  **Problem:** the vocabulary file is where the README sends readers (README.md "The vocabulary is defined in CONTEXT.md"). The README outline in flight (DOC1, the owner's ask on 11-11) takes its Judge text from ADR-0011 for this reason.

  **Fix:** Phase 8 H0 rewrites the Judge entry. S1 rewrites the VerifySuspicious entry when it merges.

  **Acceptance:** each entry matches its ADR as built, with the slice's squash hash.

## Housekeeping

- **The `../coxswain-c1` worktree** (the owner's, `slice/c1-multiarch` at 973f179) is still on disk. Its remote branch was deleted when #473 merged. The reviewer has not touched it; removing it is the owner's call.
- **README "Status: pre-alpha":** the line predates v0.2.0. DOC1 does not change it (one concern per PR). It is for the owner to reword.

## What I did not verify

- **arm64 at runtime:** no image has run on arm64 hardware (D137).
- **Why `docker manifest inspect` said "no such manifest"** for all five images while the registry API served their indexes to an anonymous token. The devbox's docker has no `buildx` plugin. The cause is not shown.
- **The release's install.yaml and Helm chart contents** beyond their presence as assets. R1 installed from the repo with kustomize, not from the release assets.
