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
	"strconv"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"

	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
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

// s5aBaseCommit is the pinned base SHA (the clone the agent started from);
// distinct from the head commit so the tamper diff has two distinct SHAs.
const s5aBaseCommit = "b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3" // 40-hex

// verify pod label + container name constants (goconst).
const (
	s5aJobNameLabel = "job-name"
	s5aImportAgent  = "import-agent"
	s5aCloneBase    = "clone-base"
	s5aTamper       = "tamper"
	s5aArtifact     = "artifact"
	s5aCheck0       = "check-0"
	s5aCheck1       = "check-1"
	s5aCheckPassCmd = "echo pass0"
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

// s5aClaimPodWithIteration is s5aClaimPod with an explicit iteration marker
// in the claim (the .coxswain/iteration the operator's phase-init wrote when
// the pod was created; the runner echoes it into the claim). Used to plant a
// STALE claim (iteration > loop.Status.Iteration) to exercise the S5a
// stale-iteration guard.
func s5aClaimPodWithIteration(ns, name, phase, head string, iteration int) {
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
	msg := `{"observedPhase":"` + phase + `","status":"success","blockedReason":"","iteration":` + strconv.Itoa(iteration)
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

// s5aVerifyPodMultiCheck is s5aVerifyPod with N checks: trusted inits all 0
// (clone + import + tamper + artifact), check-<k> non-zero for each k in
// failing (exit code exitCode) and 0 otherwise. It exercises the
// verify-outcome mapping with a FAILING check that is NOT check-0 (P2: the
// failing-check name/code must come from the first check-* container with
// a non-zero exit, never a fixed index).
func s5aVerifyPodMultiCheck(ctx context.Context, ns, name, jobName string, checkCount int, failing map[int]int32) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-verify-pod",
			Namespace: ns,
			Labels:    map[string]string{s5aJobNameLabel: jobName, verifyForLabel: name},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}}},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	inits := make([]corev1.ContainerStatus, 0, 4+checkCount)
	inits = append(inits,
		corev1.ContainerStatus{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		corev1.ContainerStatus{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		corev1.ContainerStatus{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		corev1.ContainerStatus{Name: s5aArtifact, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
	)
	for i := range checkCount {
		code := int32(0)
		if c, ok := failing[i]; ok {
			code = c
		}
		inits = append(inits, corev1.ContainerStatus{
			Name:  fmt.Sprintf("check-%d", i),
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}},
		})
	}
	pod.Status.InitContainerStatuses = inits
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

// s5aVerifyPod creates the stand-in verify Job pod with the given init
// exit codes (envtest has no Job controller; the operator reads the
// initContainerStatuses from this pod). checkExit 0/absent = all checks
// pass; a non-zero checkExit makes check-0 fail.
func s5aVerifyPod(ctx context.Context, ns, name, jobName string, checkExit int32) {
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
		{Name: s5aCheck0, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: checkExit}}},
	}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

