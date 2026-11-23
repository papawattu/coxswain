# Agent harnesses: running Claude Code, Codex, pi, Hermes and opencode as Loop agents

This is the reference for the agent adapter images. The plan and its decisions are in `docs/PLAN-AGENT-ADAPTERS.md` (D147–D149).

## How an adapter works

**The adapter binary.** An adapter image runs the same binary as the reference runner, `/usr/local/bin/runner`. When the image carries `/etc/coxswain/engine.json`, the runner does each phase's agent work with that CLI (`runner.CLIEngine`) instead of its own model loop. Everything else is the runner's, unchanged:
- the desired phase;
- the prompts, including memory and the `LESSON:` instruction;
- `PLAN.md`;
- the commit, made by the adapter, which resets anything the agent staged and leaves `.coxswain` out;
- `lessons.json`;
- the claim and `result.json`.

**The engine spec** (`agents/<name>/engine.json`) names:
- the command line, with the phase prompt as one argument, `${COX_PROMPT}`;
- the environment;
- the config files to render under `$HOME` (provider base URL = `COX_MODEL_BASE_URL`, the model, a dummy key, and telemetry, updates and fetches off);
- where the final answer is: stdout, or a file.

`${COX_PROMPT}` is allowed only in the command, never in a rendered file. A value rendered into a file must have no quotes, backslashes or control characters.

**How the CLI runs:**
- in its own process group, which is killed on timeout (default 45 min) or on SIGTERM;
- a non-zero exit blocks the phase with `EngineFailed: <agent> exit <n>: <last stderr line>`.

**The model.** The CLI talks only to the Loop's model proxy, which holds the real key (ADR-0006).

## The images

They are built from one file, `agents/Dockerfile`, with one target per agent (`make agent-images`).

| Agent | Version | Base | Model API | Size |
|---|---|---|---|---|
| pi | `@earendil-works/pi-coding-agent@1.6.0` | `agent-base` (Go 1.26, trixie, node 24) | OpenAI chat completions | 1.15 GB |
| opencode | `opencode-ai@1.19.6` | `agent-base` | OpenAI chat completions | 1.46 GB |
| codex | `@openai/codex@0.171.0` | `agent-base` | OpenAI Responses (`/v1/responses`) | 1.48 GB |
| claude-code | `@anthropic-ai/claude-code@2.1.331` | `agent-base` | Anthropic Messages (`/v1/messages`) | 1.43 GB |
| hermes | `nousresearch/hermes-agent:v0.23.3` (digest-pinned) | the official image, plus Go and the adapter | OpenAI chat completions | 3.47 GB |

**What every image has in common:**
- uid and gid 65532, and `HOME=/scratch`;
- the CLI is installed at build time;
- the Go toolchain and git, so the agent can build and test the repo.

**Hermes** refuses to build from source, by design. Its official image is the supported install, so the adapter, Go and the engine spec are copied into it. Its s6 entrypoint, which refuses to run as an arbitrary uid, is bypassed: the adapter runs the venv's `hermes` directly.

**Claude Code** (D149 (a)): its target is a build recipe only. The image is not published.

## What each CLI fetches unless it is switched off (S0, 11-23)

Each CLI was run hardened: uid 65532, a read-only root, and tmpfs `/scratch` and `/tmp`. A `tcpdump` sharing its network namespace recorded every packet. Without the switches below, three of the five reached the internet at run time.

