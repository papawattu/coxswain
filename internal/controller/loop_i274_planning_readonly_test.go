// R59 I274 / D143(a): the Planning agent mounts the workspace read-only,
// with a writable .coxswain subPath mount; every other phase keeps the
// writable workspace. The kubelet enforces the read-only tree (a `git commit`
// or an edit to a tracked file fails because .git is read-only); the runner's
// own writes (PLAN.md, the claim, conversation.json, result.json, gotmp) all
// land under .coxswain and stay writable. The phase-init container keeps its
// writable mount in every phase (it creates .coxswain/gotmp and the approval
// files before the agent starts).
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// i274AgentMounts finds the agent container's workspace-related VolumeMounts
// (MountPath == /workspace or /workspace/.coxswain). Returns nil if there is
// no agent container.
func i274AgentMounts(sb *sandboxv1beta1.Sandbox) []corev1.VolumeMount {
	for i := range sb.Spec.PodTemplate.Spec.Containers {
		c := &sb.Spec.PodTemplate.Spec.Containers[i]
		if c.Name != agentContainerName {
			continue
		}
		var out []corev1.VolumeMount
		for _, m := range c.VolumeMounts {
			if m.MountPath == agentWorkspaceMount || m.MountPath == agentWorkspaceMount+"/.coxswain" {
				out = append(out, m)
			}
		}
		return out
	}
	Fail("no agent container on the built sandbox")
	return nil
}

// i274PhaseInitMounts finds the phase-init container's workspace mount.
// The phase-init container is in InitContainers (not Containers).
func i274PhaseInitMounts(sb *sandboxv1beta1.Sandbox) []corev1.VolumeMount {
	for i := range sb.Spec.PodTemplate.Spec.InitContainers {
		c := &sb.Spec.PodTemplate.Spec.InitContainers[i]
		if c.Name != phaseInitContainerName {
			continue
		}
		var out []corev1.VolumeMount
		for _, m := range c.VolumeMounts {
			if m.MountPath == agentWorkspaceMount {
				out = append(out, m)
			}
		}
		return out
	}
	Fail("no phase-init container on the built sandbox")
	return nil
}

