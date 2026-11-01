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

// D85 (owner decision, R34): plan approval moves from a user-written status
// condition to a one-shot annotation the operator consumes. The CLI's
// `cox approve` / `cox reject` patch the Loop OBJECT's metadata
// (coxswain.io/approve: <plan sha256> / coxswain.io/reject: <reason>), and
// the operator — while the Loop is held for approval — turns the annotation
// into the PlanApproved / PlanRejected condition ITSELF (the same message
// format the CLI wrote before, so approvedPlanHash / rejectedPlanReason keep
// working) and removes the annotation. The operator is now the ONLY writer
// of the two conditions; the editor role goes back to get-only on
// loops/status.
//
// The specs cover (TDD — red first):
//   - approve with the CURRENT plan hash at AwaitingApproval → PlanApproved
//     True and the annotation is gone;
//   - approve → the operator copies the annotation's hash VERBATIM into
//     PlanApproved (the operator does NOT read the plan — staleness is
//     I93's job, the runner's plan-hash check at Implementing start, see
//     loop_i93_planhash_envtest_test.go);
//   - reject → PlanRejected, and the existing reject flow continues
//     (Replan: reason durable, phase to Planning, condition cleared);
//   - BOTH annotations → reject wins, both are removed, an Event says so;
//   - an annotation on a Loop that is NOT held for approval → removed with a
//     Normal Event and no effect;
//   - re-approving after a re-plan → the approval binds to the NEW hash
//     (approvedPlanHash on the new plan, the old stale one does not advance);
//   - a FAILED status write at consume time → the annotation is NOT removed
//     (the user's decision survives on the object), and the next reconcile
//     (no failure) converts it to the condition and removes the annotation
//     (the P1 on #377: the annotation removal must not precede the
//     condition's persistence);
//   - a reject annotation whose value already starts with "Rejected: " →
//     the leading prefix is stripped at consume time (no doubling in the
//     condition's message — the RejectedEvent's message carries the reason
//     exactly as the user wrote it, no "Rejected: Rejected: ").

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// d85StatusFaultClient makes the FIRST status-subresource write of the named
// Loop fail with an injected error (the P1 fault-injection spec on #377: a
// failed status write at consume time must NOT remove the annotation).
// Every other call passes through to the inner client. It implements
// client.Client by delegation (the envtest plain client does not implement
// client.WithWatch, so the conflictOnceClient/b1CreateFailClient idiom is
// used rather than client/interceptor). The SubResourceWriter methods (the
// "status" subresource the consume writes through Status().Patch /
// Status().Update) are the ones that inject; the plain-resource methods
// (Get/List/Create/Delete/Update/Patch/DeleteAllOf/Apply) are pass-through.
type d85StatusFaultClient struct {
	inner client.Client
	name  string
	left  int
}

func (c *d85StatusFaultClient) isTarget(obj client.Object) bool {
	l, ok := obj.(*coxv1alpha1.Loop)
	return ok && l.Name == c.name
}

// injectNextWrite fails once when c.left > 0 (the injected status write),
// decrementing left; otherwise nil.
func (c *d85StatusFaultClient) injectNextWrite(obj client.Object) error {
	if c.isTarget(obj) && c.left > 0 {
		c.left--
		return fmt.Errorf("injected: first status write at consume time must fail")
	}
	return nil
}

// client.Client methods (pass-through — the consume's metadata PATCH and
// every other write must reach the API unharmed; only the status write
// carries the fault).

func (c *d85StatusFaultClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.inner.Get(ctx, key, obj, opts...)
}

func (c *d85StatusFaultClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.inner.List(ctx, list, opts...)
}

func (c *d85StatusFaultClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return c.inner.Create(ctx, obj, opts...)
}

func (c *d85StatusFaultClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return c.inner.Delete(ctx, obj, opts...)
}

func (c *d85StatusFaultClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return c.inner.Update(ctx, obj, opts...)
}

