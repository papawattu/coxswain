# Task: S5b — make sample-run + EVIDENCE.md (end-to-end demo)

PR: https://github.com/papawattu/coxswain/pull/53
Branch: slice/s5b-sample-run
Head: ecb2114 (marked ready for review via GraphQL; comment 5967833080 posted)

## Commits (on slice/s5b-sample-run, all pushed)

17fa16b S5b: make sample-run + checked-in Task 1 Loop manifest (scaffold)
03072e4 S5b: fix samples-git-cred type (kubernetes.io/basic-auth is immutable) + sample-run driver
319256a S5b: pin Task 1 runner image to coxswain-runner:s5b1 (the build tag)
a487436 S5b: drop policyRefs from the demo Loop (exec fencing can't cover an agent shell yet, D41)
0a00490 S5b: make the demo Loop's runner image overridable (RUNNER_IMG)
b8edd63 S5b: leave spec.agent.image empty + preflight the controller's --runner-image
40aeb31 S5b: fix verifyOutcome treating an in-progress check as a failure (S5a regression)
ecb2114 S5b: evidence generator fixes + agent diff section + --evidence-only mode

## The P1 found and fixed (verifyOutcome, internal/controller/loop_verify_job.go)

The first real run passed every verify step (tamper clean, check-0 go build
exit 0, check-1 go test exit 0) yet the Loop iterated and ended
Failed: MaxIterationsExceeded with 'check-0 failed (exit 0)'. Root cause:
a check-* init whose State.Terminated is nil (still Running/Waiting when
the operator polls) was treated as a failure (verifyIterate, exit 0). A
non-terminated check is PENDING, exactly like a non-terminated tamper:
verifyNoDecision + requeue. New envtest spec covers both shapes (check-0
Running; check-0=0 with check-1 Waiting); mutation (restore the
iterate-on-pending branch) makes the spec fail (192 passed / 1 failed).
Full suite green: 193 specs in internal/controller.

## Demo run (kind coxswain-dev, controller coxswain-controller:s5b2)

Loop samples/gocli-task1 Succeeded at iteration 0 in ~5 min (Planning ->
Implementing -> Verifying -> Succeeded). Real vLLM at 192.168.1.20:8000,
in-cluster Gitea only (no external pushes). Evidence at
.samples/gocli-1/EVIDENCE.md (regenerated via --evidence-only after the
generator fixes). Agent's change verified: round.go rounds half away from
zero (base 69b361c -> 286c2ec), plus the built gocli binary (a limitation).

## Environment state (coxswain-dev kind cluster)

- controller deploy coxswain-controller-manager runs coxswain-controller:s5b2
  (the verifyOutcome fix) with --runner-image=coxswain-runner:s5b1.
- coxswain-runner:s5b1 and alpine/git are loaded into the kind node.
- Loop samples/gocli-task1 exists in phase Succeeded (evidence-only mode
  reads it; it does not block a fresh run — the driver deletes prior Loops).
- The restart-looping terminal-phase sandbox pod (gocli-task1-sandbox) is
  still around; harmless, and a follow-up item (below).

## make test / make lint

Both green (root + runner modules). `make test`: envtest controller suite
193 specs. `make lint`: 0 issues.

## Limitations / follow-ups (also in the PR body)

- The runner commits build artifacts (the 2.5MB gocli binary is in the
  verified commit). Follow-up: the runner's commit step should respect
  .gitignore (or the seed should ignore the binary).
- The dev model-proxy stand-in cannot count forwarded requests (per-pod-start
  marker only). The real egress proxy (I42a) audits per request.
- PolicyEnforced reads EnforcementDisabled even though KubeArmor was
  actually enforcing on this host (dev escape hatch --allow-unenforced);
  the condition text is misleading — follow-up.
- Exec fencing for the agent shell is D41 (AgentPolicy example is
  unreferenced for that reason).
- A terminal-phase sandbox keeps restarting (the runner refuses an unknown
  desired phase and the operator restarts it) until the Loop is deleted;
  the sample-run driver now waits for its deletion before a fresh run, but
  a fresh run is not required for evidence regeneration.
- kind clusters do not pull from a registry: alpine/git (evidence peek pod)
  must be pre-loaded; a preflight for this is a follow-up.

## Reviewer notes

- No force-push after 40aeb31 (follow-up commits only). An earlier amend
  (67fbf0b -> 40aeb31) happened before that instruction; the diff between
  them was the envtest lint fix only.
- The PR #53 body was replaced with the full evidence, how to run, the
  agent diff, test/lint results, the verifyOutcome fix + mutation, and the
  limitations. Marked ready via GraphQL (D40). Comment 5967833080.
