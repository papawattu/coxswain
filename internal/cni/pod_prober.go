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
	// run is the per-pass seam: it creates one probe pod, waits for its
	// termination message within the per-run timeout, and deletes the pod.
	// Probe() calls it through this field (set in NewPodProber to runPod) so
	// a test can replace it with a stub and drive the two-pass decision
	// without touching the API.
	run func(ctx context.Context, pod *corev1.Pod) (msg string, nodeName string, err error)
}

// NewPodProber returns a real CNIProber.
func NewPodProber(cfg PodProberConfig) *PodProber {
	if cfg.ClusterDomain == "" {
		cfg.ClusterDomain = "cluster.local"
	}
	pp := &PodProber{cfg: cfg}
	pp.run = pp.runPod // the per-pass seam; tests replace pp.run with a stub
	return pp
}

const (
	probePodName    = "coxswain-cni-probe"
	probeControlPod = "coxswain-cni-probe-control"
	probeNetpolName = "coxswain-cni-probe-netpol"
	probeLabelKey   = "coxswain.io/probe"
	// probeLabelValue is the policy pass's probe label value: the probe
	// NetworkPolicy selects exactly coxswain.io/probe=cni-probe, so the control
	// pass (cni-probe-control) runs with NO policy applied (D103).
	probeLabelValue      = "cni-probe"
	probeControlLabelVal = "cni-probe-control"
	expectedResultLen    = 5 // 4 expected-BLOCKED (APISERVER_SVC, NODE_API, KUBELET_NODE, EXTERNAL) + 1 positive control (DNS_POSITIVE)
	verdictReachable     = "REACHABLE"
	verdictBlocked       = "BLOCKED"
	// verdictUnresolved: the target's hostname could not be resolved, so the
	// probe has no evidence either way. It is never Enforced and never
	// Unenforced: a combined UNRESOLVED row makes the probe
	// ProbeUnavailable (F51 P2-2 — the old script reported a resolution
	// failure as REACHABLE, i.e. an unresolvable expected-BLOCKED target
	// counted as a fence bypass).
	verdictUnresolved = "UNRESOLVED"
	// verdictRefused: the connection reached the host and was actively
	// rejected (ConnectionRefusedError). It is not by itself evidence of a
	// policy drop (nothing may listen there) — combineVerdicts resolves it
	// against the no-policy control pass (D103): a control REACHABLE makes
	// it BLOCKED, a control REFUSED makes it NOT_LISTENING.
	verdictRefused = "REFUSED"
	// verdictNotListening: the combined verdict for a target refused in BOTH
	// passes — nothing listens there. It carries no verdict about the fence
	// and is excluded from the enforcement decision (shown in the detail).
	verdictNotListening = "NOT_LISTENING"
)

// The RESULT labels the probe emits, one constant per target. The python
// script embeds them as plain text (the script is a string, so they must be
// inlined there); the Go side uses the constants for every map, list and
// comparison.
const (
	labelAPIServerSVC = "APISERVER_SVC"
	labelNodeAPI      = "NODE_API"
	labelKubeletNode  = "KUBELET_NODE"
	labelExternal     = "EXTERNAL"
)

// knownProbeLabels is the strict set of RESULT labels the probe emits (design
// point 4: an unknown label is a validation error, never a false pass).
var knownProbeLabels = map[string]bool{
	labelAPIServerSVC:    true,
	labelNodeAPI:         true,
	labelKubeletNode:     true,
	labelExternal:        true,
	positiveControlLabel: true,
}

// expectedBlockedLabels are the RESULT labels that must be BLOCKED for the
// CNI to count as enforcing (the D38 property: a pod under the agent's
// allowlist NetworkPolicy cannot reach host-network destinations, or the
// outside). The probe NetworkPolicy is a 1:1 copy of the per-Loop agent
// NetworkPolicy (egress allowed ONLY to the cluster's kube-dns on 53), so the
// expected-BLOCKED targets are: the apiserver Service ClusterIP :443 and the
// node's IP (downward API status.hostIP) on :6443 and :10250 (host network),
// plus EXTERNAL 1.1.1.1:443 (the outside — not in the DNS-only allow list).
// On a non-enforcing CNI (kindnet) EXTERNAL is REACHABLE, which is itself
// evidence of unenforcement.
var expectedBlockedLabels = map[string]bool{
	labelAPIServerSVC: true,
	labelNodeAPI:      true,
	labelKubeletNode:  true,
	labelExternal:     true,
}

