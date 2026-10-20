#!/usr/bin/env bash
# run-envtest.sh: run the internal/controller envtest suite and ALWAYS clean up
# the etcd + kube-apiserver orphans it leaves when the suite times out or is
# interrupted.
#
# WHY THIS EXISTS (R29 / P5a reviewer): `go test` with the default 600s
# timeout KILLS the test binary mid-suite when the suite exceeds 600s.
# envtest's DeferCleanup never runs (the binary was killed, not exited), so
# the etcd + kube-apiserver (children of the binary) become ppid=1 orphans
# under bin/k8s/... Each timed-out run leaves a pair; the orphans hold ~1-2GiB
# each and exhaust available memory. The pipe (tee|grep|tail) is NOT the
# leak — the 600s timeout killing the binary is.
#
# This wrapper:
#   1. Runs the suite (via `go test`, with an explicit -timeout so the caller
#      controls how long it may run).
#   2. On exit (success, failure, timeout, or Ctrl-C), reaps ANY ppid=1
#      etcd/kube-apiserver orphans under the project's bin/k8s.
#
# Usage:
#   hack/run-envtest.sh                       # default: -timeout 900s, full suite
#   hack/run-envtest.sh 1800s                 # custom -timeout
#   hack/run-envtest.sh 900s -ginkgo.focus "P5a"   # custom args after the timeout
#
# NEVER kill by name (pkill -f): the orphans are reaped by PID after verifying
# ppid=1 AND a bin/k8s cmdline immediately before the kill (PID-reuse safe).
# shellcheck disable=SC2317 # reap_orphans is invoked by the EXIT trap below

set -u

# The project's bin/k8s dir (where envtest's etcd/apiserver live).
BIN_K8S_DIR="$(cd "$(dirname "$0")/.." && pwd)/bin/k8s"

# Default timeout if none given.
TIMEOUT="900s"
ARGS=()
if [ "$#" -gt 0 ]; then
  # First arg is the timeout (a Go duration like 900s / 15m); the rest are
  # pass-through go test args.
  TIMEOUT="$1"
  shift
  ARGS=("$@")
fi

# reap_orphans: SIGKILL any ppid=1 etcd/kube-apiserver under BIN_K8S_DIR.
# Re-verifies ppid=1 AND the bin/k8s cmdline immediately before the kill
# (defence against PID reuse between the check and the kill). Never pkill -f.
reap_orphans() {
  local killed=0 pid ppid cmd
  for pid in /proc/[0-9]*; do
    pid="${pid#/proc/}"
    [ -d "/proc/$pid" ] || continue
    ppid=$(awk '/^PPid:/{print $2}' "/proc/$pid/status" 2>/dev/null)
    [ "$ppid" = "1" ] || continue
    cmd=$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null | head -c 200)
    case "$cmd" in
      *"$BIN_K8S_DIR"/*)
        # Re-verify ppid=1 + bin/k8s IMMEDIATELY before the kill.
        ppid2=$(awk '/^PPid:/{print $2}' "/proc/$pid/status" 2>/dev/null)
        cmd2=$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null | head -c 200)
        if [ "$ppid2" = "1" ] && case "$cmd2" in *"$BIN_K8S_DIR"/*) true;; *) false;; esac; then
          kill -9 "$pid" 2>/dev/null && { echo "  reaped orphan $pid ($(echo "$cmd2" | head -c 60))"; killed=$((killed+1)); }
        fi
        ;;
    esac
  done
  if [ "$killed" -gt 0 ]; then
    echo "reaped $killed envtest orphan(s) under $BIN_K8S_DIR"
  else
    echo "no envtest orphans to reap"
  fi
}

# trap: reap on ANY exit (success, failure, timeout, signal).
trap reap_orphans EXIT INT TERM

echo "=== run-envtest: go test ./internal/controller/ -timeout $TIMEOUT ${ARGS[*]+"${ARGS[*]}"} ==="
echo "    (BIN_K8S_DIR=$BIN_K8S_DIR; orphans reaped on exit)"
# Run the suite. The -timeout lets the caller bound it; the wrapper reaps
# orphans regardless of how it ends.
go test ./internal/controller/ -count=1 -timeout "$TIMEOUT" "${ARGS[@]+"${ARGS[@]}"}"
STATUS=$?
echo "=== run-envtest: go test exited $STATUS; reaping orphans ==="
exit "$STATUS"
