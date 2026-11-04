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

package cni

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// fakeReader is a client.Reader backed by an empty fake client (the reader and
// the writer differ: the reader is a direct API reader, the writer is the
// cached client). getReader() must return this, not the writer.
type fakeReader struct{ client.Reader }

// fakeClient is a client.Client backed by an empty fake client.
type fakeClient struct{ client.Client }

// probeTestImage is the probe image used in unit tests (python:3-alpine — the
// default --cni-probe-image; no bash, no /dev/tcp).
const probeTestImage = "python:3-alpine"

// D38s2: the probe pod is the D38 property measured from inside a pod with an
// agent-shaped allow (DNS only). The probe must be runnable on the default
// image (python:3-alpine — no bash, no /dev/tcp), must emit exactly
// expectedResultLen RESULT lines plus DONE on stdout (the termination
// message), and must target the D38 property's endpoints.
func TestProbeCommandShape(t *testing.T) {
	p := NewPodProber(PodProberConfig{
		Namespace:  probePodName,
		ProbeImage: probeTestImage,
	})
	cmd := p.probeCommand()
	if len(cmd) != 3 || cmd[0] != "python3" || cmd[1] != "-c" {
		t.Fatalf("the probe must run as python3 -c on the default image (no bash), got: %v", cmd)
	}
	script := cmd[2]
	for _, want := range []string{
		"RESULT %s REACHABLE", "RESULT %s BLOCKED", "RESULT %s UNRESOLVED", "RESULT %s REFUSED", "DONE",
		"kubernetes.default.svc.cluster.local", "10250", "6443", "1.1.1.1",
		"kube-dns.kube-system.svc.cluster.local",
		"settimeout", "status.hostIP",
		// The RESULT lines must be written to the termination message from
		// the script itself (a clean exit leaves /dev/termination-log empty
		// unless the script writes it; FallbackToLogsOnError only substitutes
		// the logs on a non-zero exit).
		"/dev/termination-log",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("probe script missing %q", want)
		}
	}
	// The five target labels the parse() validator expects. The node IP must
	// come from the downward API (status.hostIP env NODE_IP) — the pod's own
	// HOSTNAME resolves to the pod's own IP, never the node's.
	for _, label := range []string{"APISERVER_SVC", "NODE_API", "KUBELET_NODE", "EXTERNAL", "DNS_POSITIVE"} {
		if !strings.Contains(script, label) {
			t.Errorf("probe script missing target label %q", label)
		}
	}
	if strings.Contains(script, "99.99.99.99") || strings.Contains(script, "BLOCK_ONLY") {
		t.Errorf("probe script still references the removed BLOCK_ONLY/99.99.99.99 target (allowed by the netpol — not a CNI check)")
	}
}

// D103 (slice B): the non-zero-exit check stays for BOTH passes. A pass that
// exits non-zero is ProbeUnavailable (detail names the pass) even if the
// message looks valid — never a verdict about the fence.
func TestProbeNonZeroExitInEitherPassIsUnavailable(t *testing.T) {
	okMsg := "RESULT APISERVER_SVC BLOCKED\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT DNS_POSITIVE REACHABLE\nDONE"
	for _, pass := range []string{"control", "policy"} {
		t.Run(pass+" pass", func(t *testing.T) {
			cl := newProbeFakeClient(t)
			p := NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage, ProbeTimeout: 30 * time.Second, Client: cl, Reader: cl})
			if pass == "control" {
				p.run = func(ctx context.Context, pod *corev1.Pod) (string, string, error) {
					return "", "", fmt.Errorf("%s pod: probe exited non-zero: 3", pod.Name)
				}
				res, _ := p.Probe(context.Background())
				if res.Reason != ReasonProbeUnavailable || !strings.Contains(res.Detail, "control pass") {
					t.Fatalf("want ProbeUnavailable naming the control pass, got %v %q", res.Reason, res.Detail)
				}
				return
			}
			// Policy pass exits non-zero, control pass fine.
			calls := 0
			p.run = func(ctx context.Context, pod *corev1.Pod) (string, string, error) {
				calls++
				if calls == 1 {
					return okMsg, "n1", nil
				}
				return "", "", fmt.Errorf("%s pod: probe exited non-zero: 3", pod.Name)
			}
			res, _ := p.Probe(context.Background())
			if res.Reason != ReasonProbeUnavailable || !strings.Contains(res.Detail, "policy pass") {
				t.Fatalf("want ProbeUnavailable naming the policy pass, got %v %q", res.Reason, res.Detail)
			}
		})
	}
}

func runTwoPasses(t *testing.T, p *PodProber, controlMsg, policyMsg string) CNIProbeResult {
	t.Helper()
	if p.cfg.Client == nil {
		cl := newProbeFakeClient(t)
		p.cfg.Client = cl
		p.cfg.Reader = cl
	}
	calls := 0
	p.run = func(ctx context.Context, pod *corev1.Pod) (string, string, error) {
		calls++
		if calls == 1 {
			return controlMsg, "n1", nil
		}
		return policyMsg, "n1", nil
	}
	res, err := p.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return res
}

// F51 P2-2 / D103: UNRESOLVED is not a verdict about the fence. A combined
// UNRESOLVED row must make the result ProbeUnavailable, and its Detail must
// name the target that could not be resolved instead of the CNIUnenforced
// rendering ("REACHABLE targets: …").
func TestProbeUnresolvedIsUnavailableNotAVerdict(t *testing.T) {
	// Both passes must carry the UNRESOLVED row: combineVerdicts is policy-
	// first (policy UNRESOLVED dominates the cell), so a control-only
	// UNRESOLVED would combine to the policy pass's BLOCKED.
	unresolvedMsg := "RESULT APISERVER_SVC BLOCKED\nRESULT NODE_API UNRESOLVED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT DNS_POSITIVE REACHABLE\nDONE"
	for _, tc := range []struct {
		name string
		c, p string
	}{
		{"policy pass UNRESOLVED", unresolvedMsg, unresolvedMsg},
		{"control pass UNRESOLVED", unresolvedMsg, unresolvedMsg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cl := newProbeFakeClient(t)
			res := runTwoPasses(t, NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage, Client: cl, Reader: cl}), tc.c, tc.p)
			if res.Reason != ReasonProbeUnavailable {
				t.Fatalf("a combined UNRESOLVED target must be ProbeUnavailable, got %v", res.Reason)
			}
			if !strings.Contains(res.Detail, "NODE_API") {
				t.Fatalf("the Detail must name the unresolved target, got %q", res.Detail)
			}
			if got := res.Describe(); strings.Contains(got, "REACHABLE targets") {
				t.Fatalf("an unresolved target must not render as a fence bypass, got %q", got)
			}
		})
	}
}

