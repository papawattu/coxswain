#!/usr/bin/env python3
"""P2b e2e in-kind fake model: an OpenAI-compatible /v1/chat/completions endpoint.

Runs on hostNetwork (binds 0.0.0.0:PORT, the kind-network eth0 the dial targets
reaches it). It:
  - answers POST /v1/chat/completions (and /chat/completions) with a JSON
    completion that carries `usage` (prompt_tokens/completion_tokens), echoing
    whether `stream_options.include_usage` was in the request body;
  - logs one JSON line per request to stdout: the Authorization header (to
    prove the metering proxy injected the mounted credential),
    stream_options.include_usage (to prove the proxy forced it on streaming
    requests), and any Accept-Encoding (to prove the proxy did NOT forward the
    client's).

This is the P2b model-side upstream: it stands in for a real model so the e2e
is deterministic and needs no network access to a real model.
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import os


class Handler(BaseHTTPRequestHandler):
    def _log(self, event):
        line = json.dumps(event) + "\n"
        sys.stdout.write(line)
        sys.stdout.flush()
        try:
            with open("/tmp/fake-model.log", "a") as f:
                f.write(line)
        except Exception:
            pass

    def log_message(self, *a):
        pass  # silence the default access log (we emit our own)

    def _reply(self, status, obj):
        data = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        path = self.path
        body = self.rfile.read(int(self.headers.get("Content-Length", 0) or 0))
        try:
            req = json.loads(body or b"{}")
        except Exception:
            req = {}
        auth = self.headers.get("Authorization", "")
        accept_enc = self.headers.get("Accept-Encoding", "")
        stream_opts = req.get("stream_options")
        include_usage = bool(
            isinstance(stream_opts, dict) and stream_opts.get("include_usage")
        )
        is_stream = bool(req.get("stream"))

        self._log(
            {
                "event": "request",
                "auth": auth,
                "accept_encoding": accept_enc,
                "stream": is_stream,
                "stream_options_include_usage": include_usage,
            }
        )

        # 404 any path that isn't the chat endpoint (the D41 fixed-path shape).
        if path not in ("/v1/chat/completions", "/chat/completions"):
            self._reply(404, {"error": "not found"})
            return

        if is_stream:
            # A minimal SSE stream that ends with a usage chunk.
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            def sse(obj):
                self.wfile.write(b"data: " + json.dumps(obj).encode() + b"\n\n")
                self.wfile.flush()
            sse(
                {
                    "id": "1",
                    "object": "chat.completion.chunk",
                    "choices": [
                        {
                            "delta": {"role": "assistant", "content": "ok"},
                            "index": 0,
                        }
                    ],
                }
            )
            sse(
                {
                    "id": "1",
                    "object": "chat.completion.chunk",
                    "choices": [],
                    "usage": {
                        "prompt_tokens": 7,
                        "completion_tokens": 3,
                    },
                }
            )
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
            return

        self._reply(
            200,
            {
                "id": "1",
                "object": "chat.completion",
                "model": req.get("model", "fake"),
                "choices": [
                    {
                        "message": {"role": "assistant", "content": "done"},
                        "finish_reason": "stop",
                        "index": 0,
                    }
                ],
                "usage": {"prompt_tokens": 5, "completion_tokens": 2},
            },
        )

    def do_GET(self):
        if self.path == "/ok":
            self._reply(200, {"ok": True})
            return
        self._reply(404, {"error": "not found"})


if __name__ == "__main__":
    port = int(os.environ.get("PORT", "8443"))
    # Log each request to stdout (the pod log) AND /tmp/fake-model.log (so a
    # kubectl-exec reader can read it without relying on the pod log being
    # captured at exactly the right moment). BaseHTTPRequestHandler._log
    # writes one line per request to stdout via log_message; we ALSO tee the
    # per-request JSON to the file here for robustness.
    def _run():
        ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()
    try:
        _run()
    except Exception as e:
        with open("/tmp/fake-model.log", "a") as f:
            f.write("FATAL: %r\n" % e)
        raise
