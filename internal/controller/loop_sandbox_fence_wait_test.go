// fence-wait in the Loop sandbox pod (D38; homelab brief 2026-11-07): the
// sandbox's FIRST init container waits until the pod's own NetworkPolicy
// refuses the node's kubelet, so neither the workspace clone nor the agent can
// start inside the CNI's programming window. A fence-wait that runs out of
// budget is surfaced as SandboxFenceNotReady; the agent never starts.
package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
)

var _ = Describe("fence-wait: the sandbox pod waits for its own NetworkPolicy", func() {
	ctx := context.Background()

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

	buildSandbox := func(r *LoopReconciler, name, ns string) *sandboxv1beta1.Sandbox {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb)).To(Succeed())
		return sb
	}

	newNS := func(prefix string) string {
		ns := prefix + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })
		return ns
	}

	It("puts fence-wait FIRST, before the clone and phase-init", func() {
		ns := newNS("fence-first-")
		Expect(k8sClient.Create(ctx, buildLoop("fenced", ns))).To(Succeed())
		r := &LoopReconciler{
			CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme(),
			FenceWaitImage: "example.test/fence:1", FenceWaitBudget: 7 * time.Second,
		}
		sb := buildSandbox(r, "fenced", ns)

		inits := sb.Spec.PodTemplate.Spec.InitContainers
		Expect(len(inits)).To(BeNumerically(">=", 2))
		fw := inits[0]
		Expect(fw.Name).To(Equal(cni.FenceWaitContainerName),
			"fence-wait must be initContainers[0]: any init before it (the git clone, with its credential) would run unfenced")
		names := make([]string, 0, len(inits))
		for _, c := range inits {
			names = append(names, c.Name)
		}
		Expect(names).To(ContainElement(phaseInitContainerName))
		Expect(fw.Image).To(Equal("example.test/fence:1"), "fence-wait runs the configured image (--cni-probe-image)")
		Expect(fw.ImagePullPolicy).To(Equal(corev1.PullIfNotPresent))

		env := map[string]corev1.EnvVar{}
		for _, e := range fw.Env {
			env[e.Name] = e
		}
		Expect(env["NODE_IP"].ValueFrom).NotTo(BeNil())
		Expect(env["NODE_IP"].ValueFrom.FieldRef.FieldPath).To(Equal("status.hostIP"))
		Expect(env["FENCE_WAIT_BUDGET_SECONDS"].Value).To(Equal("7"))

		sc := fw.SecurityContext
		Expect(sc).NotTo(BeNil())
		Expect(*sc.RunAsNonRoot).To(BeTrue())
		Expect(*sc.ReadOnlyRootFilesystem).To(BeTrue())
		Expect(*sc.AllowPrivilegeEscalation).To(BeFalse())
		Expect(sc.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))
		Expect(fw.VolumeMounts).To(BeEmpty(), "fence-wait needs no volume: no workspace, no credential")
	})

	It("defaults the image to the pinned probe image", func() {
		ns := newNS("fence-default-")
		Expect(k8sClient.Create(ctx, buildLoop("fenced", ns))).To(Succeed())
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme()}
		sb := buildSandbox(r, "fenced", ns)
		fw := sb.Spec.PodTemplate.Spec.InitContainers[0]
		Expect(fw.Name).To(Equal(cni.FenceWaitContainerName))
		Expect(fw.Image).To(Equal(cni.PinnedProbeImage))
		Expect(fw.Env).To(ContainElement(corev1.EnvVar{Name: "FENCE_WAIT_BUDGET_SECONDS", Value: "30"}))
	})

	It("has no fence-wait under --allow-unenforced-network (it would never pass)", func() {
		ns := newNS("fence-hatch-")
		Expect(k8sClient.Create(ctx, buildLoop("hatch", ns))).To(Succeed())
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme(), AllowUnenforcedNetwork: true}
		sb := buildSandbox(r, "hatch", ns)
		for _, c := range sb.Spec.PodTemplate.Spec.InitContainers {
			Expect(c.Name).NotTo(Equal(cni.FenceWaitContainerName))
		}
	})
})

func fenceStatus(state, last *corev1.ContainerStateTerminated, running bool) corev1.ContainerStatus {
	cs := corev1.ContainerStatus{Name: cni.FenceWaitContainerName}
	switch {
	case state != nil:
		cs.State.Terminated = state
	case running:
		cs.State.Running = &corev1.ContainerStateRunning{}
	default:
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
	}
	cs.LastTerminationState.Terminated = last
	return cs
}

