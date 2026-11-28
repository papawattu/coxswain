# Release notes: v0.3.7

A patch release on v0.3.6. The sandbox is Suspended for the whole Verifying phase, so the verify Job no longer competes with an idle sandbox pod for the Loop's ReadWriteOnce workspace volume (#539).

**Found live.** On the homelab (Ceph RBD workspace volumes), Loop `cox-test/cleanlog-retry-tests-v4` ended `Failed: VerifyInfraFailed`. All three verify pods were scheduled on a different node from the sandbox, failed with `Multi-Attach error ... Volume is already used by pod(s) <loop>-sandbox`, and sat in Init until their 900s deadline.

Entries are added per merged PR, each citing the PR and the squash commit it landed as in the heading, like `RELEASE-NOTES-v0.3.0.md`.

## Summary

- **The sandbox is Suspended for the whole Verifying phase** (#539, bd8f76e).

## Upgrade

There is no CRD schema, RBAC or flag change since v0.3.6, so replacing the operator image is enough.

**Behaviour change:** at Verifying the sandbox's `operatingMode` is `Suspended`, so no sandbox pod runs during verify. Before, it was Suspended only in the Delivering and Judging holds. A verify failure moves the Loop back to Implementing, and the sandbox is rebuilt Running as before.

**Who was affected:** clusters whose workspace StorageClass is network block storage (Ceph RBD, cloud disks), where a ReadWriteOnce volume attaches to one node at a time. On local-path storage (kind, k3d) the volume's node affinity already puts every consumer on one node.

## Changes

### The sandbox is Suspended for the whole Verifying phase — #539 (bd8f76e)

**What was wrong:** on the Implementing → Verifying advance, the operator rebuilt the sandbox Running. The runner executes nothing at Verifying and idles, but its pod kept the workspace volume attached to its node. The verify Job mounts the same volume read-only and is not tied to the sandbox's node. When the scheduler put it on another node, the volume could not attach.

**What changed:**
- The sandbox suspension gate stops the sandbox at Verifying. That includes the Delivering and Judging holds, which are sub-states of Verifying.
- The runner has no claim to read at Verifying, and the verify evidence comes only from the verify Job.

## Known issues

- When a verify pod cannot mount its volumes, the `VerifyInfraFailed` message says "the check hung" instead of naming the mount failure.
- The `PolicyBlocked` condition can read `True/BlocksObserved` with the message "0 action(s) blocked".
- The v0.3.6 known issues still apply.
