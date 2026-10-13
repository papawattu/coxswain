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

// B2 (PLAN-BETA, PLAN.md Phase 3 part 2): restore, rollback and fork.
//
// Three behaviours share one mechanism: the operator recreates the Loop's
// workspace PVC with spec.dataSource = a VolumeSnapshot (a PVC dataSource
// is immutable — the PVC is deleted and recreated), then the A2 PVC-resume
// path picks the Loop up on the restored content.
//
//   - RESTORE (reactive): the workspace PVC is LOST (NotFound — e.g. a
//     local-path PV loss, or an operator bug that deleted it) and the Loop
//     has a usable checkpoint. The operator restores from the latest
//     ReadyToUse checkpoint and surfaces it (WorkspaceRestored Event + the
//     Restore condition). No usable checkpoint: it falls back to the A2
//     PVC-resume (recreate the PVC EMPTY) and surfaces THAT (I90: the
//     fallback is visible — a WorkspaceRestoreUnavailable Event + the
//     Restore condition, not a silent ignore).
//   - ROLLBACK (explicit): spec.restoreFrom: iter-N names the checkpoint
//     the operator restores from. The checkpoint must EXIST (a recorded
//     status.checkpoints entry whose VolumeSnapshot is present) and be
//     ReadyToUse. An unknown iter is rejected VISIBLY (Restore condition
//     False reason RestoreSourceMissing + an Event, terminal — the object
//     is gone); a not-yet-ready snapshot is DEFERRED (Restore condition
//     False reason RestoreSourceNotReady + an Event, retried each reconcile
//     — the in-progress state, no restore decision, I49).
//   - FORK (explicit, at creation): spec.forkOf {loop, iteration} seeds the
//     Loop from another Loop's checkpoint. At start the operator restores
//     this Loop's OWN workspace PVC from the source snapshot
//     checkpoint-<loop>-iter-<N>; the fork then runs a normal one-shot
//     Loop (own branch, own deliver, own PR — ADR-0003: forks, not
//     re-tasks). Deleting the source Loop does NOT break a fork that has
//     already restored: the fork's PVC owns its content (the content was
//     materialised from the snapshot at restore), and the source's
//     checkpoint GC (the source's finalizer, CleaningUp) deletes the
//     SOURCE's snapshots, which the fork no longer reads. The fork's
//     restore is a ONE-SHOT decision recorded in status (a
//     status.condition Restore=True/Restored is the marker): once the
//     source snapshot is gone the fork carries on (it does not re-restore,
//     does not treat the gone snapshot as a lost-PVC fallback case — the
//     PVC is present).
//
// The Restore condition is set on every restore decision (the I83 pattern:
// the condition is the durable record; the Event is the transient signal).
// The condition is NOT re-set on every reconcile once the restore is
// complete (Restore=True/Restored stays until the Loop ends) — a re-set on
// every reconcile would churn the status and race the A2 path.

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// B2 checkpoint-source resolution.
//
// resolveCheckpointSource reads the VolumeSnapshot named after the Loop's
// checkpoint for (loopName, iteration) and returns (snapshot, decision):
//   - decision "ready": the snapshot exists AND is ReadyToUse
//     (status.boundSnapshotVolumeReadyToUse == true) — the operator may
//     recreate the PVC from it.
//   - decision "notready": the snapshot exists but is not ReadyToUse yet —
//     DEFER (requeue; the in-progress state, no decision, I49).
//   - decision "missing": the snapshot does not exist — the checkpoint is
//     gone (GC'd or never taken) — the operator must not restore from it
//     (the Restore condition names the reason; the Event is emitted).
//
// A read error (other than NotFound) is reported as "error" (the caller
// requeues; the operator does not make a restore decision on a transient
// read failure — the conservative reading, as in B1's support check).
const (
	b2SourceReady    = "ready"
	b2SourceNotReady = "notready"
	b2SourceMissing  = "missing"
	b2SourceError    = "error"
)

