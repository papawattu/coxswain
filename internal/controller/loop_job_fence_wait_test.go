// fence-wait in the Job pods (D38; homelab brief 2026-11-07): the verify,
// memory-writer, Judge and deliver Jobs each start with fence-wait, so nothing
// in them (clone-base and the push hold the git credential; the checks run
// agent-authored code) runs before the Job's own NetworkPolicy is up.
package controller

import (
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
)

func jobFenceLoop() *coxv1alpha1.Loop {
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "fenced", Namespace: "ns"},
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
	loop.Spec.Verify.Judge = &coxv1alpha1.JudgeConfig{}
	loop.Status.BaseCommit = "0123456789abcdef0123456789abcdef01234567"
	loop.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: "89abcdef0123456789abcdef0123456789abcdef"}
	return loop
}

func jobPodSpecsUnderTest(t *testing.T, r *LoopReconciler) map[string]corev1.PodSpec {
	t.Helper()
	loop := jobFenceLoop()
	judge, err := r.buildJudgeJob(loop)
	if err != nil {
		t.Fatalf("buildJudgeJob: %v", err)
	}
	return map[string]corev1.PodSpec{
		"verify":        r.buildVerifyJobSpec(loop).Template.Spec,
		"memory-writer": r.buildMemoryWriterJob(loop, "fenced-memory", "succeeded").Spec.Template.Spec,
		"judge":         judge.Spec.Template.Spec,
		"deliver":       r.buildDeliverJobSpec(loop).Template.Spec,
	}
}

func TestJobPodsStartWithFenceWait(t *testing.T) {
	r := &LoopReconciler{FenceWaitImage: "example.test/fence:1", FenceWaitBudget: 7 * time.Second}
	for name, spec := range jobPodSpecsUnderTest(t, r) {
		t.Run(name, func(t *testing.T) {
			if len(spec.InitContainers) == 0 || spec.InitContainers[0].Name != cni.FenceWaitContainerName {
				t.Fatalf("initContainers[0] must be fence-wait, got %d inits", len(spec.InitContainers))
			}
			fw := spec.InitContainers[0]
			if fw.Image != "example.test/fence:1" {
				t.Errorf("image = %q, want the configured --cni-probe-image", fw.Image)
			}
			if len(fw.VolumeMounts) != 0 {
				t.Errorf("fence-wait must mount nothing (no credential, no workspace), got %v", fw.VolumeMounts)
			}
			for _, rl := range []corev1.ResourceList{fw.Resources.Requests, fw.Resources.Limits} {
				if rl.Cpu().IsZero() || rl.Memory().IsZero() {
					t.Errorf("fence-wait must carry cpu+memory requests and limits (F58), got %+v", fw.Resources)
				}
			}
			sc := fw.SecurityContext
			if sc == nil || *sc.RunAsUser != 65532 || *sc.RunAsGroup != 65532 || !*sc.RunAsNonRoot ||
				!*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation {
				t.Errorf("fence-wait is not hardened as the Job's non-root user: %+v", sc)
			}
			for _, c := range spec.InitContainers[1:] {
				if c.Name == cni.FenceWaitContainerName {
					t.Errorf("fence-wait appears twice")
				}
			}
		})
	}
}

func TestJobPodsNoFenceWaitUnderAllowUnenforcedNetwork(t *testing.T) {
	for name, spec := range jobPodSpecsUnderTest(t, &LoopReconciler{AllowUnenforcedNetwork: true}) {
		for _, c := range spec.InitContainers {
			if c.Name == cni.FenceWaitContainerName {
				t.Errorf("%s: fence-wait under --allow-unenforced-network would never pass", name)
			}
		}
	}
}

// A repo-less Loop has no verify NetworkPolicy (ensureVerifyNetworkPolicy),
// so a fence-wait there could never pass.
func TestVerifyJobNoFenceWaitWithoutRepo(t *testing.T) {
	loop := jobFenceLoop()
	loop.Spec.Workspace.Repo = ""
	spec := (&LoopReconciler{}).buildVerifyJobSpec(loop).Template.Spec
	for _, c := range spec.InitContainers {
		if c.Name == cni.FenceWaitContainerName {
			t.Fatal("a repo-less verify pod has no NetworkPolicy to wait for")
		}
	}
}

// A verify fence-wait that ran out of budget is an infra failure naming
// fence-wait (bounded retry), never a verdict on the agent's work; one still
// running is no decision.
func TestVerifyOutcomeFenceWait(t *testing.T) {
	waiting := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}
	pod := func(fw corev1.ContainerState) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
			{Name: cni.FenceWaitContainerName, State: fw},
			{Name: verifyCloneBaseInit, State: waiting},
			{Name: verifyImportAgentInit, State: waiting},
		}}}
	}
	timedOut := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: cni.FenceWaitTimeoutExit}}
	outcome, requeue, container, code, _ := verifyOutcome(pod(timedOut), 1, true, false)
	if outcome != verifyInfraFailed || requeue || container != cni.FenceWaitContainerName || code != cni.FenceWaitTimeoutExit {
		t.Errorf("timed-out fence-wait = (%d, %v, %q, %d), want infra failure naming fence-wait", outcome, requeue, container, code)
	}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	if outcome, requeue, _, _, _ := verifyOutcome(pod(running), 1, false, false); outcome != verifyNoDecision || !requeue {
		t.Errorf("running fence-wait = (%d, %v), want no decision + requeue", outcome, requeue)
	}
}

// afterFenceWait asserts a Job pod's initContainers[0] is fence-wait and
// returns the init containers after it: the positional verify specs below
// index the Job's own inits (clone-base = 0) from there.
func afterFenceWait(inits []corev1.Container) []corev1.Container {
	GinkgoHelper()
	Expect(inits).NotTo(BeEmpty())
	Expect(inits[0].Name).To(Equal(cni.FenceWaitContainerName), "fence-wait must be the Job pod's first init container")
	return inits[1:]
}
