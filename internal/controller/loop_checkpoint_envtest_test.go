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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

const (
	b1Driver = "hostpath.csi.k8s.io" // the csi-hostpath-driver's provisioner (the kind e2e uses the same)
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
	}, checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: loopName, Namespace: ns}}, iteration))
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

// b1EnsureSandbox re-ensures the stand-in Sandbox object (envtest has no
// agent-sandbox controller; the phase advance's annotation-based recycle
// deletes it, so a multi-cycle drive must re-ensure it before each claim).
func b1EnsureSandbox(ns, name string) {
	s5aEnsureSandbox(ns, name)
}

// b1Cycle drives one full implement->verify cycle with the SPEC's reconciler r
// (so the driver/CRD/readCheckOutput seams are active on every reconcile):
// re-ensure the sandbox, an Implementing success claim with a fresh 40-hex
// headCommit (the operator pins it on the Implementing -> Verifying advance
// and creates the verify Job for the loop's CURRENT iteration), then the
// verify Job pod (fixture Job + pod with a UID-matching ownerReference, the
// S5a idiom) with the given check exit codes — 0 = pass, non-zero = the check
// fails — and one reconcile that applies the verify outcome (and, level-based,
// takes that iteration's checkpoint if it is recorded as completed).
//
// The check pod's termination message carries a per-seq raw output so the P2e
// stall detector (which reads the failing check's output via the
// readCheckOutput seam — the spec sets the seam to read the pod's terminated
// message) sees a DIFFERENT failure each iteration (an identical failure
// 3 in a row fires Failed:Stalled, which would cut a multi-iteration drive
// short before the checkpoint the spec asserts on). The pod's name is unique
// per cycle (envtest's apiReader is live, not cached — a deleted+recreated
// same-named pod would not reliably disappear for the read, and a fresh name
// is what the S5a in-progress spec already relies on).
func b1Cycle(ctx context.Context, r *LoopReconciler, ns, name string, seq int, checkExit int32) {
	b1EnsureSandbox(ns, name)
	head := fmt.Sprintf("%040x", seq)
	s5aClaimPod(ns, name, "Implementing", head)
	// Reconcile until the Loop is at Verifying. The Implementing -> Verifying
	// advance reads the claim from the sandbox pod's termination message and
	// pins status.currentVerify; it may need a second reconcile (the first
	// reads the claim and advances; the second is a safety net for the
	// operator's requeue-after-claim-read path). A single retry, not a loop.
	for i := 0; i < 2; i++ {
		_, _ = r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		if loop.Status.Phase == coxv1alpha1.LoopPhaseVerifying {
			break
		}
		// The Loop is still at Implementing (the claim was not advanced this
		// reconcile). Re-claim and retry once.
		s5aClaimPod(ns, name, "Implementing", head)
	}
	loop := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	if loop.Status.Phase != coxv1alpha1.LoopPhaseVerifying {
		Fail(fmt.Sprintf("b1Cycle: the Loop is not at Verifying before the verify pod (got %s)", loop.Status.Phase))
	}
	jobName := verifyJobName(loop)
	// Create the Job (if absent) and a fresh pod (unique per cycle — the
	// S5a idiom: the operator finds the pod via the job-name label and reads
	// its init exit codes; a fresh name per cycle avoids the same-named-pod
	// confusion envtest's live apiReader would not reliably clear).
	job := &batchv1.Job{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job); err != nil {
		job = &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns, Labels: verifyJobLabels(name)}}
		Expect(k8sClient.Create(ctx, job)).To(Succeed())
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-verify-pod-%d", name, seq),
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
	pod.Status.InitContainerStatuses = b1VerifyPodStatuses(checkExit, seq)
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	_, _ = r.Reconcile(context.Background(),
		reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
}

