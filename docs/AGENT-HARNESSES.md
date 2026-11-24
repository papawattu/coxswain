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
| hermes | `hermes-agent.nousresearch.com` and `nousresearch.github.io` (the plugin catalogue: a fresh home installs its default plugins); on longer tasks also `proxy.golang.org` (it runs `go install gopls` for its LSP), the model catalogue (`hermes-agent.nousresearch.com`, with a fallback on `raw.githubusercontent.com`), and `openrouter.ai` (a reasoning-capabilities cache it warms in the background) | `HERMES_DISABLE_LAZY_INSTALLS=1`, `security.allow_lazy_installs: false`, `plugins.auto_update_check_hours: 0`, `lsp.enabled: false`, `model_catalog.enabled: false`, `curator.enabled: false`, `_config_version: 54` (without it Hermes treats the config as old and migrates it), `HERMES_DISABLE_MODEL_METADATA_FETCH`, `HERMES_DISABLE_TELEMETRY`, `HERMES_DISABLE_PLUGINS`. Two caches are pre-seeded: an empty plugin catalogue, and the OpenRouter reasoning-capabilities cache with a timestamp in 2100, because that cache has no switch. The toolsets are limited to `terminal,file,code_execution,todo`, so there are no web, browser, image or skills-hub tools |

`OPENCODE_DISABLE_PROJECT_CONFIG=1` also stops a repo from shipping its own opencode config, such as providers or permissions.

**Web tools are off** where a CLI has them, because the sandbox has no internet and a web tool could only make blocked attempts:
- Claude Code: `--disallowedTools WebFetch,WebSearch`;
- opencode: `tools.webfetch: false`;
- Hermes: the toolset list above.

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

### T2 on the real task, under the pod's limits (11-23 and 11-24)

This is the gocli sample's goal: fix `Round` so it rounds half away from zero, without touching the protected tests. Each run used the sandbox pod's limits (512 MiB and half a CPU, or a harness override), a `/tmp` that allows exec, and `GOTMPDIR=/workspace/.coxswain/gotmp`, all as in a pod. After Implementing, T2 runs `go test ./...` on the committed result. That check fails on the unfixed repo, which is its positive control.

| Agent | Memory | Planning | Implementing | Committed | `go test` on the commit | Network | Evidence |
|---|---|---|---|---|---|---|---|
| pi | 512 MiB | 57 s | 8 s | `round.go` | ok | all 46 packets to the proxy | `t2-gocli-final-512` |
| codex | 512 MiB | 55 s | 10 s | `round.go` | ok | all 204 to the proxy | `t2-gocli-final-512` |
| claude-code | 512 MiB | 90 s | 27 s | `round.go` | ok | all 182 to the proxy | `t2-gocli-final-512` |
| opencode | 1 GiB | 73 s | 20 s | `round.go` | ok | all 92 to the proxy | `t2-gocli-final-1g` |
| hermes | 1 GiB | 100 s | 99 s | `round.go` | ok | all 182 to the proxy | `t2-gocli-hermes-cat` |

The canary was found nowhere in any run. Evidence is reviewer-local, under `.samples/agent-images/`.

