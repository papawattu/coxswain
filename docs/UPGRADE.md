# Upgrade notes

## Tool proxy NetworkPolicies created before D41d

Tool proxy NetworkPolicies created before PR #74 (D41d) carry no
`coxswain.io/tool-proxy-for` or `coxswain.io/tool` label. The operator does
not recreate them: `createOrUpdateNP` merges the labels into the live
object in place (`maps.Copy(np.Labels, desired.Labels)` in
`createOrUpdateNP`, `internal/controller/loop_controller.go`), so on the first reconcile
post-upgrade the **live** tool netpols are labelled in place and heal.

The leak is narrower than it looks: `cleanupStaleToolProxyNetpols`
(`internal/controller/loop_controller.go`) lists by the
`coxswain.io/tool-proxy-for` label, so a pre-D41d netpol for a tool that is
**still in the Loop's policy** is healed on the first reconcile (labelled in
place) and is no longer stale. Only netpols whose tool was **removed from
the policy before the upgrade** are never reconciled again (the ensure path
skips tools not in the effective policy, so their netpol is never
labelled) and they leak. This is a **one-time** issue: after the first
reconcile, every live tool netpol carries the label, and subsequent
removals are cleaned up correctly.

## The one-time fix

**Precondition:** the upgraded operator must have reconciled every Loop at
least once, so that every **live** tool netpol has been labelled in place
by `createOrUpdateNP` (`internal/controller/loop_controller.go`).
Verify this before running the delete:

```bash
# For each Loop in the namespace: confirm its live tool netpols carry the
# coxswain.io/tool-proxy-for label. A live tool netpol WITHOUT the label
# means the operator has not yet reconciled that Loop (or the tool was just
# added post-upgrade) — do NOT delete anything until every live tool netpol
# is labelled.
NS=<ns>
# NOTE: read each netpol's label by NAME, not from the first list's columns
# (a range over the names keeps the name paired with its own label). The
# jsonpath below escapes the dot in the label key: coxswain\.io.
kubectl -n $NS get networkpolicy \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' \
  | grep -E '\-tool\-.*\-netpol$' \
  | while IFS= read -r np; do
      loop=$(kubectl -n $NS get networkpolicy "$np" -o jsonpath='{.metadata.labels.coxswain\.io/tool-proxy-for}')
      tool=$(echo "$np" | sed 's/.*-tool-//; s/-netpol//')
      # Check if this tool is in the Loop's effective policy:
      in_policy=$(kubectl -n $NS get agentpolicy -o json 2>/dev/null | python3 -c "
import json,sys
data=json.load(sys.stdin)
found=False
for ap in data.get('items',[]):
  for t in ap.get('spec',{}).get('tools',[]):
    if t.get('name')=='$tool': found=True
print('yes' if found else 'no')
" 2>/dev/null || echo "unknown")
      if [ -z "$loop" ] && [ "$in_policy" = "yes" ]; then
        echo "  [WARN] $np: tool '$tool' is in the policy but the netpol is UNLABELLED — the operator has not reconciled this Loop yet. DO NOT run the delete until this is labelled."
      elif [ -z "$loop" ]; then
        echo "  [OK] $np: tool '$tool' is NOT in the policy (leaked, safe to delete)"
      else
        echo "  [OK] $np: labelled (loop=$loop)"
      fi
    done
```

**Do not run the delete until the precondition above passes** (every live
tool netpol is labelled; any unlabelled tool netpol is for a tool no
longer in the policy). If the precondition does not pass, trigger a
reconcile (e.g. `kubectl -n $NS get loop <loop> -o json | kubectl
apply -f -`) and re-check.

Delete the pre-D41d tool netpols for tools that are no longer in the
policy. A tool netpol is safe to delete **only if** its tool is absent from
the Loop's effective policy (the union of `spec.policyRefs[].spec.tools`):
deleting a **live** tool's netpol opens an egress hole — until the next
reconcile recreates it, no policy selects that tool proxy pod, which means
allow-all egress for it.

