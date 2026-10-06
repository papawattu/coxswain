#!/usr/bin/env python3
# P2h kind acceptance fixture: the STUB MODEL SERVER (docs/TDD-PLAN-PHASE2.md,
# "P2h — Kind acceptance", pinned stub behaviour, item 14).
#
# An OpenAI-compatible /v1/chat/completions server (stdlib http only) that
# serves the reference runner. Its behaviour is PINNED by the P2h plan:
#
#   1. Every completion response contains a fixed IMPLEMENT-INSTRUCTION the
#      reference runner's shell tool executes — a one-liner that appends a
#      newline to a NON-PROTECTED file (README.md), so every Implementing
#      iteration produces a NEW commit (the verify Job re-runs, the
#      iteration advances). A stub that writes no code may never produce a
#      new commit (item 14).
#
#   2. Every response carries a FIXED usage {prompt_tokens: 100,
#      completion_tokens: 100} = 200 tokens per request (item I's arithmetic
#      fix: the budget cap math uses 200/request).
#
#   3. Non-streaming (stream: false), the reference runner's shape (the
#      steering-proof measures are P2b's unit coverage, not exercised here).
#
#   4. The failing check the Loops' acceptanceChecks use is `test -f
#      /nonexistent` — it fails with the SAME message regardless of the
#      commit, so every iteration's verify failure output is identical and
#      the stall detector's consecutive-identical run advances.
#
#   5. The stub BOUNDS the request count itself (the P2h plan does NOT pin a
#      runner -max-steps cap — the runner runs with the operator's default
#      25): the stub answers a request whose LAST message is NOT a tool
#      result with the fixed shell tool call, and a request whose LAST
#      message IS a tool result (role "tool") with a plain "done" answer
#      (no tool call), so the runner's model loop ends by itself after the
#      tool call — exactly 2 model requests per phase run (the tool call +
#      the "done"), and the token math (200/request) holds without an
#      operator cap.
#
#      The decision keys on the LAST message's role, NOT the messages-list
#      length. The reference runner ALWAYS sends at least a system prompt +
#      the user seed (its first Implementing call is length 2: system +
#      user), so a length-based pin (the original fixture assumed a
#      length-1 seed-only first call and sent the tool call only when
#      len(messages) == 1) never fired in-kind: every Implementing run got
#      the plain "done" answer on its first call, the shell tool never ran,
#      README.md never changed, and the verify Job kept cloning the
#      unchanging base commit (the P2h kind-run stall). Keying on the last
#      message makes the pin independent of the runner's message framing:
#      a fresh phase run's first call ends with the user seed -> the tool
#      call; the re-call after the tool ran ends with the tool result ->
#      "done". A blocked run's re-call (the tool result is a non-zero
#      failure) still ends with a tool result -> "done" (the run is blocked
#      and the operator requeues, as intended).
#
#      The command itself is a bare `echo >> README.md`: the runner's shell
#      tool runs with cmd.Dir = the workspace (runner.go execShell) and the
#      runner commits the work itself at the end of a successful
#      Implementing (commitWorkspace, phase.go — it stages only
#      non-protected SOURCE paths, never the operator-owned .coxswain dir).
#      No cd (COX_WORKSPACE is NOT an agent env — a `cd "$COX_WORKSPACE"`
#      under set -e aborts on the empty word before the echo), no git
#      add/commit (the runner's commitWorkspace owns the commit; a
#      tool-side `git add -A` would stage the operator-owned .coxswain dir
#      into the verified commit).
#
#   6. GET /ok answers 200 (liveness; the script polls it).
#
# Each request is logged to stdout (the stub is the meter's INPUT, not the
# cross-check's oracle — item 14) and to /tmp/stub-requests.jsonl for the
# operator-side per-Loop token-count cross-check (assertion 2: the per-Loop
# status.budget totals must equal this log's 100*N prompt / 100*N completion).
#
# Runs in-kind as a hostNetwork python:3-alpine pod (the d41/p2b in-kind
# upstream pattern), bound to 0.0.0.0:$PORT (default 8444).

import json
import os
import socket
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("PORT", "8444"))
# The request-audit log path. The in-kind pod uses the default
# /tmp/stub-requests.jsonl (the plan's path); the I55 execstub test overrides
# STUB_LOG via the env (a temp file it inspects). The file is the meter's
# INPUT only (item 14: the per-Loop status.budget counts are the source of
# truth, cross-checked against the REAL vLLM backend delta, never against
# this file).
LOG_PATH = os.environ.get("STUB_LOG", "/tmp/stub-requests.jsonl")

# The SLOW model (the P2h A4 determinism fix): a request whose model name is
# "p2h-stub-slow" is answered AFTER a SLOW_DELAY_S sleep. The P2h resume Loop
# uses it so its Implementing phase lasts long enough for the acceptance
# script's suspend-flip window to land INSIDE an Implementing (the flip had
# to race the phase transition — the stub's default 2-request Implementing
# finishes in seconds, and the pause can land with the Loop already at
# Verifying, where the pause's pausedFrom reads Verifying and the iteration
# advances between the pre-pause read and the pause landing). With a 30s
# sleep on the first model call, Implementing lasts >= 30s and the flip
# (polled every 2s) lands inside it deterministically.
SLOW_MODEL = "p2h-stub-slow"
# 90s (the A4 race fix, run-20261006211120): the resume Loop's suspend flip
# must land INSIDE an Implementing. The script polls the phase every 2s, so
# the Implementing window needs headroom far beyond the single model call:
# at 30s the flip window (the 2s poll + the pause landing) raced the
# Implementing->Verifying transition twice (run-20261006200606:
# pausedFrom=Verifying, iteration advanced; run-20261006211120:
# pausedFrom=Verifying again). 90s gives 3x headroom; the pinned stub
# behaviour (sleep BEFORE answering, usage unchanged) is preserved.
SLOW_DELAY_S = float(os.environ.get("SLOW_DELAY_S", "90"))