func terminatedProbePod(exit int32, message string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: probePodName, Namespace: probePodName},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "probe",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode: exit,
						Reason:   "Completed",
						Message:  message,
					},
				},
			}},
		},
	}
}

func TestWaitForTerminationEmptyMessageIsFailure(t *testing.T) {
	// The reader returns the terminated probe pod with an EMPTY message: the
	// classic "stdout-only, clean exit" shape FallbackToLogsOnError does not
	// save (the script now writes /dev/termination-log itself, but a pod whose
	// message is empty must still fail fast, not spin to the timeout).
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	terminatedEmpty := terminatedProbePod(0, "")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(terminatedEmpty).Build()
	p := NewPodProber(PodProberConfig{
		Namespace:  probePodName,
		ProbeImage: probeTestImage,
		Client:     cl,
		Reader:     cl, // the direct reader is the same fake (test)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	msg, _, err := p.waitForTermination(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: probePodName, Namespace: probePodName},
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("waitForTermination: expected an error on a Terminated container with an empty message, got msg=%q", msg)
	}
	if !strings.Contains(err.Error(), "empty termination message") {
		t.Fatalf("want an 'empty termination message' error (fail-closed, not a timeout), got: %v", err)
	}
	// A hard failure must be returned promptly — not after the per-run timeout.
	if elapsed > 4*time.Second {
		t.Fatalf("an empty termination message must fail immediately, not spin to the timeout (took %s)", elapsed)
	}
}

// D38s3 control: a Terminated container WITH a termination message returns it
// (the normal success path).
func TestWaitForTerminationReturnsMessageOnTermination(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	msg := "RESULT APISERVER_SVC BLOCKED\nDONE"
	terminatedWithMsg := terminatedProbePod(0, msg)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(terminatedWithMsg).Build()
	p := NewPodProber(PodProberConfig{
		Namespace:  probePodName,
		ProbeImage: probeTestImage,
		Client:     cl,
		Reader:     cl,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, read, err := p.waitForTermination(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: probePodName, Namespace: probePodName},
	})
	if err != nil {
		t.Fatalf("waitForTermination: %v", err)
	}
	if got != msg {
		t.Fatalf("want the termination message %q, got %q", msg, got)
	}
	// The pod it read (with the scheduler's node and the container status) is
	// what runPod returns its node from — not the created object.
	if read == nil {
		t.Fatal("waitForTermination must return the pod it read (runPod reads spec.nodeName off it)")
	}
}

// D103 (slice B, round 2, P1): the probe pod has NO node at creation — the
// scheduler assigns spec.nodeName later — so runPod must read the node off
// the pod it READ (waitForTermination's Get), not the object it created.
// The interceptor plays the scheduler: the Get of the probe pod comes back
// with spec.nodeName set and the probe container terminated. Reviewer
// mutation M-B5 (return the created pod's NodeName again) must fail this.
func TestRunPodReturnsTheScheduledNode(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if pod, ok := obj.(*corev1.Pod); ok && key.Name == probePodName {
					// The scheduler's assignment arrives AFTER creation, so the
					// interceptor applies it after the real read (mutating the
					// object before the read would hit a fresh object, not the
					// stored one).
					pod.Spec.NodeName = "n1"
					pod.Status.Phase = corev1.PodSucceeded
					pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
						Name: "probe",
						State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 0,
							Reason:   "Completed",
							Message:  controlOK,
						}},
					}}
				}
				return nil
			},
		}).Build()
	p := NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage, ProbeTimeout: 30 * time.Second, Client: cl, Reader: cl})
	// Built with NO node: the created object's spec.nodeName is ""; only the
	// scheduler's assignment (via the interceptor) knows the node.
	pod := p.buildPod(probePodName, probeLabelValue, "")
	if pod.Spec.NodeName != "" {
		t.Fatalf("precondition: the created pod must have no node, got %q", pod.Spec.NodeName)
	}
	msg, node, err := p.runPod(context.Background(), pod)
	if err != nil {
		t.Fatalf("runPod: %v", err)
	}
	if node != "n1" {
		t.Fatalf("runPod node = %q, want \"n1\" (the scheduler's assignment, read off the terminated pod — the created object has none)", node)
	}
	if msg != controlOK {
		t.Fatalf("runPod msg = %q, want the termination message", msg)
	}
}

// D103 (slice B, round 2, P2): a pass whose probe container terminated with a
// NON-ZERO exit code is a hard failure ("probe exited non-zero: <code>" →
// ProbeUnavailable), even if the termination message would parse — tested
// through the REAL runPod, not a stub. Reviewer mutation M-B6 (drop the
// non-zero exit check) must fail this.
func TestRunPodNonZeroExitIsError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if pod, ok := obj.(*corev1.Pod); ok && key.Name == probePodName {
					pod.Spec.NodeName = "n1"
					pod.Status.Phase = corev1.PodFailed
					pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
						Name: "probe",
						State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 3,
							Reason:   "Error",
							Message:  controlOK, // a message that WOULD parse — the exit code must still win
						}},
					}}
				}
				return nil
			},
		}).Build()
	p := NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage, ProbeTimeout: 30 * time.Second, Client: cl, Reader: cl})
	_, node, err := p.runPod(context.Background(), p.buildPod(probePodName, probeLabelValue, ""))
	if err == nil {
		t.Fatalf("runPod: a non-zero probe exit must be an error, got node %q", node)
	}
	if !strings.Contains(err.Error(), "non-zero") {
		t.Fatalf("want a 'probe exited non-zero' error, got: %v", err)
	}
}

