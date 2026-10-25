# Phase 1 review, round 33: the memory writer as one design, F43, and two loose bounds

Round 33, 2026-10-25. It follows R32 (PR #235, not yet merged) and covers the
work merged since R32 was opened (#234 to #245). Code line numbers are against
main at `4d7b5d2`. The review was built from these input files, which are not
committed:
- a post-merge review of the memory writer after F31-F33 (`.samples/review-postmerge-4.md`);
- the pre-reviews of PRs #234 and #236-#245 (`.samples/prereview-*.md`);
- the F41 investigation (`.samples/f41/FINDINGS.md`, summarised in
  `.samples/f41-summary-for-owner.md`);
- a cross-doc consistency check (`.samples/docs-consistency.md`, `.samples/consistency-rest.md`);
- builder #1's gathering for this round (`.samples/r33-skeleton.md`, `.samples/r33-draft.md`)
  and the read-only builder's check of it (`.samples/prereview-r33-skeleton.md`).

| PR | Item | Merge |
|---|---|---|
| #234 | F33: memory-writer audit hashes the trimmed lesson; batch duplicates; NetworkPolicy before the Job | 069a655 |
| #238 | F34: remove stale lessons.json; restore hold covers StoppingWorkloads | 5e82c96 |
| #241 | F41: retry the curator call on a transport failure | 4b71ff4 |
| #243 | F78: counts line first and a line-oriented audit cut | bd4c457 |
| #244 | F43: refuse a non-descendant pin; runner result/PLAN.md/rename fixes | 80dac60 |
| #236, #237, #239, #240, #242, #245 | Docs: index, upgrade notes, evil-agent e2e, memory write outcome, two consistency passes | a6d73c5, f9569dc, acb40b0, b850406, 23124e7, 4d7b5d2 |

**Status:**
- **F43 closes a way to get unrelated work verified.** The verify
  Job now refuses a pinned commit that does not descend from the base commit,
  and the runner no longer re-emits a stored result whose commit is not in
  the workspace. The reviewer reproduced both mutations against the real
  generated script.
- **The memory writer is now consistent as one design** after F31-F34 and F78,
  except for one bound it claims and does not enforce (I115).
- **F41's cause is still not shown.** A new pod's first calls to an in-cluster
  proxy can fail for a few seconds on k3d-hermes-sim; the curator now retries
  within a batch budget. The runner may hit the same window (I116).
- **One CI flake surfaced** (I117).
- **Pre-review kept catching real defects before merge** (I118).
- No new owner decision. D72-D84 are still open
  (`.samples/owner-decision-brief.md`).

---

## P2

- [ ] **I115: the memory writer's summary is not size-bounded**

  **Where:** `cmd/memory-writer/main.go` (`emitSummary` writes the whole
  buffer to stdout and the termination-message file with no size check).

  **Problem:** the termination message is capped at 4096 bytes by Kubernetes.
  A batch of 20 lessons (the per-write cap) with long or non-ASCII curator
  reasons produces a larger summary, so the operator reads a cut summary with
  no marker. Comments claimed a 4096-byte bound until #245 corrected them;
  the code never enforced one. F78 put the counts line first, so the counts
  survive a prefix cut, but the lesson lines past the cap are lost silently.

  **Fix:** F80 (queued). Bound the summary in the writer below 4096 bytes,
  line-oriented like F78 (counts line, audit lines, whole lesson lines, then
  "… N more lines").

  **Acceptance:** a spec writes a summary over 4096 bytes and asserts the
  termination file is at most 4096 bytes, keeps the counts line, and ends with
  a truncation marker; a named mutation (no bound) fails it.

- [ ] **I116: the runner may fail its first model call in the same pod-start window**

  **Where:** `runner/runner.go` (the I89 retry treats connection refused as
  final and retries only timeouts); `.samples/f41/FINDINGS.md`.

  **Problem:** F41 showed that on k3d-hermes-sim a new pod's first calls to an
  in-cluster proxy can fail fast for up to about 10 seconds; the mechanism
  was not found. The runner's first model call comes from a fresh sandbox
  pod. If it hits the same window, the claim blocks with `ModelUnavailable`.
  This is not yet observed for the runner.

  **Fix:** F79 (queued). First measure whether a runner's first call fails on
  k3d-hermes-sim. Only if it does, retry dial-class failures briefly, with a
  log line per retry.

  **Acceptance:** either a lab run showing the runner's first call does not
  fail (and F79 closes with that evidence), or a fix with a spec, a named
  mutation, and a lab run showing the first call now succeeds.

---

## P3

- [ ] **I117: an envtest spec hard-codes a ClusterIP and flakes**

  **Where:** `internal/controller/loop_i42b_egress_proxy_test.go:286`
  (`ClusterIP: "10.0.99.99"`).

  **Problem:** on PR #245's CI run 40149610512 the spec failed with "provided
  IP is already allocated" in the shared envtest API server. It passes
  elsewhere, so a run fails at random.

  **Fix:** F81 (queued). Let the API server allocate the address and read it
  back, or use an address no other spec uses and delete the Service after.

  **Acceptance:** the spec no longer names a fixed ClusterIP, or a repeated
  run (`-ginkgo.repeat`) passes.

- [ ] **I118: what pre-review caught this round, and process slips**

  **Where:** `.samples/prereview-241.md`, `prereview-243.md`,
  `prereview-244.md`, and the docs pre-reviews for #236-#245.

  **Problem:** without pre-review these would have merged:
  - #241: the production retry ladder had no spec (retries could be switched
    off with tests still green), and its budget claim was per lesson, not per
    batch;
  - #244: an empty successful model answer still blanked an approved PLAN.md;
  - post-merge review 4: F32 and F33 together dropped the audit's counts line
    first (F78);
  - the docs PRs: a troubleshooting step pointing at a distroless pod with no
    shell, a `cox logs` form the CLI rejects, and an e2e claim no test makes.

  Slips: the reviewer's first fix commit on #242 (`50db42b` on the branch)
  says it contains three fixes but holds one, because a script edit failed
  midway and the commit still ran; the other two landed in a second commit.
  Two of the reviewer's mutation runs first failed to build or used a flag
  the package does not define, and were re-run correctly before being cited.

  **Fix:** keep pre-review on every PR. The reviewer chains multi-step edits
  so a failed edit cannot commit, and runs a mutation's baseline before
  citing it.

  **Acceptance:** no commit message claims more than its diff.

---

## Owner decisions

None new this round. D72-D84 remain open.
