# probe is the D41e e2e assertion 4c connect-probe. It is a standalone static
# Go binary built to /usr/local/bin/tool-proxy on distroless, so the tool
# proxy's KubeArmorPolicy process allowlist (which permits ONLY
# /usr/local/bin/tool-proxy) permits the fence pod to run it.
#
# It is NOT the real tool-proxy — it is a tiny net.DialTimeout connect-probe
# (test/e2e/probe/main.go). It has its own go.mod so it builds independently
# of the coxswain module.
#
# Build from the test/e2e/probe directory:
#   docker build -t coxswain-tool-proxy:probe .
#
# UID 65535 (nonroot) — matches the real tool-proxy so the KubeArmorPolicy and
# any UID-based selection behave identically.
FROM golang:1.26 AS build
WORKDIR /app
COPY main.go .
RUN go mod init probe && CGO_ENABLED=0 go build -o /app/tool-proxy .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /app/tool-proxy /usr/local/bin/tool-proxy
USER 65535
ENTRYPOINT ["/usr/local/bin/tool-proxy"]
