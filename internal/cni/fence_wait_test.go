package cni

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fence-wait (homelab brief 2026-11-07, re-measured 2026-11-21): on k3s the
// embedded kube-router programs a NEW pod's NetworkPolicy rules 0.05-1.2s
// AFTER its container starts. The probe connected at t=0, measured inside that
// window and reported CNIUnenforced on a cluster that does enforce. The policy
// pass now starts with a fence-wait init container; the control pass (no
// policy) must not, or it would never see a fence and always time out.
func TestBuildPodPolicyPassStartsWithFenceWait(t *testing.T) {
	p := NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage, FenceWaitBudget: 7 * time.Second})

	for _, pod := range []*corev1.Pod{
		p.buildPod(probePodName, probeLabelValue, ""),
		p.buildPod(probePodName, probeLabelValue, "n1"),
	} {
		if len(pod.Spec.InitContainers) != 1 {
			t.Fatalf("the policy pod must have exactly one init container (fence-wait), got %d", len(pod.Spec.InitContainers))
		}
		fw := pod.Spec.InitContainers[0]
		if fw.Name != fenceWaitContainerName {
			t.Fatalf("initContainers[0] = %q, want %q", fw.Name, fenceWaitContainerName)
		}
		main := pod.Spec.Containers[0]
		if fw.Image != main.Image || fw.ImagePullPolicy != main.ImagePullPolicy {
			t.Errorf("fence-wait must run the probe's pinned image and pull policy, got %q/%q", fw.Image, fw.ImagePullPolicy)
		}
		if fw.SecurityContext == nil || fw.SecurityContext.Capabilities == nil || len(fw.SecurityContext.Capabilities.Drop) == 0 {
			t.Error("fence-wait must carry the probe container's restricted security context")
		}
		env := map[string]corev1.EnvVar{}
		for _, e := range fw.Env {
			env[e.Name] = e
		}
		if e, ok := env["NODE_IP"]; !ok || e.ValueFrom == nil || e.ValueFrom.FieldRef == nil || e.ValueFrom.FieldRef.FieldPath != "status.hostIP" {
			t.Errorf("fence-wait must get NODE_IP from status.hostIP, got %+v", env["NODE_IP"])
		}
		if got := env["FENCE_WAIT_BUDGET_SECONDS"].Value; got != "7" {
			t.Errorf("FENCE_WAIT_BUDGET_SECONDS = %q, want %q (from PodProberConfig.FenceWaitBudget)", got, "7")
		}
	}

	if n := len(p.buildPod(probeControlPod, probeControlLabelVal, "").Spec.InitContainers); n != 0 {
		t.Fatalf("the control pod runs with NO policy, so it must not fence-wait (it would always time out), got %d init containers", n)
	}
}

func TestFenceWaitBudgetDefault(t *testing.T) {
	p := NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage})
	pod := p.buildPod(probePodName, probeLabelValue, "")
	for _, e := range pod.Spec.InitContainers[0].Env {
		if e.Name == "FENCE_WAIT_BUDGET_SECONDS" && e.Value != "30" {
			t.Fatalf("default FENCE_WAIT_BUDGET_SECONDS = %q, want 30", e.Value)
		}
	}
}

func TestFenceWaitCommandIsValidPython(t *testing.T) {
	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	cmd := fenceWaitCommand()
	if len(cmd) != 3 || cmd[0] != "python3" || cmd[1] != "-c" {
		t.Fatalf("fenceWaitCommand() = %v; want [python3 -c <script>]", cmd)
	}
	c := exec.Command(python3, "-c", "import sys; compile(sys.stdin.read(), 'fence-wait', 'exec')")
	c.Stdin = bytes.NewBufferString(cmd[2])
	var eout bytes.Buffer
	c.Stderr = &eout
	if err := c.Run(); err != nil {
		t.Fatalf("fence-wait script is not valid Python: %v\n%s", err, eout.String())
	}
}

