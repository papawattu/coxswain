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
//     ReadyToUse checkpoint and surfaces it (Restore Event + the Restore
//     condition). No usable checkpoint: it falls back to the A2 PVC-resume
//     (recreate the PVC EMPTY) and surfaces THAT (I90: the fallback is
//     visible — a Restore Event + the Restore condition, not a silent
//     ignore).
//   - ROLLBACK (explicit): spec.restoreFrom: iter-N names the checkpoint
//     the operator restores from. The checkpoint must EXIST (a recorded
//     status.checkpoints entry whose VolumeSnapshot is present) and be
//     ReadyToUse. An unknown iter is rejected VISIBLY (Restore condition
//     False reason RestoreSourceMissing + an Event, terminal — the object
//     is gone); a not-yet-ready snapshot is DEFERRED (Restore condition
//     False reason RestoreSourceNotReady + an Event, retried each
//     reconcile — the in-progress state, no restore decision, I49).
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
//     restore is a ONE-SHOT decision recorded in the Restore condition
//     (True/Restored): once the restore is complete the fork does not
//     re-restore and does not treat a gone source snapshot as a lost-PVC
//     fallback case — the PVC is present.
//
// The PVC replacement is LEVEL-BASED (no blocking poll in Reconcile): the
// decision, the sandbox stop, the PVC delete and the PVC create are each a
// separate reconcile step, and progress is recorded in the Restore
// condition (the decision survives a restart — the reviewer's P1, the R-B2
// review at 20ef538):
//
//  1. a rollback target with a READY source and a PRESENT PVC: the sandbox
//     is STOPPED (Suspended — a mounted PVC is held by
//     kubernetes.io/pvc-protection until the pod is gone; the A2 kill path
//     stops it the same way) and the Restore condition is False reason
//     StoppingSandbox (the in-progress state — no PVC change yet, no
//     restore decision, I49). The PVC is NOT deleted yet (the delete of a
//     still-mounted PVC would not complete on a real cluster).
//  2. a rollback target with a ready source and the sandbox STOPPED: the
//     PVC is DELETED (the dataSource is immutable — delete + recreate is
//     the only way) and the condition is False reason DeletingPVC. The
//     delete is async (the apiserver removes the object after the
//     finalizer work is done); a still-present PVC (a pending delete) does
//     NOT re-trigger the delete (idempotent) and the reconcile returns.
//  3. a rollback target with a ready source and the PVC GONE: the PVC is
//     CREATED with spec.dataSource = the snapshot (AlreadyExists is a
//     success — a concurrent pass created it; the content is from the
//     snapshot either way) and the condition is True reason Restored (the
//     one-shot marker). The restore then completes: no further PVC action
//     until the next explicit restoreFrom (the next rollback re-arms via
//     the spec's generation — see the marker discussion in
//     b2RestoreTarget).
//
// A FORK runs the same steps at creation, but the PVC is ABSENT (a fresh
// Loop) and the sandbox does not exist yet, so it skips the stop + delete
// and goes straight to the create (step 3). A LOST-PVC restore is the
// reactive case: the PVC is already gone, so it also goes straight to the
// create (step 3), with the A2 fallback (empty PVC + a visible condition +
// Event) when the latest checkpoint is missing.
//
// The Restore condition is set on every restore decision (the I83 pattern:
// the condition is the durable record; the Event is the transient signal).
// It is NOT re-set on every reconcile once the restore is complete (True/
// Restored stays until the next explicit restoreFrom or the Loop ends) —
// a re-set on every reconcile would churn the status and race the A2 path.

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// B2 checkpoint-source resolution.
//
// resolveCheckpointSource reads the VolumeSnapshot named after the Loop's
// checkpoint for (loopName, iteration) and returns (snapshot, decision):
//   - decision "ready": the snapshot exists AND is ReadyToUse
//     (status.readyToUse == true) — the operator may recreate the PVC
//     from it.
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

