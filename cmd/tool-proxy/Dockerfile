# tool-proxy is a package in the root coxswain module (no separate go.mod).
# Build from the repository root:
#   docker build -f cmd/tool-proxy/Dockerfile .
#
# UID 65535 (nonroot) — distinct from the agent (65532), the model proxy
# (65533), and the egress proxy (65534), per ADR-0008's hardening parity
# table.
FROM golang:1.26 AS build
WORKDIR /app
COPY go.mod go.sum ./
COPY cmd/tool-proxy/main.go ./cmd/tool-proxy/main.go
COPY internal/toolproxy/ ./internal/toolproxy/
RUN CGO_ENABLED=0 go build -o /app/tool-proxy ./cmd/tool-proxy

FROM gcr.io/distroless/static:nonroot
COPY --from=build /app/tool-proxy /usr/local/bin/tool-proxy
USER 65535
ENTRYPOINT ["/usr/local/bin/tool-proxy"]
