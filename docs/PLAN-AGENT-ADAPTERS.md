# Agent adapters: Claude Code, Codex, pi, Hermes and opencode on one template

> **Status:** PLANNED, not started. Needs the owner's answers to D147–D149 (below) before S3.
>
> **Ask (owner, 11-23):** create agents for Claude Code, Codex, pi, Hermes and opencode, all from the same template, plus tests that show each one is working and self-contained.

## Goal

A Loop can run any of five third-party coding agents in place of the reference runner. Each one:
- runs Planning and Implementing under the same phase contract as the runner;
- holds no credentials;
- reaches nothing but the Loop's model proxy;
- is proven by the same tests.

**"Working"** means a gocli Loop (`config/samples/loop-fix-failing-test.yaml`) with that agent goes Planning → Implementing → Verifying → Succeeded on kind-ref:
- the operator's verify Job passes;
- there is no `TamperedVerify`;
- the Judge's verdict is true;
- the PR is delivered.

**"Self-contained"** means four things, each with its own test (T2, T3):
1. the image holds everything the agent needs, so nothing is installed or downloaded at run time;
2. the agent's only network peer is the model proxy, with no telemetry, update check, model catalogue or plugin fetch;
3. no credential is in the agent container (env, files or arguments);
4. the agent runs under the existing hardening: uid 65532, a read-only root filesystem, writes only to `/workspace`, `/scratch` and `/tmp`, no service-account token.

## What exists today (checked on `76b5d4d`)

- **The contract is already written for this.**
  - `docs/PLAN.md:36` says "any image that reads its instructions from `.coxswain/`, edits `/workspace`, commits locally, and writes `result.json` is a runner".
  - `api/v1alpha1/loop_types.go:760` says `spec.agent.image` can be "an adapter for an external agent".
  - The contract itself is in the header of `runner/phase.go` (lines 1–50):
    - `.coxswain/desired-phase` in;
    - `.coxswain/PLAN.md` from Planning;
    - a local commit from Implementing (`commitWorkspace`, `:964`);
    - `.coxswain/lessons.json`;
    - `.coxswain/result.json`;
    - a small claim in `/dev/termination-log`.
- **But only the runner is ever run.**
  - `runnerRuns` (`internal/controller/loop_controller.go:2059-2061`) is true only when `spec.agent.image` is empty or equals `--runner-image`.
  - Any other image keeps the `sh -c sleep infinity` stand-in (`:2137-2149`), so the Loop never advances.
  - The runner's `-extra-body` flag, which turns Qwen's thinking pass off, is only passed to the runner (`:2150-2166`).
- **What the agent container gets** (`:2167-2245` env, `:2290-2321` container):
  - **Env:** `HOME=/scratch`, `COX_GOAL`, `COX_MODEL_BASE_URL` (the model proxy Service), `COX_MODEL`, `COX_MODEL_TIMEOUT`; the egress proxy variables when a policy allows network; tool URLs.
  - **Rules for env:** `spec.agent.env` cannot set `COX_*` or proxy names (I34).
  - **Filesystem:** a read-only root, with writable `/workspace`, `/scratch` and `/tmp`. The workspace is read-only during Planning, except `.coxswain`.
- **The model proxy forwards any path, but meters only OpenAI chat-completions usage.**
  - It is a path-agnostic reverse proxy (`internal/proxy/proxy.go:129-185`).
  - Usage is parsed only from `prompt_tokens` and `completion_tokens` (`internal/proxy/shape.go:16-17,48-49`). Anthropic Messages and OpenAI Responses report `input_tokens` and `output_tokens`, so a call in either API would read as unmetered, and the budget gates would see no usage.
  - The credential always goes in `Authorization: Bearer …` (`proxy.go:84-86`). Nothing sets `CredHeader` (`cmd/model-proxy/main.go`).
- **The lab model** is `qwen3.8-27b` on vLLM behind `192.168.1.20:8000`, which serves OpenAI chat completions. Whether it also serves `/v1/messages` (Anthropic) or `/v1/responses` (OpenAI Responses) has not been checked (S0).
- **Audit gaps that affect the tests:** R63's I284 (DNS and the repo host bypass the egress proxy) and I285 (the eBPF audit missed the runner's connections to the model proxy). So "no network peer but the proxy" cannot yet be proven from the activity audit alone. T2 proves it outside the cluster, and T3 does not rely on the audit alone.

## The five agents

From the CLIs on the devbox where installed (`--help`, 11-23). The rest is to confirm in S0. Nothing in this table is assumed to work until S0 has run it.

