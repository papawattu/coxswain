#!/usr/bin/env python3
"""The D41e kind-acceptance tool upstream (upstream-server.py).

A plain stdlib HTTP server on 0.0.0.0:80 inside a hostNetwork python:3-alpine
pod on the kind node (imagePullPolicy IfNotPresent — the kind node already
has the image). GET /ok -> 200 "ok"; everything else -> 404. One line per
request to stdout: "method=<METHOD> path=<PATH> auth=present|none
authValue=<value>" — the upstream has no auth of its own, so the injected
Authorization header is what makes the (tool-proxy) call identifiable; the
script's assertion 3a matches the dummy test token in this line.
"""
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer


class Handler(BaseHTTPRequestHandler):
    def _reply(self, code, body):
        self.send_response(code)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)

    def _log(self):
        auth = self.headers.get("Authorization", "")
        if auth:
            print("method=%s path=%s auth=present authValue=%s"
                  % (self.command, self.path, auth), flush=True)
        else:
            print("method=%s path=%s auth=none"
                  % (self.command, self.path), flush=True)

    def do_GET(self):  # noqa: N802
        self._log()
        if self.path.split("?", 1)[0] == "/ok":
            self._reply(200, b"ok")
        else:
            self._reply(404, b"not found")

    def do_POST(self):  # noqa: N802
        self._log()
        self._reply(404, b"not found")

    def log_message(self, *args):  # silence the default stderr access log
        pass


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 80
    HTTPServer(("0.0.0.0", port), Handler).serve_forever()
