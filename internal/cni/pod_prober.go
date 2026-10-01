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

// Package cni — the real pod-based CNIProber (D38s2). It creates the probe
// pod + NetworkPolicy in the fixed coxswain-cni-probe namespace, waits for it
// with a per-run timeout, reads the termination message, validates the RESULT
// lines strictly, and deletes the pod. A probe that cannot complete returns
// ReasonProbeUnavailable — it never returns an error that is treated as
// "enforced".
package cni

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodProberConfig configures the real CNIProber.
type PodProberConfig struct {
	// Client is the controller-runtime client (the manager's cached client).
	// Used for Create/Delete of the probe pod + NetworkPolicy.
	Client client.Client
	// Reader is the direct API reader (mgr.GetAPIReader()) for Gets. The
	// manager's cached client has a cache scoped to the controller's
	// selectors (policy.ProxyComponentSelector); the probe pod carries only
	// coxswain.io/probe=cni-probe, so a cached Get on it reads NotFound.
	// Every Get (probe pod + NetworkPolicy) goes through the direct reader;
	// the cached client is used only for Create/Delete.
	// If Reader is nil, Client is used for Gets too (tests, envtest).
	Reader client.Reader
	// Namespace is the fixed probe namespace (coxswain-cni-probe).
	Namespace string
	// ProbeImage is the image the probe pod runs.
	ProbeImage string
	// ProbeTimeout is the per-run timeout for one probe.
	ProbeTimeout time.Duration
	// ClusterDomain is the cluster's service DNS domain (for the probe's
	// apiserver-svc target FQDN). Defaults to cluster.local if empty.
	ClusterDomain string
}

// PodProber is the real CNIProber.
type PodProber struct {
	cfg PodProberConfig
}

// NewPodProber returns a real CNIProber.
func NewPodProber(cfg PodProberConfig) *PodProber {
	if cfg.ClusterDomain == "" {
		cfg.ClusterDomain = "cluster.local"
	}
	return &PodProber{cfg: cfg}
}

const (
	probePodName      = "coxswain-cni-probe"
	probeNetpolName   = "coxswain-cni-probe-netpol"
	probeLabelKey     = "coxswain.io/probe"
	expectedResultLen = 4
)

// knownProbeLabels is the strict set of RESULT labels the probe emits (design
// point 4: an unknown label is a validation error, never a false pass).
var knownProbeLabels = map[string]bool{
	"APISERVER_SVC": true,
	"KUBELET_NODE":  true,
	"EXTERNAL":      true,
	"BLOCK_ONLY":    true,
}

// probeCommand is the agent-shaped probe, run as the pod's own command (the
// image is python:3-alpine by default — it ships python3 and neither bash nor
// /dev/tcp, so the probe is a Python socket connect, exactly like
// verify-cni.sh's probe). It writes one RESULT line per target to stdout
// (captured to the termination message via
// terminationMessagePolicy: FallbackToLogsOnError) and exits 0.
//
// The targets are the D38 property's (design point 2): the apiserver
// service IP (pod -> ServiceClusterIP -> host network), the node's kubelet
// :10250 (host network, node-local), an external IP (pod -> node egress to
// the outside), and 99.99.99.99:80 — a bare IP the NetworkPolicy must
// block (a "should never be allowed" control: a CNI that fails open lets
// it through; a CNI that enforces blocks it). The netpol allows only DNS
// 53 to the node's coredns, so every one of these four targets must be
// BLOCKED for the CNI to count as enforcing.

// podTerminated reports whether the named container's state is Terminated
// (read from its ContainerStatus; Status.Phase alone lags the container
// state, and an empty status (freshly created pod) means not terminated).
func podTerminated(pod *corev1.Pod, container string) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == container && cs.State.Terminated != nil {
			return true
		}
	}
	return false
}