| Agent | Version seen | Non-interactive run | Model API it speaks | Licence (to confirm, D149) | S0 must confirm |
|---|---|---|---|---|---|
| pi | 1.4.0 | `pi -p --mode json --no-session --offline` (all in `pi --help`) | OpenAI chat (custom provider) | MIT | the custom provider config file; `--offline` turns off every start-up fetch |
| opencode | 1.4.3 | `opencode run --format json -m <provider/model>` | OpenAI-compatible provider | MIT | provider config for a base URL; how to stop the model-catalogue fetch |
| Hermes Agent (Nous Research) | not installed | `hermes -z …` (the worker path the homelab kanban uses) | OpenAI-compatible | MIT | the exact flags; config dir under `HOME`; plugin and skill fetches off |
| Codex CLI | not installed | `codex exec …` | Responses API by default | Apache-2.0 | whether a custom provider can use chat completions; if not, the lab endpoint must serve `/v1/responses` (D148) |
| Claude Code | 2.1.330 | `claude -p --output-format json` | Anthropic Messages | proprietary | that `ANTHROPIC_BASE_URL` reaches the proxy; that the non-essential traffic switch stops every other call; whether the lab endpoint serves `/v1/messages` (D148) |

## The template

**One adapter binary.** The phase driver (`runner/phase.go`) is split from the model loop. An `Engine` interface is added:

```
Run(ctx, prompt, workspace) (answer string, trace []string, err error)
```

- The **built-in engine** is today's shell-tool loop (`runner/runner.go:661`).
- The **CLI engine** runs a configured external command non-interactively, with the prompt on stdin or in a file. It reads the final answer from the command's JSON output, and enforces a timeout.

Everything else stays in the adapter, so every agent gets it unchanged:
- the desired-phase file;
- the prompts, including the memory section and the lessons instruction;
- `PLAN.md`;
- the operator-filtered commit;
- `lessons.json`;
- the claim and `result.json`.

The adapter, not the agent, decides what is committed.

**One base image,** `agent-base`:
- Debian slim, git, ca-certificates, `tini`;
- the adapter at `/usr/local/bin/runner`, the path the operator already execs;
- uid and gid 65532.

Each agent image is then:
- `FROM agent-base`;
- the agent's CLI at a pinned version (by digest or lockfile), installed at build time;
- `/etc/coxswain/engine.json`, which holds:
  - the command line;
  - the config file the adapter renders into `/scratch` at start: provider base URL = `COX_MODEL_BASE_URL`, model = `COX_MODEL`, a dummy API key because the proxy injects the real one, and telemetry, updates and catalogue fetches off;
  - how to read the final answer;
  - the agent's equivalent of the runner's thinking-off switch;
- one build file per agent under `agents/<name>/`. They differ only in the install step and `engine.json`.

The reference runner becomes the sixth image built this way, with the built-in engine. That proves the template before any third-party CLI goes in.

## Operator and proxy changes

- **O1, run an adapter image (D147).** Replace the exact-image match in `runnerRuns` with an operator-owned map from a harness name to a pinned image, for example `--agent-harness=pi=…@sha256:…`, and a `spec.agent.harness` field. A Loop can choose only from images the operator lists, and the operator execs `/usr/local/bin/runner` in them. `spec.agent.image` stays the debug stand-in it is now. `RunnerAvailable` and `ModelConfigValid` treat any listed harness as a runner.
- **O2, the model API (D148).** Claude Code needs Anthropic Messages, and Codex may need Responses.
  - **Metering:** the proxy reads usage from all three shapes, streaming included (the Messages `message_delta` usage, the Responses `response.completed` usage).
  - **Credential header:** set per API: `x-api-key` for Messages, `Authorization` otherwise.
  - **The endpoint:** see D148.
- **O3, an AgentPolicy per harness.** Each image has an exec allowlist (node or python, git, sh) for when an AgentPolicy restricts exec, so a KubeArmor allowlist does not crash-loop it. This is the same concern #515 raised for fence-wait's python3.
- **O4, prompt parity.** The adapter sends each CLI the same goal, plan and memory text the runner sends, so differences in results come from the agent and not the prompt.

## Tests

Every test has a positive control, and every gate spec names a mutation that must FAIL it, as AGENTS.md requires.

- **T1, conformance (unit, no cluster).** The adapter runs against a fake engine that returns scripted answers and file edits, in a temporary git workspace. It asserts the phase contract:
  - `PLAN.md` within its cap;
  - the claim's JSON;
  - the commit excludes `.coxswain`, and a staged artifact is reset;
  - `lessons.json` from `LESSON:` lines;
  - a blocked claim on engine timeout or error;
  - no claim when the desired phase is Verifying.

  The same suite runs against the built-in engine and the CLI engine. Mutations: drop the index reset; write the claim before the commit.

