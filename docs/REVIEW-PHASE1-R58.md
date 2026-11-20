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

- [x] **I273: a new operator leader holds every Loop's sandbox Suspended until its first CNI probe, which stops agents mid-phase** (`a6ebe62`, `0b4d419`)

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
  - 13:53:45: the agent logs `headCommit=59070f7… staged=1` and `status=success` (`r2-across-runner.log:10-11`, 13:53:45.6). The stop was at 13:53:16, so this was under a second before the 30 s grace ran out. Its work survived only because its model call finished in time. Its exit code was not captured: the pod was gone before the next poll.

  R2b (no leader election) shows the same timeline. It does not show the cause:
  - The v0.3.0 pod's log for 12:00:58–12:01:09 was not captured. R2b's stream attached to the old pod, and the later read starts at 12:03:50.
  - Its only `NetworkEnforced=Unknown` event shows the last of 14 occurrences (12:01:09), not the first.
  - R2c, with leader election, does show the cause (`r2-operator-candidate.stream.log:1756`, the Unknown Event at 13:53:16, then `Killing`).

  The R2b timeline:
  - 12:00:59: `Killing … Stopping container agent`.
  - 12:01:29: the agent container ends with exitCode 137 "Error", 30 s after the stop (`r2b-poll.log`). A new sandbox pod was created at 12:01:33, and the Loop still Succeeded at iteration 0.
  - 12:00:58–12:01:09: the sandbox flaps between Ready and Suspended about once a second (the old pod's `ensured loop sandbox` lines at 12:00:58, :59, :59, 12:01:00 …). That fits two operators reconciling at once, but it is not shown. With leader election (R2c) there was a single stop and no flapping.

  What the hold protects: while the operator is down, a running sandbox is already unguarded. Its pod and policies persist, and nothing re-checks the CNI. The pre-probe hold therefore protects only the window after a new leader starts, and it pays for that by stopping the running agents. That trade-off is D142.

  **Fix:** D142.

  **Acceptance:**
  - An upgrade run by the R2c method on kind, with a Loop mid-Implementing at the handover:
    - no `Killing` event for that sandbox between the new leader's lease and its first probe;
    - the agent container keeps the same container ID from before the upgrade to its own exit.
  - An envtest gate spec (the exact behaviour depends on D142) that fails with the change disabled. Per the test norms, the mutation diff and the focus command are in the PR.
  - A spec showing that a sandbox not yet created still waits for the first probe, so fail-closed still holds for new pods.
  - A spec with **two** reconciles before the first probe. The second reconcile must not stop the sandbox either, because the first one must not have replaced the recorded True with Unknown (`loop_controller.go:6259-6263` writes the condition from `cniNetworkStatus()` on every reconcile).
  - A spec showing that a CNIUnenforced or ProbeUnavailable first result reaches every running Loop even when the regate channel is full.

## Owner decision

- **D142: what a new operator leader does to sandboxes before its first CNI probe.**
  - **(a) Recommended:**
    - Before its first probe, a new leader stops no sandbox that is already running with `NetworkEnforced=True` recorded on its Loop.
    - Only sandbox creation is held.
    - **Before the first probe, the condition is not rewritten.** Today every reconcile writes NetworkEnforced from the holder (`loop_controller.go:6259-6263`). In R2c, the first reconcile replaced the recorded True with Unknown at 13:53:16. Under (a), the window before the first probe must neither overwrite a recorded True nor read one that has already been overwritten.
    - **How stale a recorded True may be is part of the decision.** The condition carries only `lastTransitionTime`, not a probe time, so a True from days ago counts the same as a fresh one.
    - **The first probe result applies to every Loop as now:** CNIUnenforced or ProbeUnavailable still Suspends them. That result must reach every running Loop:
      - Re-gating runs only when the result changes (`internal/cni/prober.go:155`).
      - It sends one event per Loop into a 128-slot channel (`:240`) and drops an event when the channel is full (`:199`).
      - The comment at `:199` says "the next periodic probe re-gates", but an unchanged result never re-gates.
      - Today a dropped event leaves a Loop held. Under (a) it would leave the Loop running, which is fail-open.
      - So the first result needs guaranteed delivery, or a bounded requeue while the holder is Unknown.
    - Also set `LeaderElectionReleaseOnCancel: true`, so the 18 s lease gap goes away. Nothing runs after `mgr.Start` returns (`cmd/main.go:525-528`); the commented-out option is at `:408`.
    - **The exposure:** fail-closed stays for every new pod. A running sandbox keeps going until the first probe result, if the CNI stopped enforcing while the operator was down. That was 27 s in R2c, but it can be up to the probe timeout, 120 s by default (`cmd/main.go:142`), when a probe hangs.
  - **(b)** Seed the holder from a persisted last result (a ConfigMap or Lease annotation) with a short validity window. This is broader than (a), and it adds a stored result to keep trustworthy.
  - **(c)** Keep the hold as designed and document it in docs/OPERATOR.md: an operator restart stops running agents for one probe plus the lease gap. Running agents may lose their phase.
  - **Separately, whichever option is chosen:** the runner acts on SIGTERM during the model loop. It writes no claim and exits promptly, so a stopped agent is not SIGKILLed at 30 s. Today the runner registers for SIGTERM at `runner/cmd/main.go:128`, which turns off Go's default exit, and then never reads it during a phase.

## What I did not verify

- **How often a running agent is lost:**
  - R2b's agent was SIGKILLed (exit 137) and a new sandbox pod followed.
  - R2c's agent survived with under a second of grace left.
  - That is n=2, on kind, with qwen3.8-27b call times.
- **The R2c agent's exit code** (not captured).
- **Run 1's AgentFailed** (R2 run 1, 11-15): it did not recur in R2b or R2c. Its cause is still unshown.
- **Whether the lessons skip is related:** every phase of both R2c Loops reports `lessonsAvailable:false` (`MemoryWriteSkipped`). The runner logged no `LESSON:` or "dropping lesson" line, against a positive control of 1. The R2c agent said "The fix was already applied from a prior loop", so the gitea `gocli` main branch may already contain the fix. That cause is not shown.