// resolveCheckpointSource reads the checkpoint's VolumeSnapshot and
// classifies it (ready / notready / missing / error). It is a method on the
// reconciler so the specs can drive it (the snapshot CRD seam: a cluster
// without the CRD gets "missing" — the operator must not read a type the
// RESTMapper does not resolve; B1's I88 gate). When the CRD is absent
// (snapshotCRDPresent=false), every source is reported as "missing" (the
// operator cannot read a type the RESTMapper does not resolve; reading it
// would wedge the manager's cache — the I88 gate, B1).
func (r *LoopReconciler) resolveCheckpointSource(ctx context.Context, ns, loopName string, iteration int) (*unstructured.Unstructured, string) {
	if !r.snapshotCRDPresentCall(ctx) {
		return nil, b2SourceMissing
	}
	name := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: loopName}}, iteration)
	snap := &unstructured.Unstructured{Object: map[string]any{unstructuredAPI: snapshotGV, unstructuredKind: volumeSnapshotKind}}
	snap.SetGroupVersionKind(volumeSnapshotGVK)
	key := types.NamespacedName{Namespace: ns, Name: name}
	if err := r.Get(ctx, key, snap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, b2SourceMissing
		}
		return nil, b2SourceError
	}
	ready, found, _ := unstructured.NestedBool(snap.Object, "status", "readyToUse")
	if !found || !ready {
		return snap, b2SourceNotReady
	}
	return snap, b2SourceReady
}

// restorePVCFromSnapshot deletes the workspace PVC (if present) and
// recreates it with spec.dataSource = the named VolumeSnapshot (a PVC
// dataSource is IMMUTABLE — the only way to change it is delete + create).
// It returns an error only on a real create/read failure (the delete is
// idempotent: a NotFound is a success). The restored PVC is controller-owned
// by the Loop (the same GC contract as the original PVC).
func (r *LoopReconciler) restorePVCFromSnapshot(ctx context.Context, loop *coxv1alpha1.Loop, snapshotName string) error {
	snapshotGroupRef := snapshotGroup
	key := types.NamespacedName{Namespace: loop.Namespace, Name: workspacePVCName(loop.Name)}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, key, pvc); err == nil {
		// The PVC is present: delete it (the dataSource is immutable on an
		// existing PVC; the new PVC must carry the new dataSource). A
		// NotFound is a success (the PVC was already gone — the lost-PVC
		// restore path arrives here with the PVC already deleted).
		if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete workspace PVC %s for restore: %w", workspacePVCName(loop.Name), err)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read workspace PVC %s for restore: %w", workspacePVCName(loop.Name), err)
	}
	// Recreate the PVC with the dataSource (the A2 path's ensureWorkspacePVC
	// will NOT rewrite it — an existing PVC is left untouched except the
	// owner ref, so the dataSource survives).
	newPVC := workspacePVCSpec(loop)
	ds := corev1.TypedLocalObjectReference{
		APIGroup: &snapshotGroupRef,
		Kind:     volumeSnapshotKind,
		Name:     snapshotName,
	}
	newPVC.Spec.DataSource = &ds
	if err := controllerutil.SetControllerReference(loop, newPVC, r.Scheme); err != nil {
		return fmt.Errorf("set owner on restored workspace PVC %s: %w", workspacePVCName(loop.Name), err)
	}
	if err := r.Create(ctx, newPVC); err != nil {
		return fmt.Errorf("create restored workspace PVC %s from snapshot %s: %w", workspacePVCName(loop.Name), snapshotName, err)
	}
	return nil
}

// b2RestoreTarget names the checkpoint a Loop should restore from, and
// WHY (the Restore condition's message carries it). For a rollback
// (spec.restoreFrom) the target is the named iter; for a fork (spec.forkOf)
// it is the source checkpoint; for a lost-PVC restore it is the latest
// recorded checkpoint.
type b2RestoreTarget struct {
	loopName  string // the Loop whose checkpoints are the source
	iteration int    // the checkpoint's iteration
	snapshot  string // the checkpoint's VolumeSnapshot name (checkpointName)
	kind      b2Kind // the restore's kind (the condition's message)
}

// b2Kind names the restore's kind (the Restore condition's message).
type b2Kind string

const (
	b2KindLostPVC  b2Kind = "lostPVC"    // the workspace PVC was lost (reactive restore)
	b2KindRollback b2Kind = "rollback"   // spec.restoreFrom (explicit)
	b2KindFork     b2Kind = "fork"       // spec.forkOf (explicit, at creation)
	b2KindFallback b2Kind = "noSnapshot" // the PVC was lost with no usable checkpoint (A2 fallback)
)

