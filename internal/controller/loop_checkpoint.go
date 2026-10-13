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

// B1 (PLAN-BETA, PLAN.md Phase 3): workspace checkpoints after each
// successful verify.
//
// The operator takes a VolumeSnapshot of the Loop's workspace PVC after each
// successful verify, named checkpoint-<loop>-iter-<N>, records the latest in
// status.checkpoint and keeps the full status list (status.checkpoints).
// Retention is the last 3 plus every milestone (successful-verify)
// checkpoint; the rest are GC'd (their VolumeSnapshots deleted). A finalizer
// deletes the Loop's snapshots in CleaningUp.
//
// D59 (owner decision): checkpoints are mandatory only when the Loop asks
// for them. spec.checkpoints: BestEffort (the default) takes snapshots when
// possible — when the VolumeSnapshot CRD exists AND the workspace PVC's CSI
// driver has a VolumeSnapshotClass — and otherwise sets
// CheckpointsAvailable=False with a named reason and carries on with the A2
// PVC-resume (a VISIBLE fallback, I90). spec.checkpoints: Required fails at
// start (terminal Failed:CheckpointUnavailable) when they are not possible.
//
// I88: the VolumeSnapshot watch is registered ONLY if the RESTMapper
// resolves the CRD at setup (the KubeArmorPolicy CRD-gated-watch pattern) —
// a manager on a cluster without the snapshot CRD must start. The
// reconcile-side reads go through the cached client (r.List/r.Get) and are
// gated on the CRD being present (a List on an absent CRD would make the
// manager cache permanently error, and the operator would not start).

package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// The snapshot types are handled as unstructured (the operator never
// imports the external-snapshotter client module): the CRDs are not part of
// k8s.io/api and the operator reads them through the manager's cache as
// unstructured.Unstructured with these GVKs.

// GVKs for the VolumeSnapshot family (snapshot.storage.k8s.io/v1).
const (
	// snapshotGroup is the API group for the VolumeSnapshot family.
	snapshotGroup = "snapshot.storage.k8s.io"

	snapshotGV = "snapshot.storage.k8s.io/v1"

	volumeSnapshotKind        = "VolumeSnapshot"
	volumeSnapshotListKind    = "VolumeSnapshotList"
	volumeSnapshotClassKind   = "VolumeSnapshotClass"
	volumeSnapshotContentKind = "VolumeSnapshotContent"
)

// snapshotGroupRef is the addressable form of snapshotGroup (the PVC
// dataSource's APIGroup field needs a *string; the const itself cannot be
// addressed).
var snapshotGroupRef = snapshotGroup

// volumeSnapshotGVK is the GVK the operator creates and reads for the
// per-Loop checkpoints (the unstructured watch + the cached List/Get).
var volumeSnapshotGVK = schema.GroupVersionKind{Group: snapshotGroup, Version: "v1", Kind: volumeSnapshotKind}

// volumeSnapshotListGVK is the GVK the operator's cached client uses for
// List (controller-runtime derives the list type from the Kind + "List").
var volumeSnapshotListGVK = schema.GroupVersionKind{Group: snapshotGroup, Version: "v1", Kind: volumeSnapshotListKind}

// B1 checkpoint condition reasons (the CheckpointsAvailableCondition's
// named reasons; D59 requires the fallback to be NAMED and visible).
const (
	// CheckpointReasonAvailable: checkpoints are being taken.
	CheckpointReasonAvailable = "Available"
	// CheckpointReasonCRDMissing: the VolumeSnapshot CRD is not installed
	// (the RESTMapper does not resolve it).
	CheckpointReasonCRDMissing = "VolumeSnapshotCRDMissing"
	// CheckpointReasonNoSnapshotClass: no VolumeSnapshotClass exists for the
	// workspace PVC's CSI driver (the StorageClass's provisioner).
	CheckpointReasonNoSnapshotClass = "NoSnapshotClassForDriver"
	// CheckpointReasonCreateFailed: a checkpoint create/update attempt
	// failed (visible, retried next reconcile).
	CheckpointReasonCreateFailed = "SnapshotCreateFailed"
)

// B1 terminal-fail reason (spec.checkpoints=Required, no snapshot support).
const checkpointUnavailableReason = "CheckpointUnavailable"

// checkpointRetention is the number of most-recent checkpoints kept beyond
// the milestones (B1 retention: the last 3 plus every milestone checkpoint).
const checkpointRetention = 3

// loopCheckpointFinalizer is the finalizer the operator adds to every Loop
// (B1): CleaningUp waits for the finalizer's work (the Loop's
// VolumeSnapshots are deleted) before the Loop can be removed.
const loopCheckpointFinalizer = "coxswain.wattu.com/checkpoints"

// checkpointName is the checkpoint name for a Loop and iteration:
// checkpoint-<loop>-iter-<N> (B1).
func checkpointName(loop *coxv1alpha1.Loop, iteration int) string {
	return "checkpoint-" + loop.Name + "-iter-" + strconv.Itoa(iteration)
}

