# Release notes: v0.3.1

A patch release on v0.3.0. It contains the fixes merged since v0.3.0: the operator's leader-election timings, the I273 restart fixes, the read-only Planning workspace (I274), and the F6 wording fix.

**Not proven on kind.** Unlike v0.3.0 (R2), no pre-tag smoke or upgrade run was done for this tag. Each entry below was covered only by its PR's unit tests and CI.

Entries are added per merged PR, each citing the PR and the squash commit it landed as in the heading, like `RELEASE-NOTES-v0.3.0.md`.

## Summary

- **The operator survives short API stalls** (#511, 1049aff): leader election now uses lease 60s / renew deadline 40s / retry 5s, up from controller-runtime's 15s / 10s / 2s, and the new `--leader-elect-lease-duration`, `--leader-elect-renew-deadline` and `--leader-elect-retry-period` flags can set them.
- **A new leader keeps enforced sandboxes running until its first CNI probe** (I273, D142 = a, #506, a6ebe62): an operator restart no longer suspends every running agent.
- **The runner exits promptly on SIGTERM during a phase** (I273, #508, 0b4d419).
- **The Planning agent mounts the workspace read-only** (I274, D143 = a, #509, bed9ba7).
- **The AgentFailed `Recovered` wording says what the code checks** (F6, #501, 48a7573): no behaviour change.

## Upgrade

No CRD, RBAC or admission change since v0.3.0: replacing the operator image is enough. Upgrading from v0.2.0 or earlier still follows the v0.3.0 (and v0.2.0) Upgrade sections, CRDs first.

### Leader-election timings (#511)

Nothing to set: the new defaults apply. The trade-off is that after an **unclean** leader death (node loss, OOM kill), a standby takes over after up to 60s, instead of 15s. A clean shutdown or rollout still hands over at once (`LeaderElectionReleaseOnCancel`, D142a). To keep the old behaviour, pass `--leader-elect-lease-duration=15s --leader-elect-renew-deadline=10s --leader-elect-retry-period=2s`. A combination client-go would reject (lease ≤ renew, or renew ≤ 1.2 × retry) is a startup error naming the flag.

## Fixes

### The operator survives short API stalls — #511 (1049aff)

**What was wrong:** after ~10s of failed lease renewals, controller-runtime's defaults made the manager give up leadership and exit. On the homelab cluster, an etcd member on a slow SATA SSD stalls lease reads for a few seconds every 20–40 min, and the operator exited 124 times in 12 days. Flux (35s/30s/5s) and cert-manager (60s/40s) rode out the same stalls.

**What changed:** the defaults are 60s / 40s / 5s, set through three new flags and validated at startup (`cmd/leader_election_flag.go`).

**On upgrade:** see Upgrade.

### I273: a new leader keeps running enforced sandboxes until its first CNI probe (D142 = a) — #506 (a6ebe62)

**What was wrong:** until its first CNI probe, a new leader treated the network as Unknown and suspended every running sandbox (R58).

**What changed:** before the first probe, only sandbox *creation* is held. A sandbox that is already Running, on a Loop that records `NetworkEnforced=True/CNIEnforced`, keeps running, and that recorded condition is not rewritten to Unknown.

**On upgrade:** nothing to do; the next operator restart behaves this way.

### I273: the runner exits promptly on SIGTERM during a phase — #508 (0b4d419)

**What was wrong:** the runner ran the model loop under `context.Background()`, so a SIGTERM during Planning or Implementing was ignored until the kubelet's SIGKILL (exit 137).

**What changed:** the model loop is cancelled when SIGTERM arrives. The runner then returns with no claim, no PLAN.md, no commit and no lesson, and exits 0.

**On upgrade:** nothing to do.

### I274: the Planning agent mounts the workspace read-only (D143 = a) — #509 (bed9ba7)

**What was wrong:** in Planning, the agent had the same writable workspace as in Implementing, so a Planning edit could be committed by Implementing (R59).

**What changed:** in Planning, `/workspace` is mounted read-only, plus a writable `/workspace/.coxswain` subPath for the runner's own files. Other phases are unchanged.

**On upgrade:** a Loop that enters Planning after the upgrade gets the read-only mount. A Loop already past Planning is unaffected.

### F6: the AgentFailed `Recovered` wording says what the code checks — #501 (48a7573)

Comments, a condition message and generated descriptions only; no behaviour change.

## Known issues

The v0.3.0 known issues (buildx, multi-arch `kind load`) still apply.
