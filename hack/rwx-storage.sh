#!/usr/bin/env bash
# F21: install a dynamic ReadWriteMany StorageClass for the coxswain memory PVC
# on a cluster whose default class is RWO-only (kind, k3d/k3s local-path).
#
# It installs a SECOND, pinned rancher local-path-provisioner in its own
# namespace, with its own provisioner name and its own ClusterRole, running in
# sharedFileSystemPath mode (which is what lets it provision RWX volumes), and
# a StorageClass (default name local-shared) that points at it. The cluster's
# own local-path provisioner and default StorageClass are NOT touched.
#
# sharedFileSystemPath is only truly shared if that path is the same
# filesystem on every node (NFS, CephFS, a docker volume mounted into every
# k3d node). On a single-node cluster any path works. Pass --shared-path to
# point at a genuinely shared mount on a multi-node cluster.
#
# Idempotent: every object is applied with `kubectl apply`; a re-run changes
# nothing. If a StorageClass of the same name already exists with a DIFFERENT
# provisioner (a StorageClass provisioner is immutable), the script stops and
# says so instead of replacing it.
#
# Usage: hack/rwx-storage.sh --context <ctx> | --kubeconfig <file> [flags]
#   --context CTX        kubeconfig context (repeatable with --kubeconfig)
#   --kubeconfig FILE    kubeconfig file
#   --namespace NS       provisioner namespace          (default coxswain-rwx-storage)
#   --storage-class NAME StorageClass to create          (default local-shared)
#   --provisioner NAME   provisioner name                (default coxswain.io/local-path-shared)
#   --shared-path PATH   sharedFileSystemPath            (default /var/local-path-shared)
#   --image IMG          provisioner image (pinned)
#   --helper-image IMG   helper (busybox) image (pinned)
# Use the storage class with the operator's --memory-storage-class (and
# `cox memory import --storage-class`).
set -euo pipefail

KUBECTL="${KUBECTL:-kubectl}"
CTX="" KCFG=""
NS="coxswain-rwx-storage"
SC="local-shared"
PROV="coxswain.io/local-path-shared"
SHARED_PATH="/var/local-path-shared"
IMAGE="docker.io/rancher/local-path-provisioner:v0.0.31"
HELPER="docker.io/library/busybox:1.36"

die() { echo "Error: $*" >&2; exit 1; }
while [ $# -gt 0 ]; do
	[ $# -ge 2 ] || die "flag $1 needs a value"
	case "$1" in
	--context) CTX="$2" ;;
	--kubeconfig) KCFG="$2" ;;
	--namespace) NS="$2" ;;
	--storage-class) SC="$2" ;;
	--provisioner) PROV="$2" ;;
	--shared-path) SHARED_PATH="$2" ;;
	--image) IMAGE="$2" ;;
	--helper-image) HELPER="$2" ;;
	*) die "unknown flag $1" ;;
	esac
	shift 2