// newVolumeSnapshotObj returns an unstructured VolumeSnapshot for the Loop's
// workspace PVC checkpoint (B1). The spec.source.persistentVolumeClaimName
// names the PVC; snapshotClassName names the VolumeSnapshotClass (the
// operator passes the one it already resolved for the PVC's CSI driver —
// setting it means the snapshot-controller does not need to find a
// DEFAULT class, which is more robust across snapshot-controller versions
// (v8.6.0's default-class lookup is unreliable in some clusters, while an
// explicit class name is always honored)). snapshotClassName is optional: an
// empty string omits it (the envtest specs pass "" — envtest has no
// snapshot-controller, so the class name is inert there).
//
// (The deletionPolicy is intentionally left unset: defaulting is the
// external-snapshotter controller's job (the CRD does not default it), and
// an operator that pins it here would fight a controller version that
// defaults it differently. B2's restore reads the snapshot; the content's
// deletionPolicy governs the content's life, not the snapshot object's.)
func newVolumeSnapshotObj(loop *coxv1alpha1.Loop, name, snapshotClassName string) *unstructured.Unstructured {
	spec := map[string]any{
		"source": map[string]any{
			"persistentVolumeClaimName": workspacePVCName(loop.Name),
		},
	}
	if snapshotClassName != "" {
		spec["volumeSnapshotClassName"] = snapshotClassName
	}
	return &unstructured.Unstructured{Object: map[string]any{
		unstructuredAPI:  snapshotGV,
		unstructuredKind: volumeSnapshotKind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": loop.Namespace,
			"labels": map[string]any{
				managedByLabel: partOfCoxswain,
				loopLabelKey:   loop.Name,
			},
		},
		"spec": spec,
	}}
}

// unstructuredAPI is the "apiVersion" key (a goconst-shared constant across
// the controller's unstructured object builders; defined here because the
// loop_testfixture_test.go constant is in a _test file and not visible to
// production code). The value is the K8s API version key, not a loop-specific
// value.
const unstructuredAPI = "apiVersion"

// unstructuredKind is the "kind" key (a goconst-shared constant across the
// controller's unstructured object builders; defined here because the
// loop_testfixture_test.go constant is in a _test file and not visible to
// production code). The value is the K8s kind key, not a loop-specific value.
const unstructuredKind = "kind"

// snapshotCRDPresent reports whether the VolumeSnapshot CRD is resolvable
// (the I88 gate for the watch + the reconcile-side reads). It is a FUNCTION
// FIELD (not a method) so a test can override it (the no-CRD spec sets it
// to return false without wiring a RESTMapper that does not resolve the
// GVK). The default (nil field) reads r.RESTMapper: a List on an absent
// CRD would make the manager's cache for the type error on every watch
// refresh (the operator would log errors forever and the cache would never
// be ready), so the operator only issues snapshot reads when the CRD is
// present. The cache's lazy watch starts on the FIRST read; gating the
// first read on the RESTMapper is what keeps a CRD-less cluster clean. A
// nil mapper (a bare test reconciler without SetupWithManager) is treated
// as PRESENT (the test envtest has the CRD).
func (r *LoopReconciler) snapshotCRDPresentImpl(ctx context.Context) bool {
	if r.snapshotCRDPresent != nil {
		return r.snapshotCRDPresent(ctx)
	}
	if r.RESTMapper == nil {
		return true
	}
	_, err := r.RESTMapper.RESTMapping(volumeSnapshotGVK.GroupKind())
	return err == nil
}

// snapshotCRDPresent is the call site's entry: it routes to the override
// (a test) or the live RESTMapper read (production + the default test).
func (r *LoopReconciler) snapshotCRDPresentCall(ctx context.Context) bool {
	return r.snapshotCRDPresentImpl(ctx)
}

// workspaceDriver resolves the workspace PVC's CSI driver (the provisioner).
// It is the LIVE read: the PVC's storageClassName resolves to a
// StorageClass whose provisioner is the CSI driver a VolumeSnapshotClass
// must match. A test drives the B1 checkpoint seam instead
// (r.workspaceDriverSeam), because the workspace PVC is created with a nil
// storageClassName (the cluster's default StorageClass), and the live read
// returns ("", false) for a nil-named PVC (the default is not
// name-addressable without reading every StorageClass's is-default
// annotation) — the seam is the test's way to assert the no-support /
// support branches without a real StorageClass.
//
// Returns ("", false) when the PVC does not exist yet, names no
// storageClassName, or its StorageClass cannot be resolved (the support check
// is deferred to a later reconcile).
func (r *LoopReconciler) workspaceDriver(ctx context.Context, loop *coxv1alpha1.Loop) (string, bool) {
	if r.workspaceDriverSeam != nil {
		return r.workspaceDriverSeam(ctx, loop)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Namespace: loop.Namespace, Name: workspacePVCName(loop.Name)}
	if err := r.Get(ctx, key, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false // the PVC does not exist yet
		}
		return "", false // a transient read failure defers the check (not an error)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		// The PVC uses the cluster's default StorageClass (S4/R17 creates it
		// with a nil storageClassName). The default is not name-addressable
		// without reading the is-default-StorageClass annotation across every
		// StorageClass; B1 treats a nil-named PVC as driver-unknown (the
		// conservative reading). A cluster whose default StorageClass does not
		// support snapshots is the same class of "no support" the BestEffort
		// fallback covers, so the check's answer is the same either way.
		return "", false
	}
	sc := &storagev1.StorageClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: *pvc.Spec.StorageClassName}, sc); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false // the named StorageClass is gone (deferral)
		}
		return "", false
	}
	return sc.Provisioner, true
}

