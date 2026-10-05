#!/bin/sh
# The D41e kind-acceptance tool upstream listener (upstream-server.sh): a
# fixed-path HTTP server on 0.0.0.0:80 inside a hostNetwork busybox pod on
# the kind node. The pod listens on every interface, including the node's
# loopback, where the script has added 198.18.0.10/32 (RFC 2544
# benchmarking range — in NO carve-out: not 10/8, 172.16/12, 192.168/16,
# 169.254/16, 127/8, 100.64/10, 0/8, 224/4, 240/4, or any IPv6 local
# range). The tool upstream URL is http://198.18.0.10:80 — the controller's
# ToolUpstreamInCluster check passes (the IP is in no carve-out) and the
# tool-proxy netpol's external carve-out + the tool-proxy's resolved-IP
# backstop permit the dial. The correct product behaviour, not a carve-out
# change.
#
# GET /ok → 200 "ok" + a log line naming the request's Authorization header
# (the upstream has no auth of its own — the injected header is what makes
# the call identifiable); everything else → 404 + a log line.
#
# Each connection is answered by the `nc -lk -p 80 -s 0.0.0.0 -e sh`
# listener (one busybox nc, persistent). For EVERY connection:
#   1. read the request on fd 0 (the accepted client socket) with a SHORT
#      SLEEP as the "client done sending" heuristic (the tool proxy sends
#      the request in one burst and waits for the response — the sleep is
#      shorter than the proxy's read timeout), logging the request line +
#      the Authorization header to /tmp/upstream.log;
#   2. write the HTTP response on fd 1 (the same client socket).
#
# (The earlier variant used `nc -l -c sh -c '...'`, which busybox nc does
# NOT support — `nc -c` is not a flag; the listener never bound and every
# preflight dial was refused. `nc -lk -e sh` is the busybox form.)
#
# The listener binds to 0.0.0.0:80 on the NODE (hostNetwork): this is a
# DEV-ONLY kind acceptance fixture. In a production cluster a hostNetwork
# pod binding :80 would conflict with the node's own services; the kind
# node has no :80 service (verified by the preflight), so the binding is
# safe in this isolated cluster.
set -e
LOG=/tmp/upstream.log
: > "$LOG"
# One persistent nc listener; each accepted connection runs the inline
# handler with the client socket on fd 0/1 (busybox nc -e runs PROG after
# the connect, on the client socket).
nc -lk -p 80 -s 0.0.0.0 -e sh -c '
  # One connection. The proxy sends the request in one burst; a short
  # sleep lets the burst land on fd 0 before we drain it. The request is
  # small (request line + a few headers + no body).
  sleep 0.3
  REQ=""
  AUTH=none
  { read -r REQ; while IFS= read -r L && [ -n "$L" ]; do
      case "$L" in
        [Aa]*uthorization:*) AUTH=$(printf "%s" "$L" | cut -d" " -f2- | tr -d "\r") ;;
      esac
    done; } 2>/dev/null
  [ -n "$AUTH" ] || AUTH=none
  REQ=$(printf "%s" "$REQ" | tr -d "\r")
  case "$REQ" in
    "GET /ok HTTP/1.1")
      echo "path=/ok auth=$AUTH" >> /tmp/upstream.log
      printf "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
      ;;
    *)
      echo "req ${REQ:-<empty>} auth=$AUTH" >> /tmp/upstream.log
      printf "HTTP/1.1 404 Not Found\r\nContent-Length: 9\r\nConnection: close\r\n\r\nnot found"
      ;;
  esac
' 2>/dev/null
