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
	"k8s.io/apimachinery/pkg/api/meta"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrl "sigs.k8s.io/controller-runtime"
)

// The snapshot types are handled as unstructured (the operator never
// imports the external-snapshotter client module): the CRDs are not part of
// k8s.io/api and the operator reads them through the manager's cache as
// unstructured.Unstructured with these GVKs.

// GVKs for the VolumeSnapshot family (snapshot.storage.k8s.io/v1).
const (
	snapshotGV = "snapshot.storage.k8s.io/v1"

	volumeSnapshotKind         = "VolumeSnapshot"
	volumeSnapshotListKind     = "VolumeSnapshotList"
	volumeSnapshotClassKind    = "VolumeSnapshotClass"
	volumeSnapshotContentKind  = "VolumeSnapshotContent"
)

// volumeSnapshotGVK is the GVK the operator creates and reads for the
// per-Loop checkpoints (the unstructured watch + the cached List/Get).
var volumeSnapshotGVK = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: volumeSnapshotKind}

// volumeSnapshotListGVK is the GVK the operator's cached client uses for
// List (controller-runtime derives the list type from the Kind + "List").
var volumeSnapshotListGVK = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: volumeSnapshotListKind}

// volumeSnapshotClassGVK is the GVK the operator reads for the
// VolumeSnapshotClass (cluster-scoped) that backs the PVC's CSI driver.
var volumeSnapshotClassGVK = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: volumeSnapshotClassKind}

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
// names the PVC; the snapshot controller provisions the content.
//
// (The deletionPolicy is intentionally left unset: defaulting is the
// external-snapshotter controller's job (the CRD does not default it), and
// an operator that pins it here would fight a controller version that
// defaults it differently. B2's restore reads the snapshot; the content's
// deletionPolicy governs the content's life, not the snapshot object's.)
func newVolumeSnapshotObj(loop *coxv1alpha1.Loop, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(volumeSnapshotGVK)
	u.SetName(name)
	u.SetNamespace(loop.Namespace)
	_ = unstructured.SetNestedField(u.Object, workspacePVCName(loop.Name),
		"spec", "source", "persistentVolumeClaimName")
	// Label so the finalizer's GC (CleaningUp) can find them without a
	// name-prefix List: app.kubernetes.io/managed-by + the Loop name.
	u.SetLabels(map[string]string{
		"app.kubernetes.io/managed-by": "coxswain",
		"coxswain.wattu.com/loop":      loop.Name,
	})
	return u
}

// snapshotCRDPresent reports whether the VolumeSnapshot CRD is resolvable
// (the RESTMapper check, I88). The reconcile-side cache reads (the List in
// ensureCheckpoint and the GC) are gated on this: a List on an absent CRD
// would make the manager's cache for the type error on every watch refresh
// (the operator would log errors forever and the cache would never be
// ready), so the operator only issues snapshot reads when the CRD is
// present. The cache's lazy watch starts on the FIRST read; gating the first
// read on the RESTMapper is what keeps a CRD-less cluster clean.
//
// It is a METHOD (not an inlined check) so a test can override it directly
// (the no-CRD spec sets it to false without wiring a RESTMapper; the
// default reads r.RESTMapper, and a nil mapper — a bare test reconciler —
// is treated as PRESENT because the test envtest has the CRD).
func (r *LoopReconciler) snapshotCRDPresent(ctx context.Context) bool {
	_ = ctx
	if r.RESTMapper == nil {
		// No mapper wired (bare test reconciler): assume present (the test
		// envtest has the CRD).
		return true
	}
	_, err := r.RESTMapper.RESTMapping(volumeSnapshotGVK.GroupKind())
	return err == nil
}