// checkpointSupport returns the CheckpointsAvailable condition's (status,
// reason, message) for the Loop's current snapshot support, computed from:
//
//   - the VolumeSnapshot CRD present (snapshotCRDPresent, I88), and
//   - the workspace PVC's CSI driver has a VolumeSnapshotClass
//     (snapshotClassForDriver).
//
// It returns (True, Available, "") when both hold, and (False, reason,
// message) otherwise. The CRD-missing case wins over the driver case: a
// cluster without the CRD has no VolumeSnapshotClass to look up.
//
// When the PVC does not exist yet (the Loop has not created its workspace)
// the driver is unknown; the check reports (True, Available, "") is NOT
// correct here — the honest answer is Unknown until the PVC exists. But the
// operator only needs the check to be right at two moments: (1) at start,
// for the Required gate (where the PVC exists — ensureLoopArtifacts runs
// before the gate), and (2) after a successful verify, to decide whether to
// take a checkpoint (where the PVC has existed since Planning). So the
// no-PVC case cannot occur at those moments; it is reported as
// (False, NoSnapshotClassForDriver, "the workspace PVC does not exist yet")
// — the conservative reading (do not claim availability the operator has
// not yet been able to verify).
func (r *LoopReconciler) checkpointSupport(ctx context.Context, loop *coxv1alpha1.Loop) (metav1.ConditionStatus, string, string) {
	policy := effectiveCheckpointPolicy(loop)
	if !r.snapshotCRDPresentCall(ctx) {
		return metav1.ConditionFalse, CheckpointReasonCRDMissing,
			"the VolumeSnapshot CRD is not installed (spec.checkpoints=" + string(policy) + ": " + checkpointPolicyCarryOn(policy) + ")"
	}
	driver, known := r.workspaceDriver(ctx, loop)
	if !known {
		// The PVC's driver is not known yet (the PVC does not exist, names no
		// storageClassName, or the StorageClass cannot be resolved). The check
		// is DEFERRED (the condition is Unknown, not False — the operator has
		// not yet been able to verify support; a False here would be a claim
		// of "not possible" the operator has not made). By the time a
		// checkpoint is due (after the first successful verify) the PVC and
		// its StorageClass exist (ensureLoopArtifacts created them at start),
		// so the check resolves on the next reconcile.
		return metav1.ConditionUnknown, CheckpointReasonNoSnapshotClass,
			"the workspace PVC's CSI driver is not known yet (the PVC does not exist, names no StorageClass, or the StorageClass cannot be resolved); the snapshot support check is deferred"
	}
	if _, ok := r.snapshotClassForDriver(ctx, driver); !ok {
		return metav1.ConditionFalse, CheckpointReasonNoSnapshotClass,
			"no VolumeSnapshotClass exists for the workspace PVC's CSI driver " + driver +
				" (spec.checkpoints=" + string(policy) + ": " + checkpointPolicyCarryOn(policy) + ")"
	}
	return metav1.ConditionTrue, CheckpointReasonAvailable,
		"volume-snapshot checkpoints are being taken after each successful verify (driver " + driver + ")"
}

// effectiveCheckpointPolicy returns the Loop's effective checkpoint policy
// (D59): spec.checkpoints, defaulting to BestEffort when unset (the CRD
// default; the constant mirrors it for bare reconcilers that skip the CRD
// defaulting).
func effectiveCheckpointPolicy(loop *coxv1alpha1.Loop) coxv1alpha1.CheckpointPolicy {
	if loop.Spec.Checkpoints == "" {
		return coxv1alpha1.CheckpointPolicyBestEffort
	}
	return loop.Spec.Checkpoints
}

// checkpointPolicyCarryOn renders the carry-on wording for the
// CheckpointsAvailable message (BestEffort carries on; Required does not
// get here — Required without support is terminal at start, before the
// condition is set to False).
func checkpointPolicyCarryOn(p coxv1alpha1.CheckpointPolicy) string {
	switch p {
	case coxv1alpha1.CheckpointPolicyRequired:
		return "the Loop fails at start with a terminal CheckpointUnavailable, so it never runs"
	default:
		return "the Loop carries on with the A2 PVC-resume"
	}
}