The command below lists every NetworkPolicy whose name matches the tool
netpol pattern (`-tool-…-netpol`; `derivedName`
(`internal/controller/loop_controller.go`) shortens near-63-char Loop
names with a hash, so match on the suffix, not the full Loop name) and, for
each, checks the `coxswain.io/tool-proxy-for` label. It prints the leaked
ones (unlabelled) and deletes them; it keeps the live ones (labelled by the
first post-upgrade reconcile) and skips non-tool netpols (wrong name
pattern).

```bash
NS=<ns>
K="kubectl -n $NS"
# List + classify every netpol in the namespace:
$K get networkpolicy \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.labels.coxswain\.io/tool-proxy-for}{"\n"}{end}' \
  | while IFS=$'\t' read -r np label; do
      case "$np" in
        *-tool-*-netpol)
          if [ -n "$label" ]; then
            echo "LIVE (labelled, kept): $np"
          else
            echo "LEAKED (unlabelled, would delete): $np"
          fi ;;
        *) echo "NOT A TOOL NETPOL (skipped): $np" ;;
      esac
    done
# To delete the leaked ones (review the list first):
$K get networkpolicy \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.labels.coxswain\.io/tool-proxy-for}{"\n"}{end}' \
  | while IFS=$'\t' read -r np label; do
      case "$np" in
        *-tool-*-netpol)
          [ -z "$label" ] && $K delete networkpolicy "$np" ;;
      esac
    done
# Verify (prints nothing when the leak is cleared):
$K get networkpolicy \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.labels.coxswain\.io/tool-proxy-for}{"\n"}{end}' \
  | while IFS=$'\t' read -r np label; do
      case "$np" in
        *-tool-*-netpol)
          [ -z "$label" ] && echo "LEAKED: $np" ;;
      esac
    done
```

**Why the operator's own cleanup doesn't just do this:**
`cleanupStaleToolProxyNetpols` lists by the
`coxswain.io/tool-proxy-for` label. A pre-D41d netpol for a removed tool has
no label, so the list never sees it, and it is never deleted. Manual
deletion is the one-time fix for netpols created before the label existed.

**Shown working on kind** (context `kind-coxswain-dev`, namespace
`i70-demo`, 2026-10-07): three netpols created — one unlabelled tool netpol
(the leak), one labelled tool netpol (live), one non-tool netpol. The list
command classified all three correctly (lived kept, leaked flagged, non-tool
skipped); the delete command removed only the leaked one; the verify
command printed nothing; the live and non-tool netpols were untouched.

A fresh install is unaffected (every tool netpol is created labelled from
the start).

## Upgrading from v0.1.2

The following changes landed after v0.1.2. Each entry says what changed and
what you must DO (or why there is nothing to do).

### The operator's ClusterRole gained a serviceaccounts rule

