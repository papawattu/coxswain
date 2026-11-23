# syntax=docker/dockerfile:1
#
# Agent adapter images (docs/PLAN-AGENT-ADAPTERS.md): one template, one target
# per agent. Every image is the same shape:
#   - the adapter at /usr/local/bin/runner (the runner binary with the CLI
#     engine; the operator execs this path), and its engine spec at
#     /etc/coxswain/engine.json;
#   - the agent's CLI at a pinned version, installed at BUILD time (nothing is
#     installed or downloaded at run time);
#   - the Go toolchain and git (the agent builds and tests the repo), uid and
#     gid 65532, HOME=/scratch (the sandbox's writable emptyDir; the root
#     filesystem is read-only).
#
# Build one agent (from the repo root):
#   docker build -f agents/Dockerfile --target pi -t local/coxswain/agent-pi:dev .
# Targets: pi, opencode, codex, claude-code, hermes.
#
# D149 (a): the claude-code target is a build recipe only; its image is not
# published (Claude Code's licence). The other four are open source.

ARG GO_IMAGE=golang:1.26-trixie@sha256:cff05dd6b81c901f2e8249eb6f91c69a60451d0f12c26bfef08e476a85b32d4d
ARG NODE_IMAGE=node:24-trixie-slim@sha256:af02d57b57719b9657a45199a79264a9437940cd55b3c7b3fc99615654f124a0
ARG HERMES_IMAGE=nousresearch/hermes-agent:v0.23.3@sha256:3e9fe4c6ef36490f43aa0d0d80f9df64e9c92a576eee3042f1a97c8813c05355

# The adapter: the runner binary (static, so it runs on any base).
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS adapter
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY runner/ ./
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -o /out/runner ./cmd

FROM ${NODE_IMAGE} AS node

# The common base for the npm-installed agents.
FROM ${GO_IMAGE} AS agent-base
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git ripgrep python3 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/npm
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx
COPY --from=adapter /out/runner /usr/local/bin/runner
ENV HOME=/scratch
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/runner"]

FROM agent-base AS pi
ARG PI_VERSION=1.6.0
RUN npm install -g --ignore-scripts "@earendil-works/pi-coding-agent@${PI_VERSION}" && npm cache clean --force
COPY agents/pi/engine.json /etc/coxswain/engine.json
USER 65532:65532

FROM agent-base AS opencode
ARG OPENCODE_VERSION=1.19.6
RUN npm install -g "opencode-ai@${OPENCODE_VERSION}" && npm cache clean --force
COPY agents/opencode/engine.json /etc/coxswain/engine.json
USER 65532:65532

FROM agent-base AS codex
ARG CODEX_VERSION=0.171.0
RUN npm install -g "@openai/codex@${CODEX_VERSION}" && npm cache clean --force
COPY agents/codex/engine.json /etc/coxswain/engine.json
USER 65532:65532

FROM agent-base AS claude-code
ARG CLAUDE_CODE_VERSION=2.1.331
RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" && npm cache clean --force
COPY agents/claude-code/engine.json /etc/coxswain/engine.json
USER 65532:65532

# Hermes Agent does not support building from source (its wheel build refuses,
# by design); its official image is the supported install. The adapter and the
# Go toolchain are copied in, and its s6 entrypoint is replaced: the operator
# execs the adapter, which runs the venv's hermes binary directly.
FROM ${GO_IMAGE} AS gotoolchain
FROM ${HERMES_IMAGE} AS hermes
COPY --from=gotoolchain /usr/local/go /usr/local/go
COPY --from=adapter /out/runner /usr/local/bin/runner
COPY agents/hermes/engine.json /etc/coxswain/engine.json
ENV PATH=/usr/local/go/bin:/opt/hermes/.venv/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    HOME=/scratch
WORKDIR /workspace
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/runner"]
CMD []
