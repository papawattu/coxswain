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
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// S5a (B3, GAP 4): the verify Job — the Verifying evidence path. In envtest
// (no Job controller) the specs:
//
//   - drive a Loop to Verifying through the claim path (Planning +
//     Implementing claims with a 40-hex headCommit — the operator pins
//     status.currentVerify.verifiedCommit from it),
//   - assert the verify Job's SPEC shape (init container order, no
//     credentials next to the agent tree, read-only PVC, backoffLimit 0,
//     restartPolicy Never),
//   - set the verify Job pod's initContainerStatuses exit codes directly and
//     assert the operator's outcome mapping (Succeeded / iterate /
//     TamperedVerify).
//
// The claim's headCommit is the operator's pin source (ADR-0005 claim —
// strictly 40-hex-validated); a malformed headCommit is a malformed claim
// (never advanced on).

const s5aHeadCommit = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2" // 40-hex

// s5aLoopSpec is a Loop with acceptance checks (so the verify Job's check
// inits are non-empty) + a workspace repo (so baseCommit/clone shape apply).
func s5aLoopSpec() coxv1alpha1.LoopSpec {
	return coxv1alpha1.LoopSpec{
		Goal:      loopGoal,
		Workspace: testWorkspace(),
		Verify: coxv1alpha1.VerifyConfig{
			AcceptanceChecks: []string{loopCheckCmd},
		},
	}
}

// s5aReconcile runs one Reconcile and returns the fresh Loop.
func s5aReconcile(r *LoopReconciler, ns, name string) *coxv1alpha1.Loop {
	_, err := r.Reconcile(context.Background(),
		reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
	Expect(err).NotTo(HaveOccurred())
	loop := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	return loop
}

// s5aDriveToVerifying drives a fresh Loop (ns/name already created) through
// the claim path to Verifying: a Planning claim, then an Implementing claim
// carrying the 40-hex headCommit (the operator pins it on the advance).
// Returns the reconciler (with the readPhaseClaim seam reset to nil).
func s5aDriveToVerifying(ns, name, headCommit string) *LoopReconciler {
	ctx := context.Background()
	r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
	// A stand-in sandbox object (envtest has no agent-sandbox controller).
	ensure := func() {
		sb := &sandboxv1beta1.Sandbox{}
		if apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)) {
			_ = k8sClient.Create(ctx, &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
			})
		}
	}
	ensure()
	// Bootstrap: Pending -> Planning on the first reconcile.
	s5aReconcile(r, ns, name)
	// Stand-in sandbox pod whose agent container carries the claim (the S4
	// claim-reader path reads the agent termination message via the
	// APIReader).
	claimPod := func(phase, head string) {
		pod := &corev1.Pod{}
		exists := k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, pod)
		if !apierrors.IsNotFound(exists) {
			Expect(k8sClient.Delete(ctx, pod)).To(Succeed())
		}
		pod = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: agentContainerName, Image: s3StandinImage}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		msg := `{"observedPhase":"` + phase + `","status":"success","blockedReason":""`
		if head != "" {
			msg += `,"headCommit":"` + head + `"`
		}
		msg += `}`
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{
			{Name: agentContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0, Message: msg}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}
	// Planning claim (the runner executed Planning).
	claimPod("Planning", "")
	s5aReconcile(r, ns, name)
	// The operator's annotation-based recycle deleted the Sandbox on the
	// advance; re-ensure it before the next claim.
	ensure()
	// Implementing claim WITH the headCommit (the operator pins it on the
	// Implementing -> Verifying advance).
	claimPod("Implementing", headCommit)
	s5aReconcile(r, ns, name)
	return r
}

// s5aFindVerifyPod finds the verify Job's pod by its job-name label.
func s5aFindVerifyPod(ns, loopName string) *corev1.Pod {
	ctx := context.Background()
	list := &corev1.PodList{}
	Expect(k8sClient.List(ctx, list,
		client.InNamespace(ns),
		client.MatchingLabels{"job-name": verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: loopName, Namespace: ns}})})).To(Succeed())
	Expect(list.Items).To(HaveLen(1))
	return &list.Items[0]
}

// s5aSetInitExit sets the named init container's terminated exit code on the
// verify pod's status (the envtest stand-in for the kubelet).
func s5aSetInitExit(pod *corev1.Pod, name string, code int32) {
	ctx := context.Background()
	var ics *corev1.ContainerStatus
	for i := range pod.Status.InitContainerStatuses {
		if pod.Status.InitContainerStatuses[i].Name == name {
			ics = &pod.Status.InitContainerStatuses[i]
		}
	}
	if ics == nil {
		pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses,
			corev1.ContainerStatus{Name: name})
		ics = &pod.Status.InitContainerStatuses[len(pod.Status.InitContainerStatuses)-1]
	}
	ics.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

