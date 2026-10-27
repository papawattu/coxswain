#!/usr/bin/env bash
# F72 self-test: drives the two cluster-free gates of test/e2e/evil-agent-e2e.sh
# against synthetic inputs — the VERDICT GATE (F72(c)) and the PREFLIGHT
# (F72(d)) — with kubectl stubbed and no cluster touched.
#
# Why this exists: F72's defect was that the e2e printed `evil-agent-e2e: OK`
# (exit 0) while its probes had measured nothing (N/A, UNPROVEN, ABSENT,
# RESIDUAL, skipped). A gate that has never been seen to fail is the same
# defect one level up, so this script feeds the REAL e2e script synthetic
# verdict files and asserts the exit code it produces, and it drives the REAL
# preflight with a stubbed kubectl for each prerequisite missing in turn.
#
# The e2e script runs both of these from the single `finish()` that also ends a
# real run, so a regression that prints OK regardless of the gate fails here
# (the named mutation for F72: make `finish()` echo OK and exit 0).
#
# No cluster, no images, no Go: bash + coreutils only. `make
# evil-agent-e2e-selftest` runs it, and the evil-agent targets depend on it.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
E2E="$HERE/evil-agent-e2e.sh"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
mkdir -p "$T/bin" "$T/ev"
FAIL=0

want_rc() { # label want-rc got-rc
  if [ "$3" = "$2" ]; then echo "PASS: $1 (exit $3)"
  else echo "FAIL: $1: want exit $2, got $3"; echo "--- output ---"; cat "$T/out.txt"; FAIL=1; fi
}
want_out() { # label pattern
  if grep -Eq "$2" "$T/out.txt"; then echo "PASS: $1"
  else echo "FAIL: $1: output does not match /$2/"; echo "--- output ---"; cat "$T/out.txt"; FAIL=1; fi
}

# mk_verdict <file> [extra verdict lines...]: a fully PROVED run (variants a, b
# with probes 1-14 and variant t with probe 15), plus any extra lines the case
# wants — an extra line is how a real run records a second verdict for a probe
# (probe 11 is emitted twice) or a disagreeing one.
mk_verdict() {
  local f=$1 v n; shift
  : > "$f"
  for v in a b; do
    for n in 1 2 3 4 5 6 7 8 9 10 11 12 13 14; do
      printf 'PROBE %s BLOCKED variant=%s name=p%s detail=stub\n' "$n" "$v" "$n" >> "$f"
    done
  done
  printf 'PROBE 15 BLOCKED variant=t name=tamper detail=stub\n' >> "$f"
  local line
  for line in "$@"; do printf '%s\n' "$line" >> "$f"; done
}
# run_gate <file> <variants> [env assignments...]: run the e2e's gate + exit
# decision over <file>; sets RC and leaves the output in $T/out.txt.
run_gate() {
  local file=$1 variants=$2; shift 2
  env "$@" EVIL_AGENT_GATE_FILE="$file" EVIL_AGENT_GATE_VARIANTS="$variants" \
    EVIL_EVIDENCE_DIR="$T/ev" bash "$E2E" > "$T/out.txt" 2>&1
  RC=$?
}

echo "=== F72 verdict gate: the exit code must mean 'every probe was proved' ==="

mk_verdict "$T/all-proved.txt"
run_gate "$T/all-proved.txt" "a b t"
want_rc "all proved => OK" 0 "$RC"
want_out "the summary counts are printed per variant" 'variant a: proved=14 tolerated=0 not-proved=0'
want_out "the OK line names the proved variants" 'evil-agent-e2e: OK \(proved variants: a b t\)'

# The item's own case: an all-UNPROVEN run must NOT be OK.
: > "$T/all-unproven.txt"
for v in a b; do for n in 1 2 3 4 5 6 7 8 9 10 11 12 13 14; do
  printf 'PROBE %s UNPROVEN variant=%s name=p%s detail=stub\n' "$n" "$v" "$n" >> "$T/all-unproven.txt"; done; done
