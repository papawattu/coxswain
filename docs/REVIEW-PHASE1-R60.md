# Phase 1 review, round 60: D142 and D143 answered (a), I273 closed, I274's kind proof

**Scope:** the owner's answers to D142 (R58) and D143 (R59), the three PRs that implement them, and the two kind runs that prove them: R2d (11-17) and R3 (11-18), on kind-ref. Main is `bed9ba7`. This round does not audit the window since R59. R57's I271 and I272 are not re-checked here and stay open. Evidence is reviewer-local and not committed: `.samples/r2d/run1/` and `.samples/r3/run1/`, `run2/`. Every line quoted below comes from those files.

## Answered

- **D142 = (a)** (owner, 11-17). A new leader stops no sandbox that is already running with `NetworkEnforced=True`, until its first CNI probe. Only sandbox creation is held. `LeaderElectionReleaseOnCancel` is set. Merged as #506 (`a6ebe62`).
- **D142's separate item: the runner acts on SIGTERM during a phase.** Merged as #508 (`0b4d419`). On a stop during the model loop or a shell command, `PhaseRun` returns a zero Result: no claim, no commit, exit 0. The shell-command part was a review fix (`2365acc`): `execShell` now derives its timeout from the stop ctx.
- **D143 = (a)** (owner, 11-17). In Planning only, the agent container mounts `/workspace` read-only, plus a writable `subPath: .coxswain` mount at `/workspace/.coxswain`. The init containers stay writable. Merged as #509 (`bed9ba7`).

## The kind runs