// b2RestoreTarget computes the Loop's restore target (nil when no restore
// applies this reconcile):
//
//   - a FORK (spec.forkOf) whose restore is not yet complete: the source
//     checkpoint (one-shot at creation).
//   - a ROLLBACK (spec.restoreFrom): the named iter, when the restore for
//     that generation has not yet completed. The one-shot marker is the
//     Restore condition itself (True/Restored) PLUS the spec's generation:
//     once a rollback has completed the operator does not re-restore (a
//     re-restore would wipe the Loop's work since the restore) — the
//     condition's observedGeneration carries the spec generation the
//     restore completed for (a condition whose observedGeneration < the
//     Loop's generation is a stale marker — a NEW spec.restoreFrom value
//     bumps the generation and re-arms the restore; a same-value update
//     does not). The CRD makes restoreFrom immutable, so in practice the
//     re-arm happens only for a Loop that changed the field through a
//     bypassing client (defensive) or a future CRD relaxation.
//   - a LOST workspace PVC (NotFound) with no completed restore for the
//     current generation: the reactive case — the latest recorded
//     checkpoint (nil when none — the A2 fallback recreates the PVC
//     empty). The reactive case re-evaluates every reconcile until the
//     PVC is back.
//
// The in-progress markers (StoppingSandbox / DeletingPVC) do NOT stop the
// target from applying: they ARE the restore's progress, and the step
// (b2ApplyRestoreStep) is idempotent on them (a still-present PVC while
// DeletingPVC is not re-deleted; a running sandbox while StoppingSandbox is
// not re-updated).
func (r *LoopReconciler) b2RestoreTarget(ctx context.Context, loop *coxv1alpha1.Loop) *b2RestoreTarget {
	// A completed restore is one-shot: the condition is the marker. The
	// marker is valid for the spec generation it was recorded against
	// (observedGeneration >= loop.Generation): a new spec.restoreFrom value
	// bumps the generation and re-arms (the condition's observedGeneration
	// stays at the old generation until the next restore writes it).
	if c := meta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.RestoreCondition); c != nil &&
		c.Status == metav1.ConditionTrue && c.Reason == coxv1alpha1.RestoreReasonRestored &&
		c.ObservedGeneration >= loop.Generation {
		// The restore is complete for this spec generation. Do NOT
		// re-restore if the PVC is present (re-restore would wipe the Loop's
		// work since the restore). A LOST PVC on a RESTORED Loop is the
		// reactive case again (the PVC was restored once and then lost):
		// the target is the rollback target if set (the explicit one wins),
		// otherwise the latest recorded checkpoint.
		if r.workspacePVCPresent(ctx, loop) {
			return nil // the PVC is present: the restore is complete, do not re-restore
		}
		if loop.Spec.RestoreFrom != "" {
			if it, err := parseRestoreFrom(loop.Spec.RestoreFrom); err == nil {
				return &b2RestoreTarget{loopName: loop.Name, iteration: it, snapshot: checkpointName(loop, it), kind: b2KindRollback}
			}
		}
		return r.b2LostPVCTarget(ctx, loop)
	}
	// A fork: seed from the source checkpoint (one-shot at creation; a fork
	// never re-arms — its spec.forkOf is immutable and its Restore marker is
	// written with the fork's generation).
	if loop.Spec.ForkOf != nil {
		return &b2RestoreTarget{loopName: loop.Spec.ForkOf.Loop, iteration: loop.Spec.ForkOf.Iteration,
			snapshot: checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: loop.Spec.ForkOf.Loop}}, loop.Spec.ForkOf.Iteration), kind: b2KindFork}
	}
	// A rollback: the named iter (one-shot per spec generation).
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
// PVC is LOST (NotFound) and no explicit restore applies: the latest
// recorded checkpoint (status.checkpoints' last entry), or nil (no usable
// checkpoint — the caller applies the A2 fallback). It returns nil when the
// PVC is present (the reactive case does not apply — the PVC is there).
func (r *LoopReconciler) b2LostPVCTarget(ctx context.Context, loop *coxv1alpha1.Loop) *b2RestoreTarget {
	if r.workspacePVCPresent(ctx, loop) {
		return nil // the PVC is present: no reactive restore
	}
	// A transient read failure (a non-NotFound error): no decision (the
	// operator does not act on a failed read; the next reconcile retries).
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: loop.Namespace, Name: workspacePVCName(loop.Name)}, pvc); err != nil && !apierrors.IsNotFound(err) {
		return nil
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

// workspacePVCPresent reports whether the Loop's workspace PVC currently
// exists (a transient read failure is reported as "not present" — the
// caller's decision is retried next reconcile, and a failed read is not
// grounds for a restore decision — the conservative reading, as in B1's
// support check).
func (r *LoopReconciler) workspacePVCPresent(ctx context.Context, loop *coxv1alpha1.Loop) bool {
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: loop.Namespace, Name: workspacePVCName(loop.Name)}, pvc)
	return err == nil
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

// b2RestoreHoldSuspended reports whether the B2 workspace restore is in
// progress in a way that must hold the sandbox Suspended (the reviewer's
// design point (b), R-B2): the Restore condition is False, reason
// StoppingSandbox or DeletingPVC. ensureSandbox consults it BEFORE the
// gate cascade: while the restore is in progress, the sandbox must not be
// flipped back to Running mid-restore (a flip would remount the PVC the
// restore is about to delete — kubernetes.io/pvc-protection would hold the
// delete until the pod was gone again, stalling the sequence). The hold
// clears with the one-shot marker (True/Restored): from the next reconcile
// the sandbox is built as usual on the restored content (the A2 PVC-resume
// path). A REJECTED (False/RestoreSourceMissing) or DEFERRED (False/
// RestoreSourceNotReady) restore does NOT apply to the sandbox: those
// states leave the PVC and the sandbox untouched (the deferred restore may
// not touch the sandbox at all — its snapshot is not ready; the rejected
// target is gone).
func (r *LoopReconciler) b2RestoreHoldSuspended(loop *coxv1alpha1.Loop) bool {
	c := meta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.RestoreCondition)
	if c == nil || c.Status != metav1.ConditionFalse {
		return false
	}
	return c.Reason == coxv1alpha1.RestoreReasonStoppingSandbox || c.Reason == coxv1alpha1.RestoreReasonDeletingPVC
}