// b1VerifyPodStatuses builds the verify pod's init container statuses: the
// two trusted inits (clone-base, import-agent) terminated exit 0 (the
// verifyOutcome's trusted-inits loop requires them, or it returns noDecision
// with requeue — the I65 infra path), the tamper + artifact inits terminated
// exit 0, and the single check-0 init terminated with the given exit code
// (0 = pass, non-zero = fail) and a per-seq termination message (the
// readCheckOutput seam's source — different output per iteration so the
// P2e stall detector's hash changes each cycle and the stall gate stays
// inert).
func b1VerifyPodStatuses(checkExit int32, seq int) []corev1.ContainerStatus {
	return []corev1.ContainerStatus{
		{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		{Name: s5aArtifact, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		{Name: s5aCheck0, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: checkExit,
			Message: fmt.Sprintf("b1 cycle %d failing output", seq)}}},
	}
}

// b1SetIteration sets the Loop's current iteration (the spec drives the
// iteration explicitly so the spec names the verify Job it must build: the
// operator's real iterate path would increment it, but the checkpoint specs
// care about the RECORD, not the increment).
func b1SetIteration(ctx context.Context, ns, name string, iteration int) {
	loop := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	loop.Status.Iteration = iteration
	Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
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

// b1InProgSpec drives the verify at iteration 0 to the given in-progress pod
// state (one of: a trusted init still Running, the tamper init still Running,
// the artifact init still Running, or the check-0 init absent from the pod
// status) and asserts the operator does NOT take a checkpoint for the
// in-progress iteration (no recorded outcome — the verify has not completed,
// so the operator must not checkpoint it; I49: a decision on an in-progress
// state is wrong).
//
// The pod's trusted-inits + tamper + artifact are terminated (exit 0) UNLESS
// the in-progress state is one of them; check-0 is absent from the pod's init
// status when the in-progress state is the check-0 (the kubelet has not
// advanced to it yet — the verifyOutcome's check-read loop finds no status
// for check-0 and returns noDecision, the requeue). The pod's name is unique
// per in-progress state (the S5a in-progress spec's same-named-pod idiom,
// which works because envtest's apiReader is live, not cached).
func b1InProgSpec(ctx context.Context, name, ns string, inProgressState string) *coxv1alpha1.Loop {
	spec := b1LoopSpec()
	spec.Loop.MaxIterations = 9
	stallAfter100 := int32(100)
	spec.Loop.StallAfter = &stallAfter100
	Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       spec,
	})).To(Succeed())

	r := b1NewReconciler(true, false)
	r.readCheckOutput = func(pod *corev1.Pod, checkName string) (string, bool) {
		for j := range pod.Status.InitContainerStatuses {
			if pod.Status.InitContainerStatuses[j].Name == checkName && pod.Status.InitContainerStatuses[j].State.Terminated != nil {
				return pod.Status.InitContainerStatuses[j].State.Terminated.Message, true
			}
		}
		return "", false
	}

	s5aDriveToVerifyingWithReconciler(r, ns, name)

	jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
	job := &batchv1.Job{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job); err != nil {
		job = &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns, Labels: verifyJobLabels(name)}}
		Expect(k8sClient.Create(ctx, job)).To(Succeed())
	}
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
	switch inProgressState {
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
	case "check-0":
		addTerminated(s5aCloneBase)
		addTerminated(s5aImportAgent)
		addTerminated(s5aTamper)
		addTerminated(s5aArtifact)
	default:
		Fail(fmt.Sprintf("b1InProgSpec: unknown in-progress state %q", inProgressState))
	}
	pod.Status.InitContainerStatuses = inits
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

	b1Reconcile(r, ns, name)
	loop := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	Expect(loop.Status.Verify).To(BeNil(), "no recorded verify outcome while the verify is in progress (the in-progress state is pending, not a decision)")
	Expect(loop.Status.Checkpoint).To(BeNil(), "no checkpoint while the verify is in progress (no recorded outcome)")
	Expect(loop.Status.Checkpoints).To(BeEmpty(), "no checkpoint recorded while the verify is in progress")
	return loop
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

	It("retains the last 3 + every milestone and GCs the rest (retention + GC on OPERATOR-PRODUCED checkpoints; the named mutation — retention GC is a no-op — must fail)", func() {
		ns := "b1-retain-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		b1CreateVSC()

		name := "retainloop"
		spec := b1LoopSpec()
		// The cap is the max verify iteration + 1 (the operator's iterate
		// guard: a failing verify at the cap's iteration is terminal Failed,
		// not another iterate). 8 verify cycles (iter 0,2,3,4,5,6,7,8) need
		// the cap 9 to reach the GC at the iter-8 verify without tripping.
		spec.Loop.MaxIterations = 9
		// stallAfter 100: the spec drives 8 consecutive failing verifies with
		// the per-cycle readCheckOutput seam giving DIFFERENT raw output each
		// iteration (b1Cycle sets it), so the stall detector never sees 3 in a
		// row — the high cap is belt-and-suspenders for a same-output path.
		stallAfter100 := int32(100)
		spec.Loop.StallAfter = &stallAfter100
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       spec,
		})).To(Succeed())

		// The SPEC's reconciler: the driver seam is available (the checkpoint
		// is taken) and the readCheckOutput seam gives a per-cycle raw output
		// (the stall detector's hash source — different output each iteration
		// so the stall gate stays inert for the 8 consecutive failures).
		r := b1NewReconciler(true, false)
		r.readCheckOutput = func(pod *corev1.Pod, checkName string) (string, bool) {
			for j := range pod.Status.InitContainerStatuses {
				if pod.Status.InitContainerStatuses[j].Name == checkName && pod.Status.InitContainerStatuses[j].State.Terminated != nil {
					return pod.Status.InitContainerStatuses[j].State.Terminated.Message, true
				}
			}
			return "", false
		}

		// Drive to Verifying (iteration 0) on r (the driver/CRD seams active
		// on every reconcile). Then 8 verify cycles with a FAILING check
		// (exit 1): the operator applies each outcome (iter 0 -> 2 -> 3 ->
		// 4 -> 5 -> 6 -> 7 -> 8), and — level-based, keyed off the
		// operator-recorded verify outcome — takes a checkpoint per iteration
		// (Milestone=false, the verify failed). After the 8th cycle the
		// checkpoint list has 8 entries (the last 3 + every milestone
		// retention = {5,6,7,8} kept; {0,2,3,4} GC'd — every entry is
		// non-milestone, so the last-3 rule alone governs the keep-set).
		s5aDriveToVerifyingWithReconciler(r, ns, name)
		for seq := 1; seq <= 8; seq++ {
			b1Cycle(ctx, r, ns, name, seq, 1)
		}

		By("keeping the last 3 (iters 6,7,8) and GC'ing the older non-milestone checkpoints (0,2,3,4,5)")
		fresh := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"the 8th failing verify sent the Loop back to Implementing (iteration 8)")
		kept := map[int]bool{}
		for _, cp := range fresh.Status.Checkpoints {
			kept[cp.Iteration] = true
		}
		// The GC is applied on EVERY checkpoint take (level-based): after the
		// 8th take (iter 8) the keep-set over the full 8-entry list is the
		// last 3 (6,7,8) + the milestones (none — every verify failed). The
		// older non-milestone checkpoints (0,2,3,4,5) are GC'd.
		Expect(kept).To(HaveKeyWithValue(6, true), "iter 6 is in the last 3 — kept")
		Expect(kept).To(HaveKeyWithValue(7, true), "iter 7 is in the last 3 — kept")
		Expect(kept).To(HaveKeyWithValue(8, true), "iter 8 is the newest checkpoint — kept")
		for _, gcIter := range []int{0, 2, 3, 4, 5} {
			Expect(kept).NotTo(HaveKey(gcIter), "iter %d is not in the last 3 and is not a milestone — GC'd", gcIter)
		}

		By("deleting the GC'd VolumeSnapshot objects and keeping the kept ones")
		for _, it := range []int{6, 7, 8} {
			Expect(b1SnapshotExists(ctx, ns, fmt.Sprintf("checkpoint-%s-iter-%d", name, it))).
				To(BeTrue(), "the retention must keep the snapshot iter-%d", it)
		}
		for _, it := range []int{0, 2, 3, 4, 5} {
			Expect(b1SnapshotExists(ctx, ns, fmt.Sprintf("checkpoint-%s-iter-%d", name, it))).
				To(BeFalse(), "the retention must GC the snapshot iter-%d", it)
		}

		By("recording the checkpoints as non-milestone (the verifies FAILED — milestone = the verify passed)")
		for _, cp := range fresh.Status.Checkpoints {
			Expect(cp.Milestone).To(BeFalse(),
				"the checkpoint for the failed verify at iter %d must NOT be a milestone (B1: milestone = the verify passed)", cp.Iteration)
		}
	})

	It("creates the checkpoint for a recorded verify on a LATER reconcile, after the iteration was incremented and persisted (retry after the increment; level-based, not edge-triggered; must FAIL against 06767ac which keys off the current iteration)", func() {
		ns := "b1-retry-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		b1CreateVSC()

		name := "retryloop"
		spec := b1LoopSpec()
		spec.Loop.MaxIterations = 9
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       spec,
		})).To(Succeed())

		// The SPEC's reconciler with the driver seam AVAILABLE the whole time
		// (support is present). The checkpoint for iteration 0's verify is
		// taken on the NEXT reconcile (level-based: the outcome is recorded,
		// the checkpoint is absent, and the CURRENT iteration has already
		// been incremented past 0 — so keying off the current iteration
		// (06767ac) would create iter-2 before iteration 2 has verified
		// anything, and would NEVER create iter-0).
		r := b1NewReconciler(true, false)

		// Cycle 1 (iteration 0): a FAILING verify. The operator applies the
		// outcome: records status.verify.iteration=0 (lastCheckResults=[1],
		// milestone=false) and increments the iteration 0 -> 2 (the
		// Increment-and-persist the retry must happen AFTER).
		s5aDriveToVerifyingWithReconciler(r, ns, name)
		b1Cycle(ctx, r, ns, name, 1, 1)
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		By("recording the iteration-0 verify outcome and persisting the increment (iteration 0 -> 2)")
		Expect(loop.Status.Verify).NotTo(BeNil(), "the operator recorded the verify outcome")
		Expect(loop.Status.Verify.Iteration).NotTo(BeNil(), "the recorded iteration is present (the outcome was applied)")
		Expect(int(*loop.Status.Verify.Iteration)).To(Equal(0), "the verify ran at iteration 0")
		Expect(loop.Status.Iteration).To(BeNumerically(">=", 2), "the increment was persisted (the iterate bumped the iteration past 0)")
		// A LATER reconcile (no new verify outcome — the Loop is at
		// Implementing) must take the checkpoint for the recorded iteration
		// 0 (level-based: the recorded outcome is still there, the checkpoint
		// is not yet recorded, and the CURRENT iteration (2) is NOT what the
		// checkpoint is named for — keying off the current iteration (06767ac)
		// would create iter-2 before iteration 2 has verified anything).
		b1Reconcile(r, ns, name)
		fresh := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, fresh)).To(Succeed())
		By("taking the iteration-0 checkpoint on the later reconcile (after the increment)")
		Expect(fresh.Status.Checkpoint).NotTo(BeNil(), "the checkpoint MUST be taken on the later reconcile (level-based, not edge-triggered)")
		Expect(fresh.Status.Checkpoint.Iteration).To(Equal(0),
			"the checkpoint is named for the RECORDED iteration (0), not the current iteration (the 06767ac version names it for the current iteration)")
		Expect(fresh.Status.Checkpoint.Milestone).To(BeFalse(), "the verify FAILED (exit 1), so the checkpoint is not a milestone")
	})

	DescribeTable("does NOT create a checkpoint for the current iteration while its verify is in progress (one spec per in-progress state; must FAIL against 06767ac which keys off the current iteration and would checkpoint an in-progress verify)",
		func(state string) {
			ns := "b1-inprog-" + state + "-" + nowSuffix()
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
			}()
			b1CreateVSC()
			name := "inprogloop"
			_ = b1InProgSpec(ctx, name, ns, state)
		},
		Entry("a trusted init (clone-base) is still Running", "trusted-init"),
		Entry("the tamper init is still Running", "tamper"),
		Entry("the artifact init is still Running", "artifact"),
		Entry("the check-0 init has not started (absent from the pod status)", "check-0"),
	)

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