- **R2d run A (#506), 23:24Z–23:29Z on 11-17.** The R2c upgrade method (v0.2.0 → candidate), with r2-across mid-Implementing at the handover (`r2d-run.out:233`).
  - r2-across's agent started at 23:24:58 (`r2c-poll.log:8`, `"startedAt":"2026-11-17T23:24:58Z"`). It wrote its Implementing claim at 23:26:04 (`r2-across-runner.log:9`).
  - In between, the candidate acquired the lease at 23:25:29 (`r2-operator-candidate.stream.log:9`). Its first probe ran at 23:25:57 (`:7461`, `"old": "Unknown", "new": "CNIEnforced"`).
  - No Killing for r2-across-sandbox in [23:24:56, 23:26:04] (`r2d-run.out:235`). The table's two Killing rows are later: r2-across at 23:27:11 after its claim, and r2-smoke at 23:29:28 (`r2c-events.txt:130,219`). The same awk over R2c's table finds R2c's 13:53:16 Killing (the positive control).
  - New sandboxes still wait for the first probe. r2-smoke, created on the candidate before the probe, was held: `23:25:30Z Warning NetworkEnforced r2-smoke … NetworkEnforced=Unknown reason=Unknown: no CNI probe result yet; sandbox held Suspended (fail-closed)` (`r2c-events.txt:52`).
  - Both Loops Succeeded and delivered PR 19 and PR 20. The candidate log has `forbidden=0` (`r2d-run.out:214-225`).
  - The script printed `R2 RESULT: NOT PROVEN` (`:234-236`) on one capture gap. It recorded the agent container at the upgrade instant, 23:24:56, when the pod was still `PodInitializing` (`r2c-poll.log:7`), so it recorded "none". That the container which made the claim is the one that ran across the handover is shown by the poll log (`startedAt 23:24:58`, never restarted before the claim) and by the events. The verdict on #506 (review 5948466850) explains this.
  - The lease gap was still 18 s, because v0.2.0 has no ReleaseOnCancel. Only upgrades from #506 onwards lose it.
- **R2d run B (#508 before its review fix), 23:30Z–23:34Z.** spec.suspend=true mid-Implementing.
  - Killing came at 23:31:25 (`r2d-events-late.json`). The runner logged `status= observedPhase= summary=""` at 23:31:54.96 (`r2-suspend-runner.log:7`), with exit 0 and an empty termination message (`r2d-run.out:278-279`). That is 29 s of the 30 s grace.
  - A scratch unit test, a stop during `sleep 20`, returned after 20 s. That points at the shell command. The kind run had no tool trace, so the shell command is not shown to be what delayed it there.
  - `r2d-run.out:280` reports "has a claim line". That is a script bug: the check matched the empty `observedPhase=` of the stop line. R3 fixed it (see I275).
- **R3 run 2 (#508 after `2365acc`, and #509), 20:42Z–20:47Z on 11-18.** Candidate `badd801` = main `a6ebe62` + #508 + #509, the same tree as main `bed9ba7` (`git diff badd801 bed9ba7` is empty). One Loop, r3-ro: the gocli sample with no delivery. Its goal starts with "run exactly this shell command and wait for it to finish: sleep 40".
  - **Planning (#509),** `run2/r3-p-exec.log`:
    - `/proc/mounts` shows `/workspace ext4 ro,relatime` and `/workspace/.coxswain ext4 rw,relatime`.
    - `: >> /workspace/round.go` fails with `cannot create /workspace/round.go: Read-only file system`. `git ls-files` confirms round.go is tracked.
    - `git commit --allow-empty` fails with `fatal: Unable to create '/workspace/.git/index.lock': Read-only file system`.
    - A touch and rm under `/workspace/.coxswain` succeeds.
  - **Implementing (#509):**
    - The same grep shows `/workspace ext4 rw,relatime` and no `.coxswain` mount (the control for the `ro` match).
    - `.coxswain/PLAN.md` is 1706 bytes, written by the Planning runner through the subPath.
    - The Loop went Planning → Implementing → Verifying → Succeeded.
  - **The stop (#508),** `run2/r3-run.out`:
    - At 20:44:27, `sh -c sleep 40` and `sleep 40` were running in the Implementing agent (`r3-b-sleep.log`).
    - The Loop was suspended at 20:44:29.
    - Events: `20:44:29Z Paused`, `20:44:29Z Killing Stopping container agent`.
    - The runner logged the stop result at 20:44:29.299, in the same second as the Killing event (events have second resolution). The script's 1 s poll measured 2 s from the suspend to `terminated`.
    - A pod watch recorded the container `terminated` with `exitCode 0`, an empty message and `finishedAt 20:44:29Z` (`r3-b-podwatch.json`).
    - The Loop resumed and Succeeded.
  - The operator stream log has 36,896 lines and `forbidden` = 0.
  - Run 1 (`run1/`, 20:35Z) showed the same mounts and the same stop timing: Killing at 20:37:02, stop result at 20:37:02.12. It was NOT PROVEN on three script gaps (I275).
- **The lab now runs main.** The lab now runs `bed9ba7` (11-20): all five images, CRDs, RBAC and admission. The rollout's log has `forbidden=0`. The lab's admission had drifted: before R3 it had only `coxswain-deny-ephemeral-containers`, and main's `coxswain-deny-loop-status-writes` is now applied too.

## Closed

- I273 is ticked in R58 (`a6ebe62`, `0b4d419`). Each acceptance item is met:
  - no Killing between the lease and the first probe (R2d A);
  - the same container across the handover (R2d A, from the poll log);
  - the gate spec, its mutation, and the new-sandbox, two-reconcile and first-result-delivery specs (#506, review 5948466850);
  - the runner part (#508, R3 run 2).

## P2

- [ ] **I275: the reviewer's slips in this window**

  **Where:** my #508 brief, and the R2d and R3 lab scripts.

  **Problem:**
  1. **My #508 brief caused the 29 s stop.** Point 4 said "Shell tool commands keep their own timeout". b4 did what it said, and R2d run B showed the cost. I owned it in the #508 review, and the fix is `2365acc`.
  2. **R2d recorded the agent container at the upgrade instant.** The pod was still initializing, so the same-container check printed NOT PROVEN on a capture gap. It should record the container once it is running.
  3. **R2d's run B did not restart the event watch.** Its events were recovered with a late `get events -o json`, within the events TTL.
  4. **R2d and R3 run 1 grepped `observedPhase=` as a claim.** That matched the runner's stop line, which has an empty `observedPhase=`, so the check reported NOT PROVEN when there was no claim.
  5. **R3 run 1's exec'd git stopped at "dubious ownership".** It never touched the tree, so the commit check proved nothing.
  6. **R3 run 1 read the exit code with a 1 s poll.** The pod was gone before the poll, so the exit code was not captured.

  None of these produced a false PASS. Each one either printed NOT PROVEN or was caught before the verdict.

  **Fix:**
  - (1) A brief that keeps existing behaviour says what that behaviour costs, and the reviewer checks that cost before sending it.
  - (2)–(6) are fixed in `.samples/r3/r3-lab.sh`:
    - `git -c safe.directory=/workspace`;
    - `get pods -w -o json` from before the suspend;
    - a claim check of `observedPhase=[^ ]`;
    - one event watch for the whole run.

  **Acceptance:** R3 run 2 shows (4)–(6) working. Close this at R61 if no new slip of this kind shows up in that window.

## Owner decision

- **D144: whether I274 needs the runner specs its acceptance names, now that D143 = (a) is a mount.**
  - What R59's I274 acceptance asked for, and where it stands:
    - an envtest gate spec with its mutation: **met**, #509; my focused mutation run gave 2 FAIL;
    - a spec where the Planning shell edits a tracked file, and one where it runs `git commit`: **not as specs, but both shown on kind** (R3 run 2);
    - a spec where Manual approval rejects a plan after Planning edited a file: **not done**;
    - the kind run, with PLAN.md, the plan-hash and `.coxswain/gotmp` still working: **met**:
      - PLAN.md was written through the subPath;
      - the Loop advanced past Planning's plan-hash check;
      - `.coxswain` is writable, and gotmp is inside it;
      - no Go build ran in Planning, so gotmp itself was not exercised.
  - **(a) Recommended: accept the kind evidence for the two edit/commit items, and drop the Manual-reject spec.**
    - Under (a), the kubelet blocks the write. A runner spec would have to fake the read-only tree (for example `chmod`), so it would test the fake, not the operator.
    - The envtest mutation already fails if the mount loses `ReadOnly`.
    - The Manual-reject case, as I274 states it, cannot arise: Planning cannot write the tree, so no tree edit survives a rejected plan. What does survive is R59's residual for (a): `.coxswain` (PLAN.md included) and `/scratch` stay writable by the Planning model. That residual is not part of I274.
    - I274 is then ticked at R61 with `bed9ba7`.
  - **(b)** b4 writes the runner specs on a chmod'd workspace (D143(a) step 2) and the Manual-reject spec, then I274 is ticked.
  - **Either way, R59's "whichever option" item stays owed:** capture `result.json`'s ToolTrace in the lab evidence, so a run can show which phase ran which command. R3 did not capture it.

## What I did not verify

- **The read-only plus subPath mount on storage classes other than kind's local-path.** R3 is n=1, on local-path.
- **The agent-sandbox controller's own handling of the PodSpec.** Only its API type was checked: a plain `corev1.PodSpec` in agent-sandbox v1.0.4.
- **A model that tries to edit or commit in Planning by itself.** In R3 the EROFS failures came from my `kubectl exec`, not from the model. The model's own Planning commands are not traced (D144).
- **Manual approval with (a).** Both runs used Auto.
- **Two Implementing claims from r2-across.** It logged one at 23:26:04.08 and another at 23:26:05.98 (`r2-across-runner.log:9,13`). The second comes from a container restart after the claim, which R2c also showed. Its cause is not shown, and it is not new in this window.