// D38 R16 P1: the manager's cached client has a cache scoped to the
// controller's selectors (policy.ProxyComponentSelector); the probe pod
// carries only coxswain.io/probe=cni-probe, so a cached Get on it reads
// NotFound. The PodProber must use a direct API reader (mgr.GetAPIReader())
// for Gets so the probe pod is found. This test verifies that getReader()
// returns the Reader when it is set (the reader and the writer differ), and
// falls back to the Client when Reader is nil (tests, envtest).
func TestPodProberGetReader(t *testing.T) {
	// When Reader is set, getReader() returns it (not the Client).
	fakeReader := &fakeReader{Reader: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()}
	fakeClient := &fakeClient{Client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()}
	p := NewPodProber(PodProberConfig{
		Namespace:  probePodName,
		ProbeImage: probeTestImage,
		Client:     fakeClient,
		Reader:     fakeReader,
	})
	got := p.getReader()
	if got != any(fakeReader) {
		t.Errorf("getReader() should return the Reader when set, got %T (want %T)", got, fakeReader)
	}

	// When Reader is nil, getReader() falls back to the Client.
	p2 := NewPodProber(PodProberConfig{
		Namespace:  probePodName,
		ProbeImage: probeTestImage,
		Client:     fakeClient,
		// Reader nil
	})
	got2 := p2.getReader()
	if got2 != any(fakeClient) {
		t.Errorf("getReader() should fall back to the Client when Reader is nil, got %T (want %T)", got2, fakeClient)
	}
}

// D38 (R16): the probe command is a python3 -c '<script>' whose script is a Go
// raw string with one in-string + apiserverSVC + splice. A missing quote around
// that splice (the original "unrecognised probe line: File \"<string>\", line 14"
// failure) produces a Python SyntaxError that only surfaces at pod runtime —
// the operator then sees the pod exit non-zero and reports ProbeUnavailable,
// which looks like the CNI is missing, not that the probe itself was broken.
// This test compiles the generated script with the host's python3 so a
// quoting regression is caught in `make test`, not on a kind cluster.
func TestProbeCommandIsValidPython(t *testing.T) {
	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH; skipping probe-script compile check")
	}
	// Default cluster domain ("" -> "cluster.local").
	p := NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage})
	cmd := p.probeCommand()
	if len(cmd) != 3 || cmd[0] != "python3" || cmd[1] != "-c" {
		t.Fatalf("probeCommand() = %v; want [python3 -c <script>], got %d args", cmd, len(cmd))
	}
	script := cmd[2]

	// python3 -c 'import sys; compile(sys.stdin.read(), "probe", "exec")'
	// reads the script on stdin and compile()s it; a SyntaxError exits 1.
	cmdExec := exec.Command(python3, "-c", "import sys; compile(sys.stdin.read(), 'probe', 'exec')")
	cmdExec.Stdin = bytes.NewBufferString(script)
	var eout bytes.Buffer
	cmdExec.Stderr = &eout
	if err := cmdExec.Run(); err != nil {
		t.Fatalf("generated probe script is not valid Python (python3 %v): %v\nstderr:\n%s\nscript (around the splice):\n%s",
			cmdExec.ProcessState, err, eout.String(), aroundSplice(script))
	}
}

// TestProbeScriptUnresolvedIsNotReachable executes the REAL generated probe
// script (only the socket module is stubbed) and pins the three verdict arms of
// probe(): F51 P2-2 was that a resolution failure printed REACHABLE, so an
// unresolvable expected-BLOCKED target counted as a fence bypass. The refused
// arm is pinned too (a refused connection reached the host, so it is not a
// policy drop), and a timeout stays BLOCKED as the positive control that the
// script still emits a real verdict when it has one.
func TestProbeScriptUnresolvedIsNotReachable(t *testing.T) {
	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH; skipping the probe-script execution check")
	}
	p := NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage})
	cmd := p.probeCommand()
	script := cmd[2]

	const harness = `
import os, sys, types, socket as real_socket
mode = os.environ["PROBE_STUB_MODE"]
stub = types.ModuleType("socket")
class FakeSock:
    def settimeout(self, t):
        pass
    def close(self):
        pass
    def connect(self, addr):
        if mode == "refused":
            raise ConnectionRefusedError(111, "Connection refused")
        if mode == "timeout":
            raise real_socket.timeout("timed out")
        raise AssertionError("unexpected connect in mode %s" % mode)
def gethostbyname(host):
    if mode == "gaierror":
        raise real_socket.gaierror(-2, "Name or service not known")
    return "127.0.0.1"
stub.socket = lambda *a, **k: FakeSock()
stub.AF_INET = real_socket.AF_INET
stub.SOCK_STREAM = real_socket.SOCK_STREAM
stub.timeout = real_socket.timeout
stub.gaierror = real_socket.gaierror
stub.gethostbyname = gethostbyname
sys.modules["socket"] = stub
exec(compile(sys.stdin.read(), "probe", "exec"))
`

	for _, tc := range []struct {
		name   string
		mode   string
		want   string
		reject string
	}{
		{"resolution failure -> UNRESOLVED", "gaierror", "RESULT APISERVER_SVC UNRESOLVED", "REACHABLE"},
		// D103: a refused connection now prints REFUSED (reached the host and
		// was rejected there — the no-policy control pass, slice B, tells a
		// policy drop from nothing listening); noaddr (no address) stays
		// UNRESOLVED.
		{"connection refused -> REFUSED", "refused", "RESULT APISERVER_SVC REFUSED", "UNRESOLVED"},
		{"a timeout stays BLOCKED (positive control)", "timeout", "RESULT APISERVER_SVC BLOCKED", "UNRESOLVED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := exec.Command(python3, "-c", harness)
			run.Stdin = strings.NewReader(script)
			run.Env = append(os.Environ(), "NODE_IP=127.0.0.1", "PROBE_STUB_MODE="+tc.mode)
			out, err := run.CombinedOutput()
			if err != nil {
				t.Fatalf("the generated probe script failed to run in mode %q: %v\noutput:\n%s", tc.mode, err, out)
			}
			got := string(out)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("mode %q: want %q in the probe output, got:\n%s", tc.mode, tc.want, got)
			}
			for line := range strings.SplitSeq(got, "\n") {
				if strings.HasPrefix(line, "RESULT ") && strings.Contains(line, tc.reject) {
					t.Fatalf("mode %q: verdict %q must not appear, got line %q", tc.mode, tc.reject, line)
				}
			}
			// Every target is measured in a run: five RESULT lines plus DONE.
			if n := strings.Count(got, "RESULT "); n != expectedResultLen {
				t.Fatalf("mode %q: want %d RESULT lines, got %d:\n%s", tc.mode, expectedResultLen, n, got)
			}
		})
	}
}

// aroundSplice returns the region of the script from TARGETS onward so a
// failure message shows the exact splice line that is broken.
func aroundSplice(script string) string {
	i := strings.Index(script, "TARGETS")
	if i == -1 {
		return script
	}
	return script[i:min(i+160, len(script))]
}

