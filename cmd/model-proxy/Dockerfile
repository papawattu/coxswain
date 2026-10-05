# The model-proxy (P2b, ADR-0009) is a package in the root coxswain module
# (no separate go.mod). Build from the repository root:
#   docker build -f cmd/model-proxy/Dockerfile -t coxswain-proxy:metering .
FROM golang:1.26 AS build
WORKDIR /app
COPY go.mod go.sum ./
COPY cmd/model-proxy/main.go ./cmd/model-proxy/main.go
COPY internal/proxy/ ./internal/proxy/
RUN CGO_ENABLED=0 go build -o /app/proxy ./cmd/model-proxy

FROM gcr.io/distroless/static:nonroot
COPY --from=build /app/proxy /usr/local/bin/proxy
USER 65533
ENTRYPOINT ["/usr/local/bin/proxy"]
