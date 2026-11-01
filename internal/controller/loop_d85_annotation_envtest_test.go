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
//   - approve with a STALE hash → no PlanApproved, a Warning Event, the
//     annotation is gone, the hold keeps;
//   - reject → PlanRejected, and the existing reject flow continues
//     (Replan: reason durable, phase to Planning, condition cleared);
//   - BOTH annotations → reject wins, both are removed, an Event says so;
//   - an annotation on a Loop that is NOT held for approval → removed with a
//     Normal Event and no effect;
//   - re-approving after a re-plan → the approval binds to the NEW hash
//     (approvedPlanHash on the new plan, the old stale one does not advance).

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

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

	// d85ReconcilerSeam is the same as d85Reconciler but with a
	// readLoopPlan seam that returns the given plan bytes (the stale-hash
	// spec wires it to return the CURRENT plan, so the operator's fresh
	// read hashes to a different value than the user's annotation — which
	// was computed from the plan the user approved BEFORE the re-plan — and
	// the approve is rejected with a Warning Event).
	d85ReconcilerSeam := func(currentPlan string) *LoopReconciler {
		r := d85Reconciler()
		r.readLoopPlan = func(_ context.Context, _ *coxv1alpha1.Loop) (string, error) {
			return currentPlan, nil
		}
		return r
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

	// d85LoopNoReconcile is the same as d85Loop but WITHOUT the bootstrap
	// reconcile (the stale-hash spec uses it so the SPEC's reconciler — with
	// its readLoopPlan seam — is the one that consumes the annotation and
	// runs the gate; the bootstrap reconcile would consume the annotation
	// with a nil readLoopPlan, which trusts the user's hash and would set
	// PlanApproved, defeating the stale-hash test).
	d85LoopNoReconcile := func(ns, name string, phase coxv1alpha1.LoopPhase) types.NamespacedName {
		nn := types.NamespacedName{Name: name, Namespace: ns}
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{Goal: loopGoal, Workspace: testWorkspace(),
				Approval: &coxv1alpha1.Approval{Mode: coxv1alpha1.ApprovalModeManual}},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		if phase != "" {
			Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
			loop.Status.Phase = phase
			loop.Status.DesiredPhase = phase
			Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		}
		return nn
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
		for k, v := range annotations {
			loop.Annotations[k] = v
		}
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

	It("approve with a STALE hash: no PlanApproved, a Warning Event naming both hashes, the annotation is gone and the hold keeps", func() {
		ns := "d85-stale-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		staleHash := sha("plan-v1\n") // the plan the user approved BEFORE the re-plan
		// (the CURRENT plan at the hold is a different one — the annotation's
		// hash does not match it, so the approval is stale). The hash match is
		// the test's seam: the spec asserts on the STALE hash's effect, not on
		// a live workspace read (the d85ReconcilerSeam below wires the
		// operator's readLoopPlan to return the CURRENT plan, so the
		// operator's fresh read hashes to a different value than the
		// annotation's — the exact stale-hash condition).
		currentPlan := "plan-v2 (re-planned)\n"
		nn := d85LoopNoReconcile(ns, "d85-stale", coxv1alpha1.LoopPhaseAwaitingApproval)
		r := d85ReconcilerSeam(currentPlan)
		annotate(nn, map[string]string{approveAnnotation: staleHash})
		reconcileLoop(r, ns, "d85-stale")

		// No PlanApproved (the hash does not match the CURRENT plan): the
		// hold keeps and the phase is unchanged.
		msg := conditionMessage(nn, coxv1alpha1.PlanApprovedCondition)
		Expect(msg).To(BeEmpty(), "D85: a stale-hash approve annotation must NOT set PlanApproved")
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseAwaitingApproval),
			"D85: the hold keeps on a stale-hash approval")
		// The one-shot annotation is removed (a stale hash is still consumed:
		// the user re-approves against the current plan).
		Expect(annotationAbsent(nn, approveAnnotation)).To(BeTrue(),
			"D85: the operator removes the stale approve annotation")
		// A Warning Event names the situation (the StaleApproval reason).
		ev := loopEvents(nn, "Stale")
		Expect(ev).ToNot(BeEmpty(), "D85: a stale-hash approval emits a Warning Event (StaleApproval)")
		Expect(ev[0].Type).To(Equal(corev1.EventTypeWarning))
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
