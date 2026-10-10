# Upgrade notes

## Tool proxy NetworkPolicies created before D41d

Tool proxy NetworkPolicies created before PR #74 (D41d) carry no
`coxswain.io/tool-proxy-for` or `coxswain.io/tool` label. The operator does
not recreate them: `createOrUpdateNP` merges the labels into the live
object in place (`maps.Copy(np.Labels, desired.Labels)` in
`internal/controller/loop_controller.go:3946`), so on the first reconcile
post-upgrade the **live** tool netpols are labelled in place and heal.

The leak is narrower than it looks: `cleanupStaleToolProxyNetpols`
(`internal/controller/loop_controller.go:3814`) lists by the
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
by `createOrUpdateNP` (`internal/controller/loop_controller.go:3946`).
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
(`internal/controller/loop_controller.go:2376`) shortens near-63-char Loop
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
`cleanupStaleToolProxyNetpols` lists by the `coxswain.io/tool-proxy-for`
label (line 3816). A pre-D41d netpol for a removed tool has no label, so
the list never sees it, and it is never deleted. Manual deletion is the
one-time fix for netpols created before the label existed.

**Shown working on kind** (context `kind-coxswain-dev`, namespace
`i70-demo`, 2026-10-07): three netpols created — one unlabelled tool netpol
(the leak), one labelled tool netpol (live), one non-tool netpol. The list
command classified all three correctly (lived kept, leaked flagged, non-tool
skipped); the delete command removed only the leaked one; the verify
command printed nothing; the live and non-tool netpols were untouched.

A fresh install is unaffected (every tool netpol is created labelled from
the start).
