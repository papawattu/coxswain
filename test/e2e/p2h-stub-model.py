#!/usr/bin/env python3
# P2h kind acceptance fixture: the STUB MODEL SERVER (docs/TDD-PLAN-PHASE2.md,
# "P2h — Kind acceptance", pinned stub behaviour, item 14).
#
# An OpenAI-compatible /v1/chat/completions server (stdlib http only) that
# serves the reference runner. Its behaviour is PINNED by the P2h plan:
#
#   1. Every completion response contains a fixed IMPLEMENT-INSTRUCTION the
#      reference runner's shell tool executes — a one-liner that commits a
#      one-character change to a NON-PROTECTED file (append a newline to
#      README.md) every time, so every Implementing iteration produces a NEW
#      commit (the verify Job re-runs, the iteration advances). A stub that
#      writes no code may never produce a new commit (item 14).
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
#      25): the stub answers the FIRST model call of a phase run (the
#      messages-list length 1 = the seed only) with the fixed shell tool
#      call and EVERY LATER request (length >= 2) with a plain "done" answer
#      (no tool call), so the runner's model loop ends by itself after the
#      tool call — exactly 2 model requests per phase run (the tool call +
#      the "done"), and the token math (200/request) holds without an
#      operator cap. The runner's loop (runner.go run): the first model call
#      returns the tool call (the loop executes the shell tool and re-calls
#      the model); the second returns the plain answer (the loop exits with
#      the "done" content). No max-steps pressure: the runner exits the loop
#      on the no-tool-call answer, never reaching the 25-step cap.
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
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("PORT", "8444"))
# The request-audit log path. The in-kind pod uses the default
# /tmp/stub-requests.jsonl (the plan's path); the I55 execstub test overrides
# STUB_LOG via the env (a temp file it inspects). The file is the meter's
# INPUT only (item 14: the per-Loop status.budget counts are the source of
# truth, cross-checked against the REAL vLLM backend delta, never against
# this file).
LOG_PATH = os.environ.get("STUB_LOG", "/tmp/stub-requests.jsonl")

# The fixed implement-instruction (item 14): the runner's shell tool executes
# it. Appends ONE newline to README.md (a non-protected path — the "go"
# preset protects **/*_test.go, **/testdata/**, go.mod, go.sum; README.md is
# none of those) and commits it, so EVERY Implementing run produces a NEW
# commit and the verify Job re-runs. The workspace PVC is root-owned (the
# seed / init containers run as root) while the runner's shell runs as uid
# 65532: git refuses the repo without safe.directory (the runner's own gitC
# carries the flag per-command, but the tool's shell does not) — the command
# sets GIT_CONFIG_GLOBAL=/dev/null (no HOME git config needed, and nothing is
# left on the workspace) + safe.directory. set -e so a failure (e.g. not a
# repo) surfaces as a non-zero exit the runner feeds back.
IMPLEMENT_CMD = (
    "set -e; export GIT_CONFIG_GLOBAL=/dev/null; "
    "export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0=\"$COX_WORKSPACE\"; "
    "cd \"$COX_WORKSPACE\"; "
    "git add -A; "
    "echo >> README.md; "
    "git add README.md; "
    "git -c user.name=p2h-stub -c user.email=p2h-stub@local "
    "commit -m 'stub: append a newline to README.md'"
)

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


def count_messages(req):
    # The stub's request log (item 14) counts by the messages-list length.
    # The FIRST model call of a fresh run has ONLY the seed message
    # (length 1); the second has seed + assistant + tool result (length 3).
    # The in-kind per-phase pod starts a fresh conversation, so the
    # phase's first call is length 1 (the tool call) and the second is
    # length 3 (the plain answer). A LATER request (length >= 4, the
    # re-run of the phase within the same pod — rare; the operator
    # re-creates the pod per phase run, so this is a defensive case) is
    # the plain answer (the loop has already exited; a tool call here
    # would be a second commit, which the plan does not require).
    return len(req.get("messages", []))


def build_choice(index, req_index):
    # The runner's model loop is bounded by the operator's -max-steps 2: the
    # FIRST model call of a phase run is the fixed shell tool call; the
    # SECOND is a plain "done" answer (no tool call -> the loop exits). The
    # stub's request log (the meter's input, item 14) counts by the
    # messages-list length, so the FIRST model call of a run (index 0 = the
    # seed message only) -> the tool call; the SECOND (index 2 = the seed +
    # the tool result) -> the plain answer. The P2h plan's "every
    # Implementing iteration produces a new commit" is satisfied by the
    # Implementing run's first tool call; the runner's second ("done") call
    # exits the loop without a second commit, and the verify Job already ran
    # against the first run's commit (the iteration's advance is driven by
    # the verify Job's failure, not by the runner's second call). A NEW
    # sandbox pod (the operator re-creates the pod per phase run) starts a
    # NEW conversation at index 0, so the phase's first call in each pod is
    # again the tool call (a new commit, if the phase is Implementing and
    # the work is not already done — the idempotent git add -A; the commit
    # of an unchanged README.md is a no-op the runner feeds back, not a
    # failure).
    # The phase's first model call of a FRESH run is the messages-list
    # length 1 (the seed only); the second is length 3 (seed + assistant +
    # tool result). The tool call is for length 1 ONLY; length >= 3 is the
    # plain "done" answer (the loop exits after the tool call). This is the
    # pin the P2h e2e's execstub test asserts (request 0 = the seed-only
    # call, request 1 = the length-3 call). The in-kind pod's conversation
    # starts fresh per phase run (the operator re-creates the sandbox pod
    # per phase — the S4 phase-driver semantics: one container run per
    # phase), so the phase's first model call is always length 1.
    if req_index == 1:
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
        # The stub is NON-streaming regardless of the request's stream flag
        # (the reference runner's shape; the plan pins stream:false).
        req_index = count_messages(req)
        body = {
            "id": "stub-p2h-%d" % req_index,
            "object": "chat.completion",
            "model": req.get("model") or "p2h-stub",
            "choices": [build_choice(0, req_index)],
            "usage": {"prompt_tokens": 100, "completion_tokens": 100,
                      "total_tokens": 200},
        }
        # Audit log (stdout + file): the stub's request record. It is the
        # meter's INPUT only (item 14: the per-Loop status.budget counts are
        # the source of truth, cross-checked against the REAL vLLM backend
        # delta, never against this file).
        rec = {
            "n": req_index,
            "model": req.get("model"),
            "tools": len(req.get("tools") or []),
            "stream": bool(req.get("stream")),
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
