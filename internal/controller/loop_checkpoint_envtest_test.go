// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// B1 (PLAN-BETA): the workspace checkpoints. After each successful verify the
// operator takes a VolumeSnapshot of the Loop's workspace PVC
// (checkpoint-<loop>-iter-<N>), records it in status.checkpoint and
// status.checkpoints, applies retention (the last 3 plus every milestone),
// and — via the finalizer — deletes the Loop's snapshots in CleaningUp.
//
// D59: checkpoints are mandatory only when the Loop asks for them.
// BestEffort (default) takes snapshots when possible and, when they are not
// (no VolumeSnapshot CRD, or no VolumeSnapshotClass for the PVC's CSI
// driver), sets CheckpointsAvailable=False with a named reason and carries on
// with the A2 PVC-resume (a VISIBLE fallback, I90). Required fails at start
// (terminal Failed:CheckpointUnavailable) when they are not possible.
//
// The specs (envtest, no snapshot controller — the specs assert the OBJECT
// the operator creates, not the content it would produce):
//
//   - create + naming: a successful verify creates checkpoint-<loop>-iter-1
//     with spec.source.persistentVolumeClaimName = <loop>-workspace, and
//     records status.checkpoint / status.checkpoints,
//   - retention + GC: a status with more than last-3 + milestones GCs the
//     non-kept snapshots (a named mutation — break retention — must fail),
//   - finalizer: the finalizer is added; a deleting Loop with snapshots does
//     not delete until they are gone; a deleting Loop with none deletes,
//   - BestEffort without the CRD: CheckpointsAvailable=False reason
//     VolumeSnapshotCRDMissing, the Loop still Succeeds (carries on),
//   - Required without the CRD: terminal Failed:CheckpointUnavailable at
//     start (the named mutation — drop the Required check — must fail),
//   - the same-Loop update spec (I43): the operator does not re-take a
//     checkpoint for an already-recorded iteration, and a new verify
//     produces a new iteration + a new checkpoint (the operator UPDATES the
//     object — re-reconciles and the status changes — not just creates it).
//
// The checkpoint seam: the workspace PVC is created with a nil
// storageClassName (the cluster's default StorageClass), so the LIVE driver
// check defers (Unknown) in envtest. The specs drive the support check two
// ways: (a) the no-CRD case overrides snapshotCRDPresent (the method, so a
// test can set it to false without a RESTMapper) and asserts the
// CRD-missing branch; (b) the support-present case drives
// r.workspaceDriverSeam to (driver, true) and creates a matching
// VolumeSnapshotClass (cluster-scoped, unstructured) so the support check
// resolves True and the checkpoint is taken.

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

const (
	b1HeadCommit = "c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4" // 40-hex
	b1BaseCommit = "d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5" // 40-hex
	b1Driver     = "hostpath.csi.k8s.io"                      // the csi-hostpath-driver's provisioner (the kind e2e uses the same)
)

// b1LoopSpec is a minimal Loop (no workspace repo, one acceptance check) so
// the verify outcome path is the minimal one (a single check-0).
func b1LoopSpec() coxv1alpha1.LoopSpec {
	return coxv1alpha1.LoopSpec{
		Goal:      loopGoal,
		Workspace: testWorkspace(),
		Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
	}
}

// b1Reconcile runs one Reconcile and returns the fresh Loop.
func b1Reconcile(r *LoopReconciler, ns, name string) *coxv1alpha1.Loop {
	_, err := r.Reconcile(context.Background(),
		reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
	Expect(err).NotTo(HaveOccurred())
	loop := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	return loop
}

// b1NewReconciler builds a fresh reconciler for a Loop. withSupport drives
// the support check to present (the CRD seam is the default — present — and
// the driver seam resolves b1Driver). withoutCRD overrides
// snapshotCRDPresent to false (the no-CRD branch).
func b1NewReconciler(withSupport, withoutCRD bool) *LoopReconciler {
	r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
	if withoutCRD {
		r.snapshotCRDPresent = func(context.Context) bool { return false }
	}
	if withSupport {
		r.workspaceDriverSeam = func(context.Context, *coxv1alpha1.Loop) (string, bool) { return b1Driver, true }
	}
	return r
}

// b1CreateVSC creates the cluster-scoped VolumeSnapshotClass (unstructured).
// The vendored CRD's v1 schema is FLAT (driver and deletionPolicy at the top
// level, NOT nested under a spec key — the spec key is not in the v1
// openAPIV3Schema, so a nested spec is dropped by the apiserver's structural
// pruning, which is why a spec-nested object fails validation with "driver:
// Required value, deletionPolicy: Required value").
func b1CreateVSC() {
	ctx := context.Background()
	vsc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion":     snapshotGV,
		"kind":           volumeSnapshotClassKind,
		"metadata":       map[string]any{"name": "b1-test-vsc"},
		"driver":         b1Driver,
		"deletionPolicy": "Delete",
	}}
	if err := k8sClient.Create(ctx, vsc); err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).ToNot(HaveOccurred(), "create the VolumeSnapshotClass fixture")
	}
}

// b1MakeSnapshotObj builds an unstructured VolumeSnapshot object the way the
// operator builds it (the spec uses it to seed the GC fixture snapshots).
func b1MakeSnapshotObj(loopName, ns string, iteration int) *unstructured.Unstructured {
	u := newVolumeSnapshotObj(&coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: loopName, Namespace: ns},
	}, checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: loopName, Namespace: ns}}, iteration), "")
	return u
}

// b1CreateLoopSnapshots seeds the status + the VolumeSnapshot objects for a
// Loop with the given iterations (the GC fixture: the operator's GC reads the
// status entries and deletes the snapshots behind them).
func b1CreateLoopSnapshots(ctx context.Context, loop *coxv1alpha1.Loop) {
	for i := range loop.Status.Checkpoints {
		cp := loop.Status.Checkpoints[i]
		snap := b1MakeSnapshotObj(loop.Name, loop.Namespace, cp.Iteration)
		if err := k8sClient.Create(ctx, snap); err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).ToNot(HaveOccurred())
		}
	}
}

// b1SnapshotExists reports whether a VolumeSnapshot object exists.
func b1SnapshotExists(ctx context.Context, ns, name string) bool {
	snap := &unstructured.Unstructured{}
	snap.SetGroupVersionKind(volumeSnapshotGVK)
	return !apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, snap))
}

// b1CheckpointCondition returns the Loop's CheckpointsAvailable condition.
func b1CheckpointCondition(loop *coxv1alpha1.Loop) *metav1.Condition {
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == string(coxv1alpha1.CheckpointsAvailableCondition) {
			return &loop.Status.Conditions[i]
		}
	}
	return nil
}

