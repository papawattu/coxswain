# Phase 1 review, round 58: an operator restart stops every running agent until its first CNI probe

**Scope:** one finding, from the v0.3.0 upgrade runs R2b and R2c on kind-ref (11-17), at `52a52ed` (v0.3.0) and main `996bc74`. The owner asked for it to be raised as a review item. This round does not audit the window since R57 (`a7eed6b` → `996bc74`, Phase 8). R57's open items I271 and I272 are not re-checked here and stay open.

## The runs

- **R2b** (11:48Z–12:05Z):
  - v0.2.0 → ghcr v0.3.0, with r2-across live across the upgrade.
  - The lab's operator Deployment has no `--leader-elect`, so both operator pods reconciled at once during the rollout.
  - Verdict NOT PROVEN on capture only. Every upgrade check passed.
- **R2c** (13:50Z–13:57Z): the same upgrade with `--leader-elect` on, as `config/manager/manager.yaml:64` ships it.
  - **R2 RESULT: PASS.** Both Loops Succeeded, delivered PR 17 and PR 18, and got Judge score 100. The candidate log has `forbidden=0`.
  - Each operator log was streamed from its pod's start.
  - Events were watched with their real times.
  - The runner log was captured per container.
- **Evidence:** reviewer-local and not committed: `.samples/r2b-v030/run1/` and `.samples/r2c-v030/run1/`. Every line quoted below comes from those files.

## P2

- [ ] **I273: a new operator leader holds every Loop's sandbox Suspended until its first CNI probe, which stops agents mid-phase**

  **Where:**
  - `internal/cni/prober.go:44-47`: the probe holder is in memory, and "Its initial state is Unknown".
  - `internal/controller/loop_controller.go:1615`: an Unknown result returns "no CNI probe result yet; sandbox held Suspended (fail-closed)".
  - `cmd/main.go:451`: the probe Runnable is leader-elected, with its first probe at startup.
  - `cmd/main.go:408`: `LeaderElectionReleaseOnCancel` is left commented out, so a new pod waits for the old lease to expire.
  - `runner/cmd/main.go:128`: `signal.Notify(stopCh, syscall.SIGTERM, …)`. The runner reads `stop` only before a phase starts (`runner/phase.go:577`) and while idling (`runner/phase.go:298`), not during the model loop.

  **Problem:** an operator restart Suspends sandboxes whose network enforcement was already proven. This happens on every upgrade, crash or leader handover.

  The hold itself is the D38 fail-closed design, working as written. But the kubelet stops the running agent, and the agent then either:
  - finishes inside the 30 s grace period, or
  - is SIGKILLed with its phase lost.

  The runner never acts on the SIGTERM, so it keeps running until the grace period ends.

  Shown on kind, R2c (`--leader-elect` on):
  - 13:52:46: r2-across's Implementing container starts (`r2-across-runner.log:9`).
  - 13:52:56: the v0.2.0 pod shuts down (`r2-operator-v020.stream.log:19029`).
  - 13:53:14: the v0.3.0 pod acquires the lease (`r2-operator-candidate.stream.log:9`), 18 s after the old pod stopped.
  - 13:53:16: Warning `NetworkEnforced=Unknown … no CNI probe result yet; sandbox held Suspended (fail-closed)`, `SandboxSuspended`, and `Killing … Stopping container agent` (`r2c-events.txt`).
  - 13:53:41: `CNI probe result changed {"old": "Unknown", "new": "CNIEnforced"}`.
  - 13:53:45: the agent logs `headCommit=59070f7… staged=1` and `status=success` (`r2-across-runner.log:10`). That is 29 s after the stop, inside the 30 s grace. Its work survived only because its model call finished in time. Its exit code was not captured: the pod was gone before the next poll.

  Shown on kind, R2b (no leader election):
  - 12:00:59: `Killing … Stopping container agent`.
  - 12:01:29: the agent container ends with exitCode 137 "Error", 30 s after the stop (`r2b-poll.log`). A new sandbox pod was created at 12:01:33, and the Loop still Succeeded at iteration 0.
  - 12:00:58–12:01:09: the sandbox flaps between Ready and Suspended about once a second while two operators reconcile at once. With leader election (R2c) there was a single stop and no flapping.

  What the hold protects: NetworkEnforced is a property of the cluster's CNI, and an operator restart does not change it. The NetworkPolicy and the KubeArmor policy stay in force while the operator is down. That leaves the question in D142.

  **Fix:** D142.

  **Acceptance:**
  - An upgrade run by the R2c method on kind, with a Loop mid-Implementing at the handover:
    - no `Killing` event for that sandbox between the new leader's lease and its first probe;
    - the agent container keeps the same container ID from before the upgrade to its own exit.
  - An envtest gate spec (the exact behaviour depends on D142) that fails with the change disabled. Per the test norms, the mutation diff and the focus command are in the PR.
  - A spec showing that a sandbox not yet created still waits for the first probe, so fail-closed still holds for new pods.

## Owner decision

- **D142: what a new operator leader does to sandboxes before its first CNI probe.**
  - **(a) Recommended:**
    - Before its first probe, a new leader stops no sandbox that is already running with `NetworkEnforced=True` recorded on its Loop.
    - Only sandbox creation is held.
    - The first probe result applies to every Loop as now: CNIUnenforced or ProbeUnavailable still Suspends them.
    - Also set `LeaderElectionReleaseOnCancel: true`, so the 18 s lease gap goes away. Check first that nothing runs after the manager stops (`cmd/main.go:404-407`).
    - Fail-closed stays for every new pod. The exposure is a running sandbox for up to one probe (27 s in R2c) on a CNI that stopped enforcing while the operator was down.
  - **(b)** Seed the holder from a persisted last result (a ConfigMap or Lease annotation) with a short validity window. This is broader than (a), and it adds a stored result to keep trustworthy.
  - **(c)** Keep the hold as designed and document it in docs/OPERATOR.md: an operator restart stops running agents for one probe plus the lease gap. Running agents may lose their phase.
  - **Separately, whichever option is chosen:** the runner acts on SIGTERM during the model loop. It writes no claim and exits promptly, so a stopped agent is not SIGKILLed at 30 s. Today the runner registers for SIGTERM at `runner/cmd/main.go:128`, which turns off Go's default exit, and then never reads it during a phase.

## What I did not verify

- **How often a running agent is lost:**
  - R2b lost one phase (exit 137, repeated).
  - R2c's agent survived by 1 s of grace.
  - That is n=2, on kind, with qwen3.8-27b call times.
- **The R2c agent's exit code** (not captured).
- **Run 1's AgentFailed** (R2 run 1, 11-15): it did not recur in R2b or R2c. Its cause is still unshown.
- **Whether the lessons skip is related:** every phase of both R2c Loops reports `lessonsAvailable:false` (`MemoryWriteSkipped`). The runner logged no `LESSON:` or "dropping lesson" line, against a positive control of 1. The R2c agent said "The fix was already applied from a prior loop", so the gitea `gocli` main branch may already contain the fix. That cause is not shown.