**What earlier runs found and what fixed it:**
- **opencode at 512 MiB:** its CLI was killed by SIGKILL in both phases. Its peak memory was 688 MiB, and Hermes's 664 MiB (`docker stats`, every 2 s). Both run with `--agent-harness …,memory=1Gi`. Hermes's "Gateway disconnected" at 512 MiB went away at 1 GiB.
- **Codex:** it runs its tool in a login shell (`bash -lc`), which resets `PATH` and lost `/usr/local/go/bin`. It then rebuilt the Go environment inside the workspace, and the adapter refused the build cache in the commit. The images now link `go` and `gofmt` into `/usr/local/bin`.
- **A T2 defect:** its first version mounted `/tmp` `noexec` (Docker's tmpfs default) and set no `GOTMPDIR`. Every agent's `go test` then failed to exec its test binary, and the agents moved Go caches into the workspace to work around it. T2 now mirrors the pod.
- **Hermes:** it kept reaching four hosts until the Hermes switches in the table above were added. The final run reached nothing but the proxy.

### T2 run, 11-23 (the small task)

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

## T3 on kind (R6, 11-24)

The kind-ref cluster ran an integration candidate, `61ed6ff` (main, plus #529, #530 and #531, merged locally). The operator ran with `--agent-harness` for all five, opencode and Hermes with `memory=1Gi`. Each Loop is the gocli sample with the Judge, PR delivery, and a `LESSON:` line. They ran three at a time, followed by the reference runner as a regression check. Evidence is reviewer-local in `.samples/r6/c6/` (`summary.txt`, `r6-lab.log`, each Loop's agent logs and model-proxy log, `samples-podwatch.json`).

| Loop | Phase | PR | `RunnerAvailable` | Judge | Memory write | Proxy calls (200s, all metered) |
|---|---|---|---|---|---|---|
| r6-runner | Succeeded | 25 | `RunnerImageSet` | true | written | 15 of 15 |
| r6-pi | Succeeded | 26 | `HarnessImageSet` | true | written | 32 of 32 |
| r6-opencode | Succeeded | 24 | `HarnessImageSet` | **false** | `LessonsWithheld` (D140: a false verdict withholds lessons) | 9 of 9 |
| r6-codex | Succeeded | 27 | `HarnessImageSet` | true | written | 12 of 12 |
| r6-claude-code | Succeeded | 28 | `HarnessImageSet` | true | written | 10 of 10 |
| r6-hermes | Succeeded | 29 | `HarnessImageSet` | true | written | 9 of 9 completions |

**Every Loop:**
- every sandbox pod ran its harness's pinned image through `/usr/local/bin/runner`, with 1Gi for opencode and Hermes and 512Mi otherwise (`samples-podwatch.json`);
- `VerifySuspicious=False`, `NetworkEnforced=True/CNIEnforced`;
- the operator log has 499,454 lines and `forbidden` = 0;
- the lab was restored to main `7ab11ec` (`restored args == old: yes`, 0 Loops).

**What the proxy logs show:**
- **Claude Code** got 8 HTTP 500s from vLLM. It retried and the Loop succeeded. The audit line has no path, so which endpoint returned them is not shown.
- **Hermes** made 8 404s and 4 unmetered 200s. These are its start-up probes of other servers' model-list endpoints. The 4 unmetered 200s are in the same probe set, but the audit line has no path, so that they are model-list GETs is not shown.

**Node CLIs and `blockedCount`.** For pi (5) and opencode (3), `blockedCount` is above 0 although they made no outside attempt. Each block is KubeArmor's default posture refusing an `AF_NETLINK` raw socket: Node opens one at start to list network interfaces (`operator.stream.log`, `"action":"Block","target":"domain=AF_NETLINK …"`). The `PolicyBlocked` count for a Node agent therefore includes these. They are not egress.

**Egress controls (`r6x-*`):** the same Loop, with a goal that first asks the agent to run `curl -sS -m 10 https://example.com`.
- The runner, pi, Codex and Hermes each ran it and reported `curl: (6) Could not resolve host: example.com`.
- The Claude Code and opencode logs do not show the command's result. Whether they ran it is not shown.
- No control reached example.com.
- The operator log has 5 KubeArmor lines naming example.com: the processes that carried it in their arguments.
- The DNS lookup itself is not in the audit (R63 I284), so `blockedCount` is not a measure of these attempts.

## Not covered yet

- **T4:** the isolation checks on kind, per agent.
- **Qwen's thinking switch:** the reference runner turns Qwen's thinking pass off with `-extra-body`. The CLIs get no equivalent. None of the five needed it for the gocli task.
- **Claude Code's bundled skills:** a Claude Code summary mentions an "update-config skill", one of its built-in skills, which it loads on its own. They are part of the pinned CLI, not fetched.
