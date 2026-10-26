/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/pkg/memkey"
)

// P5a (Phase 5 shared memory, PLAN-PHASE5.md:34; D66 accepted): the per-
// (namespace, git URL) memory PVC + the copy-in init container.
//
// The PVC. A memory PVC per (namespace, git URL) named memory-<url-hash> in
// the Loop's namespace (memkey.PVCName). A PVC is mountable only by pods in
// its own namespace, and sandboxes run in the Loop's namespace (D66) — so
// Loops on the same repo in the same namespace share one PVC, and Loops in
// different namespaces get distinct PVCs (cross-namespace sharing is out of
// scope for the first cut). It is NOT a per-Loop PVC (contrast the workspace
// PVC <loop>-workspace, which is per-Loop and Loop-owned): the memory PVC is
// SHARED across all Loops on the same repo in the namespace and is NOT
// Loop-owned (a controller owner ref would garbage-collect it when the first
// Loop to create it is deleted, wiping the repo's lessons for every other
// Loop). It survives Loops deliberately; cox memory export/import is the
// manual cross-cluster path (never automatic).
//
// Access mode: ReadWriteMany (RWO would not schedule — several sandboxes on
// different nodes mounting one RWO PVC fail to place). On clusters whose
// storage class lacks RWX support (kind's local-path), the PVC stays Pending
// and the copy-in below is SKIPPED (the operator must not mount an unbound
// PVC into the sandbox — the kubelet would never start the pod). The
// degradation is VISIBLE, never silent (R26 I90): an Event (reason
// MemoryPVCUnbound) is emitted — re-announced once per quiet window while the
// PVC stays unbound (R39 I144) — and the Loop runs without memory.
//
// The copy-in init container (D66 fallback, universal). Rather than a
// long-lived RWX mount into the agent (the agent would hold a writable shared
// volume while untrusted), the memory is COPIED at every phase start: a
// trusted-image init container (--workspace-git-image, the same trusted
// clone image, non-root, read-only rootfs) mounts the memory PVC READ-ONLY
// at /memory and the workspace PVC (the operator-owned .coxswain dir) at
// /workspace, and copies /memory/* into /workspace/.coxswain/memory/. It
// runs BEFORE the phase-init container (init-order: workspace-init, copy-in,
// phase-init, agent). A fresh repo (an empty memory PVC) leaves NO .coxswain/
// memory dir at all (the runner's prompt is unchanged when the dir is
// absent). The agent container never mounts the memory PVC — memory is
// read-only in the agent (it lands in the workspace's operator-owned
// .coxswain dir, which the agent can read but which the runner's commit
// excludes, ADR-0004).
const (
	// memoryPVCVolumeName is the sandbox pod's volume name for the memory PVC
	// (mounted into the copy-in init container ONLY).
	memoryPVCVolumeName = "memory-pvc"
	// memoryCopyInContainerName is the copy-in init container's name.
	memoryCopyInContainerName = "memory-copy-in"
	// memoryMountPath is the copy-in init container's READ-ONLY mount of the
	// memory PVC.
	memoryMountPath = "/memory"
	// memoryCopyDestPath is the copy-in init container's destination (the
	// workspace's operator-owned .coxswain/memory dir, on the workspace PVC).
	memoryCopyDestPath = "/workspace/.coxswain/" + memkey.MemoryDirName
	// memoryPVCUnboundReason is the Event reason for the visible degradation
	// when the memory PVC is not Bound (the RWX cluster lacks the driver, or
	// the PVC is still provisioning): the copy-in is skipped, the Loop runs
	// without memory, and the Event is the record (R26 I90). It re-announces
	// once per memoryPVCUnboundQuiet while the PVC stays unbound (R39 I144):
	// an Event that is emitted once is invisible after --event-ttl (1h), which
	// is the window a non-RWX class leaves the PVC unbound for.
	memoryPVCUnboundReason = "MemoryPVCUnbound"
	// memoryPVCUnboundNotifiedAnnotation is the operator-owned quiet-window
	// marker on the Loop: the TIME of the last MemoryPVCUnbound Event
	// (RFC3339, F48f/I144). A pre-I144 `true` value parses as "not recently
	// notified", so an upgrade announces once and rewrites the marker as a
	// time. The key is unchanged so an upgraded operator reads it.
	memoryPVCUnboundNotifiedAnnotation = "coxswain.io/memory-pvc-unbound-notified"
	// memoryPVCUnboundQuiet is how long the marker mutes a repeat Event. It
	// matches the sibling surfaces (F48b's events-unreadable, F83b's foreign
	// verify Job) rather than being its own number.
	memoryPVCUnboundQuiet = 5 * time.Minute
	// memoryCopyInScriptPath: the copy-in script's fixed DEST (the /workspace
	// mount). A named constant so the execution test can substitute it.
	memoryCopyInDestSub = "__DEST__"
)

