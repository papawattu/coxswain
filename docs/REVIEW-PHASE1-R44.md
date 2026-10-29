# Phase 1 review, round 44: the four rebased drafts verified after merge, and an evidence trail with gaps in the bodies

Round 44, drafted by the reviewer at ~06:45Z on 2026-10-28 while both deepseek-flash agents (b2, pi3) were stopped
by an API billing error (I191), and published at ~05:20Z on 2026-10-29. It follows R43 (`docs/REVIEW-PHASE1-R43.md`,
merged as **`47bec33`**, PR #357, 2026-10-29 04:18:05Z, tagged `review/phase1-r43`; its last ID is I185, its last
decision D91) and covers the merges after R43's window (R43 covered #323-#356):

| PR | Item | Merge |
|---|---|---|
| #358 | Docs: prune Known issues fixed on 2026-10-28 (reviewer) | `d6a62d9` 03:34:53 |
| #359 | F11: the no-op egress Service reconcile must issue no write (R43 I177; reviewer) | `9da5cb7` 04:03:01 |
| #322 | F58: the verify Job's containers are resource-bounded, OOM is infra | `17be3ba` 04:19:26 |
| #325 | F64: PolicyTranslationLossy reports a port nothing carries | `54b3786` 04:19:30 |
| #332 | F68: validate spec.workspace.ref and spec.verify.acceptanceChecks | `911d2c6` 04:19:33 |
| #337 | F75 + F76-doc + F77-pin: restore "do now" | `d7311ca` 04:19:36 |
| #320 | review(phase1-r41) | `721cc41` 04:20:55 |
| #297 | review(phase1-r40) | `f372446` 04:20:59 |
| #296 | review(housekeeping): ticks in R15, R30-R37 | `9f35e19` 04:21:02 |
| #361 | Docs: release notes for #322, #325, #332, #337; prune F58, F64, F68 (reviewer) | `497792d` 04:40:21 |
| #357 | review(phase1-r43) | `47bec33` 10-29 04:18:05 |
| #360 | review(housekeeping): ticks in R40, R41 | `b0f9c6e` 10-29 04:18:09 |

(`gh pr view <n> --json mergedAt,mergeCommit`; times are 2026-10-28 unless marked.) Evidence files are under `.samples/` (uncommitted, AGENTS I68).

**Status:**

