#!/usr/bin/env bash
# P2h I55 execution test (R21 I55): the p2h-stub-model.py file is executed with
# only path/host constants substituted (PORT env + /ok + one chat request),
# before any kind run. The in-kind pod is the same file carried by a
# ConfigMap and run by the pod's sh -c `exec python /opt/stub-model/p2h-stub-model.py`
# (no further substitution), so running it here with a local PORT is the
# real execution the kind pod performs.
#
# Exits non-zero on any failed check. Logs to stdout (tee'd by the caller).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
STUB="$REPO_ROOT/test/e2e/p2h-stub-model.py"
PORT="${PORT:-18444}"
STUB_LOG_FILE="$(mktemp /tmp/p2h-execstub-requests.XXXXXX.jsonl)"
SERVER_LOG="$(mktemp /tmp/p2h-execstub-server.XXXXXX.log)"

cleanup() { kill "$SERVER_PID" 2>/dev/null || true; rm -f "$STUB_LOG_FILE" "$SERVER_LOG"; }
trap cleanup EXIT

# The real execution: the same file the in-kind pod runs (the PORT env is the
# only substituted constant; the file itself is the ConfigMap's content).
PORT="$PORT" STUB_LOG="$STUB_LOG_FILE" python3 "$STUB" > "$SERVER_LOG" 2>&1 &
SERVER_PID=$!

FAILS=0
python3 - "$PORT" <<'PYEOF' || FAILS=1
import json
import sys
import time
import urllib.request
import socket

port = int(sys.argv[1])
socket.setdefaulttimeout(5)

def ok():
    try:
        urllib.request.urlopen("http://127.0.0.1:%d/ok" % port, timeout=10)
        return True
    except Exception:
        return False

def chat(n_msgs):
    req = urllib.request.Request(
        "http://127.0.0.1:%d/v1/chat/completions" % port,
        data=json.dumps({"model": "p2h-stub",
                         "messages": [{"role": "user", "content": "hi"}] * n_msgs}).encode(),
        headers={"Content-Type": "application/json"})
    return json.loads(urllib.request.urlopen(req, timeout=10).read().decode())

fails = []
for i in range(30):
    if ok():
        break
    time.sleep(0.5)
else:
    fails.append("stub /ok never answered on port %d" % port)

if not fails:
    r0 = chat(1)
    c0 = r0["choices"][0]
    if c0.get("finish_reason") != "tool_calls":
        fails.append("request 0 (n=1, the seed-only call): finish_reason=%r (expected tool_calls)" % c0.get("finish_reason"))
    tcs = c0.get("message", {}).get("tool_calls") or []
    if len(tcs) != 1 or tcs[0].get("function", {}).get("name") != "shell":
        fails.append("request 0: no single shell tool call: %r" % tcs)
    else:
        cmd = json.loads(tcs[0]["function"]["arguments"]).get("command", "")
        if cmd != "echo >> README.md":
            fails.append("request 0: the implement-instruction is not the bare README.md one-liner: %r" % cmd)
        # The command must NOT reference COX_WORKSPACE: the runner's shell tool
        # runs with cmd.Dir = the workspace (runner.go execShell) and the agent
        # container has no COX_WORKSPACE env — a `cd \"$COX_WORKSPACE\"` under
        # set -e aborts on the empty word (the P2h kind-run stall: every
        # tool call exited 2, so no Implementing run ever changed README.md
        # and the verify Job cloned an unchanging base commit).
        if "COX_WORKSPACE" in cmd:
            fails.append("request 0: the implement-instruction references COX_WORKSPACE (unset in the agent — the cd aborts under set -e): %r" % cmd)
        # The command must NOT stage/commit itself: the runner's commitWorkspace
        # owns the commit and stages only non-protected source paths (a
        # tool-side `git add -A` would pull the operator-owned .coxswain dir
        # into the verified commit).
        if "git add" in cmd or "git commit" in cmd:
            fails.append("request 0: the implement-instruction must not git add/commit (commitWorkspace owns the commit): %r" % cmd)
    u0 = r0.get("usage", {})
    if (u0.get("prompt_tokens"), u0.get("completion_tokens")) != (100, 100):
        fails.append("request 0: usage %r (expected prompt=100 completion=100)" % u0)

    r1 = chat(3)
    c1 = r1["choices"][0]
    if c1.get("finish_reason") != "stop" or not (c1.get("message", {}).get("content") or ""):
        fails.append("request 1 (n=3, the second call): not a plain content answer: %r" % c1)
    u1 = r1.get("usage", {})
    if (u1.get("prompt_tokens"), u1.get("completion_tokens")) != (100, 100):
        fails.append("request 1: usage %r (expected prompt=100 completion=100)" % u1)

for f in fails:
    print("   [FAIL] %s" % f)
if fails:
    sys.exit(1)