// b2RestoreTargetFor computes the Loop's restore target (nil when no
// restore applies this reconcile):
//   - a fork (spec.forkOf) whose restore is not yet complete (the
//     Restore condition is not True/Restored): the source checkpoint.
//   - a rollback (spec.restoreFrom): the named iter (always — a rollback
//     is a one-time restore decision, recorded once the PVC is restored).
//   - a LOST workspace PVC (NotFound) with no restoreFrom/forkOf: the
//     latest recorded checkpoint (status.checkpoints' last entry), when
//     one exists.
//
// The rollback/fork restore is ONE-SHOT per Loop life: once the Restore
// condition is True/Restored (the PVC was recreated from the snapshot),
// the operator does not re-restore (re-restore would be a wedge: the
// snapshot may have been GC'd in the meantime, and the PVC now holds the
// restored content the Loop is working on). A LOST PVC is a different
// decision (reactive): it re-evaluates every reconcile until the PVC is
// back (a restore or the empty-PVC fallback).
func (r *LoopReconciler) b2RestoreTarget(ctx context.Context, loop *coxv1alpha1.Loop) *b2RestoreTarget {
	// A completed restore is one-shot: the condition is the marker.
	if c := meta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.RestoreCondition); c != nil &&
		c.Status == metav1.ConditionTrue && c.Reason == coxv1alpha1.RestoreReasonRestored {
		// The restore is complete. Do NOT re-restore if the PVC is present
		// (re-restore would wipe the Loop's work since the restore).
		// A LOST PVC on a RESTORED Loop is the reactive case again (the PVC
		// was restored once and then lost): the target is the rollback
		// target if set (the explicit one wins), otherwise the latest
		// recorded checkpoint.
		pvc := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: loop.Namespace, Name: workspacePVCName(loop.Name)}, pvc); err == nil {
			return nil // the PVC is present: the restore is complete, do not re-restore
		}
		if loop.Spec.RestoreFrom != "" {
			if it, err := parseRestoreFrom(loop.Spec.RestoreFrom); err == nil {
				return &b2RestoreTarget{loopName: loop.Name, iteration: it, snapshot: checkpointName(loop, it), kind: b2KindRollback}
			}
		}
		return r.b2LostPVCTarget(ctx, loop)
	}
	// A fork: seed from the source checkpoint (one-shot at creation).
	if loop.Spec.ForkOf != nil {
		return &b2RestoreTarget{loopName: loop.Spec.ForkOf.Loop, iteration: loop.Spec.ForkOf.Iteration,
			snapshot: checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: loop.Spec.ForkOf.Loop}}, loop.Spec.ForkOf.Iteration), kind: b2KindFork}
	}
	// A rollback: the named iter (one-shot per Loop life).
	if loop.Spec.RestoreFrom != "" {
		if it, err := parseRestoreFrom(loop.Spec.RestoreFrom); err == nil {
			return &b2RestoreTarget{loopName: loop.Name, iteration: it, snapshot: checkpointName(loop, it), kind: b2KindRollback}
		}
		// A malformed restoreFrom is rejected visibly (the condition + the
		// Event, below in b2ApplyRestoreStep); no target. The CRD pattern
		// validates the shape at admission, so this is a defensive branch
		// (a Loop created with a bypassing client or a future CRD relaxation).
		return nil
	}
	// A lost PVC (no explicit restore): the reactive case.
	return r.b2LostPVCTarget(ctx, loop)
}

// b2LostPVCTarget returns the reactive restore target when the workspace
// PVC is LOST (NotFound) and no explicit restore is set: the latest
// recorded checkpoint (status.checkpoints' last entry), or nil (no usable
// checkpoint — the caller applies the A2 fallback). It returns nil when the
// PVC is present (the reactive case does not apply — the PVC is there).
func (r *LoopReconciler) b2LostPVCTarget(ctx context.Context, loop *coxv1alpha1.Loop) *b2RestoreTarget {
	pvc := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Namespace: loop.Namespace, Name: workspacePVCName(loop.Name)}
	err := r.Get(ctx, key, pvc)
	if err == nil {
		return nil // the PVC is present: no reactive restore
	}
	if !apierrors.IsNotFound(err) {
		return nil // a transient read failure: no decision (retried next reconcile)
	}
	// The PVC is LOST. The latest recorded checkpoint is the restore source
	// (nil when none — the A2 fallback recreates the PVC empty).
	cps := loop.Status.Checkpoints
	if len(cps) == 0 {
		return nil
	}
	last := cps[len(cps)-1]
	return &b2RestoreTarget{loopName: loop.Name, iteration: last.Iteration, snapshot: last.Name, kind: b2KindLostPVC}
}

