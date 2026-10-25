# Operator FAQ

Plain-language answers to the questions a new coxswain operator is most likely
to ask, taken from this `docs/` set. Each answer ends with a `See:` pointer to
the section that backs it. (Questions the docs do not answer are tracked
separately, not here.)

## 1. Why is my Loop Suspended?

The sandbox is held Suspended while a fail-closed gate is not satisfied. The
most common holds: `NetworkEnforced=False` (reason `CNIUnenforced` — the CNI
does not enforce NetworkPolicy, kindnet's default), the egress proxy pod not
Ready (external-repo clone is gated on it), the agent NetworkPolicy absent or
failed, a tool proxy that the Loop does not own or that is not Ready, the
manual-approval hold, a restore-in-progress hold, a budget pause, or the
KubeArmor policy gate.
`kubectl cox describe <loop>` shows the conditions; their `reason` field is
the most useful diagnostic.

See: [TROUBLESHOOTING.md#the-other-holds-on-a-suspended-sandbox](TROUBLESHOOTING.md#the-other-holds-on-a-suspended-sandbox)

## 2. Why did my Loop fail with TamperedVerify?

`TamperedVerify` is the tamper check's verdict: the diff between the base
commit and the verified commit across the protected globs was non-empty, i.e.
the agent changed a protected file (added, modified, deleted, or renamed).
That is distinct from a plain acceptance-check failure and from
`VerifyInfraFailed`, which means the check could not run at all (git error,
not tampering). The verify Job's log is the evidence for what changed and why
the checks failed.

See: [GLOSSARY.md#glossary](GLOSSARY.md#glossary), [TROUBLESHOOTING.md#verifyinfrafailed--was-the-agent-tampering-f39](TROUBLESHOOTING.md#verifyinfrafailed--was-the-agent-tampering-f39)

## 3. How do I give the agent access to a host?

Declare a network allow (`host:port`) on the AgentPolicy the Loop references;
the effective policy is additive over default-deny. The agent's egress to
allowed hosts then runs through the per-Loop egress proxy, which enforces the
allowlist at the CONNECT/SNI/Host layer. In-cluster targets (`.svc` names,
cluster DNS suffixes, in-CIDR IPs) cannot be allowed at all — policy
validation rejects them, and the proxy's resolved-IP check rejects private,
link-local, and cluster-CIDR IPs as a backstop.

See: [OPERATOR.md#what-the-sandbox-can-reach](OPERATOR.md#what-the-sandbox-can-reach)

## 4. How do I fork a Loop?

A fork seeds a new one-shot Loop from another Loop's checkpoint, inheriting
the source's spec (goal, agent, verify, delivery, loop, checkpoints) and
running its own branch, deliver, and PR. Use `kubectl cox fork my-fork
--loop <source> --iteration 2`, with optional `--check` (replaces the
acceptance checks) and `--goal` (replaces the goal); without those flags the
fork inherits them. The source's checkpoint for that iteration must exist and
be ReadyToUse; the operator defers the restore while the snapshot is not ready
(`RestoreSourceNotReady`) and rejects the fork when it is absent
(`RestoreSourceMissing`).

See: [QUICKSTART.md#7-forking-a-loop](QUICKSTART.md#7-forking-a-loop)

## 5. How do I read what the agent is doing?

Use `kubectl cox logs <loop> --component agent` to stream the runner's log
(planning, implementing, the model calls). To see the Loop's state — phase,
iteration, conditions, budget, delivery — use `kubectl cox describe <loop>`;
the full audit trail is `kubectl cox history <loop>` and the agent's plan is
`kubectl cox plan <loop>`.

See: [OPERATOR.md#reading-loop-status](OPERATOR.md#reading-loop-status)

## 6. Why is my Loop stuck in Implementing with no model traffic?

The runner's model call may be hanging: a hung backend times out at 120s per
request, retries 3 times with backoff, then surfaces `ModelUnavailable` and
exits blocked — the operator maps that to a `ModelAvailable=False`
condition. Worst case is about 8 minutes before the condition appears. If you
see `ModelUnavailable` on a healthy backend, the model is slow, not dead:
there is no user-facing knob to tune the timeout, so reduce the load or the
context size. Check the proxy log and the model endpoint
(`spec.agent.endpointSecretRef` / `modelEndpoint`).

See: [QUICKSTART.md#8-model-timeout-and-tuning](QUICKSTART.md#8-model-timeout-and-tuning), [TROUBLESHOOTING.md#modelavailablefalse--modelunavailable-on-the-loop](TROUBLESHOOTING.md#modelavailablefalse--modelunavailable-on-the-loop)

## 7. Why did my Loop fail with VerifyInfraFailed?

`VerifyInfraFailed` means the tamper check could not run — git itself failed
(a missing rev, a corrupt index, a clone problem), not that the agent
tampered. The tamper script exiting 2 maps to the internal `TamperUnknown`
verdict (an infrastructure failure), and verify infra failures are retried
twice (three total runs) before the Loop fails with this reason, which is
distinct from `TamperedVerify` (a protected path actually changed). Check the
repo is reachable from the verify Job, that `baseCommit` is a real commit, and
the verify Job's logs.

See: [TROUBLESHOOTING.md#verifyinfrafailed--was-the-agent-tampering-f39](TROUBLESHOOTING.md#verifyinfrafailed--was-the-agent-tampering-f39)

## 8. Why did my Loop fail with PinNotDescendant?

`PinNotDescendant` means the verify Job's import refused the pinned commit
before any check ran: the commit the operator pinned
(`status.currentVerify.verifiedCommit`) does not descend from
`status.baseCommit`. The pin is the runner's `headCommit` claim, not
operator evidence — a runner (or a corrupted `result.json`) can pin any
40-hex SHA reachable in the agent's repo, including pre-existing history
that predates the Loop, so F43 makes the operator check ancestry at import
time (`git merge-base --is-ancestor <base> <pin>` in the verify Job's
`import-agent` init container; exit 97 maps to this reason). It is terminal
with no retry, like `TamperedVerify`: a re-run would import the same pin. It
is distinct from `VerifyInfraFailed` (git itself failed — that is retried
twice) and from `TamperedVerify` (a protected path actually changed). Compare
`status.baseCommit` and `status.currentVerify.verifiedCommit` with
`kubectl cox describe`; if the pin is honest the workspace likely diverged
from base, so fix the Loop's starting point and restart rather than waiting
for a retry.

See: [GLOSSARY.md#glossary](GLOSSARY.md#glossary) (the PinNotDescendant entry), [TROUBLESHOOTING.md#pinnotdescendant--the-verify-job-refused-the-pinned-commit-f43](TROUBLESHOOTING.md#pinnotdescendant--the-verify-job-refused-the-pinned-commit-f43)

## 9. Why did the operator reject my edit to spec.restoreFrom?

`spec.restoreFrom` is immutable (a CEL rule, `self == oldSelf`), so a
mid-Loop rollback target cannot be swapped — a different rollback target is a
fork, not an edit. To roll back to a different checkpoint, create a fork
(`cox fork`) or a new Loop seeded from that checkpoint; do not edit the field.

See: [TROUBLESHOOTING.md#editing-specrestorefrom-is-rejected-by-admission-f35](TROUBLESHOOTING.md#editing-specrestorefrom-is-rejected-by-admission-f35)

## 10. Why did a re-plan put my Loop back in AwaitingApproval?

With manual approval, when the runner detects the plan changed after approval
(a re-plan), the stale approval is cleared so an unapproved re-plan cannot be
implemented. You get a `PlanChangedReHold` Event and the Loop re-enters
`AwaitingApproval`. Approve the new plan (`cox approve`) or reject it
(`cox reject`); if you did not expect a re-plan, read the plan in the
workspace to see what changed.

See: [TROUBLESHOOTING.md#event-planchangedrehold-f38](TROUBLESHOOTING.md#event-planchangedrehold-f38)

## 11. My Loop Succeeded but there's no PR. What do I do?

A Loop that passes verify is held at phase `Verifying` (`Verifying=True
reason=Delivering`, sandbox Suspended) until the `Delivered` condition is
terminal; a failed delivery is `Succeeded` + `Delivered=False`. If the
deliver Job failed (e.g. the `git-cred` Secret was missing), re-trigger
delivery with `kubectl cox redeliver <loop>`, which sets the
`coxswain.io/redeliver` annotation and the operator re-runs the deliver Job
on the next reconcile.

See: [OPERATOR.md#7-redeliver](OPERATOR.md#7-redeliver)

## 12. How do I give the agent access to a tool (not just a host)?

Declare the tool on the AgentPolicy (`spec.tools[]`); each declared tool gets
its own credentialed, per-Loop tool proxy and its own egress peer in the
agent's NetworkPolicy, so the agent uses the tool without holding that
credential. A tool proxy that the Loop does not own or that is not Ready
holds the sandbox Suspended.

See: [GLOSSARY.md#glossary](GLOSSARY.md#glossary) (the tool proxy entry), [TROUBLESHOOTING.md#the-other-holds-on-a-suspended-sandbox](TROUBLESHOOTING.md#the-other-holds-on-a-suspended-sandbox) (the suspend hold), [OPERATOR.md#what-the-sandbox-can-reach](OPERATOR.md#what-the-sandbox-can-reach) (the egress peer)

## 13. Where does the git credential live, and can the agent read it?

The git credential is the `git-cred` basic-auth Secret. It is mounted
read-only into the workspace init container for the first clone only — once
the base SHA is pinned it is omitted from the pod, and the agent container
never mounts it. The agent container holds no model key and no git
credential at runtime, and it carries no service-account token either
(`automountServiceAccountToken: false`), so it has no API-server identity.

See: [QUICKSTART.md#3-the-github-token-secret](QUICKSTART.md#3-the-github-token-secret)

## 14. My Loop is Paused for a stall or budget. How do I resume it?

A stall with `stallAction=Pause` or a budget cap with `onExceeded=Pause`
holds the Loop Paused (`Paused=True reason=Stall` / `reason=Budget`). Resume
with `kubectl cox resume <loop>`, which sets `spec.suspend=false`; for a
stall/budget pause, also set the `coxswain.io/resume` annotation to clear the
pause condition. To pause a running Loop deliberately, use `kubectl cox pause
<loop>`.

See: [OPERATOR.md#6-pause-and-resume](OPERATOR.md#6-pause-and-resume)

## 15. The memory PVC won't bind. Why?

The memory PVC is ReadWriteMany, but kind, k3d, and k3s ship an RWO-only
default StorageClass, so the PVC stays Pending. `hack/rwx-storage.sh`
installs a second, pinned local-path provisioner and a `local-shared`
StorageClass, and you then run the operator with
`--memory-storage-class=local-shared` (render `install.yaml` with
`MEMORY_STORAGE_CLASS=local-shared`). On a multi-node cluster the same host
directory must be mounted at that path on every node; on a single-node
cluster nothing else is needed. Once bound, the PVC holds at most 100
lessons (the memory-writer Job evicts the oldest beyond the cap).

See: [OPERATOR.md#shared-memory-storage-phase-5](OPERATOR.md#shared-memory-storage-phase-5)

## 16. What do the conditions mean?

`kubectl cox describe <loop>` shows the conditions with status, reason, and
message: `status.phase=Succeeded` (the goal was met — verify passed and
delivery is terminal; there is no separate `Succeeded` condition), `Verifying=True reason=Delivering` (verify passed, delivery in
progress), `Failed=True reason=Stalled` (k consecutive identical verify
failures), `Failed=True reason=BudgetExceeded` (a cap was hit),
`ModelAvailable=False reason=ModelUnavailable` (the backend is unreachable or
timed out), and so on. The `reason` field is the most useful diagnostic.

See: [OPERATOR.md#key-conditions-to-watch](OPERATOR.md#key-conditions-to-watch)

## 17. How do I upgrade the operator?

Build the new installer manifest with your image tag, then apply it — the CRDs
update in place and the manager Deployment rolls. The one-time fix for tool
proxy NetworkPolicies created before D41d is documented separately (confirm
each live tool netpol carries the `coxswain.io/tool-proxy-for` label before
deleting anything).

See: [OPERATOR.md#upgrade](OPERATOR.md#upgrade)

## 18. How do I uninstall coxswain cleanly?

Remove the running Loops first (this deletes the sandbox pods, verify Jobs,
deliver Jobs, and workspace PVCs) and wait until none are left, then remove
the operator (CRDs, RBAC, the manager Deployment), and remove the CRDs if you
want a clean cluster.

See: [OPERATOR.md#uninstall](OPERATOR.md#uninstall)