- **T2, self-contained (Docker, no cluster).** Run once per image in `make agent-images-test`.
  - **Static:**
    - the image runs as 65532;
    - `docker run --read-only --tmpfs /scratch --tmpfs /tmp` starts and answers `runner --selftest`;
    - the CLI's version matches the pin;
    - no package manager runs at start: KubeArmor or an strace check shows no `npm`, `pip` or `curl` exec.
  - **Network:** the container runs on an `internal: true` Docker network whose only other member is a fake model server speaking that agent's API. A capture in the container's network namespace (tcpdump in a sidecar sharing it) records every DNS query and connect during one Planning and one Implementing run. The test passes only if every destination is the fake model.
    - Positive control: the same capture with `curl https://example.com` run in the container must show that attempt.
    - This proves the CLIs' own network behaviour without relying on the cluster's audit, which I284 and I285 show is incomplete.
  - **Credentials:** the run's env, `/scratch` and the process arguments contain no value of the fake server's real key. The fake server only accepts the key the test "proxy" injects. The agent is given a dummy.

- **T3, kind, one Loop per agent.** The gocli Loop with the Judge and PR delivery. It asserts:
  - "working" as defined above;
  - `status.policy.blockedCount` = 0, with the relay healthy;
  - the model proxy's request count is > 0 and matches its audit lines (this needs I286's log capture).

  Negative control per agent: a second Loop whose goal tells the agent to `curl https://example.com`. The attempt is blocked by NetworkPolicy and recorded, and the Loop is not failed for it (ADR-0007 Q5). The lab script records the operator, proxy and sandbox logs before cleanup.

- **T4, isolation (kind, the EVIL-AGENT checks, a subset).** For each image:
  - the container has no SA token;
  - it cannot write outside `/workspace`, `/scratch` and `/tmp`;
  - a test-file edit gives `TamperedVerify`;
  - the model key is not visible (`env`, `/proc/1/environ`, files).

  These reuse `docs/EVIL-AGENT-E2E.md`'s checks with the agent's own shell tool.

## Slices

| Slice | What | Needs |
|---|---|---|
| S0 | Spike, no code merged: run each CLI on the devbox against the lab endpoint through a local copy of the model proxy. Record the confirmed flags, config, network calls (tcpdump) and API paths, and update the agents table above. | none |
| S1 | Engine interface split. Only a refactor; the runner's existing specs stay green. T1 with the built-in engine. | — |
| S2 | `agent-base`, the CLI engine, and the reference runner rebuilt on the template. T1 with the CLI engine; T2 harness, with the runner as the first image. | S1 |
| S3 | O1 operator harness selection. | D147, S2 |
| S4 | O2 proxy metering and credential header for Messages and Responses. | D148, S0 |
| S5 | pi (OpenAI chat; the closest to the runner). T2 and T3. | S3 |
| S6 | opencode. T2 and T3. | S3 |
| S7 | Hermes Agent. T2 and T3. | S3 |
| S8 | Codex CLI. T2 and T3. | S3, and S4 if it needs Responses |
| S9 | Claude Code. T2 and T3. | S3, S4 |
| S10 | T4 for all, the T3 matrix as one lab script, docs (`docs/AGENTS-HARNESSES.md`), sample AgentPolicies (O3). | S5–S9 |

Each slice is one PR, reviewed on the PR, with its kind evidence before merge. S5–S9 can run in parallel once S3 is in.

## Owner decisions

- **D147: how does a Loop pick an agent?**
  - **(a) Recommended:** `spec.agent.harness: <name>`, mapped by the operator to an image it pins (`--agent-harness name=image@digest`). Only images the operator lists run the contract. A Loop author cannot point the operator's exec at an arbitrary image.
  - **(b)** Any `spec.agent.image` runs `/usr/local/bin/runner`. Simpler, but any image a Loop author names gets the contract's trust.
- **D148: Claude Code (Anthropic Messages) and possibly Codex (Responses), against an endpoint that serves chat completions.**
  - **(a) Recommended:** the endpoint must serve the API natively (vLLM's Anthropic-compatible and Responses endpoints, if S0 shows the lab version has them). The proxy only meters the extra shapes (O2).
  - **(b)** The proxy translates Messages or Responses to chat completions. That works with any endpoint, but coxswain then owns a translator inside its trusted proxy.
  - **(c)** A LiteLLM sidecar does the translation. That is a new third-party component in the trust boundary.
- **D149: publish the images, or ship build recipes only?** Claude Code's licence may not allow redistributing its binary in a public image.
  - **(a) Recommended:** publish the four open-source agents' images with each release. Ship Claude Code as a build recipe the user runs.
  - **(b)** Recipes only for all five.

These IDs follow R63, which uses D146. R63 notes that if I277 renumbers Phase 9's decisions, it must start after the last ID in use.

## Not in scope

- Running two agents in one Loop, or switching agent between phases.
- Interactive or TUI use.
- MCP servers inside the sandbox: every agent runs with MCP off, so tool access stays with the tool proxies (ADR-0008).
- Hosted model APIs. The lab uses its local endpoint. A hosted endpoint is a Secret plus a `modelEndpoint` change, with no adapter change.