// memoryPVCName returns the Loop's memory PVC name: memory-<url-hash>, keyed
// by (namespace, git URL) — the name is namespace-scoped by the PVC object
// itself (memkey.PVCName is a pure function of the URL). "" for a repo-less
// Loop (no memory).
func memoryPVCName(loop *coxv1alpha1.Loop) string {
	return memkey.PVCName(loop.Spec.Workspace.Repo)
}

// memoryPVCSpec returns the memory PVC's spec: ReadWriteMany (RWO would not
// schedule several sandboxes on different nodes), 1Gi (a repo's lessons are
// bounded — one file per lesson, ≤500 chars each, P5b), default StorageClass
// (nil = the cluster default). The access mode is RWX PER THE PLAN; on a
// storage class that lacks RWX the PVC stays Pending and the copy-in is
// skipped (the Event surfaces the degradation).
func memoryPVCSpec(loop *coxv1alpha1.Loop) *corev1.PersistentVolumeClaim {
	oneGi := resource.MustParse("1Gi")
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      memoryPVCName(loop),
			Namespace: loop.Namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			// ReadWriteMany (NOT RWO): several sandboxes on different nodes
			// must all mount it. The mutation "use RWO instead of RWX" must
			// make the envtest accessModes spec FAIL.
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: oneGi,
				},
			},
		},
	}
}

// ensureMemoryPVC creates the memory PVC if it does not exist (idempotent:
// an existing PVC is left BYTE-IDENTICAL — no spec rewrite, no resourceVersion
// churn on a no-change reconcile, the same I43 rule as the workspace PVC; a
// phase recycle must NOT replace it). Unlike the workspace PVC, it is NOT
// Loop-owned (no controller owner ref): it is shared across Loops on the same
// repo in the namespace and must outlive the first Loop to create it (an
// owner ref would GC it on the first Loop's delete, wiping the repo's
// lessons). A repo-less Loop has no memory PVC (""). Returns an error only on
// a real read/create failure.
func (r *LoopReconciler) ensureMemoryPVC(ctx context.Context, loop *coxv1alpha1.Loop) error {
	name := memoryPVCName(loop)
	if name == "" {
		return nil // repo-less Loop: no memory
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: loop.Namespace,
		},
	}
	err := r.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: name}, pvc)
	if apierrors.IsNotFound(err) {
		pvc.Spec = memoryPVCSpec(loop).Spec
		// P5a: the memory PVC's StorageClass (the operator's
		// --memory-storage-class flag; empty = the cluster default).
		// On kind clusters whose default SC is RWO-only (rancher.io/local-path,
		// nodePathMap), set this to a RWX-capable class (e.g. local-shared) so
		// the PVC can be Bound and the copy-in init container runs.
		if r.MemoryStorageClass != "" {
			sc := r.MemoryStorageClass
			pvc.Spec.StorageClassName = &sc
		}
		// NO controller owner ref (contrast ensureWorkspacePVC): the memory
		// PVC is shared across Loops and must outlive any single Loop.
		if cerr := r.Create(ctx, pvc); cerr != nil {
			return fmt.Errorf("create memory PVC %s/%s: %w", loop.Namespace, name, cerr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read memory PVC %s/%s: %w", loop.Namespace, name, err)
	}
	// An existing PVC is left untouched (no owner ref to assert, no spec
	// rewrite): a Loop in the same namespace on the same repo reuses it.
	return nil
}

// memoryPVCBound reports whether the Loop's memory PVC is Bound (the copy-in
// init container must NOT be mounted when it is not — a Pending PVC would
// keep the kubelet from starting the pod). A repo-less Loop (no PVC) returns
// false with a nil error (the caller treats it as "no memory, no Event"). A
// real read failure (other than NotFound) is surfaced: the caller must not
// silently skip memory on an API error (fail-closed, the needsEgressProxy
// pattern).
func (r *LoopReconciler) memoryPVCBound(ctx context.Context, loop *coxv1alpha1.Loop) (bool, error) {
	name := memoryPVCName(loop)
	if name == "" {
		return false, nil
	}
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: name}, pvc)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read memory PVC %s/%s: %w", loop.Namespace, name, err)
	}
	return pvc.Status.Phase == corev1.ClaimBound, nil
}