// sandboxPodGone reports whether the Loop's sandbox POD is gone — the
// REST of the restore's level-based sequence (the PVC delete) may advance
// only once the pod is gone.
//
// "Stopped" here means the POD is gone, NOT just the sandbox's
// spec.operatingMode == Suspended (the reviewer's design point (a), R-B2):
// spec-level suspension is a REQUEST — the pod still exists (and still
// mounts the workspace PVC) while the agent-sandbox controller acts on it,
// and kubernetes.io/pvc-protection holds the PVC until no pod uses it.
// Deleting the PVC on a Suspended-but-pod-still-present sandbox would not
// complete on a real cluster (the delete would stay in Terminating behind
// pvc-protection). The operator therefore advances the delete step only
// once the pod is actually gone (NotFound). A read error is reported as the
// error (the caller requeues; no decision on a transient read failure).
func (r *LoopReconciler) sandboxPodGone(ctx context.Context, loop *coxv1alpha1.Loop) (bool, error) {
	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: loop.Namespace, Name: sandboxName(loop.Name)}
	if err := r.Get(ctx, key, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil // no pod: the sandbox is gone
		}
		return false, err
	}
	return false, nil // the pod is still present
}

// stopSandboxForRestore sets the sandbox's operatingMode to Suspended (the
// A2 kill path's stop: a PVC delete is held by kubernetes.io/pvc-protection
// while the sandbox's pod still mounts the PVC — the operator must stop the
// sandbox first and let the pod go before the PVC can be deleted). A missing
// sandbox is a no-op (nothing to stop — the fork case and a Loop whose
// sandbox was never built).
func (r *LoopReconciler) stopSandboxForRestore(ctx context.Context, loop *coxv1alpha1.Loop) error {
	sb := &sandboxv1beta1.Sandbox{}
	key := client.ObjectKey{Namespace: loop.Namespace, Name: sandboxName(loop.Name)}
	if err := r.Get(ctx, key, sb); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get sandbox %s for the restore stop: %w", key, err)
	}
	if sb.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeSuspended {
		return nil // already suspended
	}
	sb.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
	if err := r.Update(ctx, sb); err != nil {
		return fmt.Errorf("suspend sandbox %s for the restore: %w", key, err)
	}
	log.FromContext(ctx).Info("sandbox suspended for the workspace restore (pvc-protection holds a mounted PVC)", "sandbox", sandboxName(loop.Name), "loop", loop.Name)
	return nil
}

// deleteWorkspacePVC deletes the Loop's workspace PVC (the level-based
// restore's delete step). The delete is ASYNC (the apiserver removes the
// object only after the finalizer work is done — on a real cluster the csi
// driver releases the volume and the pvc-protection finalizer is removed
// when no pod uses the PVC); the caller does NOT wait (the Reconcile must
// never block on the PVC's delete — the reviewer's P1, the R-B2 review at
// 20ef538). A NotFound is a success (the PVC was already gone).
func (r *LoopReconciler) deleteWorkspacePVC(ctx context.Context, loop *coxv1alpha1.Loop) error {
	pvc := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Namespace: loop.Namespace, Name: workspacePVCName(loop.Name)}
	if err := r.Get(ctx, key, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // already gone
		}
		return fmt.Errorf("read workspace PVC %s for the restore delete: %w", key, err)
	}
	if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete workspace PVC %s for the restore: %w", key, err)
	}
	log.FromContext(ctx).Info("workspace PVC deleted for the restore (the delete is async; the PVC is recreated from the snapshot once it is gone)",
		"pvc", workspacePVCName(loop.Name), "loop", loop.Name)
	return nil
}

// createWorkspacePVCFromSnapshot creates the Loop's workspace PVC with
// spec.dataSource = the named VolumeSnapshot (the level-based restore's
// create step — it runs only when the PVC is GONE, so the name is free).
// AlreadyExists is a success (a concurrent pass created it; the content is
// from the snapshot either way). The restored PVC is controller-owned by
// the Loop (the same GC contract as the original PVC — a deleted Loop's
// restored PVC is garbage-collected with it).
func (r *LoopReconciler) createWorkspacePVCFromSnapshot(ctx context.Context, loop *coxv1alpha1.Loop, snapshotName string) error {
	newPVC := &corev1.PersistentVolumeClaim{}
	newPVC.Spec = workspacePVCSpec(loop).Spec
	newPVC.Name = workspacePVCName(loop.Name)
	newPVC.Namespace = loop.Namespace
	snapshotGroup := snapshotGroupRef
	newPVC.Spec.DataSource = &corev1.TypedLocalObjectReference{
		APIGroup: &snapshotGroup,
		Kind:     volumeSnapshotKind,
		Name:     snapshotName,
	}
	if err := controllerutil.SetControllerReference(loop, newPVC, r.Scheme); err != nil {
		return fmt.Errorf("set owner on restored workspace PVC %s: %w", newPVC.Name, err)
	}
	if err := r.Create(ctx, newPVC); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil // a concurrent pass created it
		}
		return fmt.Errorf("create restored workspace PVC %s from snapshot %s: %w", newPVC.Name, snapshotName, err)
	}
	log.FromContext(ctx).Info("workspace PVC recreated from the checkpoint snapshot",
		"pvc", newPVC.Name, "loop", loop.Name, "snapshot", snapshotName)
	return nil
}