// D38s3 (Calico run) / D103: the holder must be written ONLY by probeOnce (the
// Runnable). Probe() must NOT call Holder().Set — if it did, probeOnce's own
// Holder().Set(newResult) would read changed=false and skip the re-gate (no
// log, no metric, no GenericEvent), so Loops would keep the stale Unknown
// condition forever. This test drives Probe() (two stubbed passes) directly
// and asserts the holder is untouched (still Unknown, the pre-Probe state).
func TestProbeDoesNotWriteTheHolder(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)

	// The holder starts at Unknown.
	if got := Holder().Result().Reason; got != ReasonUnknown {
		t.Fatalf("precondition: holder should start at Unknown, got %v", got)
	}

	okMsg := "RESULT APISERVER_SVC BLOCKED\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT DNS_POSITIVE REACHABLE\nDONE"
	// Probe() returns CNIEnforced for all-BLOCKED passes — but it must NOT
	// write the holder. probeOnce (the Runnable) is the only writer.
	cl := newProbeFakeClient(t)
	res := runTwoPasses(t, NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage, Client: cl, Reader: cl}), okMsg, okMsg)
	if res.Reason != ReasonCNIEnforced {
		t.Fatalf("two all-BLOCKED passes must be CNIEnforced, got %v", res.Reason)
	}
	// The holder must STILL be Unknown: Probe() must not have written it.
	if got := Holder().Result().Reason; got != ReasonUnknown {
		t.Fatalf("Probe() wrote the holder (reason=%v); the holder must only be written by probeOnce (the Runnable)", got)
	}
}

// F65 (P2-2): the probe pod is pinned and zero-credential.
//
// The image is digest-pinned by the operator default (cmd/main.go), the pull
// policy is EXPLICIT (an unset policy is derived from the ref shape, so it
// silently changes when a digest is added), and the SA token mount stays
// disabled — the probe needs no API access (the operator reads the pod's
// termination message).
//
// Mutation: delete the ImagePullPolicy field from buildPod → this test fails.
// D103 (slice B): the table now covers BOTH pod builders (the policy pod and
// the no-policy control pod) — same image, command, env, security context and
// token-automount=false for both, different name and probe label value.
func TestBuildPodIsPinnedAndCredentialFree(t *testing.T) {
	p := NewPodProber(PodProberConfig{
		Namespace:  probePodName,
		ProbeImage: probeTestImage,
	})
	for _, tc := range []struct {
		name       string
		pod        *corev1.Pod
		wantName   string
		wantLabel  string
		wantPinned string // "" = no node constraint
	}{
		{"policy pod", p.buildPod(probePodName, probeLabelValue, ""), probePodName, probeLabelValue, ""},
		{"policy pod pinned to the control node", p.buildPod(probePodName, probeLabelValue, "n1"), probePodName, probeLabelValue, "n1"},
		{"control pod", p.buildPod(probeControlPod, probeControlLabelVal, ""), probeControlPod, probeControlLabelVal, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.pod
			if c.Name != tc.wantName {
				t.Fatalf("pod name = %q, want %q", c.Name, tc.wantName)
			}
			if got := c.Labels[probeLabelKey]; got != tc.wantLabel {
				t.Errorf("pod label %s = %q, want %q", probeLabelKey, got, tc.wantLabel)
			}
			if len(c.Spec.Containers) != 1 {
				t.Fatalf("probe pod has %d containers, want 1", len(c.Spec.Containers))
			}
			if got := c.Spec.Containers[0].ImagePullPolicy; got != corev1.PullIfNotPresent {
				t.Errorf("probe container ImagePullPolicy = %q, want %q (explicit: a pinned image must not be re-resolved)", got, corev1.PullIfNotPresent)
			}
			if c.Spec.AutomountServiceAccountToken == nil || *c.Spec.AutomountServiceAccountToken {
				t.Error("the probe pod must not mount a service-account token (it needs no API access, ADR-0006)")
			}
			if c.Spec.Containers[0].Image == "" {
				t.Error("the probe container must carry the configured image")
			}
			pin := nodeAffinityMatchFieldsValue(c)
			if pin != tc.wantPinned {
				t.Errorf("NodeSelector[\"kubernetes.io/hostname\"] = %q, want %q", pin, tc.wantPinned)
			}
		})
	}
}

// D103 (slice A): the REFUSED+control cell of the D103 table. One table row
// per cell, plus the label-missing-from-control case. Reviewer mutations M-A1
// (REFUSED+REACHABLE -> UNRESOLVED) and M-A2 (REFUSED+REFUSED -> BLOCKED) must
// fail this test.
func TestCombineVerdictsTable(t *testing.T) {
	policy := map[string]string{
		"APISERVER_SVC": verdictReachable,  // REACHABLE cell
		"NODE_API":      verdictBlocked,    // BLOCKED cell
		"KUBELET_NODE":  verdictRefused,    // REFUSED cells
		"EXTERNAL":      verdictRefused,    // (control missing -> UNRESOLVED)
		"DNS_POSITIVE":  verdictUnresolved, // UNRESOLVED cell
	}
	control := map[string]string{
		"APISERVER_SVC": verdictBlocked,   // ignored: policy REACHABLE
		"NODE_API":      verdictReachable, // ignored: policy BLOCKED
		"KUBELET_NODE":  verdictReachable, // REFUSED+REACHABLE -> BLOCKED
		"EXTERNAL":      verdictRefused,   // REFUSED+REFUSED -> NOT_LISTENING
		"DNS_POSITIVE":  verdictReachable, // ignored: policy UNRESOLVED
	}
	got := combineVerdicts(policy, control)
	for _, tc := range []struct{ label, want string }{
		{"APISERVER_SVC", verdictReachable},
		{"NODE_API", verdictBlocked},
		{"KUBELET_NODE", verdictBlocked},
		{"EXTERNAL", verdictNotListening},
		{"DNS_POSITIVE", verdictUnresolved},
	} {
		if got[tc.label] != tc.want {
			t.Errorf("combine(%s) = %s, want %s", tc.label, got[tc.label], tc.want)
		}
	}

	// REFUSED + control BLOCKED or UNRESOLVED -> UNRESOLVED.
	for _, c := range []string{verdictBlocked, verdictUnresolved} {
		if got := combineVerdicts(map[string]string{"NODE_API": verdictRefused}, map[string]string{"NODE_API": c})["NODE_API"]; got != verdictUnresolved {
			t.Errorf("REFUSED + control %s -> %s, want UNRESOLVED", c, got)
		}
	}
	// A label missing from the control pass -> UNRESOLVED.
	if got := combineVerdicts(map[string]string{"NODE_API": verdictRefused}, map[string]string{})["NODE_API"]; got != verdictUnresolved {
		t.Errorf("REFUSED + missing control -> %s, want UNRESOLVED", got)
	}
	// REFUSED + REACHABLE stays BLOCKED even for a label the control pass did
	// not emit everything else about (only that label matters).
	if got := combineVerdicts(map[string]string{"EXTERNAL": verdictRefused}, map[string]string{"EXTERNAL": verdictReachable})["EXTERNAL"]; got != verdictBlocked {
		t.Errorf("REFUSED + REACHABLE -> %s, want BLOCKED", got)
	}
}

