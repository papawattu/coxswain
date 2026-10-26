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
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
		"RESULT %s REACHABLE", "RESULT %s BLOCKED", "RESULT %s UNRESOLVED", "DONE",
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

// D38 design point 4: strict validation. A termination message with the wrong
// line count, an unknown label, an unknown verdict, or a non-zero pod exit is
// ProbeUnavailable — never a false pass.
func TestProbeParseStrictValidation(t *testing.T) {
	p := NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage})
	// The normal PASS case: all four expected-BLOCKED targets BLOCKED
	// (APISERVER_SVC, NODE_API, KUBELET_NODE, EXTERNAL) and the positive
	// control (DNS_POSITIVE) REACHABLE -> CNIEnforced.
	allBlocked := "RESULT APISERVER_SVC BLOCKED\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT DNS_POSITIVE REACHABLE\nDONE"
	res, err := p.parse(allBlocked, pod(allBlocked, 0))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.Reason != ReasonCNIEnforced {
		t.Fatalf("all expected-BLOCKED targets BLOCKED + control REACHABLE -> CNIEnforced, got %v", res)
	}

	oneReachable := "RESULT APISERVER_SVC REACHABLE\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT DNS_POSITIVE REACHABLE\nDONE"
	res, err = p.parse(oneReachable, pod(oneReachable, 0))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.Reason != ReasonCNIUnenforced {
		t.Fatalf("one expected-BLOCKED target REACHABLE -> CNIUnenforced, got %v", res)
	}
	// The Event/condition message renders "REACHABLE targets: " + Detail, so
	// Detail must carry the reachable expected-BLOCKED label (otherwise the
	// message reads "REACHABLE targets: " with nothing after it).
	if res.Detail != "APISERVER_SVC" {
		t.Fatalf("CNIUnenforced Detail must list the reachable expected-BLOCKED labels, got %q", res.Detail)
	}

	for _, tc := range []struct {
		name, msg string
		exit      int32
	}{
		// The positive control (DNS_POSITIVE) is what the probe netpol
		// ALLOWS (kube-dns :53). A BLOCKED control means the probe has no
		// network -> ProbeUnavailable, never CNIEnforced on an unproven fence.
		{"BLOCKED control -> ProbeUnavailable", "RESULT APISERVER_SVC BLOCKED\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT DNS_POSITIVE BLOCKED\nDONE", 0},
		// EXTERNAL is now an expected-BLOCKED target (not in the DNS-only
		// allow list). EXTERNAL REACHABLE (e.g. on a non-enforcing CNI like
		// kindnet) -> CNIUnenforced, NOT a control failure.
		{"EXTERNAL REACHABLE -> CNIUnenforced", "RESULT APISERVER_SVC BLOCKED\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL REACHABLE\nRESULT DNS_POSITIVE REACHABLE\nDONE", 0},
		{"missing RESULT line", "RESULT APISERVER_SVC BLOCKED\nDONE", 0},
		{"unknown label", "RESULT APISERVER_SVC BLOCKED\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT WHATEVER BLOCKED\nDONE", 0},
		{"unknown verdict", "RESULT APISERVER_SVC MEDIUM\nRESULT KUBELET_NODE BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL REACHABLE\nRESULT DNS_POSITIVE REACHABLE\nDONE", 0},
		{"non-zero exit", allBlocked, 1},
		{"empty message", "", 0},
		// F51 P2-2: an UNRESOLVED row is a missing measurement, not a
		// verdict. It must never be CNIEnforced (the fence would look
		// proven) and never CNIUnenforced (a resolution failure would look
		// like a fence bypass), whichever target it is.
		{"UNRESOLVED expected-BLOCKED target -> ProbeUnavailable", "RESULT APISERVER_SVC UNRESOLVED\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT DNS_POSITIVE REACHABLE\nDONE", 0},
		{"UNRESOLVED positive control -> ProbeUnavailable", "RESULT APISERVER_SVC BLOCKED\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT DNS_POSITIVE UNRESOLVED\nDONE", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := p.parse(tc.msg, pod(tc.msg, tc.exit))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if tc.name == "EXTERNAL REACHABLE -> CNIUnenforced" {
				if res.Reason != ReasonCNIUnenforced {
					t.Fatalf("want CNIUnenforced, got %v (detail=%q)", res.Reason, res.Detail)
				}
				return
			}
			if res.Reason != ReasonProbeUnavailable {
				t.Fatalf("want ProbeUnavailable, got %v (detail=%q)", res.Reason, res.Detail)
			}
		})
	}
}