done
# --shared-path lands inside a JSON string and a hostPath: absolute, and only
# a safe charset (an embedded quote would emit invalid config.json and the only
# symptom would be a rollout timeout).
case "$SHARED_PATH" in
/*) ;;
*) die "--shared-path must be an absolute path (got '$SHARED_PATH')" ;;
esac
[ "$SHARED_PATH" != "/" ] || die "--shared-path must not be /"
if ! printf '%s' "$SHARED_PATH" | grep -Eq '^/[A-Za-z0-9._/-]+$'; then
	die "--shared-path may contain only letters, digits and . _ / - (got '$SHARED_PATH')"
fi
# The cluster's own local-path provisioner owns this name; two provisioners
# fighting over it would break every PVC on the cluster.
[ "$PROV" != "rancher.io/local-path" ] || die "--provisioner must not be rancher.io/local-path (the cluster's own local-path provisioner)"
[ -n "$CTX" ] || [ -n "$KCFG" ] || die "pass --context and/or --kubeconfig (this script never uses the ambient context)"

kc() {
	local a=()
	[ -z "$KCFG" ] || a+=(--kubeconfig "$KCFG")
	[ -z "$CTX" ] || a+=(--context "$CTX")
	"$KUBECTL" "${a[@]}" "$@"
}

# A StorageClass's provisioner and volumeBindingMode are immutable.
# Refuse to fight an existing class with another provisioner.
read -r existing existing_mode < <(kc get storageclass "$SC" -o jsonpath='{.provisioner} {.volumeBindingMode}{"\n"}' 2>/dev/null || true) || true
if [ -n "${existing:-}" ] && [ "$existing" != "$PROV" ]; then
	die "StorageClass $SC already exists with provisioner '$existing' (want '$PROV'); pick another --storage-class or delete it yourself (this script does not replace a StorageClass)"
fi
# F24: the class must be Immediate. The operator mounts the memory PVC only once
# it is Bound, and a WaitForFirstConsumer PVC is only bound by a consumer pod, so
# WaitForFirstConsumer deadlocks (memory never binds). sharedFileSystemPath
# volumes are not node-bound, so Immediate is safe. An older run of this script
# created the class as WaitForFirstConsumer; the mode cannot be patched, so
# recreate OUR OWN class (provisioner matched above). Existing PVs/PVCs are
# unaffected: a StorageClass is only a template.
if [ -n "${existing:-}" ] && [ "${existing_mode:-}" != "Immediate" ]; then
	echo "Recreating StorageClass $SC: volumeBindingMode ${existing_mode:-<unset>} -> Immediate (F24)"
	kc delete storageclass "$SC"
fi

# The provisioner reads its config + helper-pod template once at start, so a
# changed ConfigMap alone does not take effect. A checksum annotation on the pod
# template (this script + the flag values that feed the ConfigMap) makes a
# changed config roll the Deployment, and an unchanged one a no-op.
CFG_SUM="$(printf '%s\n%s\n%s\n%s\n%s' "$(sha256sum < "${BASH_SOURCE[0]}")" "$SHARED_PATH" "$HELPER" "$PROV" "$IMAGE" | sha256sum | cut -c1-16)"

kc apply -f - <<YAML
apiVersion: v1
kind: Namespace
metadata:
  name: $NS
  labels: {app.kubernetes.io/part-of: coxswain-rwx-storage}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: coxswain-rwx-provisioner, namespace: $NS}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: coxswain-rwx-provisioner}
rules:
- apiGroups: [""]
  resources: [nodes, persistentvolumeclaims, configmaps, pods, pods/log]
  verbs: [get, list, watch]
- apiGroups: [""]
  resources: [persistentvolumes]
  verbs: [get, list, watch, create, patch, update, delete]
- apiGroups: [""]
  resources: [events]
  verbs: [create, patch, update]
- apiGroups: [storage.k8s.io]
  resources: [storageclasses]
  verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: coxswain-rwx-provisioner}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: coxswain-rwx-provisioner}
subjects:
- {kind: ServiceAccount, name: coxswain-rwx-provisioner, namespace: $NS}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: coxswain-rwx-provisioner, namespace: $NS}
rules:
- apiGroups: [""]
  resources: [pods]
  verbs: [get, list, watch, create, delete]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: coxswain-rwx-provisioner, namespace: $NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: coxswain-rwx-provisioner}
subjects:
- {kind: ServiceAccount, name: coxswain-rwx-provisioner, namespace: $NS}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: coxswain-rwx-config, namespace: $NS}
data:
  config.json: |-
    {"sharedFileSystemPath": "$SHARED_PATH"}
  helperPod.yaml: |-
    apiVersion: v1
    kind: Pod
    metadata:
      name: helper-pod
    spec:
      # The helper only mkdir/rm's on the host path; it never calls the API.
      automountServiceAccountToken: false
      containers:
      - name: helper-pod
        image: $HELPER
        imagePullPolicy: IfNotPresent
  setup: |-
    #!/bin/sh
    set -eu
    mkdir -m 0777 -p "\$VOL_DIR"
  teardown: |-
    #!/bin/sh
    set -eu
    rm -rf "\$VOL_DIR"
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: coxswain-rwx-provisioner, namespace: $NS}
spec:
  replicas: 1
  selector:
    matchLabels: {app: coxswain-rwx-provisioner}
  template:
    metadata:
      labels: {app: coxswain-rwx-provisioner}
      annotations: {coxswain.io/config-sum: "$CFG_SUM"}
    spec:
      serviceAccountName: coxswain-rwx-provisioner
      containers:
      - name: provisioner
        image: $IMAGE
        imagePullPolicy: IfNotPresent
        command:
        - local-path-provisioner
        - start
        - --config=/etc/config/config.json
        - --configmap-name=coxswain-rwx-config
        - --service-account-name=coxswain-rwx-provisioner
        - --provisioner-name=$PROV
        - --helper-image=$HELPER
        - --helper-pod-file=/etc/config/helperPod.yaml
        env:
        - name: POD_NAMESPACE
          valueFrom: {fieldRef: {fieldPath: metadata.namespace}}
        volumeMounts:
        - {name: config, mountPath: /etc/config}
      volumes:
      - name: config
        configMap: {name: coxswain-rwx-config}
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: $SC}
provisioner: $PROV
volumeBindingMode: Immediate
reclaimPolicy: Delete
YAML

kc -n "$NS" rollout status deployment/coxswain-rwx-provisioner --timeout=180s
echo "OK: StorageClass $SC (RWX, provisioner $PROV) ready; use --memory-storage-class=$SC"
