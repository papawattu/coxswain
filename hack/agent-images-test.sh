#!/bin/bash
# T2 (docs/PLAN-AGENT-ADAPTERS.md): each agent image works and is
# self-contained, shown with Docker only (no cluster).
#
# Per agent image:
#   static   the image's user is 65532:65532, and `runner -selftest` passes with
#            a read-only root, tmpfs /scratch and /tmp, and no network.
#   phases   the adapter runs Planning, then Implementing, in a workspace (a
#            git copy of examples/gocli), each in a fresh container, as the
#            sandbox runs them: uid 65532, read-only root, writable /workspace
#            and /scratch only. Planning must claim success and write PLAN.md;
#            Implementing must claim success and commit NOTES.md.
#   network  the agent container is on an internal Docker network whose only
#            other member is the real coxswain model proxy (local image, holding
#            a canary key), which forwards to MODEL_ENDPOINT. A tcpdump in the
#            agent's network namespace records every packet. PASS only if every
#            outbound packet goes to the proxy, and the only DNS name asked is
#            the proxy's.
#   creds    the canary key the proxy holds appears nowhere the agent could
#            write or log: the workspace (git objects included), /scratch, and
#            the agent's stdout and stderr.
#   proxy    the proxy logged at least one audit line per phase (one JSON line
#            per request, R63 I286).
#
# Positive controls, run once before the agents (a FAIL here means the checks
# above cannot be trusted, and the script stops):
#   egress   an agent image running `getent hosts example.com; curl` on the same
#            setup must be detected (a DNS query for example.com is seen).
#   canary   the grep finds the canary in the proxy's own key file.
#
# A step that is not run prints SKIP or NOT RUN, never PASS (R25 I84).
#
# Pod limits: the agent containers run with the sandbox pod's limits (512 MiB
# of memory, half a CPU; T2_MEMORY and T2_CPUS override), and /scratch's size
# is reported against the pod's 350Mi emptyDir limit.
#
# The task: T2_GOAL (default: add NOTES.md), and T2_EXPECT, the file the
# Implementing commit must contain (default NOTES.md). The real gocli task is
#   T2_GOAL="Fix Round in round.go so it rounds half away from zero. ..." T2_EXPECT=round.go
# and then the script also runs `go test ./...` on the committed result.
#
# Usage: hack/agent-images-test.sh [agent ...]   (default: all five)
# Env: AGENT_IMAGE_PREFIX (local/coxswain/agent-), AGENT_TAG (dev),
#      PROXY_IMAGE (local/coxswain/main-model-proxy:t2), MODEL_ENDPOINT
#      (http://192.168.1.20:8000), MODEL (qwen3.8-27b), EVIDENCE (a new dir under
#      .samples/agent-images/).
set -u -o pipefail

