/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the code for the distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the code for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// S4 (ADR-0004, R19 OS1/OS5/OS8): the runner as a one-shot phase driver and
// the operator's claim reader.
//
// These specs drive the LIVE reader (readPhaseClaimFromTerminationMessage via
// the APIReader path — no readPhaseClaim seam) by standing in for the
// one-shot runner: a sandbox pod with a terminated agent container whose
// termination message carries the strict claim JSON. The envtest API server
// is the pod source of truth, so a pod + status created here is exactly what
// the live APIReader read sees — the same path a live deployment uses (the
// sandbox pod is NOT in the manager's Pod cache, so only the non-cached
// client reads it; the mutation check at the bottom of this file proves it).
//
// One container run per phase (the S4 design choice, documented in
// loop_s4_phase.go): after each advance the operator recycles the sandbox pod
// (fresh emptyDir, phase-init writes the NEXT desired phase), so the happy-
// path spec recycles the stand-in pod between advances exactly as the
// operator does in a live cluster.

var _ = Describe("S4: ADR-0004 claim reader + phase machine advance", func() {
	ctx := context.Background()

	// s4LoopSpec builds the spec the reader specs use: the fixture repo (no
	// model config, so the D35 proxy gate does not hold the sandbox).
	s4LoopSpec := coxv1alpha1.LoopSpec{Goal: loopGoal, Workspace: testWorkspace()}

	// s4Reconciler builds the reconciler the reader specs use: the live
	// reader (apiReader = the plain client — a real, non-cached Get) and the
	// OS5 FakeRecorder (the specs assert the PhaseAdvanced Event).
	s4Reconciler := func(recorder *record.FakeRecorder) *LoopReconciler {
		return &LoopReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			apiReader: k8sClient,
			Recorder:  recorder,
		}
	}

	// createStandinPod creates the stand-in sandbox pod (envtest has no
	// agent-sandbox controller to run the real one) with no status yet.
	createStandinPod := func(ns, name string) {
		_ = k8sClient.Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: agentContainerName, Image: s3StandinImage}}},
		})
	}

	// writeAgentTermination sets (or replaces) the sandbox pod's status with a
	// terminated agent container carrying message — the one-shot runner's
	// single container run for the current phase.
	writeAgentTermination := func(ns, name, message string) {
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, pod)).To(Succeed())
		terminated := int32(0)
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{
			{Name: agentContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: terminated, Message: message,
			}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	// drainEvents drains the FakeRecorder's channel (Gomega's ContainElement
	// does not match on channels).
	drainEvents := func(recorder *record.FakeRecorder) []string {
		var out []string
	Loop:
		for {
			select {
			case e, ok := <-recorder.Events:
				if !ok {
					break Loop
				}
				out = append(out, e)
			default:
				break Loop
			}
		}
		return out
	}

	// primeReconcile creates the Loop, reconciles once (the option-B bootstrap
	// moves it Pending -> Planning and recycles the not-yet-existing sandbox),
	// and returns the reconciled Loop.
	primeReconcile := func(r *LoopReconciler, ns, name string) (*coxv1alpha1.Loop, types.NamespacedName) {
		nn := types.NamespacedName{Name: name, Namespace: ns}
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s4LoopSpec,
		})).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		return loop, nn
	}

	// oneShotRun stands in for ONE one-shot runner run of the CURRENT phase:
	// recreate the stand-in sandbox + pod (the operator's recycle deleted
	// them), write the agent's terminated status with the claim, and
	// reconcile. createStandinPod also creates the sandbox (the recycle
	// deleted it); withStatus writes the terminated status (false = skip it,
	// for a run the reader has not yet observed).
	// ensureSandboxObject creates the sandbox object if it does not exist
	// (the operator's annotation-based recycle deletes it on a phase advance).
	ensureSandboxObject := func(ns, name string) {
		sb := &sandboxv1beta1.Sandbox{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb); apierrors.IsNotFound(err) {
			_ = k8sClient.Create(ctx, &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
			})
		}
	}

	oneShotRun := func(r *LoopReconciler, nn types.NamespacedName, claimed coxv1alpha1.LoopPhase, withStatus bool) *coxv1alpha1.Loop {
		createStandinPod(nn.Namespace, nn.Name)
		if withStatus {
			// The runner's REAL claim shape (ADR-0004): the phase the runner
			// EXECUTED (the current phase — the runner never reports the next
			// phase), the status of that execution, and an empty
			// blockedReason on success. This is the exact strict-JSON object
			// the live runner writes to /dev/termination-log.
			writeAgentTermination(nn.Namespace, nn.Name, fmt.Sprintf(
				`{"observedPhase":"%s","status":"success","blockedReason":""}`, claimed))
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		ensureSandboxObject(nn.Namespace, nn.Name)
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		return loop
	}

	It("sets status.observedPhase from the claim via the APIReader path (no reader seam)", func() {
		ns := "s4-obs-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		recorder := record.NewFakeRecorder(64)
		r := s4Reconciler(recorder)
		loop, nn := primeReconcile(r, ns, "obsloop")
		By("priming the fresh Loop to Planning (option B bootstrap, sandbox recycled)")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning))
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhasePlanning))

		By("standing in for the one-shot runner: a pod whose agent terminated with a BLOCKED claim (the phase did not complete: no advance, but status.observedPhase IS recorded)")
		ensureSandboxObject(ns, "obsloop")
		createStandinPod(ns, "obsloop")
		writeAgentTermination(ns, "obsloop", `{"observedPhase":"Planning","status":"blocked","blockedReason":"model endpoint unreachable"}`)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		ensureSandboxObject(ns, "obsloop")
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		By("recording status.observedPhase from the claim (the B1 field) via the live APIReader path, without advancing (a blocked claim is not a completion)")
		Expect(loop.Status.ObservedPhase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"the live APIReader path must read the agent termination message and record the claimed phase")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"a blocked claim (status=blocked) must never advance the phase")
	})

	It("rejects an oversized claim (size-limited read, ADR-0005)", func() {
		ns := "s4-big-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		recorder := record.NewFakeRecorder(64)
		r := s4Reconciler(recorder)
		_, nn := primeReconcile(r, ns, "bigloop")

		ensureSandboxObject(ns, "bigloop")
		createStandinPod(ns, "bigloop")
		// A claim over the reader's cap (s4ClaimMaxBytes): the strict JSON
		// object is padded with a long blockedReason (the reader must reject
		// it, not act on it).
		oversized := `{"observedPhase":"Implementing","status":"blocked","blockedReason":"` +
			strings.Repeat("x", s4ClaimMaxBytes+256) + `"}`
		writeAgentTermination(ns, "bigloop", oversized)

		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		By("requeueing (the malformed claim is logged, never acted on; the message is stable until the pod is recreated)")
		Expect(res.RequeueAfter).To(BeNumerically(">", time.Second), "an oversized claim must requeue (never a phase completion)")
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		By("advancing neither the phase nor observedPhase (fail-closed)")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning))
		Expect(loop.Status.ObservedPhase).To(BeEmpty(), "a rejected claim must not be recorded")
	})

	It("rejects a malformed claim (strict JSON, ADR-0005)", func() {
		ns := "s4-mal-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		recorder := record.NewFakeRecorder(64)
		r := s4Reconciler(recorder)
		_, nn := primeReconcile(r, ns, "mloop")

		malformed := map[string]string{
			"not-json-at-all":                 "a plain-text message (the runner crashed without writing the claim)",
			`{"observedPhase":"Implementing"`: "truncated JSON (a write cut off mid-object)",
			`["observedPhase"]`:               "a JSON non-object",
			`{"status":"success"}`:            "an object with no observedPhase",
		}
		for msg, label := range malformed {
			By(fmt.Sprintf("case: %s", label))
			ensureSandboxObject(ns, "mloop")
			createStandinPod(ns, "mloop")
			writeAgentTermination(ns, "mloop", msg)

			res, rerr := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(rerr).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", time.Second),
				fmt.Sprintf("a malformed claim (%s) must requeue, never advance", label))

			loop := &coxv1alpha1.Loop{}
			Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
			Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
				fmt.Sprintf("a malformed claim (%s) must not advance the phase", label))
			Expect(loop.Status.ObservedPhase).To(BeEmpty(),
				fmt.Sprintf("a malformed claim (%s) must not be recorded", label))

			// The malformed message is stable (the container stays
			// Terminated): clear the stand-in pod so the NEXT case's message
			// is what the reader sees (the live equivalent is the operator's
			// pod recycle).
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mloop-sandbox", Namespace: ns}, pod)).To(Succeed())
			Expect(k8sClient.Delete(ctx, pod)).To(Succeed())
		}
	})

	It("advances Planning -> Implementing -> Verifying from successive claims, records progress, and emits PhaseAdvanced Events", func() {
		ns := "s4-happy-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		recorder := record.NewFakeRecorder(128)
		r := s4Reconciler(recorder)
		primed, nn := primeReconcile(r, ns, "happylp")
		Expect(primed.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning), "the bootstrap must leave the fresh Loop at Planning")

		// Pin the baseCommit directly (the S3 init-container read-back is
		// stubbed here; the spec asserts on the operator's pins in progress,
		// not on the S3 read-back itself). The sandbox exists now (prime
		// created it) but the stand-in pod is created by the spec — the pod
		// must exist before its status is written.
		ensureSandboxObject(ns, "happylp")
		createStandinPod(ns, "happylp")
		pinsLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, pinsLoop)).To(Succeed())
		pinsLoop.Status.BaseCommit = s3BaseCommitSHA
		Expect(k8sClient.Status().Update(ctx, pinsLoop)).To(Succeed())
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "happylp-sandbox", Namespace: ns}, pod)).To(Succeed())
		terminated := int32(0)
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: workspaceInitContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: terminated, Message: s3BaseCommitSHA}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		By("claim 1: the runner executed Planning (the claim names the CURRENT phase, status=success) — the completed phase advances one step")
		loop := oneShotRun(r, nn, coxv1alpha1.LoopPhasePlanning, true)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"a success claim for the current phase completes that phase (Planning -> Implementing)")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		Expect(loop.Status.ObservedPhase).To(Equal(coxv1alpha1.LoopPhasePlanning))
		Expect(loop.Status.Progress).NotTo(BeNil(), "the OS1 progress record must be populated from the claim")
		Expect(loop.Status.Progress.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning))
		Expect(loop.Status.Progress.LastResultStatus).To(Equal("success"))
		Expect(loop.Status.Progress.LastActivityTime).NotTo(BeNil(), "the operator must record WHEN the claim arrived (the pin is the operator's, not the claim's)")

		By("claim 2: the runner executed Implementing (success claim for the current phase) — completes Implementing and advances to Verifying")
		loop = oneShotRun(r, nn, coxv1alpha1.LoopPhaseImplementing, true)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "a success claim for the current phase completes that phase (Implementing -> Verifying)")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(loop.Status.ObservedPhase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"status.observedPhase records the phase the claim NAMES (the phase executed), not the phase the machine advanced to")
		Expect(loop.Status.Progress.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing), "the progress record follows the latest claim")

		By("claim 3: a success claim naming Verifying (a phase the runner does not execute) — no advance (the machine holds at Verifying; the exit is evidence-gated, B3)")
		loop = oneShotRun(r, nn, coxv1alpha1.LoopPhaseVerifying, true)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a claim naming Verifying has no successor mapping and is not the current phase (the runner stops at Implementing; Verifying is operator-owned, ADR-0005) — the machine holds")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(loop.Status.ObservedPhase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(loop.Status.Progress.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))

		By("emitting a PhaseAdvanced Event on every advance (OS5, stable reason)")
		events := drainEvents(recorder)
		Expect(events).To(ContainElement(ContainSubstring(phaseAdvancedReason)), "every phase transition must emit an Event with the stable PhaseAdvanced reason")
		Expect(events).To(ContainElement(ContainSubstring("Planning -> Implementing")), "the Event message carries the from/to phases")
		Expect(events).To(ContainElement(ContainSubstring("Implementing -> Verifying")))
	})

	It("holds a blocked claim (status=blocked, phase not completed: no advance)", func() {
		ns := "s4-block-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		recorder := record.NewFakeRecorder(64)
		r := s4Reconciler(recorder)
		_, nn := primeReconcile(r, ns, "blocklp")

		ensureSandboxObject(ns, "blocklp")
		createStandinPod(ns, "blocklp")
		// The runner ended PLANNING blocked: it reports the phase it was IN
		// (Planning == the current phase), never a completed step. The pure
		// nextPhase match rejects it (a blocked phase run is not a completion).
		writeAgentTermination(ns, "blocklp", `{"observedPhase":"Planning","status":"blocked","blockedReason":"model endpoint unreachable"}`)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		// Note: RequeueAfter is 5s (the S4 claim reader requeues while the
		// one-shot runner has not terminated, or the baseCommit is pending in
		// envtest). The spec asserts on phase, progress, and Events, not on
		// the requeue (which is expected behavior).
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		By("recording the blocked claim into progress (OS1) without advancing (the machine stays in the phase)")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning))
		Expect(loop.Status.Progress).NotTo(BeNil())
		Expect(loop.Status.Progress.LastResultStatus).To(Equal("blocked"))
		Expect(loop.Status.Progress.BlockedReason).To(Equal("model endpoint unreachable"))
		events := drainEvents(recorder)
		Expect(events).NotTo(ContainElement(ContainSubstring(phaseAdvancedReason)), "no advance -> no PhaseAdvanced Event")
	})

	It("advances from the runner's REAL claim shape: {Planning, success} -> Implementing", func() {
		ns := "s4-real-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		recorder := record.NewFakeRecorder(64)
		r := s4Reconciler(recorder)
		_, nn := primeReconcile(r, ns, "reallp")

		// The EXACT strict JSON the live runner writes for a completed
		// Planning phase (observedPhase == the phase executed, never the
		// next phase; status=success; empty blockedReason).
		ensureSandboxObject(ns, "reallp")
		createStandinPod(ns, "reallp")
		writeAgentTermination(ns, "reallp", `{"observedPhase":"Planning","status":"success","blockedReason":""}`)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		ensureSandboxObject(ns, "reallp")
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		By("completing Planning (success claim for the current phase) and advancing one step")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"{observedPhase:Planning,status:success} must advance Planning -> Implementing")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		events := drainEvents(recorder)
		Expect(events).To(ContainElement(ContainSubstring("Planning -> Implementing")))
	})

	It("advances from the runner's REAL claim shape: {Implementing, success} -> Verifying", func() {
		ns := "s4-real2-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		recorder := record.NewFakeRecorder(64)
		r := s4Reconciler(recorder)
		_, nn := primeReconcile(r, ns, "real2lp")

		// Drive the Loop to Implementing first (the completed-Planning
		// advance), then stand in for the completed Implementing run.
		loop := oneShotRun(r, nn, coxv1alpha1.LoopPhasePlanning, true)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing), "claim 1 must advance to Implementing first")
		loop = oneShotRun(r, nn, coxv1alpha1.LoopPhaseImplementing, true)
		By("completing Implementing (success claim for the current phase) and advancing to Verifying")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"{observedPhase:Implementing,status:success} must advance Implementing -> Verifying (B3 then gates the exit)")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
	})

	It("holds a blocked claim for the current phase (no successor mapping on failure)", func() {
		ns := "s4-real3-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		recorder := record.NewFakeRecorder(64)
		r := s4Reconciler(recorder)
		_, nn := primeReconcile(r, ns, "real3lp")

		ensureSandboxObject(ns, "real3lp")
		createStandinPod(ns, "real3lp")
		writeAgentTermination(ns, "real3lp", `{"observedPhase":"Planning","status":"blocked","blockedReason":"model HTTP 400"}`)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		By("holding the phase (a blocked claim never advances, even though it names the current phase)")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"{observedPhase:Planning,status:blocked} must NOT advance")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhasePlanning))
		events := drainEvents(recorder)
		Expect(events).NotTo(ContainElement(ContainSubstring(phaseAdvancedReason)))
	})

	It("records the claim's iteration and the operator's pins into progress (OS1)", func() {
		ns := "s4-pins-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		recorder := record.NewFakeRecorder(64)
		r := s4Reconciler(recorder)
		_, nn := primeReconcile(r, ns, "pinslp")

		// Stand-in for ONE run: the pod carries BOTH the terminated init
		// container (pinning baseCommit via the S3 read) and the terminated
		// agent (the claim carrying iteration 3). The progress record must
		// carry the claim's iteration AND the operator's pins.
		ensureSandboxObject(ns, "pinslp")
		createStandinPod(ns, "pinslp")
		// Pin the baseCommit directly (the S3 init-container read-back is
		// stubbed here; the spec asserts on the operator's pins in progress,
		// not on the S3 read-back itself).
		pinsLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, pinsLoop)).To(Succeed())
		pinsLoop.Status.BaseCommit = s3BaseCommitSHA
		Expect(k8sClient.Status().Update(ctx, pinsLoop)).To(Succeed())
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "pinslp-sandbox", Namespace: ns}, pod)).To(Succeed())
		terminated := int32(0)
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: workspaceInitContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: terminated, Message: s3BaseCommitSHA}}},
		}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{
			{Name: agentContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: terminated, Message: `{"observedPhase":"Planning","status":"success","iteration":3}`}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Progress).NotTo(BeNil(), "a readable claim must record progress")
		Expect(loop.Status.Progress.Iteration).To(Equal(3), "the claim's iteration rides into progress (OS1)")
		Expect(loop.Status.Progress.ObservedGeneration).To(Equal(loop.Generation), "the generation pin is the operator's")
		Expect(loop.Status.Progress.BaseCommit).To(Equal(s3BaseCommitSHA), "the baseCommit pin is the operator's (D10), never the claim's")
	})

	It("holds at Verifying: a runner claim (even {Verifying, success}) never changes phase or progress (Verifying is operator-owned, B3)", func() {
		ns := "s4-verify-hold-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		recorder := record.NewFakeRecorder(64)
		r := s4Reconciler(recorder)
		_, nn := primeReconcile(r, ns, "vhlp")

		// Pin the baseCommit directly (the S3 init-container read-back is
		// stubbed here; without it the S3 requeue would mask the hold's
		// zero-requeue assertion below). The sandbox exists now (prime
		// created it) but the stand-in pod is created by the spec — the pod
		// must exist before its status is written.
		ensureSandboxObject(ns, "vhlp")
		createStandinPod(ns, "vhlp")
		pinsLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, pinsLoop)).To(Succeed())
		pinsLoop.Status.BaseCommit = s3BaseCommitSHA
		Expect(k8sClient.Status().Update(ctx, pinsLoop)).To(Succeed())
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "vhlp-sandbox", Namespace: ns}, pod)).To(Succeed())
		terminated := int32(0)
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: workspaceInitContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: terminated, Message: s3BaseCommitSHA}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		// Drive the machine to Verifying: two one-shot success runs
		// (Planning -> Implementing -> Verifying).
		loop := oneShotRun(r, nn, coxv1alpha1.LoopPhasePlanning, true)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		loop = oneShotRun(r, nn, coxv1alpha1.LoopPhaseImplementing, true)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "the machine must land at Verifying before the hold is asserted")
		progressAtVerifying := loop.Status.Progress.DeepCopy()

		// The stand-in claim: the live runner idles at a Verifying desired
		// phase (no model call, no claim, exit on SIGTERM), but this spec
		// proves the OPERATOR'S hold is independent of the runner's behavior —
		// even a claim the operator would parse (a success claim naming
		// Verifying, the shape a stale/mis-set pod could present) must not
		// move the phase or rewrite progress.
		ensureSandboxObject(ns, "vhlp")
		createStandinPod(ns, "vhlp")
		writeAgentTermination(ns, "vhlp", `{"observedPhase":"Verifying","status":"success","blockedReason":""}`)
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())

		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"at Verifying no runner claim advances the phase (the exit is evidence-gated, B3; S5)")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(loop.Status.ObservedPhase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"a claim at Verifying must not touch status.observedPhase (the hold returns before any claim handling)")
		Expect(loop.Status.Progress).To(Equal(progressAtVerifying),
			"a claim at Verifying must not rewrite progress (no churn from the runner's crash-loop or a stale claim)")
		Expect(res.RequeueAfter).To(BeZero(), "the Verifying hold must not requeue on a claim (the requeue belongs to the B3 verify Job)")
		events := drainEvents(recorder)
		Expect(events).NotTo(ContainElement(ContainSubstring(phaseAdvancedReason)), "no advance at Verifying -> no PhaseAdvanced Event")
	})
})