// b1DriveToSucceeded drives a fresh Loop to a successful verify (Succeeded,
// iteration 1) via the claim path (Planning claim -> Implementing claim with
// a 40-hex headCommit -> the verify Job pod's checks all exit 0). It returns
// the reconciler (the caller keeps using it).
//
// (The operator's own verify path creates the verify Job and reads the pod's
// init exit codes; envtest has no Job controller, so the spec creates the
// pod the Job would create, the s5a idiom — the same stand-in the S5a specs
// use. The workspace PVC is created by ensureLoopArtifacts on the first
// reconcile (nil storageClassName — the cluster default).)
// b1DriveToSucceeded drives a fresh Loop to a successful verify (Succeeded,
// iteration 1) via the established S5a idiom: s5aDriveToVerifying (bootstrap
// -> Planning -> Implementing with the headCommit -> Verifying) then a
// successful verify pod (s5aVerifyPod, check-0 exit 0) + one reconcile. It
// returns the reconciler (the caller keeps using it) so the spec can read the
// fresh Loop and assert the checkpoint the reconcile took.
func b1DriveToSucceeded(ctx context.Context, r *LoopReconciler, ns, name string) {
	// Drive to Verifying using the SPEC's reconciler r (so the driver seam
	// and CRD seam are active on every reconcile, not just the final one).
	s5aDriveToVerifyingWithReconciler(r, ns, name)
	// The successful verify pod (a stand-in the operator's verify path reads
	// init exit codes from; envtest has no Job controller).
	jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
	s5aVerifyPod(ctx, ns, name, jobName, 0)
	// Reconcile: the successful verify -> Succeeded, iteration 1, and the
	// checkpoint is taken (after applyVerifyOutcome flips the phase).
	b1Reconcile(r, ns, name)
}

