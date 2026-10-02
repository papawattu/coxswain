# syntax=docker/dockerfile:1
# S3b (GAP 1): the Coxswain runner agent image.
#
# Base golang:1.26 (the same base the sandbox dev default uses — the agent's
# HOME/scratch and Go toolchain assumptions hold) + the compiled runner binary +
# git (the init container clones; the agent may inspect the repo) + python3
# (the pylib sample app's toolchain) + curl (health/debug).
#
# The binary is copied from a build stage (the runner is its own Go module,
# runner/go.mod — the root module does not build it). The agent runs as the
# platform non-root UID (65532, set by the operator's securityContext), so the
# binary must be readable/executable by that UID (it is: 0755 root-owned).
FROM golang:1.26 AS build
WORKDIR /src
# The runner is a separate module (runner/go.mod); the root go.mod does not
# cover it. Copy the sources and build hermetically (GOFLAGS=-mod=mod so a
# missing go.sum is written from the module cache rather than failing the
# COPY). The build is for the target platform, not the builder's.
COPY runner/ ./
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ENV CGO_ENABLED=0
RUN GOFLAGS=-mod=mod GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /out/runner ./cmd

FROM golang:1.26
RUN apt-get update && apt-get install -y --no-install-recommends \
        git \
        python3 \
        curl \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/runner /usr/local/bin/runner
# The operator's init-container and agent both run as UID 65532; the binary
# and its dir are world-readable (default root:root 0755).
USER 65532:65532
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/runner"]
