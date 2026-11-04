# Release notes: changes since v0.1.3 (unreleased)

Entries are added per merged PR, each citing the PR and the squash commit it
landed as, like `RELEASE-NOTES-v0.1.3.md`.

## Upgrade

_(none yet)_

## New features

_(none yet)_

## Security fixes

_(none yet)_

## Other changes

### I208: PolicyValid's message names both policy kinds — #TBD

A Loop whose `spec.policyRefs` resolves to a `ClusterAgentPolicy` (P6a) was
left with `PolicyValid=True` and the message "all referenced AgentPolicies
are valid" — the message named the wrong kind for a cluster-only ref. The
True message is now kind-neutral ("all referenced policies (AgentPolicy /
ClusterAgentPolicy) are valid"); the reason stays `Valid`.

### I212: a runner Loop with no model endpoint is held — #396

**What was wrong:** A Loop on the operator's runner (`spec.agent.image` empty
or equal to `--runner-image`) with no `spec.agent.endpointSecretRef` and no
`spec.agent.modelEndpoint` was admitted, the sandbox was created anyway, and
the agent crash-looped in Planning with `COX_MODEL_BASE_URL is empty; the
runner requires the operator-set model proxy URL`, with no condition naming
the cause (the v0.1.3 smoke run hit this applying the shipped sample as-is;
the three samples ship the model fields commented out).

**What changed:** The operator now holds such a Loop pre-sandbox with
`ModelConfigValid=False reason=NoModelEndpoint` (the D34 pair check's
`MissingModelEndpoint` is unchanged); no sandbox, no model-proxy, no egress-proxy objects are created. A non-runner `spec.agent.image` is not held.
The three sample comments now say the model fields are required before
applying. `docs/OPERATOR.md` lists the new reason under the key conditions.

**On upgrade:** a runner Loop without model fields that used to crash-loop is
now held with `ModelConfigValid=False NoModelEndpoint`. Recreate it with
`spec.agent.endpointSecretRef` + `spec.agent.modelEndpoint` (+ `model`) set —
`modelEndpoint` is immutable, so an existing Loop cannot be edited in place.
