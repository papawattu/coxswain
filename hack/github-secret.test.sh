#!/usr/bin/env bash
# D55 execution test (the test norm: run the REAL changed script with a stub
# kubectl on PATH and a fake token file in a temp dir — never the real token
# file, never cat it). Asserts:
#   1. the generated Secret manifest has type kubernetes.io/basic-auth,
#      username=x-access-token, and the base64 of the fake password
#      (verifies the token is read via --from-file, not hand-encoded);
#   2. the script's stdout+stderr NEVER contain the fake token (the D55 rule:
#      the token is never printed, echoed, logged, or placed on a command
#      line);
#   3. a missing token file fails with 'token file missing';
#   4. an empty token file fails with 'token file missing'.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/hack/github-secret.sh"
FAKE_TOKEN="FAKE-TOKEN-D55-DO-NOT-USE-0123456789"

# base64 of the fake password (the value the manifest's password key must
# carry). No padding edge cases (length 33 -> base64 length 44 with '==').
EXPECTED_B64="$(printf '%s' "$FAKE_TOKEN" | base64)"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# A stub kubectl on PATH. It records every invocation (argv) to $KUBECTL_LOG.
# For 'create secret ... --dry-run=client -o yaml' it builds the manifest and
# writes it to STDOUT (the pipeline carries it to the downstream 'apply',
# which cats stdin — the real kubectl apply -f - reads stdin). It actually
# READS the file passed to --from-file (like the real kubectl), so a missing
# --from-file or a wrong path produces a detectable empty password. It NEVER
# prints the token.
STUB_DIR="$tmp/stub-bin"
mkdir -p "$STUB_DIR"
cat > "$STUB_DIR/kubectl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
DIR="$(cd "$(dirname "$0")/.." && pwd)"
KLOG="$DIR/kubectl-log"
printf '%s\n' "$*" >> "$KLOG"
if [ "${1:-}" = "create" ] && [ "${2:-}" = "secret" ]; then
	name=""
	typev=""
	username=""
	fromfile=""
	shift 2 # drop 'create secret'
	while [ "$#" -gt 0 ]; do
		case "$1" in
		-n) : ;;
		--type=*) typev="${1#--type=}" ;;
		--from-literal=*)
			kv="${1#--from-literal=}"
			key="${kv%%=*}"; val="${kv#*=}"
			if [ "$key" = "username" ]; then username="$val"; fi
			;;
		--from-file=*)
			kv="${1#--from-file=}"
			key="${kv%%=*}"; path="${kv#*=}"
			if [ "$key" = "password" ]; then fromfile="$path"; fi
			;;
		*) : ;;
		esac
		shift
	done
	# Read the file (like the real kubectl --from-file). This is the ONLY
	# place the token is read — and the stub reads it into a value it puts
	# in the manifest; the test then greps the script's stdout/stderr (not
	# this stub's internal value) for the token.
	if [ -n "$fromfile" ] && [ -f "$fromfile" ]; then
		pwd_b64="$(base64 < "$fromfile" | tr -d '\n')"
	else
		pwd_b64=""
	fi
	cat <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${name:-git-cred}
  namespace: ${NS:-default}
type: ${typev:-Opaque}
data:
  username: $(printf '%s' "${username:-}" | base64)
  password: ${pwd_b64}
EOF
	exit 0
fi
if [ "${1:-}" = "apply" ]; then
	# The real 'kubectl apply -f -' reads the manifest from stdin and prints a
	# 'created'/'configured' message. The stub cats stdin (the manifest) to its
	# own stdout so the test can assert on the type/username/password.
	cat -
	exit 0
fi
# Other invocations: a no-op (the stub never touches a real cluster).
exit 0
STUB
chmod +x "$STUB_DIR/kubectl"
KUBECTL_LOG="$tmp/kubectl-log"

# Run the real script with the stub on PATH and a fake token file. Captures
# stdout+stderr and the exit code into RUN_OUT / RUN_RC. Under set -e a
# failing command substitution in an assignment aborts the shell before the
# exit code is read, so errexit is disabled only around the substitution and
# restored immediately after.
run_script() {
	ns="${1:-ns-x}"; secret="${2:-git-cred}"; tokfile="${3:-$tmp/token}"
	set +e
	RUN_OUT="$(PATH="$STUB_DIR:$PATH" COXSWAIN_GITHUB_TOKEN_FILE="$tokfile" bash "$SCRIPT" "$ns" "$secret" 2>&1)"
	RUN_RC=$?
	set -e
}