// stopWorkloadsForRestore deletes the Loop-owned verify and deliver Jobs
// (the restore's new step 0, the reviewer's P1, the R-B2 review at 6833e2e:
// the restore logic only waits for the SANDBOX pod (not the verify/deliver
// Job pods). The verify pods mount the PVC and keep it protected
// (kubernetes.io/pvc-protection). The restore must also delete or await
// every Loop-owned pod mounting the PVC (verify and deliver Job pods), as a
// level-based step with a visible reason).
//
// The verify and deliver Jobs use DIFFERENT label keys than the Loop label
// (loopLabelKey): the verify Job uses verifyLoopLabel ("coxswain.io/loop"),
// the deliver Job uses deliverForLabel ("coxswain.io/deliver-for"). The
// method lists Jobs with BOTH label keys (the verify and deliver Jobs) and
// deletes them (propagationPolicy=Background, so the Job controller deletes
// the Pods once the Jobs are gone). The method then lists ALL Pods in the
// namespace and reports whether any Loop-owned pod mounts the PVC (the pod
// must carry one of the Loop's labels AND mount the PVC). "Stopped" = no
// such pod (the Job controller has deleted the Pods).
//
// (The envtest has no Job controller, so the spec must manually delete the
// Pods; the kind cluster has the Job controller, so the Pods are deleted
// automatically once the Jobs are deleted.)
//
// (b2ApplyRestoreStep calls this before StoppingSandbox — the restore's
// sequence is: StoppingWorkloads → StoppingSandbox → DeletingPVC →
// Restored.)
func (r *LoopReconciler) stopWorkloadsForRestore(ctx context.Context, loop *coxv1alpha1.Loop) (bool, error) {
	// List the verify Jobs (the B1 label key: verifyLoopLabel).
	verifyJobs := &batchv1.JobList{}
	if err := r.List(ctx, verifyJobs,
		client.InNamespace(loop.Namespace),
		client.MatchingLabels{verifyLoopLabel: loop.Name},
	); err != nil {
		return false, fmt.Errorf("list the verify Jobs for the restore: %w", err)
	}
	// List the deliver Jobs (the B1 label key: deliverForLabel).
	deliverJobs := &batchv1.JobList{}
	if err := r.List(ctx, deliverJobs,
		client.InNamespace(loop.Namespace),
		client.MatchingLabels{deliverForLabel: loop.Name},
	); err != nil {
		return false, fmt.Errorf("list the deliver Jobs for the restore: %w", err)
	}
	// Delete the Jobs (propagationPolicy=Background, so the Job controller
	// deletes the Pods once the Jobs are gone). The delete is idempotent
	// (IsNotFound is tolerated).
	for i := range verifyJobs.Items {
		job := &verifyJobs.Items[i]
		if job.DeletionTimestamp == nil {
			propagation := metav1.DeletePropagationBackground
			if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("delete the verify Job %s for the restore: %w", job.Name, err)
			}
		}
	}
	for i := range deliverJobs.Items {
		job := &deliverJobs.Items[i]
		if job.DeletionTimestamp == nil {
			propagation := metav1.DeletePropagationBackground
			if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("delete the deliver Job %s for the restore: %w", job.Name, err)
			}
		}
	}
	// List ALL Pods in the namespace and report whether any Loop-owned pod
	// mounts the workspace PVC. "Loop-owned" = the pod carries one of the
	// Loop's labels (loopLabelKey, verifyLoopLabel, or deliverForLabel).
	// "Mounts the PVC" = the pod's spec.volumes includes a
	// PersistentVolumeClaimVolumeSource with ClaimName = workspacePVCName(loop).
	//
	// The Pod list uses the APIReader (the non-cached client) because the
	// manager's Pod cache is scoped to ProxyComponentSelector (cmd/main.go:
	// the cache ByObject for corev1.Pod uses the proxy label selector), so a
	// cached List would never see the verify/deliver pods (the reviewer's P2,
	// the R-B2 review at e338d06: the Pod cache is limited to proxy pods, so
	// in production the cached List never sees verify pods). The APIReader
	// hits the API server directly, so the verify/deliver pods are always
	// visible. A nil apiReader (a bare test reconciler without
	// SetupWithManager) falls back to the cached client (the envtest has no
	// cache scoping, so the cached List works in envtest).
	//
	// Pods with a deletionTimestamp are NOT skipped (the reviewer's P2, the
	// R-B2 review at e338d06: a pod with a deletionTimestamp still mounts
	// the PVC until it is gone — the kubernetes.io/pvc-protection finalizer
	// holds the PVC until no pod uses it, so a pod being deleted still
	// protects the PVC). The restore must wait until the pod is fully gone
	// (not just terminating).
	podReader := client.Reader(r.Client)
	if r.apiReader != nil {
		podReader = r.apiReader
	}
	podList := &corev1.PodList{}
	if err := podReader.List(ctx, podList, client.InNamespace(loop.Namespace)); err != nil {
		return false, fmt.Errorf("list the pods for the restore: %w", err)
	}
	for i := range podList.Items {
		pod := &podList.Items[i]
		// DO NOT skip pods with a deletionTimestamp (the reviewer's P2, the
		// R-B2 review at e338d06: a pod with a deletionTimestamp still mounts
		// the PVC until it is gone — the kubernetes.io/pvc-protection
		// finalizer holds the PVC until no pod uses it, so a pod being
		// deleted still protects the PVC). The restore must wait until the
		// pod is fully gone (not just terminating).
		// Check if the pod is Loop-owned (it carries one of the Loop's labels).
		loopOwned := false
		if _, ok := pod.Labels[loopLabelKey]; ok && pod.Labels[loopLabelKey] == loop.Name {
			loopOwned = true
		}
		if _, ok := pod.Labels[verifyLoopLabel]; ok && pod.Labels[verifyLoopLabel] == loop.Name {
			loopOwned = true
		}
		if _, ok := pod.Labels[deliverForLabel]; ok && pod.Labels[deliverForLabel] == loop.Name {
			loopOwned = true
		}
		if !loopOwned {
			continue // the pod is not Loop-owned (it does not block the restore).
		}
		// Check if the pod mounts the workspace PVC.
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == workspacePVCName(loop.Name) {
				// The pod is Loop-owned AND mounts the PVC. The restore is
				// in progress (the PVC is protected while this pod is
				// present, even if the pod is being deleted).
				log.FromContext(ctx).Info("Loop-owned pod still mounts the workspace PVC; the restore is in progress",
					"pod", pod.Name, "pvc", workspacePVCName(loop.Name), "loop", loop.Name)
				return false, nil
			}
		}
	}
	// No Loop-owned pod mounts the workspace PVC (the Job controller has
	// deleted the Pods once the Jobs are deleted, or the spec manually
	// deleted them). The restore can proceed.
	log.FromContext(ctx).Info("No Loop-owned pod mounts the workspace PVC; the restore can proceed",
		"pvc", workspacePVCName(loop.Name), "loop", loop.Name)
	return true, nil
}