// surfaceMemoryPVCUnbound surfaces the visible degradation (R26 I90): the
// memory PVC is not Bound (the storage class lacks RWX, or it is still
// provisioning), so the copy-in init container is SKIPPED and the Loop runs
// without memory.
//
// R39 I144: the Event was a one-shot (a boolean marker, cleared only when the
// PVC bound), so a PVC unbound for longer than the cluster's --event-ttl (1h,
// the case docs/UPGRADE.md names for a non-RWX class) left the degradation
// with no surface at all. It now goes through the shared quiet-window marker
// (F83f, loop_quiet_marker.go): the marker holds the last Event's TIME and the
// Event re-announces once per memoryPVCUnboundQuiet for as long as the PVC
// stays unbound. That keeps it a signal rather than a per-reconcile noise
// source (every envtest Loop with a repo has a Pending memory PVC) while
// making sure it never goes silent for the Loop's life.
//
// Best-effort: a nil Recorder skips the Event, and a failed marker patch only
// means the Event may repeat on the next pass. Neither blocks the reconcile.
//
// It returns how long until the marker's window expires, so the caller can
// SCHEDULE the re-announce (I144 review P2-1): the Event only repeats on a
// reconcile, and a Loop that is merely waiting for its PVC to bind has nothing
// else to wake it, so "re-announces every 5 minutes" needs a timer, not just a
// window. 0 would mean the window is already open (the surface just emitted, so
// the wait is the whole window).
func (r *LoopReconciler) surfaceMemoryPVCUnbound(ctx context.Context, loop *coxv1alpha1.Loop, pvcName string) time.Duration {
	r.markAndSurfaceQuietly(ctx, loop, quietMarker{
		key:    memoryPVCUnboundNotifiedAnnotation,
		quiet:  memoryPVCUnboundQuiet,
		reason: memoryPVCUnboundReason,
		message: fmt.Sprintf("memory PVC %s is not Bound (accessModes ReadWriteMany; the cluster storage class may not support RWX) — the Loop runs without shared memory (the copy-in init is skipped)",
			pvcName),
	})
	// markAndSurfaceQuietly refreshed the marker in memory (it writes the
	// emission time, or leaves the previous one when this pass was muted).
	if at, ok := quietMarkerNotifiedAt(loop.Annotations[memoryPVCUnboundNotifiedAnnotation]); ok {
		if d := at.Add(memoryPVCUnboundQuiet).Sub(r.operatorNow().Time); d > 0 {
			return d
		}
	}
	return memoryPVCUnboundQuiet
}

// memoryCopyInScript returns the copy-in init container's sh script: copy the
// memory PVC's contents (mounted read-only at /memory) into the workspace's
// .coxswain/memory/ (on the workspace PVC, mounted at /workspace). The script
// is a real, executable shell script (the execution test runs the ACTUAL
// generated script with /memory and /workspace substituted to tmp dirs — the
// AGENTS.md test norm: no paraphrased logic).
//
// Semantics:
//   - An EMPTY /memory (a fresh repo, no lessons yet) leaves NO .coxswain/
//     memory dir at all (the runner's prompt is unchanged when the dir is
//     absent). The dir is created ONLY when there is something to copy.
//   - A NON-EMPTY /memory is copied in, ADDING files (never deleting an
//     existing file that is not in the PVC — memory files are written only by
//     P5b's writer Job, and the copy-in runs at every phase start, so a
//     lesson written between phase starts lands in the PVC and the next
//     copy-in adds it; a file that exists in the workspace but not the PVC
//     is left alone — the copy-in is an additive read of the PVC).
//   - Idempotent: a second copy-in on the same contents is a no-op (cp -a
//     overwrites files with the same name; the contents are identical).
//
// The DEST is a placeholder (memoryCopyInDestSub) so the execution test can
// substitute the workspace path without re-implementing the script.
func memoryCopyInScript() string {
	return `#!/bin/sh
set -eu
DEST=` + memoryCopyInDestSub + `
SRC=/memory
# A fresh repo: the memory PVC is empty (no lessons yet). Leave NO
# .coxswain/memory dir at all — the runner's prompt is unchanged when the
# dir is absent (the P5a spec: "unchanged when absent (a fresh repo)").
if [ -z "$(find "${SRC}" -mindepth 1 -maxdepth 1 2>/dev/null)" ]; then
  exit 0
fi
mkdir -p "${DEST}"
# Additive copy: copy the PVC's files into DEST, never deleting a file that
# is not in the PVC (memory is written only by P5b's writer Job; a file in
# the workspace but not the PVC is left alone). cp -a preserves the tree.
find "${SRC}" -mindepth 1 -maxdepth 1 | while IFS= read -r p; do
  cp -a "${p}" "${DEST}/"
done
`
}

