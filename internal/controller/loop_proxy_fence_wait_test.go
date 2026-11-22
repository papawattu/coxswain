// fence-wait in the proxy pods (D38; homelab brief 2026-11-07): the model,
// Judge, egress and tool proxies each start with fence-wait, so nothing in a
// proxy (two of which hold a credential) runs before the proxy's own
// NetworkPolicy is up.
package controller

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
)

func proxyFenceLoop() *coxv1alpha1.Loop {
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "fenced", Namespace: "ns"},
		Spec: coxv1alpha1.LoopSpec{
			Agent: coxv1alpha1.AgentConfig{
				EndpointSecretRef: "cox-model-creds",
				ModelEndpoint:     "http://10.0.0.5:8000",
			},
		},
	}
	loop.Spec.Verify.Judge = &coxv1alpha1.JudgeConfig{
		ModelEndpoint:     "http://10.0.0.6:8000",
		EndpointSecretRef: "judge-creds",
	}
	return loop
}

// proxyPodsUnderTest builds every proxy pod shape with r.
func proxyPodsUnderTest(r *LoopReconciler) map[string]*corev1.Pod {
	loop := proxyFenceLoop()
	tool := coxv1alpha1.ToolSpec{
		Name:                "gh",
		Upstream:            "https://api.github.com",
		CredentialSecretRef: coxv1alpha1.CredentialSecretRef{Name: "gh-cred", Key: "token"},
	}
	return map[string]*corev1.Pod{
		"model":  r.buildProxyPod(loop, loop.Name, loop.Namespace, "example.test/proxy:1"),
		"judge":  r.buildJudgeProxyPod(loop),
		"egress": r.buildEgressProxyPod(loop.Name, loop.Namespace, "example.test/egress:1", []string{"github.com:443"}, "h", "10.42.0.0/16", "10.43.0.0/16"),
		"tool":   r.buildToolProxyPod(loop.Name, loop.Namespace, "example.test/tool:1", tool, "h", "10.42.0.0/16", "10.43.0.0/16"),
	}
}

func TestProxyPodsStartWithFenceWait(t *testing.T) {
	r := &LoopReconciler{FenceWaitImage: "example.test/fence:1", FenceWaitBudget: 7 * time.Second}
	for name, pod := range proxyPodsUnderTest(r) {
		t.Run(name, func(t *testing.T) {
			inits := pod.Spec.InitContainers
			if len(inits) == 0 || inits[0].Name != cni.FenceWaitContainerName {
				t.Fatalf("initContainers[0] must be fence-wait, got %v", inits)
			}
			fw := inits[0]
			if fw.Image != "example.test/fence:1" {
				t.Errorf("image = %q, want the configured --cni-probe-image", fw.Image)
			}
			var budget string
			for _, e := range fw.Env {
				if e.Name == "FENCE_WAIT_BUDGET_SECONDS" {
					budget = e.Value
				}
			}
			if budget != "7" {
				t.Errorf("FENCE_WAIT_BUDGET_SECONDS = %q, want 7", budget)
			}
			if len(fw.VolumeMounts) != 0 {
				t.Errorf("fence-wait must mount nothing (no credential), got %v", fw.VolumeMounts)
			}
			// fence-wait runs as the proxy's own user, under the same hardening.
			main := pod.Spec.Containers[0].SecurityContext
			sc := fw.SecurityContext
			if sc == nil || main == nil {
				t.Fatalf("missing security context: fence-wait %v, proxy %v", sc, main)
			}
			if *sc.RunAsUser != *main.RunAsUser || *sc.RunAsGroup != *main.RunAsGroup {
				t.Errorf("fence-wait runs as %d:%d, the proxy as %d:%d", *sc.RunAsUser, *sc.RunAsGroup, *main.RunAsUser, *main.RunAsGroup)
			}
			if !*sc.RunAsNonRoot || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation ||
				len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != allCaps {
				t.Errorf("fence-wait is not hardened: %+v", sc)
			}
		})
	}
}

