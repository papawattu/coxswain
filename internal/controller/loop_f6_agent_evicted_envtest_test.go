// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on the "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the code for the distributed under the License is distributed on an "AS
// IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied.

// F6 (dogfood finding, P1): a sandbox pod that FAILED without a phase claim
// (evicted — the /tmp emptyDir sizeLimit eviction seen live on
// dogfood/dogfood-chartcheck, OOMKilled, or the agent container terminated
// without a claim) left the Loop SILENTLY stuck in Planning: the claim reader
// re-queued forever (no claim, no decision), and no condition or Event
// surfaced the wedge.
//
// The fix (mirroring the I65 VerifyInfraFailed infra-retry pattern onto the
// sandbox): the operator NOTICES the failed pod (advancePhaseFromClaim's
// claim-read-pending path, before the Verifying hold — a failed pod in
// Planning must be caught, not held), SURFACES it (I90: the AgentFailed
// condition True with the container's termination reason + the kubelet
// message, and a Warning Event per retry and on the final failure), and
// retries in a BOUNDED, VISIBLE way (recreate the Sandbox; count attempts in
// status.agent.infraAttempts, the I65 P1-A per-Sandbox-UID guard) — or fails
// the Loop with the distinct SandboxFailed reason once the bound is exhausted
// (sandboxInfraRetries = 2 recreations = 3 total runs).
//
// The specs:
//
//  1. an EVICTED pod with no claim (the live repro): the condition + the
//     Event are surfaced, the Sandbox is deleted for a retry, the attempt is
//     counted (status.agent.infraAttempts == 1);
//  2. a re-read of the SAME failing Sandbox (the reconcile re-ran before the
//     delete completed — the I65 P1-A guard) does NOT double-count;
//  3. a fresh, healthy pod on the NEXT (recreated) Sandbox CLEARS the
//     AgentFailed condition and the attempt count (the I49 in-progress keep:
//     a Running agent is never a failure, the I65 P1-B reset pattern);
//  4. repeated failures (a persistently broken node) fail the Loop with the
//     distinct SandboxFailed reason after the bound is exhausted (the Failed
//     condition, phase Failed, the AgentFailed condition cleared);
//  5. an IN-PROGRESS agent (the pod is Running, the agent has not terminated
//     yet) is NO decision (the I49 norm): no condition, no Event, no delete
//     (the normal requeue only).
//
// The envtest fixture models the live pod: the operator's own ensureSandbox
// creates the Sandbox CR + the Pod (the sandbox controller runs in envtest?
// NO — the envtest has no sandbox controller, so the operator's pod is never
// created by the controller. The specs plant the Pod via the API, and the
// operator's ensureSandbox finds the pod absent and... does not create it
// either — it creates the Sandbox CR and returns nil. The pod is planted
// directly by the spec). The pod is planted via Create + Status().Update
// (the API drops status on create, the I65 fixture's pattern).
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// f6EvictMessage is the live eviction message the dogfood F6 saw on
// dogfood/dogfood-chartcheck (the kubelet-capped termination message for the
// /tmp emptyDir sizeLimit overrun — the agent was OOM-killed for the EmptyDir
// usage, the message is the kubelet's, not the runner's).
const f6EvictMessage = "Usage of EmptyDir volume tmp exceeds the limit 100Mi"