func (c *d85StatusFaultClient) Patch(ctx context.Context, obj client.Object, data client.Patch, opts ...client.PatchOption) error {
	return c.inner.Patch(ctx, obj, data, opts...)
}

func (c *d85StatusFaultClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	return c.inner.DeleteAllOf(ctx, obj, opts...)
}

func (c *d85StatusFaultClient) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	return c.inner.Apply(ctx, obj, opts...)
}

func (c *d85StatusFaultClient) GroupVersionKindFor(obj runtime.Object) (schema.GroupVersionKind, error) {
	return c.inner.GroupVersionKindFor(obj)
}

func (c *d85StatusFaultClient) IsObjectNamespaced(obj runtime.Object) (bool, error) {
	return c.inner.IsObjectNamespaced(obj)
}

func (c *d85StatusFaultClient) Scheme() *runtime.Scheme {
	return c.inner.Scheme()
}

func (c *d85StatusFaultClient) RESTMapper() meta.RESTMapper {
	return c.inner.RESTMapper()
}

// SubResource returns the inner client's sub-resource client (the fault
// lives on the Status() path; SubResource is not used by the consume).
func (c *d85StatusFaultClient) SubResource(subResource string) client.SubResourceClient {
	return c.inner.SubResource(subResource)
}

// compile-time check.
var _ client.Client = (*d85StatusFaultClient)(nil)
var _ client.SubResourceWriter = (*d85StatusFaultSubWriter)(nil)

// d85StatusFaultSubWriter is the SubResourceWriter the consume's status
// writes go through. It is a separate type so the SubResourceWriter's own
// Create/Update/Patch/Apply methods (different signatures from the
// plain-resource ones) do not collide with d85StatusFaultClient's.
type d85StatusFaultSubWriter struct {
	c     *d85StatusFaultClient
	inner client.SubResourceWriter
}

func (w *d85StatusFaultSubWriter) Create(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	return w.inner.Create(ctx, obj, subResource, opts...)
}

func (w *d85StatusFaultSubWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if err := w.c.injectNextWrite(obj); err != nil {
		return err
	}
	return w.inner.Update(ctx, obj, opts...)
}

func (w *d85StatusFaultSubWriter) Patch(ctx context.Context, obj client.Object, data client.Patch, opts ...client.SubResourcePatchOption) error {
	if err := w.c.injectNextWrite(obj); err != nil {
		return err
	}
	return w.inner.Patch(ctx, obj, data, opts...)
}

func (w *d85StatusFaultSubWriter) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	return w.inner.Apply(ctx, obj, opts...)
}

// statusWriter returns a new sub-writer (the fault state is on the outer
// client, so the left counter is shared across calls).
func (c *d85StatusFaultClient) statusWriter() *d85StatusFaultSubWriter {
	return &d85StatusFaultSubWriter{c: c, inner: c.inner.Status()}
}

// Status returns the sub-writer (the fault lives on the subresource methods,
// not on the plain-resource ones).
func (c *d85StatusFaultClient) Status() client.SubResourceWriter {
	return c.statusWriter()
}

// d85EventSink is the broadcaster's EventSink (it posts the Event to the
// envtest cluster via k8sClient.Create — the broadcaster's default sink
// writes to a file, which the specs cannot see).
type d85EventSink struct {
	client client.Client
}

func (s d85EventSink) Create(e *corev1.Event) (*corev1.Event, error) {
	out := &corev1.Event{}
	err := s.client.Create(context.Background(), e)
	return out, err
}

func (s d85EventSink) Update(e *corev1.Event) (*corev1.Event, error) {
	out := &corev1.Event{}
	err := s.client.Update(context.Background(), e)
	return out, err
}

func (s d85EventSink) Patch(e *corev1.Event, data []byte) (*corev1.Event, error) {
	return e, nil
}