func TestProxyPodsDefaultFenceWaitImage(t *testing.T) {
	for name, pod := range proxyPodsUnderTest(&LoopReconciler{}) {
		if got := pod.Spec.InitContainers[0].Image; got != cni.PinnedProbeImage {
			t.Errorf("%s: fence-wait image = %q, want cni.PinnedProbeImage", name, got)
		}
	}
}

func TestProxyPodsNoFenceWaitUnderAllowUnenforcedNetwork(t *testing.T) {
	for name, pod := range proxyPodsUnderTest(&LoopReconciler{AllowUnenforcedNetwork: true}) {
		for _, c := range pod.Spec.InitContainers {
			if c.Name == cni.FenceWaitContainerName {
				t.Errorf("%s: fence-wait under --allow-unenforced-network would never pass", name)
			}
		}
	}
}

// fence-wait passes only once the pod's policy refuses NODE_IP:10250. The
// egress and tool proxies allow 0.0.0.0/0 minus egressCarveOutCIDRs, so the
// carve-outs must cover the private ranges node IPs live in; otherwise those
// proxies would reach the kubelet and fence-wait would hold them forever.
func TestProxyCarveOutsCoverPrivateNodeIPs(t *testing.T) {
	cidrs := egressCarveOutCIDRs("", "")
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		prefixes = append(prefixes, netip.MustParsePrefix(c))
	}
	for _, ip := range []string{"10.0.0.1", "172.16.5.4", "192.168.1.40", "100.64.0.9"} {
		addr := netip.MustParseAddr(ip)
		covered := false
		for _, p := range prefixes {
			if p.Contains(addr) {
				covered = true
			}
		}
		if !covered {
			t.Errorf("node IP %s is not carved out of the proxies' 0.0.0.0/0 egress", ip)
		}
	}
}

// The Judge proxy's policy (built standalone) allows only DNS and the Judge
// endpoint on its own port: never 10250.
func TestJudgeProxyNetPolRefusesKubelet(t *testing.T) {
	np := (&LoopReconciler{}).buildJudgeProxyNetPol(proxyFenceLoop())
	for _, rule := range np.Spec.Egress {
		if len(rule.Ports) == 0 {
			t.Fatalf("egress rule with no port restriction: %+v", rule)
		}
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntValue() == 10250 {
				t.Fatalf("judge proxy egress admits the kubelet port: %+v", rule)
			}
		}
	}
}

func TestModelProxyNotReadyReportsFenceTimeout(t *testing.T) {
	loop := proxyFenceLoop()
	loop.UID = "loop-uid"
	trueP := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: proxyPodName(loop.Name), Namespace: loop.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: coxv1alpha1.GroupVersion.String(), Kind: "Loop",
				Name: loop.Name, UID: loop.UID, Controller: &trueP,
			}},
		},
		Status: corev1.PodStatus{
			PodIP: "10.42.0.9",
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  cni.FenceWaitContainerName,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: cni.FenceWaitTimeoutExit, Message: "FENCE_TIMEOUT elapsed=30.01 attempts=60",
				}},
			}},
		},
	}
	notReady, why, _ := modelProxyNotReady(pod, loop, nil)
	if !notReady || !strings.Contains(why, "fence-wait ran out of budget") {
		t.Fatalf("modelProxyNotReady = %v, %q; want a fence-wait hold", notReady, why)
	}
	if strings.Contains(why, "attempts=") {
		t.Errorf("the condition phrase must be fixed (F84c), got %q", why)
	}

	// The first attempt still running (no timeout yet) is the plain not-Ready.
	pod.Status.InitContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
	if _, why, _ := modelProxyNotReady(pod, loop, nil); why != "it is not Ready" {
		t.Errorf("why = %q, want the plain not-Ready", why)
	}
}
