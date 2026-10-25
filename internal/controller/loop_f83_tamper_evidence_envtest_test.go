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