var _ = Describe("D85 plan-approval annotation", func() {
	ctx := context.Background()

	const (
		approveAnnotation = "coxswain.io/approve"
		rejectAnnotation  = "coxswain.io/reject"
	)

	sha := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return hex.EncodeToString(h[:])
	}

	// d85Reconciler builds the reconciler the specs drive (the same setup as
	// the B3 suite: a plain client, a readPhaseClaim seam that serves
	// "agent still running", the approval gate wired). A REAL event recorder
	// is wired (the broadcaster posts Kubernetes Events to the envtest
	// cluster; the specs assert on the live Events via k8sClient.List — the
	// FakeRecorder's channel would be invisible to the API).
	d85Reconciler := func() *LoopReconciler {
		broadcaster := record.NewBroadcaster()
		broadcaster.StartRecordingToSink(d85EventSink{client: k8sClient})
		r := &LoopReconciler{CNIProber: testCNIProber(),
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: broadcaster.NewRecorder(k8sClient.Scheme(), corev1.EventSource{Component: "d85-spec"}),
		}
		r.wireApprovalGate()
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return nil, nil
		}
		return r
	}

	reconcileLoop := func(r *LoopReconciler, ns, name string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
	}

	// d85Loop creates a Manual-mode Loop (the S4 bootstrap primes it) and
	// moves it to the given phase with a Status().Update (the same
	// phase-forcing the B3 suite uses for its hold specs).
	d85Loop := func(ns, name string, phase coxv1alpha1.LoopPhase) (types.NamespacedName, *LoopReconciler) {
		nn := types.NamespacedName{Name: name, Namespace: ns}
		r := d85Reconciler()
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{Goal: loopGoal, Workspace: testWorkspace(),
				Approval: &coxv1alpha1.Approval{Mode: coxv1alpha1.ApprovalModeManual}},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop(r, ns, name)
		if phase != "" {
			Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
			loop.Status.Phase = phase
			loop.Status.DesiredPhase = phase
			Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		}
		return nn, r
	}

	// annotate patches the Loop's metadata with the given annotations
	// (simulating `cox approve` / `cox reject` writing the one-shot
	// annotation via a JSON merge patch on the main resource).
	annotate := func(nn types.NamespacedName, annotations map[string]string) {
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		if loop.Annotations == nil {
			loop.Annotations = map[string]string{}
		}
		maps.Copy(loop.Annotations, annotations)
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())
	}

	// annotationAbsent reports whether the named annotation is gone from the
	// live Loop.
	annotationAbsent := func(nn types.NamespacedName, key string) bool {
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		_, present := loop.Annotations[key]
		return !present
	}

	// conditionMessage returns the live message of the named condition
	// ("" when absent).
	conditionMessage := func(nn types.NamespacedName, condType string) string {
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		for _, c := range loop.Status.Conditions {
			if c.Type == condType {
				return c.Message
			}
		}
		return ""
	}

	// loopEvents lists the live Events whose involved-object name matches and
	// returns the (type, reason, message) triples for reasons that contain
	// the given needle.
	loopEvents := func(nn types.NamespacedName, reasonNeedle string) []corev1.Event {
		var ev corev1.EventList
		Expect(k8sClient.List(ctx, &ev, client.InNamespace(nn.Namespace))).To(Succeed())
		var out []corev1.Event
		for _, e := range ev.Items {
			if e.InvolvedObject.Name != nn.Name {
				continue
			}
			if reasonNeedle == "" || strings.Contains(e.Reason, reasonNeedle) {
				out = append(out, e)
			}
		}
		return out
	}

	It("approve with the CURRENT plan hash: the operator sets PlanApproved itself and removes the annotation", func() {
		ns := "d85-approve-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		currentPlan := "plan-v1\n"
		nn, r := d85Loop(ns, "d85-approve", coxv1alpha1.LoopPhaseAwaitingApproval)
		// The user's `cox approve` writes the one-shot annotation with the
		// hash of the plan it read (the same hash the CLI put in the
		// condition's message before D85).
		annotate(nn, map[string]string{approveAnnotation: sha(currentPlan)})
		reconcileLoop(r, ns, "d85-approve")

		// The operator is now the sole writer of PlanApproved: the condition
		// is True and its message carries the plan hash (the same
		// "Approved: <hash>" format approvedPlanHash parses).
		msg := conditionMessage(nn, coxv1alpha1.PlanApprovedCondition)
		Expect(msg).To(ContainSubstring(sha(currentPlan)),
			"D85: the operator sets PlanApproved from the approve annotation (message carries the plan hash)")
		// The one-shot annotation is consumed (removed).
		Expect(annotationAbsent(nn, approveAnnotation)).To(BeTrue(),
			"D85: the operator removes the approve annotation after consuming it")
		// The gate's approved branch then releases the hold on this same
		// reconcile (the annotation is converted BEFORE the switch runs).
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"D85: an approved current plan releases the hold (AwaitingApproval -> Implementing)")
	})

	It("the operator copies the annotation's hash VERBATIM into PlanApproved (it does not read the plan)", func() {
		ns := "d85-verbatim-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		// The operator does NOT read the plan (no peek pod, no pod-log
		// read): whatever hash the annotation carries is copied VERBATIM
		// into the PlanApproved message — staleness is I93's job (the
		// runner's plan-hash check at Implementing start, see
		// loop_i93_planhash_envtest_test.go). An arbitrary 64-hex value
		// (not a real plan's hash) must still land in the message.
		arbHash := sha("not-a-plan-hash\n")
		nn, r := d85Loop(ns, "d85-verbatim", coxv1alpha1.LoopPhaseAwaitingApproval)
		annotate(nn, map[string]string{approveAnnotation: arbHash})
		reconcileLoop(r, ns, "d85-verbatim")

		msg := conditionMessage(nn, coxv1alpha1.PlanApprovedCondition)
		Expect(msg).To(ContainSubstring(arbHash),
			"D85: the operator copies the annotation's hash verbatim into PlanApproved (no plan read)")
		Expect(annotationAbsent(nn, approveAnnotation)).To(BeTrue(),
			"D85: the operator removes the approve annotation after consuming it")
	})

	It("reject: the operator sets PlanRejected from the annotation and the existing reject flow continues (Replan)", func() {
		ns := "d85-reject-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		const reason = "the plan misses the edge case"
		nn, r := d85Loop(ns, "d85-reject", coxv1alpha1.LoopPhaseAwaitingApproval)
		annotate(nn, map[string]string{rejectAnnotation: reason})
		reconcileLoop(r, ns, "d85-reject")

		// The reject annotation's value is consumed as-is (a leading
		// "Rejected: " prefix the user wrote by hand is stripped at consume
		// time, so the message never carries it twice — the next spec).
		// The existing reject flow continues: the gate's Replan branch ran on
		// this same reconcile — the durable reason is set, the condition is
		// CLEARED (the re-plan starts fresh), and the phase moved to Planning.
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Approval).ToNot(BeNil(), "D85: the rejection reason is DURABLE after the reject reconcile")
		Expect(loop.Status.Approval.LastRejectionReason).To(ContainSubstring(reason),
			"D85: the durable reason carries the reject annotation's value")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"D85: the Replan action re-enters Planning (the existing reject flow)")
		hasRejected := false
		for _, c := range loop.Status.Conditions {
			if c.Type == coxv1alpha1.PlanRejectedCondition && c.Status == metav1.ConditionTrue {
				hasRejected = true
			}
		}
		Expect(hasRejected).To(BeFalse(), "D85: the PlanRejected condition is cleared by the existing flow (the re-plan starts fresh)")
		Expect(annotationAbsent(nn, rejectAnnotation)).To(BeTrue(),
			"D85: the operator removes the reject annotation after consuming it")
	})

	It("a reject annotation whose value already starts with 'Rejected: ': the leading prefix is stripped at consume time", func() {
		ns := "d85-rejprefix-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn, r := d85Loop(ns, "d85-rejprefix", coxv1alpha1.LoopPhaseAwaitingApproval)
		annotate(nn, map[string]string{rejectAnnotation: "Rejected: the reason already prefixed"})
		reconcileLoop(r, ns, "d85-rejprefix")

		// The consume stripped the leading "Rejected: " so the condition's
		// message ("Rejected: <reason>") carries the reason once — not
		// "Rejected: Rejected: <reason>". The Replan flow cleared the
		// condition on this same pass (the durable reason survives on
		// status.approval.lastRejectionReason). The strip is observed on the
		// durable reason: the operator's format "Rejected: <reason>" applied
		// to the user's "Rejected: <reason>" (with the leading prefix
		// stripped) is "Rejected: <reason>" — not "Rejected: Rejected: <reason>".
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Approval).ToNot(BeNil(), "D85: the rejection reason is durable")
		// The reason (the whole message, the operator's format "Rejected: <reason>")
		// carries the reason ONCE. If the prefix had NOT been stripped, the reason
		// would be "Rejected: Rejected: <reason>".
		Expect(loop.Status.Approval.LastRejectionReason).To(Equal("Rejected: the reason already prefixed"),
			"D85: the durable reason carries the reason ONCE (not 'Rejected: Rejected: <reason>')")
		Expect(annotationAbsent(nn, rejectAnnotation)).To(BeTrue(),
			"D85: the operator removes the reject annotation after consuming it")
	})

	It("a FAILED status write at consume time: the annotation is kept and the next reconcile converts it (P1 on #377)", func() {
		ns := "d85-faultstatus-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		// The operator persists the condition FIRST (a status write) and
		// removes the annotation only after that write succeeds. Inject a
		// failure into the FIRST status write of the Loop: the consume must
		// abort WITHOUT removing the annotation (the user's decision survives
		// on the object), and no condition is set.
		faultClient := &d85StatusFaultClient{inner: k8sClient, name: "d85-faultstatus", left: 1}
		faultReconciler := d85Reconciler()
		faultReconciler.Client = faultClient

		nn, _ := d85Loop(ns, "d85-faultstatus", coxv1alpha1.LoopPhaseAwaitingApproval)
		annotate(nn, map[string]string{approveAnnotation: sha("plan-v1\n")})
		reconcileLoop(faultReconciler, ns, "d85-faultstatus")

		// Pass 1 (the injected failure): the annotation is STILL present —
		// the consume's persist failed and the annotation removal was not
		// attempted, so the user's decision survives on the object.
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Annotations[approveAnnotation]).ToNot(BeEmpty(),
			"D85 P1: a failed status write at consume time must NOT remove the annotation")

		// Pass 2 (no injection): the still-present annotation is re-consumed
		// (idempotent: same hash, same condition) — PlanApproved is set and
		// the annotation is gone.
		reconcileLoop(faultReconciler, ns, "d85-faultstatus")
		msg := conditionMessage(nn, coxv1alpha1.PlanApprovedCondition)
		Expect(msg).To(ContainSubstring(sha("plan-v1\n")),
			"D85 P1: the next reconcile converts the surviving annotation to PlanApproved")
		Expect(annotationAbsent(nn, approveAnnotation)).To(BeTrue(),
			"D85 P1: the annotation is removed after the successful consume")
	})

	It("BOTH annotations: reject wins, both are removed, and the Event says so", func() {
		ns := "d85-both-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn, r := d85Loop(ns, "d85-both", coxv1alpha1.LoopPhaseAwaitingApproval)
		annotate(nn, map[string]string{
			approveAnnotation: sha("plan-v1\n"),
			rejectAnnotation:  "both were set",
		})
		reconcileLoop(r, ns, "d85-both")

		// Reject wins: the reject flow ran (durable reason, Planning), and NO
		// PlanApproved was set (the approve annotation is dropped without
		// effect).
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"D85: reject wins over a same-reconcile approve (the Replan action ran)")
		Expect(conditionMessage(nn, coxv1alpha1.PlanApprovedCondition)).To(BeEmpty(),
			"D85: the approve annotation is dropped when both are present (reject wins)")
		// BOTH annotations are removed (the Event names the collision).
		Expect(annotationAbsent(nn, approveAnnotation)).To(BeTrue(), "D85: the approve annotation is removed")
		Expect(annotationAbsent(nn, rejectAnnotation)).To(BeTrue(), "D85: the reject annotation is removed")
		ev := loopEvents(nn, "RejectWins")
		Expect(ev).ToNot(BeEmpty(), "D85: both annotations emit an Event naming the collision (RejectWins)")
	})

	It("an annotation on a Loop that is NOT held for approval: removed with a Normal Event and no effect", func() {
		ns := "d85-nohold-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		// Implementing is NOT the approval hold: the annotation is stale
		// (the Loop already advanced, or was set after the hold released).
		nn, r := d85Loop(ns, "d85-nohold", coxv1alpha1.LoopPhaseImplementing)
		annotate(nn, map[string]string{
			approveAnnotation: sha("plan-v1\n"),
			rejectAnnotation:  "too late",
		})
		reconcileLoop(r, ns, "d85-nohold")

		// No effect: the phase is unchanged and no approval condition was
		// set.
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"D85: an annotation outside the approval hold has no effect (the phase is unchanged)")
		Expect(conditionMessage(nn, coxv1alpha1.PlanApprovedCondition)).To(BeEmpty())
		Expect(conditionMessage(nn, coxv1alpha1.PlanRejectedCondition)).To(BeEmpty())
		// The annotations are removed with a Normal Event (not a Warning —
		// the user did nothing wrong).
		Expect(annotationAbsent(nn, approveAnnotation)).To(BeTrue(), "D85: the stale approve annotation is removed")
		Expect(annotationAbsent(nn, rejectAnnotation)).To(BeTrue(), "D85: the stale reject annotation is removed")
		var ev corev1.EventList
		Expect(k8sClient.List(ctx, &ev, client.InNamespace(ns))).To(Succeed())
		normals := 0
		for _, e := range ev.Items {
			if e.InvolvedObject.Name == nn.Name && e.Type == corev1.EventTypeNormal {
				normals++
			}
		}
		Expect(normals).To(BeNumerically(">=", 1),
			"D85: removing an annotation outside the hold emits a Normal Event")
	})

	It("re-approving after a re-plan: the approval binds to the NEW hash (the old one does not advance)", func() {
		ns := "d85-replan-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		oldPlan := "plan-v1\n"
		newPlan := "plan-v2\n"
		nn, r := d85Loop(ns, "d85-replan", coxv1alpha1.LoopPhaseAwaitingApproval)
		// Round 1: the user approves the CURRENT plan (oldPlan) → the hold
		// releases (the existing flow).
		annotate(nn, map[string]string{approveAnnotation: sha(oldPlan)})
		reconcileLoop(r, ns, "d85-replan")
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"round 1: the current-plan approval releases the hold")
		// The re-plan re-enters the hold on the NEW plan (a rejection / plan
		// change — here simulated with a phase force, the same pattern the B3
		// suite uses for its re-hold specs).
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		loop.Status.Phase = coxv1alpha1.LoopPhaseAwaitingApproval
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseAwaitingApproval
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		// The user re-approves with the NEW plan's hash → the approval BOUND
		// to the new hash advances the Loop (approvedPlanHash parses the new
		// hash from the message).
		annotate(nn, map[string]string{approveAnnotation: sha(newPlan)})
		reconcileLoop(r, ns, "d85-replan")
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"round 2: the re-plan's approval releases the hold")
		Expect(conditionMessage(nn, coxv1alpha1.PlanApprovedCondition)).To(ContainSubstring(sha(newPlan)),
			"round 2: PlanApproved is bound to the NEW plan's hash (the old stale one does not advance)")
		Expect(annotationAbsent(nn, approveAnnotation)).To(BeTrue())
	})
})