// memoryCopyInScriptForWorkspace returns the copy-in script with the DEST
// placeholder substituted to the workspace's .coxswain/memory path (the
// sandbox pod's real path; the execution test substitutes its own tmp path).
func memoryCopyInScriptForWorkspace() string {
	return strings.Replace(memoryCopyInScript(), memoryCopyInDestSub, memoryCopyDestPath, 1)
}

// memoryCopyInContainer returns the copy-in init container: the trusted
// --workspace-git-image (busybox sh + cp + find; the same trusted image the
// workspace init runs — the operator selects it, the agent's image never
// controls the copy), running the real generated script. It mounts the memory
// PVC READ-ONLY at /memory and the workspace PVC (RW, the operator-owned
// .coxswain dir) at /workspace. It runs as non-root (65532, the coxswain
// non-root UID) with a read-only rootfs, all caps dropped, and seccomp — the
// same hardening as the workspace init container (ADR-0006: a trusted,
// hardened boundary; no credentials here, the PVC is read-only).
//
// The image is the WORKSPACE GIT image, not the runner: the copy-in needs
// only sh + cp + find (no model, no git), and the git image is the
// operator-selected trusted image already present on the cluster (the
// workspace init uses it). It deliberately does NOT reuse the runner image
// (the copy-in must not depend on the runner's build).
func (r *LoopReconciler) memoryCopyInContainer() corev1.Container {
	nonRootUID := int64(65532)
	nonRootGID := int64(65532)
	falseP := false
	trueP := true
	readOnlyRootfs := true
	return corev1.Container{
		Name:    memoryCopyInContainerName,
		Image:   r.workspaceGitImage(),
		Command: []string{verifySh, "-c", memoryCopyInScriptForWorkspace()},
		VolumeMounts: []corev1.VolumeMount{
			// The memory PVC, READ-ONLY (the copy-in is a pure read of the PVC;
			// the agent never mounts the memory PVC).
			{Name: memoryPVCVolumeName, MountPath: memoryMountPath, ReadOnly: true},
			// The workspace PVC (RW — the copy-in writes the .coxswain/memory
			// dir, which is the operator-owned result dir, ADR-0004).
			{Name: workspaceVolumeName, MountPath: agentWorkspaceMount},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &falseP,
			RunAsNonRoot:             &trueP,
			RunAsUser:                &nonRootUID,
			RunAsGroup:               &nonRootGID,
			ReadOnlyRootFilesystem:   &readOnlyRootfs,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allCaps}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
}

// memoryVolume returns the sandbox pod's volume for the memory PVC (mounted
// into the copy-in init container ONLY — the agent never mounts it).
func memoryVolume(pvcName string) corev1.Volume {
	return corev1.Volume{
		Name: memoryPVCVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvcName,
				ReadOnly:  true,
			},
		},
	}
}

// appendMemoryCopyIn returns initContainers+volumes with the memory copy-in
// init container and its volume appended, when memoryBound is true (the
// memory PVC is Bound; a repo-less Loop or an unbound PVC returns the inputs
// unchanged). The copy-in runs BEFORE the phase-init container (init-order:
// workspace-init, copy-in, phase-init, agent), so the .coxswain/memory dir
// exists before the phase-init writes the desired-phase (the phase-init does
// not touch the memory dir; the order is for the agent, which reads
// .coxswain/memory after both inits).
func appendMemoryCopyIn(initContainers []corev1.Container, volumes []corev1.Volume,
	memoryBound bool, pvcName string, r *LoopReconciler) ([]corev1.Container, []corev1.Volume) {
	if !memoryBound || pvcName == "" {
		return initContainers, volumes
	}
	// Insert the copy-in BEFORE the phase-init container (the phase-init is
	// the LAST init container in the current order: workspace-init, then
	// phase-init). The phase-init's position is its index (it is appended
	// last in agentPodSpec). The copy-in goes immediately before it so the
	// memory dir exists before the phase-init runs (the phase-init does not
	// depend on it, but the order keeps the agent's view of .coxswain stable:
	// memory first, then the phase marker).
	// agentPodSpec appends workspace-init (if first clone) then phase-init.
	// The copy-in must run AFTER workspace-init (the .coxswain dir is created
	// by workspace-init on a fresh PVC) and BEFORE phase-init. Insert before
	// the phase-init (the last init container); if it is not present (it is
	// always present in agentPodSpec), append.
	idx := len(initContainers)
	for i := range initContainers {
		if initContainers[i].Name == phaseInitContainerName {
			idx = i
			break
		}
	}
	out := make([]corev1.Container, 0, len(initContainers)+1)
	out = append(out, initContainers[:idx]...)
	out = append(out, r.memoryCopyInContainer())
	out = append(out, initContainers[idx:]...)
	volumes = append(volumes, memoryVolume(pvcName))
	return out, volumes
}
