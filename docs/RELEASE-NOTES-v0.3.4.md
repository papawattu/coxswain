# Release notes: v0.3.4

A release on v0.3.3. It fixes an unfenced egress proxy (#533), and adds the agent adapters: a CLI engine in the runner (#529), `spec.agent.harness` in the operator (#530), and proxy metering for the Anthropic Messages and OpenAI Responses APIs (#531).

**Kind proof.** R6 (T3, 11-24) ran #529, #530 and #531 on kind-ref: all five harness Loops and the reference runner Succeeded, each delivering a PR, and every completion through the proxy was metered. #533 is covered by its envtest spec and CI only; it was found on the live homelab and has not yet re-run there.

Entries are added per merged PR, each citing the PR and the squash commit it landed as in the heading, like `RELEASE-NOTES-v0.3.0.md`.

## Summary

- **The egress proxy is fenced whenever it exists** (#533, fa44b1c): a Loop with an external repo and no AgentPolicy network allows got an egress proxy with no NetworkPolicy. On v0.3.3 that proxy never passed fence-wait, so the Loop stayed in Planning.
- **`spec.agent.harness` runs an operator-pinned agent adapter** (D147, #530, 8ffdd4b), with the new `--agent-harness` flag.
- **The runner's CLI engine and five agent adapter image recipes** (#529, edd0488): pi, opencode, codex, claude-code and hermes.
- **The model proxy meters Anthropic Messages and OpenAI Responses, and sends the key in each API's header** (D148, #531, 0cdf936).

## Upgrade

**CRD:** the Loop CRD gains `spec.agent.harness`. Apply the new CRDs (`install.yaml` or the chart) before, or with, the new operator image. A Loop without `harness` behaves as before.

**On upgrade:**
- No flag needs setting. With no `--agent-harness` listed, a Loop that sets `spec.agent.harness` is held (`RunnerAvailable=False/UnknownHarness`); every other Loop is unchanged.
- The egress proxy of an external-repo Loop with no network allows gets its `<loop>-egress-proxy-netpol`. On v0.3.3 such a proxy is stuck in `Init` on fence-wait; it passes once the policy exists.
- The model proxy now drops `Authorization`, `x-api-key` and `api-key` from the agent's requests and injects its own key: in `x-api-key` for `/v1/messages`, and in the configured header otherwise.

**Agent adapter images are not published by this release.** The release workflow builds the controller, runner and proxy images only. Build the adapters with `make agent-images` (`AGENT_IMAGE_PREFIX`, `AGENT_TAG`), push them to your registry, and list each with `--agent-harness name=image[,memory=Q][,cpu=Q]`. opencode and Hermes need `memory=1Gi`. The Claude Code target is a build recipe only (D149).

## Changes

### The egress proxy is fenced whenever it exists — #533 (fa44b1c)

**What was wrong:** `ensureEgressProxy` creates the egress proxy when the AgentPolicy grants network allows, or when the workspace clone, the verify clone-base or the deliver push of an external repo needs it. `ensureNetworkPolicy` created the proxy's NetworkPolicy only in the first case. So an external-repo Loop with no network allows got an egress proxy with no NetworkPolicy:
- before v0.3.3, the proxy ran unfenced, with the node and the cluster reachable;
- on v0.3.3, its fence-wait never passed, and the Loop stayed in Planning.

**What changed:** both decisions come from one function, `egressProxyAllows`. If the AgentPolicies can't be read, the NetworkPolicy is kept (fail closed). The agent's own egress rules are unchanged.

### `spec.agent.harness` runs an operator-pinned agent adapter — D147, #530 (8ffdd4b)

**What changed:**
- `spec.agent.harness` (CRD): a DNS-label-style name of at most 40 characters. It can't be changed, and can't be set together with `image`.
- `--agent-harness name=image[,memory=Q][,cpu=Q]` (repeatable) lists the harnesses the operator will run. `memory` and `cpu` override the agent container's 512Mi and 500m limits for that harness only.
- A listed harness runs `/usr/local/bin/runner` in the pinned image: `RunnerAvailable=True/HarnessImageSet`.
- An unlisted harness holds the Loop with no sandbox: `RunnerAvailable=False/UnknownHarness`, naming the listed harnesses, and one Warning Event per transition. Listing it releases the Loop on the next reconcile.

### The runner's CLI engine and five agent adapter images — #529 (edd0488)

**What changed:**
- The runner's phase work goes through an Engine. With no `/etc/coxswain/engine.json` it is the built-in model and shell loop, unchanged.
- The CLI engine runs an external agent CLI from `engine.json`: the prompt in argv only, config rendered under `$HOME` or `/tmp` from a fixed set of variables, a timeout, and the CLI's process group killed on timeout or SIGTERM. A spec that fails to load is fatal, never a fallback.
- `agents/Dockerfile` has one target per agent (pi, opencode, codex, claude-code, hermes): uid 65532, pinned CLIs, and every switch that stops a CLI reaching the internet on its own. `docs/AGENT-HARNESSES.md` lists them.
- `make agent-images-test` (T2) checks each image under the sandbox pod's limits, through the real model proxy, with a packet capture.

### The model proxy meters Messages and Responses — D148, #531 (0cdf936)

**What was wrong:** Claude Code speaks Anthropic Messages and Codex speaks OpenAI Responses. The proxy recorded no usage for either, so the budget gates saw no tokens for those agents.

**What changed:**
- Non-streaming and streaming usage is read for both APIs. Messages cache tokens count as input. Messages stream counts are cumulative, so the last value is used, not the sum.
- The credential is injected per API (see Upgrade). ADR-0009 amendment A1 records it.

## Known issues

- Agent adapter images are not published (see Upgrade).
- The v0.3.3 known issues (public node IPs and the egress and tool proxies' fence-wait, I278, I279) still apply.
- The v0.3.0 known issues (buildx, multi-arch `kind load`) still apply.
