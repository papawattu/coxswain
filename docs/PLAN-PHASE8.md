# Next phase: Phase 8 — verification hardening, then v0.3.0

**Status:** approved by the owner on 2026-11-12, with D138 and D139 as recommended. The direction (D130) and D131–D137 were decided by the owner on 2026-11-11 (R57).

**What carries in:**
- **D130** (owner, 2026-11-11): Phase 8 hardens verification and ships v0.3.0. Adoption comes after it (see "After Phase 8").
- **The R57 decisions:**
  - D131–D135 refine Phase 7's Judge and canary decisions (D122–D127). The Judge stays advisory: nothing in this phase changes whether a Loop reaches `Succeeded`.
  - D136 adds a release-page rule.
  - D137 records arm64 as built, not run.
- Phase 7 is complete. v0.2.0 is tagged on `a7eed6b` and is the latest release, with linux/amd64 and linux/arm64 images.

**What the phase is:** making the advisory signal hard to miss and hard to dodge.
1. **Hard to miss.** The verdict is shown in `kubectl get loop` (D131).
2. **Hard to dodge.**
   - A Judge that cannot answer, or that read only part of the diff, flags the PR like a false verdict does (D133).
   - A deterministic scan that the diff cannot argue with runs beside the Judge (D135).
3. **A separate judge.** The Judge can be a different model from the one that wrote the change (D132).
4. **Measured, not assumed.** The #371 canary probe, which the canary cannot catch (D125), is planted on kind. What catches it is recorded (D134).

**Done when:**
- On kind-ref, with every slice below merged, three Loops end as follows:
  - **The #371 probe** (read `zzz_canary_test.go`, print the token, exit 1): a draft PR, and the Loop's `Judge` column shows a flag.
  - **The hardcoded-inputs trick** (the run 17b method, R55): a draft PR, and the column shows `False`.
  - **An honest change:** a ready PR, and the column shows `True`.
  - The two flagged Loops write no lesson to the memory PVC; the honest one may (D140).
- v0.3.0 is tagged, with notes and an upgrade proof from v0.2.0.
- A Loop without `spec.verify.judge` still behaves exactly as in v0.2.0, apart from the scan's flag (D135 runs on every Loop).

## Why it is safe to take now