# The fixed implement-instruction (item 14). Appends ONE newline to
# README.md (a non-protected path — the "go" preset protects **/*_test.go,
# **/testdata/**, go.mod, go.sum; README.md is none of those). It is a bare
# `echo >> README.md`: the runner's shell tool runs it with the workspace as
# its working directory (runner.go execShell sets cmd.Dir = workspace) and
# the runner commits the work itself at the end of a successful Implementing
# (commitWorkspace, phase.go — it stages only non-protected SOURCE paths,
# never the operator-owned .coxswain dir). No cd (COX_WORKSPACE is NOT an
# agent env — a `cd "$COX_WORKSPACE"` under set -e aborts on the empty word
# before the echo), no git add/commit (the runner's commitWorkspace owns the
# commit; a tool-side `git add -A` would stage the operator-owned .coxswain
# dir into the verified commit).
IMPLEMENT_CMD = "echo >> README.md"

# The assistant turn with the fixed tool call (the OpenAI/vLLM wire shape the
# runner parses: id + function-name/type + arguments as a JSON string).
TOOL_CALL_MSG = {
    "id": "stub-call",
    "type": "function",
    "function": {"name": "shell", "arguments": json.dumps({"command": IMPLEMENT_CMD})},
}

DONE_CONTENT = (
    "done: applied the one-character change to README.md; "
    "the acceptance check still fails (test -f /nonexistent)"
)


def choice_kind(req):
    # The stub's pinned choice (item 5): a request whose LAST message is a
    # tool result (role "tool" — the re-call after the runner executed the
    # shell tool) -> the plain "done" answer (no tool call, the loop exits).
    # ANY other last message (a fresh phase run's first call ends with the
    # user seed) -> the fixed shell tool call. Keying on the LAST message's
    # role — NOT the messages-list length — makes the pin independent of the
    # runner's message framing: the reference runner always sends at least a
    # system prompt + the user seed (its first call is length 2), so a
    # length-based pin never fired in-kind (the P2h kind-run stall: every
    # Implementing run got "done" on its first call, the shell tool never
    # ran, and README.md never changed). A blocked run's re-call (the tool
    # result carries a non-zero failure) still ends with a tool result ->
    # "done" (the run is blocked and the operator requeues, as intended).
    msgs = req.get("messages") or []
    if msgs and isinstance(msgs[-1], dict) and msgs[-1].get("role") == "tool":
        return "done"
    return "tool_call"


def build_choice(index, kind):
    if kind == "tool_call":
        return {
            "index": index,
            "message": {
                "role": "assistant",
                "content": "",
                "tool_calls": [TOOL_CALL_MSG],
            },
            "finish_reason": "tool_calls",
        }
    return {
        "index": index,
        "message": {"role": "assistant", "content": DONE_CONTENT},
        "finish_reason": "stop",
    }


class StubHandler(BaseHTTPRequestHandler):
    server_version = "p2h-stub-model/1.0"

    def log_message(self, fmt, *args):
        sys.stderr.write("[stub] %s\n" % (fmt % args))

    def _send(self, code, body_bytes, content_type):
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body_bytes)))
        self.end_headers()
        self.wfile.write(body_bytes)

    def do_GET(self):
        if self.path in ("/ok", "/"):
            self._send(200, b"ok", "text/plain")
            return
        self._send(404, b"not found", "text/plain")

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        if not self.path.startswith("/v1/chat/completions"):
            self._send(404, b"not found", "application/json")
            return
        try:
            req = json.loads(raw.decode() or "{}")
        except Exception as e:
            self._send(400, json.dumps({"error": "bad request: %s" % e}).encode(),
                       "application/json")
            return
        # The SLOW model's sleep (the A4 determinism fix): the sleep is
        # BEFORE the audit log + response, so the request's wall clock
        # includes the delay. Only the resume Loop sends this model.
        model = req.get("model") or "p2h-stub"
        if model == SLOW_MODEL:
            time.sleep(SLOW_DELAY_S)
        # The stub is NON-streaming regardless of the request's stream flag
        # (the reference runner's shape; the plan pins stream:false).
        kind = choice_kind(req)
        body = {
            "id": "stub-p2h-%d" % len(req.get("messages") or []),
            "object": "chat.completion",
            "model": model,
            "choices": [build_choice(0, kind)],
            "usage": {"prompt_tokens": 100, "completion_tokens": 100,
                      "total_tokens": 200},
        }
        # Audit log (stdout + file): the stub's request record. It is the
        # meter's INPUT only (item 14: the per-Loop status.budget counts are
        # the source of truth, cross-checked against the REAL vLLM backend
        # delta, never against this file).
        rec = {
            "n": len(req.get("messages") or []),
            "model": model,
            "tools": len(req.get("tools") or []),
            "stream": bool(req.get("stream")),
            "choice": kind,
        }
        sys.stdout.write(json.dumps(rec) + "\n")
        sys.stdout.flush()
        try:
            with open(LOG_PATH, "a") as f:
                f.write(json.dumps(rec) + "\n")
        except Exception as e:
            sys.stderr.write("[stub] request log write failed: %s\n" % e)
        self._send(200, json.dumps(body).encode(), "application/json")


def main():
    socket.setdefaulttimeout(30)
    srv = ThreadingHTTPServer(("0.0.0.0", PORT), StubHandler)
    sys.stderr.write("[stub] listening on 0.0.0.0:%d\n" % PORT)
    srv.serve_forever()


if __name__ == "__main__":
    main()