var _ = Describe("I274 / D143(a): the Planning agent mounts the workspace read-only", func() {
	ctx := context.Background()

	newReconciler := func() *LoopReconciler {
		return &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme()}
	}

	buildLoop := func(name, ns string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: "cox-model-creds",
					ModelEndpoint:     d34ModelEndpoint,
				},
			},
		}
	}

	setDesiredPhase := func(name, ns string, phase coxv1alpha1.LoopPhase) {
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Status.Phase = phase
		loop.Status.DesiredPhase = phase
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
	}

	buildSandbox := func(name, ns string) *sandboxv1beta1.Sandbox {
		_, err := newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)).To(Succeed())
		return sb
	}

	planMounts := func(sb *sandboxv1beta1.Sandbox) (workspaceMount *corev1.VolumeMount, coxswainMount *corev1.VolumeMount) {
		for _, m := range i274AgentMounts(sb) {
			if m.MountPath == agentWorkspaceMount {
				workspaceMount = &m
			}
			if m.MountPath == agentWorkspaceMount+"/.coxswain" {
				coxswainMount = &m
			}
		}
		return workspaceMount, coxswainMount
	}

	It("mounts the workspace read-only in Planning, with a writable .coxswain", func() {
		ns := "i274-plan-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "plan-readonly"
		loop := buildLoop(name, ns)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		setDesiredPhase(name, ns, coxv1alpha1.LoopPhasePlanning)
		sb := buildSandbox(name, ns)

		workspaceMount, coxswainMount := planMounts(sb)
		Expect(workspaceMount).NotTo(BeNil(), "the agent must keep its /workspace mount in Planning")
		Expect(workspaceMount.ReadOnly).To(BeTrue(),
			"I274 / D143(a): the Planning agent's workspace mount must be ReadOnly (the kubelet enforces the read-only tree)")
		Expect(coxswainMount).NotTo(BeNil(),
			"I274 / D143(a): the Planning agent must get a writable .coxswain mount for the runner's own writes")
		Expect(coxswainMount.SubPath).To(Equal(".coxswain"), "the .coxswain mount must use SubPath .coxswain on the same volume")
		Expect(coxswainMount.Name).To(Equal(workspaceVolumeName), "the .coxswain mount must be the same workspace volume")
		Expect(coxswainMount.ReadOnly).To(BeFalse(), "the .coxswain mount must be writable (PLAN.md, claim, conversation.json, result.json, gotmp)")

		// The phase-init container keeps its writable workspace mount in Planning
		// (it creates .coxswain/gotmp and the approval files before the agent starts).
		initMounts := i274PhaseInitMounts(sb)
		Expect(initMounts).NotTo(BeEmpty(), "the phase-init container must mount the workspace in Planning")
		for i := range initMounts {
			Expect(initMounts[i].ReadOnly).To(BeFalse(),
				"I274 / D143(a): the phase-init workspace mount must stay writable (it creates .coxswain/gotmp)")
		}
	})

	It("mounts the workspace writable in Implementing (no .coxswain subPath)", func() {
		ns := "i274-impl-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "impl-writable"
		loop := buildLoop(name, ns)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		setDesiredPhase(name, ns, coxv1alpha1.LoopPhaseImplementing)
		sb := buildSandbox(name, ns)

		workspaceMount, coxswainMount := planMounts(sb)
		Expect(workspaceMount).NotTo(BeNil(), "the agent must keep its /workspace mount in Implementing")
		Expect(workspaceMount.ReadOnly).To(BeFalse(),
			"I274 / D143(a): only Planning gets the read-only workspace; Implementing stays writable")
		Expect(coxswainMount).To(BeNil(),
			"I274 / D143(a): no .coxswain subPath mount in Implementing (only Planning adds it)")
	})

	It("recreates the sandbox with a writable workspace when DesiredPhase moves Planning -> Implementing", func() {
		ns := "i274-move-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "phase-move"
		loop := buildLoop(name, ns)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		setDesiredPhase(name, ns, coxv1alpha1.LoopPhasePlanning)
		sbBefore := buildSandbox(name, ns)
		planWS, planCox := planMounts(sbBefore)
		Expect(planWS).NotTo(BeNil())
		Expect(planWS.ReadOnly).To(BeTrue(), "precondition: the Planning sandbox's agent workspace mount is read-only")
		Expect(planCox).NotTo(BeNil(), "precondition: the Planning sandbox has the writable .coxswain mount")

		// The input changed: move the desired phase to Implementing and re-reconcile.
		// The operator deletes the old sandbox on the desired-phase annotation change
		// (one reconcile), then creates the new sandbox (second reconcile).
		// The NEW sandbox's agent mount must be writable.
		setDesiredPhase(name, ns, coxv1alpha1.LoopPhaseImplementing)
		// First reconcile: deletes the stale-phase sandbox and requeues.
		_, err := newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		// Second reconcile: creates the new Implementing-phase sandbox.
		sbAfter := buildSandbox(name, ns)
		Expect(sbAfter.UID).NotTo(Equal(sbBefore.UID),
			"I274 / D143(a): a desired-phase change must recycle the sandbox (the mount change is per-phase)")
		implWS, implCox := planMounts(sbAfter)
		Expect(implWS).NotTo(BeNil(), "the new Implementing sandbox's agent must keep its /workspace mount")
		Expect(implWS.ReadOnly).To(BeFalse(),
			"I274 / D143(a): the Implementing sandbox's agent workspace mount must be writable")
		Expect(implCox).To(BeNil(), "the Implementing sandbox must have no .coxswain subPath mount")
	})
})