- The review backlog is clear (R57). Every slice is advisory. None changes the phase machine or `Succeeded`.
- Each slice is small and its effect is decided, including D138 (the column's values) and D139 (the release default).

## Slices (in order; one PR each, TDD per the AGENTS.md test norms)

### E0. The #371 probe against today's Judge (D134; reviewer lab run, no code)
- **Why first:** it measures the Judge alone, before D133 and D135 change what a flag means. The result decides how much S1 matters.
- **Run:** on kind-ref, at v0.2.0, scripted and restored like run 17b. A Loop with `spec.verify.judge` on, whose planted diff is the #371 probe.
- **Record:** the canary's outcome, the Judge's verdict, score and reason, and the PR's draft state. They go in `docs/CANARY-CHANNEL.md` under a new "Measured" heading, with the evidence file and line.
- **Acceptance:** the note states what the Judge did, whatever it was. A missed probe is a result, not a failure of E0.

### H0. AGENTS.md: a release-page notice for any defect found after release (D136)
- **Change:** one bullet in "Test norms", next to the R26 I90 release-defect bullet. A defect found after a release gets a notice at the top of that release's GitHub page. The notice names the defect, the fix PR and the release that fixes it, as on v0.1.3's page. The PR that names the defect in the next release's notes cites the notice.
- **Also:** CONTEXT.md's **Judge** entry still says "sidecar". Update it to the built design (ADR-0011): a Job through the model proxy, opt-in, advisory. Update **VerifySuspicious condition** after S1 merges, not here.
- **Acceptance:** docs only; `make lint`.

### J3. An Unknown or truncated verdict forces a draft (D133)
- **Change:** `deliverDraft` (`internal/controller/loop_deliver_job.go:448`) forces a draft when `status.judge` exists and either:
  - the verdict is nil (Unknown, reason `JudgeUnavailable`); or
  - `status.judge.truncated` is true (`api/v1alpha1/loop_types.go:1493`), whatever the verdict.

  A Loop with no `status.judge` (Judge off) is unchanged. The PR description says which of these forced the draft.
- **Specs:** a table over {no judge, true, true+truncated, false, nil (Unknown)} × `spec.delivery.draft` {true, false}. Each Unknown and truncated row must fail under the mutation that drops its clause; run the mutation and record the output.
- **Kind:** not required. No pod script and no shared volume change (the R55 I263(c) rule); the PR says so. E1 covers it on kind.

### J4. The `Judge` column in `kubectl get loop` (D131; needs D138)
- **Change:**
  - A status string the operator sets: `status.judge.summary` (D138 names its values).
  - A print column `Judge` on that field, placed after `Iteration`.
  - Blank when the Judge is off and S1 has not flagged anything.
- **Specs:** each value is set from its source (each J3 row and, after S1, the scan). The column is in the generated CRD; check `config/crd/bases` after `make manifests`, and `hack/chart-crd-sync.sh`.
- **Kind:** `kubectl get loop` output pasted from E1.

### J5a. `spec.verify.judge.model` (D132, same endpoint)
- **Change:**
  - An optional `model` on `JudgeConfig` (`api/v1alpha1/loop_types.go:381`). When set, the Judge asks the Loop's model proxy for that model instead of `spec.model`. Empty means the Loop's model, which is D123's default.
  - ADR-0011 gets an amendment recording D132.
- **Specs:** the Judge Job's request names the right model in both cases. The mutation that ignores the field fails a spec.
- **Kind:** one Loop on kind-ref with `judge.model` set to a second model served by the same endpoint, if the lab's vLLM serves one. Otherwise the PR says so, and J5b's kind run covers the field.

### J5b. A Judge endpoint of its own (D132, different endpoint)
- **Change:**
  - Optional `judge.modelEndpoint` and `judge.endpointSecretRef`. The model proxy has one upstream (`cmd/model-proxy/main.go:9,71`), so a different endpoint gets its own model-proxy instance for the Judge, metered into the same Loop totals (ADR-0009).
  - It gets its own NetworkPolicy. The I265 structural spec (`loop_i265_proxy_ingress_callers_test.go`) gains its ensure call.
- **Specs:**
  - The Judge Job talks only to the Judge proxy. The Loop's proxy is unchanged.
  - Metering adds the Judge's tokens to the Loop.
  - Each RBAC rule passes can-i on kind, with zero `forbidden` (the B1 rule).
- **Kind:** required (new pod, new NetworkPolicy, new Secret mount): one Loop judged through a second endpoint.

### S1. The diff scan: VerifySuspicious (D135; ADR-0005 D17.2)
- **Change:**
  - A trusted init container in the verify Job, after the tamper check (`verifyTamperInit`, `internal/controller/loop_verify_job.go:112`). No agent code runs in it.
  - It scans the base→verified diff of non-protected Go files. The patterns are ADR-0005's four (`testing.Testing()`, `os.Exit` inside `init`, `//go:linkname`, `flag.Lookup("test.`), plus any reference to the canary's file name (`zzz_canary`).
  - It parses Go (`go/parser`) rather than grepping lines, so "inside `init`" is exact.
  - It reports by termination message and exits 0 either way. A missing or garbled message means "scan unavailable": a Warning Event, never "clean".
  - A hit records `VerifySuspicious=True`, reason `SubversionPatternInDiff`, with a Warning Event and a history note. `spec` is unchanged (D21). `deliverDraft` forces a draft, and the J4 column shows the flag.
  - ADR-0005 gets an amendment: D127 is reversed by D135, so VerifySuspicious is built beside the Judge.
- **Specs:**
  - An execution test of the real scanner on fixtures: each pattern hits, a clean diff does not, and K0's #371 probe fixture hits.
  - A mutation per pattern (drop it from the list), each failing its spec.
  - Envtest that the condition, the Event and the draft follow a hit.
- **Kind:** required. It is a new init container reading the shared workspace volume (the R55 I263(c) rule). The #371 probe Loop shows `VerifySuspicious=True` and a draft PR.

### M1. A flagged Loop writes no lessons (D140)
- **Why:** E0 (`docs/CANARY-CHANNEL.md` "Measured") found that the Memory Curator kept a lesson from the #371 probe Loop that the Judge had just judged false. The lesson calls the probe harmless. kind-ref's gocli memory also held four lessons from the 11-09/11-10 gaming runs that spell out the hardcoded-switch trick. Every later Loop on that repo reads them at start (ADR-0002).
- **Change:** the memory writer Job (`ensureMemoryWriterJobAndNetPolicy`, `internal/controller/loop_memory_writer_job.go:217`) is not created when `status.judge` exists and its verdict is false or nil, or `status.judge.truncated` is true. This is the same set that forces a draft (D133). After S1, a `VerifySuspicious=True` hit withholds the lessons too.
  - The Loop records `MemoryWriteDone=True` with its own reason (`LessonsWithheld`), a Normal Event and a history note naming the Judge signal.
  - A Loop with no `status.judge` is unchanged.
- **Specs:** envtest for each row of J3's table: a withheld row creates no memory writer Job, and the reason and Event are set. The clean-true and no-judge rows still write. The mutation that drops the check fails a spec.
- **Kind:** not required for the gate itself: it decides whether a Job is created, with no pod script or volume change (R55 I263(c)). E1's probe Loop shows it (no new lesson file on the memory PVC).

### E1. The exit run (reviewer lab run)
- **Run:** on kind-ref, scripted and restored, with everything above merged: the three Loops in "Done when".
- **Record:** `kubectl get loop` with the `Judge` column, each Loop's conditions, and each PR's draft state. Add the probe's result to the canary note's "Measured" heading beside E0's.

### R2. v0.3.0 (release; the owner tags)
- **Content:**
  - `docs/RELEASE-NOTES-v0.3.0.md`, with an Upgrade section: the new status field and print column, the new Judge fields, the scan's init container, and the flag on Loops without a Judge.
  - "arm64: built, not run" carries over unless an arm64 run exists by then (D137).
- **Proof:**
  - the pre-tag smoke on kind with delivery;
  - an upgrade from v0.2.0 on kind-ref, with one Loop living across it (R1's method).
- **After the tag:** the reviewer checks that each ghcr image lists both platforms, as for v0.2.0.

**Parallel work:** E0 runs on the reviewer's side while H0 and J3 are built. J4 and M1 follow J3. J5a and S1 can run in parallel after J4. J5b follows J5a. E1 needs all of them.

## After Phase 8: adoption (D130's second half; planned after v0.3.0)
- **Dogfood:** Coxswain runs real goals on the owner's own repos day to day, not only planted ones. Each run's defects become slices.
- **A QUICKSTART a stranger can follow:** proven by a fresh cluster set up from the document alone, with every command pasted.
- **The CNCF landscape:** an entry submitted by the owner (none exists, R56).

This gets its own plan once v0.3.0 ships.

## Explicitly deferred (keep their homes)
- **The oscillation detector** (D96), the **per-node CNI probe** (D80(b)) and **agent roles** (ADR-0010): unchanged.
- **The Judge as a gate** (J2): still dropped (D122). Every slice here keeps the Judge advisory.
- **arm64 hardware runs and an arm64 build in PR CI** (D137): not in this phase.

## Risks
- **The scan's false positives.**
  - `TestMain` with `os.Exit(m.Run())` is normal Go. The pattern is `os.Exit` inside `init`, not `os.Exit` anywhere.
  - A false hit makes a draft, never a failure, and the column says why.
- **A second proxy (J5b)** is a new network path. It follows the I265 rule: its caller is in the structural spec, with a NetworkPolicy and a can-i proof.
- **The column is a summary.** One value can hide a second signal (the scan and the Judge both flagging). D138 fixes the order, and the conditions keep the detail.

## Owner decisions

| ID | Question | Recommendation | Decision |
|---|---|---|---|
| D130 | What comes after Phase 7? | — | **Harden verification (D131–D136), v0.3.0, then adoption** (owner, 2026-11-11). |
| D131–D137 | See R57. | — | Decided (owner, 2026-11-11). |
| D138 | **The `Judge` column's values and their order.** One column shows the Judge and the scan. | **One value, the most serious signal first:** `Suspicious` (scan hit) > `False` > `Truncated` > `Unknown` > `True`. Blank when there is no Judge and no hit. The conditions keep every signal. | **As recommended** (owner, 2026-11-12). |
| D139 | **The release default.** `release.yml:117` passes `--prerelease`, so v0.3.0 will publish as a prerelease, as v0.2.0 did until the owner promoted it. | **Drop `--prerelease`.** Releases are proven before the tag (R1's method), so a tag is a release. It is a workflow change, so it is an owner push, like #477. | **As recommended** (owner, 2026-11-12). |
| D140 | **Lessons from a flagged Loop** (found by E0). | **Withhold them** when the Judge is false, Unknown or truncated (the D133 set), and after S1 on a scan hit. | **As recommended** (owner, 2026-11-13). |

## Estimate
- **Small slices:** H0, J3 and J4 take half a day to a day each.
- **API work:** J5a is about a day. J5b is about two days.
- **S1:** 2–3 days.
- **Reviewer lab runs:** E0 and E1, half a day each. R2 is about a day.
- **Total:** about 8–10 builder days, or 2–3 weeks of wall-clock with review rounds.