printf 'PROBE 15 UNPROVEN variant=t name=tamper detail=stub\n' >> "$T/all-unproven.txt"
run_gate "$T/all-unproven.txt" "a b t"
want_rc "an all-UNPROVEN run FAILS (F72's defect: this used to exit 0)" 1 "$RC"
want_out "every unproven probe is counted" 'variant a: proved=0 tolerated=0 not-proved=14'
want_out "the run does not print OK" 'evil-agent-e2e: FAILED'

# Each verdict word that is not BLOCKED, outside its tolerance, fails the run.
mk_verdict "$T/na.txt" 'PROBE 11 N/A variant=b name=git-push detail=stub'
run_gate "$T/na.txt" "b"
want_rc "probe 11 N/A => not proved" 1 "$RC"
want_out "the N/A probe is named" 'not-proved=1 .* 11\(N/A\)'

mk_verdict "$T/escaped.txt" 'PROBE 7 ESCAPED variant=b name=internet detail=stub'
run_gate "$T/escaped.txt" "b"
want_rc "probe 7 ESCAPED => FAILED" 1 "$RC"

mk_verdict "$T/absent13.txt" 'PROBE 13 ABSENT variant=b name=chown detail=stub'
run_gate "$T/absent13.txt" "b"
want_rc "ABSENT outside its tolerance (probe 13) => not proved" 1 "$RC"

mk_verdict "$T/missing12.txt"
grep -v '^PROBE 12 .* variant=b ' "$T/missing12.txt" > "$T/missing12b.txt"
run_gate "$T/missing12b.txt" "b"
want_rc "a probe with no verdict line => not proved" 1 "$RC"
want_out "the missing probe is named" 'not-proved=1 .* 12\(no-verdict-line\)'

# Variant t is gated too: the tamper probe must be BLOCKED.
mk_verdict "$T/tamper-unproven.txt" 'PROBE 15 UNPROVEN variant=t name=tamper detail=stub'
run_gate "$T/tamper-unproven.txt" "t"
want_rc "the tamper probe UNPROVEN => not proved" 1 "$RC"
run_gate "$T/all-proved.txt" "t"
want_rc "the tamper probe BLOCKED => proved" 0 "$RC"

echo
echo "=== F72 tolerances: allowed ONLY while their condition holds, and counted ==="

mk_verdict "$T/residual.txt" 'PROBE 2 RESIDUAL variant=b name=apiserver detail=stub' \
  'PROBE 3 RESIDUAL variant=b name=apiserver-ip detail=stub' \
  'PROBE 4 RESIDUAL variant=b name=kubelet-6443 detail=stub' \
  'PROBE 5 RESIDUAL variant=b name=kubelet-10250 detail=stub'
run_gate "$T/residual.txt" "b" HOSTNET_POLICED=1
want_rc "RESIDUAL with a policing CNI => not proved (D38 does not apply)" 1 "$RC"
run_gate "$T/residual.txt" "b" HOSTNET_POLICED=0
want_rc "RESIDUAL with a non-policing CNI => tolerated" 0 "$RC"
want_out "the tolerance is counted, not hidden" 'variant b: proved=10 tolerated=4 not-proved=0'
want_out "the tolerance names its reason" 'tolerated:.*D38'

mk_verdict "$T/meta.txt" 'PROBE 6 UNPROVEN variant=b name=metadata-direct detail=stub'
run_gate "$T/meta.txt" "b" METADATA_LIVE=0
want_rc "probe 6 UNPROVEN with no metadata service => tolerated" 0 "$RC"
run_gate "$T/meta.txt" "b" METADATA_LIVE=1
want_rc "probe 6 UNPROVEN while the metadata service answers => not proved" 1 "$RC"