// D103 (slice A): the verdict rules of decide(), in order. Reviewer mutation
// M-A3 (drop the "at least one host-network BLOCKED" rule) must fail the
// all-host-network-NOT_LISTENING case.
func TestDecideRules(t *testing.T) {
	full := func(v map[string]string) map[string]string {
		out := make(map[string]string, len(passLabels))
		for _, label := range passLabels {
			out[label] = v[label]
		}
		return out
	}
	enforced := func() map[string]string {
		return full(map[string]string{
			"APISERVER_SVC": verdictBlocked, "NODE_API": verdictBlocked,
			"KUBELET_NODE": verdictBlocked, "EXTERNAL": verdictBlocked,
			"DNS_POSITIVE": verdictReachable,
		})
	}
	allBlockedPass := map[string]string{
		"APISERVER_SVC": verdictBlocked, "NODE_API": verdictBlocked,
		"KUBELET_NODE": verdictBlocked, "EXTERNAL": verdictBlocked,
		"DNS_POSITIVE": verdictReachable,
	}
	allReachablePass := map[string]string{
		"APISERVER_SVC": verdictReachable, "NODE_API": verdictReachable,
		"KUBELET_NODE": verdictReachable, "EXTERNAL": verdictReachable,
		"DNS_POSITIVE": verdictReachable,
	}

	t.Run("rule 1: any combined UNRESOLVED -> ProbeUnavailable naming the label", func(t *testing.T) {
		combined := enforced()
		combined["EXTERNAL"] = verdictUnresolved
		res := decide(allBlockedPass, allBlockedPass, combined)
		if res.Reason != ReasonProbeUnavailable || res.Detail != "probe target was not measurable: EXTERNAL" {
			t.Fatalf("want ProbeUnavailable naming EXTERNAL, got %v %q", res.Reason, res.Detail)
		}
	})
	t.Run("rule 2: DNS not REACHABLE in both passes -> ProbeUnavailable", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			p, c map[string]string
		}{
			{"policy DNS BLOCKED", map[string]string{"DNS_POSITIVE": verdictBlocked}, map[string]string{"DNS_POSITIVE": verdictReachable}},
			{"control DNS BLOCKED", map[string]string{"DNS_POSITIVE": verdictReachable}, map[string]string{"DNS_POSITIVE": verdictBlocked}},
			{"control DNS REFUSED", map[string]string{"DNS_POSITIVE": verdictReachable}, map[string]string{"DNS_POSITIVE": verdictRefused}},
		} {
			policy := enforced()
			policy["DNS_POSITIVE"] = tc.p["DNS_POSITIVE"]
			control := enforced()
			control["DNS_POSITIVE"] = tc.c["DNS_POSITIVE"]
			// The combined pass used here must NOT have a combined UNRESOLVED
			// (rule 1 would fire first): DNS REFUSED in one pass only combines
			// to NOT_LISTENING, which is not UNRESOLVED.
			res := decide(policy, control, combineVerdicts(policy, control))
			if res.Reason != ReasonProbeUnavailable {
				t.Fatalf("%s: want ProbeUnavailable, got %v %q", tc.name, res.Reason, res.Detail)
			}
			if !strings.Contains(res.Detail, positiveControlLabel) {
				t.Fatalf("%s: Detail must name %s, got %q", tc.name, positiveControlLabel, res.Detail)
			}
		}
	})
	t.Run("rule 3: expected-BLOCKED label combined REACHABLE -> CNIUnenforced", func(t *testing.T) {
		// The kindnet case: the policy pass is REACHABLE -> CNIUnenforced.
		// The kindnet case: the policy pass is REACHABLE -> CNIUnenforced.
		res := decide(allReachablePass, allBlockedPass, allReachablePass)
		if res.Reason != ReasonCNIUnenforced || res.Detail != "APISERVER_SVC, NODE_API, KUBELET_NODE, EXTERNAL" {
			t.Fatalf("want CNIUnenforced listing the reachable labels, got %v %q", res.Reason, res.Detail)
		}
	})
	t.Run("rule 4: APISERVER_SVC + a host-network label BLOCKED -> CNIEnforced", func(t *testing.T) {
		// The Calico case: policy BLOCKED, control REACHABLE -> CNIEnforced.
		res := decide(allBlockedPass, allReachablePass, allBlockedPass)
		if res.Reason != ReasonCNIEnforced {
			t.Fatalf("want CNIEnforced, got %v %q", res.Reason, res.Detail)
		}
		// Rows carry every label's combined verdict.
		if len(res.Rows) != expectedResultLen {
			t.Fatalf("Rows must carry all %d labels, got %d", expectedResultLen, len(res.Rows))
		}
	})
	t.Run("rule 4/5: APISERVER_SVC BLOCKED but every host-network target NOT_LISTENING -> ProbeUnavailable", func(t *testing.T) {
		// Rule 4 fails on the host-network term (not the apiserver term):
		// the apiserver svc being blocked alone must not read CNIEnforced
		// when nothing on the node was measurable. This is the case
		// mutation A3 (drop the host-network term) must fail.
		combined := full(map[string]string{
			"APISERVER_SVC": verdictBlocked, "NODE_API": verdictNotListening,
			"KUBELET_NODE": verdictNotListening, "EXTERNAL": verdictBlocked,
			"DNS_POSITIVE": verdictReachable,
		})
		res := decide(map[string]string{"DNS_POSITIVE": verdictReachable}, map[string]string{"DNS_POSITIVE": verdictReachable}, combined)
		if res.Reason != ReasonProbeUnavailable || res.Detail != "no host-network target was measurable" {
			t.Fatalf("want ProbeUnavailable (no host-network target was measurable), got %v %q", res.Reason, res.Detail)
		}
	})
	t.Run("rule 5: all host-network targets NOT_LISTENING -> ProbeUnavailable", func(t *testing.T) {
		// REFUSED in both passes for every host-network target (nothing
		// listens); the apiserver route alone cannot prove the fence.
		combined := full(map[string]string{
			"APISERVER_SVC": verdictNotListening, "NODE_API": verdictNotListening,
			"KUBELET_NODE": verdictNotListening, "EXTERNAL": verdictNotListening,
			"DNS_POSITIVE": verdictReachable,
		})
		res := decide(map[string]string{"DNS_POSITIVE": verdictReachable}, map[string]string{"DNS_POSITIVE": verdictReachable}, combined)
		if res.Reason != ReasonProbeUnavailable || res.Detail != "no host-network target was measurable" {
			t.Fatalf("want ProbeUnavailable (no host-network target was measurable), got %v %q", res.Reason, res.Detail)
		}
		// NOT_LISTENING rows are still shown in Rows.
		for _, row := range res.Rows {
			if row.Label == "NODE_API" && row.Verdict != verdictNotListening {
				t.Fatalf("Rows must show the NOT_LISTENING verdict, got %+v", row)
			}
		}
	})
	t.Run("the homelab case: REJECT-style policy controller -> CNIEnforced", func(t *testing.T) {
		// Policy pass: everything refused except DNS (a worker node's k3s
		// embedded controller REJECTS instead of DROPS). Control pass: all
		// REACHABLE except NODE_API refused (no apiserver on a worker).
		policy := full(map[string]string{
			"APISERVER_SVC": verdictRefused, "NODE_API": verdictRefused,
			"KUBELET_NODE": verdictRefused, "EXTERNAL": verdictRefused,
			"DNS_POSITIVE": verdictReachable,
		})
		control := full(map[string]string{
			"APISERVER_SVC": verdictReachable, "NODE_API": verdictRefused,
			"KUBELET_NODE": verdictReachable, "EXTERNAL": verdictReachable,
			"DNS_POSITIVE": verdictReachable,
		})
		combined := combineVerdicts(policy, control)
		// APISERVER_SVC/KUBELET_NODE/EXTERNAL: REFUSED+REACHABLE -> BLOCKED;
		// NODE_API: REFUSED+REFUSED -> NOT_LISTENING.
		if combined["APISERVER_SVC"] != verdictBlocked || combined["KUBELET_NODE"] != verdictBlocked || combined["EXTERNAL"] != verdictBlocked || combined["NODE_API"] != verdictNotListening {
			t.Fatalf("combined pass is not the D103 table: %v", combined)
		}
		res := decide(policy, control, combined)
		if res.Reason != ReasonCNIEnforced {
			t.Fatalf("the homelab case must be CNIEnforced (the policy rejected a connection that succeeds without it), got %v %q", res.Reason, res.Detail)
		}
		nodeAPI := ""
		for _, row := range res.Rows {
			if row.Label == "NODE_API" {
				nodeAPI = row.Verdict
			}
		}
		if nodeAPI != verdictNotListening {
			t.Fatalf("Rows must show NODE_API NOT_LISTENING, got %q (rows %+v)", nodeAPI, res.Rows)
		}
	})
}