// setRestoreCondition sets the Restore condition (the B2 durable record)
// with the condition's observedGeneration = the Loop's current generation
// (the one-shot marker's re-arm check: a new spec.restoreFrom bumps the
// generation and invalidates the stale marker).
func setRestoreCondition(loop *coxv1alpha1.Loop, status bool, reason, message string) {
	st := metav1.ConditionFalse
	if status {
		st = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&loop.Status.Conditions, metav1.Condition{
		Type:               coxv1alpha1.RestoreCondition,
		Status:             st,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: loop.Generation,
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

// b2ApplyRestoreStep is the B2 restore step (the reviewer's P1, the R-B2
// review at 20ef538): the level-based workspace restore (restore /
// rollback / fork). It runs BEFORE phaseBootstrap in Reconcile. It:
//
//  1. computes the restore target (b2RestoreTarget — nil means no restore
//     applies this reconcile),
//  2. resolves the source snapshot (ready / notready / missing / error),
//  3. on notready: DEFERS (Restore=False reason RestoreSourceNotReady +
//     the Event; the requeue is the caller's 5s RequeueAfter — the
//     in-progress state, no restore decision, I49),
//  4. on error: requeues (no decision on a transient read failure),
//  5. on missing: the kind decides — a rollback/fork is TERMINAL
//     (Restore=False reason RestoreSourceMissing + the Event; no requeue —
//     the object is gone); a lost-PVC restore falls back to the A2 path
//     (recreate the PVC empty + Restore=False reason NoUsableCheckpoint +
//     the Restore Event — I90: the fallback is VISIBLE),
//  6. on ready: the PVC replacement, level-based:
//     - the sandbox running -> STOP it (Suspended) + Restore=False reason
//     StoppingSandbox (the PVC is held by pvc-protection while mounted —
//     delete it only once the sandbox is stopped, like the A2 kill
//     path);
//     - the sandbox stopped and the PVC present -> DELETE the PVC +
//     Restore=False reason DeletingPVC (a still-present PVC (a pending
//     delete) does NOT re-trigger the delete);
//     - the PVC gone -> CREATE it from the snapshot (AlreadyExists is a
//     success) + Restore=True reason Restored (the one-shot marker).
//
// It returns (changed, requeue, err). changed is true when the condition
// was (re)set this pass; requeue is true when a requeue is needed
// (notready, error, or an in-progress stop/delete — the 5s RequeueAfter in
// the caller's FINAL return); err is a real failure (the caller returns
// it). The step persists the condition at the point of decision (the I90
// write: a later step in the same reconcile failing must not lose the
// decision — the Status().Update here is what survives it), and the
// caller's finalizeLoopStatus skips the re-write when the step already
// persisted.
func (r *LoopReconciler) b2ApplyRestoreStep(ctx context.Context, loop *coxv1alpha1.Loop) (bool, bool, bool, error) {
	// A fork whose restore is complete (the one-shot marker) has nothing
	// left to do here EXCEPT remove its fork-retention label from the source
	// snapshot (the source's GC is then free to delete it — the fork's PVC
	// is self-sufficient). The step's target is nil for a completed fork
	// (b2RestoreTarget returns nil once the marker is set and the PVC is
	// present), so the removal is driven from here, before the target
	// check (the label's absence is the source-side release; a failed
	// removal is a no-op — the snapshot's persistence is a known B2
	// limitation, documented in gcLoopCheckpoints).
	if loop.Spec.ForkOf != nil {
		if c := meta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.RestoreCondition); c != nil &&
			c.Status == metav1.ConditionTrue && c.Reason == coxv1alpha1.RestoreReasonRestored &&
			c.ObservedGeneration >= loop.Generation && r.workspacePVCPresent(ctx, loop) {
			r.removeForkRetentionLabel(ctx, loop)
		}
	}
	target := r.b2RestoreTarget(ctx, loop)
	if target == nil {
		return false, false, false, nil // no restore applies this reconcile
	}
	// persist is the I90 write (the reviewer's guidance, R-B2): the
	// condition is already set on `loop` by the time it is called, so
	// persisting it HERE (at the point of decision) makes the decision
	// durable even when a later step in the same reconcile fails (e.g.
	// ensureSandbox). The Event is emitted immediately (the transient
	// signal fires at the point of decision, before any later failure can
	// hide it).
	persist := func() {
		if err := r.Status().Update(ctx, loop); err != nil {
			log.FromContext(ctx).Error(err, "the restore decision could not be persisted; it is retried on the next reconcile",
				"loop", loop.Name)
		}
	}
	snap, decision := r.resolveCheckpointSource(ctx, loop.Namespace, target.loopName, target.iteration)
	switch decision {
	case b2SourceReady:
		if loop.Spec.ForkOf != nil || target.kind == b2KindLostPVC {
			// A fork (a fresh Loop — the PVC is absent, the sandbox does not
			// exist) and a lost-PVC restore (the PVC is already gone) go
			// straight to the create.
			if err := r.createWorkspacePVCFromSnapshot(ctx, loop, target.snapshot); err != nil {
				setRestoreCondition(loop, false, coxv1alpha1.RestoreReasonRejected,
					"restoring the workspace PVC from "+target.snapshot+" ("+string(target.kind)+" iter-"+strconv.Itoa(target.iteration)+") failed: "+err.Error())
				persist()
				r.emitRestoreEvent(loop, false, coxv1alpha1.RestoreReasonRejected,
					"workspace PVC restore from "+target.snapshot+" failed; retrying")
				return true, true, true, nil
			}
			setRestoreCondition(loop, true, coxv1alpha1.RestoreReasonRestored,
				"workspace PVC restored from "+target.snapshot+" ("+string(target.kind)+", iter-"+strconv.Itoa(target.iteration)+")")
			// B2 FORK-RETENTION: a fork that has just restored its PVC claims
			// the release on the source's snapshot (the fork-retention label:
			// while present, the source's GC skips it — see
			// forkRetainedSnapshotLabelKey). A rollback or a lost-PVC restore
			// does NOT touch the label (it restores from the Loop's OWN
			// snapshot; the source's GC is unaffected).
			if target.kind == b2KindFork {
				r.markForkRetentionLabel(ctx, loop, target.snapshot)
			}
			persist()
			r.emitRestoreEvent(loop, true, coxv1alpha1.RestoreReasonRestored,
				"workspace PVC restored from "+target.snapshot)
			return true, false, false, nil
		}
		// The ROLLBACK (an explicit restoreFrom on a running Loop): the PVC
		// is present and the sandbox may hold it mounted (pvc-protection).
		if !r.workspacePVCPresent(ctx, loop) {
			// A rollback whose PVC is already gone (a previous pass deleted it,
			// the delete has completed): skip to the create (the delete is done,
			// the PVC is recreated from the snapshot).
			if err := r.createWorkspacePVCFromSnapshot(ctx, loop, target.snapshot); err != nil {
				setRestoreCondition(loop, false, coxv1alpha1.RestoreReasonRejected,
					"restoring the workspace PVC from "+target.snapshot+" ("+string(target.kind)+") failed: "+err.Error())
				persist()
				r.emitRestoreEvent(loop, false, coxv1alpha1.RestoreReasonRejected,
					"workspace PVC restore from "+target.snapshot+" failed; retrying")
				return true, true, true, nil
			}
			setRestoreCondition(loop, true, coxv1alpha1.RestoreReasonRestored,
				"workspace PVC restored from "+target.snapshot+" ("+string(target.kind)+", iter-"+strconv.Itoa(target.iteration)+")")
			persist()
			r.emitRestoreEvent(loop, true, coxv1alpha1.RestoreReasonRestored,
				"workspace PVC restored from "+target.snapshot)
			return true, false, false, nil
		}
		// Step 0: the Loop-owned verify and deliver Jobs must be STOPPED
		// (the reviewer's P1, the R-B2 review at 6833e2e: the restore logic
		// only waits for the SANDBOX pod (not the verify/deliver Job pods).
		// The verify pods mount the PVC and keep it protected
		// (kubernetes.io/pvc-protection). The restore must also delete or
		// await every Loop-owned pod mounting the PVC (verify and deliver Job
		// pods), as a level-based step with a visible reason).
		// "Stopped" = no Loop-owned pod mounts the workspace PVC (the Job
		// controller deletes the Pods once the Jobs are gone; the PVC is held
		// by kubernetes.io/pvc-protection until no pod uses it, so the
		// sandbox stop (below) may advance only once no Loop-owned pod
		// mounts the PVC). A still-present pod reports StoppingWorkloads (the
		// in-progress state — no PVC change yet, I49).
		workloadsStopped, err := r.stopWorkloadsForRestore(ctx, loop)
		if err != nil {
			log.FromContext(ctx).Error(err, "the restore could not stop the Loop-owned workloads; no decision on a transient read failure",
				"loop", loop.Name)
			return false, true, true, nil
		}
		if !workloadsStopped {
			setRestoreCondition(loop, false, coxv1alpha1.RestoreReasonStoppingWorkloads,
				"restoring the workspace PVC from "+target.snapshot+" ("+string(target.kind)+"): the Loop-owned workloads are being stopped (step 0 of 4; the sandbox is stopped once no Loop-owned pod mounts the PVC)")
			persist()
			r.emitRestoreEvent(loop, false, coxv1alpha1.RestoreReasonStoppingWorkloads,
				"restoring the workspace PVC from "+target.snapshot+": the Loop-owned workloads are being stopped (step 0 of 4)")
			return true, true, true, nil
		}
		// Step 1: the sandbox must be STOPPED (the A2 kill path's stop).
		// "Stopped" = the sandbox POD is gone (the reviewer's design point
		// (a), R-B2): spec.operatingMode == Suspended is a REQUEST and the
		// pod may still exist while it drains; kubernetes.io/pvc-protection
		// holds the PVC until no pod uses it, so the delete step (below) may
		// advance only once the pod is gone. A still-present pod reissues the
		// suspend (idempotent) and reports StoppingSandbox (the in-progress
		// state — no PVC change yet, I49).
		podGone, err := r.sandboxPodGone(ctx, loop)
		if err != nil {
			log.FromContext(ctx).Error(err, "the restore could not read the sandbox pod; no decision on a transient read failure",
				"loop", loop.Name)
			return false, true, true, nil
		}
		if !podGone {
			if err := r.stopSandboxForRestore(ctx, loop); err != nil {
				setRestoreCondition(loop, false, coxv1alpha1.RestoreReasonRejected,
					"stopping the sandbox before the rollback restore of "+target.snapshot+" failed: "+err.Error())
				persist()
				r.emitRestoreEvent(loop, false, coxv1alpha1.RestoreReasonRejected,
					"sandbox stop for the rollback restore failed; retrying")
				return true, true, true, nil
			}
			setRestoreCondition(loop, false, coxv1alpha1.RestoreReasonStoppingSandbox,
				"restoring the workspace PVC from "+target.snapshot+" ("+string(target.kind)+"): the sandbox is being stopped (step 1 of 4; the PVC is deleted once the sandbox pod is gone)")
			persist()
			r.emitRestoreEvent(loop, false, coxv1alpha1.RestoreReasonStoppingSandbox,
				"restoring the workspace PVC from "+target.snapshot+": the sandbox is being stopped (step 1 of 4)")
			return true, true, true, nil
		}
		// The sandbox is stopped. The PVC is present (checked above):
		// DELETE it (the A2 kill path's delete). The delete is async
		// (pvc-protection releases it once the pod is gone; the apiserver
		// sets the deletionTimestamp immediately).
		if err := r.deleteWorkspacePVC(ctx, loop); err != nil {
			setRestoreCondition(loop, false, coxv1alpha1.RestoreReasonRejected,
				"deleting the workspace PVC for the rollback restore from "+target.snapshot+" failed: "+err.Error())
			persist()
			r.emitRestoreEvent(loop, false, coxv1alpha1.RestoreReasonRejected,
				"workspace PVC delete for the rollback restore failed; retrying")
			return true, true, true, nil
		}
		setRestoreCondition(loop, false, coxv1alpha1.RestoreReasonDeletingPVC,
			"restoring the workspace PVC from "+target.snapshot+" ("+string(target.kind)+"): the PVC is deleted (step 2 of 4; the PVC is recreated from the snapshot once the delete completes)")
		persist()
		r.emitRestoreEvent(loop, false, coxv1alpha1.RestoreReasonDeletingPVC,
			"restoring the workspace PVC from "+target.snapshot+": the PVC is deleted (step 2 of 3)")
		return true, true, true, nil
	case b2SourceNotReady:
		// DEFER: the snapshot exists but is not ReadyToUse (the in-progress
		// state — no restore decision, I49). The condition names it; the
		// requeue retries (the 5s RequeueAfter in the caller's final return).
		_ = snap
		setRestoreCondition(loop, false, coxv1alpha1.RestoreReasonSourceNotReady,
			"checkpoint "+target.snapshot+" ("+string(target.kind)+") exists but is not ReadyToUse yet; the restore is deferred")
		persist()
		r.emitRestoreEvent(loop, false, coxv1alpha1.RestoreReasonSourceNotReady,
			"checkpoint "+target.snapshot+" not ReadyToUse yet; restore deferred")
		return true, true, true, nil
	case b2SourceMissing:
		switch target.kind {
		case b2KindRollback, b2KindFork:
			// TERMINAL: the explicit target is gone. The condition names it;
			// NO requeue (the object will not come back — the source Loop's
			// GC deleted it, or it was never taken).
			setRestoreCondition(loop, false, coxv1alpha1.RestoreReasonSourceMissing,
				"checkpoint "+target.snapshot+" ("+string(target.kind)+") does not exist (it was never taken, or it was GC'd); the restore is rejected")
			persist()
			r.emitRestoreEvent(loop, false, coxv1alpha1.RestoreReasonSourceMissing,
				"checkpoint "+target.snapshot+" does not exist; restore rejected")
			// The hold covers the REJECTED state (the reviewer's diagnosis 2,
			// R-B2): a rejected fork is not seeded with an empty PVC by the
			// bootstrap (phaseBootstrap -> ensureLoopArtifacts would create it
			// in the same pass, masking the reject as "a fresh empty
			// workspace"). The hold stays while the target applies: on the
			// next pass b2RestoreTarget is nil only when the Restore marker
			// says True/Restored (it says False/RestoreSourceMissing here), so
			// the hold is permanent and the user's choice is visible (the
			// condition + the Event) — seed it by deleting + recreating the
			// fork's Loop, or wait for the source's snapshot to reappear.
			return true, false, true, nil
		default: // b2KindLostPVC
			// The A2 fallback: the PVC is lost and the (latest) checkpoint
			// is gone. Recreate the PVC EMPTY (the A2 PVC-resume — the
			// content is lost, the Loop continues from the untouched base)
			// and surface the fallback (I90: VISIBLE).
			if err := r.ensureWorkspacePVC(ctx, loop); err != nil {
				return false, true, false, fmt.Errorf("A2 fallback: recreate workspace PVC empty: %w", err)
			}
			setRestoreCondition(loop, false, coxv1alpha1.RestoreReasonNoUsableCheckpoint,
				"the workspace PVC was lost and checkpoint "+target.snapshot+" does not exist; fell back to the A2 PVC-resume (an EMPTY workspace — the checkpoint's content is lost, I90)")
			persist()
			r.emitRestoreEvent(loop, false, coxv1alpha1.RestoreReasonNoUsableCheckpoint,
				"workspace PVC lost, no usable checkpoint; A2 fallback (empty workspace)")
			return true, false, false, nil
		}
	case b2SourceError:
		// A transient read failure: no decision, requeue.
		log.FromContext(ctx).V(1).Info("restore: reading the checkpoint snapshot failed; requeueing (no decision on a transient read failure)",
			"snapshot", target.snapshot, "kind", string(target.kind))
		return false, true, true, nil
	}
	return false, false, false, nil
}

// forkRetainedSnapshotLabelKey is the label key the fork's operator writes
// onto the source Loop's checkpoint snapshot (the value is the fork's Loop
// name). See the interaction documentation at the top of this file (the
// step's comment block above b2ApplyRestoreStep covers the decided
// interaction with the source's retention and finalizer).
const forkRetainedSnapshotLabelKey = "coxswain.wattu.com/fork-of"

// forkSnapshotLabelKey is the label key on the source snapshot (the same as
// forkRetainedSnapshotLabelKey; a separate name for the reader's clarity —
// the label KEY is shared, the VALUE is the fork's name).
const forkSnapshotLabelKey = forkRetainedSnapshotLabelKey

// markForkRetentionLabel sets the fork-retention label on the source's
// checkpoint snapshot (the fork's PVC restore just created the fork's PVC
// from this snapshot; while the label is present the source's GC skips the
// snapshot — the safety net for the race in which the source's GC runs
// between the fork's snapshot read and the fork's PVC create). Best-effort:
// a failed label write does NOT break the fork (the fork's PVC create has
// already been accepted — the content is the csi driver's to materialise,
// not the snapshot's to keep serving); the label's persistence is a known
// B2 limitation (a snapshot the source's GC skips forever), documented in
// gcLoopCheckpoints.
func (r *LoopReconciler) markForkRetentionLabel(ctx context.Context, fork *coxv1alpha1.Loop, snapshotName string) {
	if !r.snapshotCRDPresentCall(ctx) {
		return // no CRD: the snapshot is not present
	}
	snap := &unstructured.Unstructured{Object: map[string]any{unstructuredAPI: snapshotGV, unstructuredKind: volumeSnapshotKind}}
	snap.SetGroupVersionKind(volumeSnapshotGVK)
	key := types.NamespacedName{Namespace: fork.Namespace, Name: snapshotName}
	if err := r.Get(ctx, key, snap); err != nil {
		log.FromContext(ctx).V(1).Info("fork: the source snapshot is absent; the fork-retention label could not be set (best-effort)",
			"snapshot", snapshotName, "fork", fork.Name)
		return
	}
	labels := snap.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[forkSnapshotLabelKey] = fork.Name
	snap.SetLabels(labels)
	if err := r.Update(ctx, snap); err != nil {
		log.FromContext(ctx).V(1).Info("fork: setting the fork-retention label on the source's snapshot failed; the label persists (a known B2 limitation; the fork's PVC is self-sufficient)",
			"snapshot", snapshotName, "fork", fork.Name, "err", err.Error())
	}
}

// removeForkRetentionLabel removes the fork-retention label from the source
// Loop's checkpoint snapshot (the fork's restore is complete — the fork's
// PVC owns its content — so the source's GC is free to delete the snapshot).
// The source snapshot's name is derived from the fork's spec.forkOf (the
// checkpoint naming is deterministic: checkpoint-<source>-iter-<N>).
// Best-effort: a failure is logged and the label persists (a known B2
// limitation — the source's GC skips a labelled snapshot, so a failed
// removal orphans the snapshot; the data is in the fork's PVC, which the
// fork's own GC deletes).
func (r *LoopReconciler) removeForkRetentionLabel(ctx context.Context, fork *coxv1alpha1.Loop) {
	if !r.snapshotCRDPresentCall(ctx) {
		return
	}
	src := fork.Spec.ForkOf
	snapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: src.Loop}}, src.Iteration)
	snap := &unstructured.Unstructured{Object: map[string]any{unstructuredAPI: snapshotGV, unstructuredKind: volumeSnapshotKind}}
	snap.SetGroupVersionKind(volumeSnapshotGVK)
	key := types.NamespacedName{Namespace: fork.Namespace, Name: snapName}
	if err := r.Get(ctx, key, snap); err != nil {
		return // the snapshot is absent (already GC'd — a no-op)
	}
	labels := snap.GetLabels()
	if labels == nil {
		return // no labels: the label was never set (a no-op)
	}
	if _, ok := labels[forkSnapshotLabelKey]; !ok {
		return // the label is absent (a no-op: it was never set, or it was
		// already removed)
	}
	delete(labels, forkSnapshotLabelKey)
	snap.SetLabels(labels)
	if err := r.Update(ctx, snap); err != nil {
		log.FromContext(ctx).V(1).Info("fork: removing the fork-retention label from the source's snapshot failed; the label persists (a known B2 limitation; the fork's PVC is self-sufficient)",
			"snapshot", snapName, "fork", fork.Name, "err", err.Error())
	}
}
