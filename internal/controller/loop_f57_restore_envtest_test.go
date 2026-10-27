/*
Copyright 2026.

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
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// f57RestoreClient injects the two failures F57 is about into the restore's
// PVC step, and counts the writes it makes to a VolumeSnapshot (F57b's churn
// assertion). It delegates everything else to the real (envtest) client, so
// the rest of the reconcile behaves normally.
type f57RestoreClient struct {
	client.Client
	// alreadyExists makes the restored-PVC Create report AlreadyExists
	// instead of delegating (the PVC is then NOT there to be read).
	alreadyExists bool
	// raceCreate models the concurrent pass the AlreadyExists branch exists
	// for: the Create really creates the PVC, then reports AlreadyExists
	// because a pass that ran just before it did.
	raceCreate bool
	// failCreate makes the restored-PVC Create fail with a plain error
	// (quota, an admission webhook, a missing class).
	failCreate bool
	// confirmErr is returned by the Get that CONFIRMS an AlreadyExists create
	// (only after a create was attempted, so the step's first PVC read — the
	// "is the PVC still there" probe — still reaches the real client).
	confirmErr  error
	createTried bool
	snapPatches int
}

func (c *f57RestoreClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if pvc, ok := obj.(*corev1.PersistentVolumeClaim); ok && pvc.Spec.DataSource != nil {
		c.createTried = true
		if c.raceCreate {
			if err := c.Client.Create(ctx, obj, opts...); err != nil {
				return err
			}
			return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "persistentvolumeclaims"}, pvc.Name)
		}
		if c.alreadyExists {
			return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "persistentvolumeclaims"}, pvc.Name)
		}
		if c.failCreate {
			return errors.New("injected restored-PVC create failure")
		}
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *f57RestoreClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.PersistentVolumeClaim); ok && c.createTried && c.confirmErr != nil {
		return c.confirmErr
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *f57RestoreClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if isSnapshot(obj) {
		c.snapPatches++
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

var _ = Describe("F57: the restore's unconfirmed PVC, and the fork label's write churn", func() {
	ctx := context.Background()

	BeforeEach(func() {
		// The fork fixture drives the source Loop to a checkpoint, which needs
		// the workspace VolumeSnapshotClass (cluster-scoped, idempotent).
		b1CreateVSC()
	})

	// f57Fork creates a source Loop with a ready checkpoint and a fork of it,
	// with the fork's own workspace PVC absent (so the restore's create step
	// runs). It returns the namespace, the fork name and the source snapshot.
	f57Fork := func() (ns, forkName, srcSnap string) {
		ns = "f57-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		srcName := "src"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: ns}, Spec: b2LoopSpec()})).To(Succeed())
		r, _ := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, srcName)
		b1DriveToSucceeded(ctx, r, ns, srcName)
		srcSnap = checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, srcSnap)
		forkName = "fork"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: forkName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal: loopGoal, Workspace: testWorkspace(),
				Verify: coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
				ForkOf: &coxv1alpha1.ForkOf{Loop: srcName, Iteration: 0},
			},
		})).To(Succeed())
		b2EnsureSandbox(ns, forkName)
		return ns, forkName, srcSnap
	}

	// f57Step runs the restore step once, directly (so a failure injected into
	// one step cannot be confused with a later step's error).
	f57Step := func(r *LoopReconciler, ns, name string) (bool, bool, bool, *coxv1alpha1.Loop, error) {
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		changed, requeue, hold, err := r.b2ApplyRestoreStep(ctx, loop)
		// The status write the step may have made is only visible on a re-read.
		fresh := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, fresh)).To(Succeed())
		return changed, requeue, hold, fresh, err
	}

	DescribeTable("F57a: an AlreadyExists create that cannot be confirmed is NOT a restored PVC",
		func(confirmErr error) {
			ns, forkName, _ := f57Fork()
			r, _ := b2NewReconcilerWithRecorder()
			r.Client = &f57RestoreClient{Client: k8sClient, alreadyExists: true, confirmErr: confirmErr}

			changed, requeue, _, fresh, err := f57Step(r, ns, forkName)

			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeFalse(), "an unconfirmed create is no restore decision (I49)")
			Expect(requeue).To(BeTrue(), "the step must requeue and read again")
			c := b2RestoreCondition(fresh)
			Expect(c).To(BeNil(), "no Restore condition may be recorded for a PVC that was never seen; before F57 this pass recorded Restored")
			_, pvcErr := b2GetPVC(ctx, ns, forkName)
			Expect(apierrors.IsNotFound(pvcErr)).To(BeTrue(), "nothing created: the injected AlreadyExists means the PVC is not there")
		},
		Entry("the confirming read fails (a transient API error)", errors.New("injected transient read failure")),
		Entry("the PVC is gone again by the time it is read (NotFound)",
			apierrors.NewNotFound(schema.GroupResource{Resource: "persistentvolumeclaims"}, "ws-fork")),
	)

	It("F57a positive control: a confirmed AlreadyExists IS a restored PVC", func() {
		ns, forkName, _ := f57Fork()
		// The race the branch exists for: a concurrent pass created the PVC
		// microseconds before this one, whose Create therefore reports
		// AlreadyExists. The confirming read finds it, so it is a restore.
		r, _ := b2NewReconcilerWithRecorder()
		r.Client = &f57RestoreClient{Client: k8sClient, raceCreate: true}

		changed, requeue, _, fresh, err := f57Step(r, ns, forkName)

		Expect(err).NotTo(HaveOccurred())
		Expect(changed).To(BeTrue())
		Expect(requeue).To(BeFalse())
		c := b2RestoreCondition(fresh)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))
	})

	It("F57b: a create that keeps failing does not re-patch the source snapshot or re-emit the Warning on every pass", func() {
		ns, forkName, srcSnap := f57Fork()
		c := &f57RestoreClient{Client: k8sClient, failCreate: true}
		r, rec := b2NewReconcilerWithRecorder()
		r.Client = c

		// Pass 1: the label is written once, the create fails, the Warning is
		// emitted, and the label STAYS (the fork still needs the snapshot).
		b2Reconcile(r, ns, forkName)
		Expect(c.snapPatches).To(Equal(1), "pass 1 writes the fork-retention label once")
		Expect(b2RestoreCondition(b2ReconcileGet(ns, forkName)).Reason).To(Equal(coxv1alpha1.RestoreReasonRejected))
		warnings := f57DrainRejectedEvents(rec)
		Expect(warnings).To(Equal(1), "the first failing pass emits the Warning (the transition into Rejected)")

		snap, err := b2GetSnapshot(ctx, ns, srcSnap)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.GetLabels()).To(HaveKeyWithValue(forkSnapshotLabelKey, forkName),
			"the label stays while the create is retried: the fork still needs the source snapshot alive")
		rv := snap.GetResourceVersion()

		// Pass 2: same failure. No second patch, no second Warning.
		b2Reconcile(r, ns, forkName)
		Expect(c.snapPatches).To(Equal(1), "a second failing pass must not write the source snapshot again (it used to patch twice per pass)")
		Expect(f57DrainRejectedEvents(rec)).To(Equal(0),
			"the condition is the steady state: the Warning marks the transition only")
		Expect(b2RestoreCondition(b2ReconcileGet(ns, forkName)).Reason).To(Equal(coxv1alpha1.RestoreReasonRejected),
			"the condition still reports the failed restore")
		snap, err = b2GetSnapshot(ctx, ns, srcSnap)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.GetResourceVersion()).To(Equal(rv), "no write reached the snapshot on the second pass")

		// Pass 3: the create succeeds (the real client) — the restore
		// completes and the next pass removes the label.
		ok, _ := b2NewReconcilerWithRecorder()
		b2Reconcile(ok, ns, forkName)
		Expect(b2RestoreCondition(b2ReconcileGet(ns, forkName)).Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))
		b2Reconcile(ok, ns, forkName)
		snap, err = b2GetSnapshot(ctx, ns, srcSnap)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.GetLabels()).NotTo(HaveKey(forkSnapshotLabelKey),
			"a completed fork releases the label (the pre-F57 completion path)")
	})

	It("F57b: a ROLLBACK create that keeps failing also emits the Warning on the transition only", func() {
		// The rollback branch (spec.restoreFrom, the PVC already gone) had its
		// own copy of the create-failure handling, which still set the
		// condition and emitted the Warning unconditionally: the same
		// per-pass Event storm the fork/lost-PVC branch had.
		ns := "f57-rb-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		name := "rb"
		spec := b2LoopSpec()
		spec.RestoreFrom = "iter-0"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec})).To(Succeed())
		snapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		Expect(k8sClient.Create(ctx, b1MakeSnapshotObj(name, ns, 0))).To(Succeed())
		b2MakeSnapshotReady(ctx, ns, snapName)

		r, rec := b2NewReconcilerWithRecorder()
		r.Client = &f57RestoreClient{Client: k8sClient, failCreate: true}

		_, requeue, _, err := r.b2ApplyRestoreStep(ctx, b2ReconcileGet(ns, name))
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeTrue())
		c := b2RestoreCondition(b2ReconcileGet(ns, name))
		Expect(c).NotTo(BeNil())
		Expect(c.Reason).To(Equal(coxv1alpha1.RestoreReasonRejected))
		Expect(f57DrainRejectedEvents(rec)).To(Equal(1),
			"the first failing rollback pass emits the Warning")

		_, requeue, _, err = r.b2ApplyRestoreStep(ctx, b2ReconcileGet(ns, name))
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeTrue())
		Expect(f57DrainRejectedEvents(rec)).To(Equal(0),
			"a second failing rollback pass must not re-emit the Warning (the condition is the steady state)")
	})

	It("F57a: a ROLLBACK AlreadyExists create that cannot be confirmed is NOT a restored PVC", func() {
		// The rollback branch (spec.restoreFrom, the PVC already gone) carries
		// its OWN copy of the AlreadyExists handling. An AlreadyExists create
		// whose confirming read fails is no evidence the PVC carries the
		// snapshot's content (F57a), so this pass makes no restore decision at
		// all: without this arm the error fell through to the Rejected branch
		// and a create nobody had confirmed was recorded as a failed restore.
		// A separate It rather than a table entry: the F57a table above is the
		// fork/lost-PVC shape (it drives b2ApplyRestoreStep through a fork), and
		// this branch is a different call site with its own copy.
		ns := "f57-rb-u-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		name := "rb"
		spec := b2LoopSpec()
		spec.RestoreFrom = "iter-0"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec})).To(Succeed())
		snapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		Expect(k8sClient.Create(ctx, b1MakeSnapshotObj(name, ns, 0))).To(Succeed())
		b2MakeSnapshotReady(ctx, ns, snapName)

		r, rec := b2NewReconcilerWithRecorder()
		r.Client = &f57RestoreClient{
			Client:        k8sClient,
			alreadyExists: true,
			confirmErr:    errors.New("injected transient read failure"),
		}

		changed, requeue, _, err := r.b2ApplyRestoreStep(ctx, b2ReconcileGet(ns, name))
		Expect(err).NotTo(HaveOccurred())
		Expect(changed).To(BeFalse(), "an unconfirmed ROLLBACK create is no restore decision (I49)")
		Expect(requeue).To(BeTrue(), "the step must requeue and read again")

		fresh := b2ReconcileGet(ns, name)
		c := b2RestoreCondition(fresh)
		Expect(c).To(BeNil(),
			"no Restore condition may be recorded for a PVC that was never seen; before F57a this arm recorded Rejected")
		Expect(f57DrainRejectedEvents(rec)).To(Equal(0), "and no Warning: there was no observed failure to announce")
		_, pvcErr := b2GetPVC(ctx, ns, name)
		Expect(apierrors.IsNotFound(pvcErr)).To(BeTrue(),
			"nothing created: the injected AlreadyExists means the PVC is not there")
	})

	It("F57b: a fork that gives up (the Loop is deleted) releases the label", func() {
		ns, forkName, srcSnap := f57Fork()
		r, _ := b2NewReconcilerWithRecorder()
		r.Client = &f57RestoreClient{Client: k8sClient, failCreate: true}
		b2Reconcile(r, ns, forkName) // the create fails and the label stays
		snap, err := b2GetSnapshot(ctx, ns, srcSnap)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.GetLabels()).To(HaveKey(forkSnapshotLabelKey))

		// The deletion guard runs before any other step; a Loop carrying a
		// deletionTimestamp is what the API server hands the reconciler once
		// the user deletes it, so the spec passes that shape (no API write of
		// the timestamp is possible).
		gone := b2ReconcileGet(ns, forkName).DeepCopy()
		now := metav1.Now()
		gone.DeletionTimestamp = &now
		clean, _ := b2NewReconcilerWithRecorder()
		changed, requeue, hold, err := clean.b2ApplyRestoreStep(ctx, gone)
		Expect(err).NotTo(HaveOccurred())
		Expect([]bool{changed, requeue, hold}).To(Equal([]bool{false, false, false}), "the deletion guard decides nothing")

		snap, err = b2GetSnapshot(ctx, ns, srcSnap)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.GetLabels()).NotTo(HaveKey(forkSnapshotLabelKey),
			"the label must not outlive the fork: the source's GC would skip the snapshot for ever")

		// And a SECOND deleting pass writes nothing: the deletion guard runs on
		// every pass while the Loop's finalizers are still being removed, and
		// removeForkRetentionLabel must not re-patch a snapshot whose label is
		// already gone (a read-only no-op, the same way the mark skips an
		// already-set label).
		rv := snap.GetResourceVersion()
		counter, _ := b2NewReconcilerWithRecorder()
		counting := &f57RestoreClient{Client: k8sClient}
		counter.Client = counting
		_, _, _, err = counter.b2ApplyRestoreStep(ctx, gone)
		Expect(err).NotTo(HaveOccurred())
		Expect(counting.snapPatches).To(Equal(0), "the second deleting pass must not write the snapshot again")
		snap, err = b2GetSnapshot(ctx, ns, srcSnap)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.GetResourceVersion()).To(Equal(rv), "no write reached the snapshot")
	})
})

// b2ReconcileGet reads the Loop the way the reconciler sees it (a fresh copy).
func b2ReconcileGet(ns, name string) *coxv1alpha1.Loop {
	loop := &coxv1alpha1.Loop{}
	ExpectWithOffset(1, k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	return loop
}

// f57DrainRejectedEvents counts the Restore=False/Rejected Events in the
// recorder's buffer (the FakeRecorder's channel is drained, so a later pass
// starts from empty).
func f57DrainRejectedEvents(rec *record.FakeRecorder) int {
	n := 0
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, "reason="+coxv1alpha1.RestoreReasonRejected) {
				n++
			}
		default:
			return n
		}
	}
}