| Agent | Network calls seen in S0 | What turns them off (in its `engine.json`) |
|---|---|---|
| pi | none beyond the model | `PI_OFFLINE=1`, `PI_SKIP_VERSION_CHECK=1`, `PI_TELEMETRY=0`, `--no-session --no-mcp` |
| claude-code | none beyond the model | `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, `DISABLE_AUTOUPDATER`, `DISABLE_TELEMETRY`, `DISABLE_ERROR_REPORTING` |
| codex | `ab.chatgpt.com` (analytics); `github.com` (2,556 packets: plugins and skills) | `[analytics] enabled = false`, `check_for_update_on_startup = false`, and `--disable` for plugins, remote plugins, apps, browser and computer use, image generation, skill and workspace dependency installs, in-app updates |
| opencode | `models.opencode.ai` (model catalogue); `registry.npmjs.org` (it installs `@opencode-ai/plugin` into its config dir on every tool-using run) | the `OPENCODE_DISABLE_*` switches (models fetch, autoupdate, default plugins, LSP download, external skills, Claude Code compatibility, project config), and the npm and Bun registry pointed at `http://127.0.0.1:9/` so the plugin install fails inside the container |
| hermes | `hermes-agent.nousresearch.com` and `nousresearch.github.io` (the plugin catalogue: a fresh home installs its default plugins) | `HERMES_DISABLE_LAZY_INSTALLS=1` and `security.allow_lazy_installs: false` |

`OPENCODE_DISABLE_PROJECT_CONFIG=1` also stops a repo from shipping its own opencode config, such as providers or permissions.

## The tests

- **T1, the phase contract** (`runner/cli_engine_test.go`, `make runner-test`). A fake CLI (a shell script) checks:
  - `PLAN.md` from its answer;
  - the commit holds only the CLI's change, with no staged artifact and no `.coxswain`;
  - `lessons.json`;
  - `EngineFailed` and `EngineTimeout`;
  - SIGTERM ends the phase with no claim and kills the CLI's child processes;
  - config rendering;
  - misconfigured specs are refused before the CLI runs.

  `runner/agents_spec_test.go` loads every `agents/*/engine.json`.

- **T2, each image works and is self-contained** (`make agent-images-test`, `hack/agent-images-test.sh`). Docker only, no cluster. Per image:
  - **static:** user 65532, and `runner -selftest` passes with a read-only root and no network;
  - **phases:** Planning (workspace read-only except `.coxswain`), then Implementing, each in a fresh container;
  - **network:** the agent is on an internal network with only the real model proxy (which forwards to the model). A `tcpdump` in the agent's namespace must show every outbound packet going to the proxy, and no DNS name but the proxy's;
  - **credentials:** the proxy's canary key must appear nowhere in the workspace, `/scratch`, or the agent's output;
  - **proxy:** at least one audit line per phase.

  Positive controls run first:
  - `getent hosts example.com` from an agent image must be detected;
  - the canary must be found in the proxy's own key file.

### T2 run, 11-23

Against `qwen3.8-27b` on the lab endpoint. Evidence is reviewer-local in `.samples/agent-images/t2-all-1/`.

| Agent | Planning | Implementing | Outbound packets, all to the proxy | Canary found | Proxy audit lines (Planning / Implementing) |
|---|---|---|---|---|---|
| pi | 3 s | 3 s | 21 | no | 2 / 3 |
| opencode | 5 s | 7 s | 53 | no | 3 / 5 |
| codex | 5 s | 6 s | 78 | no | 2 / 3 |
| claude-code | 10 s | 9 s | 69 | no | 2 / 5 |
| hermes | 32 s | 32 s | 122 | no | 9 / 5 |

Both controls passed, and every agent committed `NOTES.md`.

**Found by T2, not yet fixed:** the proxy reads usage only from chat completions. 14 of its 200 responses (Claude Code's Messages calls and Codex's Responses calls) were logged with `usagePresent: false`, so the budget gates see no tokens for those two agents. That is O2 in the plan (D148 (a)), a separate PR.

**Other proxy lines:**
- Hermes probes `/api/v1/models`, `/api/tags` and `/v1/props` at start (404, harmless; all to the proxy).
- One Claude Code request got a 500 from the endpoint and was retried.

## Not covered yet

- **T3:** a kind Loop per agent with the Judge and delivery, after the operator's `spec.agent.harness` (O1).
- **T4:** the isolation checks on kind.
- **Qwen's thinking switch:** the reference runner turns Qwen's thinking pass off with `-extra-body`. The CLIs get no equivalent yet. T2's small tasks did not need it, and T3 will show whether real tasks do.