The ClusterRole now includes `serviceaccounts` (create, get, list, watch)
(F29, PR #199). A standard `kubectl apply -f dist/install.yaml` picks it up
automatically; there is nothing to do. If your RBAC is managed separately
(not via `install.yaml`), apply the new rule to your ClusterRole.

### `spec.workspace.restoreFrom` is now immutable

`restoreFrom` is immutable (F35, PR #215). Changing it on an existing Loop
is now rejected by the API server; there is nothing to do. A different
rollback target is a fork, not a re-task of the same Loop.

### Two new manager flags

`--memory-storage-class` (P5a, PR #191) sets the StorageClass for the
per-(namespace, git-URL) memory PVC; empty is the cluster default.
`--memory-writer-image` (P5b, PR #182) sets the image for the memory-writer
Job; it must be the operator image, because only the operator image contains
the `/usr/local/bin/coxswain-memory-write` binary. `config/default`
(`config/manager/manager.yaml`) passes `--memory-writer-image`; if your
install is not `config/default`, set it. Set `--memory-storage-class` to a
ReadWriteMany-capable class if your cluster's default StorageClass does not
support ReadWriteMany.

**For Helm installs:** the chart also emits `--memory-writer-image`
derived from `manager.image` (before `manager.args`), so if you put it in
`manager.args` as well it appears twice and the last occurrence wins. Set
`manager.image` instead, and leave it out of `manager.args` (see the note in
`dist/chart/values.yaml`). For `--memory-storage-class`, `hack/rwx-storage.sh`
installs a dynamic ReadWriteMany StorageClass (with `bindingMode: Immediate`,
so a memory PVC cannot sit unbound under `WaitForFirstConsumer`) on a cluster
whose default class is RWO-only — point `--memory-storage-class` at it.

### The agent NetworkPolicy is always created

The operator now always creates the `<loop>-agent-netpol` NetworkPolicy,
even when `spec.agent.endpointSecretRef` is empty and the policy has no
network allows (F42, PR #213). Before this change, such a Loop's agent ran
with no ingress or egress restrictions. After the upgrade, the agent netpol
carries deny-all ingress, and egress only to DNS plus the narrow set every
Loop gets: the model proxy (if the Loop has one), the egress proxy (for an
external repo's init clone), the repo host (for an in-cluster repo), and its
tool proxies.

The operator creates the netpol on the first reconcile after the upgrade,
before it touches the sandbox, so there is nothing to do. But it applies to
running pods at once: a Loop that is mid-implementation loses any network
access it had been relying on and can start failing straight away. If a Loop
needs a host, add it to the Loop's `AgentPolicy` network allows (or add the
tool to the policy) so the egress or tool proxy carries it. Terminal Loops
are unaffected.

### The sandbox waits for the egress proxy for the init clone

For a Loop whose workspace-init clone must traverse the egress proxy (an
external repo before the first clone), the sandbox is held `Suspended` until
the egress proxy pod is `Ready` (F55, PR #220). Before
this change, such a Loop could start before the proxy was ready and the
init clone would fail. On upgrade, such a Loop may briefly be held
`Suspended` if the proxy is not yet ready. There is nothing to do.

### Memory PVC binding is now visible (P5a)

While a Loop's new memory PVC is unbound (the class is not ReadWriteMany,
or the provisioner has not created it yet), the Loop runs without memory —
no lessons are seeded and nothing is written back. A Warning Event
`MemoryPVCUnbound` (listable via `kubectl get events`) flags the binding
wait. The Event is not a one-shot (R39 I144): while the Loop runs and the PVC
stays unbound it re-announces every 5 minutes, because one Event is gone after
the cluster's `--event-ttl` (default 1h) while a class without ReadWriteMany
leaves the PVC unbound for much longer. The operator schedules a reconcile for
that window, so an otherwise idle Loop still repeats it. A paused or completed
(Succeeded or Failed) Loop is not warned about its memory PVC at all (a paused
Loop re-announces after it resumes, if the PVC is still unbound). The last
Event's time is recorded on the Loop with the
`coxswain.io/memory-pvc-unbound-notified` annotation (a pre-upgrade `true`
value there simply announces once and is rewritten). There is nothing to do
unless you want the seeding: point `--memory-storage-class` at a
ReadWriteMany-capable class (see the flag entry above).

### Memory lesson cap

The memory-writer Job keeps at most 100 lessons per memory PVC, evicting the
oldest by mtime after a successful write (F32, PR #233). An existing PVC with
more than 100 lessons is trimmed on the next memory-writer run that has at
least one eligible lesson, even if every eligible lesson is a duplicate (a
run with no eligible lessons performs no eviction). The memory
reader also now stops adding whole files at its 16 KiB section cap instead of
silently dropping the whole memory section past that size. There is nothing
to do.

### The verify Job checks get a read-only checkout and the Job a deadline

The verify Job's acceptance checks now mount the verified checkout
(`/verify`) **read-only**, each check container gets its **own** writable
`/tmp` scratch, and the Job carries `activeDeadlineSeconds: 900` (F53).
The read-only mount closes the gap where check-0 could rewrite a `*_test.go`
that check-1 compiles (ADR-0005 D19 said the checkout is read-only
unconditionally).

A check that wrote into the checkout now fails with `read-only file system`.
The writable places are the check's own `/tmp` (also `HOME`, `TMPDIR`,
`GOCACHE`, `GOPATH`, `XDG_CACHE_HOME`, and `PYTHONPYCACHEPREFIX`, which keeps
`python3 -m compileall .` working). The shipped samples moved their build
output off the tree (`go build -o /tmp/<name> ./...`); a check of your own
that emits a binary or a cache into the checkout needs the same treatment:
write under `/tmp`. A check that hangs is now bounded — the Job is killed at
the deadline and the Loop is retried (then fails with `VerifyInfraFailed`,
which names the deadline in the message) instead of staying in `Verifying`
for ever.

There is nothing to do for an existing Loop unless one of its checks writes
into the checkout.

### The operator's default images are digest-pinned (F65)

Three images the operator pulls at run time now carry a registry digest:
`--cni-probe-image` (`docker.io/library/python:3.13-alpine@sha256:bf12ecc0…`),
`--workspace-git-image` (`docker.io/alpine/git:v2.54.0@sha256:e8ceedc7…`) and
`--verify-image` (`docker.io/library/golang:1.26@sha256:672c944c…`). The
workspace-git image is the container that mounts the git credential, and a tag
is mutable, so what runs could otherwise change under the operator between two
Loops.

Two consequences:

- **The CNI probe image moved from `python:3-alpine` to `python:3.13-alpine`**
  (version-exact, then pinned). A probe on a node whose cached `3-alpine` was a
  different minor version now runs 3.13. The probe script only uses the standard
  library, so nothing else changes.
- **The probe container pulls `IfNotPresent`** (explicit, instead of the policy
  the kubelet derives from the reference shape). A node with no cached copy
  pulls the exact digest; a node with one keeps it. A cluster that cannot reach
  Docker Hub must pre-load the image (the `kind-calico-up` target's `kind load`
  loop does, and its list now carries `python:3.13-alpine` to match the pin).

There is nothing to do unless you override one of the three flags: an override
is yours to pin. `grep -c 'sha256:' dist/install.yaml` is non-zero for the
operator's own image, as before.

### The verify Job's containers are resource-bounded (F58)

Every container in the verify Job now carries CPU and memory requests **and**
limits (F58). Before this the Job was BestEffort: the first pod the kubelet
evicts under node pressure, and a check that allocated without bound could
take the node (and the sandbox on it) with it.

| container | requests | limits |
|---|---|---|
| `clone-base`, `import-agent`, `tamper`, `artifact` (trusted) | cpu `100m`, memory `128Mi` | cpu `1`, memory `1Gi` |
| `check-*` (your acceptance checks) | cpu `250m`, memory `256Mi`, ephemeral-storage `1Gi` | cpu `2`, memory `2Gi`, ephemeral-storage `2Gi` |
| `noop` (the no-op main) | cpu `10m`, memory `16Mi` | cpu `100m`, memory `64Mi` |

The `check-*` containers are the ones that run your commands, so they are the
only ones with a knob: `spec.verify.resources` (a standard
`ResourceRequirements`) replaces the defaults **per resource name** — setting
only `limits.memory` keeps every other default. The trusted containers keep
the operator's constants, and no Loop field moves them.

Two behaviours of that knob are worth knowing before you copy a value in from
somewhere else (F58): **lowering** a limit lowers the matching request with it
(each name merges independently, and a request above its own limit is an invalid
Job, so the operator lowers the request to the limit — `limits.memory: 128Mi`
runs the check at 128Mi/128Mi), and **raising a request alone changes nothing**
(`requests.memory: 4Gi` without a limit is lowered to the default `2Gi` limit).
Only `requests` and `limits` are honoured: `claims` is accepted by the schema
because the field is a whole `ResourceRequirements`, but it is **ignored** — the
verify Job template carries no claim set, so a claim could never be satisfied.

The per-check `/tmp` scratch is bounded too: each `check-tmp-<i>` emptyDir
carries a `1Gi` `SizeLimit`. That is a constant, not part of
`spec.verify.resources` (an ephemeral-storage *limit* only makes the kubelet
evict sooner; the SizeLimit is what caps the disk). The `verify` checkout
volume stays unbounded on purpose — its size is your repository.

An acceptance check the kubelet **OOM-kills** is now reported as an
infrastructure failure (`VerifyInfraRetry`, then `VerifyInfraFailed` at the
bound), with the memory limit and the field that raises it in the message. It
no longer comes back as `check-0 failed (exit 137)` and burns an iteration.
Exit `137` with any other reason (a check that kills itself) still iterates.

**What to do:** nothing, for an existing Loop whose checks fit cpu `2` /
memory `2Gi` and write less than `1Gi` into `/tmp`. A heavier build sets
`spec.verify.resources` (e.g. `limits.memory: 4Gi`). A check that legitimately
needs more than `1Gi` of scratch cannot raise it yet — that bound is a
follow-up field; write less, or clean up as you go.

### The chart no longer renders a second VAP binding

Helm installs from v0.1.0–v0.1.2 rendered **two** `ValidatingAdmissionPolicyBinding` objects for the one ephemeral-container `ValidatingAdmissionPolicy`: the literal `coxswain-deny-ephemeral-containers` (correct), plus a templated one named after the release's fullname, `{{ fullname }}-deny-ephemeral-containers`, whose `policyName` named the templated name and therefore no existing VAP — so the second one was **inert** (it bound nothing; the gate still worked). PR #249 (F45) removed the templated template. `helm upgrade` normally deletes objects that the previous release tracked but the new manifest no longer carries, but that is not guaranteed if the binding was created out-of-band (a hand-rendered `helm template` piped to `kubectl apply`, a switch from `install.yaml` to Helm, or lost release history) — so run the manual check below either way. It is harmless: the stale binding is inert, and deleting it cannot weaken or disable the ephemeral-container gate.

**At release name `coxswain` (the default): there is nothing to do.** The two released templates collide on one object whose `policyName` names the real VAP, so there is nothing stale.

**At any other release name, check for and delete the stale binding.** (The stale name is `{{ fullname }}-deny-ephemeral-containers`, where `fullname` is the release name when it contains "coxswain", otherwise `<release>-coxswain`; the check below is name-agnostic, so you do not need to work it out.)

```sh
# 1. Should be exactly one binding, `coxswain-deny-ephemeral-containers`, with
#    POLICY = the VAP's name. A binding whose POLICY names no existing
#    ValidatingAdmissionPolicy is the stale one.
kubectl get validatingadmissionpolicybindings \
  -o custom-columns=NAME:.metadata.name,POLICY:.spec.policyName
kubectl get validatingadmissionpolicy coxswain-deny-ephemeral-containers

# 2. Delete the stale one if present (inert: it binds nothing, so this cannot
#    weaken or disable the ephemeral-container gate).
kubectl delete validatingadmissionpolicybinding <stale-name>

# 3. Confirm the gate still works (it is a Deny, so the command should be
#    refused).
kubectl debug -it <a coxswain pod> --image=busybox
```

If the check in step 1 shows two bindings for one release, keep the literal `coxswain-deny-ephemeral-containers` and delete the other.

## Stricter Secret-name validation (F48)

`spec.workspace.gitCredentialSecret` and `spec.agent.endpointSecretRef` are
now validated by the CRD: at most 253 characters and a DNS-1123 subdomain
(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`).
A Loop whose existing value does not match (for example uppercase letters
or an underscore) is not deleted or changed. Two update paths behave
differently. A spec or metadata update (a `kubectl edit`, or the operator
removing its finalizer) is validated: on Kubernetes 1.30 and later,
CRD validation ratcheting lets it through when it leaves that field
unchanged, so only an update that changes the field must make it valid; on
an older API server it may be rejected until the field is fixed. A status
update never validates the spec on any version, so the operator's status
writes are not blocked. (Not verified against a live API server for this
CRD.) Kubernetes cannot have created a Secret with such
a name, so a non-matching value was already a Loop that could never start.
Before upgrading, list the values and look for non-matching ones:

```bash
kubectl get loops -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\t"}{.spec.workspace.gitCredentialSecret}{"\t"}{.spec.agent.endpointSecretRef}{"\n"}{end}'
```

## Sandbox-failure counts recorded before F48g (no run key), and the terminal mount failure

An operator older than F48g wrote `status.agent.infraAttempts` and
`status.agent.infraSandboxUID` but no `status.agent.infraEpisode`. The key is
what scopes the count to one run, and an absent key reads as "a run that is not
the current one", so an in-flight failure episode restarts on the first failure
the upgraded operator counts: a Loop that had used 2 of its 3 attempts restarts
at attempt 1 instead of failing terminally on the next failure, gets the full
bound again (up to `sandboxInfraRetries`, 2, extra recreate cycles), and still
ends at `Failed:SandboxFailed` if the cause persists. If the recorded
`infraSandboxUID` still names the live Sandbox, the UID guard skips that first
failure instead (it is the one already counted), so nothing is double counted.
Nothing needs doing and nothing is migrated: the next counted failure writes the
key, and a Loop that is not mid-episode (no count) is unaffected. The upgrade is
visible as one more "attempt 1 of 3" Warning Event on a Loop that was
mid-episode, and the terminal failure is unchanged. `status.agent.infraEpisode`
is a new optional status field; it needs no schema migration, and a downgrade
simply ignores it. `status.verify.infraAttempts` needs no upgrade line: it has
no episode key, a pre-I65 status has no count, and its reset rule (a new pin) is
unchanged by this work.

A sandbox volume the kubelet cannot mount (a `gitCredentialSecret` that does not
exist, say) now ends at `Failed:SandboxFailed`, with one attempt per recreate up
to the bound rather than an unbounded recreate loop. That failure is terminal and
there is no retry command: create or fix the Secret, then **delete and re-apply
the Loop**. A rollback does not clear it (a rollback never changes the phase), so
do not reach for one as a retry.

## Stricter `ref`, `acceptanceChecks` and delivery-branch validation (F68)

Admission now bounds four fields that reach the verify and deliver scripts
as git/shell arguments:

| field | rule |
|---|---|
| `spec.workspace.ref` | at most 255 characters; characters `[A-Za-z0-9._/@+]` and `-`; may not start with `-`; **empty stays valid** (it means the repo's default branch) |
| `spec.verify.acceptanceChecks[]` | at most 16 entries, each 1–4096 characters and containing a non-whitespace character (the contents are otherwise free — a check is a shell command) |
| `spec.delivery.baseBranch` | characters `[A-Za-z0-9._/-]`; may not start with `-` (it is interpolated into the deliver Job's `git fetch origin <base>`) |
| `spec.delivery.branchPrefix` | the same character set and the same no-leading-dash rule |

A value that starts with `-` is refused because git parses a dash-leading
argument as an **option**: `ref: -f` or `baseBranch: --all` changes what the
credentialed clone fetches instead of naming a branch. The other bounds exist
because these strings are unbounded today: a 16 KiB check, a 40-entry list, or
a 300-character ref.

**Nothing is migrated, and no Loop is changed by the upgrade.** What changes is
what the API server will accept *from now on*. An existing Loop outside these
bounds behaves differently depending on whether your API server enforces
updates against the new schema:

- **On an API server with CRD validation ratcheting (Kubernetes 1.30 and
  later), an update that does not change the offending field is still
  admitted** — the same behaviour the F48 Secret-name bounds rely on (see the
  section above). Since the operator's status writes and its finalizer removal
  never touch `ref`/`acceptanceChecks`/the delivery branch fields, such a Loop
  keeps running and can still be deleted normally.
- **On an older API server, every update of that Loop is rejected** (422
  Invalid, naming the field), because the whole stored object is validated.
  That freezes the Loop: its status cannot be written, and on delete its
  checkpoint finalizer cannot be removed, so it sits in `Terminating`. On
  1.30+ this only applies to an update that *changes* the offending field.

If a Loop is frozen this way (or you simply want it inside the new bounds), the
remediation is to patch the offending field to a valid value. That update is
valid, so it is accepted, and the operator can then write status and remove its
finalizer:

```bash
# ref: reset to the default branch (empty is valid)
kubectl -n <ns> patch loop <name> --type=merge -p '{"spec":{"workspace":{"ref":""}}}'
# acceptanceChecks: trim the list (or shorten an entry) to fit the bounds
kubectl -n <ns> patch loop <name> --type=json \
  -p '[{"op":"replace","path":"/spec/verify/acceptanceChecks","value":["go test ./..."]}]'
# delivery branches: drop the leading dash / fix the characters
kubectl -n <ns> patch loop <name> --type=merge -p '{"spec":{"delivery":{"baseBranch":"main"}}}'
```

A frozen deleting Loop also emits a `FinalizerRemovalBlocked` Warning Event
naming this document (the same Event a terminating namespace produces), so
`kubectl describe loop <name>` shows why it is stuck.

(Not verified against a live API server for this CRD: the ratcheting split
above is the behaviour the F48 section documents, and the finalizer-removal
Event is covered by a spec.)
