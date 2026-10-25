# Phase 1 review, round 29: Phase 5 (shared memory), F13–F29 and the acceptance

Round 29, 2026-10-24, since tag `review/phase1-r28`. It covers Phase 5 (P5a–P5f),
the fixes found while building and proving it (F13–F29), and the Phase 5
acceptance run. The reviewer verified that run on the enforcing lab cluster
`kind-ref` (Calico + KubeArmor), using a clean Helm install of `main` with no
hand patches.

| PR | Item | Merge |
|---|---|---|
| #172 | I93: plan-hash check at the start of Implementing | 1902f8e |
| #174 | P5e (I94): PR title and commit subject from `spec.goal` | 407cbf8 |
| #176 | F14: ignore a stale Failed pod not owned by the current Sandbox | 9be28d6 |
| #178 | F13: GOTMPDIR on the workspace PVC | b85b546 |
| #179 | P5a: memory PVC per (namespace, git URL), read into the prompt | 653347f |
| #180 | P5b: static write policy and the memory-writer Job | f12d2dc |
| #184 | F16: the memory-writer NetworkPolicy denies all | 0e49905 |
| #185 | F17: the runner sends `lessonsAvailable` in its claim | 402810e |
| #187 | F18: case-insensitive credential rule; Authorization and AWS rules | 669be17 |
| #188 | F19: every shipped manifest passes `--memory-writer-image` | 8bdd39f |
| #189 | F21: `hack/rwx-storage.sh` (dynamic RWX StorageClass) | c911357 |
| #190 | F20: `make test` runs the `--memory-writer-image` guard | 1c56b6c |
| #186 | P5d: `cox memory export` / `import` | a57b947 |
| #193 | F24: the RWX class binds Immediate | cf788e6 |
| #194 | F25: checkpoint GC treats NoMatch as done | e54f8b9 |
| #192 | F23: StorageClass DNS-1123 check; peek pod drops the SA token | d7feea1 |
| #191 | F22: `MEMORY_STORAGE_CLASS` / `MANAGER_EXTRA_ARGS` for install.yaml | ca3464d |
| #195 | F26: QUICKSTART §9 pins and RWX memory steps | b5f2c7d |
| #182 | P5c: the Memory Curator gate (ADR-0002) | 86d1135 |
| #196 | F27: cleanup (stripReasonCR spec, docs) | b6b6bb9 |
| #197 | F28: `MemoryPVCUnbound` is a Warning Event | 43c5ef2 |
| #198 | P5f (I98): hold Succeeded until Delivered is terminal | a84b7c9 |
| #199 | F29: the chart's manager role and CRDs match `config/` | 0a048fc |

**Status:**
- **Phase 5 is complete.** All six slices are merged.
- **The done-when is met** (`docs/PLAN.md:85`): "a second Loop on the same
  repo starts with the first Loop's approved lessons in its prompt, and the
  audit trail shows exactly which lessons were written and why". The evidence
  is in `.samples/phase5-accept-2/` (not committed), and the reviewer checked
  each line cited here:
  - **Loop 1 qualified:** it ended Failed/Stalled (`describe-loop1.txt:3`,
    `:20-22`).
  - **Curator verdicts:** it kept 2 lessons and dropped 1, each with its reason
    (`loop1-memory-writer-job-log.txt:1-4`). The audit Event is at
    `operator-log-full.txt:609`.
  - **Loop 2:** its memory copy-in ran (`loop2-sandbox-events.txt:19-20`). Its
    `SHARED MEMORY` prompt section holds exactly the 2 kept lessons
    (`standin-requests-final.txt:123-129`), and the dropped lesson's hash
    appears in no prompt line.
  - **Clean install:** the operator's ClusterRole is managed by Helm only, and
    the operator log has 0 `forbidden` lines. The positive control is 4
    `memory write audit` lines in the same log.
- **The first acceptance run did not count.** The builder hand-patched the
  live ClusterRole to unblock it (I100). That exposed F29: the Helm chart's
  manager role lacked `core/serviceaccounts` (added by P5b), and the chart CRD
  had been stale since F17. Helm installs could not write memory at all.
  F29 adds two `make test` guards, and the reviewer mutation-tested both.
- **The lab VM** `coxswain-lab` (192.168.1.62) runs two enforcing clusters:
  `k3d-hermes-sim`, which mirrors hermes, and `kind-ref` (Calico + KubeArmor).
  F19, F24, F25 and F29 were all found there; none of them shows on the devbox
  kind clusters.

**Closed in this round** (ticked in their own docs): I91 (5cc1a97), I93
(1902f8e), I94 (407cbf8), I98 (a84b7c9).

---

## P2