AGENTS=("$@")
[ ${#AGENTS[@]} -eq 0 ] && AGENTS=(pi opencode codex claude-code hermes)
PREFIX=${AGENT_IMAGE_PREFIX:-local/coxswain/agent-}
TAG=${AGENT_TAG:-dev}
PROXY_IMAGE=${PROXY_IMAGE:-local/coxswain/main-model-proxy:t2}
MODEL_ENDPOINT=${MODEL_ENDPOINT:-http://192.168.1.20:8000}
MODEL=${MODEL:-qwen3.8-27b}
EVIDENCE=${EVIDENCE:-.samples/agent-images/$(date -u +%Y%m%dT%H%M%SZ)}
REPO=$(git rev-parse --show-toplevel)
RUN=t2-$$
KEYDIR=
NET=$RUN-int
CANARY="t2-canary-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
CAP_IMAGE=local/coxswain/t2-tcpdump:dev
T2_MEMORY=${T2_MEMORY:-512m}
T2_CPUS=${T2_CPUS:-0.5}
T2_EXPECT=${T2_EXPECT:-NOTES.md}
mkdir -p "$EVIDENCE"
EVIDENCE=$(cd "$EVIDENCE" && pwd)
FAILS=0

say() { echo "$*" | tee -a "$EVIDENCE/summary.txt"; }
fail() { say "FAIL $*"; FAILS=$((FAILS + 1)); }
pass() { say "PASS $*"; }

# Files the agent wrote are owned by 65532; remove them as root in a container.
rootrm() { docker run --rm -u 0 -v "$1":/d alpine:3.22 sh -c 'rm -rf /d/* /d/.[!.]* 2>/dev/null; true' >/dev/null 2>&1; rmdir "$1" 2>/dev/null; }

cleanup() {
  docker rm -f "$RUN-proxy" "$RUN-holder" "$RUN-cap" >/dev/null 2>&1
  docker network rm "$NET" >/dev/null 2>&1
  rm -rf "$KEYDIR"
}
trap cleanup EXIT

# --- shared setup ------------------------------------------------------------
printf 'FROM alpine:3.22\nRUN apk add --no-cache tcpdump\nENTRYPOINT ["tcpdump"]\n' |
  docker build -q -t "$CAP_IMAGE" - >/dev/null || { echo "FATAL cannot build $CAP_IMAGE"; exit 2; }
docker image inspect "$PROXY_IMAGE" >/dev/null 2>&1 || { echo "FATAL no proxy image $PROXY_IMAGE"; exit 2; }
docker network create --internal "$NET" >/dev/null || { echo "FATAL cannot create $NET"; exit 2; }
mkdir -p "$EVIDENCE/proxy"
# The canary key lives outside the evidence dir and is removed on exit, so no
# key is left in the evidence (S10). The evidence records only where it was.
KEYDIR=$(mktemp -d "${TMPDIR:-/tmp}/t2-key.XXXXXX")
printf '%s' "$CANARY" > "$KEYDIR/key"
chmod 0644 "$KEYDIR/key"
echo "the proxy's canary key was $KEYDIR/key (removed on exit; not kept in the evidence)" > "$EVIDENCE/proxy/key-location.txt"
# The proxy: on the default bridge (to reach MODEL_ENDPOINT) and on the internal
# network as "model-proxy" (the agent's only peer).
docker run -d --name "$RUN-proxy" -e MODEL_ENDPOINT="$MODEL_ENDPOINT" -e MODEL_CRED_FILE=/creds/key \
  -e PROXY_USAGE_FILE=/tmp/usage.json -e LOOP_NAME=t2 -e LOOP_NAMESPACE=t2 \
  -v "$KEYDIR/key":/creds/key:ro --tmpfs /tmp "$PROXY_IMAGE" >/dev/null || { echo "FATAL proxy start"; exit 2; }
docker network connect --alias model-proxy "$NET" "$RUN-proxy" || { echo "FATAL proxy network"; exit 2; }
PROXY_IP=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" "$RUN-proxy")
say "setup: network $NET (internal), proxy $PROXY_IMAGE at $PROXY_IP -> $MODEL_ENDPOINT, model $MODEL, evidence $EVIDENCE"

# start_capture <name>: a namespace holder on the internal network only, and a
# tcpdump sharing it. The agent containers then join the same namespace.
start_capture() {
  docker rm -f "$RUN-holder" "$RUN-cap" >/dev/null 2>&1
  docker run -d --name "$RUN-holder" --network "$NET" alpine:3.22 sleep 7200 >/dev/null
  docker run -d --name "$RUN-cap" --net "container:$RUN-holder" --cap-add NET_RAW --cap-add NET_ADMIN \
    "$CAP_IMAGE" -i any -nn -l >/dev/null
  sleep 2
}
stop_capture() { # <outfile>
  sleep 1
  docker logs "$RUN-cap" > "$1" 2>&1
  docker rm -f "$RUN-holder" "$RUN-cap" >/dev/null 2>&1
}
# The agent's own IP on the internal network (the source of its outbound packets).
holder_ip() { docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" "$RUN-holder"; }

# bad_traffic <tcpdump file> <own ip>: every outbound packet not to the proxy
# and not on loopback, and every DNS question other than model-proxy.
bad_traffic() {
  local f=$1 own=$2
  grep ' Out ' "$f" | grep -F "IP $own." | grep -vF "> $PROXY_IP." || true
  grep -oE ' (A|AAAA|HTTPS|SRV|TXT)\? [^ ]+' "$f" | grep -vE '\? model-proxy\.?$' || true
}

# --- positive controls -------------------------------------------------------
CTL_IMAGE="$PREFIX${AGENTS[0]}:$TAG"
start_capture
OWN=$(holder_ip)
docker run --rm --net "container:$RUN-holder" --entrypoint sh "$CTL_IMAGE" \
  -c 'getent hosts example.com; curl -m 5 -s https://example.com >/dev/null; true' > "$EVIDENCE/control-egress.out" 2>&1
stop_capture "$EVIDENCE/control-egress.tcpdump"
if bad_traffic "$EVIDENCE/control-egress.tcpdump" "$OWN" | grep -q 'example.com'; then
  pass "control egress: the capture detects an agent's DNS query for example.com"
else
  fail "control egress: the capture did NOT detect example.com; the network check cannot be trusted"
  exit 1
fi
if grep -rqF "$CANARY" "$KEYDIR"; then
  pass "control canary: the grep finds the canary in the proxy's key file"
else
  fail "control canary: the grep does not find the canary where it is; the credential check cannot be trusted"
  exit 1
fi

# --- per agent ---------------------------------------------------------------
# run_phase <agent> <phase> <ws> <scratch> <goal>: one adapter run, as a sandbox
# pod runs it.
run_phase() {
  local agent=$1 phase=$2 ws=$3 scratch=$4 goal=$5 out=$EVIDENCE/$1
  printf '%s' "$phase" > "$ws/.coxswain/desired-phase"
  local t0; t0=$(date +%s)
  # As in a sandbox pod: Planning sees the workspace read-only except .coxswain.
  local wsmount=(-v "$ws":/workspace)
  [ "$phase" = Planning ] && wsmount=(-v "$ws":/workspace:ro -v "$ws/.coxswain":/workspace/.coxswain)
  docker run --rm --net "container:$RUN-holder" --read-only -u 65532:65532 \
    --memory "$T2_MEMORY" --memory-swap "$T2_MEMORY" --cpus "$T2_CPUS" \
    --tmpfs /tmp:exec,uid=65532,gid=65532 -v "$scratch":/scratch "${wsmount[@]}" \
    -e HOME=/scratch -e COX_GOAL="$goal" -e COX_MODEL_BASE_URL=http://model-proxy:8080 -e COX_MODEL="$MODEL" \
    -e COX_WORKSPACE=/workspace -e GOTMPDIR=/workspace/.coxswain/gotmp "$PREFIX$agent:$TAG" \
    > "$out/$phase.stdout" 2> "$out/$phase.stderr"
  local rc=$?
  echo "exit=$rc seconds=$(( $(date +%s) - t0 ))$([ "$rc" = 137 ] && echo ' (137: killed, likely out of memory)')" > "$out/$phase.exit"
}

# result_ok <ws> <phase>: result.json claims success for the phase.
result_ok() {
  python3 -I - "$1/.coxswain/result.json" "$2" <<'EOF'
import json, sys
try:
    r = json.load(open(sys.argv[1]))
except Exception as e:
    print("no result.json:", e); sys.exit(1)
ok = r.get("status") == "success" and r.get("observedPhase") == sys.argv[2]
print("status=%s observedPhase=%s notes=%s" % (r.get("status"), r.get("observedPhase"), (r.get("verificationNotes") or "")[:200]))
sys.exit(0 if ok else 1)
EOF
}

for agent in "${AGENTS[@]}"; do
  IMG="$PREFIX$agent:$TAG"
  OUT=$EVIDENCE/$agent
  mkdir -p "$OUT"
  say "== $agent ($IMG)"
  if ! docker image inspect "$IMG" >/dev/null 2>&1; then
    fail "$agent: no image $IMG; nothing else run for it"
    continue
  fi

  # static
  user=$(docker image inspect -f '{{.Config.User}}' "$IMG")
  if [ "$user" = "65532:65532" ]; then
    pass "$agent static: image user $user"
  else
    fail "$agent static: image user '$user', want 65532:65532"
  fi
  st=$(docker run --rm --read-only --network none -u 65532:65532 --tmpfs /scratch:uid=65532,gid=65532 \
    --tmpfs /tmp:uid=65532,gid=65532 -e HOME=/scratch "$IMG" -selftest 2>&1 | tail -1)
  echo "$st" > "$OUT/selftest.json"
  if echo "$st" | python3 -I -c 'import json,sys; d=json.load(sys.stdin); sys.exit(0 if d.get("ok") is True and d.get("uid")==65532 else 1)' 2>/dev/null; then
    pass "$agent static: selftest $st"
  else
    fail "$agent static: selftest $st"
  fi

  # phases, under capture
  WS=$(mktemp -d); SCRATCH=$(mktemp -d)
  cp -r "$REPO/examples/gocli/." "$WS/"
  (cd "$WS" && git init -q && git add -A && git -c user.email=t2@coxswain -c user.name=t2 commit -qm base)
  mkdir -p "$WS/.coxswain"
  chmod -R a+rwX "$WS" "$SCRATCH"
  start_capture
  OWN=$(holder_ip)
  before=$(docker logs "$RUN-proxy" 2>/dev/null | grep -c '"source":"model-proxy"' || true)
  GOAL=${T2_GOAL:-"Add a file NOTES.md at the repository root containing exactly the line: hello from $agent"}
  run_phase "$agent" Planning "$WS" "$SCRATCH" "$GOAL"
  if r=$(result_ok "$WS" Planning) && [ -s "$WS/.coxswain/PLAN.md" ]; then
    pass "$agent phases: Planning claimed success and wrote PLAN.md ($(wc -c < "$WS/.coxswain/PLAN.md") bytes); $(cat "$OUT/Planning.exit")"
  else
    fail "$agent phases: Planning: $r; PLAN.md $( [ -s "$WS/.coxswain/PLAN.md" ] && echo present || echo missing); $(cat "$OUT/Planning.exit")"
  fi
  mid=$(docker logs "$RUN-proxy" 2>/dev/null | grep -c '"source":"model-proxy"' || true)
  run_phase "$agent" Implementing "$WS" "$SCRATCH" "$GOAL"
  committed=$(cd "$WS" && git log -1 --name-only --format= 2>/dev/null | tr '\n' ' ')
  if r=$(result_ok "$WS" Implementing) && echo " $committed" | grep -qF " $T2_EXPECT "; then
    pass "$agent phases: Implementing claimed success and committed: $committed; $(cat "$OUT/Implementing.exit")"
  else
    fail "$agent phases: Implementing: $r; HEAD files: '$committed'; $(cat "$OUT/Implementing.exit")"
  fi
  stop_capture "$OUT/network.tcpdump"
  scratch_kib=$(docker run --rm -u 0 -v "$SCRATCH":/s:ro alpine:3.22 du -sk /s | cut -f1)
  say "INFO $agent scratch: $((scratch_kib / 1024)) MiB used (the pod's /scratch emptyDir limit is 350Mi)"
  if [ "$T2_EXPECT" = round.go ]; then
    # The real task: the committed result must pass the repo's own tests.
    if docker run --rm -v "$WS":/w -w /w golang:1.26-trixie sh -c 'git config --global --add safe.directory "*"; git clone -q /w /tmp/c && cd /tmp/c && go test ./... 2>&1 | tail -3' > "$OUT/go-test.txt" 2>&1 && grep -q '^ok' "$OUT/go-test.txt" && ! grep -q FAIL "$OUT/go-test.txt"; then
      pass "$agent task: go test ./... passes on the committed result ($(head -1 "$OUT/go-test.txt"))"
    else
      fail "$agent task: go test ./... on the committed result: $(tr '\n' ' ' < "$OUT/go-test.txt")"
    fi
  fi
  after=$(docker logs "$RUN-proxy" 2>/dev/null | grep -c '"source":"model-proxy"' || true)

  # network
  bad=$(bad_traffic "$OUT/network.tcpdump" "$OWN")
  toproxy=$(grep ' Out ' "$OUT/network.tcpdump" | grep -F "IP $OWN." | grep -cF "> $PROXY_IP." || true)
  if [ -z "$bad" ] && [ "$toproxy" -gt 0 ]; then
    pass "$agent network: $toproxy outbound packets, all to the proxy; no other DNS name (tcpdump: $OUT/network.tcpdump)"
  elif [ "$toproxy" -eq 0 ]; then
    fail "$agent network: no packets to the proxy at all; the capture proves nothing"
  else
    echo "$bad" > "$OUT/network.bad"
    fail "$agent network: traffic other than the proxy ($(echo "$bad" | wc -l) lines, $OUT/network.bad): $(echo "$bad" | head -3 | tr '\n' ';')"
  fi

  # creds
  hits=$(docker run --rm -u 0 -v "$WS":/w:ro -v "$SCRATCH":/s:ro alpine:3.22 grep -rlF "$CANARY" /w /s 2>/dev/null; grep -lF "$CANARY" "$OUT"/*.stdout "$OUT"/*.stderr 2>/dev/null)
  if [ -z "$hits" ]; then
    pass "$agent creds: the canary key is in none of the workspace, /scratch, or the agent's output"
  else
    fail "$agent creds: the canary key found in: $hits"
  fi

  # proxy audit
  if [ "$((mid - before))" -ge 1 ] && [ "$((after - mid))" -ge 1 ]; then
    pass "$agent proxy: audit lines Planning=$((mid - before)) Implementing=$((after - mid))"
  else
    fail "$agent proxy: audit lines Planning=$((mid - before)) Implementing=$((after - mid)); want at least 1 each"
  fi

  (cd "$WS" && git log --stat -3 > "$OUT/git-log.txt" 2>&1; cp -r .coxswain "$OUT/coxswain" 2>/dev/null)
  rootrm "$WS"; rootrm "$SCRATCH"
done

docker logs "$RUN-proxy" > "$EVIDENCE/proxy/proxy.log" 2>&1
say "== done: $FAILS failure(s); evidence $EVIDENCE"
[ "$FAILS" -eq 0 ]