- **No P1 in merged code.** The one P1 security item left open is F60 (`docs/RELEASE-NOTES-next.md` at `497792d`,
  the P1 sentence rewritten by #361).
- **R43 I177 is fixed** by #359: the F11 spec now counts writes; under #340's exact mutation it fails with
  `Expected <int>: 1` and passes on main (`.samples/m359/mutation.log`, `baseline.log`). R42 I165 stays unticked
  as a box (its acceptance conjoins #340 and #347); both halves now have output.
- **Every owed mutation that could be run has been run, and every one fails at least one spec.** Wave 2 (the
  ten 02:35Z merges): 22 mutations, all EXIT=1 (`.samples/post-merge/INDEX-wave2-mutations.md:10`). Wave 3 (the
  four 04:19Z merges): #322 M1-M8, #325 M1-M3, #332 M1-M6, #337 M1-M9, all failing
  (`.samples/post-merge/wave3/README-wave3-state.md:8-11`). The reviewer posted the result per PR.
- **Still owed:** #322's two kind observations (I189); #338's measurement and #342's two Calico runs (owner);
  #332's pre-1.30 proof (blocked by the node image, I189).

## P1

- None.

## P2

- [ ] **I186: four drafts merged after a hand rebase with their verification owed, and the bodies did not carry runnable mutations**

  **Where:** #322, #325, #332, #337 merged at 04:19:26-04:19:36Z (table above), after the owner rebased them in the
  shared tree (R43 I184). pi3's owed list for them, written at 04:33Z, records the state at merge
  (`.samples/post-merge-owed-3.md:46-49`): #322 8 mutations owed and "no diff exists anywhere"; #325 3 owed,
  "prose only, no hunks published"; #332 6 owed, "described in prose, no hunks", and its `make manifests` evidence
  "pre-rebase"; #337 9 owed (its 9 diffs exist and apply).

  **Problem:** the outcome is good (every mutation, once written, fails its spec; wave-3 README `:8-11`), but three
  of the four bodies named mutations without publishing the diffs, so the hunks that were finally run are
  pi3's re-derivations, not the authors'. A reader of #322/#325/#332 cannot re-run the body's claim from the body.
  This is R42 I165's class (a claim without its output) one step earlier: a claim without its instrument.

  **Fix:** a body that names a mutation publishes the exact diff (in the body or a committed-path-free `.samples`
  file named in the body) and the exact focus command; the wave-3 logs are the record for these four.

  **Acceptance:** the next mutation-carrying PR's body contains, per mutation, a fenced diff and a focus command;
  the reviewer can apply each with `git apply` against the PR head.

- [ ] **I187: a published focus command selects no test, and several mutations are caught only above the unit the body names**

  **Where:** #337's body runs `go test ./internal/controller/ -run TestAPIs -ginkgo.focus="F75|F77"`
  (`.samples/post-merge-owed-3.md:66-76`); `git grep -n '^func TestAPIs' internal/controller/` returns nothing at
  `main` (`:76`), so the command selects no test. In wave 3, #325 M3 and #337 M2, M3, M4, M5, M8, M9 fail the
  envtest Describe (focus 1, EXIT=1) while the unit functions the body names stay green (focus 2, EXIT=0)
  (`.samples/post-merge/wave3/337-M2-no-clear-at-lost-pvc.log` "focus 1" EXIT=1 / "focus 2" EXIT=0; same shape in the
  other six logs). #322 M1's first focus contained `cpu+memory`, a regex quantifier, and selected 0 of 719 specs
  (`.samples/post-merge/wave3/322-M1-check-container-resources-deleted.log:27-42`).

  **Problem:** R43 I181 (#344's focus) recurs twice in one window: a focus that selects nothing reads as "the spec
  passes under the mutation". And where a body names unit detectors that the mutation does not move, the claim
  "spec X catches M" is wrong even though some spec does.

  **Fix:** every focus in a body is checked to select >0 specs (`Will run N of M`, N>0) and `-ginkgo.focus` strings
  avoid unescaped regex metacharacters; the body names the spec that actually fails.

  **Acceptance:** each mutation log in the next window shows `Will run N of M` with N>0 for the body's own focus.

## P3

- [ ] **I188: filed mutation diffs that do not compile**

  **Where:** #337 M6 (`declared and not used`, `.samples/post-merge/wave3/337-mutations-summary.log:33-34`); #338 M3
  (`"log" imported and not used`, `.samples/post-merge/INDEX-wave2-mutations.md:44-45`); #324 M7 (unused local,
  same file `:47`). Each was re-run with a one-line keep-alive and the first attempt kept.

  **Problem:** same class as R43 I182: the published artefact is not the diff that ran.

  **Fix:** the author compiles each filed mutation before publishing it.

  **Acceptance:** every filed `.diff` in the next window builds (`go build ./...` after `git apply`).

- [ ] **I189: #322's kind observations are still owed, and #332's pre-1.30 proof cannot run on this repo's kind**

  **Where:** `.samples/post-merge/wave3/README-wave3-state.md:8` (#322 OWED: the OOM/VerifyInfraRetry path at a
  256Mi limit and the gocli sample at the defaults) and `:10` (#332 BLOCKED: kind node image `v1.34.0`). The #322
  run had recorded the starting state (`322-kind-01-starting-state.log`, `322-kind-02-crd-before.yaml`) and the
  image build (`322-kind-03-docker-build.log`) when the builder stopped (I191); the cluster was checked read-only
  afterwards and is unchanged (deployment `coxswain-controller:r43-d6fd33a`, helm revision 23; re-checked read-only
  at 05:14Z on 10-29 with the same result). Nothing has run since: the builder is still stopped (I191).

  **Fix:** finish #322's two observations when the builder is back (handoff written); record #332's proof as
  owner-decided (accept the CRD-level evidence, or provide a pre-1.30 cluster).

  **Acceptance:** `322-kind-*` logs show both observations with file:line and the cluster restored to a main build.

- [ ] **I190: the reviewer cited the wrong evidence file on three PRs again (R43 I183 class)**

  **Where:** wave-2 comments on #324, #333, #338 named the wrong batch summary; corrected minutes later
  (comments `issuecomment-6460377943`, `-6460378231`, `-6460378517`). The shas were right. Before that, R43 I183's
  own correction carried an unverified "files unchanged" sentence, retracted on five PRs.

  **Problem:** a verification comment is evidence; a wrong file name breaks the chain even when the result holds.

  **Fix (done):** the reviewer now builds every comment from command output (`head`, `grep -l`, `ls`), recorded as a
  standing rule. Later wave-3 comments on #322, #325, #332, #337 were generated from the log files.

  **Acceptance:** no correcting comment is needed in the next window.

- [ ] **I191: the deepseek-flash agents stopped on a billing error mid-task**

  **Where:** from ~05:05Z on 10-28 both b2 and pi3 return `402 Insufficient Balance`. The reviewer re-tested
  hourly; it was still 402 at 05:14Z on 10-29, about 24 hours on (request id `2752d116-...`). b2 stopped after issuing the #322 image build, before `kind load`/`helm upgrade`; pi3 stopped
  early in this round's draft.

  **Problem:** two of three agents depend on one prepaid account; when it empties, cluster work can stop between
  steps. This time nothing was left half-applied (checked read-only), by luck of where the stop fell.

  **Fix (owner):** top up, and consider a balance alert; builders keep writing evidence before each cluster step so
  any stop is resumable (they did).

  **Acceptance:** the balance is restored and #322's kind run completes from its handoff.

- [ ] **I192: test runs in the shared tree left orphaned envtest processes**

  **Where:** the reviewer killed three ppid=1 etcd + kube-apiserver pairs whose cwd was the shared tree's
  `internal/controller` and whose parent test run had exited (02:55Z 3221403/3221460, 03:21Z 3255466/3255529,
  03:44Z 3389432/3389531), each after confirming the live `controller.test` was not their parent.

  **Problem:** `go test ... | tail -3` in the shared tree does not run `hack/run-envtest.sh`'s reaper, so every run
  leaves ~1 GiB behind on a host with ~1-2 GiB free.

  **Fix:** run envtest through `make test` / `hack/run-envtest.sh` (which reaps), or reap after a raw `go test`.

  **Acceptance:** no ppid=1 envtest pair after a test run in the next window.

## Owner decisions

**D92 (new): #332's pre-1.30 freeze proof.** The repo's kind uses `kindest/node:v1.34.0`; the proof needs a pre-1.30
API server. Options: (a) accept the CRD-level evidence (validation specs + parity) and close; (b) stand up a
pre-1.30 cluster for one run. Recommendation: (a), since the supported floor is the repo's own kind version.

**Still open:** D72-D91 (digest `.samples/owner-decisions-digest.md`), I180's AGENTS edit, #338's measurement, and #342's
two Calico runs. R43 (#357) and the R40/R41 ticks (#360) merged at 04:18Z on 10-29.

## What I did not verify

The #322 kind observations (not run; I189). Whether any other spec catches the unit-level misses in I187 beyond the
envtest Describe (each log runs only the two named focuses). The cause of the billing stop (I191) beyond the 402
message itself.