// Runs the REAL fence-wait script with only the socket module stubbed. The
// stub plays FENCE_STUB_SEQ, one outcome per connect (the last repeats).
func TestFenceWaitScriptBehaviour(t *testing.T) {
	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	const harness = `
import os, sys, types, socket as real_socket
seq = os.environ["FENCE_STUB_SEQ"].split(",")
calls = {"n": 0}
stub = types.ModuleType("socket")
class FakeSock:
    def settimeout(self, t): pass
    def close(self): pass
    def connect(self, addr):
        assert addr == (os.environ["NODE_IP"], 10250), addr
        i = min(calls["n"], len(seq) - 1)
        calls["n"] += 1
        o = seq[i]
        if o == "reach": return
        if o == "refused": raise ConnectionRefusedError(111, "Connection refused")
        if o == "timeout": raise real_socket.timeout("timed out")
        raise AssertionError(o)
stub.socket = lambda *a, **k: FakeSock()
stub.AF_INET = real_socket.AF_INET
stub.SOCK_STREAM = real_socket.SOCK_STREAM
stub.timeout = real_socket.timeout
sys.modules["socket"] = stub
exec(compile(sys.stdin.read(), "fence-wait", "exec"))
`
	for _, tc := range []struct {
		name     string
		seq      string
		budget   string
		wantExit int
		want     string
	}{
		{"fence comes up: two consecutive non-REACHABLE", "reach,reach,refused,refused", "5", 0, "FENCE_UP"},
		{"timeouts count as fenced too", "reach,timeout,timeout", "5", 0, "FENCE_UP"},
		{"one blocked attempt is not enough", "reach,refused,reach,refused,refused", "5", 0, "attempts=5"},
		{"never fenced: budget exhausted", "reach", "0.5", fenceWaitTimeoutExit, "FENCE_TIMEOUT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := exec.Command(python3, "-c", harness)
			run.Stdin = strings.NewReader(fenceWaitCommand()[2])
			run.Env = append(os.Environ(), "NODE_IP=10.0.0.9", "FENCE_STUB_SEQ="+tc.seq, "FENCE_WAIT_BUDGET_SECONDS="+tc.budget)
			out, err := run.CombinedOutput()
			code := 0
			if ee, ok := errors.AsType[*exec.ExitError](err); ok {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatalf("run: %v\n%s", err, out)
			}
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d\n%s", code, tc.wantExit, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("want %q in output, got:\n%s", tc.want, out)
			}
		})
	}

	t.Run("missing NODE_IP is a hard failure", func(t *testing.T) {
		run := exec.Command(python3, "-c", fenceWaitCommand()[2])
		run.Env = []string{"FENCE_WAIT_BUDGET_SECONDS=1"}
		out, err := run.CombinedOutput()
		ee, ok := errors.AsType[*exec.ExitError](err)
		if !ok || ee.ExitCode() == 0 || ee.ExitCode() == fenceWaitTimeoutExit {
			t.Fatalf("want a non-zero exit other than the timeout code, got %v\n%s", err, out)
		}
	})
}

func coreScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

func fenceWaitFailedPod(exit int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: probePodName, Namespace: probePodName},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  fenceWaitContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: "FENCE_TIMEOUT elapsed=30.0"}},
			}},
		},
	}
}

func TestWaitForTerminationFenceTimeout(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(coreScheme()).WithObjects(fenceWaitFailedPod(fenceWaitTimeoutExit)).Build()
	p := NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage, Client: cl, Reader: cl})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := p.waitForTermination(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: probePodName, Namespace: probePodName}})
	if !errors.Is(err, errFenceTimeout) {
		t.Fatalf("a fence-wait that exhausted its budget must surface errFenceTimeout, got %v", err)
	}
}

func TestWaitForTerminationFenceWaitOtherFailure(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(coreScheme()).WithObjects(fenceWaitFailedPod(3)).Build()
	p := NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage, Client: cl, Reader: cl})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := p.waitForTermination(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: probePodName, Namespace: probePodName}})
	if err == nil || errors.Is(err, errFenceTimeout) || !strings.Contains(err.Error(), "fence-wait exited non-zero: 3") {
		t.Fatalf("a fence-wait crash is not a verdict about the fence, got %v", err)
	}
}

// A fence that never comes up within the budget IS evidence of unenforcement:
// the node's kubelet stayed REACHABLE under a DNS-only policy for the whole
// budget. Any other policy-pass failure stays ProbeUnavailable.
func TestProbeFenceTimeoutIsUnenforced(t *testing.T) {
	cl := newProbeFakeClient(t)
	p := NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage, ProbeTimeout: 30 * time.Second, Client: cl, Reader: cl})
	calls := 0
	p.run = func(ctx context.Context, pod *corev1.Pod) (string, string, error) {
		calls++
		if calls == 1 {
			return controlOK, "n1", nil
		}
		return "", "", fmt.Errorf("%s pod: %w", pod.Name, errFenceTimeout)
	}
	res, _ := p.Probe(context.Background())
	if res.Reason != ReasonCNIUnenforced {
		t.Fatalf("want CNIUnenforced, got %v %q", res.Reason, res.Detail)
	}
	if !strings.Contains(res.Detail, labelKubeletNode) || !strings.Contains(res.Detail, "fence-wait") {
		t.Fatalf("the detail must name the target and fence-wait, got %q", res.Detail)
	}
}