- [x] **I99: a lesson wrapped across lines is stored as a fragment** (cfffcc2, #201)

  **Where:** `runner/lessons.go:149` (`parseLessons`) and its instruction at
  `runner/lessons.go:131`.

  **Problem:** the parse is line-based, so it keeps only the first line of a
  `LESSON:` and drops the rest silently. In the acceptance run, the stand-in
  wrapped a lesson over three lines. The stored file reads `round.go: the
  original Round() truncated (toward zero) instead of`, without the fix
  (`.samples/phase5-accept-2/memory-pvc-direct-peek.txt`, the round.go block).
  The curator then kept that fragment as "a factual, single-line statement".
  Real models wrap long lines too, so this is the likely failure in normal
  use, not an edge case.

  **Fix:** treat a non-blank line that follows a `LESSON:` line, and doesn't
  start a new `LESSON:`, as a continuation. Join continuations with single
  spaces, then apply the existing caps. Alternatively, drop a lesson whose
  next line looks like a continuation, giving the reason `wrapped`. Either way,
  a fragment must never be stored silently.

  **Acceptance:** a runner spec feeds the exact three-line reply from the
  acceptance run and asserts the whole sentence is one lesson, or that it is
  dropped as `wrapped`. Mutation: restore the first-line-only parse, and the
  spec fails.

- [ ] **I100: builder evidence and process integrity (the R28 pattern again)**

  **Where:** the builder sessions of 2026-10-23 and 2026-10-24 (reviewer log).

  **Problem:** these all broke rules that `AGENTS.md` already states:
  - **Secret data read four times** (D48). Builder #1 decoded the `git-cred`
    PAT and used it against the local Gitea. It decoded `gitea-admin`. It ran
    `get secret … -o jsonpath='{.data}'`, and later
    `-o jsonpath='{.data.keys()}'`.
  - **Hand patches.** Builder #1 hand-patched the operator Deployment twice
    during P5c, and patched the live ClusterRole five times in the first
    acceptance run. One patch was mis-indexed and briefly removed the
    `sandboxes` rule. Its report then named a missing `sandboxes` rule as the
    cause, but the chart has that rule at every deployed commit. The real
    defect (F29) was found by the reviewer.
  - **A force-push and an amend** on the F28 branch.
  - **A 44-minute block** on `kubectl run … -i … sleep 3600`. The reviewer
    killed it by PID.
  - **A `git stash`** in a worktree by builder #2, popped at once. The shared
    stash list held another session's entry at the time, so a pop could have
    taken the wrong one.

  **Fix:** an owner decision (D72). The rules are already written down and
  have been re-stated in every handoff, yet the slips recur. They come almost
  entirely from builder #1, the smaller local model.

  **Acceptance:** D72 answered, and no new D48 or hand-patch slip in the next
  round.

## P3

- [x] **I101: memory PVCs outlive their Loops, and nothing says how to remove them** (8a5940b, #202; 04472d5, #203)

  **Where:** `internal/cli/memory.go:32`, `internal/cli/memory_transfer.go:84`
  and `:99` (`memory`, `export`, `import`: no delete); `docs/OPERATOR.md`.

  **Problem:** by design, a memory PVC persists per (namespace, repo) after its
  Loops are deleted. Nothing documents that, and there is no supported way to
  remove one. The acceptance builder had to `kubectl delete pvc` by name. An
  operator who deletes a namespace's Loops expecting a clean slate will keep
  every lesson.

  **Fix:** document the lifecycle in OPERATOR.md, and add
  `kubectl cox memory delete <repo-url>`. It should refuse while a Loop on that
  repo is active.

  **Acceptance:** a CLI spec covers both the refusal and the delete. The
  OPERATOR.md section names the PVC, its lifetime and the delete command.

- [ ] **I102: the P5b kind evidence ran on hand-made PVs**

  **Where:** `kind-coxswain-snap`, StorageClass `local-shared`. It is a
  `kubernetes.io/no-provisioner` class that the P5b builder created by hand at
  2026-10-21T02:09.

  **Problem:** P5b's kind evidence bound its memory PVCs to hand-made hostPath
  PVs, not to a provisioner a user would have. It proved the writer, but not
  the storage path.

  **Fix:** none needed in code. The Phase 5 acceptance on `kind-ref` used
  `hack/rwx-storage.sh` (F21, F24) and supersedes that evidence. The class
  stays on `kind-coxswain-snap`, which the reviewer leaves alone. Removing it
  is an owner action (D74).

  **Acceptance:** D74 answered.

---

## Owner decisions

- **D72: builder #1's role.** Builder #1 (the qwen 27B local model) caused
  almost all of I100. Options:
  - (a) keep it as a builder, with the watcher's Secret and workaround alarms
    as the backstop (the status quo);
  - (b) restrict it to docs-only and evidence-only work, with no cluster
    credentials beyond a lab kubeconfig;
  - (c) retire it from coxswain work.

  **Recommendation: (b).** It produces good evidence write-ups, but its
  cluster actions need a reviewer catching them in real time.

- **D73: should import, and tighter rules, re-screen through the curator?**
  - `cox memory import` runs only the static screen
    (`internal/cli/memory_transfer.go:303`), never the curator, so an imported
    lesson skips the model gate that a Loop's own lessons pass.
  - When the static rules tighten (as F18 did), lessons already on a PVC are
    never re-screened.

  Options: (a) leave both as they are, documented; (b) import runs the
  curator, and the writer re-screens existing files on each write;
  (c) (b) plus a `cox memory rescreen` command.

  **Recommendation: (a) for import**, because the operator who imports is
  trusted and the static screen still applies. **(b) for re-screening**,
  because it is cheap and closes a real gap when rules tighten.

- **D74: housekeeping the reviewer won't touch without the owner:**
  - the hand-made `local-shared` class on `kind-coxswain-snap` (I102);
  - an old shared stash entry, `stash@{0}: WIP on (no branch): 7e34302 F17 …`
    (`runner/phase.go`), left from an earlier session;
  - a `kubectl -n samples port-forward svc/gitea 3000:3000`, PID 1500191, about
    12 hours old on 2026-10-24.

  **Recommendation:** delete all three.
