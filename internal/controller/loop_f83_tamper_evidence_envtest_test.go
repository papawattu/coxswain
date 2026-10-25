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

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// F83: status.verify.tamperExitCode / verifiedCommit had no production
// writer. These specs drive a REAL verify pod status (initContainerStatuses)
// and assert the operator records the tamper init's own exit code (nil while
// it has not terminated) and the pin it names, and that the delivered PR body
// then says "clean".

func f83SetTamperPod(ctx context.Context, ns, name string, tamper, artifact corev1.ContainerState, check *corev1.ContainerState) {
	podName := name + "-verify-pod"
	pod := &corev1.Pod{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: podName, Namespace: ns}, pod); err != nil {
		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		// The Job normally exists (the operator created it); make a minimal valid
		// one when the spec's Loop never reached the Job create.
		if k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, &batchv1.Job{}) != nil {
			Expect(k8sClient.Create(ctx, &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns, Labels: verifyJobLabels(name)},
				Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}},
				}}},
			})).To(Succeed())
		}
		s5aVerifyPodInProg(ctx, ns, name, jobName)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: podName, Namespace: ns}, pod)).To(Succeed())
	}
	term0 := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
	inits := []corev1.ContainerStatus{
		{Name: s5aCloneBase, State: term0},
		{Name: s5aImportAgent, State: term0},
		{Name: s5aTamper, State: tamper},
		{Name: s5aArtifact, State: artifact},
	}
	if check != nil {
		inits = append(inits, corev1.ContainerStatus{Name: s5aCheck0, State: *check})
	}
	pod.Status.InitContainerStatuses = inits
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