// D103 (slice A): parsePass strictness. An unknown label, a bad verdict and
// a short table are each an error — never a partial pass.
func TestParsePassStrict(t *testing.T) {
	valid := "RESULT APISERVER_SVC BLOCKED\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT DNS_POSITIVE REACHABLE\nDONE"
	pass, err := parsePass(valid)
	if err != nil {
		t.Fatalf("a valid pass must parse: %v", err)
	}
	if len(pass) != expectedResultLen || pass["APISERVER_SVC"] != verdictBlocked {
		t.Fatalf("parsePass returned %v, want 5 labels with APISERVER_SVC BLOCKED", pass)
	}
	// REFUSED is a known verdict (the new arm).
	withRefused := "RESULT APISERVER_SVC REFUSED\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT DNS_POSITIVE REACHABLE\nDONE"
	pass, err = parsePass(withRefused)
	if err != nil || pass["APISERVER_SVC"] != verdictRefused {
		t.Fatalf("REFUSED is a known verdict: err=%v pass=%v", err, pass)
	}
	for _, tc := range []struct {
		name, msg, wantErr string
	}{
		{"unknown label", "RESULT WHATEVER BLOCKED\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT DNS_POSITIVE REACHABLE\nDONE", "unknown probe target label: WHATEVER"},
		{"bad verdict", "RESULT APISERVER_SVC MEDIUM\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT DNS_POSITIVE REACHABLE\nDONE", "unrecognised verdict: APISERVER_SVC MEDIUM"},
		{"bad line shape", "RESULT APISERVER_SVC\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT DNS_POSITIVE REACHABLE\nDONE", "unrecognised probe line: RESULT APISERVER_SVC"},
		{"short table", "RESULT APISERVER_SVC BLOCKED\nDONE", "expected 5 RESULT lines, got 1"},
		{"empty message", "", "expected 5 RESULT lines, got 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parsePass(tc.msg)
			if err == nil {
				t.Fatalf("want an error for %s, got none", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q must contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// D103 (slice A, R2 P2-2): a repeated label overwrites the map entry but
// still counts toward expectedResultLen, so APISERVER_SVC twice plus no
// EXTERNAL line would pass the count check and the missing label would slip
// through (a CNIEnforced with EXTERNAL never measured). parsePass must reject
// the duplicate. Reviewer mutation M-A5 (drop the check) must fail this test.
func TestParsePassRejectsDuplicateLabel(t *testing.T) {
	msg := "RESULT APISERVER_SVC BLOCKED\nRESULT APISERVER_SVC BLOCKED\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT DNS_POSITIVE REACHABLE\nDONE"
	_, err := parsePass(msg)
	if err == nil {
		t.Fatalf("a duplicate label must be an error (it would let a missing label slip the count check), got none")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error %q must name the duplicate label", err.Error())
	}
}

func newProbeFakeClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

const (
	controlOK = "RESULT APISERVER_SVC BLOCKED\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT DNS_POSITIVE REACHABLE\nDONE"
	policyOK  = "RESULT APISERVER_SVC BLOCKED\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT DNS_POSITIVE REACHABLE\nDONE"
)

// D103 (slice B): the control pod must NOT be selected by the probe
// NetworkPolicy — that is what makes it a no-policy baseline. Convert the
// netpol's PodSelector with metav1.LabelSelectorAsSelector and assert it
// matches the policy pod's labels and does NOT match the control pod's.
// Reviewer mutation M-B1 (the control pod gets the probe label) must fail this.
func TestControlPodNotSelectedByProbeNetpol(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	p := NewPodProber(PodProberConfig{
		Namespace:  probePodName,
		ProbeImage: probeTestImage,
		Client:     cl,
		Reader:     cl,
	})
	ctx := context.Background()
	if err := p.ensureNetpol(ctx); err != nil {
		t.Fatalf("ensureNetpol: %v", err)
	}
	got := &networkingv1.NetworkPolicy{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: probePodName, Name: probeNetpolName}, got); err != nil {
		t.Fatalf("get netpol: %v", err)
	}
	sel, err := metav1.LabelSelectorAsSelector(&got.Spec.PodSelector)
	if err != nil {
		t.Fatalf("the netpol podSelector must convert: %v", err)
	}
	policyPod := p.buildPod(probePodName, probeLabelValue, "")
	controlPod := p.buildPod(probeControlPod, probeControlLabelVal, "")
	if !sel.Matches(labels.Set(policyPod.Labels)) {
		t.Fatalf("the probe netpol must SELECT the policy pod (labels %v)", policyPod.Labels)
	}
	if sel.Matches(labels.Set(controlPod.Labels)) {
		t.Fatalf("the probe netpol must NOT select the control pod (labels %v) — it would run under the very policy the control pass exists to bypass", controlPod.Labels)
	}
}

// D103 (slice B): the policy pod is pinned to the control pod's node so
// NODE_IP (downward status.hostIP) names the same node in both passes. A stub
// run returning node n1 for the control pod must produce a second call whose
// policy pod carries NodeSelector kubernetes.io/hostname=n1. Reviewer
// mutation M-B2 (drop the pin) must fail this.
func TestPolicyPodPinnedToControlNode(t *testing.T) {
	cl := newProbeFakeClient(t)
	p := NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage, ProbeTimeout: 30 * time.Second, Client: cl, Reader: cl})
	var policyPod *corev1.Pod
	calls := 0
	p.run = func(ctx context.Context, pod *corev1.Pod) (string, string, error) {
		calls++
		if calls == 1 {
			// The control pass: report the node the policy pass must pin to.
			return controlOK, "n1", nil
		}
		policyPod = pod
		return policyOK, pod.Spec.NodeName, nil
	}
	res, _ := p.Probe(context.Background())
	if res.Reason != ReasonCNIEnforced {
		t.Fatalf("two all-BLOCKED/REACHABLE passes must be CNIEnforced, got %v %q", res.Reason, res.Detail)
	}
	if calls != 2 {
		t.Fatalf("the probe must run exactly two passes, ran %d", calls)
	}
	if policyPod == nil {
		t.Fatal("the policy pod was never built (the policy pass never ran)")
	}
	if got := policyPod.Name; got != probePodName {
		t.Fatalf("the second pass must be the policy pod %q, got %q", probePodName, got)
	}
	if got := policyPod.Labels[probeLabelKey]; got != probeLabelValue {
		t.Fatalf("the policy pod must carry the netpol-selected label %s=%q", probeLabelKey, got)
	}
	// The pin is a required node affinity on the node's NAME (a MatchFields
	// metadata.name In [node] term) — the kubernetes.io/hostname label is not
	// guaranteed to equal the node name, so a hostname NodeSelector would not
	// schedule on a renamed node (D103 round 2, P3).
	if got := nodeAffinityMatchFieldsValue(policyPod); got != "n1" {
		t.Fatalf("the policy pod must be pinned to the control pod's node by a required node affinity metadata.name In [n1], got affinity %v", policyPod.Spec.Affinity)
	}
	if len(policyPod.Spec.NodeSelector) != 0 {
		t.Errorf("the policy pod must not use a NodeSelector (the hostname label is not guaranteed to equal the node name), got %v", policyPod.Spec.NodeSelector)
	}
}

// nodeAffinityMatchFieldsValue returns the value of the policy pod's required
// node affinity MatchFields term for metadata.name ("" when absent).
func nodeAffinityMatchFieldsValue(pod *corev1.Pod) string {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return ""
	}
	req := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if req == nil {
		return ""
	}
	for _, term := range req.NodeSelectorTerms {
		for _, mf := range term.MatchFields {
			if mf.Key == "metadata.name" && mf.Operator == corev1.NodeSelectorOpIn && len(mf.Values) == 1 {
				return mf.Values[0]
			}
		}
	}
	return ""
}

// D103 (slice B): the homelab case, end-to-end through the seam. Control
// (no policy): all REACHABLE except NODE_API REFUSED (no apiserver on the
// worker node). Policy (netpol, a REJECT-style controller): all REFUSED
// except DNS_POSITIVE REACHABLE. REFUSED+REACHABLE combines to BLOCKED,
// REFUSED+REFUSED to NOT_LISTENING -> CNIEnforced.
func TestProbeTwoPassHomelabIsEnforced(t *testing.T) {
	control := "RESULT APISERVER_SVC REACHABLE\nRESULT NODE_API REFUSED\nRESULT KUBELET_NODE REACHABLE\nRESULT EXTERNAL REACHABLE\nRESULT DNS_POSITIVE REACHABLE\nDONE"
	policy := "RESULT APISERVER_SVC REFUSED\nRESULT NODE_API REFUSED\nRESULT KUBELET_NODE REFUSED\nRESULT EXTERNAL REFUSED\nRESULT DNS_POSITIVE REACHABLE\nDONE"
	cl := newProbeFakeClient(t)
	res := runTwoPasses(t, NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage, Client: cl, Reader: cl}), control, policy)
	if res.Reason != ReasonCNIEnforced {
		t.Fatalf("the homelab (REJECT-style) case must be CNIEnforced, got %v %q", res.Reason, res.Detail)
	}
	// Rows carry every label's combined verdict, NOT_LISTENING included.
	want := map[string]string{"APISERVER_SVC": verdictBlocked, "NODE_API": verdictNotListening, "KUBELET_NODE": verdictBlocked, "EXTERNAL": verdictBlocked, "DNS_POSITIVE": verdictReachable}
	if len(res.Rows) != len(want) {
		t.Fatalf("Rows must carry all %d labels, got %d", len(want), len(res.Rows))
	}
	for _, row := range res.Rows {
		if want[row.Label] != row.Verdict {
			t.Errorf("combined %s = %s, want %s (rows %+v)", row.Label, row.Verdict, want[row.Label], res.Rows)
		}
	}
}

