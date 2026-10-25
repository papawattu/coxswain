# coxswain Helm chart (dist/chart)

The chart is generated from the kustomize overlays by `make build-installer`
(hack/build-installer.sh renders it with the helm plugin). It ships the
operator's CRDs, RBAC, manager Deployment, admission (ValidatingAdmissionPolicy
+ binding), the cni-probe namespace, and the metrics service — the same set as
`dist/install.yaml` (the `chart-check` target verifies the chart is a superset
of install.yaml: every (kind, name) the kustomize build carries is in the
chart's rendered output).

## Values of note

- `rbac.namespaced` (default `false`): `false` installs ClusterRole +
  ClusterRoleBinding (all namespaces); `true` installs Role + RoleBinding
  (release namespace only).
- `rbac.helpers.enabled` (default `false`): installs the convenience
  `loop-admin` / `loop-editor` / `loop-viewer` roles for the `loops` CRD.
- `metrics.enabled` (default `true`): installs the metrics Service, the
  metrics-auth Role/RoleBinding, and passes `--metrics-bind-address` to the
  manager; `metrics.secure` (default `true`) selects HTTPS + the auth RBAC
  path. Set `metrics.enabled=false` to disable all of it (the manager then
  binds metrics to :0).
- `crd.enabled` (default `true`) / `crd.keep` (default `true`): install the
  CRDs with the chart and keep them on uninstall.
- `certManager.enabled` (default `false`): the Prometheus ServiceMonitor's
  `tlsConfig.ca` is emitted from the cert-manager issuer only when this is
  true (with `prometheus.enabled=true` and `metrics.secure=true`).
- `serviceAccount.enabled` (default `true`): install the chart's
  ServiceAccounts (including the fixed-name `coxswain-cni-probe` SA in the
  `coxswain-cni-probe` namespace); with `enabled=false` set
  `serviceAccount.name` to an existing SA.
- `manager.env` (list) and `manager.envOverrides` (map): the envOverrides
  map wins over a same-name entry in the env list.
- `manager.healthProbe.port` (default `8081`): the liveness/readiness port
  (the manager serves /healthz and /readyz on it).
- `rbac.roleNamespaces` and `webhook.*`: currently NOT read by any template
  (the cni-probe namespace is hard-wired and the chart ships no operator
  webhook); changing them has no effect on the rendered chart today.

## Driving Manual-mode approvals with the cox CLI (rbac.helpers.enabled)

`cox approve <loop>` and `cox reject <loop> --reason <reason>` read the Loop
with `get` and write the whole object back to the `loops/status` subresource
with an `update` (PUT; they set the PlanApproved / PlanRejected conditions).
Both also read `PLAN.md` from the workspace through a short-lived
peek pod (`internal/cli/workspace.go`), so the user needs pod RBAC in the
Loop's namespace as well. The base operator roles grant neither, so a human
driving Manual-mode approvals (`approval.mode: Manual`) needs both:

- **Loop access:** install with `--set rbac.helpers.enabled=true` (creates
  the `<fullname>-loop-editor-role` helper role, `coxswain-loop-editor-role`
  at the default release name: `create;delete;get;list;patch;update;watch` on
  `coxswain.wattu.com/loops` and `get;update;patch` on `loops/status`), then
  bind it to a user or group:

  ```bash
  kubectl create rolebinding <name> \
    --role=coxswain-loop-editor-role --user=<user> \
    -n <release-namespace>
  ```

  (the role is named `<release>-coxswain-loop-editor-role` when the release
  name does not contain `coxswain`, e.g. `my-cox-coxswain-loop-editor-role`
  for release `my-cox`; with `rbac.namespaced=false` the role is a
  ClusterRole: use `--clusterrole=<that name> --user=<user> -n <loop-namespace>`
  — a RoleBinding to a ClusterRole is still namespaced, it is only the flag
  that changes.)

- **Pod access (the peek pod):** the helper role does NOT include this. Bind
  a Role in the Loop's namespace granting `create`, `get`, `delete` on `pods`
  and `get` on `pods/log`.

- or create your own Role/ClusterRole with: `get` on `loops`, `update` on
  `loops/status`, and the pod rules above (the minimum `cox approve` /
  `cox reject` need). A role with only `patch` on `loops` is NOT enough: the
  API server answers Forbidden on `loops/status`, and `cox approve` fails at
  `create peek pod` without the pod rules. (`cox pause` / `cox resume` use
  `patch` on `loops`, a separate verb.)

Without such a binding, `cox approve`/`reject` are rejected by the API
server. The operator is unaffected: Auto-mode Loops advance without any
user action, and the `cox` CLI still works for read-only commands
(`cox get/describe/history/plan/diff` use `get`).