print("   ok: the stub's pinned responses (request 0: the shell tool call; request 1: the plain answer; usage {100,100})")
PYEOF

# The stub's request audit log (item 14: the meter's INPUT, NOT the
# cross-check oracle): it must have recorded exactly the two chat requests.
CHAT_REQS="$(grep -c '"n":' "$STUB_LOG_FILE" 2>/dev/null || echo 0)"
if [ "$CHAT_REQS" = "2" ]; then
  echo "   ok: the stub's request log recorded the 2 chat requests (n=1, n=3) — the meter's input (item 14)"
else
  echo "   [FAIL] the stub's request log recorded $CHAT_REQS chat requests (expected 2: n=1, n=3)" >&2
  FAILS=1
fi

# The I55 execution test for IMPLEMENT_CMD itself: run the EXACT command the
# stub serves (request 0's tool call) in a temp git repo with COX_WORKSPACE
# UNSET (the agent container has no COX_WORKSPACE env — this is the shape of
# the kind stall: a `cd "$COX_WORKSPACE"` under set -e aborts on the empty
# word before the echo, so README.md never grew and the verify Job cloned an
# unchanging base commit). The runner's shell tool runs the command with
# cmd.Dir = the workspace (runner.go execShell), so the temp repo stands in
# for /workspace. Assert README.md GREW after the command.
CMD_FROM_STUB="$(python3 -c "import importlib.util,sys; spec=importlib.util.spec_from_file_location('stub','$STUB'); m=importlib.util.module_from_spec(spec); spec.loader.exec_module(m); print(m.IMPLEMENT_CMD)")"
WORKTREE="$(mktemp -d /tmp/p2h-execstub-worktree.XXXXXX)"
( cd "$WORKTREE" && git init -q && git config user.name stub && git config user.email stub@local && echo seed > README.md && git add README.md && git commit -qm seed )
BEFORE_LEN="$(wc -l < "$WORKTREE/README.md")"
if ( cd "$WORKTREE" && env -u COX_WORKSPACE sh -c "$CMD_FROM_STUB" ); then
  AFTER_LEN="$(wc -l < "$WORKTREE/README.md")"
  if [ "$AFTER_LEN" -gt "$BEFORE_LEN" ]; then
    echo "   ok: the implement-instruction ran in a git repo with COX_WORKSPACE unset and grew README.md ($BEFORE_LEN -> $AFTER_LEN lines)"
  else
    echo "   [FAIL] the implement-instruction ran but README.md did not grow (still $AFTER_LEN lines)" >&2
    FAILS=1
  fi
else
  echo "   [FAIL] the implement-instruction exited non-zero with COX_WORKSPACE unset (the kind-stall shape: cd \"\$COX_WORKSPACE\" aborts under set -e): %s" "$CMD_FROM_STUB" >&2
  FAILS=1
fi
rm -rf "$WORKTREE"

# The sh -c positional-argument case (the seed_repo's R="$1" pattern): the
# first argument after the script is $0, NOT $1 — the repo name must be
# passed with a placeholder for $0 (sh -c '<script>' _ "$1"). This case
# asserts the name ARRIVES as $1 (the prior run's 422 "Name Required" was
# the $0/$1 shift: R="$1" was empty because the repo name was $0).
SH_C_POS="$(sh -c 'echo "$1"' _ p2h-positional-test)"
if [ "$SH_C_POS" = "p2h-positional-test" ]; then
  echo "   ok: the sh -c positional case — sh -c '<script>' _ <name> passes the name as \$1 (the seed_repo's R=\"\$1\" pattern)"
else
  echo "   [FAIL] the sh -c positional case: sh -c 'echo \"\$1\"' _ p2h-positional-test printed [${SH_C_POS}] (expected [p2h-positional-test]) — the R=\"\$1\" pattern would be empty" >&2
  FAILS=1
fi
# The NEGATIVE case (the prior run's bug): WITHOUT the placeholder, the
# repo name is $0 and $1 is EMPTY (the R="$1" would be empty -> the API
# 422s with "Name Required").
SH_C_NOPOS="$(sh -c 'echo "[$1]"' p2h-no-positional-test)"
if [ "$SH_C_NOPOS" = "[]" ]; then
  echo "   ok: the sh -c NEGATIVE case — without the placeholder, \$1 is empty (the prior run's 422 root cause)"
else
  echo "   [FAIL] the sh -c NEGATIVE case: sh -c 'echo \"[\$1]\"' p2h-no-positional-test printed [${SH_C_NOPOS}] (expected [[]] — the empty \$1)" >&2
  FAILS=1
fi

if [ "$FAILS" -ne 0 ]; then
  echo "=== p2h-stub-model.py execution test FAILED (port $PORT) ==="
  echo "--- server log ---"
  cat "$SERVER_LOG"
  echo "--- end ---"
  exit 1
fi
echo "=== p2h-stub-model.py execution test PASSED (port $PORT) ==="
exit 0