// parseRestoreFrom parses "iter-N" (the CRD pattern validates the shape;
// this is the int extraction, error = malformed). Iteration 0 is a valid
// checkpoint (the first verify; the same lesson as B1's iter-0).
func parseRestoreFrom(s string) (int, error) {
	rest, ok := strings.CutPrefix(s, "iter-")
	if !ok || rest == "" {
		return 0, fmt.Errorf("malformed restoreFrom %q", s)
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("malformed restoreFrom %q", s)
	}
	return n, nil
}

// b2ApplyRestoreStep is the B2 restore step extracted from Reconcile (the
// gocyclo budget). It runs BEFORE phaseBootstrap (the bootstrap's
// ensureLoopArtifacts would recreate a LOST PVC EMPTY before the restore
// could decide; the sandbox build reads the PVC by name, not spec, so a
// restored PVC is invisible to it). It:
//
//  1. computes the restore target (b2RestoreTarget),
//  2. resolves the source snapshot (ready / notready / missing / error),
//  3. on ready: restores the PVC from the snapshot, sets Restore=True
//     (reason Restored) and emits the WorkspaceRestored Event,
//  4. on notready: DEFERS (Restore=False reason RestoreSourceNotReady +
//     the Event; the requeue is the 5s RequeueAfter in the final return),
//  5. on missing: the kind decides — a rollback/fork is TERMINAL
//     (Restore=False reason RestoreSourceMissing + the Event; no requeue —
//     the object is gone); a lost-PVC restore falls back to the A2 path
//     (recreate the PVC empty + Restore=False reason NoUsableCheckpoint +
//     the WorkspaceRestoreUnavailable Event — I90: the fallback is
//     VISIBLE),
//  6. on error: requeues (no decision on a transient read failure).
//
// It returns (changed, requeue, err). changed is true when the condition was
// (re)set this pass; requeue is true when a requeue is needed (notready or
// error); err is a real failure (the caller returns it).
//
// The step is ONE-SHOT per Loop life for an explicit restore (rollback /
// fork): once the Restore condition is True/Restored the operator does not
// re-restore (re-restore would be a wedge: the snapshot may have been GC'd
// in the meantime, and the PVC now holds the restored content the Loop is
// working on). A LOST PVC on a RESTORED Loop is the reactive case again
// (the PVC was restored once and then lost): the target is the latest
// recorded checkpoint (or the rollback target if set — the explicit one
// wins). The one-shot marker is the condition itself (b2RestoreTarget
// reads it), so the step never re-decides a completed restore.
func (r *LoopReconciler) b2ApplyRestoreStep(ctx context.Context, loop *coxv1alpha1.Loop) (bool, bool, error) {
	target := r.b2RestoreTarget(ctx, loop)
	if target == nil {
		return false, false, nil // no restore applies this reconcile
	}
	snap, decision := r.resolveCheckpointSource(ctx, loop.Namespace, target.loopName, target.iteration)
	// persist is the I90 write (the reviewer's guidance, R-B2): the condition
	// is already set on `loop` by this point, so persisting it HERE (at the
	// point of decision) makes the decision durable even when a later step in
	// the same reconcile fails (e.g. ensureSandbox). The Event is emitted
	// immediately (the transient signal fires at the point of decision, before
	// any later failure can hide it); the Status().Update is what survives a
	// later failure in the same reconcile. The step returns (persisted,
	// requeue, err): persisted is true when the step wrote the status this
	// pass (the caller ORs it into `changed` so a failed later step does not
	// make finalizeLoopStatus skip the write — the step already wrote it);
	// requeue is true when a requeue is needed (notready or error); err is a
	// real failure (the caller returns it).
	persist := func() bool { return r.Status().Update(ctx, loop) == nil }
	switch decision {
	case b2SourceReady:
		if err := r.restorePVCFromSnapshot(ctx, loop, target.snapshot); err != nil {
			// A restore failure is surfaced (I90) and requeued (the next
			// reconcile retries — the PVC delete may have succeeded and the
			// create failed; the retry recreates it).
			setRestoreCondition(loop, false, coxv1alpha1.RestoreReasonRejected,
				"restoring the workspace PVC from "+target.snapshot+" ("+string(target.kind)+" iter-"+strconv.Itoa(target.iteration)+") failed: "+err.Error())
			persisted := persist()
			r.emitRestoreEvent(loop, false, coxv1alpha1.RestoreReasonRejected,
				"workspace PVC restore from "+target.snapshot+" failed; retrying")
			return persisted, true, nil
		}
		// B2 FORK-RETENTION: a fork that has just restored its PVC from the
		// source's checkpoint REMOVES the fork-of label from the source's
		// snapshot (the fork's PVC now owns its content — the snapshot was
		// materialised into the PVC at restore, and the fork no longer
		// reads it). The label removal is the operator's own action (the
		// fork's operator, not the source's). Once the label is removed,
		// the source's GC (the source's loopCheckpointFinalizer) is free
		// to delete the snapshot on the source's next reconcile (the fork
		// is self-sufficient). A rollback or a lost-PVC restore does NOT
		// touch the label (it is the fork's responsibility; a rollback
		// restores from the Loop's OWN snapshot, not another Loop's).
		if target.kind == b2KindFork {
			r.removeForkRetentionLabel(ctx, loop, target.loopName, target.snapshot)
		}
		setRestoreCondition(loop, true, coxv1alpha1.RestoreReasonRestored,
			"workspace PVC restored from "+target.snapshot+" ("+string(target.kind)+", iter-"+strconv.Itoa(target.iteration)+")")
		persisted := persist()
		r.emitRestoreEvent(loop, true, coxv1alpha1.RestoreReasonRestored,
			"workspace PVC restored from "+target.snapshot)
		return persisted, false, nil
	case b2SourceNotReady:
		// DEFER: the snapshot exists but is not ReadyToUse (the in-progress
		// state — no restore decision, I49). The condition names it; the
		// requeue retries (the 5s RequeueAfter in the final return). The
		// snap value is not needed (the condition names the snapshot).
		_ = snap
		setRestoreCondition(loop, false, coxv1alpha1.RestoreReasonSourceNotReady,
			"checkpoint "+target.snapshot+" ("+string(target.kind)+") exists but is not ReadyToUse yet; the restore is deferred")
		persisted := persist()
		r.emitRestoreEvent(loop, false, coxv1alpha1.RestoreReasonSourceNotReady,
			"checkpoint "+target.snapshot+" not ReadyToUse yet; restore deferred")
		return persisted, true, nil
	case b2SourceMissing:
		switch target.kind {
		case b2KindRollback, b2KindFork:
			// TERMINAL: the explicit target is gone. The condition names it;
			// NO requeue (the object will not come back — the source Loop's
			// GC deleted it, or it was never taken).
			setRestoreCondition(loop, false, coxv1alpha1.RestoreReasonSourceMissing,
				"checkpoint "+target.snapshot+" ("+string(target.kind)+") does not exist (it was never taken, or it was GC'd); the restore is rejected")
			persisted := persist()
			r.emitRestoreEvent(loop, false, coxv1alpha1.RestoreReasonSourceMissing,
				"checkpoint "+target.snapshot+" does not exist; restore rejected")
			return persisted, false, nil
		default: // b2KindLostPVC
			// The A2 fallback: the PVC is lost and the (latest) checkpoint
			// is gone. Recreate the PVC EMPTY (the A2 PVC-resume — the
			// content is lost, the Loop continues from the untouched base)
			// and surface the fallback (I90: VISIBLE).
			if err := r.recreateWorkspacePVCEmpty(ctx, loop); err != nil {
				return false, true, fmt.Errorf("A2 fallback: recreate workspace PVC empty: %w", err)
			}
			setRestoreCondition(loop, false, noUsableCheckpointReason,
				"the workspace PVC was lost and checkpoint "+target.snapshot+" does not exist; fell back to the A2 PVC-resume (an EMPTY workspace — the checkpoint's content is lost, I90)")
			persisted := persist()
			r.emitRestoreEvent(loop, false, noUsableCheckpointReason,
				"workspace PVC lost, no usable checkpoint; A2 fallback (empty workspace)")
			return persisted, false, nil
		}
	case b2SourceError:
		// A transient read failure: no decision, requeue.
		log.FromContext(ctx).V(1).Info("restore: reading the checkpoint snapshot failed; requeueing (no decision on a transient read failure)",
			"snapshot", target.snapshot, "kind", string(target.kind))
		return false, true, nil
	}
	return false, false, nil
}

