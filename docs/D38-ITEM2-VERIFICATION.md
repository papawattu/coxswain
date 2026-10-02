# D38 item 2: static verification (Makefile + label sets)

Verification evidence for the D38 handoff item 2, recorded at
`slice/d38-cni-self-test` (PR #42). No cluster mutation was required.

## (a) kind-calico-up does NOT pass --allow-unenforced-network

`make kind-calico-up` (Makefile:149-212) deploys the **base** install
(`kustomize build config/default`), NOT the dev overlay:

```
# Makefile ~line 205-209 (comment in the recipe itself):
# D38: the enforcing-CNI profile deploys the BASE install (config/default): \
# NO --allow-unenforced and NO --allow-unenforced-network. Calico is the \
# enforcing CNI, so the D38 CNI self-test gate is expected to PASS \
# WITHOUT the escape hatch (NetworkEnforced=True). (A dev overlay here \
# would make the network gate pass-via-flag and prove nothing about \
# the CNI.) \
(cd "$$TMP_OVERLAY" && "$(LOCALBIN)/kustomize" build config/default | kubectl --context $$CTX apply -f -)
```

The only place the flags exist at all is `config/dev/kustomization.yaml`
(lines 23 and 31, values `--allow-unenforced` / `--allow-unenforced-network`),
and `kind-calico-up` never references `config/dev` — it kustomizes
`config/default` in a temp overlay (image swap only). Confirmed: **no
`--allow-unenforced-network` (nor `--allow-unenforced`) is passed by
`kind-calico-up`**.

## (b) the probe namespace is not selected by agent KubeArmor or Loop NetworkPolicies

The probe pod (`internal/cni/pod_prober.go:236-243 buildPod`) carries
exactly ONE label:

```go
Labels: map[string]string{probeLabelKey: "cni-probe"}   // coxswain.io/probe=cni-probe
```

The probe's own NetworkPolicy selects only that label
(`pod_prober.go:205`):

```go
PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{probeLabelKey: "cni-probe"}},
```

The selectors that could otherwise reach into the probe namespace:

1. **Agent KubeArmor fence** — `internal/engine/kubearmor.go:107`:
   ```go
   KaptMatchLabelsKey: map[string]string{"coxswain.io/loop": loopName},
   ```
   A KubeArmorPolicy with `matchLabels: {coxswain.io/loop: <loop>}`. The
   probe pod has NO `coxswain.io/loop` label -> the selector misses it
   structurally.

2. **Loop agent NetworkPolicy** — controller `loop_controller.go:2111-2121`
   (`podAgentLabels`): selects `coxswain.io/loop=<loop>` +
   `app.kubernetes.io/component=agent`. Probe pod has neither -> miss.

3. **Proxy pod/Service NetworkPolicy (D33)** — `loop_controller.go:2023`
   + `loop_d34_netpol_test.go`: selects `app.kubernetes.io/component`
   (`model-proxy` / the proxy component value) plus the per-Loop identity.
   Probe pod carries no `app.kubernetes.io/component` -> miss.

The probe **namespace** itself (install-time, `config/cni-probe/namespace.yaml`,
labels `app.kubernetes.io/name: coxswain-cni-probe`,
`app.kubernetes.io/part-of: coxswain`, `coxswain.io/probe-namespace: "true"`)
matters only insofar as pods inside it are selected by pod-label selectors —
all three selectors above are pod-label selectors and all miss the probe pod.

**Conclusion:** the probe pod in `coxswain-cni-probe` is unreachable by the
agent KubeArmorPolicy selector and by the per-Loop agent/proxy
NetworkPolicy selectors. Verified statically from the current source
(internal/cni/pod_prober.go, internal/engine/kubearmor.go,
internal/controller/loop_controller.go); no cluster mutation required.
