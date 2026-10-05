#!/bin/sh
# The D41e kind-acceptance tool upstream listener (upstream-server.sh): a
# fixed-path HTTP server on 0.0.0.0:80 inside a hostNetwork busybox pod on
# the kind node (the pod gets the node's IP 172.21.0.2 — OUTSIDE the
# operator's pod 10.244.0.0/16 / service 10.96.0.0/12 CIDRs and outside the
# RFC1918/loopback/link-local ranges the controller's ToolUpstreamInCluster
# check rejects). The tool upstream URL is http://172.21.0.2:80 — the
# controller check passes (the IP is a public /24, treated as external) and
# the tool-proxy netpol's external carve-out (0.0.0.0/0 except pod/service
# CIDRs) permits the dial.
#
# GET /ok → 200 "ok" + a log line naming the request's Authorization header
# (the upstream has no auth of its own — the injected header is what makes
# the call identifiable); everything else → 404 + a log line.
#
# Each connection is answered by a SINGLE busybox nc process that:
#   1. reads the request on fd 0 (the client socket, set up by
#      `nc -l -p 80 -s 0.0.0.0 -c sh -c '...'` — the accepted socket is
#      stdin/stdout of the -c command), logging the request line + the
#      Authorization header to /tmp/upstream.log;
#   2. writes the HTTP response on fd 1 (the same client socket).
#
# The request is read with a SHORT SLEEP as the "client done sending"
# heuristic (the tool proxy sends the request in one burst and waits for
# the response — the sleep is shorter than the proxy's read timeout). The
# log line in /tmp/upstream.log is what the script asserts (which requests
# reached the upstream + the Authorization header carried).
#
# The listener binds to 0.0.0.0:80 on the NODE (hostNetwork): this is a
# DEV-ONLY kind acceptance fixture. In a production cluster a hostNetwork
# pod binding :80 would conflict with the node's own services; the kind
# node has no :80 service (verified by the preflight), so the binding is
# safe in this isolated cluster.
set -e
LOG=/tmp/upstream.log
: > "$LOG"
# A loop that handles one connection per iteration (the tool proxy opens a
# new connection per HTTP request; the listener re-listens for the next).
while :; do
  timeout 5 nc -l -p 80 -s 0.0.0.0 -c sh -c '
    # One connection (busybox nc -c runs this sh with the client socket on
    # fd 0/1). The proxy sends the request in one burst; a short sleep lets
    # the burst land on fd 0 before we drain it. The request is small
    # (request line + a few headers + no body).
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
        echo "GET /ok auth=$AUTH" >> /tmp/upstream.log
        printf "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
        ;;
      *)
        echo "req ${REQ:-<empty>} auth=$AUTH" >> /tmp/upstream.log
        printf "HTTP/1.1 404 Not Found\r\nContent-Length: 9\r\nConnection: close\r\n\r\nnot found"
        ;;
    esac
  ' 2>/dev/null || true
  sleep 0.05
done
