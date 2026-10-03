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

	networkingv1 "k8s.io/api/networking/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
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

// s5aBaseCommit is the pinned base SHA (the clone the agent started from);
// distinct from the head commit so the tamper diff has two distinct SHAs.
const s5aBaseCommit = "b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3" // 40-hex

// verify pod label + container name constants (goconst).
const (
	s5aJobNameLabel = "job-name"
	s5aImportAgent  = "import-agent"
	s5aCloneBase    = "clone-base"
	s5aTamper       = "tamper"
	s5aCheck0       = "check-0"
)

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

// s5aEnsureSandbox creates the stand-in Sandbox object (envtest has no
// agent-sandbox controller). The Sandbox CRD requires a non-empty podTemplate
// spec, so the stand-in carries a minimal agent container.
func s5aEnsureSandbox(ns, name string) {
	ctx := context.Background()
	sb := &sandboxv1beta1.Sandbox{}
	if apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)) {
		Expect(k8sClient.Create(ctx, &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
			Spec: sandboxv1beta1.SandboxSpec{
				SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
					PodTemplate: sandboxv1beta1.PodTemplate{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: agentContainerName, Image: s3StandinImage},
							},
						},
					},
				},
			},
		})).To(Succeed())
	}
}

// s5aClaimPod writes a claim to the stand-in sandbox pod's agent container
// (delete-then-create, so the pod is fresh each call — the S4 oneShotRun
// idiom). The message is the runner's real claim shape (the phase it
// EXECUTED + status + optional headCommit).
func s5aClaimPod(ns, name, phase, head string) {
	ctx := context.Background()
	nn := types.NamespacedName{Name: name + "-sandbox", Namespace: ns}
	existing := &corev1.Pod{}
	if !apierrors.IsNotFound(k8sClient.Get(ctx, nn, existing)) {
		Expect(k8sClient.Delete(ctx, existing)).To(Succeed())
	}
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

// requireVerifyGitSafeEnv asserts the container's env carries
// safe.directory=/verify via the GIT_CONFIG_COUNT form: (KEY_0, VALUE_0)
// == (safe.directory, /verify). Mutation: drop the env from a container and
// this assertion fails. import-agent carries a SECOND entry (KEY_1/VALUE_1
// for /agent-src/.git) — the helper only checks the /verify entry (KEY_0/
// VALUE_0), so it works for every container including import-agent.
func requireVerifyGitSafeEnv(c corev1.Container, msg string) {
	key, ok := envValue(c.Env, "GIT_CONFIG_KEY_0")
	Expect(ok).To(BeTrue(), msg+" must set GIT_CONFIG_KEY_0")
	Expect(key).To(Equal("safe.directory"), msg)
	val, ok := envValue(c.Env, "GIT_CONFIG_VALUE_0")
	Expect(ok).To(BeTrue(), msg+" must set GIT_CONFIG_VALUE_0")
	Expect(val).To(Equal(verifyScratchPath), msg+" must set safe.directory=/verify")
}

// envValue reads a single env var by name from a container's Env list.
func envValue(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// s5aDriveToVerifying drives a fresh Loop (ns/name already created) through
// the claim path to Verifying: a Planning claim, then an Implementing claim
// s5aDriveToVerifying drives a fresh Loop from Planning to Verifying: a
// Planning success claim advances to Implementing, then an Implementing
// success claim carrying the 40-hex headCommit pins status.currentVerify and
// advances to Verifying. It re-ensures the stand-in Sandbox between reconciles
// (the annotation-based phase recycle deletes it on an advance).
func s5aDriveToVerifying(ns, name string) *LoopReconciler {
	ctx := context.Background()
	r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
	// A stand-in sandbox object (envtest has no agent-sandbox controller).
	ensure := func() { s5aEnsureSandbox(ns, name) }
	ensure()
	// Bootstrap: Pending -> Planning on the first reconcile.
	s5aReconcile(r, ns, name)
	// Pin status.baseCommit (the tamper diff needs it; the verify Job's
	// clone-base fetches both baseCommit and verifiedCommit). The envtest
	// sandbox pod has no terminated init container, so the operator's own
	// read-back never runs — the spec sets it directly (a 40-hex SHA).
	loop := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	loop.Status.BaseCommit = s5aBaseCommit
	Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
	// Planning claim (the runner executed Planning).
	s5aClaimPod(ns, name, "Planning", "")
	s5aReconcile(r, ns, name)
	// The operator's annotation-based recycle deleted the Sandbox on the
	// advance; re-ensure it before the next claim.
	ensure()
	// Implementing claim WITH the headCommit (the operator pins it on the
	// Implementing -> Verifying advance).
	s5aClaimPod(ns, name, "Implementing", s5aHeadCommit)
	s5aReconcile(r, ns, name)
	return r
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

		r := s5aDriveToVerifying(ns, name)
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

		By("stamping the Job with the verifiedCommit it was built for (D27)")
		Expect(job.Annotations["coxswain.io/verified-commit"]).To(Equal(s5aHeadCommit),
			"the verify Job must carry the verifiedCommit annotation (stale-evidence guard)")

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
			if inits[i].Name == s5aImportAgent {
				for _, vm := range inits[i].VolumeMounts {
					Expect(vm.Name).NotTo(Equal("git-cred"), "import-agent must never mount the credential")
				}
			}
		}

		By("mounting the agent's workspace PVC read-only into import-agent (B3c)")
		for _, vm := range inits[1].VolumeMounts {
			if vm.Name == verifyAgentVol {
				Expect(vm.ReadOnly).To(BeTrue(), "the agent workspace must be read-only in the verify Job")
			}
		}
		foundPVC := false
		for _, v := range job.Spec.Template.Spec.Volumes {
			if v.Name == verifyAgentVol && v.PersistentVolumeClaim != nil {
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

		By("clone-base fetches baseCommit from origin, NOT verifiedCommit (fix a)")
		cloneCmd := inits[0].Command[2]
		Expect(cloneCmd).To(ContainSubstring(s5aBaseCommit),
			"clone-base must reference the pinned baseCommit")
		Expect(cloneCmd).NotTo(ContainSubstring(s5aHeadCommit),
			"clone-base must NOT reference verifiedCommit (it's not on origin)")

		By("import-agent uses git fetch from file:///agent-src/.git, NOT tar (fix c)")
		importCmd := inits[1].Command[2]
		Expect(importCmd).NotTo(ContainSubstring("tar"),
			"import-agent must NOT use tar (working-tree copy is the tamper bypass)")
		Expect(importCmd).To(ContainSubstring("file:///agent-src/.git"),
			"import-agent must fetch from the agent's .git via file://")
		Expect(importCmd).To(ContainSubstring("core.hooksPath=/dev/null"),
			"import-agent must disable hooks (the agent's hooks must not run)")
		Expect(importCmd).To(ContainSubstring(s5aHeadCommit),
			"import-agent must fetch the verifiedCommit")

		By("import-agent sets safe.directory via GIT_CONFIG_COUNT env (fix c)")
		requireVerifyGitSafeEnv(inits[1], "import-agent")
		hasAgentSrcSafe := false
		for _, env := range inits[1].Env {
			if env.Name == "GIT_CONFIG_VALUE_1" && env.Value == "/agent-src/.git" {
				hasAgentSrcSafe = true
			}
		}
		Expect(hasAgentSrcSafe).To(BeTrue(),
			"import-agent must also allow the different-owner SOURCE repo /agent-src/.git")

		By("carrying safe.directory=/verify on EVERY verify container (the emptyDir is root-owned)")
		for i := range inits {
			requireVerifyGitSafeEnv(inits[i], "container "+inits[i].Name)
		}
		// The user-authored check containers carry it too (covers arbitrary
		// check commands; env-form, not argv).
		for i := range inits {
			if strings.HasPrefix(inits[i].Name, "check-") {
				requireVerifyGitSafeEnv(inits[i], "check container "+inits[i].Name)
			}
		}

		By("import-agent mounts the PVC read-only with no creds (fix c)")
		for _, vm := range inits[1].VolumeMounts {
			if vm.Name == verifyAgentVol {
				Expect(vm.ReadOnly).To(BeTrue())
			}
			Expect(vm.Name).NotTo(Equal("git-cred"),
				"import-agent must not mount the git credential")
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
		ensureSB := func() { s5aEnsureSandbox(ns, name) }
		ensureSB()
		s5aReconcile(r, ns, name)
		s5aClaimPod(ns, name, "Planning", "")
		s5aReconcile(r, ns, name)
		ensureSB()
		// Malformed headCommit (uppercase + 41 chars): the strict parser
		// rejects the claim, so the advance is never taken.
		s5aClaimPod(ns, name, "Implementing", strings.ToUpper(s5aHeadCommit)+"0")
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
		s5aEnsureSandbox(ns, name)
		s5aReconcile(r, ns, name)
		s5aClaimPod(ns, name, "Planning", "")
		s5aReconcile(r, ns, name)
		s5aEnsureSandbox(ns, name)
		s5aClaimPod(ns, name, "Implementing", "") // success, NO headCommit
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

		r := s5aDriveToVerifying(ns, name)
		loop := s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))

		// The verify Job pod (envtest stand-in: no Job controller, so the spec
		// creates the pod the Job WOULD create, labelled job-name).
		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-verify-pod",
				Namespace: ns,
				Labels:    map[string]string{s5aJobNameLabel: jobName, verifyForLabel: name},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		// Tamper clean (0) + check-0 pass (0).
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aCheck0, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
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

		r := s5aDriveToVerifying(ns, name)
		loop := s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))

		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-verify-pod",
				Namespace: ns,
				Labels:    map[string]string{s5aJobNameLabel: jobName, verifyForLabel: name},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aCheck0, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}},
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

		r := s5aDriveToVerifying(ns, name)
		loop := s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))

		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-verify-pod",
				Namespace: ns,
				Labels:    map[string]string{s5aJobNameLabel: jobName, verifyForLabel: name},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		// Tamper non-zero (a protected path changed); check-0 is 0 (clean, but
		// the tamper gate is terminal BEFORE any check).
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}},
			{Name: s5aCheck0, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
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

		r := s5aDriveToVerifying(ns, name)
		// The Job is created (the pin exists); the verify pod is NOT created
		// (envtest has no Job controller) -> no evidence -> hold + requeue.
		loop := s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"no verify evidence -> hold in Verifying (fail-closed, no decision)")

		// The reconcile requeues (the verify reader's no-evidence requeue).
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
	})

	It("clone-base uses the S3a printf-based basic-auth header when a credential secret is declared (fix b)", func() {
		ns := "s5a-cred-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "credloop"
		ws := testWorkspace()
		ws.GitCredentialSecret = "samples-git-cred"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: ws,
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
			},
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		s5aReconcile(r, ns, name)

		jobName := fmt.Sprintf("%s-verify-1", name)
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())

		cloneCt := job.Spec.Template.Spec.InitContainers[0]
		Expect(cloneCt.Name).To(Equal(s5aCloneBase))
		cloneCmd := cloneCt.Command[2]
		// The auth header uses the printf '%s:%s' pattern with BOTH username and password.
		Expect(cloneCmd).To(ContainSubstring("printf '%s:%s'"),
			"the auth header must use the S3a printf pattern (username:password)")
		Expect(cloneCmd).To(ContainSubstring("/git-cred/username"),
			"the auth header must read the username from the secret mount")
		Expect(cloneCmd).To(ContainSubstring("/git-cred/password"),
			"the auth header must read the PASSWORD from the secret mount (fix b)")
		// clone-base fetches baseCommit, not verifiedCommit.
		Expect(cloneCmd).To(ContainSubstring(s5aBaseCommit))
		Expect(cloneCmd).NotTo(ContainSubstring(s5aHeadCommit))
		// The credential mount is present.
		hasCredMount := false
		for _, vm := range cloneCt.VolumeMounts {
			if vm.Name == "git-cred" && vm.ReadOnly {
				hasCredMount = true
			}
		}
		Expect(hasCredMount).To(BeTrue(), "clone-base must mount the git-cred secret read-only")
	})

	It("updates the verify NetworkPolicy when the repo port changes (I42c pattern, fix d)", func() {
		ns := "s5a-netpol-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "netpolloop"
		// First, create the Loop with a repo on port 3000.
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "test-cred", Namespace: ns},
			Type:       corev1.SecretTypeBasicAuth,
			Data:       map[string][]byte{workspaceCredsUsernameKey: []byte("u"), workspaceCredsPasswordKey: []byte("p")},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: coxv1alpha1.Workspace{Repo: "http://gitea.coxswain-ns.svc:3000/org/repo.git", GitCredentialSecret: "test-cred"},
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
			},
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		s5aReconcile(r, ns, name)

		npolName := name + "-verify-netpol"
		npol := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: npolName, Namespace: ns}, npol)).To(Succeed())
		// The initial port is 3000.
		var portBefore int32
		for _, rule := range npol.Spec.Egress {
			for _, p := range rule.Ports {
				if p.Port.IntValue() != 53 { // skip DNS
					portBefore = p.Port.IntVal
				}
			}
		}
		Expect(portBefore).To(Equal(int32(3000)))

		// Change the repo to a different port (8080) and reconcile again.
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Spec.Workspace.Repo = "http://gitea.coxswain-ns.svc:8080/org/repo.git"
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())

		// Reconcile: the NetworkPolicy must be UPDATED (I42c — not frozen).
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: npolName, Namespace: ns}, npol)).To(Succeed())
		var portAfter int32
		for _, rule := range npol.Spec.Egress {
			for _, p := range rule.Ports {
				if p.Port.IntValue() != 53 {
					portAfter = p.Port.IntVal
				}
			}
		}
		Expect(portAfter).To(Equal(int32(8080)),
			"the verify NetworkPolicy must be UPDATED when the repo port changes (I42c)")
	})

	It("deletes a stale verify Job whose annotation does not match the current pin (D27)", func() {
		ns := "s5a-stale-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "staleloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		_ = s5aReconcile(r, ns, name)

		// The Job was created with the original pin (s5aHeadCommit).
		jobName := fmt.Sprintf("%s-verify-1", name)
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())

		// Simulate a new iteration: the pin changes to a different commit.
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Status.CurrentVerify.VerifiedCommit = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
		loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		// Reconcile 1: the stale Job must be deleted (D27). The guard deletes
		// the Job with Background propagation (the apiserver's default ORPHAN
		// propagation would add an "orphan" finalizer and wait for a GC that
		// envtest does not run, leaving the Job in place). The delete is
		// async (the apiserver removes the object in a separate step), so the
		// spec drives the requeue loop: each reconcile either deletes (first
		// pass) or creates the fresh Job (once the name is free).
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		// The old Job is gone (deleted by the stale-evidence guard).
		staleJob := &batchv1.Job{}
		Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, staleJob)
			return apierrors.IsNotFound(err)
		}, "10s", "100ms").Should(BeTrue(),
			"the stale verify Job must be deleted when the pin changes (D27)")

		// The fresh Job is created for the new pin on a subsequent reconcile
		// (the name is free once the stale Job is gone). Drive the requeue
		// loop until it appears.
		newJob := &batchv1.Job{}
		var newJobErr error
		Eventually(func() bool {
			_, newJobErr = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
			if newJobErr != nil {
				return false
			}
			newJobErr = k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, newJob)
			return !apierrors.IsNotFound(newJobErr)
		}, "10s", "100ms").Should(BeTrue(),
			"a fresh verify Job must be created for the new pin once the stale Job is gone")
		Expect(newJobErr).NotTo(HaveOccurred())

		// The new Job exists with the new pin's annotation.
		Expect(newJob.Annotations["coxswain.io/verified-commit"]).To(Equal("abcdefabcdefabcdefabcdefabcdefabcdefabcd"),
			"the recreated Job must carry the NEW pin's annotation")
	})
})
