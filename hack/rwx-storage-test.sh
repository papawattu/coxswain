#!/usr/bin/env bash
# F21 execution test: run the REAL hack/rwx-storage.sh against a stubbed
# kubectl (a fake on PATH that records argv + stdin and answers the one query
# the script makes). Asserts: it needs an explicit context; kubeconfig/context
# flags reach every kubectl call; the applied manifest is a pinned second
# provisioner with its own namespace/provisioner name/ClusterRole in
# sharedFileSystemPath mode plus the StorageClass; a re-run is idempotent
# (same manifest, apply only, no create/delete); and an existing StorageClass
# with another provisioner stops the script before anything is applied.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/bin"
cat > "$T/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "$STUB_DIR/argv.log"
case " $* " in
*" get storageclass "*) [ -z "${STUB_SC_PROVISIONER:-}" ] || printf '%s %s\n' "$STUB_SC_PROVISIONER" "${STUB_SC_MODE:-Immediate}"; exit 0 ;;
*" delete storageclass "*) exit 0 ;;
*" apply -f - "*) cat >> "$STUB_DIR/applied.yaml"; echo "---RUN-END---" >> "$STUB_DIR/applied.yaml"; exit 0 ;;
*" rollout status "*) exit 0 ;;
esac
echo "unexpected kubectl call: $*" >&2; exit 99
STUB
chmod +x "$T/bin/kubectl"
export STUB_DIR="$T" KUBECTL="$T/bin/kubectl"
fail() { echo "FAIL: $*" >&2; exit 1; }
has() { grep -Fq -- "$2" "$1" || fail "$3 (missing: $2)"; }

# 0. no context -> refuses (never the ambient context).
if "$ROOT/hack/rwx-storage.sh" >/dev/null 2>&1; then fail "ran without --context/--kubeconfig"; fi

# 1. first run.
"$ROOT/hack/rwx-storage.sh" --context ctx1 --kubeconfig /tmp/kc >"$T/out1" 2>&1 || { cat "$T/out1"; fail "run 1 failed"; }
while read -r line; do
	case "$line" in
	"--kubeconfig /tmp/kc --context ctx1 "*) ;;
	*) fail "a kubectl call lacked the --kubeconfig/--context flags: $line" ;;
	esac
done < "$T/argv.log"
A="$T/applied.yaml"
has "$A" "image: docker.io/rancher/local-path-provisioner:v0.0.31" "provisioner image not pinned"
has "$A" "--provisioner-name=coxswain.io/local-path-shared" "own provisioner name"
has "$A" '{"sharedFileSystemPath": "/var/local-path-shared"}' "sharedFileSystemPath mode"
has "$A" "name: coxswain-rwx-storage" "own namespace"
has "$A" "kind: ClusterRole" "own ClusterRole"
has "$A" "name: coxswain-rwx-provisioner" "own ClusterRole name"
has "$A" "kind: StorageClass" "StorageClass"
has "$A" "name: local-shared" "StorageClass name"
has "$A" "provisioner: coxswain.io/local-path-shared" "StorageClass provisioner"
has "$A" "volumeBindingMode: Immediate" "binding mode (F24: WaitForFirstConsumer deadlocks the operator's memory PVC)"
if grep -Fq "WaitForFirstConsumer" "$A"; then fail "StorageClass is WaitForFirstConsumer"; fi
has "$A" "coxswain.io/config-sum:" "pod template must carry the config checksum (config changes must roll the provisioner)"
has "$A" "setup: |-" "setup script key (every provision needs it)"
has "$A" "teardown: |-" "teardown script key"
# shellcheck disable=SC2016  # literal $VOL_DIR is the point
has "$A" 'mkdir -m 0777 -p "$VOL_DIR"' "setup body"
# shellcheck disable=SC2016
has "$A" 'rm -rf "$VOL_DIR"' "teardown body"
has "$A" "image: docker.io/library/busybox:1.36" "helper image not pinned"
has "$A" "--helper-image=docker.io/library/busybox:1.36" "helper image flag"
has "$A" "automountServiceAccountToken: false" "helper pod must not mount an SA token"
# the script mounts nothing itself (the every-node mount is a cluster property)
if grep -Eq 'hostPath|volumes:.*hostPath' "$A"; then fail "the script's manifest mounts a host path itself"; fi
# never touches the cluster's own provisioner / default class
if grep -Eq 'local-path-storage|rancher.io/local-path$|is-default-class' "$A"; then fail "touches the cluster's own local-path provisioner or default class"; fi
if grep -Eq ' (create|delete|patch|replace) ' "$T/argv.log"; then fail "used a non-apply verb"; fi
has "$T/argv.log" "rollout status deployment/coxswain-rwx-provisioner" "no rollout wait"

