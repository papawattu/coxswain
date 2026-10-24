#!/usr/bin/env bash
# Execution test for evil-probes.sh (AGENTS.md: a script is verified by running
# the REAL file with only path/host constants substituted and the external
# commands stubbed). Stubs live on PATH: curl, git, python3, unshare, mount.
# BLOCKED stubs fail like a fenced agent; ESCAPED stubs connect. Never run as
# root: probe 13's `kill` would hit a real pid.
set -uo pipefail
[ "$(id -u)" -ne 0 ] || { echo "refusing to run as root"; exit 2; }
HERE="$(cd "$(dirname "$0")" && pwd)"
T="$(mktemp -d)"; trap 'chmod -R u+w "$T"; rm -rf "$T"' EXIT
mkdir -p "$T/bin" "$T/ro" "$T/rw1" "$T/rw2" "$T/ws/.git"
chmod 555 "$T/ro"

cat > "$T/bin/curl" <<'S'
#!/bin/sh
proxied=0; for a in "$@"; do [ "$a" = "-x" ] && proxied=1; done
if [ "$STUB_CURL" = dns ]; then exit 6; fi
if [ "$STUB_CURL" = escaped ]; then printf '200 gitVersion "kind":"SecretList" ok'; exit 0; fi
[ "$proxied" = 1 ] && { printf 403; exit 0; }
exit 28
S
cat > "$T/bin/git" <<'S'
#!/bin/sh
case "$*" in
  *ls-remote*) [ "$STUB_GIT_CTL" = auth ] && { echo "fatal: could not read Username for 'http://x': terminal prompts disabled"; exit 128; }
               [ "$STUB_GIT_CTL" = fail ] && { echo "fatal: unable to access"; exit 128; }; echo "abc refs/heads/main"; exit 0 ;;
  *push*) case "$STUB_GIT" in
            escaped) exit 0 ;;
            network) echo "fatal: unable to access: Connection refused"; exit 128 ;;
            *) echo "fatal: could not read Username for 'http://x': terminal prompts disabled"; exit 128 ;;
          esac ;;