// noUsableCheckpointReason is the Restore condition's reason for the
// A2 fallback (the PVC was lost with no usable checkpoint; I90 names it).
// The constant lives in the API package (the CRD's status enum references
// it) and is aliased here so the controller reads as a local.
var noUsableCheckpointReason = coxv1alpha1.RestoreReasonNoUsableCheckpoint

// setRestoreCondition sets the Restore condition (the B2 durable record).
func setRestoreCondition(loop *coxv1alpha1.Loop, status bool, reason, message string) {
	st := metav1.ConditionFalse
	if status {
		st = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&loop.Status.Conditions, metav1.Condition{
		Type:    coxv1alpha1.RestoreCondition,
		Status:  st,
		Reason:  reason,
		Message: message,
	})
}

// emitRestoreEvent emits the restore Event (the transient signal; the
// condition is the durable record). Best-effort: a nil Recorder (most
// envtests) skips it.
func (r *LoopReconciler) emitRestoreEvent(loop *coxv1alpha1.Loop, status bool, reason, message string) {
	if r.Recorder == nil {
		return
	}
	typ := corev1.EventTypeNormal
	if !status {
		typ = corev1.EventTypeWarning
	}
	r.Recorder.Eventf(loop, typ, "Restore", "Restore=%t reason=%s: %s", status, reason, message)
}

