# egress-proxy is a package in the root coxswain module (no separate go.mod).
# Build from the repository root:
#   docker build -f cmd/egress-proxy/Dockerfile .
#
# UID 65534 (nonroot) — distinct from the agent (65532) and the model proxy
# (65533), per ADR-0007's hardening parity table.
FROM golang:1.26 AS build
WORKDIR /app
COPY go.mod go.sum ./
COPY cmd/egress-proxy/main.go ./cmd/egress-proxy/main.go
COPY internal/egress/ ./internal/egress/
RUN CGO_ENABLED=0 go build -o /app/egress-proxy ./cmd/egress-proxy

FROM gcr.io/distroless/static:nonroot
COPY --from=build /app/egress-proxy /usr/local/bin/egress-proxy
USER 65534
ENTRYPOINT ["/usr/local/bin/egress-proxy"]
