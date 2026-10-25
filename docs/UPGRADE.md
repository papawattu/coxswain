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
no lessons are seeded and nothing is written back. A one-shot Warning Event
`MemoryPVCUnbound` (listable via `kubectl get events`) flags the binding
wait: the operator emits it at most once per Loop, recorded with the
`coxswain.io/memory-pvc-unbound-notified` annotation (re-emitted only if that
annotation is cleared while the PVC is still unbound). There is nothing to do
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