// F51 P2-2: UNRESOLVED is not a verdict about the fence. The result must be
// ProbeUnavailable, and its Detail must name the target that could not be
// resolved instead of the CNIUnenforced rendering ("REACHABLE targets: …").
func TestProbeParseUnresolvedIsUnavailableNotAVerdict(t *testing.T) {
	p := NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage})
	msg := "RESULT APISERVER_SVC BLOCKED\nRESULT NODE_API UNRESOLVED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT DNS_POSITIVE REACHABLE\nDONE"
	res, err := p.parse(msg, pod(msg, 0))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.Reason != ReasonProbeUnavailable {
		t.Fatalf("an UNRESOLVED target must be ProbeUnavailable, got %v", res.Reason)
	}
	if !strings.Contains(res.Detail, "NODE_API") {
		t.Fatalf("the Detail must name the unresolved target, got %q", res.Detail)
	}
	if got := res.Describe(); strings.Contains(got, "REACHABLE targets") {
		t.Fatalf("an unresolved target must not render as a fence bypass, got %q", got)
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
	msg, err := p.waitForTermination(ctx, &corev1.Pod{
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
	got, err := p.waitForTermination(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: probePodName, Namespace: probePodName},
	})
	if err != nil {
		t.Fatalf("waitForTermination: %v", err)
	}
	if got != msg {
		t.Fatalf("want the termination message %q, got %q", msg, got)
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
		{"connection refused -> UNRESOLVED", "refused", "RESULT APISERVER_SVC UNRESOLVED", "BLOCKED"},
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

// D38s3 (Calico run): the holder must be written ONLY by probeOnce (the
// Runnable). parse() must NOT call Holder().Set — if it did, probeOnce's own
// Holder().Set(newResult) would read changed=false and skip the re-gate
// (no log, no metric, no GenericEvent), so Loops would keep the stale Unknown
// condition forever. This test drives parse() directly and asserts the holder
// is untouched (still Unknown, the pre-parse state).
func TestParseDoesNotWriteTheHolder(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)

	// The holder starts at Unknown.
	if got := Holder().Result().Reason; got != ReasonUnknown {
		t.Fatalf("precondition: holder should start at Unknown, got %v", got)
	}

	p := NewPodProber(PodProberConfig{Namespace: probePodName, ProbeImage: probeTestImage})
	allBlocked := "RESULT APISERVER_SVC BLOCKED\nRESULT NODE_API BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL REACHABLE\nDONE"
	// parse() returns CNIEnforced for all-BLOCKED — but it must NOT write the
	// holder. probeOnce (the Runnable) is the only writer.
	if _, err := p.parse(allBlocked, pod(allBlocked, 0)); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The holder must STILL be Unknown: parse() must not have written it.
	if got := Holder().Result().Reason; got != ReasonUnknown {
		t.Fatalf("parse() wrote the holder (reason=%v); the holder must only be written by probeOnce (the Runnable)", got)
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
func TestBuildPodIsPinnedAndCredentialFree(t *testing.T) {
	p := NewPodProber(PodProberConfig{
		Namespace:  probePodName,
		ProbeImage: probeTestImage,
	})
	pod := p.buildPod()
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("probe pod has %d containers, want 1", len(pod.Spec.Containers))
	}
	if got := pod.Spec.Containers[0].ImagePullPolicy; got != corev1.PullIfNotPresent {
		t.Errorf("probe container ImagePullPolicy = %q, want %q (explicit: a pinned image must not be re-resolved)", got, corev1.PullIfNotPresent)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("the probe pod must not mount a service-account token (it needs no API access, ADR-0006)")
	}
	if pod.Spec.Containers[0].Image == "" {
		t.Error("the probe container must carry the configured image")
	}
}
