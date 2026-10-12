#!/usr/bin/env python3
"""alpha-e2e in-kind stub model (LAST-RESORT fixture, not the default model).

docs/QUICKSTART.md's dev model is the homelab vLLM behind the nginx LB
(192.168.1.20:8000). The alpha e2e uses it when the numeric preflight says
it is healthy, and only falls back to this stub when a preflight or an
in-run check shows the endpoint is down (docs/INVESTIGATING-IMPLEMENTING-
STALL.md: record the stall with numbers, don't call it flaky).

The stub is the minimal version of test/e2e/p2h-stub-model.py (which is
PINNED to the P2h plan's behaviour and must not be modified for alpha):

  - answers POST /v1/chat/completions;
  - a request whose LAST message is NOT a tool result (role "tool") gets the
    fixed shell tool call: create .a6-marker (a NON-PROTECTED path, so the
    runner's commitWorkspace stages it and every Implementing iteration
    produces a NEW commit the verify Job re-runs against);
  - a request whose last message IS a tool result ends the run with a plain
    "done" answer (no tool call) — the runner's model loop terminates by
    itself after one tool round, like the P2h pin;
  - every response carries the fixed usage {prompt_tokens: 100,
    completion_tokens: 100} (the metering proxy's metering lines then exist
    for the A4 facts: budget used).

The marker file, not the code, is what the acceptance check verifies: the
Loop's goal and check are "create .a6-marker and make test -f .a6-marker
pass" (the alpha task is the acceptance run itself — see the header of
test/e2e/alpha-e2e.sh). The real vLLM model gets the same instructions in
the goal, where a small real edit (one added line in README.md) is made
instead.
"""
import json
import sys
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("PORT", "8000"))

TOOL_CMD = "printf 'alpha-e2e marker %s\\n' $(( $(cat .a6-marker 2>/dev/null | wc -l) + 1 )) >> .a6-marker"


def completion(msgs):
    last = msgs[-1] if msgs else {}
    if last.get("role") == "tool":
        # The tool ran (success or failure); end the run — no more tool calls.
        content = "done"
        tool_calls = []
    else:
        content = ""
        tool_calls = [
            {
                "id": "call_1",
                "type": "function",
                "function": {
                    "name": "shell",
                    "arguments": json.dumps({"command": TOOL_CMD}),
                },
            }
        ]
    return {
        "id": "cmpl-alpha",
        "object": "chat.completion",
        "created": 0,
        "model": "alpha-stub",
        "choices": [
            {
                "index": 0,
                "message": {
                    "role": "assistant",
                    "content": content,
                    **({"tool_calls": tool_calls} if tool_calls else {}),
                },
                "finish_reason": "stop",
            }
        ],
        "usage": {"prompt_tokens": 100, "completion_tokens": 100, "total_tokens": 200},
    }


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _reply(self, status, obj):
        data = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path in ("/ok", "/v1/health"):
            self._reply(200, {"ok": True})
        else:
            self._reply(404, {"error": "not found"})

    def do_POST(self):
        if self.path not in ("/v1/chat/completions", "/chat/completions"):
            self._reply(404, {"error": "not found"})
            return
        body = self.rfile.read(int(self.headers.get("Content-Length", 0) or 0))
        try:
            req = json.loads(body or b"{}")
        except Exception:
            req = {}
        self._reply(200, completion(req.get("messages", [])))


def main():
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"alpha stub model listening on 0.0.0.0:{PORT}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