# 2. re-run (the StorageClass now exists with OUR provisioner): identical manifest.
STUB_SC_PROVISIONER="coxswain.io/local-path-shared" "$ROOT/hack/rwx-storage.sh" --context ctx1 --kubeconfig /tmp/kc >/dev/null 2>&1 || fail "run 2 failed"
python3 - "$A" <<'PY' || fail "re-run applied a different manifest"
import sys
runs = [r for r in open(sys.argv[1]).read().split("---RUN-END---\n") if r.strip()]
assert len(runs) == 2, len(runs)
assert runs[0] == runs[1], "manifests differ"
PY

# 2b. F24: an existing class of OURS that is WaitForFirstConsumer is recreated
# (delete + apply), an Immediate one is not deleted.
grep -Eq ' delete ' "$T/argv.log" && fail "deleted the StorageClass when it was already Immediate"
STUB_SC_PROVISIONER="coxswain.io/local-path-shared" STUB_SC_MODE=WaitForFirstConsumer "$ROOT/hack/rwx-storage.sh" --context ctx1 --kubeconfig /tmp/kc >"$T/out2b" 2>&1 || { cat "$T/out2b"; fail "run 2b failed"; }
has "$T/argv.log" "delete storageclass local-shared" "a WaitForFirstConsumer class of ours was not recreated"
[ "$(grep -c ' delete ' "$T/argv.log")" = "1" ] || fail "expected exactly one delete"

# 3. a StorageClass with ANOTHER provisioner: stop before applying anything.
before="$(wc -c < "$A")"
if STUB_SC_PROVISIONER="kubernetes.io/no-provisioner" "$ROOT/hack/rwx-storage.sh" --context ctx1 >"$T/out3" 2>&1; then fail "replaced a foreign StorageClass"; fi
has "$T/out3" "already exists with provisioner 'kubernetes.io/no-provisioner'" "conflict message"
[ "$(wc -c < "$A")" = "$before" ] || fail "applied something despite the conflict"

# 3b. input validation: a bad --shared-path / the cluster's own provisioner name.
for bad in 'rel/path' '/srv/a"b' '/' '/srv/a b'; do
	if "$ROOT/hack/rwx-storage.sh" --context c --shared-path "$bad" >/dev/null 2>&1; then fail "accepted --shared-path '$bad'"; fi
done
if "$ROOT/hack/rwx-storage.sh" --context c --provisioner rancher.io/local-path >/dev/null 2>&1; then fail "accepted the cluster's own provisioner name"; fi
[ "$(wc -c < "$A")" = "$before" ] || fail "applied something on invalid input"

# 4. flags are honoured (custom class, namespace, shared path).
"$ROOT/hack/rwx-storage.sh" --context c2 --storage-class rwx --namespace ns9 --shared-path /srv/x >/dev/null 2>&1 || fail "run 4 failed"
tail -n +1 "$A" | grep -Fq 'name: rwx' || fail "custom storage class not used"
grep -Fq '{"sharedFileSystemPath": "/srv/x"}' "$A" || fail "custom shared path not used"
echo "OK: rwx-storage.sh applies a pinned second provisioner + StorageClass, is idempotent, and refuses a foreign StorageClass"