// D103 (slice B): the kindnet case, end-to-end through the seam. kindnet does
// not enforce NetworkPolicy: the policy pass is REACHABLE, which stays
// REACHABLE in the combined pass -> CNIUnenforced (as today).
func TestProbeTwoPassKindnetIsUnenforced(t *testing.T) {
	allReachable := "RESULT APISERVER_SVC REACHABLE\nRESULT NODE_API REACHABLE\nRESULT KUBELET_NODE REACHABLE\nRESULT EXTERNAL REACHABLE\nRESULT DNS_POSITIVE REACHABLE\nDONE"
	cl := newProbeFakeClient(t)
	res := runTwoPasses(t, NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage, Client: cl, Reader: cl}), allReachable, allReachable)
	// The control (no-policy) baseline is all REACHABLE on kindnet, and so
	// is the policy pass (kindnet does not enforce the netpol) -> CNIUnenforced.
	if res.Reason != ReasonCNIUnenforced {
		t.Fatalf("kindnet (policy REACHABLE) must be CNIUnenforced, got %v %q", res.Reason, res.Detail)
	}
	if !strings.Contains(res.Detail, "APISERVER_SVC") || !strings.Contains(res.Detail, "EXTERNAL") {
		t.Fatalf("CNIUnenforced Detail must list the reachable expected-BLOCKED labels, got %q", res.Detail)
	}
}

