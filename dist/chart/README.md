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
The base operator roles do not grant a user `update` on `loops/status`, so a human driving
Manual-mode approvals (`approval.mode: Manual`) needs the **loop editor**
role — or an equivalent binding:

- install with `--set rbac.helpers.enabled=true` (creates the `loop-editor`
  role with `create;delete;get;list;patch;update;watch` on
  `coxswain.wattu.com/loops`), then bind it to a user or group:

  ```bash
  kubectl create rolebinding <name> \
    --role=loop-editor --user=<user> \
    -n <release-namespace>
  ```

  (with `rbac.namespaced=false` the role is a ClusterRole: use
  `--clusterrole=loop-editor --user=<user>` without `-n`.)

- or create your own Role/ClusterRole + binding with `get` on `loops` and
  `update` on `loops/status` (the minimum `cox approve`/`reject` need). A
  role with only `patch` on `loops` is NOT enough: the API server answers
  Forbidden on `loops/status`.

Without such a binding, `cox approve`/`reject` are rejected by the API
server. The operator is unaffected: Auto-mode Loops advance without any
user action, and the `cox` CLI still works for read-only commands
(`cox get/describe/history/plan/diff` use `get`).