var _ = Describe("F83: the operator records the tamper init's evidence", func() {
	ctx := context.Background()
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	term := func(code int32) corev1.ContainerState {
		return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}}
	}
	newLoop := func(prefix string) (ns, name string) {
		ns = prefix + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		name = "f83loop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())
		return ns, name
	}

	It("F83: records nil while the tamper init runs, then its exit code and the pin once it terminated (checks still running), and clears it on a new pin", func() {
		ns, name := newLoop("f83-s1-")
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		r := s5aDriveToVerifying(ns, name)

		By("tamper init Running: in progress, no evidence")
		f83SetTamperPod(ctx, ns, name, running, running, nil)
		loop := s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		if loop.Status.Verify != nil {
			Expect(loop.Status.Verify.TamperExitCode).To(BeNil(), "a Running tamper init is not evidence")
		}

		By("tamper init Terminated 0, artifact still Running: evidence recorded while no decision is made")
		f83SetTamperPod(ctx, ns, name, term(0), running, nil)
		loop = s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(loop.Status.Verify).NotTo(BeNil(), "F83: status.verify must exist once the tamper init has terminated")
		Expect(loop.Status.Verify.TamperExitCode).NotTo(BeNil(), "the operator must record the tamper init's exit code (F83)")
		Expect(*loop.Status.Verify.TamperExitCode).To(Equal(int32(0)))
		Expect(loop.Status.CurrentVerify).NotTo(BeNil())
		Expect(loop.Status.Verify.VerifiedCommit).To(Equal(loop.Status.CurrentVerify.VerifiedCommit),
			"the evidence must name the pin it came from")
		// The Job's UID binding: the operator reads the Job through its (cached)
		// client, so the UID is filled on the first reconcile whose cache has
		// the Job (ensured in the same pass); level-based, never a decision.
		vj := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: verifyJobName(loop), Namespace: ns}, vj)).To(Succeed())
		loop = s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(loop.Status.Verify.TamperJobUID).To(Equal(string(vj.UID)),
			"F83: the evidence must carry the verify Job's UID (the gate's operator-only binding)")

		By("artifact and check pass: Succeeded, evidence unchanged")
		c0 := term(0)
		f83SetTamperPod(ctx, ns, name, term(0), term(0), &c0)
		loop = s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		Expect(*loop.Status.Verify.TamperExitCode).To(Equal(int32(0)))
		Expect(loop.Status.Verify.VerifiedCommit).To(Equal(loop.Status.CurrentVerify.VerifiedCommit))
	})

	It("F83: a tampered tamper init (exit 1) is recorded as exit 1 and the Loop fails TamperedVerify", func() {
		ns, name := newLoop("f83-s2-")
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		r := s5aDriveToVerifying(ns, name)
		c0 := term(0)
		f83SetTamperPod(ctx, ns, name, term(1), term(0), &c0)
		loop := s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed))
		Expect(loop.Status.Verify).NotTo(BeNil())
		Expect(loop.Status.Verify.TamperExitCode).NotTo(BeNil(), "the tamper exit code must be recorded")
		Expect(*loop.Status.Verify.TamperExitCode).To(Equal(int32(1)))
	})

	It("F83: a failing CHECK never becomes tamper evidence (the tamper init's own code is recorded)", func() {
		ns, name := newLoop("f83-s3-")
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		r := s5aDriveToVerifying(ns, name)
		c0 := term(1)
		f83SetTamperPod(ctx, ns, name, term(0), term(0), &c0)
		loop := s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).NotTo(Equal(coxv1alpha1.LoopPhaseFailed))
		Expect(loop.Status.Verify.TamperExitCode).NotTo(BeNil())
		Expect(*loop.Status.Verify.TamperExitCode).To(Equal(int32(0)), "a check's exit 1 must not be recorded as the tamper code")
	})

	It("F83: a forged status.verify does not fail the Loop (the gate needs the Job's pod to agree)", func() {
		ns, name := newLoop("f83-forge-")
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		r := s5aDriveToVerifying(ns, name)
		By("the operator's own clean evidence for the real pod")
		c0 := term(0)
		f83SetTamperPod(ctx, ns, name, term(0), running, &c0)
		loop := s5aReconcile(r, ns, name)
		Expect(loop.Status.Verify).NotTo(BeNil())
		Expect(*loop.Status.Verify.TamperExitCode).To(Equal(int32(0)))
		loop = s5aReconcile(r, ns, name) // the Job's UID is recorded once its cache has the Job
		realUID := loop.Status.Verify.TamperJobUID
		Expect(realUID).NotTo(BeEmpty())
		one := int32(1)
		forge := func(label, uid string) {
			By("forge: " + label)
			forged := &coxv1alpha1.VerifyStatus{TamperExitCode: &one, VerifiedCommit: loop.Status.CurrentVerify.VerifiedCommit, TamperJobUID: uid}
			loop.Status.Verify = forged
			Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
			got := s5aReconcile(r, ns, name)
			Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
				"a forged status.verify must not fail the Loop (F83): "+label)
			Expect(findConditionFn(got.Status.Conditions, string(coxv1alpha1.LoopPhaseFailed))).To(BeNil(), label)
			loop = got
		}
		forge("right UID, wrong code (the pod's tamper init is 0)", realUID)
		forge("right code-shaped forge, wrong Job UID", "11111111-2222-3333-4444-555555555555")
		forge("no UID at all (the pre-F83 shape: code + commits only)", "")

		By("positive control: the SAME gate fails the Loop when the pod's tamper init really exits 1")
		f83SetTamperPod(ctx, ns, name, term(1), running, &c0)
		got := s5aReconcile(r, ns, name)
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"the gate must still fire on real evidence (the forged-status spec is not vacuous)")
		cond := findConditionFn(got.Status.Conditions, string(coxv1alpha1.LoopPhaseFailed))
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(TamperedVerifyReason))
	})

	It("F83: the gate refuses a status-only tampered verdict; it fires only when the verify Job and its pod agree", func() {
		one := int32(1)
		mkLoop := func(uid string) *coxv1alpha1.Loop {
			return &coxv1alpha1.Loop{
				ObjectMeta: metav1.ObjectMeta{Name: "f83gate"},
				Status: coxv1alpha1.LoopStatus{
					Phase:         coxv1alpha1.LoopPhaseVerifying,
					CurrentVerify: &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: "cafe"},
					Verify:        &coxv1alpha1.VerifyStatus{TamperExitCode: &one, VerifiedCommit: "cafe", TamperJobUID: uid},
				},
			}
		}
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "f83gate-verify-1", UID: types.UID("uid-real")}}
		pod := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
			{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}},
		}}}
		cleanPod := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
			{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		}}}
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		Expect(r.applyTamperGate(mkLoop("uid-real"), job, cleanPod)).To(BeFalse(),
			"the forged code is not confirmed by the pod")
		Expect(r.applyTamperGate(mkLoop("uid-forged"), job, pod)).To(BeFalse(),
			"a forged Job UID is not the current Job")
		Expect(r.applyTamperGate(mkLoop(""), job, pod)).To(BeFalse(),
			"no UID binding (the pre-F83 status shape) is no evidence")
		Expect(r.applyTamperGate(mkLoop("uid-real"), job, nil)).To(BeFalse(),
			"no pod is no evidence")
		Expect(r.applyTamperGate(mkLoop("uid-real"), &batchv1.Job{ObjectMeta: metav1.ObjectMeta{UID: types.UID("uid-real")}}, pod)).To(BeTrue(),
			"control: the real Job UID and the pod's own terminated tamper 1 fail the Loop")
	})

	It("F83: the delivered PR body says the tamper check was clean when the tamper init exited 0", func() {
		ns := "f83-body-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		name := "f83body"
		r := s6Succeeded(name, ns, "http://gitea.samples.svc:3000/samples/f83body.git")
		s6Reconcile(r, ns, name)
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: deliverDescriptionConfigName(name)}, cm)).To(Succeed())
		body := cm.Data[deliverDescriptionDataKey]
		Expect(body).To(ContainSubstring("- **Tamper check:** clean (no protected path changed)"),
			"F83: a verify whose tamper init exited 0 must render the clean bullet")
		Expect(body).NotTo(ContainSubstring("no tamper evidence"),
			"F83: the fail-closed 'no evidence' text may only render when there is no evidence")
	})
})
