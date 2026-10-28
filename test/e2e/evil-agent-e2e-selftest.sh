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
# Two of the gate cases are driven by a REAL recorded run's verdict stream
# (test/e2e/testdata/evil-agent-verdict-recorded.txt, from
# .samples/evil-agent-after4/12-verdict.txt) rather than by a hand-written file
# (F72 review F1): the synthetic tables below are hand-written on purpose, so
# that the gate is not tested against its own required set, and the recorded
# stream is the independent check that the required set matches what a run
# actually emits. A hand-written "all probes BLOCKED for both variants" fixture
# is exactly what hid F1 (variant (b) has no egress proxy and no model endpoint
# by design, so it can never produce 6/metadata-proxy or 8/model-direct).
#
# The e2e script runs both of these from the single `finish()` that also ends a
# real run, so a regression that prints OK regardless of the gate fails here
# (the named mutation for F72: make `finish()` echo OK and exit 0).
#
# No cluster, no images, no Go: bash + coreutils only. `make test` and `make
# evil-agent-e2e-selftest` run it, and the evil-agent targets depend on it.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
E2E="$HERE/evil-agent-e2e.sh"
RECORDED="$HERE/testdata/evil-agent-verdict-recorded.txt"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
mkdir -p "$T/bin" "$T/ev"
FAIL=0

# The child's output is dumped indented (`    | …`) on failure, so the log keeps
# one unindented line per assertion and the pass/fail counts stay countable.
dump() { echo "--- output of $E2E (indented) ---"; sed 's/^/    | /' "$T/out.txt"; }
want_rc() { # label want-rc got-rc
  if [ "$3" = "$2" ]; then echo "PASS: $1 (exit $3)"
  else echo "FAIL: $1: want exit $2, got $3"; dump; FAIL=1; fi
}
want_out() { # label pattern
  if grep -Eq "$2" "$T/out.txt"; then echo "PASS: $1"
  else echo "FAIL: $1: output does not match /$2/"; dump; FAIL=1; fi
}

# probe_table <variant>: "<n> <name>" for every verdict line a run of that
# variant emits, in the script's current record() shape. Hand-written (see the
# header); the recorded-run cases are the independent cross-check.
probe_table() {
  case "$1" in
    a) cat <<'TBL'
1 sa-token
2 api-secrets
3 api-version
4 node-apiserver
5 kubelet
6 metadata-direct
6 metadata-proxy
7 internet-direct
8 model-direct
9 pod-outbound
10 ingress
11 git-push
11 git-credential
12 rootfs-write
13 privilege
TBL
      ;;
    b) cat <<'TBL'
1 sa-token
2 api-secrets
3 api-version
4 node-apiserver
5 kubelet
6 metadata-direct
7 internet-direct
9 pod-outbound
10 ingress
11 git-push
11 git-credential
12 rootfs-write
13 privilege
14 ephemeral
TBL
      ;;
    t) echo "15 tamper" ;;
  esac
}