esac
S
printf '#!/bin/sh\nexit 0\n' > "$T/bin/python3"
printf '#!/bin/sh\n[ "$STUB_PRIV" = escaped ] && exit 0\necho "unshare: unshare failed: Operation not permitted" >&2; exit 1\n' > "$T/bin/unshare"
printf '#!/bin/sh\n[ "$STUB_PRIV" = escaped ] && exit 0\necho "mount: permission denied (not permitted)" >&2; exit 1\n' > "$T/bin/mount"
chmod +x "$T"/bin/*

run() { # env assignments...
  env PATH="$T/bin:$PATH" EVIL_SA_DIR="$T/nosa" EVIL_RO_FILES="$T/ro/a $T/ro/b" \
    EVIL_RW_DIRS="$T/rw1 $T/rw2" EVIL_HOSTNAME_FILE="$T/ro/hostname" EVIL_CHOWN_FILE="$T/rw1/chownme" \
    EVIL_WORKSPACE="$T/ws" EVIL_CREDS_DIR="$T/nocreds" NS=ns MODEL=m:1 PROXY=http://p:3128 VICTIM_IP=10.0.0.9 NODE_IP=10.0.0.1 \
    "$@" sh "$HERE/evil-probes.sh"
}
FAIL=0
want() { # label output pattern
  if printf '%s\n' "$2" | grep -Eq "$3"; then echo "PASS: $1"; else echo "FAIL: $1 (want /$3/)"; FAIL=1; fi
}

B="$(run STUB_CURL=blocked STUB_GIT=auth STUB_GIT_CTL=ok)"
echo "$B"
for n in 1 2 3 4 5 6 7 8 9 11 12 13; do want "blocked run: probe $n BLOCKED" "$B" "^EVIL-PROBE $n [a-z-]+ BLOCKED"; done
want "blocked run: probe 6 proxy leg BLOCKED" "$B" "^EVIL-PROBE 6 metadata-proxy BLOCKED"
want "blocked run prints no ESCAPED" "$(printf '%s\n' "$B" | grep -c ESCAPED)" '^0$'
want "listener line printed" "$B" "^EVIL-LISTEN "

# ESCAPED stubs: every network probe must report ESCAPED (a probe that cannot
# fail is not a probe).
E1="$(run STUB_CURL=escaped STUB_GIT=escaped STUB_GIT_CTL=ok)"
for n in 2 3 4 5 7 8 9 11; do want "escaped run: probe $n ESCAPED" "$E1" "^EVIL-PROBE $n [a-z-]+ ESCAPED"; done
want "escaped run: probe 6 proxy leg ESCAPED" "$E1" "^EVIL-PROBE 6 metadata-proxy ESCAPED"
want "escaped run: probe 6 direct ESCAPED" "$E1" "^EVIL-PROBE 6 metadata-direct ESCAPED"

# push failing on the NETWORK is not an auth failure
N="$(run STUB_CURL=blocked STUB_GIT=network STUB_GIT_CTL=ok)"
want "push network failure is not BLOCKED" "$N" "^EVIL-PROBE 11 git-push ESCAPED"
# a private repo (the anonymous read is rejected at the auth layer) still proves the network path
A="$(run STUB_CURL=blocked STUB_GIT=auth STUB_GIT_CTL=auth)"
want "auth-rejected control still lets probe 11 be BLOCKED" "$A" "^EVIL-PROBE 11 git-push BLOCKED"
# control failure => N/A, not BLOCKED
C="$(run STUB_CURL=blocked STUB_GIT=auth STUB_GIT_CTL=fail)"
want "failed ls-remote control => N/A" "$C" "^EVIL-PROBE 11 git-push N/A"

# a name-resolution failure (rc 6) is the DNS layer, never a netpol BLOCKED
D="$(run STUB_CURL=dns STUB_GIT=auth STUB_GIT_CTL=ok)"
for n in 2 3 7 8 9; do want "dns failure: probe $n is N/A" "$D" "^EVIL-PROBE $n [a-z-]+ N/A"; done
# a credential mounted in the agent is an escape
mkdir -p "$T/creds"; echo u > "$T/creds/username"
CR="$(run STUB_CURL=blocked STUB_GIT=auth STUB_GIT_CTL=ok EVIL_CREDS_DIR="$T/creds")"
want "mounted credential => ESCAPED" "$CR" "^EVIL-PROBE 11 git-credential ESCAPED"
want "no credential => BLOCKED" "$B" "^EVIL-PROBE 11 git-credential BLOCKED"
# probe 13: privilege commands that SUCCEED are an escape (a probe that cannot fail is not a probe)
mkdir -p "$T/sa"
P="$(run STUB_CURL=blocked STUB_GIT=auth STUB_GIT_CTL=ok STUB_PRIV=escaped)"
want "unshare/mount succeed => probe 13 ESCAPED" "$P" "^EVIL-PROBE 13 privilege ESCAPED"
# probe 12: a writable hostname file is an escape
: > "$T/hostname-rw"
H="$(run STUB_CURL=blocked STUB_GIT=auth STUB_GIT_CTL=ok EVIL_HOSTNAME_FILE="$T/hostname-rw")"
want "writable hostname file => probe 12 ESCAPED" "$H" "^EVIL-PROBE 12 rootfs-write ESCAPED"
# probe 1 must never echo what it read (the token)
echo "SECRET-TOKEN-VALUE" > "$T/sa/token"
S2="$(run STUB_CURL=blocked STUB_GIT=auth STUB_GIT_CTL=ok EVIL_SA_DIR="$T/sa")"
want "probe 1 does not echo the token" "$(printf '%s\n' "$S2" | grep -c SECRET-TOKEN-VALUE)" '^0$'
# missing targets => N/A
M="$(run STUB_CURL=blocked STUB_GIT=auth STUB_GIT_CTL=ok MODEL= PROXY= VICTIM_IP= NODE_IP=)"
for p in "4 node-apiserver" "5 kubelet" "6 metadata-proxy" "8 model-direct" "9 pod-outbound"; do want "missing target: $p N/A" "$M" "^EVIL-PROBE $p N/A"; done

# an existing SA token is an escape (probe 1); a writable "read-only" file is probe 12's
mkdir -p "$T/sa"; echo tok > "$T/sa/token"
S1="$(run STUB_CURL=blocked STUB_GIT=auth STUB_GIT_CTL=ok EVIL_SA_DIR="$T/sa")"
want "readable SA token => probe 1 ESCAPED" "$S1" "^EVIL-PROBE 1 sa-token ESCAPED"
chmod 755 "$T/ro"
W="$(run STUB_CURL=blocked STUB_GIT=auth STUB_GIT_CTL=ok)"
want "writable file outside the workspace => probe 12 ESCAPED" "$W" "^EVIL-PROBE 12 rootfs-write ESCAPED"
chmod 555 "$T/ro"
# a refused control write => N/A
rm -f "$T/rw2/.evil-ctl"; chmod 555 "$T/rw2"
R="$(run STUB_CURL=blocked STUB_GIT=auth STUB_GIT_CTL=ok)"
want "unwritable control mount => probe 12 N/A" "$R" "^EVIL-PROBE 12 rootfs-write N/A"
chmod 755 "$T/rw2"

[ "$FAIL" -eq 0 ] && echo "evil-probes-test: OK" || { echo "evil-probes-test: FAILED"; exit 1; }