// getReader returns the direct API reader for Gets. The manager's cached
// client has a cache scoped to the controller's selectors
// (policy.ProxyComponentSelector); the probe pod carries only
// coxswain.io/probe=cni-probe, so a cached Get on it reads NotFound. Every
// Get goes through the direct reader (mgr.GetAPIReader()); the cached client
// is used only for Create/Delete. If Reader is nil (tests, envtest), the
// cached client is used for Gets too.
func (p *PodProber) getReader() client.Reader {
	if p.cfg.Reader != nil {
		return p.cfg.Reader
	}
	return p.cfg.Client
}

func (p *PodProber) probeCommand() []string {
	apiserverSVC := "kubernetes.default.svc." + p.cfg.ClusterDomain
	return []string{"python3", "-c", `
import os, socket, socket as _s, sys

TARGETS = [
    ("APISERVER_SVC", "` + apiserverSVC + `", 443),
    ("KUBELET_NODE", os.environ.get("HOSTNAME") or _s.gethostname(), 10250),
    ("EXTERNAL", "1.1.1.1", 443),
    ("BLOCK_ONLY", "99.99.99.99", 80),
]

lines = []
def out(line):
    lines.append(line)
    print(line)

def probe(label, host, port):
    try:
        host = socket.gethostbyname(host)
    except Exception:
        out("RESULT %s REACHABLE" % label)  # unresolvable -> not a CNI failure
        return
    s = socket.socket(_s.AF_INET, _s.SOCK_STREAM)
    s.settimeout(5)
    try:
        s.connect((host, port))
        out("RESULT %s REACHABLE" % label)
    except Exception:
        out("RESULT %s BLOCKED" % label)
    finally:
        s.close()

for label, host, port in TARGETS:
    probe(label, host, port)
out("DONE")

# The operator reads the RESULT lines from the container's termination
# message. FallbackToLogsOnError substitutes the logs only when the
# container exits non-zero; on a clean exit the message comes from
# /dev/termination-log, so the script must write it there itself (the
# lines are far under the 4 KB termination-message limit).
try:
    with open("/dev/termination-log", "w") as f:
        f.write("\n".join(lines) + "\n")
except Exception as exc:
    sys.stderr.write("could not write /dev/termination-log: %s\n" % exc)
` + "\n",
	}
}

// Probe runs one probe: it cleans up any stale probe objects, creates the
// probe NetworkPolicy + pod, waits for the termination message within the
// timeout, validates the RESULT lines strictly, deletes the pod, and reports
// the result. Any failure (timeout, not-Ready, non-zero exit, wrong/unknown
// RESULT lines) returns ReasonProbeUnavailable.
func (p *PodProber) Probe(ctx context.Context) (CNIProbeResult, error) {
	probeCtx, cancel := context.WithTimeout(ctx, p.cfg.ProbeTimeout)
	defer cancel()

	// Stale cleanup at startup (the PR #38 leak): delete any leftover probe
	// pod / NetworkPolicy from a previous run.
	if err := p.cleanup(probeCtx); err != nil {
		return unavailable("stale cleanup failed: " + err.Error()), nil
	}

	// Create the NetworkPolicy (the agent-shaped allow: dns 53/tcp + the four
	// target IPs only). The netpol is what the probe measures against — the
	// CNI's enforcement is exactly whether this netpol is obeyed.
	if err := p.ensureNetpol(probeCtx); err != nil {
		return unavailable("netpol create failed: " + err.Error()), nil
	}

	// Create the probe pod. The probe pod is operator-level state (not
	// owner-ref'd to a Loop — there is no Loop to reference); it is cleaned up
	// by name at the start of the next probe and on operator shutdown.
	pod := p.buildPod()
	if err := p.cfg.Client.Create(probeCtx, pod); err != nil {
		return unavailable("probe pod create failed: " + err.Error()), nil
	}
	// Best-effort pod deletion on any exit path (the stale cleanup at the
	// start of the next probe is the backstop).
	defer func() {
		if derr := p.cfg.Client.Delete(context.Background(), pod); derr != nil && !apierrors.IsNotFound(derr) {
			p.cfg.Client.Scheme() // no-op; the error is logged by the caller
		}
	}()

	// Wait for the termination message (or the pod to go not-Ready / time out).
	msg, err := p.waitForTermination(probeCtx, pod)
	if err != nil {
		return unavailable(err.Error()), nil
	}

	// Strict validation: exactly 4 RESULT lines, each a known label, no
	// unknown lines. A non-zero pod exit also maps to ProbeUnavailable.
	return p.parse(msg, pod)
}

