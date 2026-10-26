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

// F56 (review-startup F2): `make deploy` passed none of the four component-image
// args, so every deployed Loop ran the operator's `sh -c sleep infinity`
// stand-in (the runner image was unset, so isRunner is false) and looked Running
// while never advancing. Nothing on the object said so. These specs pin the
// RunnerAvailable condition that makes it visible, and the pod command the
// condition describes — the two must agree (runnerRuns is shared).
package controller

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("F56: a missing runner image is visible on the Loop", func() {
	ctx := context.Background()

	f56Ns := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	f56Loop := func(ns, name string, agentImage string) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
			},
		}
		loop.Spec.Agent.Image = agentImage
		return loop
	}

	f56Reconcile := func(r *LoopReconciler, ns, name string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())
	}

	f56Get := func(ns, name string) *coxv1alpha1.Loop {
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, got)).To(Succeed())
		return got
	}

	f56Condition := func(loop *coxv1alpha1.Loop) *metav1.Condition {
		for i := range loop.Status.Conditions {
			if loop.Status.Conditions[i].Type == coxv1alpha1.RunnerAvailableCondition {
				return &loop.Status.Conditions[i]
			}
		}
		return nil
	}

	// f56AgentContainer reads the sandbox's agent container (the pod command the
	// condition describes).
	f56AgentContainer := func(ns, name string) corev1.Container {
		sbx := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name + "-sandbox"}, sbx)).To(Succeed())
		for _, c := range sbx.Spec.PodTemplate.Spec.Containers {
			if c.Name == "agent" {
				return c
			}
		}
		Fail("the sandbox has no agent container")
		return corev1.Container{}
	}

	f56Drain := func(recorder *record.FakeRecorder) []string {
		out := []string{}
		for {
			select {
			case e := <-recorder.Events:
				out = append(out, e)
			default:
				return out
			}
		}
	}

	f56Unavailable := func(events []string) []string {
		out := []string{}
		for _, e := range events {
			if strings.Contains(e, RunnerUnavailableEventReason) {
				out = append(out, e)
			}
		}
		return out
	}

	It("S1: an empty spec.agent.image with no --runner-image is False/RunnerImageUnset, and the pod runs sleep infinity", func() {
		ns := f56Ns("f56-norunner")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{
			CNIProber:       testCNIProber(),
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			apiReader:       k8sClient,
			Recorder:        recorder,
			RunnerImage:     "",
			AllowUnenforced: true, // the D30 gate is not what this spec is about
		}

		name := "l1"
		Expect(k8sClient.Create(ctx, f56Loop(ns, name, ""))).To(Succeed())
		f56Reconcile(r, ns, name)

		cond := f56Condition(f56Get(ns, name))
		Expect(cond).NotTo(BeNil(), "the Loop must say the runner is missing (F56: before, nothing did)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("RunnerImageUnset"))
		Expect(cond.Message).To(ContainSubstring("no --runner-image is set"))
		Expect(cond.Message).To(ContainSubstring("sleep infinity"))

		By("the condition describes what the pod actually runs")
		agent := f56AgentContainer(ns, name)
		Expect(agent.Command).To(Equal([]string{"sh", "-c", "sleep infinity"}),
			"the stand-in command is what makes the Loop never advance")
		Expect(agent.Image).To(Equal(r.sandboxImage()))

		By("one Warning Event on the transition, not one per reconcile")
		events := f56Unavailable(f56Drain(recorder))
		Expect(events).To(HaveLen(1), "expected exactly one RunnerUnavailable Event, got %v", events)
		Expect(events[0]).To(ContainSubstring(corev1.EventTypeWarning))
		f56Reconcile(r, ns, name)
		Expect(f56Unavailable(f56Drain(recorder))).To(BeEmpty(), "the Event is gated on the transition")

		By("it never fails or terminalises the Loop")
		Expect(f56Get(ns, name).Status.Phase).NotTo(Or(Equal(coxv1alpha1.LoopPhaseFailed), Equal(coxv1alpha1.LoopPhaseSucceeded)))
	})

	It("S2: same Loop, --runner-image set: True/RunnerImageSet and the runner entrypoint", func() {
		ns := f56Ns("f56-runner")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{
			CNIProber:       testCNIProber(),
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			apiReader:       k8sClient,
			Recorder:        recorder,
			RunnerImage:     "example.test/coxswain-runner:test",
			AllowUnenforced: true,
		}

		name := "l2"
		Expect(k8sClient.Create(ctx, f56Loop(ns, name, ""))).To(Succeed())
		f56Reconcile(r, ns, name)

		cond := f56Condition(f56Get(ns, name))
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("RunnerImageSet"))
		Expect(cond.Message).To(ContainSubstring(r.RunnerImage))

		agent := f56AgentContainer(ns, name)
		Expect(agent.Image).To(Equal(r.RunnerImage))
		Expect(agent.Command).To(Equal([]string{
			"/usr/local/bin/runner",
			"-extra-body",
			`{"chat_template_kwargs":{"enable_thinking":false}}`,
		}))
		Expect(f56Unavailable(f56Drain(recorder))).To(BeEmpty(), "no Event when the runner is available")
	})

	It("S3: a pinned non-runner spec.agent.image is True/AgentImagePinned, not a degradation", func() {
		ns := f56Ns("f56-pinned")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{
			CNIProber:       testCNIProber(),
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			apiReader:       k8sClient,
			Recorder:        recorder,
			RunnerImage:     "",
			AllowUnenforced: true,
		}

		name := "l3"
		Expect(k8sClient.Create(ctx, f56Loop(ns, name, "docker.io/library/busybox:1.36"))).To(Succeed())
		f56Reconcile(r, ns, name)

		cond := f56Condition(f56Get(ns, name))
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue),
			"a pinned image is the documented debugging opt-out, not a missing runner")
		Expect(cond.Reason).To(Equal("AgentImagePinned"))
		Expect(cond.Message).To(ContainSubstring("docker.io/library/busybox:1.36"))

		By("the pinned image still gets the stand-in command (the documented opt-out)")
		Expect(f56AgentContainer(ns, name).Command).To(Equal([]string{"sh", "-c", "sleep infinity"}))
		Expect(f56Unavailable(f56Drain(recorder))).To(BeEmpty())

		By("WITH a runner image available, a pinned non-runner image is still the pinned image")
		// The predicate must consult spec.agent.image even when --runner-image is
		// set: otherwise a pinned image would be handed the runner's entrypoint
		// (a binary it does not contain). Found by a mutation that made
		// runnerRuns ignore spec.agent.image — the first version of this spec
		// left RunnerImage empty and did not catch it.
		r.RunnerImage = "example.test/coxswain-runner:test"
		name = "l3pinned"
		Expect(k8sClient.Create(ctx, f56Loop(ns, name, "docker.io/library/busybox:1.36"))).To(Succeed())
		f56Reconcile(r, ns, name)

		pinned := f56Condition(f56Get(ns, name))
		Expect(pinned).NotTo(BeNil())
		Expect(pinned.Status).To(Equal(metav1.ConditionTrue))
		Expect(pinned.Reason).To(Equal("AgentImagePinned"))
		Expect(f56AgentContainer(ns, name).Command).To(Equal([]string{"sh", "-c", "sleep infinity"}),
			"a pinned image must keep the stand-in even when --runner-image is set")
		Expect(f56Unavailable(f56Drain(recorder))).To(BeEmpty())
	})

	It("S4: the condition CLEARS on the same Loop once the runner image is set (same-Loop update)", func() {
		ns := f56Ns("f56-clear")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		recorder := record.NewFakeRecorder(64)
		r := &LoopReconciler{
			CNIProber:       testCNIProber(),
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			apiReader:       k8sClient,
			Recorder:        recorder,
			RunnerImage:     "",
			AllowUnenforced: true,
		}

		name := "l4"
		Expect(k8sClient.Create(ctx, f56Loop(ns, name, ""))).To(Succeed())
		f56Reconcile(r, ns, name)
		Expect(f56Condition(f56Get(ns, name)).Status).To(Equal(metav1.ConditionFalse))
		Expect(f56Unavailable(f56Drain(recorder))).To(HaveLen(1))

		By("the same reconciler, now with a runner image, re-reconciles the same Loop")
		r.RunnerImage = "example.test/coxswain-runner:test"
		f56Reconcile(r, ns, name)

		loop := f56Get(ns, name)
		cond := f56Condition(loop)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue), "the condition must clear, not stick (AGENTS same-Loop update)")
		Expect(cond.Reason).To(Equal("RunnerImageSet"))
		Expect(f56Unavailable(f56Drain(recorder))).To(BeEmpty(), "clearing is not a degraded transition")
	})
})