// recreateWorkspacePVCEmpty recreates a LOST workspace PVC with an empty
// spec (the A2 PVC-resume path — ensureWorkspacePVC creates it on NotFound;
// this helper makes the fallback EXPLICIT so the spec can assert the
// operator created it from the fallback path, not the bootstrap). It
// re-uses ensureWorkspacePVC (the A2 path) — the idempotency is the same.
func (r *LoopReconciler) recreateWorkspacePVCEmpty(ctx context.Context, loop *coxv1alpha1.Loop) error {
	return r.ensureWorkspacePVC(ctx, loop)
}

// removeForkRetentionLabel removes the fork-of label from the source's
// checkpoint snapshot (B2 fork-retention: the fork has completed its
// restore, and the source's snapshot is no longer needed). It is a
// best-effort action (the label removal is the fork's responsibility; a
// failure to remove the label does NOT break the fork — the fork's PVC is
// self-sufficient — but the source's GC will not delete the snapshot until
// the label is removed, so the label's persistence is a known B2
// limitation, documented in gcLoopCheckpoints). It is a no-op when the
// snapshot is absent (the source's GC already deleted it, or the label was
// already removed). The label write is the fork's operator action (the
// operator is the sole writer of its own objects' labels — the fork's
// operator, not the source's, writes the label onto the source's snapshot
// at fork creation, and removes it here at fork restore completion).
func (r *LoopReconciler) removeForkRetentionLabel(ctx context.Context, fork *coxv1alpha1.Loop, sourceLoopName, snapshotName string) {
	if !r.snapshotCRDPresentCall(ctx) {
		return // no CRD: the snapshot is not present (the fork's restore is
		// complete, so the snapshot must have been present at restore time;
		// a CRD absence now is a no-op — the label was never set)
	}
	snap := &unstructured.Unstructured{Object: map[string]any{unstructuredAPI: snapshotGV, unstructuredKind: volumeSnapshotKind}}
	snap.SetGroupVersionKind(volumeSnapshotGVK)
	key := types.NamespacedName{Namespace: fork.Namespace, Name: snapshotName}
	if err := r.Get(ctx, key, snap); err != nil {
		return // the snapshot is absent (a no-op: the label was never set, or
		// the source's GC already deleted it)
	}
	labels := snap.GetLabels()
	if labels == nil {
		return // no labels: the label was never set (a no-op)
	}
	if _, ok := labels["coxswain.wattu.com/fork-of"]; !ok {
		return // the label is absent (a no-op: it was never set, or it was
		// already removed)
	}
	delete(labels, "coxswain.wattu.com/fork-of")
	snap.SetLabels(labels)
	if err := r.Update(ctx, snap); err != nil {
		// A label-removal failure is a no-op (the fork's PVC is
		// self-sufficient; the label's persistence is a known B2
		// limitation, documented in gcLoopCheckpoints). Log it (the spec
		// asserts the label's absence after a successful removal; a failure
		// here means the label persists, and the spec will catch it).
		log.FromContext(ctx).V(1).Info("fork: removing the fork-of label from the source's snapshot failed; the label persists (a known B2 limitation; the fork's PVC is self-sufficient)",
			"snapshot", snapshotName, "sourceLoop", sourceLoopName, "err", err.Error())
	}
}