// applyCheckpointConditions runs the checkpoint support check and folds the
// result into the CheckpointsAvailable condition. It returns
// (changed, terminal, terminalReason): terminal is true only for
// spec.checkpoints=Required with no support — the caller fails the Loop
// (terminal Failed:CheckpointUnavailable, at start, before any phase work).
//
// The condition is set on EVERY reconcile (the support may change: the
// snapshot CRD can be installed after the Loop starts, and a
// VolumeSnapshotClass can be created later). A flip from False to True
// clears the Required terminal only if it has not yet fired (the gate runs
// at start; once the Loop is Failed, nothing reverts it).
func (r *LoopReconciler) applyCheckpointConditions(ctx context.Context, loop *coxv1alpha1.Loop) (changed bool, terminal bool) {
	// The support check is gated on the CRD being present: when the CRD is
	// absent there is nothing to read (the RESTMapper is the check, and a
	// cache List on the absent CRD would wedge the manager's cache).
	// snapshotCRDPresent is called inside checkpointSupport; the driver and
	// VSC reads only run when the CRD is present.
	status, reason, message := r.checkpointSupport(ctx, loop)
	meta.SetStatusCondition(&loop.Status.Conditions, metav1.Condition{
		Type:    coxv1alpha1.CheckpointsAvailableCondition,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
	changed = true
	// D59 Required: fail at start (terminal) when checkpoints are not
	// possible. "At start" is the phase-bootstrap moment (Pending ->
	// Planning): the gate fires once, on the FIRST reconcile after the PVC
	// exists. The PVC is created by ensureLoopArtifacts before the first
	// phase work, so the check at the bootstrap point has the PVC. A
	// ConditionUnknown (the driver is not known yet) is NOT a gate trigger
	// (the check is deferred, not failed).
	if status == metav1.ConditionFalse &&
		effectiveCheckpointPolicy(loop) == coxv1alpha1.CheckpointPolicyRequired &&
		loop.Status.Phase != coxv1alpha1.LoopPhaseFailed {
		terminal = true
	}
	return changed, terminal
}

// volumeSnapshotClassListGVK is the GVK the operator's cached client uses
// for the VolumeSnapshotClass List. The list is a NAMESPACED EMPTY list
// (listType=atomic, no namespace field): a cluster-scoped type read through
// a NAMESPACED client errors ("the server is trying to watch an object that
// doesn't exist" / the namespace is ignored), so the operator lists with
// list.SetNamespace("") and no InNamespace option (the standard idiom for
// cluster-scoped types through a namespaced cache — the same as the
// StorageClass read, which uses a bare-name key).
var volumeSnapshotClassListGVK = schema.GroupVersionKind{Group: snapshotGroup, Version: "v1", Kind: "VolumeSnapshotClassList"}

// snapshotClassForDriver lists the VolumeSnapshotClasses (cluster-scoped,
// unstructured, via a namespaced empty list) and returns the one whose
// driver matches. (None, false) when there is no match or the list fails (a
// list failure is treated as no match: the conservative reading — do not
// claim availability on a read error, and do not fail the Loop on a transient
// read either; the next reconcile re-reads).
func (r *LoopReconciler) snapshotClassForDriver(ctx context.Context, driver string) (*unstructured.Unstructured, bool) {
	// The CRD presence is the caller's responsibility (checkpointSupport
	// gates on it); a list on an absent CRD would error, and the operator
	// would treat the error as "no match" (the conservative reading).
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(volumeSnapshotClassListGVK)
	// cluster-scoped: the namespace must be empty (a cluster-scoped type read
	// through a namespaced client with a non-empty namespace errors).
	if err := unstructured.SetNestedField(list.Object, "", "metadata", "namespace"); err != nil {
		return nil, false
	}
	if err := r.List(ctx, list); err != nil {
		return nil, false
	}
	for i := range list.Items {
		item := &list.Items[i]
		// The vendored v1 VolumeSnapshotClass schema is FLAT (driver at the
		// top level, not under spec). Read both shapes for robustness (a
		// future CRD version could nest it; a real csi-hostpath VSC is flat).
		d, ok1, _ := unstructured.NestedString(item.Object, "driver")
		d2, ok2, _ := unstructured.NestedString(item.Object, "spec", "driver")
		drv := d
		if !ok1 {
			drv = d2
			ok1 = ok2
		}
		if ok1 && drv == driver {
			return item, true
		}
	}
	return nil, false
}

// ensureCheckpoint runs after a successful verify (B1): it takes the
// VolumeSnapshot for the just-completed iteration when support is present,
// records it in status.checkpoint + status.checkpoints, applies retention
// (GCs the non-kept checkpoints' VolumeSnapshots), and surfaces
// CheckpointsAvailable. It returns (changed, requeue): requeue is true when
// a snapshot object is being created (the operator re-reads it next
// reconcile to confirm it is recorded / the content is ReadyToUse — though
// B1's durability record is the snapshot OBJECT's existence, not the
// content's readiness; B2's restore waits for ReadyToUse).
func (r *LoopReconciler) ensureCheckpoint(ctx context.Context, loop *coxv1alpha1.Loop) (changed bool, requeue bool) {
	// The checkpoint is keyed off the operator-recorded verify outcome
	// (status.verify.iteration), not the current iteration. The current
	// iteration may have been incremented on the iterate path (N -> N+1),
	// so keying off it would lose the checkpoint for iteration N on retry
	// and could create a wrong checkpoint (iter-N+1 before N+1 has
	// implemented or verified anything).
	if loop.Status.Verify == nil || loop.Status.Verify.Iteration == nil {
		return false, false // no recorded verify outcome yet (iteration 0 is a valid recorded value)
	}
	iteration := int(*loop.Status.Verify.Iteration)
	// Idempotency: a checkpoint is taken ONCE per iteration. If
	// status.checkpoints already has an entry for this iteration, the
	// snapshot is already taken; do not re-create it.
	for _, cp := range loop.Status.Checkpoints {
		if cp.Iteration == iteration {
			return false, false
		}
	}
	// The support check gates the create (D59). When support is absent:
	// BestEffort carries on (the condition is False, set by
	// applyCheckpointConditions); Required is already terminal (the gate
	// fired at start) — nothing to do here either way.
	status, reason, message := r.checkpointSupport(ctx, loop)
	meta.SetStatusCondition(&loop.Status.Conditions, metav1.Condition{
		Type:    coxv1alpha1.CheckpointsAvailableCondition,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
	changed = true
	if status != metav1.ConditionTrue {
		return changed, false
	}
	// Resolve the VolumeSnapshotClass for the workspace PVC's CSI driver and
	// name it on the snapshot. The operator already resolved it for the
	// support check (checkpointSupport -> workspaceDriver ->
	// snapshotClassForDriver); resolve it here to get the NAME (the support
	// check only returns a bool). Setting the class name on the snapshot
	// means the snapshot-controller does not need to find a DEFAULT class
	// (more robust across snapshot-controller versions, and it is the
	// explicit, less ambiguous form). If the class has disappeared between
	// the support check and the create (rare), the create will fail
	// (surfaced via the condition + requeue, I90).
	snapshotClassName := ""
	if driver, ok := r.workspaceDriver(ctx, loop); ok {
		if vsc, found := r.snapshotClassForDriver(ctx, driver); found {
			snapshotClassName = vsc.GetName()
		}
	}
	// Create the snapshot (idempotent: the name is unique per iteration; a
	// AlreadyExists on the same-name object is the same checkpoint being
	// re-attempted, treated as recorded).
	name := checkpointName(loop, iteration)
	snap := newVolumeSnapshotObj(loop, name, snapshotClassName)
	err := r.Create(ctx, snap)
	if err != nil {
		if !isAlreadyExists(err) {
			// A create failure (other than AlreadyExists) is surfaced (I90:
			// fallbacks surface their failure) via the condition and
			// requeued; the Loop still Succeeds (B1: a checkpoint failure
			// does not fail the Loop — the verify succeeded; the durability
			// record is best-effort under BestEffort, and under Required the
			// Loop only got here because support WAS present at start, so a
			// late create failure is a degrade, surfaced, not a silent skip).
			meta.SetStatusCondition(&loop.Status.Conditions, metav1.Condition{
				Type:    coxv1alpha1.CheckpointsAvailableCondition,
				Status:  metav1.ConditionFalse,
				Reason:  CheckpointReasonCreateFailed,
				Message: "creating the checkpoint " + name + " failed: " + err.Error(),
			})
			return changed, true
		}
		// AlreadyExists: the snapshot object already exists (a previous
		// attempt created it but the status record was lost — e.g. a
		// controller restart between the create and the record). Treat it as
		// recorded (the object is there; the status record is rebuilt below).
	}
	// Record it: status.checkpoint (latest) + status.checkpoints (the list).
	at := nowTime(r)
	entry := coxv1alpha1.CheckpointStatus{
		Iteration: iteration,
		Name:      name,
		Milestone: verifyMilestone(loop), // milestone = all lastCheckResults exit codes are 0
		At:        at,
	}
	loop.Status.Checkpoint = &entry
	loop.Status.Checkpoints = append(loop.Status.Checkpoints, entry)
	// Retention: GC the non-kept checkpoints (the VolumeSnapshots behind the
	// status entries that are no longer in the keep-set).
	gcChanged, gcRequeue := r.applyCheckpointRetention(ctx, loop)
	return changed || gcChanged, gcRequeue
}

// isAlreadyExists reports whether the error is an apiserver AlreadyExists
// (the snapshot name is taken by the same object a previous attempt made).
func isAlreadyExists(err error) bool {
	return apierrors.IsAlreadyExists(err)
}

// applyCheckpointRetention applies the B1 retention policy: the last
// checkpointRetention (3) checkpoints plus every milestone checkpoint are
// kept; the rest are GC'd (their VolumeSnapshots deleted, the status entries
// removed). It returns (changed, requeue): changed is true when a snapshot
// was deleted or the status list shrank; requeue is true when a delete is
// pending (the object may not be gone yet — the operator re-reads next
// reconcile, though B1 does not block on the delete: the GC is best-effort
// per reconcile and a failed delete is retried next reconcile).
//
// The keep-set (B1 retention): a checkpoint is kept when it is a milestone
// OR it is in the last checkpointRetention entries. In B1 every checkpoint
// is a milestone, so the keep-set is every checkpoint (nothing is GC'd in
// B1) — the milestone flag is explicit in the API so B2's restore/fork can
// distinguish. The GC path (delete + status-entry removal) is exercised by
// the mutation test and a status with a non-milestone entry (a hand-built
// fixture, since B1's writer never produces one).
func (r *LoopReconciler) applyCheckpointRetention(ctx context.Context, loop *coxv1alpha1.Loop) (changed bool, requeue bool) {
	kept := keepCheckpoints(loop.Status.Checkpoints)
	if len(kept) == len(loop.Status.Checkpoints) {
		return false, false
	}
	// The GC'd entries are the status entries NOT in the keep-set. Delete
	// their VolumeSnapshots (best-effort: a NotFound means the object is
	// already gone — the GC is idempotent).
	keptNames := make(map[string]bool, len(kept))
	for _, k := range kept {
		keptNames[k.Name] = true
	}
	// The status entries are in creation order (the operator appends); the
	// keep-set preserves that order. Rebuild the list to the keep-set (the
	// GC'd entries are removed from the status), and delete the snapshots.
	var rebuilt []coxv1alpha1.CheckpointStatus
	for _, cp := range loop.Status.Checkpoints {
		if keptNames[cp.Name] {
			rebuilt = append(rebuilt, cp)
			continue
		}
		// GC this checkpoint's VolumeSnapshot (the class name is irrelevant
		// for a delete — it addresses the object by name).
		snap := newVolumeSnapshotObj(loop, cp.Name, "")
		if err := r.Delete(ctx, snap); err != nil && !isNotFound(err) {
			// A delete failure (other than NotFound) is surfaced + requeued
			// (the GC retries next reconcile; the status entry is kept so
			// the GC is not lost).
			logf.FromContext(ctx).Error(err, "checkpoint GC: delete VolumeSnapshot failed; retrying",
				"snapshot", cp.Name, "loop", loop.Name)
			rebuilt = append(rebuilt, cp)
			requeue = true
			continue
		}
		changed = true
	}
	loop.Status.Checkpoints = rebuilt
	// status.checkpoint is the latest (the last entry, when it survived).
	if n := len(rebuilt); n > 0 {
		loop.Status.Checkpoint = &rebuilt[n-1]
	} else {
		loop.Status.Checkpoint = nil
	}
	return changed, requeue
}

// keepCheckpoints returns the keep-set under the B1 retention policy: the
// last checkpointRetention entries plus every milestone entry. The order is
// preserved (creation order — the operator appends). A milestone that is
// also in the last-3 is kept once (the name set dedups).
func keepCheckpoints(cps []coxv1alpha1.CheckpointStatus) []coxv1alpha1.CheckpointStatus {
	if len(cps) <= checkpointRetention {
		return cps
	}
	keep := make(map[string]bool, len(cps))
	// The last checkpointRetention.
	for i := len(cps) - checkpointRetention; i < len(cps); i++ {
		keep[cps[i].Name] = true
	}
	// Every milestone.
	for i := range cps {
		if cps[i].Milestone {
			keep[cps[i].Name] = true
		}
	}
	var out []coxv1alpha1.CheckpointStatus
	for i := range cps {
		if keep[cps[i].Name] {
			out = append(out, cps[i])
		}
	}
	return out
}

// isNotFound reports whether the error is an apiserver NotFound (the GC's
// idempotent delete: a snapshot already gone is a success, not an error).
func isNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}

// gcLoopCheckpoints deletes the Loop's checkpoint VolumeSnapshots (B1
// finalizer: CleaningUp). It lists the snapshots by the Loop label and
// deletes each (NotFound is a success — the GC is idempotent). It returns
// (done, error): done is true when no snapshot remains (the caller removes
// the finalizer); error is a real delete failure (the finalizer stays, the
// reconcile requeues).
//
// B2 FORK-RETENTION: a checkpoint that a FORK Loop has seeded from (the
// fork's spec.forkOf names it) is NOT GC'd by the source's finalizer while
// the fork's restore is incomplete. The source's GC lists the snapshots by
// the source's loop label; a fork-seeded snapshot carries an additional
// label coxswain.wattu.com/fork-of = the fork's Loop name, and the source's
// GC SKIPS it. The fork adds the label on its OWN Loop (the fork writes its
// own spec.forkOf label onto the source's snapshot — a cross-Loop label
// write is the operator's own action, not an agent action; the operator
// already writes labels on its own objects). Once the fork's restore is
// COMPLETE (the fork's Restore condition is True/Restored), the fork's PVC
// owns its content (the content was materialised from the snapshot at
// restore), and the fork REMOVES the label — the source's GC is free to
// delete the snapshot (the fork no longer reads it). The interaction is
// therefore:
//
//   - source's snapshot, no fork label: the source's GC deletes it (B1
//     unchanged).
//   - source's snapshot, fork label present, fork's restore incomplete:
//     the source's GC SKIPS it (the fork still needs it).
//   - source's snapshot, fork label present, fork's restore complete:
//     the fork removed the label; the source's GC deletes it (the fork's
//     PVC is self-sufficient).
//
// Deleting the source Loop AFTER the fork's restore is complete does NOT
// break the fork: the fork's PVC holds the restored content (the snapshot
// was materialised into the PVC at restore), and the source's finalizer's
// GC (which runs on the source's delete) only deletes the source's
// snapshots — the fork's PVC is a separate object, controller-owned by the
// fork, and is NOT touched by the source's GC. The fork's own finalizer
// (the fork's loopCheckpointFinalizer) cleans up the FORK's own snapshots
// (the fork takes its own checkpoints after its own verifies — those are
// checkpoint-<fork>-iter-<N>, a different name, a different label value).
func (r *LoopReconciler) gcLoopCheckpoints(ctx context.Context, loop *coxv1alpha1.Loop) (done bool, err error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(volumeSnapshotListGVK)
	if !r.snapshotCRDPresentCall(ctx) {
		// No CRD: there are no snapshots to delete (the operator never
		// created any — the support check was False from the start). Done.
		return true, nil
	}
	if err := r.List(ctx, list,
		client.InNamespace(loop.Namespace),
		client.MatchingLabels{"coxswain.wattu.com/loop": loop.Name}); err != nil {
		return false, fmt.Errorf("list checkpoint VolumeSnapshots for GC: %w", err)
	}
	for i := range list.Items {
		item := list.Items[i].DeepCopy()
		// B2 FORK-RETENTION: a fork-seeded snapshot carries the label
		// coxswain.wattu.com/fork-of = the fork's Loop name (the fork's
		// operator added it when it seeded from the source's checkpoint). The
		// source's GC SKIPS it (the fork still needs it — the fork's restore
		// is incomplete, and deleting the source's snapshot now would break
		// the fork's PVC dataSource). The label is REMOVED by the fork once
		// the fork's restore is complete (the fork's operator removes it in
		// b2ApplyRestoreStep's ready-branch), after which the source's GC is
		// free to delete the snapshot (the fork's PVC is self-sufficient).
		// A snapshot WITHOUT the label is deleted (B1 unchanged).
		if _, ok := item.GetLabels()["coxswain.wattu.com/fork-of"]; ok {
			continue // a fork still needs this snapshot: skip it
		}
		if delErr := r.Delete(ctx, item); delErr != nil && !isNotFound(delErr) {
			return false, delErr
		}
	}
	// Done: re-list to confirm none remain (a delete may be async; the
	// re-list is the confirmation). When none remain, the caller removes
	// the finalizer. A fork-retained snapshot (the label present) is NOT
	// "remaining" for the finalizer's purposes: the source's work is done
	// with its own snapshots; the fork-retained one is the fork's
	// responsibility (the fork's operator removes the label when its
	// restore completes, and the source's GC will then delete it on the
	// source's NEXT reconcile — but the source is being deleted, so the
	// fork's label removal is the fork's own action, and the source's
	// finalizer is removed now, leaving the fork-retained snapshot to be
	// deleted by the fork's own GC (the fork's finalizer) when the fork is
	// deleted — the label is removed by the fork, and the fork's GC (the
	// fork's loopCheckpointFinalizer) lists by the FORK's loop label, so
	// the source's snapshot (the source's loop label) is NOT in the fork's
	// GC list. The fork-retained snapshot is therefore orphaned: it is not
	// deleted by the source's GC (skipped) and not by the fork's GC
	// (different loop label). This is a known B2 limitation: a
	// fork-retained snapshot outlives both Loops. The limitation is
	// acceptable (the snapshot is a small object; the data is in the fork's
	// PVC, which the fork's GC deletes; the orphaned snapshot is a
	// harmless leftover). A future slice may add a GC for orphaned
	// fork-retained snapshots.
	if err := r.List(ctx, list,
		client.InNamespace(loop.Namespace),
		client.MatchingLabels{"coxswain.wattu.com/loop": loop.Name}); err != nil {
		return false, fmt.Errorf("re-list checkpoint VolumeSnapshots for GC: %w", err)
	}
	// A snapshot with the fork-of label is NOT counted as "remaining" for
	// the finalizer's purposes (it is skipped above; the source's work is
	// done with its own snapshots). A snapshot WITHOUT the label is
	// "remaining" (the source's GC must delete it before the finalizer is
	// removed).
	remaining := 0
	for i := range list.Items {
		if _, ok := list.Items[i].GetLabels()["coxswain.wattu.com/fork-of"]; !ok {
			remaining++
		}
	}
	return remaining == 0, nil
}

// applyLoopFinalizer adds the checkpoint finalizer to the Loop (B1). It
// runs on every reconcile (idempotent: Set adds it once). A Loop that is
// being deleted (metadata.deletionTimestamp set) is in CleaningUp: the
// finalizer's work (gcLoopCheckpoints) runs before the finalizer is removed.
func (r *LoopReconciler) applyLoopFinalizer(ctx context.Context, loop *coxv1alpha1.Loop) error {
	if !controllerutil.ContainsFinalizer(loop, loopCheckpointFinalizer) {
		controllerutil.AddFinalizer(loop, loopCheckpointFinalizer)
		return r.Update(ctx, loop)
	}
	return nil
}

// finalizeB2DeleteStep is the B2-deleting early return (a Loop being deleted
// — deletionTimestamp set): it validates the referenced AgentPolicies +
// endpointSecretRef/modelEndpoint pair (C6a + D34), resolves the effective
// policy (D30 gate), runs the B2 workspace restore step (b2ApplyRestoreStep —
// the restore decision is the durable record a deleting Loop leaves behind),
// persists the decision (finalizeB2Delete, the I90 point-of-decision write),
// and runs the CleaningUp work (the B1 finalizer's snapshot GC + the
// finalizer removal). It is the same B1 behaviour a deleting Loop had before
// B2, with the B2 decision now running BEFORE the snapshot GC — and it is
// what keeps Reconcile within the gocyclo budget (the deleting branch is the
// whole early-return, not a nested sequence of steps).
func (r *LoopReconciler) finalizeB2DeleteStep(ctx context.Context, loop *coxv1alpha1.Loop) (ctrl.Result, error) {
	if res, done := r.validateStep(ctx, loop); done {
		return res, nil
	}
	condsBefore := make([]metav1.Condition, len(loop.Status.Conditions))
	copy(condsBefore, loop.Status.Conditions)
	if err := r.applyEffectivePolicyAndConditions(ctx, loop); err != nil {
		return ctrl.Result{}, err
	}
	b2Changed, b2Requeue, err := r.b2ApplyRestoreStep(ctx, loop)
	if err != nil {
		return ctrl.Result{}, err
	}
	return r.finalizeB2Delete(ctx, loop, condsBefore, b2Changed, b2Requeue)
}

// finalizeB2Delete is the B2-restore-decided early return for a Loop being
// deleted (deletionTimestamp set). The B2 restore step has just decided
// (b2ApplyRestoreStep, the I90 point-of-decision write) and the decision is
// persisted HERE: the finalizeLoopStatus write (changed = the B2 step's
// condition change, conditionsChanged = the D38 capture). A requeue (the
// snapshot is not ReadyToUse yet — the in-progress state, I49 — or a
// transient read failed) is a clean RequeueAfter so the decision is retried
// while the Loop is deleting (the PVC is recreated on the next pass; the
// finalizer's GC is a no-op on the PVC — the PVC is a child object the
// apiserver garbage-collects, not a snapshot, so the GC does not block on
// it). When the decision is complete (or there was no target — a deleting
// Loop with no restoreFrom/forkOf and the PVC present is a no-op), the
// CleaningUp work (the B1 finalizer's snapshot GC + the finalizer removal)
// runs and its result is returned (the RequeueAfter for an async delete, the
// error for a delete failure, or the done result). It is the same B1
// behaviour a deleting Loop had before B2, with the B2 decision now running
// BEFORE it.
func (r *LoopReconciler) finalizeB2Delete(ctx context.Context, loop *coxv1alpha1.Loop, condsBefore []metav1.Condition, b2Changed bool, b2Requeue bool) (ctrl.Result, error) {
	conditionsChanged := !equality.Semantic.DeepEqual(condsBefore, loop.Status.Conditions)
	if err := r.finalizeLoopStatus(ctx, loop, b2Changed, conditionsChanged, false, false, ""); err != nil {
		return ctrl.Result{}, err
	}
	if b2Requeue {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return r.reconcileCleaningUp(ctx, loop)
}

// reconcileCleaningUp is the B1 finalizer's work for a Loop being deleted
// (deletionTimestamp set, the CleaningUp phase): it GCs the Loop's
// checkpoint VolumeSnapshots (gcLoopCheckpoints) and, when none remain,
// removes the checkpoint finalizer so the apiserver can delete the object.
//
// It is the ONLY thing that runs for a deleting Loop (Reconcile returns early
// to it): the sandbox is owned by the Loop and is already being torn down by
// the time the Loop delete is processed (the sandbox is a namespaced child —
// the operator deletes it explicitly when the Loop leaves a terminal phase
// or is cleaned up), and the snapshots are the last thing the Loop owns that
// the operator must explicitly delete (they are NOT owned by the Loop — the
// snapshot controller creates the VolumeSnapshotContent, and a finalizer
// (not an owner reference) is the operator's hook: the Loop's PVC is
// garbage-collected with the Loop by its owner reference, but the snapshots
// are namespace-scoped objects the operator created without an owner
// reference so they survive the Loop delete until the finalizer removes
// them — a checkpoint outlives the Loop that is being deleted so the owner
// can restore from it (B2's restore path reads the snapshot BEFORE the Loop
// is gone; once the Loop is deleted the finalizer removes the snapshots).
//
// The GC returns (done, err): done means no snapshot remains (the caller
// removes the finalizer); err means a delete failed (the finalizer stays, the
// reconcile requeues). A requeue is a clean RequeueAfter (the delete may be
// async on a real cluster; the finalizer's work is idempotent — a re-read
// next reconcile finds what is left).
func (r *LoopReconciler) reconcileCleaningUp(ctx context.Context, loop *coxv1alpha1.Loop) (ctrl.Result, error) {
	done, err := r.gcLoopCheckpoints(ctx, loop)
	if err != nil {
		// A delete failed: requeue (the finalizer stays, the GC retries).
		logf.FromContext(ctx).Error(err, "CleaningUp: the checkpoint GC failed; requeueing (the finalizer keeps the Loop until the GC is done)",
			"loop", loop.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if !done {
		// The delete is async (the object is not gone yet): requeue.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	// The GC is done (no snapshot remains): remove the finalizer so the
	// apiserver can delete the Loop. A Conflict (another writer touched the
	// Loop's finalizers) is a reconcile error the controller retries — the
	// next reconcile re-runs the GC (idempotent) and retries the removal.
	if err := r.removeLoopCheckpointFinalizer(ctx, loop); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// removeLoopCheckpointFinalizer removes the checkpoint finalizer (B1) once
// the Loop's snapshots are gone (the CleaningUp finalizer's work is done).
func (r *LoopReconciler) removeLoopCheckpointFinalizer(ctx context.Context, loop *coxv1alpha1.Loop) error {
	if controllerutil.RemoveFinalizer(loop, loopCheckpointFinalizer) {
		return r.Update(ctx, loop)
	}
	return nil
}

// nowTime returns the operator's clock (the injected r.now when set, else
// metav1.Now) for the checkpoint record's at field.
func nowTime(r *LoopReconciler) metav1.Time {
	if r.now != nil {
		return r.now()
	}
	return metav1.Now()
}

// Unused imports guard: client.ObjectKey is the key type for the
// cluster-scoped reads (the StorageClass Get uses a bare name key).
var _ = client.ObjectKey{}

// ensureCheckpointStep is the B1 checkpoint step extracted from Reconcile to
// keep the gocyclo budget. It runs after applyVerifyOutcome: it takes the
// workspace checkpoint (checkpoint-<loop>-iter-<N>) and applies retention
// (GC the non-kept snapshots). It is idempotent (one checkpoint per
// iteration, guarded by status.checkpoint.iteration == iteration). The
// iteration parameter is the PRE-verify iteration (the iteration the verify
// ran at, captured before applyVerifyOutcome increments it on the iterate
// path). Returns (changed, requeue): changed is true when a snapshot was
// created or GC'd; requeue is true when the GC is pending.
func (r *LoopReconciler) ensureCheckpointStep(ctx context.Context, loop *coxv1alpha1.Loop) (changed bool, requeue bool) {
	cpChanged, cpRequeue := r.ensureCheckpoint(ctx, loop)
	changed = cpChanged
	requeue = cpRequeue
	return
}

// applyCheckpointGate is the B1 Required gate extracted from Reconcile to
// keep the gocyclo budget. It runs applyCheckpointConditions and, when the
// gate fires (spec.checkpoints=Required and the snapshot support is absent),
// sets the terminal Failed:CheckpointUnavailable phase and writes the
// status. Returns (changed, done, err): done is true when the gate fired
// (the caller should return the result and stop this reconcile); changed is
// true when the condition was (re)set this pass.
func (r *LoopReconciler) applyCheckpointGate(ctx context.Context, loop *coxv1alpha1.Loop) (bool, bool, error) {
	condChanged, cpTerminal := r.applyCheckpointConditions(ctx, loop)
	if !cpTerminal {
		return condChanged, false, nil
	}
	setCondition(loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue,
		checkpointUnavailableReason,
		"spec.checkpoints=Required but checkpoints are not available (the CheckpointsAvailable condition names the reason); the Loop fails at start (D59: checkpoints are mandatory only when the Loop asks for them)")
	loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
	loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
	if err := r.Status().Update(ctx, loop); err != nil {
		return condChanged, false, err
	}
	return condChanged, true, nil
}

// verifyMilestone reports whether the verify passed (all lastCheckResults
// exit codes are 0). A -1 entry means the check did not run (sentinel value;
// the API server rejects nil), which is NOT a pass. An empty lastCheckResults
// (no checks) is a pass (vacuously true).
func verifyMilestone(loop *coxv1alpha1.Loop) bool {
	if loop.Status.Verify == nil {
		return false
	}
	for _, code := range loop.Status.Verify.LastCheckResults {
		if code == nil {
			return false // defensive: should not happen (the operator uses -1)
		}
		if *code != 0 {
			return false // a check failed or did not run (-1)
		}
	}
	return true
}
