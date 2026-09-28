# The proxy-standin is a package in the root coxswain module (no separate
# go.mod). Build from the repository root:
#   docker build -f cmd/proxy-standin/Dockerfile .
FROM golang:1.26 AS build
WORKDIR /app
COPY go.mod go.sum ./
COPY cmd/proxy-standin/main.go ./cmd/proxy-standin/main.go
RUN CGO_ENABLED=0 go build -o /app/proxy ./cmd/proxy-standin

FROM gcr.io/distroless/static:nonroot
COPY --from=build /app/proxy /usr/local/bin/proxy
USER 65533
ENTRYPOINT ["/usr/local/bin/proxy"]