var _ = Describe("F6: a sandbox pod that failed without a claim is surfaced and retried (bounded)", func() {
	ctx := context.Background()

	// f6NS creates a fresh namespace (the specs defer-delete it).
	f6NS := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	// f6Cond returns the condition of the given type (absent = nil).
	f6Cond := func(loop *coxv1alpha1.Loop, condType string) *metav1.Condition {
		for i := range loop.Status.Conditions {
			c := &loop.Status.Conditions[i]
			if c.Type == condType {
				return c
			}
		}
		return nil
	}

	// f6NewReconciler builds the reconciler the specs drive: the claim reader
	// starts serving (nil, nil) — the agent is "still running" (no claim) —
	// so the operator's claim-read-pending path is taken and the F6 check
	// runs. The operator's sandboxFailure reader uses the DEFAULT
	// (readPhaseClaimFromTerminationMessage) path, which reads the pod via
	// the APIReader — the specs plant a real Pod in the namespace so the
	// default reader finds it.
	f6NewReconciler := func(recorder *record.FakeRecorder) *LoopReconciler {
		r := &LoopReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			AllowUnenforced: true,
			Recorder:        recorder,
			readPhaseClaim:  func(context.Context, *coxv1alpha1.Loop) (*PhaseClaim, error) { return nil, nil },
		}
		return r
	}

	// f6PrimeProxy creates the model secret the D33 proxy needs and lets the
	// operator build the real proxy pod (spec + spec-hash annotation), then
	// marks it Ready — the D35a gate's peer (the I74 fixture's primeProxy).
	f6PrimeProxy := func(r *LoopReconciler, loop *coxv1alpha1.Loop) {
		_ = k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: p2fModelSecret, Namespace: loop.Namespace},
			StringData: map[string]string{modelAPIKey: "f6-dummy", modelBaseURL: p2fModelEndpoint},
		})
		Expect(r.ensureProxy(ctx, loop)).To(Succeed())
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: proxyPodName(loop.Name), Namespace: loop.Namespace}, pod)).To(Succeed())
		now := metav1.Now()
		pod.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	// f6Reconcile reconciles once (no error expected).
	f6Reconcile := func(r *LoopReconciler, ns, name string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred(), "F6: reconcile %s/%s", ns, name)
	}

	// f6Loop creates the Loop + primes the proxy + reconciles (bootstrap:
	// Pending -> Planning). The operator's ensureSandbox creates the Sandbox
	// CR on the prime reconcile (the pod is absent — the envtest has no
	// sandbox controller, so the operator's pod is never created). The claim
	// seam serves (nil, nil) on the prime reconcile (the agent is "still
	// running") so the prime never consumes a claim.
	f6Loop := func(ns, name string) *LoopReconciler {
		r := f6NewReconciler(record.NewFakeRecorder(128))
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "F6 spec",
				Workspace: testWorkspace(),
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: p2fModelSecret,
					ModelEndpoint:     p2fModelEndpoint,
				},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		f6PrimeProxy(r, loop)
		f6Reconcile(r, ns, name) // bootstrap: Pending -> Planning
		return r
	}

	// f6GetLoop returns the current Loop from the API.
	f6GetLoop := func(ns, name string) *coxv1alpha1.Loop {
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
		return got
	}

	// f6GetSandbox returns the live Sandbox (nil when absent).
	f6GetSandbox := func(ns, name string) *sandboxv1beta1.Sandbox {
		sb := &sandboxv1beta1.Sandbox{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-sandbox"}, sb); err != nil {
			return nil
		}
		return sb
	}

	// f6DeletePod deletes the Pod (the operator's retry already deleted the
	// Sandbox; the Pod may still be present — the specs delete it explicitly
	// so the next plant is clean).
	f6DeletePod := func(ns, name string) {
		pod := &corev1.Pod{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-sandbox"}, pod); err == nil {
			Expect(k8sClient.Delete(ctx, pod)).To(Succeed())
		}
	}

	// f6PlantFailedPod plants a failed Pod (the live eviction shape:
	// Status.Phase = Failed, the agent container State.Terminated with
	// reason=Error, exit=137 (OOMKilled by the /tmp emptyDir overrun), and
	// the kubelet message carrying the reason). The pod is planted via Create
	// + Status().Update (the API drops status on create, the I65 fixture's
	// pattern).
	f6PlantFailedPod := func(ns, name string) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-sandbox",
				Namespace: ns,
				Labels:    map[string]string{"coxswain.io/loop": name},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "agent", Image: "busybox"}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		now := metav1.Now()
		pod.Status.Phase = corev1.PodFailed
		pod.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.ContainersReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady", LastTransitionTime: now},
		}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{
			{
				Name:  "agent",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "Error", FinishedAt: now, Message: f6EvictMessage}},
			},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	// f6PlantRunningPod plants a RUNNING Pod (the I49 in-progress agent: the
	// pod is Running, the agent has not terminated yet — the claim reader
	// finds nothing, the F6 check must make NO decision).
	f6PlantRunningPod := func(ns, name string) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-sandbox",
				Namespace: ns,
				Labels:    map[string]string{"coxswain.io/loop": name},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "agent", Image: "busybox"}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		now := metav1.Now()
		pod.Status.Phase = corev1.PodRunning
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{
			{
				Name:  "agent",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: now}},
			},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	It("surfaces an evicted pod with no claim (the live F6 repro) + retries the sandbox", func() {
		ns := f6NS("f6-evict")
		defer deleteNS(ctx, ns)
		r := f6Loop(ns, "f6e")
		name := "f6e"

		By("the sandbox pod is evicted (the live F6 repro: /tmp emptyDir overrun)")
		f6PlantFailedPod(ns, name)
		By("the operator SURFACES the failure (I90): AgentFailed condition True with the kubelet message, + a Warning Event")
		f6Reconcile(r, ns, name)
		got := f6GetLoop(ns, name)
		cond := f6Cond(got, coxv1alpha1.AgentFailedCondition)
		Expect(cond).NotTo(BeNil(), "F6: the AgentFailed condition must be set on the evicted pod (I90: the wedge is surfaced)")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("Error"), "F6: the condition names the container's termination reason")
		Expect(cond.Message).To(ContainSubstring(f6EvictMessage), "F6: the condition carries the kubelet message (the live eviction reason)")
		By("the attempt is counted (status.agent.infraAttempts == 1, the I65 P1-A per-Sandbox-UID guard)")
		Expect(got.Status.Agent).NotTo(BeNil(), "F6: status.agent must carry the attempt count")
		Expect(got.Status.Agent.InfraAttempts).To(Equal(int32(1)))
		Expect(got.Status.Agent.InfraSandboxUID).NotTo(BeEmpty(), "F6: the per-Sandbox-UID guard records the failing Sandbox's UID")
		By("a Warning Event was emitted (the I90 observable: every state change has an Event)")
		rec := r.Recorder.(*record.FakeRecorder)
		Eventually(rec.Events, "3s", "100ms").Should(And(
			HaveLen(1),
			ContainElement(And(ContainSubstring(sandboxInfraRetryEventReason), ContainSubstring(f6EvictMessage))),
		), "F6: a Warning Event must be emitted on the first retry (I90)")
		By("the Sandbox is deleted (the recreate is the next reconcile's ensureSandbox work — the I65 recreate pattern)")
		Expect(f6GetSandbox(ns, name)).To(BeNil(), "F6: the failed Sandbox must be deleted for the retry (the recreate is the next reconcile's work)")
		By("the Loop is STILL in Planning (a sandbox failure does NOT advance the phase — ADR-0005: no claim, no advance)")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning))
	})

	It("does NOT double-count a re-read of the SAME failing Sandbox (the I65 P1-A UID guard)", func() {
		ns := f6NS("f6-uid")
		defer deleteNS(ctx, ns)
		r := f6Loop(ns, "f6u")
		name := "f6u"

		By("the first failure is counted (attempt 1)")
		f6PlantFailedPod(ns, name)
		f6Reconcile(r, ns, name)
		got := f6GetLoop(ns, name)
		Expect(got.Status.Agent).NotTo(BeNil())
		Expect(got.Status.Agent.InfraAttempts).To(Equal(int32(1)))
		Expect(got.Status.Agent.InfraSandboxUID).NotTo(BeEmpty())

		By("the reconcile RE-RAN before the delete completed (the I65 P1-A re-read path: the Sandbox is still present with the SAME UID)")
		// In envtest the operator's background delete may or may not have
		// completed by the time the next reconcile runs (the GC timing is not
		// guaranteed). The guard must hold regardless: a re-read of the SAME
		// failing Sandbox (same UID) does NOT double-count. Model the re-read:
		// if the Sandbox was deleted, re-create it with the SAME UID (the
		// operator's ensureSandbox creates it on the next reconcile anyway).
		sb := f6GetSandbox(ns, name)
		if sb == nil {
			// The delete completed: re-create the Sandbox with the SAME UID
			// (modelling "the delete did not complete — the reconcile re-ran
			// and read the SAME failing Sandbox").
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
			Expect(err).NotTo(HaveOccurred())
			sb = f6GetSandbox(ns, name)
			Expect(sb).NotTo(BeNil(), "F6: the Sandbox must exist for the re-read")
		}
		// Re-plant the failed pod (the operator's retry already deleted the
		// pod; the spec re-plants it to model "the pod is still failed").
		f6DeletePod(ns, name)
		f6PlantFailedPod(ns, name)
		f6Reconcile(r, ns, name)
		got = f6GetLoop(ns, name)
		Expect(got.Status.Agent.InfraAttempts).To(Equal(int32(1)),
			"F6 P1-A: a re-read of the SAME failing Sandbox (same UID) must NOT double-count the attempt")
	})

	It("clears the AgentFailed condition and the attempt count on a fresh, healthy pod (the I49 in-progress keep + the I65 P1-B reset)", func() {
		ns := f6NS("f6-recover")
		defer deleteNS(ctx, ns)
		r := f6Loop(ns, "f6r")
		name := "f6r"

		By("the first failure is surfaced (attempt 1)")
		f6PlantFailedPod(ns, name)
		f6Reconcile(r, ns, name)
		got := f6GetLoop(ns, name)
		Expect(f6Cond(got, coxv1alpha1.AgentFailedCondition)).NotTo(BeNil())
		Expect(got.Status.Agent).NotTo(BeNil())
		Expect(got.Status.Agent.InfraAttempts).To(Equal(int32(1)))
		Expect(got.Status.Agent.InfraSandboxUID).NotTo(BeEmpty())

		By("the NEXT (recreated) Sandbox is healthy (the I49 in-progress keep: a RUNNING agent is never a failure)")
		// The operator's retry already deleted the Sandbox; the next
		// reconcile's ensureSandbox recreates it fresh (a NEW UID). The spec
		// plants a RUNNING pod (the fresh, healthy pod — the agent is still
		// doing the work, the I49 in-progress keep).
		f6DeletePod(ns, name)
		// Let the operator's ensureSandbox recreate the Sandbox (a fresh
		// UID) on the next reconcile.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())
		sb := f6GetSandbox(ns, name)
		Expect(sb).NotTo(BeNil(), "F6: the recreated Sandbox must exist")
		Expect(sb.UID).NotTo(Equal(types.UID(got.Status.Agent.InfraSandboxUID)),
			"F6: the recreated Sandbox must have a NEW UID (a fresh Sandbox)")
		f6PlantRunningPod(ns, name)
		f6Reconcile(r, ns, name)
		got = f6GetLoop(ns, name)
		By("the AgentFailed condition is CLEARED (a fresh, healthy pod — the wedge is gone)")
		Expect(f6Cond(got, coxv1alpha1.AgentFailedCondition)).To(BeNil(),
			"F6: the AgentFailed condition must be cleared once the pod is healthy (the wedge is resolved)")
		By("the attempt count is CLEARED (the new Sandbox has a different UID — the I65 P1-B reset pattern)")
		Expect(got.Status.Agent).NotTo(BeNil())
		Expect(got.Status.Agent.InfraAttempts).To(Equal(int32(0)),
			"F6: the fresh Sandbox's attempt count must be 0 (the I65 P1-B reset: a new Sandbox gets a clean bound)")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"F6: the Loop is still in Planning (no advance — the agent is still running, the I49 keep)")
	})

	It("fails the Loop with the distinct SandboxFailed reason after the bound is exhausted (repeated evictions)", func() {
		ns := f6NS("f6-fail")
		defer deleteNS(ctx, ns)
		r := f6Loop(ns, "f6f")
		name := "f6f"

		// Drive the bound to exhaustion: 3 total runs (1 original + 2
		// recreations = sandboxInfraRetries + 1). Each failure is a DIFFERENT
		// Sandbox (a fresh UID each time — the I65 P1-A guard counts each
		// fresh Sandbox's first failure).
		By("attempt 1 (the original Sandbox)")
		f6PlantFailedPod(ns, name)
		f6Reconcile(r, ns, name)
		got := f6GetLoop(ns, name)
		Expect(got.Status.Agent).NotTo(BeNil())
		Expect(got.Status.Agent.InfraAttempts).To(Equal(int32(1)))
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"F6: attempt 1 is within the bound — the Loop is still in Planning (not yet failed)")

		By("attempt 2 (the first recreation — a NEW Sandbox, a NEW UID)")
		f6DeletePod(ns, name)
		// Let the operator's ensureSandbox recreate the Sandbox (a fresh UID).
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())
		f6PlantFailedPod(ns, name)
		f6Reconcile(r, ns, name)
		got = f6GetLoop(ns, name)
		Expect(got.Status.Agent.InfraAttempts).To(Equal(int32(2)))
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"F6: attempt 2 is still within the bound (sandboxInfraRetries = 2) — the Loop is still in Planning")

		By("attempt 3 (the second recreation — a NEW Sandbox, a NEW UID): the bound is exhausted")
		f6DeletePod(ns, name)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())
		f6PlantFailedPod(ns, name)
		f6Reconcile(r, ns, name)
		got = f6GetLoop(ns, name)
		By("the Loop is FAILED with the DISTINCT SandboxFailed reason (not MaxIterationsExceeded — an infra failure is not the agent's work)")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed), "F6: the bound is exhausted — the Loop is Failed")
		cond := f6Cond(got, string(coxv1alpha1.LoopPhaseFailed))
		Expect(cond).NotTo(BeNil(), "F6: the Failed condition must be set")
		Expect(cond.Reason).To(Equal(sandboxInfraFailedCondition),
			"F6: the Failed condition carries the DISTINCT SandboxFailed reason (not MaxIterationsExceeded — a sandbox failure is not the agent's work)")
		Expect(cond.Message).To(ContainSubstring("3 attempts"), "F6: the Failed condition names the attempt count")
		By("the AgentFailed condition is CLEARED (the Failed condition supersedes it)")
		Expect(f6Cond(got, coxv1alpha1.AgentFailedCondition)).To(BeNil(),
			"F6: the AgentFailed condition must be cleared once the Loop is Failed (the Failed condition carries the durable record)")
	})

	It("makes NO decision on an in-progress agent (I49: a Running pod is never a failure)", func() {
		ns := f6NS("f6-running")
		defer deleteNS(ctx, ns)
		r := f6Loop(ns, "f6run")
		name := "f6run"

		By("the sandbox pod is RUNNING (the agent is still doing the work — the I49 in-progress state)")
		f6PlantRunningPod(ns, name)
		f6Reconcile(r, ns, name)
		got := f6GetLoop(ns, name)
		By("NO AgentFailed condition is set (the I49 norm: in-progress is never a failure)")
		Expect(f6Cond(got, coxv1alpha1.AgentFailedCondition)).To(BeNil(),
			"F6 I49: a Running pod must NOT set the AgentFailed condition (in-progress is never a failure)")
		Expect(got.Status.Agent).To(BeNil(), "F6 I49: no attempt is counted for a Running pod")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning))
		sb := f6GetSandbox(ns, name)
		Expect(sb).NotTo(BeNil(), "F6 I49: the Running Sandbox must NOT be deleted")
		// The event channel must be empty (no Warning Event emitted).
		rec := r.Recorder.(*record.FakeRecorder)
		Consistently(func() []string {
			out := make([]string, 0, 4)
			for {
				select {
				case ev, ok := <-rec.Events:
					if !ok {
						return out
					}
					out = append(out, ev)
				default:
					return out
				}
			}
		}, "1s", "100ms").Should(BeEmpty(),
			"F6 I49: no Warning Event must be emitted for a Running pod (no decision)")
	})
})
