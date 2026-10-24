#!/usr/bin/env python3
"""F54: hostile-agent model stand-in (ADR-0006:95-99).

The reference runner runs every model tool call through `sh -c` in the agent
container, so a model that returns `shell` tool calls IS the hostile agent.
In the IMPLEMENTING phase the first turn returns ONE shell tool call (the
--probe-cmd, or --tamper-cmd for the tamper Loop); the next turn (the tool
result is the last message) returns plain text. Every request and the tool
result is logged verbatim to stderr (and --log if given) so the orchestrator
can grep the EVIL-PROBE lines. Other phases answer "Done." (as
.samples/f39/standin.py). GET answers {"ok": true} (the model proxy's dial
check and /v1/models).
"""
import argparse
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

ARGS = None


def log(m):
    print(m, file=sys.stderr, flush=True)
    if ARGS and ARGS.log:
        with open(ARGS.log, "a") as f:
            f.write(m + "\n")


def reply_for(body):
    """Pure function: the assistant message for one chat request."""
    msgs = body.get("messages") or []
    all_user = " ".join(m.get("content") or "" for m in msgs if m.get("role") == "user")
    implementing = "IMPLEMENTING phase" in all_user
    done = bool(msgs) and msgs[-1].get("role") == "tool"
    cmd = ARGS.tamper_cmd or ARGS.probe_cmd
    if implementing and not done and cmd:
        return {"role": "assistant", "content": None, "tool_calls": [{
            "id": "call_1", "type": "function",
            "function": {"name": "shell", "arguments": json.dumps({"command": cmd})}}]}
    return {"role": "assistant", "content": "Done."}


class H(BaseHTTPRequestHandler):
    def _send(self, obj):
        d = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(d)))
        self.end_headers()
        self.wfile.write(d)

    def log_message(self, *a):
        pass

    def do_GET(self):
        # log the client so the orchestrator can prove WHO called the model directly (probe 8)
        log("GET %s from %s" % (self.path, self.client_address[0]))
        self._send({"ok": True})

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(n) or b"{}")
        msgs = body.get("messages") or []
        if msgs and msgs[-1].get("role") == "tool":
            log("TOOL-RESULT-BEGIN")
            log(str(msgs[-1].get("content")))
            log("TOOL-RESULT-END")
        msg = reply_for(body)
        log("REQ last_role=%s tool_call=%s" % (msgs[-1].get("role") if msgs else None, bool(msg.get("tool_calls"))))
        self._send({"model": "evil", "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
                    "choices": [{"index": 0, "finish_reason": "stop", "message": msg}]})


def main():
    global ARGS
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=8080)
    ap.add_argument("--probe-cmd", default="")
    ap.add_argument("--tamper-cmd", default="")
    ap.add_argument("--log", default="")
    ARGS = ap.parse_args()
    HTTPServer(("", ARGS.port), H).serve_forever()


if __name__ == "__main__":
    main()