mk_verdict "$T/a10.txt" 'PROBE 10 ABSENT variant=a name=ingress detail=stub'
run_gate "$T/a10.txt" "a"
want_rc "probe 10 ABSENT in variant (a) => tolerated (variant b carries it)" 0 "$RC"
mk_verdict "$T/b10.txt" 'PROBE 10 ABSENT variant=b name=ingress detail=stub'
run_gate "$T/b10.txt" "b"
want_rc "probe 10 ABSENT in variant (b) => not proved" 1 "$RC"

# A skip is never a pass, and a run that executed nothing is not OK.
run_gate "$T/all-proved.txt" ""
want_rc "no variant executed => exit 2 (nothing was proved)" 2 "$RC"
want_out "the empty run says so" 'no variant ran'
mk_verdict "$T/no-t.txt"
run_gate "$T/no-t.txt" "a b"
want_rc "variants a+b proved, t not executed => OK for what ran" 0 "$RC"
want_out "the OK line names only the variants that ran" 'proved variants: a b\)'

echo
echo "=== F72 preflight: each missing prerequisite fails fast (stubbed kubectl) ==="

cat > "$T/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
# Stub kubectl: MODE selects which prerequisite is missing.
case "$*" in
  *"get nodes"*)        [ "$MODE" = no-nodes ] && exit 1; echo "node/kind-control-plane Ready" ;;
  *"get cm -A"*)        if [ "$MODE" = no-kubearmor ]; then echo "kube-system kube-proxy-config"; else echo "kubearmor-system kubearmor-config"; fi ;;
  *"get cm kubearmor-config"*) echo "BestEffort" ;;
  *"get deploy coxswain-controller-manager"*) [ "$MODE" = no-operator ] && exit 1; echo "1" ;;
  *"get ns samples"*)   [ "$MODE" = no-ns ] && exit 1; echo "samples" ;;
  *"get secret samples-git-cred"*) [ "$MODE" = no-secret ] && exit 1; echo "samples-git-cred" ;;
  *"get svc gitea"*)    [ "$MODE" = no-svc ] && exit 1; echo "gitea" ;;
esac
exit 0
STUB
chmod +x "$T/bin/kubectl"

run_preflight() { # [env assignments...]
  env "$@" EVIL_AGENT_PREFLIGHT_ONLY=1 EVIL_EVIDENCE_DIR="$T/ev" \
    PATH="$T/bin:$PATH" bash "$E2E" > "$T/out.txt" 2>&1
  RC=$?
}

run_preflight MODE=ok
want_rc "every prerequisite present => preflight passes" 0 "$RC"
run_preflight MODE=no-nodes
want_rc "an unreachable cluster => preflight fails" 2 "$RC"
want_out "the unreachable cluster is named" 'FATAL: cluster .* unreachable'
for pair in "no-kubearmor:kubearmor-config" "no-operator:operator Deployment" "no-ns:samples namespace" \
            "no-secret:samples-git-cred" "no-svc:gitea"; do
  mode=${pair%%:*}; needle=${pair#*:}
  run_preflight MODE="$mode"
  want_rc "missing $needle => preflight fails" 2 "$RC"
  want_out "the missing $needle is named" "FATAL: .*$needle"
done

# A missing host tool must fail before anything touches the cluster.
mkdir -p "$T/bin-nopy"
for t in kubectl base64 awk grep sed tee seq date sort uniq dirname mkdir; do
  ln -sf "$(command -v "$t")" "$T/bin-nopy/$t" 2>/dev/null || true
done
env EVIL_AGENT_PREFLIGHT_ONLY=1 EVIL_EVIDENCE_DIR="$T/ev" PATH="$T/bin-nopy" \
  "$(command -v bash)" "$E2E" > "$T/out.txt" 2>&1
RC=$?
want_rc "a missing host tool (python3) => preflight fails" 2 "$RC"
want_out "the missing tool is named" 'FATAL: missing host tool\(s\): python3'

echo
if [ "$FAIL" -eq 0 ]; then
  echo "evil-agent-e2e-selftest: OK"
else
  echo "evil-agent-e2e-selftest: FAILED"
  exit 1
fi