// LatestResult implements CNIProber: it returns the holder's cached result
// (what the probe Runnable set). It never runs a probe.
func (p *PodProber) LatestResult() CNIProbeResult {
	return Holder().Result()
}

// cleanup deletes any leftover probe pod / NetworkPolicy (stale from a
// previous run — the PR #38 leak).
func (p *PodProber) cleanup(ctx context.Context) error {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: probePodName, Namespace: p.cfg.Namespace}}
	if err := p.cfg.Client.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	netpol := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: probeNetpolName, Namespace: p.cfg.Namespace}}
	if err := p.cfg.Client.Delete(ctx, netpol); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// ensureNetpol creates the agent-shaped probe NetworkPolicy (the allow list the
// CNI's enforcement is measured against).
func (p *PodProber) ensureNetpol(ctx context.Context) error {
	want := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: probeNetpolName, Namespace: p.cfg.Namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{probeLabelKey: "cni-probe"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				// dns 53/tcp to the node's coredns (the probe needs DNS for
				// the apiserver-svc target).
				{Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptrProtocolTCP(), Port: ptrPort(53)}}},
				// The four targets' IPs (the probe's allow list).
				{To: []networkingv1.NetworkPolicyPeer{{
					IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0",
						Except: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}},
				}}},
			},
		},
	}
	// CreateOrUpdate: the netpol is the operator's state; re-apply it each run
	// so a drift (manual edit) is corrected.
	existing := &networkingv1.NetworkPolicy{}
	reader := p.getReader()
	err := reader.Get(ctx, client.ObjectKey{Namespace: p.cfg.Namespace, Name: probeNetpolName}, existing)
	if apierrors.IsNotFound(err) {
		return p.cfg.Client.Create(ctx, want)
	}
	if err != nil {
		return err
	}
	want.ResourceVersion = existing.ResourceVersion
	return p.cfg.Client.Update(ctx, want)
}

// buildPod returns the probe pod (the agent's command, the termination
// message, the non-root/priv-dropped hardening — the same shape as the sandbox
// pod, D38 design point 2).
func (p *PodProber) buildPod() *corev1.Pod {
	nonRoot := int64(65532)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      probePodName,
			Namespace: p.cfg.Namespace,
			Labels:    map[string]string{probeLabelKey: "cni-probe"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: "coxswain-cni-probe",
			Containers: []corev1.Container{
				{
					Name:    "probe",
					Image:   p.cfg.ProbeImage,
					Command: p.probeCommand(),
					// terminationMessagePolicy FallbackToLogsOnError: the probe's
					// RESULT lines go to stdout, captured into the container's
					// terminationMessage (design point 2). The output (4 RESULT
					// lines + DONE) is far under the 4 KB termination-message
					// limit.
					TerminationMessagePath:   "/dev/termination-log",
					TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					SecurityContext: &corev1.SecurityContext{
						RunAsNonRoot:             new(bool),
						RunAsUser:                &nonRoot,
						AllowPrivilegeEscalation: new(bool),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						ReadOnlyRootFilesystem:   new(bool),
					},
				},
			},
			// The probe needs no K8s API access (the operator reads the
			// termination message off the pod object). Disabling the token
			// mount keeps the probe pod zero-credential (ADR-0006).
			AutomountServiceAccountToken: new(bool),
		},
	}
}

