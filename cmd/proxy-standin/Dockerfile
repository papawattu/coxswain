FROM golang:1.26 AS build
WORKDIR /app
COPY go.mod ./
RUN CGO_ENABLED=0 go build -o /dev/null . 2>/dev/null || true
COPY main.go ./
RUN CGO_ENABLED=0 go build -o proxy .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /app/proxy /usr/local/bin/proxy
USER 65533
ENTRYPOINT ["/usr/local/bin/proxy"]