func TestSandboxFenceTimedOut(t *testing.T) {
	timeout := &corev1.ContainerStateTerminated{ExitCode: cni.FenceWaitTimeoutExit, Message: "FENCE_TIMEOUT elapsed=30.0 attempts=280"}
	up := &corev1.ContainerStateTerminated{ExitCode: 0, Message: "FENCE_UP elapsed=0.4 attempts=5"}
	crash := &corev1.ContainerStateTerminated{ExitCode: 3}
	for _, tc := range []struct {
		name string
		cs   *corev1.ContainerStatus
		want bool
	}{
		{"no fence-wait status yet", nil, false},
		{"timed out, current state", new(fenceStatus(timeout, nil, false)), true},
		{"timed out, now in back-off", new(fenceStatus(nil, timeout, false)), true},
		{"timed out, retry running", new(fenceStatus(nil, timeout, true)), true},
		{"passed after an earlier timeout", new(fenceStatus(up, timeout, false)), false},
		{"passed", new(fenceStatus(up, nil, false)), false},
		{"crashed: not a fence verdict", new(fenceStatus(nil, crash, false)), false},
		{"first attempt running", new(fenceStatus(nil, nil, true)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{}
			if tc.cs != nil {
				pod.Status.InitContainerStatuses = []corev1.ContainerStatus{*tc.cs}
			}
			if got, _ := fenceWaitTimedOut(pod); got != tc.want {
				t.Fatalf("fenceWaitTimedOut = %v, want %v", got, tc.want)
			}
		})
	}
}

func fenceTestObjects(ns, loopName string, fw corev1.ContainerStatus) (*sandboxv1beta1.Sandbox, *corev1.Pod) {
	trueP := true
	sb := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: sandboxName(loopName), Namespace: ns, UID: "sb-uid"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: sandboxName(loopName), Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: sandboxv1beta1.GroupVersion.String(), Kind: "Sandbox",
				Name: sb.Name, UID: sb.UID, Controller: &trueP,
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{fw}},
	}
	return sb, pod
}

func fenceTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, sandboxv1beta1.AddToScheme, coxv1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestApplySandboxFenceNotReady(t *testing.T) {
	const ns, name = "fence-ns", "fenced"
	timeout := &corev1.ContainerStateTerminated{ExitCode: cni.FenceWaitTimeoutExit, Message: "FENCE_TIMEOUT elapsed=30.0"}
	sb, pod := fenceTestObjects(ns, name, fenceStatus(nil, timeout, false))
	cl := fake.NewClientBuilder().WithScheme(fenceTestScheme(t)).WithObjects(sb, pod).Build()
	rec := record.NewFakeRecorder(10)
	r := &LoopReconciler{Client: cl, Scheme: cl.Scheme(), Recorder: rec}
	loop := &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	ctx := context.Background()

	if !r.applySandboxFenceNotReady(ctx, loop) {
		t.Fatal("a timed-out fence-wait must change the Loop's status")
	}
	c := findCondition(loop, sandboxFenceNotReadyCondition)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != sandboxFenceNotReadyReason {
		t.Fatalf("want SandboxFenceNotReady=True/FenceNotReady, got %+v", c)
	}
	if len(rec.Events) != 1 {
		t.Fatalf("want exactly one Warning Event on the transition, got %d", len(rec.Events))
	}
	if r.applySandboxFenceNotReady(ctx, loop) || len(rec.Events) != 1 {
		t.Fatal("an already-surfaced timeout must not write status or emit again")
	}

	// fence-wait passes on a retry: the condition clears.
	pod.Status.InitContainerStatuses[0] = fenceStatus(&corev1.ContainerStateTerminated{ExitCode: 0}, timeout, false)
	if err := cl.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if !r.applySandboxFenceNotReady(ctx, loop) {
		t.Fatal("a fence-wait that passed must clear the condition")
	}
	if c := findCondition(loop, sandboxFenceNotReadyCondition); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "FenceUp" {
		t.Fatalf("want SandboxFenceNotReady=False/FenceUp, got %+v", c)
	}

	// The pod goes away while the condition is True: cleared too.
	setCondition(loop, sandboxFenceNotReadyCondition, metav1.ConditionTrue, sandboxFenceNotReadyReason, "x")
	if err := cl.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if !r.applySandboxFenceNotReady(ctx, loop) {
		t.Fatal("a gone pod must clear the condition")
	}
	if c := findCondition(loop, sandboxFenceNotReadyCondition); c.Status != metav1.ConditionFalse {
		t.Fatalf("want False after the pod is gone, got %+v", c)
	}
}