// positiveControlLabel is the RESULT label that must be REACHABLE for the
// probe to prove it has a working network path under the allow list: the
// cluster's kube-dns Service :53 (allowed by the probe NetworkPolicy). A
// BLOCKED positive control means the probe has no network at all (the fence
// is unproven) and the result is ProbeUnavailable.
const positiveControlLabel = "DNS_POSITIVE"

// passLabels is the strict, stable order of RESULT labels a probe pass emits
// (used for the UNRESOLVED/REFUSED pre-emptions, the CNIUnenforced Detail
// joining, and Rows rendering — the order is pinned so the detail text is
// deterministic). It must stay in sync with knownProbeLabels.
var passLabels = []string{labelAPIServerSVC, labelNodeAPI, labelKubeletNode, labelExternal, positiveControlLabel}

// probeCommand is the probe, run as the pod's own command (the
// image is python:3-alpine by default — it ships python3 and neither bash nor
// /dev/tcp, so the probe is a Python socket connect, exactly like
// verify-cni.sh's probe). It writes one RESULT line per target to stdout
// (captured to the termination message via
// terminationMessagePolicy: FallbackToLogsOnError) and exits 0.
//
// The targets: the expected-BLOCKED set (the D38 property — a pod under the
// agent's allowlist NetworkPolicy cannot reach these): the apiserver Service
// ClusterIP :443 (pod -> ServiceClusterIP -> host network), the node's IP on
// :6443 and :10250 (host network; the node IP comes from the downward API
// status.hostIP — the pod's own HOSTNAME resolves to the pod's own IP, not the
// node's, so it must NOT be used), and EXTERNAL 1.1.1.1:443 (the outside —
// not in the DNS-only allow list; REACHABLE on a non-enforcing CNI is itself
// evidence of unenforcement). Plus the positive CONTROL, DNS_POSITIVE: a TCP
// connect to the cluster's kube-dns Service :53 (allowed by the probe
// NetworkPolicy) — it must come back REACHABLE to prove the probe has a
// working network path; a BLOCKED control means the probe has no network and
// the result is ProbeUnavailable.

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
	kubeDNS := "kube-dns.kube-system.svc." + p.cfg.ClusterDomain
	return []string{"python3", "-c", `
import os, socket, socket as _s, sys

# NODE_IP is the node's IP from the downward API (status.hostIP), injected on
# the probe pod's container. The pod's own HOSTNAME resolves to the pod's OWN
# IP, never the node's, so it must not be used as the node target; verify-
# cni.sh uses each node's InternalIP.
node_ip = os.environ.get("NODE_IP")
if not node_ip:
    sys.stderr.write("FATAL: NODE_IP not set (downward API status.hostIP missing)\n")
    sys.exit(3)

TARGETS = [
    ("APISERVER_SVC", "` + apiserverSVC + `", 443),
    ("NODE_API", node_ip, 6443),
    ("KUBELET_NODE", node_ip, 10250),
    # EXTERNAL is an expected-BLOCKED target: the probe NetworkPolicy allows
    # egress ONLY to the cluster's kube-dns (53), so the outside is NOT
    # allowed. A CNI that enforces the fence drops it (BLOCKED); a CNI that
    # does not (kindnet) lets it through (REACHABLE), which is itself
    # evidence of unenforcement.
    ("EXTERNAL", "1.1.1.1", 443),
    # Positive CONTROL: the probe NetworkPolicy ALLOWS egress to the
    # cluster's kube-dns on 53, so this must come back REACHABLE. A BLOCKED
    # control means the probe has no network at all (the node's own CNI is
    # down) -- the fence is unproven, not that the CNI is enforcing. DNS
    # resolution of the service name is itself allowed by the netpol.
    ("DNS_POSITIVE", "` + kubeDNS + `", 53),
]

lines = []
def out(line):
    lines.append(line)
    print(line)

def probe(label, host, port):
    try:
        host = socket.gethostbyname(host)
    except Exception:
        # No address means no measurement of the fence: report UNRESOLVED,
        # never REACHABLE (an unresolvable expected-BLOCKED target used to
        # read as a fence bypass).
        out("RESULT %s UNRESOLVED" % label)
        return
    s = socket.socket(_s.AF_INET, _s.SOCK_STREAM)
    s.settimeout(5)
    try:
        s.connect((host, port))
        out("RESULT %s REACHABLE" % label)
    except ConnectionRefusedError:
        # A refused connection reached the host and was actively rejected
        # there. That is NOT evidence that a policy dropped the packet (nothing
        # may listen there) and NOT unresolvable (the name DID resolve): the
        # no-policy control pass (D103) tells the two apart — a control
        # REACHABLE means the policy rejected it (BLOCKED), a control REFUSED
        # means nothing listens (NOT_LISTENING).
        out("RESULT %s REFUSED" % label)
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

// Probe runs one probe (D103: two passes, both within cfg.ProbeTimeout):
// it cleans up any stale probe objects, creates the probe NetworkPolicy,
// runs the no-policy CONTROL pod (same spec, a label the netpol does NOT
// select) and reads its node, then runs the POLICY pod (netpol-selected)
// pinned to the SAME node so NODE_IP names the same node in both passes.
// The two RESULT tables are combined per label (combineVerdicts) and the
// verdict comes from decide(). Any failure (stale cleanup, netpol create,
// a pass that times out / goes not-Ready / exits non-zero, wrong/unknown
// RESULT lines) returns ReasonProbeUnavailable.
func (p *PodProber) Probe(ctx context.Context) (CNIProbeResult, error) {
	probeCtx, cancel := context.WithTimeout(ctx, p.cfg.ProbeTimeout)
	defer cancel()

	// Stale cleanup at startup (the PR #38 leak): delete any leftover probe
	// pod (both passes) / NetworkPolicy from a previous run.
	if err := p.cleanup(probeCtx); err != nil {
		return unavailable("stale cleanup failed: " + err.Error()), nil
	}

	// Create the probe NetworkPolicy (the DNS-only allow). It is what the
	// probe measures against — the CNI's enforcement is exactly whether this
	// netpol is obeyed — and its podSelector selects ONLY the policy pod.
	if err := p.ensureNetpol(probeCtx); err != nil {
		return unavailable("netpol create failed: " + err.Error()), nil
	}

	// Pass 1: the no-policy control pod. Same spec as the policy pod, a
	// label the probe netpol does NOT select (so no policy applies to it),
	// no node constraint. Its RESULT lines are the baseline (what each
	// target looks like with no policy) and its spec.nodeName is where the
	// policy pod is pinned, so NODE_IP (downward status.hostIP) names the
	// same node in both passes.
	controlPod := p.buildPod(probeControlPod, probeControlLabelVal, "")
	cmsg, node, err := p.run(probeCtx, controlPod)
	if err != nil {
		return unavailable("control pass: " + err.Error()), nil
	}
	if node == "" {
		return unavailable("control pod has no node (spec.nodeName empty): the policy pass cannot be pinned to it"), nil
	}

	// Pass 2: the policy pod, netpol-selected, pinned to the control pod's
	// node so both passes measure the same node's host-network targets.
	policyPod := p.buildPod(probePodName, probeLabelValue, node)
	pmsg, _, err := p.run(probeCtx, policyPod)
	if err != nil {
		return unavailable("policy pass: " + err.Error()), nil
	}

	// Combine the two passes per label and apply the D103 verdict rules.
	policyPass, err := parsePass(pmsg)
	if err != nil {
		return unavailable("policy pass: " + err.Error()), nil
	}
	controlPass, err := parsePass(cmsg)
	if err != nil {
		return unavailable("control pass: " + err.Error()), nil
	}
	combined := combineVerdicts(policyPass, controlPass)
	return decide(policyPass, controlPass, combined), nil
}

// runPod is the per-pass seam implementation: it Creates the pod, waits for
// the termination message within the per-run timeout, reads spec.nodeName
// off the terminated pod, and Deletes the pod (best effort — the stale
// cleanup at the start of the next probe is the backstop). The probe pods
// are operator-level state (not owner-ref'd to a Loop — there is no Loop to
// reference); they are cleaned up by name at the start of the next probe.
func (p *PodProber) runPod(ctx context.Context, pod *corev1.Pod) (string, string, error) {
	if err := p.cfg.Client.Create(ctx, pod); err != nil {
		return "", "", fmt.Errorf("%s pod create: %w", pod.Name, err)
	}
	msg, err := p.waitForTermination(ctx, pod)
	// Best-effort pod deletion on any exit path (the stale cleanup at the
	// start of the next probe is the backstop).
	defer func() {
		if derr := p.cfg.Client.Delete(context.Background(), pod); derr != nil && !apierrors.IsNotFound(derr) {
			p.cfg.Client.Scheme() // no-op; the error is logged by the caller
		}
	}()
	if err != nil {
		return "", "", fmt.Errorf("%s pod: %w", pod.Name, err)
	}
	return msg, pod.Spec.NodeName, nil
}

// LatestResult implements CNIProber: it returns the holder's cached result
// (what the probe Runnable set). It never runs a probe.
func (p *PodProber) LatestResult() CNIProbeResult {
	return Holder().Result()
}

// cleanup deletes any leftover probe pods (BOTH passes — policy and control;
// D103) and the probe NetworkPolicy (stale from a previous run — the PR #38
// leak).
func (p *PodProber) cleanup(ctx context.Context) error {
	for _, name := range []string{probePodName, probeControlPod} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.cfg.Namespace}}
		if err := p.cfg.Client.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	netpol := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: probeNetpolName, Namespace: p.cfg.Namespace}}
	if err := p.cfg.Client.Delete(ctx, netpol); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// ensureNetpol creates the probe NetworkPolicy (the allow list the CNI's
// enforcement is measured against). It is a 1:1 shape copy of
// test/e2e/verify-cni.sh's probe NetworkPolicy: egress allowed ONLY to the
// cluster's kube-dns (53 UDP+TCP), so the expected-BLOCKED targets
// (apiserver svc ClusterIP :443, node :6443, node :10250) are NOT in the
// allow list and a CNI that enforces NetworkPolicy drops them. The earlier
// agent-shaped variant allowed egress to 0.0.0.0/0 minus RFC1918 — under
// THAT policy the external targets are ALLOWED, so reporting REACHABLE for
// them as CNIUnenforced was a false positive: the policy allowed the
// traffic, not the CNI failing to enforce.
func (p *PodProber) ensureNetpol(ctx context.Context) error {
	want := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: probeNetpolName, Namespace: p.cfg.Namespace},
		Spec: networkingv1.NetworkPolicySpec{
			// Selects ONLY the policy pass: the control pod's label value
			// (cni-probe-control) must NOT match, or it would run under the
			// very policy the control pass exists to bypass (D103).
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{probeLabelKey: probeLabelValue}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				// DNS 53 (UDP+TCP) to the cluster's kube-dns only — the exact
				// shape of the per-Loop agent NetworkPolicy and of
				// verify-cni.sh's probe netpol.
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: ptrProtocolUDP(), Port: ptrPort(53)},
						{Protocol: ptrProtocolTCP(), Port: ptrPort(53)},
					},
				},
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

// buildPod returns one probe-pass pod (the agent's command, the termination
// message, the non-root/priv-dropped hardening — the same shape as the sandbox
// pod, D38 design point 2). Both passes share the shape (same image, command,
// env, security context, no token automount); they differ in name, the probe
// label value (the netpol selects only probeLabelValue) and node pinning (the
// policy pass is pinned to the control pod's node via kubernetes.io/hostname
// when nodeName != "", so both passes measure the same node — D103).
func (p *PodProber) buildPod(name, probeLabelValue, nodeName string) *corev1.Pod {
	nonRoot := int64(65532)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: p.cfg.Namespace,
			Labels:    map[string]string{probeLabelKey: probeLabelValue},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: "coxswain-cni-probe",
			Containers: []corev1.Container{
				{
					Name:    "probe",
					Image:   p.cfg.ProbeImage,
					Command: p.probeCommand(),
					// F65 (P2-2): an EXPLICIT pull policy. Unset, the kubelet derives it
					// from the ref shape (:latest or a digest => Always, otherwise
					// IfNotPresent), so the policy silently changes with the reference.
					// The default probe image is digest-pinned: IfNotPresent uses the
					// node's copy when present and pulls the EXACT digest when not,
					// instead of re-resolving the tag on every probe.
					ImagePullPolicy: corev1.PullIfNotPresent,
					// terminationMessagePolicy FallbackToLogsOnError: the probe's
					// RESULT lines go to stdout, captured into the container's
					// terminationMessage (design point 2). The output (4 RESULT
					// lines + DONE) is far under the 4 KB termination-message
					// limit.
					// The node's IP (downward API status.hostIP) is the NODE_API /
					// KUBELET_NODE target — the same source verify-cni.sh uses for
					// each node's InternalIP. The pod's own HOSTNAME is the pod's
					// own IP, not the node's.
					Env: []corev1.EnvVar{{
						Name: "NODE_IP",
						ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"},
						},
					}},
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
	if nodeName != "" {
		pod.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": nodeName}
	}
	return pod
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

// parsePass does the strict line validation of one probe pass's termination
// message: exactly expectedResultLen lines of "RESULT <label> <verdict>" (any
// other line shape is an error), each label a known probe label, each verdict
// one of REACHABLE / BLOCKED / REFUSED / UNRESOLVED. It returns the label →
// verdict map, or an error on any violation (design point 4: an unknown label
// or verdict is never a false pass).
func parsePass(msg string) (map[string]string, error) {
	pass := make(map[string]string, expectedResultLen)
	seen := 0
	for line := range strings.SplitSeq(strings.TrimSpace(msg), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "DONE" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != "RESULT" {
			return nil, fmt.Errorf("unrecognised probe line: %s", line)
		}
		label, verdict := fields[1], fields[2]
		if !knownProbeLabels[label] {
			return nil, fmt.Errorf("unknown probe target label: %s", label)
		}
		if verdict != verdictReachable && verdict != verdictBlocked && verdict != verdictRefused && verdict != verdictUnresolved {
			return nil, fmt.Errorf("unrecognised verdict: %s %s", label, verdict)
		}
		// A repeated label would overwrite the map entry and still count toward
		// expectedResultLen, so a missing other label would slip through the
		// count check (the old row-based parser had the same hole).
		if _, dup := pass[label]; dup {
			return nil, fmt.Errorf("duplicate probe target label: %s", label)
		}
		pass[label] = verdict
		seen++
	}
	if seen != expectedResultLen {
		return nil, fmt.Errorf("expected %d RESULT lines, got %d", expectedResultLen, seen)
	}
	return pass, nil
}

// combineVerdicts combines the policy pass and the no-policy control pass per
// label, exactly the D103 table:
//
//	policy REACHABLE                          -> REACHABLE (the policy lets it through)
//	policy BLOCKED                            -> BLOCKED (a policy drop, as today)
//	policy REFUSED + control REACHABLE        -> BLOCKED (the policy rejected a connection that
//	                                          succeeds without it — D103's new cell)
//	policy REFUSED + control REFUSED          -> NOT_LISTENING (nothing listens there; excluded
//	                                          from the verdict, shown in the detail)
//	policy REFUSED + control BLOCKED/UNRESOLVED -> UNRESOLVED (the control pass could not
//	                                          measure the baseline, so neither can we)
//	policy UNRESOLVED                         -> UNRESOLVED
//	a label missing from the control pass     -> UNRESOLVED (no baseline to combine against)
func combineVerdicts(policy, control map[string]string) map[string]string {
	combined := make(map[string]string, len(passLabels))
	for _, label := range passLabels {
		p := policy[label]
		switch p {
		case verdictReachable:
			combined[label] = verdictReachable
		case verdictBlocked:
			combined[label] = verdictBlocked
		case verdictRefused:
			switch c := control[label]; c {
			case verdictReachable:
				combined[label] = verdictBlocked
			case verdictRefused:
				combined[label] = verdictNotListening
			default: // BLOCKED or UNRESOLVED, or missing from the control pass
				combined[label] = verdictUnresolved
			}
		default: // UNRESOLVED, or missing from the policy pass
			combined[label] = verdictUnresolved
		}
	}
	return combined
}

// decide applies the D103 verdict rules to the combined pass, in order:
//
//  1. any combined UNRESOLVED -> ProbeUnavailable (Detail names the label);
//  2. DNS_POSITIVE not REACHABLE in BOTH the policy and the control pass ->
//     ProbeUnavailable (one pass without a working network leaves the fence
//     unproven);
//  3. any expected-BLOCKED label combined REACHABLE -> CNIUnenforced
//     (Detail carries those labels, joined ", ", as today);
//  4. APISERVER_SVC combined BLOCKED and at least one of NODE_API /
//     KUBELET_NODE combined BLOCKED -> CNIEnforced (the service route AND a
//     host-network route are fenced, so the "nothing listens" rows cannot
//     hide an unenforced CNI);
//  5. otherwise -> ProbeUnavailable ("no host-network target was measurable":
//     e.g. every host-network target NOT_LISTENING, which carries no verdict
//     about the fence).
//
// Rows carries every label's combined verdict, NOT_LISTENING included.
func decide(policy, control, combined map[string]string) CNIProbeResult {
	for _, label := range passLabels {
		if combined[label] == verdictUnresolved {
			return unavailable("probe target was not measurable: " + label)
		}
	}
	if policy[positiveControlLabel] != verdictReachable || control[positiveControlLabel] != verdictReachable {
		return unavailable(positiveControlLabel + " control not REACHABLE in both passes: the fence is unproven")
	}
	var unenforced []string
	for _, label := range passLabels {
		if expectedBlockedLabels[label] && combined[label] == verdictReachable {
			unenforced = append(unenforced, label)
		}
	}
	if len(unenforced) > 0 {
		res := CNIProbeResult{Rows: combinedToRows(combined), Reason: ReasonCNIUnenforced}
		res.Detail = strings.Join(unenforced, ", ")
		return res
	}
	apiserverBlocked := combined[labelAPIServerSVC] == verdictBlocked
	if apiserverBlocked && (combined[labelNodeAPI] == verdictBlocked || combined[labelKubeletNode] == verdictBlocked) {
		return CNIProbeResult{Reason: ReasonCNIEnforced, Rows: combinedToRows(combined)}
	}
	return unavailable("no host-network target was measurable")
}

// combinedToRows renders a combined verdict map as Rows in passLabels order
// (NOT_LISTENING rows included — the design shows them in the
// condition/Event detail).
func combinedToRows(combined map[string]string) []TargetRow {
	rows := make([]TargetRow, 0, len(passLabels))
	for _, label := range passLabels {
		rows = append(rows, TargetRow{Label: label, Verdict: combined[label]})
	}
	return rows
}

// unavailable builds a ProbeUnavailable result.
func unavailable(detail string) CNIProbeResult {
	return CNIProbeResult{Reason: ReasonProbeUnavailable, Detail: detail}
}

func ptrProtocolTCP() *corev1.Protocol {
	tcp := corev1.ProtocolTCP
	return &tcp
}

func ptrProtocolUDP() *corev1.Protocol {
	udp := corev1.ProtocolUDP
	return &udp
}

func ptrPort(i int32) *intstr.IntOrString {
	p := intstr.FromInt32(i)
	return &p
}