// D103 (slice B): a failed control pass is ProbeUnavailable naming the
// control pass, and the policy pod is NEVER run (no fence measurement is
// possible without the baseline, and running the policy pod would leak no
// information but burn the timeout).
func TestProbeControlFailureIsUnavailable(t *testing.T) {
	cl := newProbeFakeClient(t)
	p := NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage, ProbeTimeout: 30 * time.Second, Client: cl, Reader: cl})
	calls := 0
	p.run = func(ctx context.Context, pod *corev1.Pod) (string, string, error) {
		calls++
		if calls == 1 {
			return "", "", fmt.Errorf("control pod timed out (probe pod not Ready within timeout)")
		}
		t.Error("the policy pod must never be run after a control pass failure")
		return "", "", nil
	}
	res, _ := p.Probe(context.Background())
	if res.Reason != ReasonProbeUnavailable {
		t.Fatalf("a failed control pass must be ProbeUnavailable, got %v %q", res.Reason, res.Detail)
	}
	if !strings.Contains(res.Detail, "control pass") {
		t.Fatalf("the Detail must name the control pass, got %q", res.Detail)
	}
	if calls != 1 {
		t.Fatalf("the policy pod must never be run after a control failure, run was called %d times", calls)
	}
}

// D103 (slice B): cleanup deletes BOTH probe pods (policy and control — a
// stale control pod from a previous run must go too, or the next run's
// create collides) and the netpol.
func TestCleanupDeletesBothPods(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	policy := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: probePodName, Namespace: probePodName}}
	control := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: probeControlPod, Namespace: probePodName}}
	netpol := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: probeNetpolName, Namespace: probePodName}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, control, netpol).Build()
	p := NewPodProber(PodProberConfig{
		Namespace:  probePodName,
		ProbeImage: probeTestImage,
		Client:     cl,
		Reader:     cl,
	})
	if err := p.cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	for _, obj := range []client.Object{policy, control, netpol} {
		fresh := obj.DeepCopyObject().(client.Object)
		err := cl.Get(context.Background(), client.ObjectKeyFromObject(obj), fresh)
		if !apierrors.IsNotFound(err) {
			t.Errorf("cleanup must delete %v, still present (get err=%v)", obj.GetName(), err)
		}
	}
}

// D103 (slice B): a control pod that terminated on an UNBOUND node
// (spec.nodeName empty — e.g. the scheduler has not placed it yet when a
// message somehow surfaced, or a stub that reports no node) is
// ProbeUnavailable: the policy pod cannot be pinned without a node.
func TestProbeControlPodWithoutNodeIsUnavailable(t *testing.T) {
	cl := newProbeFakeClient(t)
	p := NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage, ProbeTimeout: 30 * time.Second, Client: cl, Reader: cl})
	calls := 0
	p.run = func(ctx context.Context, pod *corev1.Pod) (string, string, error) {
		calls++
		if calls == 1 {
			return controlOK, "", nil // no node
		}
		t.Error("the policy pod must never be run when the control pod has no node")
		return "", "", nil
	}
	res, _ := p.Probe(context.Background())
	if res.Reason != ReasonProbeUnavailable || !strings.Contains(res.Detail, "no node") {
		t.Fatalf("a control pod with an empty nodeName must be ProbeUnavailable naming the missing node, got %v %q", res.Reason, res.Detail)
	}
	if calls != 1 {
		t.Fatalf("the policy pod must never be run when the control pod has no node, run was called %d times", calls)
	}
}

// D103 (slice B): an invalid termination message on EITHER pass (a wrong line
// shape, an unknown label, a short table) is ProbeUnavailable naming the
// pass — never a verdict about the fence.
func TestProbeEitherPassInvalidMessageIsUnavailable(t *testing.T) {
	ok := "RESULT APISERVER_SVC BLOCKED\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT DNS_POSITIVE REACHABLE\nDONE"
	bad := "RESULT APISERVER_SVC BLOCKED\nDONE" // short table
	for _, tc := range []struct {
		name, c, p string
		pass       string
	}{
		{"policy pass invalid", ok, bad, "policy pass"},
		{"control pass invalid", bad, ok, "control pass"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cl := newProbeFakeClient(t)
			res := runTwoPasses(t, NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage, Client: cl, Reader: cl}), tc.c, tc.p)
			if res.Reason != ReasonProbeUnavailable || !strings.Contains(res.Detail, tc.pass) {
				t.Fatalf("want ProbeUnavailable naming %s, got %v %q", tc.pass, res.Reason, res.Detail)
			}
		})
	}
}