# ---------------------------------------------------------------------------
# 1+2. Happy path: the manifest is correct and the token never leaks.
# ---------------------------------------------------------------------------
printf '%s' "$FAKE_TOKEN" > "$tmp/token"
run_script "ns1" "git-cred" "$tmp/token"
if [ "$RUN_RC" -ne 0 ]; then
	echo "FATAL: the script failed on the happy path (rc=$RUN_RC):"
	printf '%s\n' "$RUN_OUT"
	exit 1
fi
OUT="$RUN_OUT"

# 1. manifest: type, username, base64 password.
[ "$(printf '%s' "$OUT" | grep -c '^type: kubernetes.io/basic-auth')" -eq 1 ] \
	|| { echo "FATAL: manifest type is not kubernetes.io/basic-auth"; printf '%s\n' "$OUT"; exit 1; }
# username x-access-token -> base64
EXPECTED_USER_B64="$(printf '%s' "x-access-token" | base64)"
[ "$(printf '%s' "$OUT" | grep -c "username: $EXPECTED_USER_B64")" -eq 1 ] \
	|| { echo "FATAL: manifest username is not the base64 of x-access-token"; printf '%s\n' "$OUT"; exit 1; }
[ "$(printf '%s' "$OUT" | grep -c "password: $EXPECTED_B64")" -eq 1 ] \
	|| { echo "FATAL: manifest password is not the base64 of the fake token"; printf '%s\n' "$OUT"; exit 1; }

# 2. the script's stdout+stderr never contain the fake token (nor its base64,
#    which would also reveal it). The manifest's password line DOES carry
#    EXPECTED_B64 (that is assertion 1), so the token is flagged as a leak
#    ONLY if it appears OUTSIDE the manifest's own password line (e.g.
#    echoed by the script or on the command line).
if printf '%s' "$OUT" | grep -Fq -- "$FAKE_TOKEN"; then
	echo "FATAL: the fake token leaked on stdout/stderr (D55 violation)"; exit 1
fi
non_manifest="$(printf '%s' "$OUT" | grep -F -- "$EXPECTED_B64" | grep -vc 'password: ' || true)"
if [ "$non_manifest" -gt 0 ]; then
	echo "FATAL: the base64 token appeared outside the manifest password line (D55 violation)"; exit 1
fi

# The stub's argv log (what was actually invoked) must not contain the raw
# token on the command line (it is passed via --from-file=<path>, never as a
# literal).
if [ -f "$KUBECTL_LOG" ] && grep -Fq -- "$FAKE_TOKEN" "$KUBECTL_LOG"; then
	echo "FATAL: the raw token was placed on the kubectl command line (D55 violation)"; exit 1
fi

# ---------------------------------------------------------------------------
# 3. Missing token file: fail with 'token file missing'.
# ---------------------------------------------------------------------------
run_script "ns1" "git-cred" "$tmp/does-not-exist"
if [ "$RUN_RC" -eq 0 ]; then
	echo "FATAL: the script succeeded with a missing token file"; exit 1
fi
if printf '%s' "$RUN_OUT" | grep -Fq -- 'token file missing'; then
	: # pass
else
	echo "FATAL: missing-file error did not say 'token file missing'; got:"; printf '%s\n' "$RUN_OUT"; exit 1
fi

# ---------------------------------------------------------------------------
# 4. Empty token file: fail with 'token file missing'.
# ---------------------------------------------------------------------------
: > "$tmp/empty-token"
run_script "ns1" "git-cred" "$tmp/empty-token"
if [ "$RUN_RC" -eq 0 ]; then
	echo "FATAL: the script succeeded with an empty token file"; exit 1
fi
if printf '%s' "$RUN_OUT" | grep -Fq -- 'token file missing'; then
	: # pass
else
	echo "FATAL: empty-file error did not say 'token file missing'; got:"; printf '%s\n' "$RUN_OUT"; exit 1
fi

echo "OK: github-secret execution test passed"