var _ = Describe("S5a: verify Job (B3 Verifying evidence)", func() {
	ctx := context.Background()

	It("pins status.currentVerify.verifiedCommit from a valid headCommit and creates the verify Job", func() {
		ns := "s5a-pin-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "pinloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name, s5aHeadCommit)
		loop := s5aReconcile(r, ns, name)

		By("pinning the current verified commit from the claim's headCommit (D11)")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(loop.Status.CurrentVerify).NotTo(BeNil())
		Expect(loop.Status.CurrentVerify.VerifiedCommit).To(Equal(s5aHeadCommit),
			"the operator pins the runner's 40-hex headCommit to status.currentVerify.verifiedCommit on the advance")

		By("creating the verify Job (owned by the Loop, named by iteration)")
		jobName := fmt.Sprintf("%s-verify-1", name)
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())
		Expect(job.OwnerReferences).To(HaveLen(1))
		Expect(job.OwnerReferences[0].Name).To(Equal(name))
		Expect(job.OwnerReferences[0].Kind).To(Equal("Loop"))

		By("shaping the Job per B3 (restartPolicy Never, backoffLimit 0)")
		Expect(job.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
		Expect(job.Spec.BackoffLimit).NotTo(BeNil())
		Expect(*job.Spec.BackoffLimit).To(Equal(int32(0)))

		By("ordering the init containers: clone-base, import-agent, tamper, then the checks")
		inits := job.Spec.Template.Spec.InitContainers
		Expect(inits).To(HaveLen(4), "clone + import + tamper + 1 check")
		Expect(inits[0].Name).To(Equal("clone-base"))
		Expect(inits[1].Name).To(Equal("import-agent"))
		Expect(inits[2].Name).To(Equal("tamper"))
		Expect(inits[3].Name).To(Equal("check-0"))
		Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
		Expect(job.Spec.Template.Spec.Containers[0].Name).To(Equal("noop"))

		By("mounting NO credentials into import-agent (only clone-base may carry the git secret)")
		for i := range inits {
			if inits[i].Name == "import-agent" {
				for _, vm := range inits[i].VolumeMounts {
					Expect(vm.Name).NotTo(Equal("git-cred"), "import-agent must never mount the credential")
				}
			}
		}

		By("mounting the agent's workspace PVC read-only into import-agent (B3c)")
		for _, vm := range inits[1].VolumeMounts {
			if vm.Name == "agent-workspace" {
				Expect(vm.ReadOnly).To(BeTrue(), "the agent workspace must be read-only in the verify Job")
			}
		}
		foundPVC := false
		for _, v := range job.Spec.Template.Spec.Volumes {
			if v.Name == "agent-workspace" && v.PersistentVolumeClaim != nil {
				foundPVC = true
				Expect(v.PersistentVolumeClaim.ReadOnly).To(BeTrue())
			}
		}
		Expect(foundPVC).To(BeTrue())

		By("hardening every container (non-root, read-only rootfs, no privilege escalation)")
		for i := range inits {
			sc := inits[i].SecurityContext
			Expect(sc).NotTo(BeNil())
			Expect(*sc.RunAsUser).To(Equal(int64(65532)))
			Expect(*sc.ReadOnlyRootFilesystem).To(BeTrue())
			Expect(*sc.AllowPrivilegeEscalation).To(BeFalse())
		}
	})

	It("does NOT advance to Verifying on a malformed headCommit (strict 40-hex, ADR-0005)", func() {
		ns := "s5a-badhead-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "badheadloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		// Drive to the Implementing phase (Planning claim), then a malformed
		// headCommit on the Implementing claim.
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		_ = r
		// Bootstrap + Planning claim.
		ensureSB := func() {
			sb := &sandboxv1beta1.Sandbox{}
			if apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)) {
				_ = k8sClient.Create(ctx, &sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
				})
			}
		}
		ensureSB()
		s5aReconcile(r, ns, name)
		claimPod := func(phase, head string) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: agentContainerName, Image: s3StandinImage}}},
			}
			_ = k8sClient.Create(ctx, pod)
			msg := `{"observedPhase":"` + phase + `","status":"success","blockedReason":""`
			if head != "" {
				msg += `,"headCommit":"` + head + `"`
			}
			msg += `}`
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{
				{Name: agentContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0, Message: msg}}},
			}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		}
		claimPod("Planning", "")
		s5aReconcile(r, ns, name)
		ensureSB()
		// Malformed headCommit (uppercase + 41 chars): the strict parser
		// rejects the claim, so the advance is never taken.
		claimPod("Implementing", strings.ToUpper(s5aHeadCommit)+"0")
		loop := s5aReconcile(r, ns, name)

		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"a malformed headCommit must NOT advance the machine (no evidence, no advance)")
		Expect(loop.Status.CurrentVerify).To(BeNil(), "no pin on a malformed claim")
	})

	It("does NOT advance to Verifying on a success claim with NO headCommit (B3 MVP gate)", func() {
		ns := "s5a-nohead-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "noheadloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		sb := &sandboxv1beta1.Sandbox{}
		if apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)) {
			Expect(k8sClient.Create(ctx, &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
			})).To(Succeed())
		}
		s5aReconcile(r, ns, name)
		claimPod := func(phase, head string) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: agentContainerName, Image: s3StandinImage}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			msg := `{"observedPhase":"` + phase + `","status":"success","blockedReason":""`
			if head != "" {
				msg += `,"headCommit":"` + head + `"`
			}
			msg += `}`
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{
				{Name: agentContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0, Message: msg}}},
			}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		}
		claimPod("Planning", "")
		s5aReconcile(r, ns, name)
		sb = &sandboxv1beta1.Sandbox{}
		if apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)) {
			Expect(k8sClient.Create(ctx, &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
			})).To(Succeed())
		}
		claimPod("Implementing", "") // success, NO headCommit
		loop := s5aReconcile(r, ns, name)

		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"a success Implementing claim without headCommit must NOT advance (the MVP gate: no evidence, no advance)")
		Expect(loop.Status.CurrentVerify).To(BeNil())
	})

	It("maps clean tamper + all checks 0 to Succeeded", func() {
		ns := "s5a-ok-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "okloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name, s5aHeadCommit)
		loop := s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))

		// The verify Job pod (envtest stand-in: no Job controller, so the spec
		// creates the pod the Job WOULD create, labelled job-name).
		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-verify-pod",
				Namespace: ns,
				Labels:    map[string]string{"job-name": jobName, "coxswain.io/verify-for": name},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "noop", Image: "busybox"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		// Tamper clean (0) + check-0 pass (0).
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: "clone-base", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: "import-agent", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: "tamper", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: "check-0", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		loop = s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"clean tamper + all checks 0 -> Succeeded (the B3 gate to Succeeded)")
	})

	It("maps a non-zero check to iterate (back to Implementing, iteration+1)", func() {
		ns := "s5a-iter-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "iterloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name, s5aHeadCommit)
		loop := s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))

		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-verify-pod",
				Namespace: ns,
				Labels:    map[string]string{"job-name": jobName, "coxswain.io/verify-for": name},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "noop", Image: "busybox"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: "clone-base", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: "import-agent", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: "tamper", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: "check-0", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		loop = s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"a non-zero check -> iterate (back to Implementing)")
		Expect(loop.Status.Iteration).To(Equal(2), "the iteration increments on the iterate")
		Expect(loop.Status.CurrentVerify).To(BeNil(), "the pin clears on the iterate (the next run re-pins)")
	})

	It("maps a non-zero tamper to Failed:TamperedVerify (terminal, before the checks)", func() {
		ns := "s5a-tamper-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "tamperloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name, s5aHeadCommit)
		loop := s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))

		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-verify-pod",
				Namespace: ns,
				Labels:    map[string]string{"job-name": jobName, "coxswain.io/verify-for": name},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "noop", Image: "busybox"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		// Tamper non-zero (a protected path changed); check-0 is 0 (clean, but
		// the tamper gate is terminal BEFORE any check).
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: "clone-base", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: "import-agent", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: "tamper", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}},
			{Name: "check-0", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		loop = s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"a non-zero tamper -> Failed (terminal, before any check)")
		found := false
		for _, c := range loop.Status.Conditions {
			if c.Type == string(coxv1alpha1.LoopPhaseFailed) && c.Reason == TamperedVerifyReason {
				found = true
			}
		}
		Expect(found).To(BeTrue(), "the Failed condition carries the TamperedVerify reason")
	})

	It("holds in Verifying (requeue) when the verify Job has no pod yet (no evidence)", func() {
		ns := "s5a-hold-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "holdloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name, s5aHeadCommit)
		// The Job is created (the pin exists); the verify pod is NOT created
		// (envtest has no Job controller) -> no evidence -> hold + requeue.
		loop := s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"no verify evidence -> hold in Verifying (fail-closed, no decision)")

		// The reconcile requeues (the verify reader's no-evidence requeue).
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
	})
})