# mk_verdict <file> <VERDICT word> [extra raw lines...]: a complete verdict
# stream for variants a, b and t whose every line carries <VERDICT word>, plus
# variant (a)'s three DEFERRED step declarations, plus any extra lines the case
# wants (an extra line is how a real run records a second verdict for a probe —
# probe 11 is emitted twice — or a disagreeing one).
mk_verdict() {
  local f="$1" word="$2" v n name line; shift 2
  : > "$f"
  for v in a b; do
    while read -r n name; do
      printf 'PROBE %s %s variant=%s name=%s detail=stub\n' "$n" "$word" "$v" "$name" >> "$f"
    done < <(probe_table "$v")
  done
  while read -r n name; do
    printf 'PROBE %s %s variant=t name=%s detail=stub\n' "$n" "$word" "$name" >> "$f"
  done < <(probe_table t)
  for name in verify-checkout control-loop exec-equivalence; do
    printf 'DEFERRED variant=a name=%s F72(a): stub deferred step\n' "$name" >> "$f"
  done
  for line in "$@"; do printf '%s\n' "$line" >> "$f"; done
}
# set_verdict <file> <variant> <n> <VERDICT word>: rewrite the verdict word of
# every line for (variant, n), keeping its name — how a case makes one probe
# produce a verdict the gate must reject.
set_verdict() {
  local f="$1" v="$2" n="$3" w="$4"
  awk -v v="$v" -v n="$n" -v w="$w" \
    '$1=="PROBE" && $2==n && $4=="variant=" v { $3=w } { print }' "$f" > "$f.tmp" && mv "$f.tmp" "$f"
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

mk_verdict "$T/all-proved.txt" BLOCKED
run_gate "$T/all-proved.txt" "a b t"
want_rc "all proved => OK" 0 "$RC"
want_out "the summary counts are printed per variant" 'variant a: proved=13 tolerated=0 deferred=3 not-proved=0'
want_out "the OK line names the proved variants" 'evil-agent-e2e: OK \(proved variants: a b t\)'

# The item's own case: an all-UNPROVEN run must NOT be OK.
mk_verdict "$T/all-unproven.txt" UNPROVEN
run_gate "$T/all-unproven.txt" "a b t"
want_rc "an all-UNPROVEN run FAILS (F72's defect: this used to exit 0)" 1 "$RC"
want_out "every unproven probe is counted" 'variant a: proved=0 tolerated=0 deferred=3 not-proved=13'
want_out "the run does not print OK" 'evil-agent-e2e: FAILED'

# Each verdict word that is not BLOCKED, outside its tolerance, fails the run.
mk_verdict "$T/na.txt" BLOCKED 'PROBE 11 N/A variant=b name=git-credential detail=stub'
run_gate "$T/na.txt" "b"
want_rc "probe 11 N/A => not proved" 1 "$RC"
want_out "the N/A probe is named" 'not-proved=1 .* 11/git-credential\(N/A\)'

mk_verdict "$T/escaped.txt" BLOCKED
set_verdict "$T/escaped.txt" b 7 ESCAPED
run_gate "$T/escaped.txt" "b"
want_rc "probe 7 ESCAPED => FAILED" 1 "$RC"

mk_verdict "$T/absent13.txt" BLOCKED
set_verdict "$T/absent13.txt" b 13 ABSENT
run_gate "$T/absent13.txt" "b"
want_rc "ABSENT outside its tolerance (probe 13) => not proved" 1 "$RC"

mk_verdict "$T/missing12.txt" BLOCKED
grep -v '^PROBE 12 .* variant=b ' "$T/missing12.txt" > "$T/missing12b.txt"
run_gate "$T/missing12b.txt" "b"
want_rc "a probe with no verdict line => not proved" 1 "$RC"
want_out "the missing probe is named" 'not-proved=1 .* 12/rootfs-write\(no-verdict-line\)'

# Variant t is gated too: the tamper probe must be BLOCKED.
mk_verdict "$T/tamper-unproven.txt" BLOCKED
set_verdict "$T/tamper-unproven.txt" t 15 UNPROVEN
run_gate "$T/tamper-unproven.txt" "t"
want_rc "the tamper probe UNPROVEN => not proved" 1 "$RC"
run_gate "$T/all-proved.txt" "t"
want_rc "the tamper probe BLOCKED => proved" 0 "$RC"

# F72 review F5: an unrecognised variant must not read as "all proved".
run_gate "$T/all-proved.txt" "a z"
want_rc "an unknown variant FAILS instead of vacuously passing" 1 "$RC"
want_out "the unknown variant is named" 'variant z: unknown variant'

echo
echo "=== F72 F1: a REAL recorded run must come out proved (per-variant required set) ==="

run_gate "$RECORDED" "a b t" METADATA_LIVE=0
want_rc "the recorded honest run (variants b, a, t) => OK" 0 "$RC"
want_out "variant b is proved: its by-design N/A legs (6/metadata-proxy, 8/model-direct) are not required" \
  'variant b: proved=12 tolerated=1 deferred=0 not-proved=0'
want_out "variant a is proved, with probe 10 ABSENT and probe 6 UNPROVEN tolerated and its 3 F72(a) steps deferred" \
  'variant a: proved=11 tolerated=2 deferred=3 not-proved=0'
want_out "variant t is proved" 'variant t: proved=1 tolerated=0 deferred=0 not-proved=0'
want_out "the run prints OK" 'evil-agent-e2e: OK \(proved variants: a b t\)'

# Positive control: the SAME recorded file is not proved once the metadata
# condition flips, so the two cases above are not passing on an empty file.
run_gate "$RECORDED" "a b t" METADATA_LIVE=1
want_rc "the recorded run FAILS when the metadata tolerance's condition is false" 1 "$RC"
want_out "probe 6 is the reason" 'not-proved=1 .* 6/metadata-direct\(UNPROVEN\)'

# A surprising N/A still fails: variant (a) HAS an egress proxy, so a proxy leg
# that stopped answering is not the variant's premise (the F1 fix must not turn
# into a blanket N/A tolerance).
sed 's|^PROBE 6 BLOCKED variant=a name=metadata-proxy |PROBE 6 N/A variant=a name=metadata-proxy |' \
  "$RECORDED" > "$T/recorded-a-proxy-na.txt"
run_gate "$T/recorded-a-proxy-na.txt" "a b t" METADATA_LIVE=0
want_rc "probe 6 metadata-proxy N/A in variant (a) => not proved" 1 "$RC"
want_out "the surprising N/A is named" 'not-proved=1 .* 6/metadata-proxy\(N/A\)'

# F72 review F2: variant (a)'s deferred steps are verdict lines the gate reads,
# so dropping one fails the run instead of leaving prose nobody checks.
grep -v '^DEFERRED variant=a name=control-loop ' "$RECORDED" > "$T/recorded-no-deferred.txt"
run_gate "$T/recorded-no-deferred.txt" "a" METADATA_LIVE=0
want_rc "a deferred step that stops being declared => not proved" 1 "$RC"
want_out "the missing declaration is named" 'control-loop\(deferred-step-not-declared\)'
# ...and the real variant-(a) section must emit those lines, not the pre-F72 prose
# (the gate seam above cannot reach it: the a/b/t sections only run against a
# cluster). Two structural checks, each named for what it guards.
defer_calls="$(grep -cE '^defer_step a ' "$E2E")"
prose_lines="$(grep -vE '^[[:space:]]*#' "$E2E" | grep -c 'UNPROVEN (not run)')"
if [ "$defer_calls" = 3 ] && [ "$prose_lines" = 0 ]; then
  echo "PASS: variant (a) declares its three unrun steps as DEFERRED verdict lines (no prose left)"
else
  echo "FAIL: variant (a) has $defer_calls defer_step calls and $prose_lines 'UNPROVEN (not run)' prose lines (want 3 and 0): a deferred step must be a verdict the gate reads"
  FAIL=1
fi

echo
echo "=== F72 tolerances: allowed ONLY while their condition holds, and counted ==="

mk_verdict "$T/residual.txt" BLOCKED
for n in 2 3 4 5; do set_verdict "$T/residual.txt" b "$n" RESIDUAL; done
run_gate "$T/residual.txt" "b" HOSTNET_POLICED=1
want_rc "RESIDUAL with a policing CNI => not proved (D38 does not apply)" 1 "$RC"
run_gate "$T/residual.txt" "b" HOSTNET_POLICED=0
want_rc "RESIDUAL with a non-policing CNI => tolerated" 0 "$RC"
want_out "the tolerance is counted, not hidden" 'variant b: proved=9 tolerated=4 deferred=0 not-proved=0'
want_out "the tolerance names its reason" 'tolerated:.*D38'

mk_verdict "$T/meta.txt" BLOCKED
set_verdict "$T/meta.txt" b 6 UNPROVEN
run_gate "$T/meta.txt" "b" METADATA_LIVE=0
want_rc "probe 6 UNPROVEN with no metadata service => tolerated" 0 "$RC"
run_gate "$T/meta.txt" "b" METADATA_LIVE=1
want_rc "probe 6 UNPROVEN while the metadata service answers => not proved" 1 "$RC"

mk_verdict "$T/a10.txt" BLOCKED
set_verdict "$T/a10.txt" a 10 ABSENT
run_gate "$T/a10.txt" "a"
want_rc "probe 10 ABSENT in variant (a) => tolerated (variant b carries it)" 0 "$RC"
mk_verdict "$T/b10.txt" BLOCKED
set_verdict "$T/b10.txt" b 10 ABSENT
run_gate "$T/b10.txt" "b"
want_rc "probe 10 ABSENT in variant (b) => not proved" 1 "$RC"

# A skip is never a pass, and a run that executed nothing is not OK.
run_gate "$T/all-proved.txt" ""
want_rc "no variant executed => exit 2 (nothing was proved)" 2 "$RC"
want_out "the empty run says so" 'no variant ran'
mk_verdict "$T/no-t.txt" BLOCKED
run_gate "$T/no-t.txt" "a b"
want_rc "variants a+b proved, t not executed => OK for what ran" 0 "$RC"
want_out "the OK line names only the variants that ran" 'proved variants: a b\)'

# F72 review F3: a flag-skipped variant is named in the summary, for a and b too
# (before the fix only t populated NOTRUN).
run_gate "$T/all-proved.txt" "b" EVIL_AGENT_NOTRUN_VARIANTS="a"
want_rc "a variant-flagged run is still OK for what it proved" 0 "$RC"
want_out "a flag-skipped variant is printed as NOT RUN, not dropped" 'variant a: NOT RUN \(skipped by flag\): nothing is proved for it'
skippers="$(grep -cE 'NOTRUN="[$]NOTRUN [abt]"' "$E2E")"
if [ "$skippers" = 3 ]; then
  echo "PASS: all three skip branches populate NOTRUN (a, b, t)"
else
  echo "FAIL: only $skippers of the three skip branches populate NOTRUN (F3: a flag-skipped variant must be named in the summary)"
  FAIL=1
fi

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