var _ = Describe("B1: workspace checkpoints (PLAN-BETA)", func() {
	ctx := context.Background()

	AfterEach(func() {
		// Clean up the cluster-scoped VSC fixtures (the namespaced fixtures
		// go with the namespace; the VSCs are cluster-scoped).
		_ = k8sClient.Delete(context.Background(), &unstructured.Unstructured{
			Object: map[string]any{"apiVersion": snapshotGV, "kind": volumeSnapshotClassKind,
				"metadata": map[string]any{"name": "b1-test-vsc"}},
		})
	})

	It("takes the workspace checkpoint after a successful verify and records it (create + naming)", func() {
		ns := "b1-create-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		b1CreateVSC()

		name := "cploop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())

		r := b1NewReconciler(true, false)
		b1DriveToSucceeded(ctx, r, ns, name)
		b1Reconcile(r, ns, name) // the reconcile that records the successful verify + takes the checkpoint

		By("creating the checkpoint VolumeSnapshot (checkpoint-<loop>-iter-0, the workspace PVC as the source)")
		snap := &unstructured.Unstructured{}
		snap.SetGroupVersionKind(volumeSnapshotGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "checkpoint-" + name + "-iter-0", Namespace: ns}, snap)).To(Succeed(),
			"B1: a successful verify must create checkpoint-<loop>-iter-0")
		pvcName, ok, _ := unstructured.NestedString(snap.Object, "spec", "source", "persistentVolumeClaimName")
		Expect(ok).To(BeTrue(), "the snapshot must source the workspace PVC")
		Expect(pvcName).To(Equal(name+"-workspace"), "the snapshot sources the Loop's workspace PVC (<loop>-workspace)")
		Expect(snap.GetLabels()["coxswain.wattu.com/loop"]).To(Equal(name), "the snapshot carries the Loop label (the GC + the watch map)")

		By("recording status.checkpoint (the latest) and status.checkpoints (the list)")
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Checkpoint).NotTo(BeNil(), "status.checkpoint is the latest checkpoint")
		Expect(loop.Status.Checkpoint.Iteration).To(Equal(0))
		Expect(loop.Status.Checkpoint.Name).To(Equal("checkpoint-" + name + "-iter-0"))
		Expect(loop.Status.Checkpoint.Milestone).To(BeTrue(), "a successful-verify checkpoint is a milestone")
		Expect(loop.Status.Checkpoints).To(HaveLen(1))
		Expect(loop.Status.Checkpoints[0].Name).To(Equal("checkpoint-" + name + "-iter-0"))

		By("reporting CheckpointsAvailable=True (the support was present)")
		c := b1CheckpointCondition(loop)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal(CheckpointReasonAvailable))
	})

	It("takes the checkpoint for iteration 0 (a recorded outcome at iteration 0 must not be treated as 'not recorded'; must FAIL against the int/Iteration==0 version)", func() {
		ns := "b1-iter0-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		b1CreateVSC()

		name := "iter0loop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())

		r := b1NewReconciler(true, false)
		b1DriveToSucceeded(ctx, r, ns, name) // drives the verify at iteration 0 to Succeeded
		b1Reconcile(r, ns, name)

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		// The recorded outcome is iteration 0 (a valid value). The checkpoint
		// must be taken for it — the int/Iteration==0 version treats 0 as
		// 'not recorded' and never creates the iter-0 checkpoint.
		Expect(loop.Status.Verify).NotTo(BeNil(), "the operator recorded the verify outcome")
		Expect(loop.Status.Verify.Iteration).NotTo(BeNil(), "the recorded iteration is present (iteration 0 is a valid value)")
		Expect(int(*loop.Status.Verify.Iteration)).To(Equal(0), "the verify ran at iteration 0")
		Expect(loop.Status.Checkpoint).NotTo(BeNil(), "the iteration-0 checkpoint MUST be taken (the int/Iteration==0 version skips it)")
		Expect(loop.Status.Checkpoint.Iteration).To(Equal(0), "the checkpoint is named for iteration 0")
		Expect(loop.Status.Checkpoint.Milestone).To(BeTrue(), "all checks passed (exit 0), so the checkpoint is a milestone")
	})

	It("does NOT re-take a checkpoint for an already-recorded iteration (idempotency, the same-Loop update spec I43)", func() {
		ns := "b1-idem-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		b1CreateVSC()

		name := "idemloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())

		r := b1NewReconciler(true, false)
		b1DriveToSucceeded(ctx, r, ns, name)
		b1Reconcile(r, ns, name) // the reconcile that records the successful verify + takes the checkpoint

		By("re-reconciling (same Loop): the operator does NOT create a second snapshot for the recorded iteration")
		for range 3 {
			b1Reconcile(r, ns, name)
		}
		// Exactly ONE snapshot object (a re-take would create a different
		// name, so it cannot be a duplicate-object AlreadyExists — but the
		// operator must not even ATTEMPT a re-create: the status already
		// records the iteration, so ensureCheckpoint's guard returns early and
		// no Create call is issued).
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(volumeSnapshotListGVK)
		Expect(k8sClient.List(ctx, list, client.InNamespace(ns),
			client.MatchingLabels{"coxswain.wattu.com/loop": name})).To(Succeed())
		Expect(list.Items).To(HaveLen(1), "no re-take: exactly one checkpoint snapshot after repeated reconciles")
		Expect(list.Items[0].GetName()).To(Equal("checkpoint-" + name + "-iter-0"))

		// The status is UNCHANGED by the re-reconcile (the operator does not
		// append a duplicate entry): the list is still one entry.
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Checkpoints).To(HaveLen(1), "the re-reconcile must not append a duplicate status entry")
	})

	It("takes the checkpoint on a LATER reconcile when the first create failed (the interceptor fails the FIRST VolumeSnapshot Create; no checkpoint is recorded, CheckpointsAvailable=False CreateFailed, requeue; a later reconcile with the normal client creates iter-0; level-based, not edge-triggered)", func() {
		ns := "b1-retry-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		b1CreateVSC()

		name := "retryloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())

		// The first checkpoint create is made to fail by a WRAPPING client:
		// the first Create whose object is a VolumeSnapshot returns an
		// injected (non-AlreadyExists) error; every other call passes through
		// to the real client. The reconciler uses the wrapped client, so
		// ensureCheckpoint's r.Create gets the real error — the CreateFailed
		// path (condition False + requeue).
		wrapped := B1CreateFailClient(k8sClient, 1)
		r := b1NewReconciler(true, false)
		r.Client = wrapped
		r.apiReader = wrapped

		s5aDriveToVerifyingWithReconciler(r, ns, name)
		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		s5aVerifyPod(ctx, ns, name, jobName, 0)

		By("the first checkpoint reconcile: the create fails (injected), so no snapshot object, no status record, CheckpointsAvailable=False CreateFailed, and a requeue")
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).NotTo(BeZero(), "a checkpoint create failure requeues (the retry is level-based, not edge-triggered)")
		Expect(b1SnapshotExists(ctx, ns, "checkpoint-"+name+"-iter-0")).To(BeFalse(), "the failed create left no snapshot object")
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Verify).NotTo(BeNil(), "the verify outcome IS recorded (the checkpoint retry keys off it)")
		Expect(loop.Status.Checkpoints).To(BeEmpty(), "no checkpoint is recorded when the create fails")
		c := b1CheckpointCondition(loop)
		Expect(c).NotTo(BeNil(), "the failure is surfaced (a VISIBLE fallback, I90)")
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(CheckpointReasonCreateFailed))

		By("a later reconcile (no verify change; the normal client) creates checkpoint-<loop>-iter-0")
		rNormal := b1NewReconciler(true, false)
		b1Reconcile(rNormal, ns, name)
		Expect(b1SnapshotExists(ctx, ns, "checkpoint-"+name+"-iter-0")).To(BeTrue(),
			"the level-based retry creates the checkpoint on the later reconcile")
		fresh := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, fresh)).To(Succeed())
		Expect(fresh.Status.Checkpoint).NotTo(BeNil())
		Expect(fresh.Status.Checkpoint.Iteration).To(Equal(0), "the checkpoint is named for the recorded verify's iteration (0)")
		Expect(fresh.Status.Checkpoint.Milestone).To(BeTrue(), "the verify passed, so the checkpoint is a milestone")
		c = b1CheckpointCondition(fresh)
		Expect(c.Status).To(Equal(metav1.ConditionTrue), "the support is available again on the retry")
		Expect(c.Reason).To(Equal(CheckpointReasonAvailable))
	})

	It("takes the checkpoint for a FAILED verify on the iterate path: checkpoint-<loop>-iter-N with Milestone=false on a later reconcile, and NO checkpoint for iter-(N+1) (the iterate increments to N+1 and persists it; the checkpoint create for N may have failed or been skipped; must FAIL against 06767ac which keys off the current iteration)", func() {
		ns := "b1-fail-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		b1CreateVSC()

		name := "failloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())

		// Drive to Verifying at iteration 0, then make the verify FAIL
		// (check-0 exits 1). The operator records the outcome for iteration 0
		// (lastCheckResults = [1]) and iterates to 1 (Implementing). The
		// checkpoint create for iteration 0 is skipped on that reconcile
		// (support absent — the driver seam reports unavailable), so nothing
		// is recorded yet.
		rUnavailable := b1NewReconciler(false, false)
		s5aDriveToVerifyingWithReconciler(rUnavailable, ns, name)
		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		s5aVerifyPod(ctx, ns, name, jobName, 1) // check-0 FAILS

		b1Reconcile(rUnavailable, ns, name) // iterate path: record outcome for iter 0, iterate 0 -> 1
		// The iterate deleted the (stand-in) Sandbox; re-ensure it so the
		// next reconcile (a plain b1Reconcile that asserts no error) does not
		// wedge on the missing sandbox.
		s5aEnsureSandbox(ns, name)
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Verify).NotTo(BeNil(), "the FAILED verify outcome is recorded")
		Expect(loop.Status.Verify.Iteration).NotTo(BeNil())
		Expect(int(*loop.Status.Verify.Iteration)).To(Equal(0), "the outcome is recorded for iteration 0")
		Expect(loop.Status.Iteration).To(BeNumerically(">", 0), "the operator iterated past 0")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing), "the failing verify iterated to Implementing")
		Expect(loop.Status.Checkpoint).To(BeNil(), "the checkpoint create for iter 0 was skipped (support was absent)")
		Expect(loop.Status.Checkpoints).To(BeEmpty())

		// A LATER reconcile (the Loop is at the iterated iteration,
		// Implementing — no verify in progress for it yet, and no verify
		// outcome change this pass). The level-based checkpoint step must
		// create checkpoint-<loop>-iter-0 with Milestone=false (the check
		// failed), and must NOT create iter-(N+1) (N+1's verify has not
		// completed, so keying off the current iteration would be wrong).
		rAvailable := b1NewReconciler(true, false)
		b1Reconcile(rAvailable, ns, name)

		By("creating checkpoint-<loop>-iter-0 (Milestone=false: the verify FAILED)")
		fresh := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, fresh)).To(Succeed())
		Expect(b1SnapshotExists(ctx, ns, "checkpoint-"+name+"-iter-0")).To(BeTrue(), "the later reconcile creates the checkpoint for the FAILED verify at iteration 0")
		Expect(fresh.Status.Checkpoint).NotTo(BeNil())
		Expect(fresh.Status.Checkpoint.Iteration).To(Equal(0))
		Expect(fresh.Status.Checkpoint.Milestone).To(BeFalse(), "Milestone is false: the verify's check-0 failed (a -1/non-zero lastCheckResults is not a pass)")

		By("NOT creating checkpoint-<loop>-iter-1 (iteration 1's verify has not completed)")
		// The operator iterates into a 0-based count (nextIter := max(iteration+1, 2)),
		// so the first retry is iteration 2 — iteration 1 never exists (F90).
		nextIter := fresh.Status.Iteration
		if nextIter > 0 {
			Expect(b1SnapshotExists(ctx, ns, fmt.Sprintf("checkpoint-%s-iter-%d", name, nextIter))).To(BeFalse(),
				"no checkpoint for the iterated-into iteration whose verify has not completed")
		}
		for _, cp := range fresh.Status.Checkpoints {
			Expect(cp.Iteration).To(Equal(0), "the only recorded checkpoint is the iter-0 one")
		}

		// Idempotency: a further reconcile adds nothing (the guard sees the
		// recorded iter-0 entry).
		b1Reconcile(rAvailable, ns, name)
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(volumeSnapshotListGVK)
		Expect(k8sClient.List(ctx, list, client.InNamespace(ns),
			client.MatchingLabels{"coxswain.wattu.com/loop": name})).To(Succeed())
		Expect(list.Items).To(HaveLen(1), "no re-take on a later reconcile")
	})

	It("does NOT create a checkpoint for the current iteration while its verify is in progress (one spec per in-progress state; the named mutation — drop the in-progress guard — must fail against the older commit)", func() {
		for _, inProg := range []string{"trusted-init", "tamper", "artifact", "check-0"} {
			ns := "b1-inprog-" + inProg + "-" + nowSuffix()
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
			}()
			b1CreateVSC()

			name := "inprogloop"
			spec := b1LoopSpec()
			spec.Loop.MaxIterations = 9
			stall := int32(100)
			spec.Loop.StallAfter = &stall
			Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec:       spec,
			})).To(Succeed())

			r := b1NewReconciler(true, false)
			s5aDriveToVerifyingWithReconciler(r, ns, name)

			// The in-progress verify pod: the in-progress state's init
			// container is still Running (not terminated), the rest
			// are terminated (exit 0). The "check-0" state uses
			// s5aVerifyPodInProg (the I49 in-progress stand-in).
			jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
			job := &batchv1.Job{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job); err != nil {
				job = &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns, Labels: verifyJobLabels(name)}}
				Expect(k8sClient.Create(ctx, job)).To(Succeed())
			}
			if inProg == "check-0" {
				s5aVerifyPodInProg(ctx, ns, name, jobName)
			} else {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name + "-verify-pod-inprog",
						Namespace: ns,
						Labels:    map[string]string{s5aJobNameLabel: jobName, verifyForLabel: name},
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion: batchv1.SchemeGroupVersion.String(),
							Kind:       i65JobKind,
							Name:       jobName,
							UID:        job.UID,
						}},
					},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}}},
				}
				Expect(k8sClient.Create(ctx, pod)).To(Succeed())
				inits := []corev1.ContainerStatus{}
				addTerminated := func(n string) {
					inits = append(inits, corev1.ContainerStatus{Name: n, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}})
				}
				addRunning := func(n string) {
					inits = append(inits, corev1.ContainerStatus{Name: n, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})
				}
				switch inProg {
				case "trusted-init":
					addRunning(s5aCloneBase)
					addTerminated(s5aImportAgent)
					addTerminated(s5aTamper)
					addTerminated(s5aArtifact)
				case "tamper":
					addTerminated(s5aCloneBase)
					addTerminated(s5aImportAgent)
					addRunning(s5aTamper)
					addTerminated(s5aArtifact)
				case "artifact":
					addTerminated(s5aCloneBase)
					addTerminated(s5aImportAgent)
					addTerminated(s5aTamper)
					addRunning(s5aArtifact)
				default:
					Fail(fmt.Sprintf("unknown in-progress state %q", inProg))
				}
				pod.Status.InitContainerStatuses = inits
				Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
			}

			// Reconcile: the verify outcome is NOT yet recorded
			// (the in-progress state is pending — the operator
			// requeues, no decision), so no checkpoint must be
			// created for the in-progress iteration.
			_, _ = r.Reconcile(ctx,
				reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
			loop := &coxv1alpha1.Loop{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
			// The object's presence means the operator has read a verify Job's pod at
			// least once; status.verify.iteration names the iteration whose OUTCOME
			// was applied, and is not cleared when a new pin is set (F83d). While
			// this verify is in progress there is no recorded outcome yet.
			if loop.Status.Verify != nil {
				Expect(loop.Status.Verify.Iteration).To(BeNil(), "no recorded verify outcome while the verify is in progress")
				Expect(loop.Status.Checkpoint).To(BeNil())
			}
			Expect(loop.Status.Checkpoint).To(BeNil(), "no checkpoint while the verify is in progress")
			Expect(loop.Status.Checkpoints).To(BeEmpty(), "no checkpoint recorded while the verify is in progress")
		}
	})

	It("retains the last 3 + every milestone and GCs the rest (retention + GC; the named mutation — break retention — must fail)", func() {
		ns := "b1-retain-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		b1CreateVSC()

		name := "retainloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())

		r := b1NewReconciler(true, false)
		// Drive the Loop to Verifying (iteration 0) using the SPEC's
		// reconciler r (so the driver seam and CRD seam are active on every
		// reconcile).
		s5aDriveToVerifyingWithReconciler(r, ns, name)
		// The verify outcome will record status.verify.iteration = 0 (the
		// current iteration). The checkpoint is for iteration 0. The
		// hand-built fixtures are for iterations 1..6 (iterations 1..4 are
		// NOT milestones; iterations 5 and 6 ARE milestones). The keep-set
		// under B1 retention (last 3 = iters 0,5,6 + the milestones 5,6) =
		// {0,5,6}; the GC'd set = {1,2,3,4}.
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		// Seed the status with 6 hand-built checkpoints (iterations 1..4 are
		// NOT milestones — a hand-built fixture that exercises the GC path
		// the operator's writer never produces on its own; iterations 5 and 6
		// ARE milestones).
		for i := 1; i <= 6; i++ {
			at := metav1.Now()
			loop.Status.Checkpoints = append(loop.Status.Checkpoints, coxv1alpha1.CheckpointStatus{
				Iteration: i,
				Name:      fmt.Sprintf("checkpoint-%s-iter-%d", name, i),
				Milestone: i >= 5,
				At:        at,
			})
		}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		// Seed the VolumeSnapshot objects the hand-built entries name.
		b1CreateLoopSnapshots(ctx, loop)
		// The successful verify pod (a stand-in the operator's verify path
		// reads init exit codes from; envtest has no Job controller).
		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		s5aVerifyPod(ctx, ns, name, jobName, 0)
		// Reconcile: the verify outcome is applied (verifyChanged is true),
		// the checkpoint step takes the new checkpoint for iteration 0 and
		// runs the retention GC over {0,1..6}.
		b1Reconcile(r, ns, name)

		By("keeping the last 3 (iters 0,5,6) and the milestones (5,6); GCing 1,2,3,4")
		fresh := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, fresh)).To(Succeed())
		kept := map[int]bool{}
		for _, cp := range fresh.Status.Checkpoints {
			kept[cp.Iteration] = true
		}
		Expect(kept).To(HaveKeyWithValue(5, true), "iter 5 is in the last 3 (and a milestone) — kept")
		Expect(kept).To(HaveKeyWithValue(6, true), "iter 6 is in the last 3 (and a milestone) — kept")
		Expect(kept).To(HaveKeyWithValue(0, true), "iter 0 is the newest checkpoint — kept")
		Expect(kept).NotTo(HaveKey(1), "iter 1 is not a milestone and not in the last 3 — GC'd")
		Expect(kept).NotTo(HaveKey(2), "iter 2 — GC'd")
		Expect(kept).NotTo(HaveKey(3), "iter 3 — GC'd")
		Expect(kept).NotTo(HaveKey(4), "iter 4 — GC'd (the last 3 are 0,5,6)")

		By("deleting the GC'd VolumeSnapshot objects (1,2,3,4) and keeping the kept objects (0,5,6)")
		for _, it := range []int{1, 2, 3, 4} {
			Expect(b1SnapshotExists(ctx, ns, fmt.Sprintf("checkpoint-%s-iter-%d", name, it))).
				To(BeFalse(), "the GC must delete the non-kept snapshot iter-%d", it)
		}
		for _, it := range []int{0, 5, 6} {
			Expect(b1SnapshotExists(ctx, ns, fmt.Sprintf("checkpoint-%s-iter-%d", name, it))).
				To(BeTrue(), "the retention must keep the snapshot iter-%d", it)
		}
	})

	// F47: status.checkpoints is user-writable (the editor role PUTs /status), so
	// the retention GC must not delete a VolumeSnapshot just because a status
	// entry names it.
	It("F47: the retention GC never deletes a snapshot the Loop does not own, and drops the untrusted entries with a Warning Event", func() {
		ns := "f47-gc-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		b1CreateVSC()

		name := "f47gc"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())
		r := b1NewReconciler(true, false)
		rec := record.NewFakeRecorder(64)
		r.Recorder = rec
		s5aDriveToVerifyingWithReconciler(r, ns, name)

		// Snapshots the forged entries point at.
		foreign := b1MakeSnapshotObj("otherloop", ns, 1) // checkpoint-otherloop-iter-1, labelled otherloop
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		oddName := "weird-snapshot" // wrong SHAPE, but labelled for THIS Loop: only the shape test catches it
		odd := b1MakeSnapshotObj(name, ns, 2)
		odd.SetName(oddName)
		Expect(k8sClient.Create(ctx, odd)).To(Succeed())
		unlabelled := b1MakeSnapshotObj(name, ns, 3) // right name, NO Loop label: only the ownership test catches it
		unlabelled.SetLabels(map[string]string{})
		Expect(k8sClient.Create(ctx, unlabelled)).To(Succeed())

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		at := metav1.Now()
		entries := []coxv1alpha1.CheckpointStatus{
			{Iteration: 1, Name: "checkpoint-otherloop-iter-1", At: at},
			{Iteration: 2, Name: oddName, At: at},
			{Iteration: 3, Name: checkpointName(loop, 3), At: at},
			{Iteration: 4, Name: checkpointName(loop, 4), At: at}, // a genuine non-kept entry: still GC'd
			{Iteration: 5, Name: checkpointName(loop, 5), At: at, Milestone: true},
			{Iteration: 6, Name: checkpointName(loop, 6), At: at, Milestone: true},
		}
		loop.Status.Checkpoints = entries
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		b1CreateLoopSnapshots(ctx, &coxv1alpha1.Loop{ObjectMeta: loop.ObjectMeta, Status: coxv1alpha1.LoopStatus{
			Checkpoints: []coxv1alpha1.CheckpointStatus{entries[3], entries[4], entries[5]}}})
		s5aVerifyPod(ctx, ns, name, verifyJobName(loop), 0)
		b1Reconcile(r, ns, name)

		By("leaving every snapshot the Loop does not own untouched")
		Expect(b1SnapshotExists(ctx, ns, "checkpoint-otherloop-iter-1")).To(BeTrue(), "another Loop's snapshot must never be deleted")
		Expect(b1SnapshotExists(ctx, ns, oddName)).To(BeTrue(), "a wrong-shape name must not be deleted (even with this Loop's label)")
		Expect(b1SnapshotExists(ctx, ns, checkpointName(loop, 3))).To(BeTrue(), "a snapshot without this Loop's label must not be deleted")
		By("still GCing a genuine non-kept checkpoint")
		Expect(b1SnapshotExists(ctx, ns, checkpointName(loop, 4))).To(BeFalse())

		By("dropping the untrusted entries from status and warning once each")
		fresh := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, fresh)).To(Succeed())
		names := make([]string, 0, len(fresh.Status.Checkpoints))
		for _, cp := range fresh.Status.Checkpoints {
			names = append(names, cp.Name)
		}
		Expect(names).NotTo(ContainElements("checkpoint-otherloop-iter-1"))
		Expect(names).NotTo(ContainElement(oddName))
		Expect(names).NotTo(ContainElement(checkpointName(loop, 3)))
		Expect(names).NotTo(ContainElement(checkpointName(loop, 4)))
		untrusted := 0
		for _, e := range drainEvents(rec) {
			if strings.Contains(e, "CheckpointEntryUntrusted") {
				untrusted++
			}
		}
		Expect(untrusted).To(Equal(3), "one Warning Event per untrusted entry")
	})

	It("retries the checkpoint on the next reconcile when the first create was skipped (level-based, not edge-triggered; must FAIL against the if-verifyChanged version)", func() {
		ns := "b1-retry-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		b1CreateVSC()

		name := "retryloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())

		// Reconciler with the driver seam set to UNAVAILABLE (the checkpoint
		// create is skipped on the first reconcile — support is absent).
		rUnavailable := b1NewReconciler(false, false) // driver unavailable
		s5aDriveToVerifyingWithReconciler(rUnavailable, ns, name)
		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		s5aVerifyPod(ctx, ns, name, jobName, 0)

		// First reconcile: the verify outcome is applied (verifyChanged is
		// true, phase → Succeeded), but the checkpoint create is skipped
		// (support is absent — the driver seam reports unavailable). The
		// checkpoint is NOT recorded.
		b1Reconcile(rUnavailable, ns, name)
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded), "the verify passed")
		Expect(loop.Status.Checkpoint).To(BeNil(), "the checkpoint was NOT recorded (support was absent)")

		// Second reconcile: the driver seam is now AVAILABLE (support is
		// present). The verify outcome was already applied (verifyChanged is
		// false — the phase is already Succeeded). The checkpoint step must
		// still run (level-based) and create the checkpoint.
		rAvailable := b1NewReconciler(true, false) // driver available
		b1Reconcile(rAvailable, ns, name)
		fresh := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, fresh)).To(Succeed())
		Expect(fresh.Status.Checkpoint).NotTo(BeNil(), "the checkpoint MUST be created on the retry reconcile (level-based, not edge-triggered)")
		Expect(fresh.Status.Checkpoint.Iteration).To(Equal(0), "the checkpoint is named for the PRE-verify iteration (0), not the post-verify iteration")
		Expect(fresh.Status.Checkpoint.Milestone).To(BeTrue(), "the verify passed, so the checkpoint is a milestone")
	})

	It("does NOT create a checkpoint for iteration 0 while its verify is in progress (no recorded outcome yet; keying off the current iteration would wrongly create iter-0 before the verify completes; must FAIL against 0840f87 which keys off the current iteration)", func() {
		ns := "b1-inprog-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		b1CreateVSC()

		name := "inprogloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())

		r := b1NewReconciler(true, false)
		// Drive to Verifying for the FIRST verify (iteration 0). The verify
		// pod is NOT created yet — the verify is in progress (no recorded
		// outcome for iteration 0).
		s5aDriveToVerifyingWithReconciler(r, ns, name)
		// The verify pod is in progress (an init container is still running,
		// not terminated). Create a pod with the trusted inits terminated but
		// check-0 still Running.
		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		s5aVerifyPodInProg(ctx, ns, name, jobName)
		// Reconcile: the verify outcome is NOT yet recorded (the check is
		// still running), so no checkpoint must be created for iteration 0
		// (the recorded outcome is absent; keying off the current iteration
		// would wrongly create iter-0 before the verify completes).
		b1Reconcile(r, ns, name)
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Checkpoint).To(BeNil(), "no checkpoint while the verify is in progress (no recorded outcome)")
		Expect(loop.Status.Checkpoints).To(BeEmpty(), "no checkpoint recorded while the verify is in progress")
	})

	It("adds the checkpoint finalizer and holds a deleting Loop until its snapshots are GC'd (finalizer, CleaningUp)", func() {
		ns := "b1-final-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		b1CreateVSC()

		name := "finloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())

		r := b1NewReconciler(true, false)
		// A reconcile adds the finalizer.
		b1Reconcile(r, ns, name)
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Finalizers).To(ContainElement(loopCheckpointFinalizer),
			"every live Loop carries the checkpoint finalizer")

		// Seed a checkpoint snapshot (the Loop is not yet Succeeded, so the
		// finalizer's GC is the path that removes it on delete).
		snap := b1MakeSnapshotObj(name, ns, 1)
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		// Delete the Loop: the finalizer holds it (the object is not gone).
		Expect(k8sClient.Delete(ctx, loop)).To(Succeed())
		for range 5 {
			got := &coxv1alpha1.Loop{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, got); err == nil {
				Expect(got.DeletionTimestamp).NotTo(BeZero(), "the deleting Loop is held by the finalizer")
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		// The finalizer's GC runs on the next reconcile: it deletes the
		// snapshot and, when none remain, removes the finalizer so the
		// apiserver can delete the object. The reconcile that removes the
		// finalizer DELETES the Loop (the apiserver finalizes it), so a
		// subsequent Get is NotFound — the loop tolerates that (a
		// b1Reconcile that asserts the Get would fail on the very reconcile
		// that deletes the object).
		var gone bool
		for range 20 {
			loopNow := &coxv1alpha1.Loop{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loopNow); apierrors.IsNotFound(err) {
				gone = true
				break
			} else if err != nil {
				Expect(err).ToNot(HaveOccurred())
			}
			if loopNow.DeletionTimestamp.IsZero() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			// Reconcile the deleting Loop (the finalizer's GC path). This
			// reconcile may delete the object, so it is NOT a b1Reconcile
			// (which asserts the post-reconcile Get).
			_, _ = r.Reconcile(context.Background(),
				reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
			time.Sleep(50 * time.Millisecond)
		}
		if !gone {
			gone = apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &coxv1alpha1.Loop{}))
		}
		Expect(gone).To(BeTrue(), "once the snapshots are GC'd the finalizer is removed and the Loop deletes")
		Expect(b1SnapshotExists(ctx, ns, "checkpoint-"+name+"-iter-1")).To(BeFalse(),
			"CleaningUp deletes the Loop's checkpoint snapshots")
	})

	It("deletes a deleting Loop with NO snapshots immediately (finalizer does not block on nothing)", func() {
		ns := "b1-final-none-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		b1CreateVSC()

		name := "nonefinloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())
		r := b1NewReconciler(true, false)
		b1Reconcile(r, ns, name) // adds the finalizer
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		// No snapshots exist: the GC's done check passes on the first
		// reconcile after the delete, and the finalizer is removed.
		Expect(k8sClient.Delete(ctx, loop)).To(Succeed())
		var gone bool
		for range 10 {
			loopNow := &coxv1alpha1.Loop{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loopNow); apierrors.IsNotFound(err) {
				gone = true
				break
			} else if err != nil {
				Expect(err).ToNot(HaveOccurred())
			}
			if loopNow.DeletionTimestamp.IsZero() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			// The GC's done check passes on the first reconcile (no
			// snapshots) and the finalizer is removed, so this reconcile may
			// delete the object — not a b1Reconcile (which asserts the
			// post-reconcile Get).
			_, _ = r.Reconcile(context.Background(),
				reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
			time.Sleep(50 * time.Millisecond)
		}
		if !gone {
			gone = apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &coxv1alpha1.Loop{}))
		}
		Expect(gone).To(BeTrue(), "a deleting Loop with no snapshots deletes (the finalizer is removed on the first reconcile)")
	})

	// F10 (dogfood 3 finding): deleting a NAMESPACE that holds a Loop leaves
	// the Loop, and so the namespace, stuck forever on the checkpoint finalizer.
	// The root cause (measured, operator logs): a deleting Loop's reconcile hit
	// the ensure/cleanup path (create proxy pod ... forbidden: namespace is
	// being terminated) and returned an error to the controller-runtime retry
	// loop, which never reaches the finalizer removal; the error backoff (up to
	// 1000s) requeues, but by then the namespace delete has already timed out or
	// the storm (F11) has monopolized the workqueue. The fix: a deleting Loop's
	// checkpoint GC TOLERATES a terminating-namespace error (Forbidden
	// list/delete — the children go away with the namespace) and removes the
	// finalizer, so the namespace can complete its delete. The finalizer-removal
	// Update is also rejected (Forbidden) on a terminating namespace, so
	// reconcileCleaningUp surfaces it as an Event and requeues with a targeted
	// RequeueAfter (5s, no backoff cap) so only that Loop is retried. These
	// specs drive the real Reconcile with a client whose VolumeSnapshot
	// list/delete return a terminating-namespace Forbidden (simulating the live
	// repro in a live
	// namespace, which envtest cannot do — a namespace delete is async and the
	// apiserver does not reject creates the way a terminating ns does).
	It("a deleting Loop whose checkpoint list returns Forbidden (terminating ns) STILL has its finalizer removed (F10)", func() {
		ns := "f10-forbidden-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		b1CreateVSC()

		name := "f10forb"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())

		// A client whose VolumeSnapshot list AND delete return a terminating-ns
		// Forbidden (the live repro: "unable to create new content in namespace
		// ... because it is being terminated"). Everything else passes through.
		forbClient := &f10TerminatingNSClient{inner: k8sClient}
		r := b1NewReconciler(true, false)
		r.Client = forbClient
		r.apiReader = forbClient

		b1Reconcile(r, ns, name) // adds the finalizer
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		// Seed a snapshot (the GC must tolerate the Forbidden delete of it).
		snap := b1MakeSnapshotObj(name, ns, 1)
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		// Delete the Loop: the finalizer holds it.
		Expect(k8sClient.Delete(ctx, loop)).To(Succeed())
		for range 5 {
			got := &coxv1alpha1.Loop{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, got); err == nil {
				Expect(got.DeletionTimestamp).NotTo(BeZero(), "the deleting Loop is held by the finalizer")
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		// Reconcile the deleting Loop. The checkpoint list returns a
		// terminating-ns Forbidden; the F10 fix treats it as "done" (the
		// children go away with the namespace) and removes the finalizer, so
		// the apiserver deletes the Loop. Without the fix, the list error
		// requeues forever and the finalizer is never removed.
		_, _ = r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		var gone bool
		for range 10 {
			if apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &coxv1alpha1.Loop{})) {
				gone = true
				break
			}
			time.Sleep(50 * time.Millisecond)
			_, _ = r.Reconcile(context.Background(),
				reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		}
		Expect(gone).To(BeTrue(),
			"a deleting Loop whose checkpoint list returns a terminating-ns Forbidden has its finalizer removed (the namespace can complete its delete)")
	})

	// F25: the VolumeSnapshot CRD is absent (hermes), but the support check
	// reads "present" (the production reconciler has no RESTMapper wired), so
	// the GC list fails with a NoMatchError. That is "nothing to GC" (done), not
	// an error: before the fix the finalizer never cleared and the Loop (and its
	// namespace) stayed Terminating.
	It("a deleting Loop whose checkpoint list returns NoMatch (VolumeSnapshot CRD absent) has its finalizer removed (F25)", func() {
		ns := "f25-nomatch-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		b1CreateVSC()

		name := "f25nomatch"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())

		nmClient := &f25NoMatchClient{Client: k8sClient}
		r := b1NewReconciler(true, false)
		r.Client = nmClient
		r.apiReader = nmClient

		b1Reconcile(r, ns, name) // adds the finalizer
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(loop, loopCheckpointFinalizer)).To(BeTrue(), "the finalizer is set before the delete")
		Expect(k8sClient.Delete(ctx, loop)).To(Succeed())

		var gone bool
		for range 10 {
			_, _ = r.Reconcile(context.Background(),
				reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
			if apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &coxv1alpha1.Loop{})) {
				gone = true
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		Expect(gone).To(BeTrue(), "a NoMatch on the checkpoint list is 'nothing to GC': the finalizer is removed and the Loop is deleted")
	})

	// F10 (second spec): a deleting Loop with CheckpointsAvailable=False (no
	// VolumeSnapshot CRD — the support check is False) still has its finalizer
	// removed. The no-CRD branch of gcLoopCheckpoints returns done immediately
	// (there are no snapshots to delete), so the finalizer is removed on the
	// first reconcile of the deleting Loop. This is the dogfood4-df4 shape
	// (CheckpointsAvailable=False, no VolumeSnapshotClass for the PVC's CSI
	// driver): the finalizer must not block on a support check that was False
	// from the start.
	It("a deleting Loop with CheckpointsAvailable=False (no snapshot CRD) STILL has its finalizer removed (F10)", func() {
		ns := "f10-nocrd-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		// No VSC created (the CRD is "absent" — snapshotCRDPresent is false).
		name := "f10nocrd"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())

		r := b1NewReconciler(false, true) // no CRD: snapshotCRDPresent false
		b1Reconcile(r, ns, name)          // adds the finalizer
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(k8sClient.Delete(ctx, loop)).To(Succeed())
		for range 5 {
			got := &coxv1alpha1.Loop{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, got); err == nil {
				Expect(got.DeletionTimestamp).NotTo(BeZero(), "the deleting Loop is held by the finalizer")
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		// Reconcile the deleting Loop. The no-CRD branch of gcLoopCheckpoints
		// returns done (no snapshots), so the finalizer is removed and the
		// Loop deletes. Without this, the finalizer would block forever on a
		// support check that was False from the start (the dogfood4-df4 shape).
		_, _ = r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		var gone bool
		for range 10 {
			if apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &coxv1alpha1.Loop{})) {
				gone = true
				break
			}
			time.Sleep(50 * time.Millisecond)
			_, _ = r.Reconcile(context.Background(),
				reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		}
		Expect(gone).To(BeTrue(),
			"a deleting Loop with CheckpointsAvailable=False (no snapshot CRD) has its finalizer removed (the namespace can complete its delete)")
	})

	// F10 (targeted RequeueAfter): a deleting Loop whose finalizer-removal
	// Update is rejected (a terminating namespace rejects the Update with
	// Forbidden) surfaces it as an Event (FinalizerRemovalBlocked) and
	// requeues with a targeted RequeueAfter (5s, NOT the error backoff cap).
	// This spec uses a client whose Loop Update returns a terminating-ns
	// Forbidden (in addition to the VolumeSnapshot list/delete Forbidden) and
	// asserts: (1) the Event is emitted, (2) the Reconcile returns
	// RequeueAfter: 5s (not an error), (3) the finalizer is NOT removed (the
	// Update failed, so the finalizer stays — the requeue will retry when the
	// namespace is gone).
	It("a deleting Loop whose finalizer-removal Update is Forbidden (terminating ns) requeues with a targeted RequeueAfter and surfaces an Event (F10)", func() {
		ns := "f10-finalizer-forb-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		b1CreateVSC()

		name := "f10finalforb"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())

		// A client whose VolumeSnapshot list/delete AND the Loop Update return
		// a terminating-ns Forbidden (the full live F10 repro: a namespace being
		// terminated rejects creates, reads, AND updates).
		forbClient := &f10TerminatingNSUpdateClient{inner: k8sClient}
		r := b1NewReconciler(true, false)
		r.Client = forbClient
		r.apiReader = forbClient

		// A fake recorder to capture the Event.
		recorder := record.NewFakeRecorder(64)
		r.Recorder = recorder

		b1Reconcile(r, ns, name) // adds the finalizer
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())

		// Delete the Loop: the finalizer holds it.
		Expect(k8sClient.Delete(ctx, loop)).To(Succeed())
		for range 5 {
			got := &coxv1alpha1.Loop{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, got); err == nil {
				Expect(got.DeletionTimestamp).NotTo(BeZero(), "the deleting Loop is held by the finalizer")
				break
			}
			time.Sleep(50 * time.Millisecond)
		}

		// Reconcile the deleting Loop. The checkpoint list returns a
		// terminating-ns Forbidden (treated as "done"), the finalizer-removal
		// Update ALSO returns a terminating-ns Forbidden (the F10 fix surfaces
		// it as an Event and requeues with a targeted RequeueAfter).
		res, err := r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).ToNot(HaveOccurred(), "the Reconcile must NOT return an error for a terminating-ns Forbidden (it is not a real failure)")
		Expect(res.RequeueAfter).To(Equal(5*time.Second), "the Reconcile must requeue with a targeted 5s RequeueAfter (not the error backoff cap)")

		// The Event must be emitted (FinalizerRemovalBlocked).
		//
		// Drain the fake recorder's BOUNDED buffer, not `for range` over the
		// channel: `for range` over a Go channel keeps looping while the channel
		// is OPEN, and FakeRecorder closes Events only when its buffer drains
		// and the recorder is stopped. This spec never stops the recorder, so
		// the range loop hangs at `<-recorder.Events` on the first unbuffered
		// send (P5a: the one-shot MemoryPVCUnbound Event queued ahead of the
		// FinalizerRemovalBlocked Event made the hang deterministic; before P5a
		// the single FinalizerRemovalBlocked Event filled the last slot and the
		// break on it exited the loop first, which is why the spec passed for
		// a year). `for len(recorder.Events) > 0` drains exactly the queued
		// events and stops.
		var sawFinalizerRemovalBlocked bool
		for len(recorder.Events) > 0 {
			event := <-recorder.Events
			if strings.Contains(event, "FinalizerRemovalBlocked") {
				sawFinalizerRemovalBlocked = true
				break
			}
		}
		Expect(sawFinalizerRemovalBlocked).To(BeTrue(), "the finalizer-removal Forbidden must be surfaced as a FinalizerRemovalBlocked Event")

		// The finalizer must NOT be removed (the Update failed, so the
		// finalizer stays — the requeue will retry when the namespace is gone).
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, got)).To(Succeed())
		hasFinalizer := slices.Contains(got.Finalizers, loopCheckpointFinalizer)
		Expect(hasFinalizer).To(BeTrue(), "the finalizer must NOT be removed when the Update is Forbidden (the requeue will retry)")
	})

	// F10b (the dogfood4-df4 hang): a deleting Loop whose referenced
	// AgentPolicy is GONE (deleted from its terminating namespace — the
	// namespaced object is gone by the time the operator reconciles the
	// deleting Loop) still has its checkpoint finalizer removed. Before F10b
	// the deleting path ran the LIVE validateStep (C6a), which is a HARD gate:
	// a missing referenced AgentPolicy sets PolicyValid=False/PolicyNotFound
	// and requeues with RequeueAfter:10s. The validation re-checks the spec on
	// every pass, the spec never changes, so the deleting path requeued
	// forever and the finalizer was never removed — the dogfood4-df4 repro
	// (finalizer stuck 8+ hours, namespace Terminating; the Loop's status
	// showed PolicyValid=False/PolicyNotFound and a single CNIUnenforced Event
	// was the only trace of a reconcile). The F10b fix observes the
	// validation (the same PolicyValid condition is recorded) but does not
	// HOLD the deleting pass, so the CleaningUp work (GC + finalizer removal)
	// always runs. This spec drives the REAL Reconcile with a missing
	// AgentPolicy (no client wrapper — the envtest apiserver returns the
	// NotFound) and asserts the finalizer is removed. MUST FAIL without the
	// fix (the exact-revert mutation: restore the validateStep gate — the
	// deleting path returns the 10s requeue and the finalizer stays).
	It("a deleting Loop whose referenced AgentPolicy is GONE still has its finalizer removed (F10b: the validation must not hold the deleting path)", func() {
		ns := "f10b-missing-policy-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "f10bmiss"
		// The Loop references an AgentPolicy that is NEVER created (it was
		// deleted from the terminating namespace in the live repro).
		loopSpec := b1LoopSpec()
		loopSpec.PolicyRefs = []string{"gone-policy"}
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       loopSpec,
		})).To(Succeed())

		r := b1NewReconciler(false, false)
		b1Reconcile(r, ns, name) // the live reconcile: adds the finalizer
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Finalizers).To(ContainElement(loopCheckpointFinalizer), "the live reconcile must have added the checkpoint finalizer")

		// Delete the Loop: the finalizer holds it.
		Expect(k8sClient.Delete(ctx, loop)).To(Succeed())
		for range 5 {
			got := &coxv1alpha1.Loop{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, got); err == nil {
				Expect(got.DeletionTimestamp).NotTo(BeZero(), "the deleting Loop is held by the finalizer")
				break
			}
			time.Sleep(50 * time.Millisecond)
		}

		// Reconcile the deleting Loop. The referenced AgentPolicy is gone
		// (NotFound); before F10b this held the pass (10s requeue, no
		// CleaningUp). F10b observes the validation (PolicyValid=False/
		// PolicyNotFound is recorded on the Loop) and proceeds to the CleaningUp
		// work: the checkpoint GC (no snapshots — the support seam is off) and
		// the finalizer removal. The Loop deletes. The reconcile must NOT return
		// an error and must NOT return the validation's 10s RequeueAfter (the
		// held-pass shape): the F10b deleting pass either completes (the Loop
		// deletes) or requeues on the finalizer's own 5s work.
		res, err := r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).ToNot(HaveOccurred(), "the deleting pass must not return an error for a missing referenced AgentPolicy (it is observed, not held — F10b)")
		Expect(res.RequeueAfter).NotTo(Equal(10*time.Second),
			"the deleting pass must not return the validateStep's 10s requeue (the held-pass shape — the dogfood4-df4 hang)")

		// The finalizer must be removed and the Loop deleted (the dogfood4-df4
		// assertion: without F10b this never happens — the validateStep gate
		// requeued forever and the Loop held the namespace in Terminating).
		// Without the F10b fix this spec FAILS: the reconcile returns the 10s
		// requeue, the finalizer stays, the Loop is never deleted, and the
		// RequeueAfter assertion above fails in the same pass.
		var gone bool
		for range 10 {
			if apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &coxv1alpha1.Loop{})) {
				gone = true
				break
			}
			time.Sleep(50 * time.Millisecond)
			// Re-run the deleting reconcile: the finalizer's requeue (when the
			// pass's own writes race with the object's delete) is what a live
			// controller-runtime manager does on every 5s RequeueAfter. The F10b
			// pass is level-based: every pass observes the (unchanged) missing
			// policy and retries the finalizer removal.
			_, _ = r.Reconcile(context.Background(),
				reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		}
		Expect(gone).To(BeTrue(),
			"a deleting Loop whose referenced AgentPolicy is gone has its finalizer removed and is deleted (the validation must not hold the deleting path — F10b, the dogfood4-df4 hang)")
	})

	It("BestEffort without the VolumeSnapshot CRD: CheckpointsAvailable=False (VolumeSnapshotCRDMissing) and the Loop still Succeeds (carries on, I90 visible fallback)", func() {
		ns := "b1-nocrd-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		// No VSC created (the CRD is "absent" — the seam overrides
		// snapshotCRDPresent to false; the support check's CRD branch wins
		// before any VSC list).

		name := "nocrdloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(), // spec.checkpoints unset -> BestEffort (default)
		})).To(Succeed())

		r := b1NewReconciler(false, true) // withoutCRD
		b1DriveToSucceeded(ctx, r, ns, name)
		b1Reconcile(r, ns, name) // the reconcile that records the successful verify + takes the checkpoint

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		By("the Loop Succeeds (BestEffort carries on with the A2 PVC-resume)")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"BestEffort without the CRD must NOT fail the Loop (it carries on)")
		By("no checkpoint snapshot is created (the support check is False)")
		Expect(b1SnapshotExists(ctx, ns, "checkpoint-"+name+"-iter-1")).To(BeFalse(),
			"no checkpoint is taken when the support check is False")
		By("CheckpointsAvailable=False, reason VolumeSnapshotCRDMissing (the named reason, I90)")
		c := b1CheckpointCondition(loop)
		Expect(c).NotTo(BeNil(), "the CheckpointsAvailable condition must be set (a VISIBLE fallback, I90)")
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(CheckpointReasonCRDMissing))
	})

	It("Required without the VolumeSnapshot CRD: terminal Failed:CheckpointUnavailable at start (the named mutation — drop the Required check — must fail)", func() {
		ns := "b1-required-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "reqloop"
		spec := b1LoopSpec()
		spec.Checkpoints = coxv1alpha1.CheckpointPolicyRequired
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       spec,
		})).To(Succeed())

		r := b1NewReconciler(false, true) // withoutCRD
		// A single reconcile at start must fail the Loop (terminal).
		loop := b1Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"Required without the CRD must fail the Loop at start (D59)")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseFailed))
		found := false
		for _, c := range loop.Status.Conditions {
			if c.Type == string(coxv1alpha1.LoopPhaseFailed) && c.Reason == checkpointUnavailableReason {
				found = true
			}
		}
		Expect(found).To(BeTrue(), "the Failed condition carries the CheckpointUnavailable reason (terminal)")
		// The Loop never ran (no sandbox pod work, no verify, no checkpoint).
		Expect(loop.Status.Iteration).To(BeZero(), "the Required gate fires at start — the Loop never reaches a verify")
	})

	It("BestEffort with a known driver but no matching VolumeSnapshotClass: CheckpointsAvailable=False (NoSnapshotClassForDriver) and the Loop still Succeeds", func() {
		ns := "b1-novsc-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		// A VSC exists but for a DIFFERENT driver (the support check's
		// list-and-match finds no match for b1Driver -> NoSnapshotClassForDriver).
		// The v1 VSC schema is FLAT (driver at the top level, not under spec).
		otherVSC := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion":     snapshotGV,
			"kind":           volumeSnapshotClassKind,
			"metadata":       map[string]any{"name": "b1-other-vsc"},
			"driver":         "other.csi.k8s.io",
			"deletionPolicy": "Delete",
		}}
		Expect(k8sClient.Create(ctx, otherVSC)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), otherVSC) }()

		name := "novscloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b1LoopSpec(),
		})).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		r.workspaceDriverSeam = func(context.Context, *coxv1alpha1.Loop) (string, bool) { return "unmatched.csi.k8s.io", true } // known driver, no matching VSC
		b1DriveToSucceeded(ctx, r, ns, name)
		b1Reconcile(r, ns, name) // the reconcile that records the successful verify + takes the checkpoint

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"BestEffort with no matching VSC must NOT fail the Loop (it carries on)")
		Expect(b1SnapshotExists(ctx, ns, "checkpoint-"+name+"-iter-1")).To(BeFalse(),
			"no checkpoint is taken when the driver has no matching VolumeSnapshotClass")
		c := b1CheckpointCondition(loop)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(CheckpointReasonNoSnapshotClass))
	})
})