// containerTerminationMessage returns the probe container's termination
// message once the container has terminated. The probe script writes its
// RESULT lines to /dev/termination-log itself (see probeCommand); Fallback
// to logs on error covers a FAILED exit.
func containerTerminationMessage(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "probe" {
			if cs.State.Terminated != nil {
				return cs.State.Terminated.Message
			}
		}
	}
	return ""
}

// waitForTermination polls the pod until the probe container has terminated
// (its termination message is then available) or the context is done. A
// not-Ready / no-exit / pull-failure within the timeout is a ProbeUnavailable
// (the caller maps it).
//
// Once the container has Terminated the message is read — even if it is
// EMPTY. An empty message is a hard failure ("empty termination message" →
// ProbeUnavailable), not a "keep waiting" state: on a non-zero exit
// FallbackToLogsOnError substitutes the logs, and on a clean exit the script
// itself writes /dev/termination-log. A Terminated container with an empty
// message from either path will never produce one — spinning to the timeout
// (60s per run) would just delay the same fail-closed result.
func (p *PodProber) waitForTermination(ctx context.Context, pod *corev1.Pod) (string, error) {
	probe := &corev1.Pod{}
	reader := p.getReader()
	for {
		if err := reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: pod.Name}, probe); err != nil {
			return "", fmt.Errorf("probe pod get: %w", err)
		}
		if podTerminated(probe, "probe") {
			msg := containerTerminationMessage(probe)
			if msg == "" {
				return "", fmt.Errorf("empty termination message (probe container terminated with no output)")
			}
			return msg, nil
		}
		if probe.Status.Phase == corev1.PodFailed {
			return "", fmt.Errorf("probe pod failed")
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("probe pod not Ready within timeout")
		case <-time.After(2 * time.Second):
		}
	}
}

// parse strictly validates the termination message: exactly expectedResultLen
// RESULT lines, each a known label, every line consumed. A non-zero pod exit or
// any deviation returns ProbeUnavailable.
func (p *PodProber) parse(msg string, pod *corev1.Pod) (CNIProbeResult, error) {
	// A non-zero exit (container terminated with a non-zero code) is
	// ProbeUnavailable even if the message looks valid.
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			return unavailable("probe exited non-zero: " + fmt.Sprint(cs.State.Terminated.ExitCode)), nil
		}
	}
	var rows []TargetRow
	reachable := false
	seen := 0
	for line := range strings.SplitSeq(strings.TrimSpace(msg), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "DONE" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != "RESULT" {
			return unavailable("unrecognised probe line: " + line), nil
		}
		label, verdict := fields[1], fields[2]
		if verdict != "REACHABLE" && verdict != "BLOCKED" {
			return unavailable("unrecognised verdict: " + label + " " + verdict), nil
		}
		if !knownProbeLabels[label] {
			return unavailable("unknown probe target label: " + label), nil
		}
		seen++
		if verdict == "REACHABLE" {
			reachable = true
		}
		rows = append(rows, TargetRow{Label: label, Verdict: verdict})
	}
	if seen != expectedResultLen {
		return unavailable(fmt.Sprintf("expected %d RESULT lines, got %d", expectedResultLen, seen)), nil
	}
	res := CNIProbeResult{Rows: rows}
	if reachable {
		res.Reason = ReasonCNIUnenforced
	} else {
		res.Reason = ReasonCNIEnforced
	}
	// Record the result in the holder (the reconcile loop reads it via
	// LatestResult).
	Holder().Set(res)
	return res, nil
}

// unavailable builds a ProbeUnavailable result.
func unavailable(detail string) CNIProbeResult {
	return CNIProbeResult{Reason: ReasonProbeUnavailable, Detail: detail}
}

func ptrProtocolTCP() *corev1.Protocol {
	tcp := corev1.ProtocolTCP
	return &tcp
}

func ptrPort(i int32) *intstr.IntOrString {
	p := intstr.FromInt32(i)
	return &p
}