// workspaceDriver reads the workspace PVC's CSI driver (the provisioner): the
// PVC's storageClassName resolves to a StorageClass whose provisioner is the
// CSI driver a VolumeSnapshotClass must match.
//
// Returns ("", false) when the PVC does not exist yet, names no
// storageClassName, or its StorageClass cannot be resolved (the support check
// is deferred to a later reconcile: the PVC is created by ensureLoopArtifacts
// before the first verify, so by the time a checkpoint is due the PVC and its
// StorageClass exist).
func (r *LoopReconciler) workspaceDriver(ctx context.Context, loop *coxv1alpha1.Loop) (string, bool) {
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
	if !r.snapshotCRDPresent(ctx) {
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

// snapshotClassForDriver lists the VolumeSnapshotClasses (cluster-scoped,
// unstructured) and returns the one whose driver matches. (None, false) when
// there is no match or the list fails (a list failure is treated as no
// match: the conservative reading — do not claim availability on a read
// error, and do not fail the Loop on a transient read either; the next
// reconcile re-reads).
func (r *LoopReconciler) snapshotClassForDriver(ctx context.Context, driver string) (*unstructured.Unstructured, bool) {
	// The CRD presence is the caller's responsibility (checkpointSupport
	// gates on it); a list on an absent CRD would error, and the operator
	// would treat the error as "no match" (the conservative reading).
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(volumeSnapshotClassGVK)
	if err := r.List(ctx, list); err != nil {
		return nil, false
	}
	for i := range list.Items {
		item := &list.Items[i]
		if d, ok, _ := unstructured.NestedString(item.Object, "spec", "driver"); ok && d == driver {
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
	if loop.Status.Phase != coxv1alpha1.LoopPhaseSucceeded {
		// B1 takes checkpoints after each successful verify. The verify
		// outcome (applyVerifyOutcome) flips the phase to Succeeded in the
		// same reconcile; the checkpoint step runs AFTER that flip (the
		// Reconcile calls it after applyVerifyOutcome), so by the time
		// ensureCheckpoint runs the phase is Succeeded and the just-completed
		// iteration is status.iteration. (A Loop that is already Succeeded
		// from a previous reconcile does NOT re-take: the idempotency guard
		// below — a recorded checkpoint for status.iteration means the
		// snapshot is already recorded.)
		// A Loop at Succeeded from a PREVIOUS verify (a second successful
		// verify is impossible in B1 — the phase machine is one-shot,
		// ADR-0003: a successful verify is terminal) has the same iteration,
		// so the guard is the idempotency.
	}
	// Idempotency: a checkpoint is taken ONCE per iteration (the
	// checkpoint-<loop>-iter-<N> name is unique per iteration — iteration
	// numbers are never reused). If status.checkpoint already records
	// status.iteration, the snapshot is already taken; do not re-create it.
	if cp := loop.Status.Checkpoint; cp != nil && cp.Iteration == loop.Status.Iteration {
		return false, false
	}
	// No verify has succeeded yet (iteration 0 means no verify ran; the
	// first successful verify is iteration 1). Nothing to checkpoint.
	if loop.Status.Iteration < 1 {
		return false, false
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
	// Create the snapshot (idempotent: the name is unique per iteration; a
	// AlreadyExists on the same-name object is the same checkpoint being
	// re-attempted, treated as recorded).
	name := checkpointName(loop, loop.Status.Iteration)
	snap := newVolumeSnapshotObj(loop, name)
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
		Iteration: loop.Status.Iteration,
		Name:      name,
		Milestone: true, // B1: every checkpoint is a successful-verify checkpoint (a milestone).
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
		// GC this checkpoint's VolumeSnapshot.
		snap := newVolumeSnapshotObj(loop, cp.Name)
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
func (r *LoopReconciler) gcLoopCheckpoints(ctx context.Context, loop *coxv1alpha1.Loop) (done bool, err error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(volumeSnapshotListGVK)
	if !r.snapshotCRDPresent(ctx) {
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
		if delErr := r.Delete(ctx, item); delErr != nil && !isNotFound(delErr) {
			return false, delErr
		}
	}
	// Done: re-list to confirm none remain (a delete may be async; the
	// re-list is the confirmation). When none remain, the caller removes
	// the finalizer.
	if err := r.List(ctx, list,
		client.InNamespace(loop.Namespace),
		client.MatchingLabels{"coxswain.wattu.com/loop": loop.Name}); err != nil {
		return false, fmt.Errorf("re-list checkpoint VolumeSnapshots for GC: %w", err)
	}
	// Nowhere in B1 is a non-milestone checkpoint produced by the operator
	// (every checkpoint is a successful-verify checkpoint); the GC path is
	// exercised by the retention spec via a hand-built non-milestone entry
	// (the mutation test and the GC spec both write the status directly).
	return len(list.Items) == 0, nil
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
