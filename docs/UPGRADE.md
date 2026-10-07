# Upgrade notes

## Tool proxy NetworkPolicies created before D41d

Tool proxy NetworkPolicies created before PR #74 (D41d) carry no
`coxswain.io/tool-proxy-for` label. The operator's stale cleanup
(`cleanupStaleToolProxyNetpols`) lists by that label, so after an upgrade it
won't find them and won't delete them when the tool set changes. This is a
**one-time** issue: after the first reconcile post-upgrade, the operator
recreates the netpols with the label, and subsequent cleanups work correctly.

**Fix:** delete the pre-D41d tool netpols once:

```bash
# In each Loop's namespace:
kubectl get networkpolicy -l '!coxswain.io/tool-proxy-for' \
  --field-selector metadata.name='*-tool-*-netpol' -o name | \
  xargs -r kubectl delete
```

Or, per Loop:

```bash
kubectl delete networkpolicy <loop>-tool-<name>-netpol -n <ns>
```

The operator will recreate the netpols (with the label) on the next
reconcile. A fresh install is unaffected.
