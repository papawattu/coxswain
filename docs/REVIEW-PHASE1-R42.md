# Phase 1 review, round 42: eleven merges with no P1 and no P2 in code, and a pause that left verification unrun

Round 42, 2026-10-27. Drafted by pi3 (the read-only pre-reviewer) and verified by the reviewer.
It follows R41 (`docs/REVIEW-PHASE1-R41.md`, PR **#320**, still open — so the `review/phase1-r41`
tag does not exist yet; see I158) and covers the **eleven merges** after `33595e6` (R41's last
merge, #319): **#321-#353**, i.e. `git log origin/main --oneline 33595e6..origin/main`.
Code line numbers are against `origin/main` at **`38222eb`**
(`38222eb2560fb40b8e0dc8ff7482f3b5543df795`, #349) unless an item pins a sha; where a line moved
inside this window the item names both. A bare `loop_*.go` / `loop_*_test.go` is under
`internal/controller/`.

The review was built from these input files, which are not committed: the pre-reviews and early
looks `.samples/prereview-{339}.md`, `.samples/early-{339,340,341,343,344,346,347,348,349}.md`, the
reviewer's own log `.samples/r42-reviewer-notes.md`, and the evidence those cite
(`.samples/f92-kind/`, `.samples/f75-mut*`). Facts the pi3 role could not verify read-only (anything
on `kind-coxswain`) say so explicitly. Every citation in this draft is audited in
`.samples/r42-selfcheck.md`.

| PR | Item | Merge |
|---|---|---|
| #321 | I161, I162: correct the F56 chart sentence and prune fixed items from Known issues | `05ca5bf` |
| #326 | I154: four comments no longer describe the crashloop C2 removed | `9f77622` |
| #327 | I159: scope the F92 hold's phase sentence to a fresh Loop | `818c7ba` |
| #334 | F71: fix QUICKSTART §9 (four images built, Step 8's uncreated manifest) | `eeb91dc` |
| #340 | F81: let the apiserver allocate the F11 spec's ClusterIP (no pinned 10.0.99.99) | `1ebc208` |
| #345 | prereview-303 N1+N2: last crashloop-claim site, log-header convention | `dcdd824` |
| #347 | F92 S1: pin the clone-pending requeue exactly instead of `> 0` | `32d9f62` |
| #348 | F92 docs: the held shape schedules no agent pod, and how we know | `2ea7654` |
| #350 | I160 (doc only): the F56 entry joins the condition and the Event clauses | `d4b3a69` |
| #353 | F61: compare the kustomization's slash paths with `path.Join` | `bbe2cad` |
| #349 | requeue: one `workRequeue` constant for the five bare 5s literals | `38222eb` |
| #351 | F57a: pin the rollback arm's unconfirmed-create branch | **closed, duplicate of #328** |
| #352 | OWNER: docs: the VerifySuspicious glossary entry stops pinning a sha | **open** (owner-merge) |

**Status:**

- **No P1 and no P2 in merged code.** The window's merges are two docs corrections, four
  comment/doc PRs, three test-only PRs, one docs+sample PR and one refactor. Every merged head's
  `lint` and `test` concluded **success** (`gh api repos/papawattu/coxswain/commits/<sha>/check-runs`
  for all eleven). The round's one P2 (I165) is about **verification state**, not behaviour: two
  merged bodies' primary evidence is a mutation that was never run.
- **The window closes four R41 items, half-closes a fifth, and moves three more.** Fixed: **I154** by #326 `9f77622` plus
  #345 `dcdd824` (the fourth sentence, `loop_i74_sandbox_terminal_test.go:51-54`); **I159** by #327
  `818c7ba`; **I161** and **I162** by #321 `05ca5bf`. Half-closed: **I160**'s doc half by #350
  `d4b3a69` (its design half still waits on the owner). Still open with a draft: **I152** (#329),
  **I156** (#344, round 3 at `d1b04a6`), **I163** (#328; #351 was closed as its duplicate). Untouched:
  **I155** (it recurs — see I165 and I174), **I157** (a kind claim this role cannot check read-only),
  **I158** (no `review/phase1-r40` or `review/phase1-r41` tag exists: `git ls-remote --tags origin`
  lists `review/phase1-r39` as the highest), **I164** (a merged body cannot be corrected
  retroactively).
- **Twenty drafts are blocked on the disk, and the pause is the round's dominant fact.** `gh pr list
  --json number,isDraft` → **20** drafts open (#322, #323, #324, #325, #328, #329, #330, #331, #332,
  #333, #335, #336, #337, #338, #339, #341, #342, #343, #344, #346), against four open non-drafts
  (#296, #297, #320, #352). The host is at **93%**, 14G free (`df -h /` →
  `/dev/sda1 197G 176G 14G 93% /`), so no builder can run `make test`, `make lint` or a mutation: every
  draft body says "specified, not run" and relies on CI for its only execution. The reviewer's log
  records the state five times (`.samples/r42-reviewer-notes.md:9`, `:15`, `:19`, `:26`, `:40`).
- **#348 merged as the corrected text, not its first cut.** The first cut cited
  `.samples/f92-kind/06-loop-final.txt:37` (the file has **33 lines**) and attributed the missing agent
  pod to the BPF node affinity; the merged paragraph says the `Suspended` mode keeps no pod, cites
  `:17`, `:20-21`, `:26`, `:33` and the events block at `:19-33`, and adds the `--allow-unenforced`
  caveat (`docs/TROUBLESHOOTING.md:940-955`). All three defects are I173's subject; the reviewer verified
  the citations at merge (`.samples/r42-reviewer-notes.md:34`).
- **#349 merged after a red head.** At `1910f2f` both `lint` and `test` failed, on
  `internal/controller/loop_checkpoint.go:49` (`"time" imported and not used` — at `1910f2f`; #349's fix
  removed the import and that line is now blank); the merge head
  `42e0f4a` is green, and the merged tree has four test sites reading the constant
  (`loop_checkpoint_envtest_test.go:1408`, `loop_f92_kubearmor_silent_test.go:130`,
  `loop_p5a_memory_script_test.go:281`, `loop_s4_reader_test.go:908`), so the refactor is complete
  rather than partial. I174 records the red.
- **#350's merge leaves I160's design question where it was**: the doc clause joined the condition and
  the Event sentences (`docs/TROUBLESHOOTING.md:466-470`), and the commit message says "the design fix
  waits on the owner".
- **#352 is the owner's, not the builder's**: `CONTEXT.md` only, branch `fix/ctx-glossary-no-sha`,
  title prefixed `OWNER:`, and it removes the pinned `d9f5619` from `CONTEXT.md:61` — the residue R41
  recorded under #316. It is open and not a draft; nothing in this round may merge it.

## P1

- None merged. (Calibration: the window's sharpest defect was **caught before merge** — #337's first
  cut re-ran a completed restore on upgrade and would have deleted the workspace PVC; see I172. Its
  fix `9be5f73` is in the branch, and #337 is still a draft.)

## P2

- [ ] **I165: two merged PRs' primary evidence is a mutation nobody ran, and the pause makes that the normal state**

  **Where:** #340's merged body opens "Draft: the repeat/randomized verification and the F11 mutation
  are PENDING" (`gh pr view 340` body `:1`) and has a `## Mutation — PENDING` section (`:109`); #347's
  merged body ends its Verification section "Draft: the mutation above was not run." (`gh pr view 347`
  body, last line). Both merged at `1ebc208` and `32d9f62`; all eleven heads are CI-green
  (`check-runs` above). The pause: `df -h /` → `/dev/sda1 197G 176G 14G 93% /`, and
  `.samples/r42-reviewer-notes.md:9,15,19,26,40`.

  **Problem:** CI proves a spec passes; it cannot prove the spec *discriminates*. For #340 the whole
  claim is "the fixture no longer pins a ClusterIP, and the spec still has teeth", and for #347 it is
  "the clone-pending row now pins 5s exactly" — in both cases the proof is the mutation, and the
  mutation is the part that did not run. This is AGENTS R28 I92's class ("a mutation claim names the
  exact diff and the exact focus command, with its output") reached through the pause rather than
  through carelessness: the bodies are honest, which is why this is P2 and not P1, but a merged body
  is the record and these records carry an unverified claim.

  **Attribution:** the reviewer merged both, under its own working rule that a test-only PR "needs no
  mutation" (review bodies on #340 and #347 say "no mutation"). That rule was wrong for a PR whose
  whole claim is that a spec still discriminates: the slip is the reviewer's, not the builder's.

  **Fix:** when the disk frees, run the two named mutations and append the output to `.samples/`
  (`M1`'s exact diff is in each body, with its focus command); then either the claim is confirmed or
  the spec is fixed. Add the rule that a body whose primary evidence is a mutation does not leave
  draft until that mutation has an output line — "unrun" is a draft state, not a merged one.

  **Acceptance:** `.samples/r42-mutations/` holds an `EXIT=1` transcript for #340's F11 mutation and
  #347's `reconcileRequeueAfter` mutation, each opening with `# sha:` naming the merged sha; and the
  next builder PR whose body names a mutation either carries the output or stays a draft (checked at
  the ready mark).

## P3

- [x] **I166: the Known-issues list still names the two defects this window fixed** (`273e4cc` (#364))

  **Where:** `docs/RELEASE-NOTES-next.md:790-792` — "**F71**: the from-source build commands in
  `docs/QUICKSTART.md` §9 are wrong (a duplicated `PROXY_IMG`, `build-installer` given only the
  controller tag, and an uncreated `/tmp/d61-loop.yaml`)" — against #334 `eeb91dc`, which fixes
  exactly those three (`docs/QUICKSTART.md` §9's build block now runs `docker-build`, `runner-build`,
  `egress-proxy-build` and the two release-workflow `docker build` lines, and Step 8 applies
  `config/samples/loop-d61-proof.yaml`); and `:815-816` — "**F81**: an envtest spec is flaky: a
  hard-coded ClusterIP …" — against #340 `1ebc208`, which removes the pin and asserts the apiserver
  allocated one (`loop_i42b_egress_proxy_test.go:281-302`).

  **Problem:** the list tells a user two fixed defects are open. #321 `05ca5bf` set the precedent in
  this same file and window: as I162's fix it deleted F53's and F56's bullets and dropped F57 from the
  umbrella bullet (`+2/−6`). #334 and #340 did not prune their own, so the same class recurs twice.

  **Fix:** delete both bullets (leaving the historical F71 mention at `:666` alone — it is the F70
  section's account, not a status claim). An `### F71` / `### F81` release-notes entry is optional:
  neither PR shipped one, and both are test/doc-only.

  **Acceptance:** `awk '/^## Known issues still open/,0' docs/RELEASE-NOTES-next.md | grep -n 'F71\|F81'`
  prints nothing, and `git log --oneline -1 05ca5bf` still shows the pruning precedent.

- [ ] **I167: the fourth `loop-*` sample is not in the samples kustomization**

  **Where:** `config/samples/kustomization.yaml:3-8` lists `coxswain_v1alpha1_loop.yaml` (`:3`),
  `loop-fix-failing-test.yaml` (`:4`), `loop-implement-missing-endpoint.yaml` (`:5`),
  `loop-python-missing-function.yaml` (`:6`) and the two policies (`:7-8`);
  `config/samples/loop-d61-proof.yaml` exists (74 lines, added by #334 `eeb91dc`) and is listed
  nowhere. The F61 coverage spec asserts the kustomization's **`policies/`** entries only —
  `list` vs `shipped` built as `path.Join("policies", …)` and compared with `ConsistOf`
  (`internal/controller/loop_f61_policy_samples_test.go:138-145`) — so nothing notices.

  **Problem:** `kubectl apply -k config/samples/` (the documented one-liner) creates three of the four
  loop samples. Nothing is broken — `docs/QUICKSTART.md:581-585` applies the new sample by path, as
  its evidence run did — but a shipped sample that the samples bundle skips is the F50 class
  ("shipped samples pointing at repo contents they test") and it is invisible to the suite.

  **Fix:** add the file to the kustomization (it is a valid Loop: `spec.workspace.repo`,
  `spec.agent.*`, `spec.checkpoints`), or state in the file's header that it is applied by path only,
  and extend the F61 spec's coverage assertion from `policies/` to the loop samples.

  **Acceptance:** `kubectl kustomize config/samples` includes `loop-d61-proof.yaml`, or the header
  says why not and the F61 spec's `ConsistOf` covers the loop entries so a future omission fails.

- [ ] **I168: the `# sha:` evidence-header convention has no home in the repo**

  **Where:** #345 `dcdd824` carried "N2" — the log-header convention — as a comment-only change, and
  its body records the convention ("every capture opens with `# sha: $(git rev-parse HEAD)`",
  `.samples/prereview-303.md` N2). It exists nowhere in the tracked tree:
  `git grep -n '# sha:' AGENTS.md` → nothing; `git grep -n '# sha: ' docs/ hack/` → nothing. The only
  carriers are untracked evidence files (AGENTS I68: `.samples/` is never committed).

  **Problem:** a convention that lives only in uncommitted evidence is re-learned every session and
  lost on a host move — and #345 merged as "the last crashloop-claim site" with the convention half
  landing nowhere. The reviewer's log already treats it as standing practice
  (`.samples/r42-reviewer-notes.md:34`).

  **Fix:** one short paragraph in `AGENTS.md`, beside the I68/I76 evidence rules: every capture opens
  with `# sha: $(git rev-parse HEAD)`, one run per file, and a capture that carries no sha line says
  so in a labelled `# NOTE` header (the shape #344's note 3b used).

  **Acceptance:** `git grep -n '# sha:' AGENTS.md` finds the rule, and the next merged kind evidence
  block's files each open with that header.

- [ ] **I169: builder #2 ran `go vet` during the disk pause**

  **Where:** `.samples/r42-reviewer-notes.md:13` ("b2 ran go vet (93->94%, disclosed in #330) … go vet
  explicitly banned"); the builder's own disclosure is in #330's body — "*Disclosure:* exactly one
  local compile-class command was run before I re-read the disk state — `go vet ./internal/controller`"
  (`gh pr view 330` body `:8-9`) and its verification list repeats it at `:128-129`. Host state:
  `df -h /` → 93%, 14G free.

  **Problem:** the pause was briefed as "no local runs" but the builder read it as "no `make test`",
  and a compile-class command is exactly what fills the build cache. It moved the disk 93→94% and was
  reported afterwards instead of blocked beforehand. The disclosure is the right half of the
  behaviour; the command is the wrong half.

  **Fix:** the pause brief names the **class** (anything that writes the Go build cache: `go build`,
  `go vet`, `go test`, `golangci-lint`) rather than a list of make targets, and the builder re-reads
  `df -h /` before any such command and reports the reading, not the result.

  **Acceptance:** the next pause brief in the reviewer's log quotes the class and the reading; no
  `go vet`/`go build` appears in a builder session log while `df` reads ≥90%.

- [ ] **I170: builder #2 amended an unpushed commit, pushed mutation branches to origin, and slept 480**

  **Where:** `.samples/r42-reviewer-notes.md:22` ("b2 disclosed an amend of its own unpushed commit (no
  reviewed history touched; R42 note)") and `:23` ("it had pushed remote mutation branches
  f75-mut-M1..M4 and was sleep 480; reset; told to delete those refs"); the refs are gone —
  `git ls-remote origin 'refs/heads/f75-mut-*'` prints nothing — and `:24` records the cleanup. The
  mutation work itself is #337's, whose branch holds `12c8bd5` → `9be5f73`.

  **Problem:** three separate slips in one session. The amend is harmless in itself (the commit had
  never been pushed) but it was disclosed after the fact, which makes the builder's history
  unauditable in the window it happened. The remote mutation branches put throwaway commits into the
  shared review surface — exactly what AGENTS means by keeping mutations local. `sleep 480` burns a
  pause window and hides a stalled agent: the harness reports an agent that has stopped, not one that
  is sleeping.

  **Fix:** no amend of any commit that has been pushed or reviewed (a follow-up commit instead); a
  mutation is applied in a scratch worktree and shown as a diff in the body, never pushed; and waits
  are bounded polls on a condition, never a fixed sleep.

  **Acceptance:** `git ls-remote origin | grep -c 'mut-'` → 0 at every ready mark; every sha the
  reviewer has seen on a branch still resolves by sha (`git cat-file -e <sha>` for each reviewed head
  of the window), i.e. no reviewed tip was rewritten; and no session log contains a `sleep` longer
  than the poll interval AGENTS allows.

- [ ] **I171: builder #2 opened a duplicate PR, and edited an owner-owned file in the same batch**

  **Where:** #351 is `state=CLOSED` (`.samples/r42-reviewer-notes.md:36`), titled "F57a: pin the
  rollback arm's unconfirmed-create branch", and its only changed file is
  `internal/controller/loop_f57_restore_envtest_test.go` — the same file as the open #328 ("I163: spec
  the rollback's F57a unconfirmed-create arm"). In the same push batch #352 opened with
  `CONTEXT.md` as its only file (branch `fix/ctx-glossary-no-sha`, title `OWNER: …`), which is the
  correct handling (`CONTEXT.md:61` carries the pinned `d9f5619`) but was flagged for the owner only
  after the reviewer sorted the batch (`:36-38`).

  **Problem:** #351 duplicated work already in an open PR and cost a review-side close; the reviewer's
  log then had to state the dedupe rule. The `OWNER:`-titled PR is fine in form — the slip is that the
  batch arrived without the check that would have separated the two.

  **Fix:** before opening a PR, list the open PRs that touch the same files
  (`gh pr list --json files`) and say in the body which one it supersedes if any; files the owner owns
  (`CONTEXT.md`, `docs/REVIEW-*`, `AGENTS.md`) are titled `OWNER:` and never marked ready by the
  builder.

  **Acceptance:** the next builder batch has no PR whose changed-file set duplicates an open PR's, and
  every owner-owned-file PR carries the `OWNER:` title and stays a draft until the owner acts.

- [ ] **I172: #337's first cut changed an upgrade path and would have deleted the workspace PVC**

  **Where:** first cut `12c8bd5` ("F75: a keyless Restored marker does not cover a target the spec
  names"): it made every keyless pre-F35 marker "incomplete" once the spec named a restore target. The
  fix `9be5f73` replaces that with a comparison against the target the marker itself names —
  `legacyMarkerCoversSpec`, `loop_restore.go:770` on `fix/f75-f77-restore-do-now` — and its commit
  message records the finding: "Review round on pi3's early-337 (P1 F1, P2 F2, notes F3-F6)". Reviewer
  log: `.samples/r42-reviewer-notes.md:23-25`.

  **Problem:** the first cut's rule was reached by **upgrading**, not by an unusual mutation: every
  rollback or fork completed on v0.1.1/v0.1.2 has a keyless marker (`restoreFrom`/`forkOf` are
  immutable, so the spec usually names a target), so the first reconcile after upgrade would have
  re-run a completed restore — deleting the workspace PVC and restoring the old snapshot over
  everything done since, announced by an ordinary `Restored` Event. My early look caught it
  (`early-337` P1 F1) and the round-2 branch carries the spec (`loop_f75_legacy_restore_envtest_test.go`)
  and the change that keeps a lost-PVC marker incomplete.

  **Fix:** already fixed on the branch. The round records the rule: an upgrade-shaped change ships, in
  its **first** cut, with a spec that constructs the pre-upgrade state (keyless marker + bound PVC + a
  completed rollback) and a mutation that must fail it.

  **Acceptance:** on `9be5f73`, deleting the `legacyMarkerCoversSpec` call fails
  `loop_f75_legacy_restore_envtest_test.go`; and the next restore/marker change's first cut carries
  that legacy-state spec (checked at the early look, not the second round).

- [x] **I173: #348's first cut cited an evidence line that does not exist, and named the wrong cause for the missing pod** (`fec72f1` (#365))

  **Where:** the first cut of #348 cited `.samples/f92-kind/06-loop-final.txt:37` — the file has **33
  lines** (`wc -l`), the `WaitForFirstConsumer` evidence is at `:26` and `:33`, and the "no pod"
  support is the sandbox's `Suspended` mode at `:17` with `SandboxSuspended` Events at `:20-21`. The
  citation was **mine**: `.samples/prereview-308.md` finding 7 wrote `:37` and the builder inherited
  it (`.samples/early-348.md:26-40` records the correction and the timestamps: the evidence file was
  written 11:27:24 on 26 Oct, my note at 12:50). The cause was the builder's: the paragraph attributed
  the absent pod to `applyBPFNodeAffinity`'s required BPF node label
  (`loop_controller.go:1875`, applied at `:2037` — both **stale at `38222eb`**: #349's +11 moved them
  to `:1884` and `:2046`) while the mechanism is the operating mode — a
  suspended sandbox's pod is deleted and none is created
  (`agent-sandbox@v1.0.4 controllers/sandbox_controller.go:1321-1352`) — and the paragraph's own "no
  `FailedScheduling`" evidence argues against the affinity story (an unschedulable pod emits exactly
  that Event). The merged text is corrected, including the `--allow-unenforced` caveat
  (`docs/TROUBLESHOOTING.md:940-955`).

  A third slip is the merged paragraph's shape: it cites **code line numbers**
  (`loop_controller.go:1875`, `:2037`, `internal/engine/kubearmor_enforcer.go:487-509`, `:481-484`),
  which the same file forbids two hundred lines above — "file paths rather than line numbers, which
  change as main moves on" (`docs/TROUBLESHOOTING.md:460`) — and two of the four were stale within the
  same window (#349 merged at 07:35, twenty minutes after #348 at 07:15).

  **Problem:** all three are the class this round keeps meeting: a claim that is checkable in one
  command and was not checked. The evidence citations *were* caught: the reviewer's log records
  "citations verified by me" at merge (`.samples/r42-reviewer-notes.md:34`). The reviewer's share is
  that the check covered the evidence lines and not the file's own paths-not-lines rule
  (`docs/TROUBLESHOOTING.md:460`), so four code-line citations passed review and are wrong in the
  merged tree — which is why this item stays open.

  **Fix:** rewrite the four line citations in `docs/TROUBLESHOOTING.md:946-950` as symbol + file
  (`applyBPFNodeAffinity` in `internal/controller/loop_controller.go`;
  `KubeArmorEnforcer.BPFNodeAffinity` in `internal/engine/kubearmor_enforcer.go`), per the file's own
  convention. The rules to record: an evidence citation is grepped before it is written
  (`wc -l`/`sed -n` on the cited file); a doc that explains *why* something did not happen cites the
  code that implements the mechanism, not a plausible one — here the sandbox controller, not the
  affinity; and a merged doc does not cite code line numbers in a file that says not to.

  **Acceptance:** the next doc PR's body lists each evidence citation with the line's text quoted from
  the file at that line, and the reviewer spot-checks two (the I164/I173 class, checked at the ready
  mark).

- [ ] **I174: #349's first cut could not compile, and its body claimed a purity CI had not yet reached**

  **Where:** `gh api repos/papawattu/coxswain/commits/1910f2f/check-runs` → `lint` failure and `test`
  failure, with the annotation `internal/controller/loop_checkpoint.go:49 "time" imported and not
  used` and `cmd/main.go:45 could not import …/internal/controller`; the merged head `42e0f4a` is green
  (`…/commits/38222eb/check-runs` → success). Cause: the four `5 * time.Second` returns were
  `loop_checkpoint.go`'s only `time.` uses, so the import at `:49` became unused. The body now says
  what happened: "the red at `1910f2f` is exactly why this branch is still a draft until it reports
  green" (`gh pr view 349` body).

  **Problem:** the draft body was published on a head that could not build, with a "no behaviour
  change" framing, and it also carried a mutation claim that could not happen (my early-349 finding:
  changing the constant cannot fail a spec that reads the constant). CI caught the compile error in
  minutes — this is the system working — but the sequence (push → claim → red) is the one AGENTS R28
  I92 asks not to produce, since the review surface briefly held an unbuildable tree with a confident
  body.

  **Fix:** for a spec/refactor PR whose stated evidence is CI, the body's first push says "CI pending"
  rather than stating the property; and any mutation claim is checked by reading which side of the
  assertion reads the value (the builder did both in the follow-up: the body now has a "Mutation —
  corrected" section saying there is no mutation).

  **Acceptance:** the next spec-only PR's first-pushed body states the CI state as pending, and its
  mutation section either names a mutation that fails a named assertion or says plainly that there is
  none.

- [ ] **I175: pi3 wrote an unrequested pre-review, and a mechanism claim it had not traced**

  **Where:** two slips of mine in this window. (a) `.samples/prereview-339.md` was written at 05:01
  after the reviewer's 04:58 note asked for an **early look** — which exists separately as
  `.samples/early-339.md` (04:52) — and the reviewer's log records only the early look
  (`.samples/r42-reviewer-notes.md:26`). (b) `.samples/early-344.md:154-168` ("Correction to my own
  finding 3"): my round-1 recommendation ("add the sibling's `activeDeadlineSeconds` Event assertion,
  then the deadline arm moved below `pod == nil` fails the new spec") was wrong, because
  `deadlineExceeded` is read from the Job at `loop_verify_job.go:1447` and passed to
  `verifyInfraFailureMessage` independently of the arm; the builder implemented exactly what I asked,
  `191a454` carried the resulting false comment, and `d1b04a6` removed it.

  **Problem:** (a) an unrequested artifact costs review-side attention in the middle of a pause when
  attention is the scarce resource, and the standing role says not to produce one. (b) the wrong
  mechanism produced a wrong acceptance recommendation that reached the tree and needed a third round;
  the cause was recommending an assertion without tracing which line supplies the value it asserts on.
  Both are mine, not the builder's — the builder followed the note faithfully.

  **Fix / acceptance:** no artifact without a request (the reviewer's request is the instruction, and
  an early look is not a pre-review); and every "this mutation fails that assertion" claim carries the
  trace through the production read that supplies the value, quoted by line, before it is written into
  a note. Checked at the next pre-review: the round's own notes name the trace or say the mutation is
  untraced.

- [ ] **I176: the reviewer's own tooling slips this window**

  **Where:** the reviewer's log `.samples/r42-reviewer-notes.md` and the session record: a background
  CI wait for #321 used `gh pr checks --json`, which this `gh` does not support, so the loop could never
  finish (caught at the next check-in and replaced with plain output); and a #347 review command nested
  a heredoc inside `$(...)`, which broke on parentheses in the body (it failed before posting anything;
  re-run with `-F body=@file`).

  **Problem:** neither changed the repo, but both are the class this round records for the builders: a
  tool used without checking it does what was assumed.

  **Fix:** the reviewer checks a command's flags once before backgrounding a wait on it, and passes every
  GitHub body from a file.

  **Acceptance:** no background wait in the next round's log uses an unverified flag, and review bodies are
  posted with `-F body=@file`.

## Owner decisions

**D90 (new): the disk-space pause — how long, and what it licenses.** Twenty drafts are frozen
(`gh pr list --json isDraft` → 20), the host has 14G free (`df -h /`), and the pause has already been
carried through one window of merges (this one). The round needs a decision, not a build: **(a)** keep
the pause and accept that drafts merge on CI alone with mutation claims unrun (I165's state, made
explicit); **(b)** unpause for a bounded clearance run — free space, then re-run the two merged
mutations (#340, #347) and the twenty drafts' mutation sets in priority order, merging nothing new
until the backlog's evidence exists; **(c)** keep the pause for merges of *behaviour* changes but allow
docs/test-only merges (this window's eleven) to continue, which is what actually happened and what the
reviewer's log shows (`:12`, `:18`, `:29`, `:33`, `:37-39`). **Recommendation: (c) then (b)** — the
window shows docs/test-only merges were safe under CI, and the accumulated unrun mutations are the
real debt. One piece of context for the choice, recorded because it shaped the window's process
findings rather than as a separate item: the reviewer's log shows **ten builder resets** in the
window (`.samples/r42-reviewer-notes.md:2,8,10,13,16,20,23,28,30,35`), one of them interrupting work
in flight at 92% (`:23`), and the two clearest builder slips (I170's remote mutation branches, I171's
duplicate PR) sit either side of a reset (`:23`, `:30`, `:36`). If the pause continues, it is worth
deciding whether it also bounds the reset cadence — a pause spent re-briefing a fresh builder is not
a pause that preserves work. **The decision is the owner's** because it trades review throughput against the R28 I92
evidence rule, and only the owner can authorise using the host differently.

**D89 stands** — #308 merged its option (a) at `7fd171d`; the round still wants the owner to confirm
it. Not exercised by this window.

**D88 stands** — `git grep -c D88 -- AGENTS.md` prints nothing, so the norm is still absent, and I165
and I174 are two more instances of the class it was meant to name.

**D84 / D86 are still open** (*per R40/R41*), untouched by this window.

**#352 is an owner action, not a review item**: `CONTEXT.md` only, `OWNER:`-titled, removes the pinned
`d9f5619` at `CONTEXT.md:61`. It needs the owner's merge, and it closes R41's #316 residue.

**R40/R41's tags are still missing** (`git ls-remote --tags origin` → highest is `review/phase1-r39`),
because #320 and #297 are open. R42's own tag must be applied to the merged commit once this round's
PR merges, and the glob must not match `r4` (I158's warning).