// s5aVerifyInitStatuses returns the standard I47 init status list (trusted
// inits 0, tamper 0, artifact per the given state, checks per the given
// exit codes by index — a nil entry = the check is absent from the pod
// status, an empty-state entry = in progress). The helper is the I47
// spec's single construction point so the specs read as "the artifact
// container did X" instead of a 6-element literal each time.
func s5aVerifyInitStatuses(artifact corev1.ContainerState, checkExits []int32) []corev1.ContainerStatus {
	inits := make([]corev1.ContainerStatus, 0, 4+len(checkExits))
	inits = append(inits,
		corev1.ContainerStatus{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		corev1.ContainerStatus{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		corev1.ContainerStatus{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		corev1.ContainerStatus{Name: s5aArtifact, State: artifact},
	)
	for i, code := range checkExits {
		inits = append(inits, corev1.ContainerStatus{
			Name:  fmt.Sprintf("check-%d", i),
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}},
		})
	}
	return inits
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

		By("ordering the init containers: clone-base, import-agent, tamper, artifact, then the checks")
		inits := job.Spec.Template.Spec.InitContainers
		Expect(inits).To(HaveLen(5), "clone + import + tamper + artifact + 1 check")
		Expect(inits[0].Name).To(Equal("clone-base"))
		Expect(inits[1].Name).To(Equal("import-agent"))
		Expect(inits[2].Name).To(Equal("tamper"))
		Expect(inits[3].Name).To(Equal("artifact"))
		Expect(inits[4].Name).To(Equal("check-0"))
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

	It("emits the PhaseIterated Event with the correct from-phase and failing check on a verify iterate (P3)", func() {
		ns := "s5a-event-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "eventloop"
		// Two checks so the event text can assert the FIRST failing check's
		// name + code (check-0 passes, check-1 fails with exit 3).
		spec := s5aLoopSpec()
		spec.Verify.AcceptanceChecks = []string{s5aCheckPassCmd, "echo fail1; exit 3"}
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       spec,
		})).To(Succeed())

		// A FakeRecorder so the Event text is assertable (the s5aDriveToVerifying
		// helper builds its own reconciler without one).
		recorder := record.NewFakeRecorder(64)
		ctx2 := context.Background()
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: recorder}
		s5aEnsureSandbox(ns, name)
		s5aReconcile(r, ns, name) // bootstrap Pending -> Planning
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx2, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Status.BaseCommit = s5aBaseCommit
		Expect(k8sClient.Status().Update(ctx2, loop)).To(Succeed())
		s5aClaimPod(ns, name, "Planning", "")
		s5aReconcile(r, ns, name)
		s5aEnsureSandbox(ns, name)
		s5aClaimPod(ns, name, "Implementing", s5aHeadCommit)
		loop = s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		s5aReconcile(r, ns, name) // verify-1 created

		// The verify evidence: check-0=0, check-1=3.
		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		s5aVerifyPodMultiCheck(ctx2, ns, name, jobName, 2, map[int]int32{1: 3})

		// Drive to the iterate. The Event fires in this reconcile.
		var iterLoop *coxv1alpha1.Loop
		for range 10 {
			iterLoop = s5aReconcile(r, ns, name)
			if iterLoop.Status.Phase == coxv1alpha1.LoopPhaseImplementing {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		Expect(iterLoop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		Expect(iterLoop.Status.Iteration).To(Equal(2))

		// P3 (mutation: emit nothing on the iterate / wrong from-phase): the
		// event stream must carry a PhaseIterated event whose message is
		// 'Verifying -> Implementing (iteration 2, check-1 exit 3)'.
		var events []string
	Loop:
		for {
			select {
			case e, ok := <-recorder.Events:
				if !ok {
					break Loop
				}
				if e != "" {
					events = append(events, e)
				}
			default:
				break Loop
			}
		}
		var iterEvent string
		for _, e := range events {
			if strings.Contains(e, "PhaseIterated") {
				iterEvent = e
				break
			}
		}
		Expect(iterEvent).NotTo(BeEmpty(), "the verify iterate must emit a PhaseIterated Event")
		Expect(iterEvent).To(ContainSubstring("Verifying -> Implementing"),
			"the iterate event's from-phase must be Verifying (the ACTUAL previous phase)")
		Expect(iterEvent).To(ContainSubstring("iteration 2"))
		Expect(iterEvent).To(ContainSubstring("check-1 exit 3"),
			"the iterate event must name the failing check and its exit code")

		// P3 (mutation: re-consume the stale claim / wrong from-phase): the
		// event stream must NOT carry a bogus PhaseAdvanced whose from-phase
		// is the phase the consumed Implementing claim no longer is (the kind
		// evidence showed 'Planning -> Implementing' at the iterate). The only
		// forward advances on this run are Planning -> Implementing and
		// Implementing -> Verifying, each emitted ONCE.
		for _, e := range events {
			if strings.Contains(e, "PhaseAdvanced") && strings.Contains(e, "-> ") {
				Expect(e).NotTo(ContainSubstring("Verifying -> "),
					"the iterate must NOT be reported as a PhaseAdvanced forward")
			}
		}
		advancedCount := 0
		for _, e := range events {
			if strings.Contains(e, "PhaseAdvanced") {
				advancedCount++
			}
		}
		Expect(advancedCount).To(BeNumerically("<=", 2),
			"the stale Implementing claim must not be re-consumed into extra PhaseAdvanced events (the kind event stream showed a spurious 'Planning -> Implementing')")
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

	It("takes NO decision while a check is still in progress (not a failure, S5a regression)", func() {
		ns := "s5a-pending-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// Two checks: (a) check-0 Running, (b) check-0=0 with check-1 Waiting.
		// The S5a bug: a check-* init whose State.Terminated is nil (still
		// Running or Waiting when the operator polls) fell into the
		// 'prior init failed' branch and counted as check-0 failed (exit 0)
		// — the kind run passed every step yet iterated to Failed:
		// MaxIterationsExceeded. A non-terminated check is PENDING, like a
		// non-terminated tamper: no decision, requeue, phase stays Verifying.
		// Two checks so case (a)'s pending check-0 is check-0 of two, and
		// case (b) can put check-0 clean with check-1 in progress (mirroring
		// the kind run, where check-0 finished while check-1 was still
		// starting — the operator read the pending state as check-0 failing).
		spec := s5aLoopSpec()
		spec.Verify.AcceptanceChecks = []string{s5aCheckPassCmd, "echo pass1"}
		runCase := func(name string, c0 corev1.ContainerState, c1 *corev1.ContainerState) {
			Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec:       spec,
			})).To(Succeed())
			r := s5aDriveToVerifying(ns, name)
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
			inits := []corev1.ContainerStatus{
				{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				{Name: s5aCheck0, State: c0},
			}
			if c1 != nil {
				inits = append(inits, corev1.ContainerStatus{Name: s5aCheck1, State: *c1})
			} else {
				// case (a) leaves check-1 clean so the only in-progress check
				// is check-0.
				inits = append(inits, corev1.ContainerStatus{Name: s5aCheck1, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}})
			}
			pod.Status.InitContainerStatuses = inits
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			// Reconcile: the outcome must be no-decision, phase stays Verifying.
			fresh := s5aReconcile(r, ns, name)
			Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
				"%s: an in-progress check is PENDING, never a failure (phase must stay Verifying)", name)
			Expect(fresh.Status.Iteration).To(BeZero(),
				"%s: no decision must not bump the iteration", name)
			if fresh.Status.Progress != nil {
				Expect(fresh.Status.Progress.LastResultStatus).NotTo(ContainSubstring("check-failed"),
					"%s: no iterate evidence may be recorded for an in-progress check", name)
			}

			// Now terminate every check cleanly: the Loop must Succeed (the
			// pending outcome was not a failure, it was a wait).
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
				{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				{Name: s5aCheck0, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				{Name: "check-1", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
			fresh = s5aReconcile(r, ns, name)
			Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded),
				"%s: once the checks finish clean the Loop must Succeed (it was waiting, not failing)", name)
		}
		// The annotation-based phase recycle deletes the Sandbox on an
		// advance, and a terminal phase deletes the verify Job — each case
		// drives its own fresh Loop.
		runCase("pendloop-a", corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, nil)
		runCase("pendloop-b", corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}, &corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}})
	})

	It("reports the FIRST failing check's name and exit code in progress (P2, not a fixed index)", func() {
		ns := "s5a-failidx-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "failidxloop"
		// Two checks: check-0 passes, check-1 fails with exit 1. The kind
		// evidence hit this exact shape ('check-failed: check-0 (exit 0)' when
		// check-1 exited 1) — the failure mapping must report the first
		// check-* container with a non-zero exit, with ITS name and code.
		spec := s5aLoopSpec()
		spec.Verify.AcceptanceChecks = []string{s5aCheckPassCmd, "echo fail1; exit 1"}
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       spec,
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		s5aReconcile(r, ns, name) // verify-1 created

		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		s5aVerifyPodMultiCheck(ctx, ns, name, jobName, 2, map[int]int32{1: 1}) // check-0=0, check-1=1

		var loop *coxv1alpha1.Loop
		for range 10 {
			loop = s5aReconcile(r, ns, name)
			if loop.Status.Phase == coxv1alpha1.LoopPhaseImplementing {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		Expect(loop.Status.Progress).NotTo(BeNil())
		// P2 (mutation: report a fixed index / the first check's code):
		// progress must name check-1 with ITS exit code, never check-0.
		Expect(loop.Status.Progress.LastResultStatus).To(Equal("check-failed: check-1 (exit 1)"),
			"the iterate progress must name the FIRST non-zero check-* container with its own exit code (kind evidence: 'check-0 (exit 0)' was wrong)")
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

	// I47 (REVIEW-PHASE1-R20, PR #55 review): the operator-side build-artifact
	// check — the trust-boundary backstop for the runner's commitWorkspace
	// filter. The runner runs in the AGENT's container and reads the
	// agent-writable .coxswain/base-commit: an agent that runs 'git commit'
	// itself during the phase run is never re-filtered (commitWorkspace only
	// sees UNCOMMITTED changes), so a self-committed binary rides into the
	// verified commit. The check runs on the operator's trusted clone in
	// /verify (next to tamper) and maps to an iterate, not a terminal fail.
	It("adds the I47 artifact init container (after tamper, before the checks, no creds, safe.directory env)", func() {
		ns := "s5a-artifact-shape-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "artloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		loop := s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))

		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())
		inits := job.Spec.Template.Spec.InitContainers
		Expect(inits).To(HaveLen(5), "clone + import + tamper + artifact + 1 check")
		Expect(inits[2].Name).To(Equal("tamper"))
		Expect(inits[3].Name).To(Equal("artifact"), "the artifact check sits AFTER tamper, BEFORE the checks")
		Expect(inits[4].Name).To(Equal("check-0"))

		By("the artifact container is a trusted git container: base image, safe.directory env, hardened profile, NO credential mount")
		art := inits[3]
		Expect(art.Image).To(Equal(r.verifyJobImage()), "the artifact check runs on the operator's trusted git image")
		requireVerifyGitSafeEnv(art, "artifact")
		Expect(art.SecurityContext).NotTo(BeNil())
		Expect(*art.SecurityContext.RunAsUser).To(Equal(int64(65532)))
		Expect(*art.SecurityContext.ReadOnlyRootFilesystem).To(BeTrue())
		for _, vm := range art.VolumeMounts {
			Expect(vm.Name).NotTo(Equal("git-cred"), "the artifact check must never mount the git credential")
		}

		By("the artifact script diffs the pinned base..verified for ADDED files only, with hooks disabled")
		script := art.Command[2]
		Expect(script).To(ContainSubstring("diff --diff-filter=A -z --name-only"), "ADDED files only (an edit to a tracked file is source work)")
		Expect(script).To(ContainSubstring("core.hooksPath=/dev/null"), "no hook in the trusted clone may run")
		Expect(script).To(ContainSubstring(s5aBaseCommit), "the diff starts at the pinned baseCommit")
		Expect(script).To(ContainSubstring(s5aHeadCommit), "the diff ends at the pinned verifiedCommit")
	})

	It("maps a non-zero artifact exit to iterate (build artifact committed, not a terminal fail)", func() {
		ns := "s5a-artifact-fail-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "artfailloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		// NOTE: no 'verify-1 created' reconcile here. The passing pending and
		// multi-check specs drive the pod in the SAME reconcile that creates
		// the Job (one reconcile after s5aDriveToVerifying). An extra
		// reconcile in between would set up the stale-pod delete+recreate
		// cycle (the operator deletes the orphaned envtest pod by label and
		// the Job never re-creates it in envtest — no Job controller), which
		// would keep the outcome noDecision forever.

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
		// Artifact exited 1 (a self-committed build artifact, e.g. the agent
		// 'git commit'ed a binary). The checks never ran (kubelet stops on
		// the first non-zero init): no check-0 status. The pod's terminated
		// message carries the offending path (the operator's evidence).
		pod.Status.InitContainerStatuses = s5aVerifyInitStatuses(
			corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1,
				Message:  "artifact: build artifact committed in base..verified:\nartifact: gocli is binary (NUL byte in the first 8 KiB)"}},
			nil) // no checks ran
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		// The iterate is decided in THIS reconcile — exactly like the
		// multi-check spec (one reconcile after the pod + statuses appear, no
		// extra 'verify-1 created' reconcile in between that would set up the
		// stale-pod delete+recreate cycle the operator runs on the orphaned
		// envtest pod).
		loop := s5aReconcile(r, ns, name)
		if loop.Status.Phase != coxv1alpha1.LoopPhaseImplementing {
			// The Job was created in this reconcile (the pod's statuses were
			// set before it); the NEXT reconcile reads them and iterates.
			loop = s5aReconcile(r, ns, name)
		}
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"a non-zero artifact exit is an ITERATE (back to Implementing), never a terminal fail (ADR-0005 fail-closed hold, not Failed)")
		Expect(loop.Status.Iteration).To(Equal(2), "the iteration increments on the iterate")
		Expect(loop.Status.CurrentVerify).To(BeNil(), "the pin clears on the iterate (the next run re-pins)")
		Expect(loop.Status.Progress).NotTo(BeNil())
		// The progress records the failing container by its own name and
		// exit code ('check-failed: artifact (exit 1)' — the stable
		// progress form; the offending paths are in the pod's terminated
		// message, the operator's evidence).
		Expect(loop.Status.Progress.LastResultStatus).To(ContainSubstring("artifact"),
			"the iterate progress must name the artifact check: %q", loop.Status.Progress.LastResultStatus)
		Expect(loop.Status.Progress.LastResultStatus).To(ContainSubstring("exit 1"),
			"the iterate progress must carry the artifact exit code: %q", loop.Status.Progress.LastResultStatus)
	})

	It("takes NO decision while the artifact check is in progress (not a failure)", func() {
		ns := "s5a-artifact-pend-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "artpendloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		// No extra 'verify-1 created' reconcile (see the iterate spec): the
		// pod + statuses are driven in the SAME reconcile that creates the
		// Job, exactly like the passing pending check spec.

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
		// Artifact still Running (Terminated == nil): PENDING, never a
		// failure (the S5a pending-regression class: a non-terminated init
		// is a wait, not an iterate).
		pod.Status.InitContainerStatuses = s5aVerifyInitStatuses(
			corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			[]int32{0})
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		// The pending decision: one reconcile (the Job is created in this
		// reconcile and reads the pod's statuses: artifact Running ->
		// noDecision -> phase stays Verifying).
		fresh := s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"in-progress artifact check is PENDING, never a failure (phase must stay Verifying)")
		Expect(fresh.Status.Iteration).To(BeZero(), "no decision must not bump the iteration")
		if fresh.Status.Progress != nil {
			Expect(fresh.Status.Progress.LastResultStatus).NotTo(ContainSubstring("artifact"),
				"no iterate evidence may be recorded for an in-progress artifact check")
		}

		// Now terminate the artifact cleanly (0) + check-0 clean: the Loop
		// must Succeed (the pending outcome was a wait, not a failure).
		pod.Status.InitContainerStatuses = s5aVerifyInitStatuses(
			corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
			[]int32{0})
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		fresh = s5aReconcile(r, ns, name)
		Expect(fresh.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"once the artifact check finishes clean and the checks pass the Loop must Succeed")
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

	It("runs the check-* containers on spec.verify.image (NOT the trusted git image)", func() {
		ns := "s5a-img-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "imgloop"
		spec := s5aLoopSpec()
		spec.Verify.Image = verifyDefaultCheckImage
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       spec,
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		s5aReconcile(r, ns, name)

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-verify-1", Namespace: ns}, job)).To(Succeed())
		inits := job.Spec.Template.Spec.InitContainers
		// check-0 uses the Loop's verify.image... (it sits AFTER the artifact
		// check — clone + import + tamper + artifact are the trusted inits).
		Expect(inits[4].Name).To(Equal("check-0"))
		Expect(inits[4].Image).To(Equal(verifyDefaultCheckImage),
			"check-* containers must run on spec.verify.image (they may need a Go toolchain)")
		// ...while the trusted inits stay on the git image (they never run
		// user commands — the I47 artifact check is one of them: it runs the
		// operator's own script on the trusted clone, never user code).
		for _, n := range []string{"clone-base", "import-agent", "tamper", "artifact"} {
			for i := range inits {
				if inits[i].Name == n {
					Expect(inits[i].Image).NotTo(Equal(verifyDefaultCheckImage),
						"the trusted init %s must stay on the git image", n)
				}
			}
		}
	})

	It("uses the built-in Go default when no verify.image is declared (check containers, not the git image)", func() {
		ns := "s5a-defimg-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "defimgloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(), // no Verify.Image
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		s5aReconcile(r, ns, name)

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-verify-1", Namespace: ns}, job)).To(Succeed())
		inits := job.Spec.Template.Spec.InitContainers
		// check-0 sits after the I47 artifact check (clone + import + tamper
		// + artifact = indices 0-3).
		Expect(inits[4].Name).To(Equal("check-0"))
		Expect(inits[4].Image).To(Equal(verifyDefaultCheckImage),
			"without spec.verify.image the checks run on the built-in Go default (go: not found fix)")
		Expect(inits[4].Image).NotTo(Equal("docker.io/alpine/git:v2.54.0"),
			"the git image has no Go toolchain — the S5a check-0 exit 127 root cause")
	})

	It("gives every check-* container a writable /tmp (check-tmp emptyDir) and the toolchain env", func() {
		ns := "s5a-tmp-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "tmploop"
		spec := s5aLoopSpec()
		spec.Verify.AcceptanceChecks = []string{"echo c0", "echo c1"} // two checks: both must carry the mount
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       spec,
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		s5aReconcile(r, ns, name)

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-verify-1", Namespace: ns}, job)).To(Succeed())
		podSpec := job.Spec.Template.Spec

		// The check-tmp emptyDir volume exists on the pod.
		var hasCheckTmpVol bool
		for _, v := range podSpec.Volumes {
			if v.Name == verifyCheckTmpVol && v.EmptyDir != nil {
				hasCheckTmpVol = true
			}
		}
		Expect(hasCheckTmpVol).To(BeTrue(), "the pod must declare the check-tmp emptyDir volume")

		// Every check-* container mounts it at /tmp and carries the env.
		var checkCount int
		for _, ic := range podSpec.InitContainers {
			if !strings.HasPrefix(ic.Name, "check-") {
				continue
			}
			checkCount++
			var tmpMount bool
			for _, m := range ic.VolumeMounts {
				if m.Name == verifyCheckTmpVol && m.MountPath == agentTmpMount {
					tmpMount = true
				}
			}
			Expect(tmpMount).To(BeTrue(),
				"check %s must mount check-tmp at /tmp (go build needs a writable scratch)", ic.Name)
			envMap := map[string]string{}
			for _, e := range ic.Env {
				envMap[e.Name] = e.Value
			}
			for k, want := range map[string]string{
				"HOME": agentTmpMount, "TMPDIR": agentTmpMount, "GOCACHE": "/tmp/gocache",
				"GOPATH": "/tmp/gopath", "XDG_CACHE_HOME": "/tmp/.cache",
			} {
				Expect(envMap[k]).To(Equal(want),
					"check %s must set %s=%s so toolchain caches land on the writable emptyDir", ic.Name, k, want)
			}
			// safe.directory=/verify must still be there (user checks may git there).
			var safeDir bool
			for i, e := range ic.Env {
				if e.Name == "GIT_CONFIG_VALUE_0" && e.Value == verifyScratchPath &&
					i > 0 && ic.Env[i-1].Name == "GIT_CONFIG_KEY_0" && ic.Env[i-1].Value == "safe.directory" {
					safeDir = true
				}
			}
			Expect(safeDir).To(BeTrue(), "check %s must keep safe.directory=/verify", ic.Name)
		}
		Expect(checkCount).To(Equal(2))

		// The trusted inits and the main container do NOT get the /tmp mount
		// (their tooling is pinned; only user checks need the scratch).
		for _, name := range []string{"clone-base", "import-agent", "tamper", verifyNoopContainer} {
			for i := range podSpec.InitContainers {
				if podSpec.InitContainers[i].Name != name {
					continue
				}
				for _, m := range podSpec.InitContainers[i].VolumeMounts {
					Expect(m.Name).NotTo(Equal(verifyCheckTmpVol),
						"%s must not mount check-tmp", name)
				}
			}
		}
		for _, m := range podSpec.Containers[0].VolumeMounts {
			Expect(m.Name).NotTo(Equal(verifyCheckTmpVol), "the no-op main must not mount check-tmp")
		}
	})

	It("iterates a failing check back to Implementing WITHOUT creating a new verify Job", func() {
		ns := "s5a-noregen-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "noregenloop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		s5aReconcile(r, ns, name) // verify-1 created

		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		s5aVerifyPod(ctx, ns, name, jobName, 1) // check-0 fails

		// Reconcile until the phase is Implementing (the apiserver may lag
		// the shared status update on the first reconcile after the check
		// failure). The hot-loop guard ensures we never see Verifying again
		// after a failed check (the claim phase-match guard blocks the stale
		// Implementing claim from re-advancing).
		var loop *coxv1alpha1.Loop
		for i := range 10 {
			loop = s5aReconcile(r, ns, name)
			GinkgoWriter.Printf("DEBUG reconcile %d: phase=%s desiredPhase=%s iter=%d curVerify=%v\n",
				i, loop.Status.Phase, loop.Status.DesiredPhase, loop.Status.Iteration, loop.Status.CurrentVerify)
			if loop.Status.Phase == coxv1alpha1.LoopPhaseImplementing {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"a failing check -> back to Implementing (no new model work is skipped)")
		Expect(loop.Status.Iteration).To(Equal(2))
		Expect(loop.Status.CurrentVerify).To(BeNil(), "the pin clears (a fresh pin at the next advance creates the fresh Job)")
		// No second Job: the name for iteration 2 does not exist, and no Job at
		// all is created while the pin is nil.
		By("NOT creating verify-2 or any other Job (the hot-loop guard)")
		for _, candidate := range []string{name + "-verify-2", name + "-verify-3"} {
			job := &batchv1.Job{}
			getErr := k8sClient.Get(ctx, types.NamespacedName{Name: candidate, Namespace: ns}, job)
			Expect(apierrors.IsNotFound(getErr)).To(BeTrue(), "no Job %s after the iterate", candidate)
		}
		// The failing check rides into progress (the next Implementing prompt
		// can include it).
		Expect(loop.Status.Progress).NotTo(BeNil())
		Expect(loop.Status.Progress.LastResultStatus).To(ContainSubstring("check-0"))
		Expect(loop.Status.Progress.LastResultStatus).To(ContainSubstring("exit 1"))

		// S5a stale-iteration guard, mutation-checked. The pod still holds the
		// Implementing claim from the pre-iterate run. Re-seed it with an
		// explicit iteration:1 marker (the operator's phase-init wrote the
		// iteration-1 value into .coxswain/iteration when the pod was
		// created; the runner echoes it into the claim). Status is now
		// iteration 2, so this claim is STALE: the guard must NOT re-advance
		// (no Implementing -> Verifying with the same old headCommit, no pin,
		// no Job flood) and must not clobber the check-failure progress record.
		// Mutation M4: drop the iteration guard in advancePhaseFromClaim ->
		// this spec FAILS (the stale claim re-advances, pins CurrentVerify,
		// and the second s5aReconcile leaves the phase in Verifying).
		s5aClaimPodWithIteration(ns, name, "Implementing", s5aHeadCommit, 1)
		loop = s5aReconcile(r, ns, name)
		GinkgoWriter.Printf("DEBUG stale-claim reconcile: phase=%s desiredPhase=%s iter=%d curVerify=%v\n",
			loop.Status.Phase, loop.Status.DesiredPhase, loop.Status.Iteration, loop.Status.CurrentVerify)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"the stale iteration-1 claim must NOT re-advance (iteration 2 is current)")
		Expect(loop.Status.CurrentVerify).To(BeNil(),
			"the stale claim must not re-pin the commit")
		Expect(loop.Status.Progress.LastResultStatus).To(ContainSubstring("check-0"),
			"the stale claim must not clobber the verify check-failure progress record")
		Expect(loop.Status.Progress.Iteration).To(Equal(2),
			"the operator's own progress record is intact — the stale claim wrote nothing")

		// Reconcile again (still Implementing, no claim): NOTHING new is
		// created — no Job flood (the kind root cause: verify-2..verify-9
		// every ~10s while stuck in Verifying).
		loop = s5aReconcile(r, ns, name)
		GinkgoWriter.Printf("DEBUG second reconcile: phase=%s desiredPhase=%s iter=%d\n",
			loop.Status.Phase, loop.Status.DesiredPhase, loop.Status.Iteration)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		Expect(loop.Status.CurrentVerify).To(BeNil())
		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs, client.InNamespace(ns))).To(Succeed())
		Expect(jobs.Items).To(HaveLen(1), "only the original verify-1 Job exists after the iterate")
		Expect(jobs.Items[0].Name).To(Equal(name + "-verify-1"))
	})

	It("fails a Loop at the maxIterations cap instead of iterating forever (hot-loop guard)", func() {
		ns := "s5a-cap-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "caploop"
		spec := s5aLoopSpec()
		spec.Loop.MaxIterations = 3 // the handoff's MVP cap
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       spec,
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		s5aReconcile(r, ns, name)

		// Drive the Loop to iteration 3 at Verifying (under the cap of 3) by
		// setting the status directly (the cap logic is in applyVerifyOutcome,
		// which doesn't depend on the sandbox recycle).
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseVerifying
		loop.Status.Iteration = 3
		loop.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		// Reconcile to create the verify-3 Job (the new pin's annotation
		// differs from the old pin, so the stale-Job guard deletes verify-1
		// and creates verify-3). s5aReconcile returns the reconciled Loop.
		_ = s5aReconcile(r, ns, name)
		// The verify-3 Job exists (the stale-Job guard deleted verify-1 and
		// created a fresh Job for the new pin).
		job3 := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-verify-3", Namespace: ns}, job3)).To(Succeed())

		// Create a failing check pod for verify-3 and reconcile -> Failed.
		s5aVerifyPod(ctx, ns, name, name+"-verify-3", 1)
		// The reconciled Loop's status is read back fresh (s5aReconcile returns
		// the live object; the assignment below feeds the phase/condition
		// assertions, so staticcheck sees the loop variable used).
		capLoop := s5aReconcile(r, ns, name)
		Expect(capLoop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"the cap (3) is hit on the failing iteration 4 -> Failed (not another iterate)")
		found := false
		for _, c := range capLoop.Status.Conditions {
			if c.Type == string(coxv1alpha1.LoopPhaseFailed) && c.Reason == MaxIterationsExceededReason && c.Status == metav1.ConditionTrue {
				found = true
				Expect(c.Message).To(ContainSubstring("check-0"))
			}
		}
		Expect(found).To(BeTrue(), "the Failed condition carries the MaxIterationsExceeded reason")
		// No verify-4 Job (the iterate is capped).
		job4 := &batchv1.Job{}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-verify-4", Namespace: ns}, job4))).To(BeTrue())
	})

	It("ignores a stale iteration-0 Implementing claim after the first iterate (I48)", func() {
		// I48 (R20): the S5a stale-iteration guard previously exempted
		// iteration-0 claims (claim.Iteration > 0 && ...). After the FIRST
		// verify iterate (status.iteration 0 -> 2), a first-cycle success
		// claim left on the old pod must be ignored like any prior-iteration
		// claim: no re-advance to Verifying, no re-pin of the old headCommit,
		// no second verify Job. The envtest stand-in for the first-cycle claim
		// carries the real iteration-0 shape: the pre-S5a runner read a
		// missing .coxswain/iteration marker as 0, and the strict parser
		// leaves an ABSENT iteration field at 0 — the same claim the guard
		// used to let through.
		// Gate mutation (scratch worktree, recorded not committed): restoring
		// the 'claim.Iteration > 0 &&' exemption makes this spec FAIL (the
		// stale claim re-advances to Verifying and re-pins s5aHeadCommit).
		ns := "i48-iter0-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "i48loop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		s5aReconcile(r, ns, name) // verify-1 created (pinned at status.iteration 0)

		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		s5aVerifyPod(ctx, ns, name, jobName, 1) // check-0 fails

		var loop *coxv1alpha1.Loop
		for range 10 {
			loop = s5aReconcile(r, ns, name)
			if loop.Status.Phase == coxv1alpha1.LoopPhaseImplementing {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"a failing check -> back to Implementing")
		Expect(loop.Status.Iteration).To(Equal(2), "the first iterate bumped status.iteration 0 -> 2")
		Expect(loop.Status.CurrentVerify).To(BeNil())

		// Plant the STALE first-cycle claim: an Implementing success with the
		// pre-S5a (iteration-0) shape — no iteration field, so the claim's
		// iteration is 0 while status.iteration is 2.
		s5aClaimPod(ns, name, "Implementing", s5aHeadCommit)
		loop = s5aReconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"the stale iteration-0 claim must NOT re-advance to Verifying (status.iteration is 2)")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		Expect(loop.Status.CurrentVerify).To(BeNil(),
			"the stale iteration-0 claim must not re-pin the old headCommit")
		Expect(loop.Status.Progress.LastResultStatus).To(ContainSubstring("check-0"),
			"the stale iteration-0 claim must not clobber the verify check-failure progress record")
		Expect(loop.Status.Progress.Iteration).To(Equal(2),
			"the operator's own progress record is intact — the stale claim wrote nothing")

		// No NEW verify Job: the pin is nil, so ensureVerifyJob holds (no Job
		// create for the cleared pin; a re-pin would create verify-2). The
		// original verify-1 is not garbage-collected in envtest (no Job
		// controller / GC), so the guard is: exactly the original verify-1
		// remains and no verify-2 exists.
		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs, client.InNamespace(ns))).To(Succeed())
		Expect(jobs.Items).To(HaveLen(1), "the stale iteration-0 claim created no new verify Job")
		Expect(jobs.Items[0].Name).To(Equal(name + "-verify-1"))
		job2 := &batchv1.Job{}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-verify-2", Namespace: ns}, job2))).To(BeTrue(),
			"no verify-2: a re-pin from the stale claim would create it")
	})
})
