/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	netip "net/netip"
	neturl "net/url"
	"os"
	"strconv"
	"strings"

	"time"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
	"github.com/papawattu/coxswain/internal/egress"
	"github.com/papawattu/coxswain/internal/engine"
	"github.com/papawattu/coxswain/internal/policy"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// C2 (ADR-0006 item 2): the model proxy sidecar contract. The agent holds no
// model key; it talks to the local proxy on COX_MODEL_BASE_URL, which holds the
// key (mounted only into the proxy) and injects auth.
const (
	// proxyContainerName is the name of the model proxy sidecar container.
	// modelCredsVolume is the name of the Secret volume that carries the model
	// API key + base URL (mounted read-only into the proxy only).
	modelCredsVolume = "model-creds"
	// coxModelBaseURL is the env var the operator sets on the agent so it talks
	// to the local proxy (a Loop cannot override it: COX_* names are rejected
	// at admission, I34).
	coxModelBaseURL = "COX_MODEL_BASE_URL"
	// localhostProxyBaseURL is where the proxy listens on the sandbox pod's
	// loopback interface. The agent reaches it over localhost, not the network.
	// readOnlyMode is the default file mode for the model-creds Secret volume
	// (0444: the key is read-only, even in the proxy).
	readOnlyMode int32 = 0o444

	// The agent's writable mount points. Single source of truth for the pod spec;
	// the AgentPolicy exec XValidation (api/v1alpha1/agentpolicy_types.go) MUST
	// stay in sync with this set — a CRD CEL rule cannot reference Go code, so
	// adding a mount here without updating the XValidation would silently make
	// the new mount a spoofable exec target.
	agentWorkspaceMount = "/workspace"
	agentScratchMount   = "/scratch"
	agentTmpMount       = "/tmp"
	// allCaps is the capability name for the Drop: ALL security context.
	allCaps corev1.Capability = "ALL"
)

// LoopReconciler reconciles a Loop object.
//
// Phase 0 scope: ensure the Loop's Sandbox exists and log it. The phase state
// machine (Planning → Implementing → Verifying, budgets, stall, checkpoints)
// lands in Phase 1 (docs/PLAN.md).
type LoopReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Enforcer is the eBPF engine seam (ADR-0007 Q3/D30). When set, the operator
	// emits the Loop's effective policy through the engine and gates the sandbox
	// on positive evidence that enforcement is active (D30). When nil, the
	// operator treats enforcement as unavailable (PolicyEnforced=False,
	// reason EngineUnavailable) and holds the sandbox Suspended (fail-closed).
	Enforcer engine.Enforcer
	// AllowUnenforced is the off-by-default escape hatch (P1 merge): when true,
	// Loops run with PolicyEnforced=False reason EnforcementDisabled rather
	// than being held Suspended. For dev/kind only — never in production.
	AllowUnenforced bool

	// CNIProber is the network-layer seam the operator uses (D38), analogous to
	// Enforcer (D30). When set, the operator gates the sandbox on the CNI probe
	// result (NetworkEnforced condition): a CNI that does not police pod ->
	// host-network egress — or an unavailable probe — holds the sandbox
	// Suspended (fail-closed) unless AllowUnenforcedNetwork is set.
	// When nil, the gate applies from the shared holder (cni.Holder()), which
	// starts Unknown — and Unknown holds the sandbox Suspended. The probe
	// itself runs in the leader-elected Runnable (internal/cni); the reconcile
	// only reads the current result via LatestResult().
	CNIProber cni.CNIProber
	// AllowUnenforcedNetwork is the off-by-default network escape hatch (D38),
	// separate from AllowUnenforced (the eBPF gate): when true, Loops run with
	// NetworkEnforced=False reason EnforcementDisabled rather than being held
	// Suspended. For dev/kind only — never in production.
	AllowUnenforcedNetwork bool

	// CNIRegateSource is the source the Loop controller watches for CNI probe
	// re-gate requests (D38, design point 3a). When set, SetupWithManager adds
	// a WatchesRawSource on it; a result change enqueues every Loop so it
	// re-reads the cached probe result and re-gates. When nil (envtest) the
	// controller only re-gates on its normal watch (the probe result is read on
	// each reconcile anyway).
	CNIRegateSource source.TypedSource[reconcile.Request]

	// Recorder emits the Kubernetes Event on a NetworkEnforced condition
	// change (D38 design point 3). nil (most envtests) = no Event.
	Recorder record.EventRecorder

	// SandboxImage is the image the sandbox pod runs. Defaults to a Go dev
	// image; overridable for the smoke test (e.g. the runner image).
	SandboxImage string

	// ProxyImage is the model proxy sidecar image (C2). Defaults to a Go dev
	// stand-in; overridable for the smoke test (e.g. the real proxy image).
	ProxyImage string

	// EgressProxyImage is the egress proxy sidecar image (I42b). Defaults to a
	// Go dev stand-in; overridable for the smoke test (e.g. the real egress
	// proxy image). When the effective policy has no network allows, the
	// egress proxy pod is not created and this image is unused.
	EgressProxyImage string

	// PodCIDR / ServiceCIDR are the cluster's pod and service CIDRs (I42e +
	// I42c NetworkPolicy carve-outs). Read from the operator's environment
	// (POD_CIDR / SERVICE_CIDR) at startup unless overridden here (tests).
	// When empty the controller-side in-cluster check skips the CIDR cases
	// (the CRD CEL rule still rejects .svc / localhost), and I42c's
	// egress-proxy NetworkPolicy omits the operator-supplied carve-outs.
	PodCIDR     string
	ServiceCIDR string

	// ClusterDomain is the cluster's service DNS domain (default
	// cluster.local, settable in tests). Used by proxyServiceURL /
	// egressProxyServiceURL / egressNOProxy (the agent's proxy env URLs) and
	// by the FQDNs the Enforcer puts on the agent's DNS allowlist, so a
	// non-default-domain cluster is supported. (R16 I44 item 2.)
	ClusterDomain string
}

// +kubebuilder:rbac:groups=coxswain.wattu.com,resources=loops,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coxswain.wattu.com,resources=loops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=coxswain.wattu.com,resources=loops/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes/status,verbs=get
// +kubebuilder:rbac:groups=coxswain.wattu.com,resources=agentpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=security.kubearmor.com,resources=kubearmorpolicies,verbs=get;list;watch;create;update;patch;delete
// D33: the operator owns the per-Loop proxy pod + Service (ensureProxy).
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update
// D34: the operator creates the per-Loop NetworkPolicies (ensureNetworkPolicy).
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// D38: the operator emits a Kubernetes Event on every Loop when its
// NetworkEnforced condition changes (the probe Runnable's re-gate Event lives
// in internal/cni; this is the condition-change Event the reconcile side emits).
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile moves the cluster state closer to the Loop's desired state.
func (r *LoopReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var loop coxv1alpha1.Loop
	if err := r.Get(ctx, req.NamespacedName, &loop); err != nil {
		// Deleted or never existed: nothing to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// C6a (R15 round 3): validate the referenced AgentPolicies BEFORE
	// creating the sandbox (defence in depth: the CRD CEL catches the common
	// case at admission; this catches anything that slips through, including
	// missing/unreadable policies). If validation fails, set
	// PolicyValid=False and suspend the sandbox (if running).
	if polResult := r.validateAgentPolicies(ctx, &loop); !polResult.valid {
		setCondition(&loop, "PolicyValid", metav1.ConditionFalse, polResult.reason, polResult.message)
		// Suspend an already-running sandbox (D30-gate pattern): set
		// operatingMode to 0 so the sandbox pod is scaled down.
		if err := r.suspendSandboxIfRunning(ctx, &loop); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Status().Update(ctx, &loop); err != nil {
			return ctrl.Result{}, err
		}
		if polResult.transientReadError {
			// Requeue: the policy could not be read due to a transient
			// error. Retry after a short delay.
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, nil
	}
	// Clean pass: set PolicyValid=True (R15 round 3: nothing ever set it
	// True before, so a fixed path left the old False condition forever).
	if len(loop.Spec.PolicyRefs) > 0 {
		setCondition(&loop, "PolicyValid", metav1.ConditionTrue, "Valid", "all referenced AgentPolicies are valid")
	}

	// D38: capture the condition set before applyEffectivePolicyAndConditions so
	// a condition-only change (e.g. the NetworkEnforced reason flipping on a
	// probe result change) still triggers a status update. Without this, the
	// "re-gate on flip" spec would never persist the new condition.
	condsBefore := make([]metav1.Condition, len(loop.Status.Conditions))
	copy(condsBefore, loop.Status.Conditions)

	if err := r.applyEffectivePolicyAndConditions(ctx, &loop); err != nil {
		return ctrl.Result{}, err
	}
	// D30 gate: if the engine is not enforcing and AllowUnenforced is not set,
	// the sandbox stays Suspended (fail-closed). An invalid policy (C6a) ALSO
	// suspends via validateAgentPolicies + suspendSandboxIfRunning above. Both
	// gates apply: an invalid policy suspends, and unenforced also suspends.

	// D34 (P1): the endpointSecretRef + modelEndpoint pair is required. A Secret
	// without an endpoint (or an endpoint without a Secret) is a misconfiguration
	// that would crash-loop the proxy; reject it early.
	if loop.Spec.Agent.EndpointSecretRef != "" && loop.Spec.Agent.ModelEndpoint == "" {
		setCondition(&loop, "ModelConfigValid", metav1.ConditionFalse, "MissingModelEndpoint",
			"spec.agent.endpointSecretRef is set but spec.agent.modelEndpoint is empty; the pair is required")
		if err := r.Status().Update(ctx, &loop); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if err := r.ensureSandbox(ctx, &loop); err != nil {
		return ctrl.Result{}, err
	}

	// D33+D34+D35: the per-Loop proxy, NetworkPolicies, and condition snapshot.
	changed, err := r.ensureProxyAndNetPolicies(ctx, &loop)
	if err != nil {
		return ctrl.Result{}, err
	}
	if loop.Status.ObservedGeneration != loop.Generation {
		loop.Status.ObservedGeneration = loop.Generation
		changed = true
	}
	// Phase 0: a fresh Loop is Pending. (Phase 1 drives the full phase machine.)
	if loop.Status.Phase == "" {
		loop.Status.Phase = coxv1alpha1.LoopPhasePending
		changed = true
	}
	// B1: advance the phase machine when the runner reports a valid forward step.
	// nextPhase(phase, observedPhase) returns the next phase when observedPhase
	// is the immediate-next phase after the operator's current phase, and leaves
	// it unchanged otherwise (terminal phases, missing/garbled reports). The
	// operator never interprets runner output beyond this pure match
	// (ADR-0004). The iterate/terminal branches (Verifying -> Implementing /
	// Failed) are completed by B3 (verify outcome) and B4 (iteration count).
	if next := nextPhase(loop.Status.Phase, loop.Status.ObservedPhase); next != loop.Status.Phase {
		loop.Status.Phase = next
		loop.Status.DesiredPhase = next
		changed = true
	}
	// B2 (D10/D24): at Verifying, the operator's own tamper evidence is the
	// gate. tamperVerdict is a tri-state over the operator's evidence (a pointer
	// to the terminated tamper init container's exit code + the verifiedCommit
	// it names), and never reads the runner's result.json claim:
	//   - TamperTampered -> Failed:TamperedVerify, TERMINAL, before any check
	//     runs (the anti-gaming property: a runner that edited a protected file
	//     and reported success still ends Failed).
	//   - TamperClean    -> not a B2 decision; B3's check containers decide the
	//     outcome from there.
	//   - TamperUnknown  -> no evidence (nil, or stale for a different
	//     verifiedCommit/Job); NEVER treated as clean (D24 fail-closed), so the
	//     Loop stays in Verifying and B3 cannot reach Succeeded on it.
	// In envtest the B-slice tests set status.verify.* directly (no Job
	// controller); in a real cluster B3 reads the tamper exit code from the
	// verify Job pod's initContainerStatuses.
	if loop.Status.Phase == coxv1alpha1.LoopPhaseVerifying && loop.Status.Verify != nil {
		// The operator's CURRENT verified commit is the one it pinned on entering
		// Verifying (status.currentVerify.verifiedCommit, D11) — never the
		// evidence's own commit (D27: passing the evidence's commit as both args
		// made the stale guard a no-op in prod). The evidence's verifiedCommit
		// (status.verify.verifiedCommit) is what the evidence NAMES. A mismatch
		// between the two, or an empty pin, makes tamperVerdict return Unknown
		// (fail-closed), so leftover evidence from a previous iteration's Job or a
		// force-pushed branch cannot be reused to reach Succeeded.
		v := loop.Status.Verify
		var currentCommit string
		if loop.Status.CurrentVerify != nil {
			currentCommit = loop.Status.CurrentVerify.VerifiedCommit
		}
		if tamperVerdict(v.TamperExitCode, v.VerifiedCommit, currentCommit) == TamperTampered {
			loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
			setCondition(&loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue, TamperedVerifyReason,
				"a protected path changed between baseCommit and verifiedCommit; terminal")
			changed = true
		}
	}
	// C6a (ADR-0007 Q2): record the effective AgentPolicy for the agent — the
	// union of the allows across every AgentPolicy the Loop references
	// (spec.policyRefs[]). The operator computes the hash and stores it in
	// status.policy.effectiveHash so the decision audit shows what the agent was
	// allowed to do (D32); the hash is over the union, not stored allows.
	if effectiveHash, found, err := r.effectivePolicyHash(ctx, &loop); err != nil {
		return ctrl.Result{}, err
	} else if found {
		if loop.Status.Policy == nil {
			loop.Status.Policy = &coxv1alpha1.PolicyStatus{}
		}
		if loop.Status.Policy.EffectiveHash != effectiveHash {
			loop.Status.Policy.EffectiveHash = effectiveHash
			changed = true
		}
	}
	conditionsChanged := !equality.Semantic.DeepEqual(condsBefore, loop.Status.Conditions)

	if changed || conditionsChanged {
		if err := r.Status().Update(ctx, &loop); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// effectivePolicyHash computes the canonical hash of the Loop's effective
// AgentPolicy (C6a): the union of the allows across every AgentPolicy the Loop
// references (spec.policyRefs[]). It returns the hash and whether any policy
// was applied (false when policyRefs is empty — the default-deny minimum). A
// referenced AgentPolicy that does not exist is an error (the operator must not
// silently run an agent with a narrower policy than the Loop declared).
func (r *LoopReconciler) effectivePolicyHash(ctx context.Context, loop *coxv1alpha1.Loop) (string, bool, error) {
	if len(loop.Spec.PolicyRefs) == 0 {
		return "", false, nil
	}
	union, err := r.effectivePolicy(ctx, loop)
	if err != nil {
		return "", false, err
	}
	return policy.EffectiveHash(union), true, nil
}

// enforcementStatus (D30, ADR-0007) reports whether the eBPF engine is enforcing
// the Loop's policy. The gate applies to EVERY Loop (no policyRefs = the
// platform minimum, still enforced); with no Enforcer (engine not installed) it
// reports EngineUnavailable, so the sandbox is held Suspended (fail-closed).
func (r *LoopReconciler) enforcementStatus(ctx context.Context, loop *coxv1alpha1.Loop) (enforced bool, reason string) {
	if r.Enforcer == nil {
		if r.AllowUnenforced {
			return true, engine.ReasonEnforcementDisabled // run, but NOT enforced
		}
		return false, engine.ReasonEngineUnavailable
	}
	enforcing, reason := r.Enforcer.Enforcing(ctx, loop)
	if !enforcing && r.AllowUnenforced {
		return true, engine.ReasonEnforcementDisabled // run anyway, not enforced
	}
	return enforcing, reason
}

// cniNetworkStatus (D38) reports whether the cluster's CNI polices pod ->
// host-network egress, from the cached probe result (the probe itself runs in
// the leader-elected Runnable — internal/cni; the reconcile only reads the
// cache). With AllowUnenforcedNetwork set it reports "run anyway" (the
// condition records EnforcementDisabled). The holder starts Unknown and
// Unknown holds the sandbox Suspended: the operator is fail-closed from
// startup, not just from the first probe failure.
func (r *LoopReconciler) cniNetworkStatus() (run bool, reason cni.Reason, status metav1.ConditionStatus, message string) {
	// No prober wired: the CNI gate does not apply (the operator is not
	// configured to probe the CNI). This is the envtest path for specs that
	// don't set a fake prober, and a no-op in production before the probe
	// Runnable is registered. The gate is active only when a prober is wired
	// (D38 production path), in which case the holder starts Unknown and
	// Unknown holds the sandbox Suspended (fail-closed from startup).
	if r.CNIProber == nil {
		return true, cni.ReasonUnknown, metav1.ConditionUnknown, "CNI probe not configured; gate not active"
	}
	// The reconcile loop reads the CURRENT result from the prober (the real
	// implementation's LatestResult returns the holder's cached result; the
	// fake returns its configured result). It NEVER runs the probe (design
	// point 3a).
	res := r.CNIProber.LatestResult()
	// The escape hatch is AllowUnenforcedNetwork (separate from AllowUnenforced
	// — the two flags are independent). When set, the Loop runs with the
	// condition recording EnforcementDisabled, regardless of the result.
	if r.AllowUnenforcedNetwork && res.Reason != cni.ReasonCNIEnforced {
		return true, cni.ReasonEnforcementDisabled, metav1.ConditionFalse,
			"--allow-unenforced-network is set: the Loop runs but the CNI does NOT police pod -> host-network egress (dev escape hatch)"
	}
	if !res.HoldsSuspended() {
		// CNIEnforced (True) or EnforcementDisabled (escape hatch set).
		if res.Reason == cni.ReasonCNIEnforced {
			return true, res.Reason, metav1.ConditionTrue, res.Describe()
		}
		return true, cni.ReasonEnforcementDisabled, metav1.ConditionFalse,
			"--allow-unenforced-network is set: the Loop runs but the CNI does NOT police pod -> host-network egress (dev escape hatch)"
	}
	switch res.Reason {
	case cni.ReasonUnknown:
		return false, cni.ReasonUnknown, metav1.ConditionUnknown, "no CNI probe result yet; sandbox held Suspended (fail-closed)"
	case cni.ReasonProbeUnavailable:
		return false, cni.ReasonProbeUnavailable, metav1.ConditionFalse, "the CNI probe could not run; sandbox held Suspended (fail-closed): " + res.Detail
	default: // CNIUnenforced
		return false, cni.ReasonCNIUnenforced, metav1.ConditionFalse, "the CNI does not police pod -> host-network egress; sandbox held Suspended. " + res.Describe()
	}
}

// networkEnforcedChanged reports whether the NetworkEnforced condition the
// given status/reason/message would produce differs from the one the Loop
// currently carries (D38 design point 3: an Event is emitted on each
// CHANGE — not on every reconcile).
func (r *LoopReconciler) networkEnforcedChanged(loop *coxv1alpha1.Loop, status metav1.ConditionStatus, reason, message string) bool {
	for i := range loop.Status.Conditions {
		c := loop.Status.Conditions[i]
		if c.Type != cni.NetworkEnforcedCondition {
			continue
		}
		return c.Status != status || c.Reason != reason || c.Message != message
	}
	return true // the condition is not present yet
}

// emitNetworkEnforcedEvent emits the Kubernetes Event on a NetworkEnforced
// condition change (D38 design point 3). Best-effort: a failed Event never
// blocks the reconcile. A nil Recorder (most envtests) skips it.
func (r *LoopReconciler) emitNetworkEnforcedEvent(loop *coxv1alpha1.Loop, status metav1.ConditionStatus, reason, message string) {
	if r.Recorder == nil {
		return
	}
	var typ string
	if status == metav1.ConditionTrue {
		typ = corev1.EventTypeNormal
	} else {
		typ = corev1.EventTypeWarning
	}
	r.Recorder.Eventf(loop, typ, "NetworkEnforced", "NetworkEnforced=%s reason=%s: %s", status, reason, message)
}

// PolicyEnforcedCondition is the non-phase condition type recording whether the
// eBPF engine is enforcing the Loop's policy (D30).
const PolicyEnforcedCondition = "PolicyEnforced"

// PolicyTranslationLossyCondition is the Loop condition that reports a lossy
// KubeArmor translation (I41): a host:PORT network allow that loses its port
// in the translation. True + reason KubeArmorDroppedPorts lists the widened
// allows; False + reason NoPortLoss when no port was lost.
const PolicyTranslationLossyCondition = "PolicyTranslationLossy"

// effectivePolicy resolves the Loop's effective policy: the union of the
// referenced AgentPolicies, or the platform minimum (empty EffectivePolicy) when
// there are no policyRefs (D30: the platform minimum is still translated,
// emitted, and enforced — the gate applies to EVERY Loop). It errors if a
// referenced AgentPolicy is missing (the operator must not silently run an
// agent narrower than declared).
func (r *LoopReconciler) effectivePolicy(ctx context.Context, loop *coxv1alpha1.Loop) (policy.EffectivePolicy, error) {
	union := policy.EffectivePolicy{}
	for _, name := range loop.Spec.PolicyRefs {
		var ap coxv1alpha1.AgentPolicy
		if err := r.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: name}, &ap); err != nil {
			return policy.EffectivePolicy{}, fmt.Errorf("resolve AgentPolicy %s/%s: %w", loop.Namespace, name, err)
		}
		union.Exec = append(union.Exec, ap.Spec.Exec...)
		union.Network = append(union.Network, ap.Spec.Network...)
		union.Files = append(union.Files, ap.Spec.Files...)
	}
	return union, nil
}

// ensureSandbox creates the Loop's Sandbox if it does not already exist, and
// logs it. It is idempotent: an existing sandbox is left functionally
// untouched except that we re-assert ownership and the container spec.
//
// I2: the controller owner ref is set *inside* the mutate func, after
// CreateOrUpdate's Get has populated `desired` with the server copy. Setting
// it beforehand (on the empty desired) is dropped by the Get for an existing
// sandbox, which orphans it (no GC, no Owns mapping). For a sandbox owned by
// a *different* controller, SetControllerReference returns AlreadyOwnedError,
// so we never silently take it over.
func (r *LoopReconciler) ensureSandbox(ctx context.Context, loop *coxv1alpha1.Loop) error {
	// Pull the logger from the context (the controller-runtime idiom) so the
	// function doesn't take both a context and a logger (logcheck).
	log := logf.FromContext(ctx)
	desired := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName(loop.Name),
			Namespace: loop.Namespace,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, desired, func() error {
		// C1 (ADR-0006 item 4): the sandbox pod is a zero-credential, hardened
		// boundary. No SA token automount; the agent is non-root, drops all caps,
		// cannot escalate privilege, is seccomp-constrained, and runs a read-only
		// rootfs with only /workspace + scratch writable.
		nonRootUID := int64(65532)
		nonRootGID := int64(65532)
		nonRootFSGroup := int64(65532)
		// D33 (P1): the proxy + model access exist only when a model endpoint is
		// configured. With no endpointSecretRef the agent runs with no model — no
		// proxy pod, no key, no COX_MODEL_BASE_URL (never a half-configured
		// proxy that would make the pod InvalidConfiguration on an empty secret).
		hasModel := loop.Spec.Agent.EndpointSecretRef != ""
		falseP := false
		trueP := true
		readOnlyRootfs := true
		// Honor spec.suspend: a suspended Loop must not run a Running sandbox
		// (S1). Running is the default for a normal Loop.
		if loop.Spec.Suspend {
			desired.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
		} else {
			desired.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
		}
		// D30 (C6b): if the engine is not enforcing and AllowUnenforced is not
		// set, hold the sandbox Suspended (fail-closed). The D30 gate applies in
		// ADDITION to the C6a policy-validity gate (validateAgentPolicies): an
		// invalid policy suspends via suspendSandboxIfRunning, and unenforced
		// also suspends here.
		if desired.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeRunning {
			enforced, _ := r.enforcementStatus(ctx, loop)
			if !enforced {
				desired.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
			}
			// D35a: the proxy pod must be Ready and owned by the Loop.
			if desired.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeRunning &&
				loop.Spec.Agent.EndpointSecretRef != "" {
				proxyPod := &corev1.Pod{}
				perr := r.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: proxyPodName(loop.Name)}, proxyPod)
				proxyReady := perr == nil && metav1.IsControlledBy(proxyPod, loop) && isPodReady(proxyPod)
				if !proxyReady {
					desired.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
				}
			}
			// I42b: the egress proxy pod must be Ready and owned by the Loop when
			// the effective policy has network allows. Same gate pattern as D35a.
			// Fails closed: a transient error reading the AgentPolicies holds the
			// sandbox Suspended (reconcile requeues), never skips the gate.
			if desired.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeRunning &&
				needsEgressProxy(ctx, r, loop) {
				egressPod := &corev1.Pod{}
				errE := r.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: egressProxyPodName(loop.Name)}, egressPod)
				egressReady := errE == nil && metav1.IsControlledBy(egressPod, loop) && isPodReady(egressPod)
				if !egressReady {
					desired.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
				}
			}
			// I42c review P2 (round 3): the NetworkPolicy gate must be
			// ORDER-INDEPENDENT (ensureSandbox runs before ensureNetworkPolicy,
			// so the conflict set there would flap the next reconcile back to
			// Running). Read the live objects: hold Suspended when any of this
			// Loop's netpol names exists and is NOT controlled by the Loop
			// (same gate pattern as D35a). Absent netpols are not a conflict
			// (ensureNetworkPolicy creates them); a real read error FAILS CLOSED
			// (Suspended), consistent with needsEgressProxy.
			if desired.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeRunning &&
				foreignNetPols(ctx, r, loop) {
				desired.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
			}
			// I42f review P2 (round 1): the KubeArmorPolicy gate mirrors the
			// NetworkPolicy gate above (reviewer: "like NetworkPolicyConflict").
			// Hold Suspended when any of this Loop's KubeArmorPolicy names exists
			// and is NOT controlled by the Loop. createOrUpdateKapt refuses to
			// take over a foreign policy (errForeignKapt) and the
			// KubeArmorPolicyConflict condition is set by applyEffectivePolicyAndConditions
			// on the sentinel; this gate is the order-independent hold (it runs
			// before the Enforcer.Apply of the same reconcile, so it must read
			// the live objects). Same fail-closed semantics: a real read error
			// holds Suspended.
			if desired.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeRunning &&
				foreignKaptPolicies(ctx, r, loop) {
				desired.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
			}
			// D38: the CNI self-test gate (network layer, analogous to the D30
			// eBPF gate above). Unknown (no probe result yet), CNIUnenforced,
			// and ProbeUnavailable all hold the sandbox Suspended (fail-closed)
			// unless AllowUnenforcedNetwork is set. The condition is set in
			// applyEffectivePolicyAndConditions (order-independent, like the
			// PolicyEnforced condition).
			if desired.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeRunning {
				run, _, _, _ := r.cniNetworkStatus()
				if !run {
					desired.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
				}
			}
		}
		// C6b (P1 #2) + D33: the sandbox pod carries the coxswain.io/loop label
		// (KubeArmorPolicy selector) and the D33 agent component label
		// (NetworkPolicy podSelector). MERGE into the map (maps.Copy) so both
		// compose — an assignment would clobber whichever was set first.
		if desired.Spec.PodTemplate.ObjectMeta.Labels == nil {
			desired.Spec.PodTemplate.ObjectMeta.Labels = map[string]string{}
		}
		maps.Copy(desired.Spec.PodTemplate.ObjectMeta.Labels, map[string]string{"coxswain.io/loop": loop.Name})
		maps.Copy(desired.Spec.PodTemplate.ObjectMeta.Labels, agentPodLabels(loop.Name))
		// I36: the agent and any future sidecars do not share a process
		// namespace (no nsenter / /proc/<pid> cross-container access).
		noShare := false
		desired.Spec.PodTemplate.Spec.ShareProcessNamespace = &noShare
		desired.Spec.PodTemplate.Spec.AutomountServiceAccountToken = &falseP
		// ndots:1 so the agent's resolver sends absolute names for the
		// in-cluster Service URLs (COX_MODEL_BASE_URL, HTTPS_PROXY) without
		// search-suffix expansion. KubeArmor's matchDNSQueries is an allowlist
		// of the bare Service FQDNs; with ndots:5 (the k8s default) the
		// resolver first queries <name>.<search-suffix> which is NOT on the
		// allowlist, so KubeArmor denies the lookup (EPERM) and the agent
		// cannot resolve the proxy or egress proxy Service names.
		ndotsVal := "1"
		desired.Spec.PodTemplate.Spec.DNSConfig = &corev1.PodDNSConfig{
			Options: []corev1.PodDNSConfigOption{{Name: "ndots", Value: &ndotsVal}},
		}
		// I35: fsGroup so the /workspace + /scratch emptyDir volumes are owned by
		// the agent's UID (writable). Set on the pod security context.
		desired.Spec.PodTemplate.Spec.SecurityContext = &corev1.PodSecurityContext{
			FSGroup: &nonRootFSGroup,
		}
		// The agent container: spec.agent.image when set, else the dev default
		// (C1). It holds no credentials (ADR-0006) — the model key lives only in
		// the proxy sidecar (C2), mounted there in a later slice.
		agentImage := r.sandboxImage()
		if loop.Spec.Agent.Image != "" {
			agentImage = loop.Spec.Agent.Image
		}
		// I35: HOME points at the writable scratch because the read-only rootfs
		// otherwise breaks every tool that writes ~/.cache (Go's build cache, git,
		// npm). TMPDIR is NOT overridden (P1, R13): /scratch/tmp is a directory a
		// fresh emptyDir never creates, so go build / mktemp failed; instead a
		// dedicated emptyDir is mounted at /tmp (writable), which also covers the
		// tools that hard-code /tmp.
		agentEnv := make([]corev1.EnvVar, 0, len(loop.Spec.Agent.Env)+2)
		agentEnv = append(agentEnv, corev1.EnvVar{Name: "HOME", Value: "/scratch"})
		// C2 (ADR-0006 item 2): the agent holds no model key. It talks to the local
		// proxy sidecar (COX_MODEL_BASE_URL), which holds the key and injects auth.
		// The operator sets this; a Loop cannot override it (COX_* names are
		// rejected at admission, I34) so the agent cannot be pointed past the proxy.
		// Only set when a model endpoint exists (P1: no half-configured proxy).
		if hasModel {
			agentEnv = append(agentEnv, corev1.EnvVar{Name: coxModelBaseURL, Value: r.proxyServiceURL(loop.Name, loop.Namespace)})
		}
		// I42d: when the egress proxy is expected (network allows present, or the
		// policy cannot be read — the fail-closed needsEgressProxy gate), the agent
		// routes external HTTP egress through the operator's egress proxy and model
		// calls bypass it. Standard (unprefixed) names so any HTTP client picks
		// them up. The COX_MODEL_BASE_URL host is in NO_PROXY so model calls go
		// straight to the model proxy. A user spec.agent.env var that collides
		// with one of these operator names is DROPPED: the operator value wins
		// (an agent that could re-route its own egress away from the egress proxy
		// would defeat the network allowlist).
		if needsEgressProxy(ctx, r, loop) {
			// The append must extend agentEnv, NOT replace it: replacing would
			// drop everything appended before (COX_MODEL_BASE_URL, HOME, ...).
			noProxy := r.egressNOProxy(loop)
			agentEnv = append(agentEnv, r.operatorProxyEnv(loop.Name, loop.Namespace)...)
			agentEnv = append(agentEnv,
				corev1.EnvVar{Name: "NO_PROXY", Value: noProxy},
				corev1.EnvVar{Name: "no_proxy", Value: noProxy},
			)
		}
		for _, e := range loop.Spec.Agent.Env {
			if isOperatorProxyEnv(e.Name) {
				// A user spec.agent.env var colliding with the operator-owned proxy
				// names is dropped (I42d): the operator value wins so the agent
				// cannot re-route its egress away from the egress proxy. A non-
				// colliding name falls through and is preserved.
				continue
			}
			agentEnv = append(agentEnv, corev1.EnvVar{Name: e.Name, Value: e.Value})
		}
		// I36 (R10): ADR-0006 item 4 requires CPU/memory limits (one agent must not
		// starve the node) + an ephemeral-storage limit (/workspace + /scratch are
		// emptyDir; an agent can fill the node's disk). Platform defaults here; a
		// per-Loop override bounded by a cluster maximum is a follow-on (I36) once
		// the coxswain-agent-defaults ConfigMap exists.
		agentLimits := corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("500m"),
			corev1.ResourceMemory:           resource.MustParse("512Mi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
		}
		agentContainer := corev1.Container{
			Name:  "agent",
			Image: agentImage,
			// Keep the container alive until the phase driver (Phase 1)
			// takes over. sleep infinity is a stand-in.
			Command: []string{"sh", "-c", "sleep infinity"},
			Env:     agentEnv,
			Resources: corev1.ResourceRequirements{
				Limits: agentLimits,
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &falseP,
				RunAsNonRoot:             &trueP,
				RunAsUser:                &nonRootUID,
				RunAsGroup:               &nonRootGID,
				ReadOnlyRootFilesystem:   &readOnlyRootfs,
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allCaps}},
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "workspace", MountPath: agentWorkspaceMount},
				{Name: "scratch", MountPath: agentScratchMount},
				// P1 (R13): a writable /tmp so go build / mktemp / any tool that
				// honors TMPDIR or hard-codes /tmp works under a read-only rootfs.
				{Name: "tmp", MountPath: agentTmpMount},
			},
		}
		desired.Spec.PodTemplate.Spec.Containers = []corev1.Container{agentContainer}
		// P3 (R13, I36): the writable emptyDirs carry explicit sizeLimits that sum
		// under the container's 1Gi ephemeral limit, so a full workspace/scratch/tmp
		// surfaces as a bounded pod eviction (and, after I36, a budget-aware signal)
		// rather than filling the node. 500+350+100 = 950Mi < 1Gi.
		// writableMountPaths is the single source of truth for the agent's writable
		// mount points; the AgentPolicy exec XValidation hard-codes the same set
		// (a CRD CEL rule cannot reference Go code), so adding a mount here MUST
		// also update the XValidation in api/v1alpha1/agentpolicy_types.go or the
		// new mount would silently become a spoofable exec target.
		writableMountPaths := []string{agentWorkspaceMount, agentScratchMount, agentTmpMount}
		_ = writableMountPaths // single source of truth (see comment)
		desired.Spec.PodTemplate.Spec.Volumes = []corev1.Volume{
			{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				SizeLimit: newLimit("500Mi"),
			}}},
			{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				SizeLimit: newLimit("350Mi"),
			}}},
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				SizeLimit: newLimit("100Mi"),
			}}},
		}
		// D33: the model-creds Secret is mounted into the per-Loop proxy pod
		// (ensureProxy), NOT the sandbox pod. The agent holds no key (C2).
		// Set the controller owner ref here, on the (possibly server-populated)
		// object. Returns AlreadyOwnedError if a different controller already
		// owns it (I2: never take over a foreign sandbox).
		return controllerutil.SetControllerReference(loop, desired, r.Scheme)
	})
	if err != nil {
		return err
	}

	// Phase 0 done-when: the controller logs the sandbox it created/updated.
	// Log at V(1) when nothing changed so a steady-state reconcile is quiet
	// (P3 tidy-up); created/updated still logs at info.
	if op == controllerutil.OperationResultNone {
		log.V(1).Info("ensured loop sandbox",
			"sandbox", desired.Name,
			"operation", op,
			"loop", loop.Name,
		)
	} else {
		log.Info("ensured loop sandbox",
			"sandbox", desired.Name,
			"operation", op,
			"loop", loop.Name,
		)
	}
	return nil
}

// sandboxName returns the Loop's Sandbox name: <loop>-sandbox. The Loop name
// is CEL-validated to be a DNS-1035 label of at most 55 chars (D20), so this
// is always a valid DNS-1035 label <= 63 chars and needs no truncation.
func sandboxName(loopName string) string {
	return loopName + "-sandbox"
}

// newLimit returns a pointer to the parsed quantity, for the emptyDir sizeLimit
// fields (I36 P3, R13). It is a two-statement wrapper so golangci-lint's
// modernize newexpr checker (which flags one-line pointer wrappers) does not
// flag it, while still giving a named, self-documenting helper.
func newLimit(q string) *resource.Quantity {
	p := new(resource.Quantity)
	*p = resource.MustParse(q)
	return p
}

// nextPhase is the operator's *claim-driven* phase-transition table (B1). It
// is a pure function of the operator's current phase and the phase the runner
// reported having finished (observedPhase). It advances one step when
// observedPhase is the immediate-next phase after current:
//
//	Pending -> Planning -> Implementing -> Verifying
//
// It never returns Succeeded or Failed. Every exit out of Verifying (→ Succeeded,
// → Implementing, → Failed:*) is evidence-gated and decided by
// verifyOutcome(job) in B3 from the verify Job's container exit codes — never by
// the runner's report (ADR-0004: observedPhase is a claim; ADR-0005: no gate
// decision on a claim). Terminal phases and unrecognised / skip-ahead reports
// leave the phase unchanged.
func nextPhase(current, reported coxv1alpha1.LoopPhase) coxv1alpha1.LoopPhase {
	switch current {
	case coxv1alpha1.LoopPhasePending:
		if reported == coxv1alpha1.LoopPhasePlanning {
			return coxv1alpha1.LoopPhasePlanning
		}
	case coxv1alpha1.LoopPhasePlanning:
		if reported == coxv1alpha1.LoopPhaseImplementing {
			return coxv1alpha1.LoopPhaseImplementing
		}
	case coxv1alpha1.LoopPhaseImplementing:
		if reported == coxv1alpha1.LoopPhaseVerifying {
			return coxv1alpha1.LoopPhaseVerifying
		}
		// Verifying, Succeeded, Failed: no claim-driven exit. Verifying's exit is
		// evidence-gated (B3); Succeeded/Failed are terminal.
	}
	return current
}

// TamperedVerifyReason is the terminal Failed reason recorded when a protected
// path changed between the operator-pinned baseCommit and verifiedCommit
// (ADR-0005 D10). It is set before any acceptance check runs.
const TamperedVerifyReason = "TamperedVerify"

// TamperVerdict is the tri-state outcome of the operator's TamperedVerify
// decision at Verifying (ADR-0005 D24). The operator's evidence is a pointer:
// a NON-NIL tamper exit code comes from a terminated tamper init container for
// the current verifiedCommit; nil means no evidence.
type TamperVerdict int

const (
	// TamperUnknown means there is no tamper evidence (nil tamperExitCode, or
	// evidence from a different verifiedCommit/Job). It is NEVER treated as
	// clean (D24 fail-closed): the Loop cannot advance out of Verifying on it.
	TamperUnknown TamperVerdict = iota
	// TamperClean means a terminated tamper init container for the current
	// verifiedCommit exited 0 (no protected path changed). The verify proceeds
	// to the check containers (B3).
	TamperClean
	// TamperTampered means a protected path differs between baseCommit and
	// verifiedCommit (terminated tamper container exited non-zero). Terminal
	// Failed:TamperedVerify (B2), before any check runs.
	TamperTampered
)

// tamperVerdict is the operator's TamperedVerify decision at Verifying (B2/D24).
// It is a pure function of the operator's OWN evidence — a pointer to the
// tamper-check container's exit code plus the verifiedCommit that evidence
// names — and returns a tri-state:
//
//
//	tamperExitCode == nil            -> TamperUnknown (no evidence; NEVER clean)
//	commit binding missing or stale    -> TamperUnknown (D27: both commits non-empty AND equal)
//	*exitCode == 0                    -> TamperClean
//	*exitCode != 0                    -> TamperTampered (terminal Failed:TamperedVerify)
//

// The decision never reads the runner's result.json claim (I28/D10 anti-gaming:
// a runner that edited a protected file and reported success still ends
// Failed:TamperedVerify when the evidence is non-zero).
func tamperVerdict(tamperExitCode *int32, evidenceCommit, verifiedCommit string) TamperVerdict {
	// No evidence (nil) -> Unknown. D24 fail-closed: absence of evidence is not a
	// pass.
	if tamperExitCode == nil {
		return TamperUnknown
	}
	// D27 fail-closed on a missing or stale binding: the evidence must be bound
	// to a KNOWN current commit. If either commit is empty (the evidence names no
	// commit, or the operator hasn't pinned the current one yet) or they differ
	// (leftover from a previous iteration's Job / a force-pushed branch), the
	// evidence is Unknown — never Clean, never Tampered-advancing.
	if evidenceCommit == "" || verifiedCommit == "" || evidenceCommit != verifiedCommit {
		return TamperUnknown
	}
	if *tamperExitCode == 0 {
		return TamperClean
	}
	return TamperTampered
}

func (r *LoopReconciler) cleanupProxy(ctx context.Context, loop *coxv1alpha1.Loop) error {
	log := logf.FromContext(ctx)
	// Delete the proxy pod (only if we control it, I2: never take over a
	// foreign object).
	pod := &corev1.Pod{}
	err := r.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: proxyPodName(loop.Name)}, pod)
	if err == nil {
		if !metav1.IsControlledBy(pod, loop) {
			log.Info("proxy pod exists but is not controlled by this Loop; not deleting",
				"proxy", proxyPodName(loop.Name), "loop", loop.Name)
		} else if delErr := r.Delete(ctx, pod); delErr != nil && !apierrors.IsNotFound(delErr) {
			return fmt.Errorf("delete proxy pod %s/%s: %w", loop.Namespace, proxyPodName(loop.Name), delErr)
		} else {
			log.Info("deleted proxy pod (endpointSecretRef absent)", "proxy", proxyPodName(loop.Name), "loop", loop.Name)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get proxy pod %s/%s: %w", loop.Namespace, proxyPodName(loop.Name), err)
	}

	// Delete the proxy Service (only if we control it).
	svc := &corev1.Service{}
	err = r.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: proxyServiceName(loop.Name)}, svc)
	if err == nil {
		if !metav1.IsControlledBy(svc, loop) {
			log.Info("proxy Service exists but is not controlled by this Loop; not deleting",
				"proxy", proxyServiceName(loop.Name), "loop", loop.Name)
		} else if delErr := r.Delete(ctx, svc); delErr != nil && !apierrors.IsNotFound(delErr) {
			return fmt.Errorf("delete proxy Service %s/%s: %w", loop.Namespace, proxyServiceName(loop.Name), delErr)
		} else {
			log.Info("deleted proxy Service (endpointSecretRef absent)", "proxy", proxyServiceName(loop.Name), "loop", loop.Name)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get proxy Service %s/%s: %w", loop.Namespace, proxyServiceName(loop.Name), err)
	}
	return nil
}

// sandboxName returns the Loop's Sandbox name (<loop>-sandbox). The name is
// CEL-validated to be a DNS-1035 label of at most 55 chars (D20), so this
// is always a valid DNS-1035 label <= 63 chars and needs no truncation.

func (r *LoopReconciler) ensureProxy(ctx context.Context, loop *coxv1alpha1.Loop) error {
	log := logf.FromContext(ctx)
	ns := loop.Namespace
	loopName := loop.Name

	// --- proxy Service ---
	svcDesired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxyServiceName(loopName),
			Namespace: ns,
		},
	}
	svcOp, err := controllerutil.CreateOrUpdate(ctx, r.Client, svcDesired, func() error {
		svcDesired.Labels = proxyLabels(loopName)
		svcDesired.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       proxyPort,
			TargetPort: intstr.FromInt32(proxyPort),
			Protocol:   corev1.ProtocolTCP,
		}}
		svcDesired.Spec.Selector = proxyLabels(loopName)
		// ClusterIP (the default): no external exposure; only in-cluster callers
		// (the agent, via COX_MODEL_BASE_URL) reach it. D34's NetworkPolicy then
		// restricts who may call it.
		svcDesired.Spec.Type = corev1.ServiceTypeClusterIP
		return controllerutil.SetControllerReference(loop, svcDesired, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensure proxy service %s/%s: %w", ns, proxyServiceName(loopName), err)
	}

	// --- proxy pod (R15 fix: Get/create/delete, never Update a Pod spec) ---
	// Build the desired pod spec first, then hash it. The hash covers the
	// actual spec (image, command, limits, volumes), so any operator upgrade
	// that changes the spec produces a different hash.
	podDesired := buildProxyPod(loop, loopName, ns, r.proxyImage())
	// Set the Loop as the controller owner (D33: GC with the Loop).
	if ownerErr := controllerutil.SetControllerReference(loop, podDesired, r.Scheme); ownerErr != nil {
		return fmt.Errorf("set owner ref on proxy pod %s/%s: %w", ns, proxyPodName(loopName), ownerErr)
	}
	proxySpecHash := proxyPodSpecHash(podDesired)
	podDesired.Annotations = map[string]string{proxySpecHashAnnotation: proxySpecHash}

	existingPod := &corev1.Pod{}
	err = r.Get(ctx, client.ObjectKey{Namespace: ns, Name: proxyPodName(loopName)}, existingPod)
	// D35b: a foreign <loop>-proxy must not be opened or deleted (I2).
	if err == nil && !metav1.IsControlledBy(existingPod, loop) {
		setCondition(loop, "ProxyConflict", metav1.ConditionTrue, "ForeignProxy",
			fmt.Sprintf("foreign proxy pod in %s/%s; sandbox held Suspended", ns, proxyPodName(loopName)))
		log.Info("proxy pod is foreign; setting ProxyConflict",
			"proxy", proxyPodName(loopName), "loop", loopName)
		return nil
	}
	if apierrors.IsNotFound(err) {
		// Pod doesn't exist: create it.
		if createErr := r.Create(ctx, podDesired); createErr != nil {
			return fmt.Errorf("create proxy pod %s/%s: %w", ns, proxyPodName(loopName), createErr)
		}
		log.Info("ensured loop proxy (created)",
			"proxy", proxyPodName(loopName), "namespace", ns, "loop", loopName)
	} else if err != nil {
		return fmt.Errorf("get proxy pod %s/%s: %w", ns, proxyPodName(loopName), err)
	} else if existingPod.Annotations[proxySpecHashAnnotation] != proxySpecHash {
		// Pod exists but the spec hash doesn't match: delete and requeue.
		// A bare Pod's spec is immutable, so we can't Update it. Deleting
		// triggers the Owns(Pod) watch, which re-reconciles and creates the
		// new pod. Only delete if we control it (I2: never take over a
		// foreign object).
		if !metav1.IsControlledBy(existingPod, loop) {
			log.Info("proxy pod spec drift detected but pod is not controlled by this Loop; not deleting",
				"proxy", proxyPodName(loopName), "loop", loopName)
		} else {
			log.Info("proxy pod spec drift detected, deleting for recreation",
				"proxy", proxyPodName(loopName), "loop", loopName)
			if delErr := r.Delete(ctx, existingPod); delErr != nil && !apierrors.IsNotFound(delErr) {
				return fmt.Errorf("delete drifted proxy pod %s/%s: %w", ns, proxyPodName(loopName), delErr)
			}
		}
		// Requeue so the next pass creates the new pod (the delete is
		// asynchronous; the pod may not be gone yet).
		return nil // the Owns(Pod) watch will trigger a re-reconcile
	}
	// Pod exists and hash matches: nothing to do.
	podOp := controllerutil.OperationResultNone

	if svcOp == controllerutil.OperationResultNone && podOp == controllerutil.OperationResultNone {
		log.V(1).Info("ensured loop proxy (no change)",
			"proxy", proxyPodName(loopName),
			"namespace", ns,
			"loop", loopName,
		)
	}
	// D35b: clear ProxyConflict when the controller's own proxy is in place.
	if loop.Spec.Agent.EndpointSecretRef != "" {
		hadConflict := false
		for _, c := range loop.Status.Conditions {
			if c.Type == "ProxyConflict" && c.Status == metav1.ConditionTrue {
				hadConflict = true
			}
		}
		if hadConflict {
			setCondition(loop, "ProxyConflict", metav1.ConditionFalse, "Resolved",
				"the foreign proxy pod is gone")
		} else {
			setCondition(loop, "ProxyConflict", metav1.ConditionFalse, "NoConflict",
				"no foreign proxy pod detected")
		}
	}
	return nil
}

// newLimit returns a pointer to the parsed quantity, for the emptyDir sizeLimit
// fields (I36 P3, R13). It is a two-statement wrapper so golangci-lint's
// modernize newexpr checker (which flags one-line pointer wrappers) does not
// flag it, while still giving a named, self-documenting helper.

// proxyNdotsOneDNSConfig returns the pod dnsConfig both operator-owned proxy
// pods carry (review P1, R16 D39): ndots:1 so the proxy resolvers send
// absolute names for in-cluster names without search-suffix expansion. With
// the default ndots:5, the first query for a bare single-label name is the
// search-expanded <name>.<ns>.svc.cluster.local form; the KubeArmorPolicy
// allowlist carries the expanded form (EmitModelProxyKubeArmorPolicy) but
// ndots:1 keeps the resolver's primary candidate on the absolute name.
func proxyNdotsOneDNSConfig() *corev1.PodDNSConfig {
	ndotsVal := "1"
	return &corev1.PodDNSConfig{
		Options: []corev1.PodDNSConfigOption{{Name: "ndots", Value: &ndotsVal}},
	}
}

func buildProxyPod(loop *coxv1alpha1.Loop, loopName, ns, image string) *corev1.Pod {
	secretMode := readOnlyMode
	falseP := false
	trueP := true
	readOnlyRootfs := true
	proxyUID := int64(65533)
	proxyGID := int64(65533)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxyPodName(loopName),
			Namespace: ns,
			Labels:    proxyLabels(loopName),
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &falseP,
			DNSConfig:                    proxyNdotsOneDNSConfig(),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  &proxyUID,
				RunAsGroup: &proxyGID,
			},
			Containers: []corev1.Container{{
				Name:  "proxy",
				Image: image,
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:              resource.MustParse("100m"),
						corev1.ResourceMemory:           resource.MustParse("128Mi"),
						corev1.ResourceEphemeralStorage: resource.MustParse("100Mi"),
					},
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &falseP,
					RunAsNonRoot:             &trueP,
					RunAsUser:                &proxyUID,
					RunAsGroup:               &proxyGID,
					ReadOnlyRootFilesystem:   &readOnlyRootfs,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allCaps}},
					SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				// (D33 acceptance) and forwards to MODEL_ENDPOINT. The Go stand-in
				// reads the 0444 model-creds Secret at startup and exits 1 if no
				// readable key file is found (the ..data atomic-mount entry is
				// skipped).
				Env: []corev1.EnvVar{
					{
						Name:  "MODEL_ENDPOINT",
						Value: modelEndpointValue(loop),
					},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: modelCredsVolume, MountPath: "/model-creds", ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{{
				Name: modelCredsVolume,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName:  loop.Spec.Agent.EndpointSecretRef,
						DefaultMode: &secretMode,
					},
				},
			}},
		},
	}
	return pod
}

// proxyPodSpecHash computes a stable hash of the proxy pod's desired spec
// (R15 fix: hash the actual pod spec, not a literal string). The hash is
// stored on the pod as the proxySpecHashAnnotation; on a mismatch the operator
// deletes and recreates the pod (spec.volumes is immutable on a bare Pod).

func proxyPodSpecHash(pod *corev1.Pod) string {
	// Hash the spec only (not the metadata, which includes the annotation we're
	// computing — that would be a circular dependency).
	data, err := json.Marshal(pod.Spec)
	if err != nil {
		// The spec is a Go struct of known types; marshaling should never fail.
		// If it does, return a fixed hash so the pod is never deleted.
		return "unhashable"
	}
	h := sha256.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func proxyLabels(loopName string) map[string]string {
	return map[string]string{
		policy.ComponentLabelKey: policy.ComponentProxyLabel,
		"coxswain.io/proxy-for":  loopName,
	}
}

// proxyImage returns the model proxy pod image. It is the reconciler's
// ProxyImage field (settable in tests and future slices; a manager flag
// --proxy-image is a candidate for a future C2b), or a working stand-in
// (golang:1.26) when unset. The stand-in has POSIX sh, head, and sleep —
// enough for the D33 kind-run acceptance (the model-creds read + sleep
// infinity).

func proxyPodName(loopName string) string {
	return loopName + proxyPodNameSuffix
}

func proxyServiceName(loopName string) string {
	return fmt.Sprintf(proxyServiceNameFmt, loopName)
}

// proxyServiceURL is the in-cluster Service URL the agent's COX_MODEL_BASE_URL
// points at (D33): http://<loop>-proxy.<namespace>.svc.<clusterDomain>:8080.
// The per-Loop Service identity is also what makes the proxy's activity-audit
// records attributable to the Loop (D35). The cluster domain is the
// reconciler's ClusterDomain field (default cluster.local, R16 I44 item 2).
func (r *LoopReconciler) proxyServiceURL(loopName, namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc.%s:%d", proxyServiceName(loopName), namespace, r.clusterDomain(), proxyPort)
}

// egressProxyServiceURL is the in-cluster URL the agent's *_PROXY env vars
// point at (I42d): http://<loop>-egress-proxy.<namespace>.svc.<clusterDomain>:3128.
// The egress proxy enforces the hostname:port allowlist at the HTTP CONNECT / SNI
// / Host layer (ADR-0007); the agent routes external egress through it and
// model calls bypass it via NO_PROXY.
func (r *LoopReconciler) egressProxyServiceURL(loopName, namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc.%s:%d", egressProxyServiceName(loopName), namespace, r.clusterDomain(), egressProxyPort)
}

// egressNOProxy is the agent's NO_PROXY value (I42d): the model proxy Service
// (short + FQDN form, so model calls — which hit COX_MODEL_BASE_URL, the model
// proxy's .svc URL — bypass the egress proxy) + localhost/loopback. The model
// proxy's host matches the COX_MODEL_BASE_URL host, which is what proxy clients
// match NO_PROXY by.
func (r *LoopReconciler) egressNOProxy(loop *coxv1alpha1.Loop) string {
	proxySvc := proxyServiceName(loop.Name)
	domain := r.clusterDomain()
	return strings.Join([]string{
		proxySvc + "." + loop.Namespace + ".svc",
		proxySvc + "." + loop.Namespace + ".svc." + domain,
		"localhost",
		"127.0.0.1",
	}, ",")
}

// operatorProxyEnvNames are the agent env var names the operator owns for the
// egress proxy (I42d): the standard proxy vars in BOTH cases (clients are
// inconsistent about case — curl reads only lowercase http_proxy, Go reads
// either, wget/pip/npm/git/apt mostly lowercase) + ALL_PROXY, which clients
// also honour. A user spec.agent.env var with one of these names is dropped:
// the operator value wins, so the agent cannot re-route its egress away from
// the egress proxy (the network allowlist is operator policy). ALL_PROXY is
// reserved (not emitted): it would catch model traffic, which must bypass the
// egress proxy via NO_PROXY.
var operatorProxyEnvNames = map[string]struct{}{
	"HTTPS_PROXY": {},
	"https_proxy": {},
	"HTTP_PROXY":  {},
	"http_proxy":  {},
	"NO_PROXY":    {},
	"no_proxy":    {},
	"ALL_PROXY":   {},
	"all_proxy":   {},
}

// operatorProxyEnv returns the operator-owned proxy env vars for the egress
// proxy (I42d): HTTPS_PROXY/https_proxy and HTTP_PROXY/http_proxy point at
// the egress proxy Service. NO_PROXY/no_proxy are appended by the caller
// (they carry the per-Loop egressNOProxy value).
func (r *LoopReconciler) operatorProxyEnv(loopName, namespace string) []corev1.EnvVar {
	proxyURL := r.egressProxyServiceURL(loopName, namespace)
	return []corev1.EnvVar{
		{Name: "HTTPS_PROXY", Value: proxyURL},
		{Name: "https_proxy", Value: proxyURL},
		{Name: "HTTP_PROXY", Value: proxyURL},
		{Name: "http_proxy", Value: proxyURL},
	}
}

// clusterDomain returns the reconciler's cluster domain (default
// policy.DefaultClusterDomain when ClusterDomain is unset, R16 I44 item 2:
// one source of truth for the default).
func (r *LoopReconciler) clusterDomain() string {
	if r.ClusterDomain != "" {
		return r.ClusterDomain
	}
	return policy.DefaultClusterDomain
}

// ProxyServiceFQDN returns the per-Loop proxy Service FQDN the agent resolves
// via DNS to reach the model proxy (D33: <loop>-proxy.<ns>.svc). Built from
// proxyServiceName (the controller's name) — NOT a literal (R16 I44 item 1).
// It is what policy.Translate puts on the agent's DNS allowlist, and is wired
// into the KubeArmorEnforcer at construction (main.go) so the enforcer cannot
// build the FQDN from a literal of its own.
func ProxyServiceFQDN(loopName, namespace string) string {
	return proxyServiceName(loopName) + "." + namespace + ".svc"
}

// EgressProxyServiceFQDN returns the per-Loop egress proxy Service FQDN the
// agent resolves via DNS to reach the egress proxy (I42d:
// <loop>-egress-proxy.<ns>.svc). Built from egressProxyServiceName (the
// controller's name) — NOT a `-egress-proxy.` literal (R16 I44 item 1). It is
// what the agent's DNS allowlist carries so a rename cannot desync the
// allowlist from the URL the agent dials.
func EgressProxyServiceFQDN(loopName, namespace string) string {
	return egressProxyServiceName(loopName) + "." + namespace + ".svc"
}

// isOperatorProxyEnv reports whether name is one of the operator-owned proxy
// env names (I42d).
func isOperatorProxyEnv(name string) bool {
	_, ok := operatorProxyEnvNames[name]
	return ok
}

// operatorProxyNames returns the uppercase + lowercase forms of each given
// uppercase proxy env name (I42d) — the two forms the operator emits/reserves
// for that name.
func operatorProxyNames(names ...string) []string {
	out := make([]string, 0, len(names)*2)
	for _, n := range names {
		out = append(out, n, strings.ToLower(n))
	}
	return out
}

// proxyLabels is the label set the per-Loop proxy pod carries and the proxy
// Service selects on (D33). The Service selects ONLY these labels, so no other
// pod in the namespace can be reached through <loop>-proxy (D29: this Loop's
// key is only usable by this Loop's agent; D34's NetworkPolicy enforces the
// rest).

const (
	// proxyPodNameSuffix / proxyServiceNameFmt are the per-Loop proxy pod and
	// Service names: <loop>-proxy (one each, owner-referenced to the Loop so
	// both are GC'd with it).
	proxyPodNameSuffix  = "-proxy"
	proxyServiceNameFmt = "%s-proxy"
	// proxySpecHashAnnotation records the hash of the desired proxy pod spec
	// on the pod itself (P2, R15: operator upgrades change the spec; a bare
	// Pod's spec is immutable, so the operator deletes and recreates the pod
	// when the hash mismatches).
	proxySpecHashAnnotation       = "coxswain.io/proxy-spec-hash"
	proxyPort               int32 = 8080
)

// ensureProxyOrCleanup (D33) creates the per-Loop proxy pod + Service when a
// model endpoint is configured, and deletes them when it is not.
func (r *LoopReconciler) ensureProxyOrCleanup(ctx context.Context, loop *coxv1alpha1.Loop) error {
	if loop.Spec.Agent.EndpointSecretRef != "" {
		return r.ensureProxy(ctx, loop)
	}
	return r.cleanupProxy(ctx, loop)
}

// egressProxyPort is the port the egress proxy listens on (3128, the standard
// HTTP CONNECT proxy port).
const egressProxyPort int32 = 3128

// egressProxyPodName returns the egress proxy pod name for a Loop.
func egressProxyPodName(loopName string) string { return loopName + "-egress-proxy" }

// egressProxyServiceName returns the egress proxy Service name for a Loop.
func egressProxyServiceName(loopName string) string { return loopName + "-egress-proxy" }

// egressProxyLabels returns the egress proxy pod + Service labels. These are
// DISJOINT from the model proxy labels (D33) and from the agent pod labels
// (C6b). The egress proxy must NOT carry coxswain.io/loop (the KubeArmorPolicy
// selector) or app.kubernetes.io/component=agent (the agent NetworkPolicy
// selector), or the agent's exec/network rules would bind to the egress proxy
// pod. The egress proxy gets its own component label + egress-proxy-for label.
func egressProxyLabels(loopName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "coxswain-egress-proxy",
		"app.kubernetes.io/instance":   loopName,
		policy.ComponentLabelKey:       netpolEgressProxyComponent,
		"app.kubernetes.io/part-of":    "coxswain",
		"coxswain.io/egress-proxy-for": loopName,
	}
}

// needsEgressProxy reports whether the egress proxy is required for the gate:
// either the effective policy has network allows, or the effective policy
// could not be read. A read error fails CLOSED (P2 review: err == nil &&
// hasAllows would skip the gate on a transient Get error — with the gate
// skipped and ensureEgressProxy returning the same error, the sandbox could
// be set Running on a later reconcile of the error window). The sandbox gate
// uses this as "require the egress proxy to be Ready"; the error case keeps
// the sandbox Suspended and the reconcile requeues on the error.
func needsEgressProxy(ctx context.Context, r *LoopReconciler, loop *coxv1alpha1.Loop) bool {
	_, hasAllows, err := r.effectivePolicyNetwork(ctx, loop)
	return err != nil || hasAllows
}

// effectivePolicyNetwork returns the union of the network allows across the
// Loop's referenced AgentPolicies (the egress proxy's allowlist, I42b). The
// second return value (hasAllows) is false when there are no network allows
// (the egress proxy is not needed). Errors are returned (never swallowed):
// a missing or unreadable referenced policy makes the egress-proxy gate fail
// closed (P3 review: a second copy of the union that swallowed Get errors
// would create the egress proxy with a nil union if the ordering with
// validateAgentPolicies ever changed).
func (r *LoopReconciler) effectivePolicyNetwork(ctx context.Context, loop *coxv1alpha1.Loop) ([]string, bool, error) {
	union, err := r.effectivePolicy(ctx, loop)
	if err != nil {
		return nil, false, err
	}
	return union.Network, len(union.Network) > 0, nil
}

// egressProxyImage returns the egress proxy pod image. It is the reconciler's
// EgressProxyImage field (settable in tests; a manager flag --egress-proxy-image
// is a candidate for a future slice), or the egress proxy stand-in when unset.
func (r *LoopReconciler) egressProxyImage() string {
	if r.EgressProxyImage != "" {
		return r.EgressProxyImage
	}
	return "coxswain-egress-proxy:standin"
}

// buildEgressProxyPod builds the egress proxy pod spec (I42b). The pod carries
// the effective policy's network allows as EGRESS_POLICY_JSON, the effective
// policy hash as EGRESS_POLICY_HASH, and the operator's pod/service CIDRs as
// POD_CIDR / SERVICE_CIDR. UID/GID 65534 (nobody); no SA token; no model
// creds; no HTTPS_PROXY / HTTP_PROXY (I42d sets those on the agent container).
func buildEgressProxyPod(loopName, ns, image string, networkAllows []string, policyHash string, podCIDR, serviceCIDR string) *corev1.Pod {
	falseP := false
	trueP := true
	readOnlyRootfs := true
	uid := int64(65534)
	gid := int64(65534)

	// Serialize the network allows as a JSON array for the egress proxy.
	policyJSON, _ := json.Marshal(networkAllows)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      egressProxyPodName(loopName),
			Namespace: ns,
			Labels:    egressProxyLabels(loopName),
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &falseP,
			DNSConfig:                    proxyNdotsOneDNSConfig(),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  &uid,
				RunAsGroup: &gid,
			},
			Containers: []corev1.Container{{
				Name:  netpolEgressProxyComponent,
				Image: image,
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:              resource.MustParse("100m"),
						corev1.ResourceMemory:           resource.MustParse("128Mi"),
						corev1.ResourceEphemeralStorage: resource.MustParse("100Mi"),
					},
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &falseP,
					RunAsNonRoot:             &trueP,
					RunAsUser:                &uid,
					RunAsGroup:               &gid,
					ReadOnlyRootFilesystem:   &readOnlyRootfs,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allCaps}},
					SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Env: []corev1.EnvVar{
					{Name: "EGRESS_POLICY_JSON", Value: string(policyJSON)},
					{Name: "EGRESS_POLICY_HASH", Value: policyHash},
					{Name: "LOOP_NAME", Value: loopName},
					{Name: "LOOP_NAMESPACE", Value: ns},
					{Name: "POD_CIDR", Value: podCIDR},
					{Name: "SERVICE_CIDR", Value: serviceCIDR},
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						TCPSocket: &corev1.TCPSocketAction{
							Port: intstr.FromInt32(egressProxyPort),
						},
					},
					InitialDelaySeconds: 1,
					PeriodSeconds:       5,
				},
				LivenessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						TCPSocket: &corev1.TCPSocketAction{
							Port: intstr.FromInt32(egressProxyPort),
						},
					},
					InitialDelaySeconds: 3,
					PeriodSeconds:       10,
				},
			},
			}},
	}
	return pod
}

// egressProxyPodSpecHash computes a stable hash of the egress proxy pod's
// desired spec (D33 pattern: hash the actual pod spec, store as annotation,
// delete+recreate on mismatch).
func egressProxyPodSpecHash(pod *corev1.Pod) string {
	data, err := json.Marshal(pod.Spec)
	if err != nil {
		return "unhashable"
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// ensureEgressProxy creates the egress proxy pod + Service when the effective
// policy has network allows (I42b). No-op when there are no network allows.
// The pod is gated on the D35a pattern: owned by the Loop + Ready.
func (r *LoopReconciler) ensureEgressProxy(ctx context.Context, loop *coxv1alpha1.Loop) error {
	log := logf.FromContext(ctx)
	// One read of the referenced AgentPolicies for the whole path (P3: no
	// second union that swallows Get errors). Fails closed: a transient error
	// propagates and the reconcile requeues; it must not be treated as
	// "no allows".
	networkAllows, hasAllows, err := r.effectivePolicyNetwork(ctx, loop)
	if err != nil {
		return fmt.Errorf("resolve egress proxy network allows for %s/%s: %w", loop.Namespace, loop.Name, err)
	}
	if !hasAllows {
		// No network allows: no egress proxy needed. Clean up if one exists.
		// P3: clear a stale EgressProxyConflict here too — if a foreign pod once
		// held the name and the allows are later removed, the D35b clear below
		// is unreachable on this early return, so the condition would report a
		// conflict that no longer applies.
		if hadEgressConflict(loop) {
			setCondition(loop, "EgressProxyConflict", metav1.ConditionFalse, "Resolved",
				"the egress proxy is no longer required (no network allows)")
		}
		return r.cleanupEgressProxy(ctx, loop)
	}

	ns := loop.Namespace
	loopName := loop.Name

	// --- egress proxy Service ---
	svcDesired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      egressProxyServiceName(loopName),
			Namespace: ns,
		},
	}
	svcOp, err := controllerutil.CreateOrUpdate(ctx, r.Client, svcDesired, func() error {
		svcDesired.Labels = egressProxyLabels(loopName)
		svcDesired.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       egressProxyPort,
			TargetPort: intstr.FromInt32(egressProxyPort),
			Protocol:   corev1.ProtocolTCP,
		}}
		svcDesired.Spec.Selector = egressProxyLabels(loopName)
		svcDesired.Spec.Type = corev1.ServiceTypeClusterIP
		return controllerutil.SetControllerReference(loop, svcDesired, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensure egress proxy service %s/%s: %w", ns, egressProxyServiceName(loopName), err)
	}
	log.V(1).Info("ensured loop egress proxy service", "op", svcOp, "service", egressProxyServiceName(loopName))

	// --- egress proxy pod (Get/create/delete, never Update a Pod spec) ---
	// Compute the effective policy hash for the EGRESS_POLICY_HASH env var.
	policyHash, _, hashErr := r.effectivePolicyHash(ctx, loop)
	if hashErr != nil {
		return fmt.Errorf("compute egress proxy policy hash for %s/%s: %w", ns, loopName, hashErr)
	}
	podDesired := buildEgressProxyPod(loopName, ns, r.egressProxyImage(), networkAllows, policyHash, r.PodCIDR, r.ServiceCIDR)
	if ownerErr := controllerutil.SetControllerReference(loop, podDesired, r.Scheme); ownerErr != nil {
		return fmt.Errorf("set owner ref on egress proxy pod %s/%s: %w", ns, egressProxyPodName(loopName), ownerErr)
	}
	egressSpecHash := egressProxyPodSpecHash(podDesired)
	podDesired.Annotations = map[string]string{proxySpecHashAnnotation: egressSpecHash}

	existingPod := &corev1.Pod{}
	err = r.Get(ctx, client.ObjectKey{Namespace: ns, Name: egressProxyPodName(loopName)}, existingPod)
	// EgressProxyConflict (P2 review): a separate condition type from the model
	// proxy's ProxyConflict (D35), so the two proxies never overwrite each
	// other on a Loop that runs both. A foreign <loop>-egress-proxy must not be
	// opened or deleted (I2).
	if err == nil && !metav1.IsControlledBy(existingPod, loop) {
		setCondition(loop, "EgressProxyConflict", metav1.ConditionTrue, "ForeignEgressProxy",
			fmt.Sprintf("foreign egress proxy pod in %s/%s; sandbox held Suspended", ns, egressProxyPodName(loopName)))
		log.Info("egress proxy pod is foreign; setting EgressProxyConflict",
			"egressProxy", egressProxyPodName(loopName), "loop", loopName)
		return nil
	}
	if apierrors.IsNotFound(err) {
		if createErr := r.Create(ctx, podDesired); createErr != nil {
			return fmt.Errorf("create egress proxy pod %s/%s: %w", ns, egressProxyPodName(loopName), createErr)
		}
		log.Info("ensured loop egress proxy (created)",
			"egressProxy", egressProxyPodName(loopName), "namespace", ns, "loop", loopName)
	} else if err != nil {
		return fmt.Errorf("get egress proxy pod %s/%s: %w", ns, egressProxyPodName(loopName), err)
	} else if existingPod.Annotations[proxySpecHashAnnotation] != egressSpecHash {
		// Spec drift: delete and recreate (the Owns(Pod) watch triggers re-reconcile).
		if !metav1.IsControlledBy(existingPod, loop) {
			log.Info("egress proxy pod spec drift detected but pod is not controlled by this Loop; not deleting",
				"egressProxy", egressProxyPodName(loopName), "loop", loopName)
		} else {
			log.Info("egress proxy pod spec drift detected, deleting for recreation",
				"egressProxy", egressProxyPodName(loopName), "loop", loopName)
			if delErr := r.Delete(ctx, existingPod); delErr != nil && !apierrors.IsNotFound(delErr) {
				return fmt.Errorf("delete drifted egress proxy pod %s/%s: %w", ns, egressProxyPodName(loopName), delErr)
			}
		}
		return nil
	}

	// D35b pattern: clear EgressProxyConflict when the controller's own egress
	// proxy is in place (no foreign pod occupies the name). The PolicyRefs
	// guard is unnecessary here: this path only runs with allows, which
	// implies PolicyRefs is non-empty.
	if hadEgressConflict(loop) {
		setCondition(loop, "EgressProxyConflict", metav1.ConditionFalse, "Resolved",
			"the foreign egress proxy pod is gone")
	}

	return nil
}

// hadEgressConflict reports whether the Loop has an active
// EgressProxyConflict=True/ForeignEgressProxy condition.
func hadEgressConflict(loop *coxv1alpha1.Loop) bool {
	for _, c := range loop.Status.Conditions {
		if c.Type == "EgressProxyConflict" && c.Status == metav1.ConditionTrue && c.Reason == "ForeignEgressProxy" {
			return true
		}
	}
	return false
}

// hadNetpolConflict reports whether the Loop has an active
// NetworkPolicyConflict=True/ForeignNetworkPolicy condition (I42c: the
// clearing at the end of ensureNetworkPolicy must only fire when a conflict
// was previously recorded, otherwise it would write a spurious Resolved).
func hadNetpolConflict(loop *coxv1alpha1.Loop) bool {
	for _, c := range loop.Status.Conditions {
		if c.Type == "NetworkPolicyConflict" && c.Status == metav1.ConditionTrue && c.Reason == "ForeignNetworkPolicy" {
			return true
		}
	}
	return false
}

// hadKaptConflict reports whether the Loop has an active
// KubeArmorPolicyConflict=True/ForeignKubeArmorPolicy condition (I42f review
// P2: the clearing after a successful Enforcer.Apply must only fire when a
// conflict was previously recorded, same hadNetpolConflict pattern).
func hadKaptConflict(loop *coxv1alpha1.Loop) bool {
	for _, c := range loop.Status.Conditions {
		if c.Type == "KubeArmorPolicyConflict" && c.Status == metav1.ConditionTrue && c.Reason == "ForeignKubeArmorPolicy" {
			return true
		}
	}
	return false
}

// cleanupEgressProxy deletes the egress proxy pod + Service when the effective
// policy has no network allows (I42b). No-op when they don't exist.
func (r *LoopReconciler) cleanupEgressProxy(ctx context.Context, loop *coxv1alpha1.Loop) error {
	log := logf.FromContext(ctx)
	ns := loop.Namespace
	loopName := loop.Name

	// Delete the pod if it exists and is owned by the Loop.
	pod := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: egressProxyPodName(loopName)}, pod); err == nil {
		if metav1.IsControlledBy(pod, loop) {
			if delErr := r.Delete(ctx, pod); delErr != nil && !apierrors.IsNotFound(delErr) {
				return fmt.Errorf("delete egress proxy pod %s/%s: %w", ns, egressProxyPodName(loopName), delErr)
			}
			log.Info("cleaned up egress proxy pod (no network allows)",
				"egressProxy", egressProxyPodName(loopName), "loop", loopName)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get egress proxy pod %s/%s: %w", ns, egressProxyPodName(loopName), err)
	}

	// Delete the Service if it exists and is owned by the Loop.
	svc := &corev1.Service{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: egressProxyServiceName(loopName)}, svc); err == nil {
		if metav1.IsControlledBy(svc, loop) {
			if delErr := r.Delete(ctx, svc); delErr != nil && !apierrors.IsNotFound(delErr) {
				return fmt.Errorf("delete egress proxy service %s/%s: %w", ns, egressProxyServiceName(loopName), delErr)
			}
			log.Info("cleaned up egress proxy service (no network allows)",
				"egressProxy", egressProxyServiceName(loopName), "loop", loopName)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get egress proxy service %s/%s: %w", ns, egressProxyServiceName(loopName), err)
	}
	return nil
}

func (r *LoopReconciler) ensureNetworkPolicy(ctx context.Context, loop *coxv1alpha1.Loop) error {
	ns := loop.Namespace
	loopName := loop.Name
	agentLabels := agentPodLabels(loopName)
	proxyL := proxyLabels(loopName)
	egressProxyL := egressProxyLabels(loopName)
	proxyPeer := networkingv1.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{MatchLabels: proxyL},
	}
	egressProxyPeer := networkingv1.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{MatchLabels: egressProxyL},
	}
	agentPeer := networkingv1.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{MatchLabels: agentLabels},
	}
	// I42c: the agent's egress to the egress proxy is expected only when the
	// egress proxy is expected (network allows present); with no network
	// allows the agent's egress stays model-proxy + DNS (D34).
	egressExpected := needsEgressProxy(ctx, r, loop)
	agentEgress := []networkingv1.NetworkPolicyEgressRule{
		{
			To:    []networkingv1.NetworkPolicyPeer{proxyPeer},
			Ports: []networkingv1.NetworkPolicyPort{{Port: intstrPtr32(8080), Protocol: new(corev1.ProtocolTCP)}},
		},
	}
	if egressExpected {
		agentEgress = append(agentEgress, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{egressProxyPeer},
			Ports: []networkingv1.NetworkPolicyPort{{Port: intstrPtr32(egressProxyPort), Protocol: new(corev1.ProtocolTCP)}},
		})
	}
	agentEgress = append(agentEgress, networkingv1.NetworkPolicyEgressRule{
		To:    []networkingv1.NetworkPolicyPeer{dnsPeer()},
		Ports: dnsPorts(),
	})

	// Agent pod NetworkPolicy: ingress deny-all (P1-2: R13 says "Ingress:
	// none" — the agent pod must be unreachable from every other pod,
	// including other Loops' agents), egress to this Loop's proxy + DNS.
	//
	// P2 (R17 R18): the AgentPolicy `network` allows are NOT translated
	// into egress rules. The naive port-only approach is rejected (I42,
	// docs/REVIEW-PHASE1-R14.md): a port-only rule is "any host on that
	// port," which is the exfiltration path. I42 is resolved by an
	// operator-owned per-Loop egress proxy that enforces the hostname:port
	// allowlist at the HTTP CONNECT / SNI / Host layer (ADR-0007, "I42
	// resolution"). Until that slice lands, agent egress stays proxy + DNS
	// (fail-closed).
	// I42c review P2 (round 2): a FOREIGN NetworkPolicy of the same name is
	// never overwritten. A conflict (errForeignNetpol from createOrUpdateNP)
	// sets NetworkPolicyConflict=True/ForeignNetworkPolicy and holds the
	// sandbox Suspended (fail-closed: the agent's netpol may not be the one
	// the operator built). Reconcile does not error-loop: the sandbox gate in
	// ensureSandbox (order-independent, round 3) is what actually holds it
	// Suspended across reconciles; a non-conflict error below still returns
	// and requeues.
	var foreignConflict bool
	mapForeign := func(npName string, err error) error {
		if !errors.Is(err, errForeignNetpol) {
			return err
		}
		foreignConflict = true
		setCondition(loop, "NetworkPolicyConflict", metav1.ConditionTrue, "ForeignNetworkPolicy",
			"NetworkPolicy "+npName+" exists but is not controlled by this Loop; left untouched")
		return nil
	}

	agentNP := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      loopName + "-agent-netpol",
			Namespace: ns,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: agentLabels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{},
			Egress:      agentEgress,
		},
	}
	if _, err := r.createOrUpdateNP(ctx, loop, agentNP); err != nil {
		if merr := mapForeign(agentNP.Name, err); merr != nil {
			return merr
		}
	}

	// Proxy pod NetworkPolicy: ingress from this Loop's agent on 8080, egress
	// to the model endpoint peer + cluster DNS.
	proxyNP := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      loopName + "-proxy-netpol",
			Namespace: ns,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: proxyL},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From:  []networkingv1.NetworkPolicyPeer{agentPeer},
					Ports: []networkingv1.NetworkPolicyPort{{Port: intstrPtr32(8080), Protocol: new(corev1.ProtocolTCP)}},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To:    []networkingv1.NetworkPolicyPeer{dnsPeer()},
					Ports: dnsPorts(),
				},
			},
		},
	}
	// P2 (R16 review): the model endpoint is a non-secret Loop spec field
	// (agent.modelEndpoint), not read from the Secret (which would require
	// cluster-wide secrets RBAC and a cluster-wide Secret informer).
	if loop.Spec.Agent.ModelEndpoint != "" {
		if peer := modelPeer(loop.Spec.Agent.ModelEndpoint); peer != nil {
			port := intstrPtr32(int32(modelEndpointPort(loop.Spec.Agent.ModelEndpoint)))
			proxyNP.Spec.Egress = append(proxyNP.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
				To:    []networkingv1.NetworkPolicyPeer{*peer},
				Ports: []networkingv1.NetworkPolicyPort{{Port: port, Protocol: new(corev1.ProtocolTCP)}},
			})
		}
	}
	if _, err := r.createOrUpdateNP(ctx, loop, proxyNP); err != nil {
		if merr := mapForeign(proxyNP.Name, err); merr != nil {
			return merr
		}
	}

	// I42c: the egress proxy NetworkPolicy is created only when the egress
	// proxy is expected (network allows present). Ingress: only from this
	// Loop's agent on 3128. Egress: the external world with carve-outs (the
	// proxy dials any port the allow specifies; the application layer enforces
	// the port) + DNS to kube-dns.
	if egressExpected {
		egressProxyNP := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      loopName + "-egress-proxy-netpol",
				Namespace: ns,
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: egressProxyL},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{
					{
						From:  []networkingv1.NetworkPolicyPeer{agentPeer},
						Ports: []networkingv1.NetworkPolicyPort{{Port: intstrPtr32(egressProxyPort), Protocol: new(corev1.ProtocolTCP)}},
					},
				},
				Egress: []networkingv1.NetworkPolicyEgressRule{
					{
						// Egress rule 1: external v4 with carve-outs (no port
						// restriction). The except list is derived from the egress
						// binary's own resolved-IP carve-outs (I42a,
						// egress.CarveOutCIDRsV4) plus the cluster's POD_CIDR and
						// SERVICE_CIDR (from the operator's config), so the two
						// layers cannot drift.
						To: []networkingv1.NetworkPolicyPeer{
							{
								IPBlock: &networkingv1.IPBlock{
									CIDR:   "0.0.0.0/0",
									Except: egressCarveOutCIDRs(r.PodCIDR, r.ServiceCIDR),
								},
							},
						},
					},
					{
						// Egress rule 1b (I42c review P3): the v6 mirror (plan's
						// "v6 mirror" rule) so an allowed host that resolves
						// AAAA-first works on a dual-stack cluster. Derived from the
						// egress binary's v6 carve-outs (egress.CarveOutCIDRsV6).
						To: []networkingv1.NetworkPolicyPeer{
							{
								IPBlock: &networkingv1.IPBlock{
									CIDR:   "::/0",
									Except: egress.CarveOutCIDRsV6(),
								},
							},
						},
					},
					{
						To:    []networkingv1.NetworkPolicyPeer{dnsPeer()},
						Ports: dnsPorts(),
					},
				},
			},
		}
		if _, err := r.createOrUpdateNP(ctx, loop, egressProxyNP); err != nil {
			if merr := mapForeign(egressProxyNP.Name, err); merr != nil {
				return merr
			}
		}
	}

	// I42c: clean up the egress proxy NetworkPolicy when the egress proxy is
	// no longer expected (the network allows went away). (The egress proxy
	// netpol is created only when egressExpected, above; cleanup runs in the
	// ELSE branch — a call inside the if-block would delete the netpol just
	// created.)
	if !egressExpected {
		if merr := r.cleanupEgressProxyNetpol(ctx, loop); merr != nil {
			return merr
		}
	}

	// I42c review P2 (round 3): the conflict is resolved once no createOrUpdateNP
	// hit a foreign netpol this reconcile. Clear NetworkPolicyConflict to
	// False/Resolved when it was previously True and no conflict ran (the
	// hadEgressConflict pattern); the live-objects gate in ensureSandbox is the
	// source of truth for suspension, so a stale condition cannot wedge it.
	if !foreignConflict && hadNetpolConflict(loop) {
		setCondition(loop, "NetworkPolicyConflict", metav1.ConditionFalse, "Resolved",
			"all per-Loop NetworkPolicies are controlled by this Loop")
	}
	if foreignConflict {
		if err := r.Status().Update(ctx, loop); err != nil {
			return fmt.Errorf("update Loop status (NetworkPolicyConflict): %w", err)
		}
		return nil
	}
	return nil
}

// foreignKaptPolicies reports whether any of this Loop's KubeArmorPolicies
// exists and is NOT controlled by the Loop (I42f review P2: the sandbox gate
// that holds the sandbox Suspended on a foreign KubeArmorPolicy must be
// order-independent of Enforcer.Apply, so it reads the live objects itself).
// It fails CLOSED on a real read error (true: hold Suspended; consistent with
// foreignNetPols). An ABSENT policy is not a conflict (Enforcer.Apply creates
// it). The three names are the KubeArmorPolicy names the Enforcer emits for
// this Loop (C6b agent policy + I42f proxy policies).
func foreignKaptPolicies(ctx context.Context, r *LoopReconciler, loop *coxv1alpha1.Loop) bool {
	for _, name := range []string{"coxswain-" + loop.Name, "coxswain-" + loop.Name + "-proxy", "coxswain-" + loop.Name + "-egress-proxy"} {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(engine.KubeArmorGVK)
		err := r.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: name}, obj)
		if apierrors.IsNotFound(err) {
			continue
		}
		// KubeArmor CRD absent: not a conflict only under --allow-unenforced
		// (D38). In production a missing KubeArmor CRD is a real read error
		// that fails closed (holds Suspended) so a misinstall does not
		// silently disable the inner fence (reviewer P1 on b25f77e).
		if meta.IsNoMatchError(err) && r.AllowUnenforced {
			continue
		}
		if err != nil {
			return true
		}
		if !metav1.IsControlledBy(obj, loop) {
			return true
		}
	}
	return false
}

// foreignNetPols reports whether any of this Loop's NetworkPolicies exists
// and is NOT controlled by the Loop (I42c review P2 round 3: the sandbox
// gate that holds the sandbox Suspended on a foreign netpol must be
// order-independent of ensureNetworkPolicy, so it reads the live objects
// itself). It fails CLOSED on a real read error (true: hold Suspended;
// consistent with needsEgressProxy): the reconcile requeues on the same
// error, so a transient failure is not terminal. An ABSENT netpol is not a
// conflict (ensureNetworkPolicy creates it).
func foreignNetPols(ctx context.Context, r *LoopReconciler, loop *coxv1alpha1.Loop) bool {
	for _, name := range []string{loop.Name + "-agent-netpol", loop.Name + "-proxy-netpol", loop.Name + "-egress-proxy-netpol"} {
		np := &networkingv1.NetworkPolicy{}
		err := r.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: name}, np)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return true
		}
		if !metav1.IsControlledBy(np, loop) {
			return true
		}
	}
	return false
}

// cleanupEgressProxyNetpol (I42c review P2) deletes the egress proxy
// NetworkPolicy when the egress proxy is no longer expected (the network
// allows went away). I42b's cleanupEgressProxy deletes the pod and Service; the
// netpol must go too, or it is drift the reconciler should own (and, with the
// P1 update fix, it would otherwise keep a stale except list). A FOREIGN netpol
// occupying the name is left alone (I2/I42b never-take-over: only delete when
// the netpol is controlled by this Loop).
func (r *LoopReconciler) cleanupEgressProxyNetpol(ctx context.Context, loop *coxv1alpha1.Loop) error {
	np := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: loop.Name + "-egress-proxy-netpol"}, np)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get egress proxy NetworkPolicy %s/%s: %w", loop.Namespace, loop.Name, err)
	}
	if !metav1.IsControlledBy(np, loop) {
		// Foreign object: leave it alone (never take over, I2/I42b).
		return nil
	}
	if err := r.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete egress proxy NetworkPolicy %s/%s: %w", loop.Namespace, loop.Name, err)
	}
	logf.FromContext(ctx).Info("cleaned up egress proxy NetworkPolicy (no network allows)",
		"networkPolicy", np.Name, "loop", loop.Name)
	return nil
}

// agentPodLabels is the label set the agent (sandbox) pod carries (D34):
// the per-Loop identity (coxswain.io/loop) plus the agent component. The
// per-Loop NetworkPolicy podSelector and the proxy's ingress peer select
// EXACTLY this set, so policies never leak across Loops in a namespace.
// C6b's KubeArmorPolicy selector also keys on coxswain.io/loop, so the two
// slices compose on the same label.

// createOrUpdateNP creates or updates a NetworkPolicy.
//
// I42c review P2 (round 2): the fixed createOrUpdateNP re-asserts the desired
// spec on an existing NetworkPolicy; it must NOT do so for a FOREIGN object
// occupying the name (the P1 fix would otherwise overwrite someone else's
// netpol with the Loop's). When the live object exists and is not controlled
// by the Loop, the mutate returns errForeignNetpol (sentinel) and the object
// is left untouched; the caller maps the sentinel to a NetworkPolicyConflict
// condition and holds the sandbox Suspended.
var errForeignNetpol = errors.New("NetworkPolicy is owned by another controller (foreign)")

func (r *LoopReconciler) createOrUpdateNP(ctx context.Context, loop *coxv1alpha1.Loop, np *networkingv1.NetworkPolicy) (controllerutil.OperationResult, error) {
	// I42c review P1: the desired spec must be applied inside the mutate
	// func. controllerutil.CreateOrUpdate first Gets the live object into np
	// (overwriting the spec we built), then runs the mutate; a no-op mutate
	// sees DeepEqual(existing, obj) and returns OperationResultNone, freezing
	// an existing NetworkPolicy's spec on the first reconcile (adds/removes
	// of network allows and POD_CIDR/SERVICE_CIDR changes would never
	// propagate). Capture the desired spec beforehand and re-assert it.
	desired := np.DeepCopy()
	return controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		// P2 (round 2): a foreign netpol of the same name is left untouched
		// (never take over, I2/I42b). controllerutil.CreateOrUpdate has
		// populated np's object meta from the Get, so np.CreationTimestamp
		// is zero only when it was the one we just created.
		if !np.CreationTimestamp.IsZero() && !metav1.IsControlledBy(np, loop) {
			return errForeignNetpol
		}
		if err := controllerutil.SetControllerReference(loop, np, r.Scheme); err != nil {
			return fmt.Errorf("set owner ref on NetworkPolicy %s: %w", np.Name, err)
		}
		np.Spec = *desired.Spec.DeepCopy()
		if np.Labels == nil {
			np.Labels = map[string]string{}
		}
		maps.Copy(np.Labels, desired.Labels)
		return nil
	})
}

// netpolAgentComponent is the app.kubernetes.io/component label value for
// the agent pod (D34 NetworkPolicy uses this to select the agent pod).

func dnsPeer() networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"kubernetes.io/metadata.name": "kube-system",
			},
		},
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"k8s-app": "kube-dns",
			},
		},
	}
}

// dnsPorts returns the DNS egress/ingress port set (53 UDP + TCP).

func dnsPorts() []networkingv1.NetworkPolicyPort {
	return []networkingv1.NetworkPolicyPort{
		{Port: intstrPtr32(53), Protocol: new(corev1.ProtocolUDP)},
		{Port: intstrPtr32(53), Protocol: new(corev1.ProtocolTCP)},
	}
}

// egressCarveOutCIDRs is the except list for the egress proxy's external
// egress ipBlock (0.0.0.0/0 minus these). It is DERIVED from
// internal/egress.CarveOutCIDRsV4 (the egress binary's own resolved-IP
// carve-outs, I42a) plus the cluster's POD_CIDR and SERVICE_CIDR (from the
// operator's config, not per-Loop discovery), so the netpol layer and the
// egress binary layer cannot drift (I42c review P2: a second hand-written v4
// list had already lost 100.64.0.0/10). An empty POD_CIDR / SERVICE_CIDR
// contributes no entry (the operator was not configured with those CIDRs).
func egressCarveOutCIDRs(podCIDR, serviceCIDR string) []string {
	cidrs := append([]string{}, egress.CarveOutCIDRsV4()...)
	if podCIDR != "" {
		cidrs = append(cidrs, podCIDR)
	}
	if serviceCIDR != "" {
		cidrs = append(cidrs, serviceCIDR)
	}
	return cidrs
}

// modelPeer is the NetworkPolicy peer for the proxy's model egress (P1-4,
// R16 review). NetworkPolicy cannot match DNS names, so:
//   - a bare in-cluster Service name (single-label, same namespace as the
//     policy): a podSelector over all pods in that namespace — the tightest
//     selector that resolves the name without needing to watch Services. The
//     hostname-level precision (only this host, not the whole namespace) is
//     enforced by the proxy's KubeArmor policy (D35) which CAN match the DNS
//     query. Recorded as a known limit in ADR-0007 alongside I41.
//   - a numeric IP: an exact ipBlock /32.
//   - anything else (external FQDN that does not resolve here): no peer —
//     the model egress rule is omitted entirely (fail-closed), and the
//     hostname-level allow belongs to D35's KubeArmor proxy policy.

func modelPeer(endpoint string) *networkingv1.NetworkPolicyPeer {
	host := endpoint
	if strings.Contains(endpoint, "://") {
		if u, err := neturl.Parse(endpoint); err == nil && u.Host != "" {
			host = u.Host
		}
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if ip := net.ParseIP(host); ip != nil {
		cidr := ip.String() + "/32"
		return &networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}}
	}
	if len(host) > 0 && host[0] != '.' && !strings.Contains(host, ".") {
		return &networkingv1.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{}},
		}
	}
	return nil
}

// ensureNetworkPolicy creates the per-Loop NetworkPolicies (D34):
//  1. Agent pod: ingress deny-all; egress only to THIS Loop's proxy on 8080
//     and cluster DNS (kube-dns in kube-system, port 53 UDP/TCP).
//  2. Proxy pod: ingress only from THIS Loop's agent on 8080; egress only to
//     the model endpoint peer (pod selector or ipBlock) and cluster DNS.
//
// Every selector and peer is per-Loop (P1-1, R16 review): the agent pod
// carries coxswain.io/loop=<loop> + the agent component label (set on the
// Sandbox pod template by ensureSandbox), the proxy pod carries
// coxswain.io/proxy-for=<loop> (D33). Two Loops in one namespace therefore
// get two disjoint policy pairs and no cross-Loop traffic is allowed.
//
// Both policies are owner-ref'd to the Loop so they are GC'd with it.

func agentPodLabels(loopName string) map[string]string {
	return map[string]string{
		"coxswain.io/loop":       loopName,
		policy.ComponentLabelKey: netpolAgentComponent,
	}
}

// intstrPtr32 returns a pointer to an intstr.IntOrString with the given int.

// ensureProxy creates the Loop's per-Loop model-proxy pod + Service (D33,
// replaces the C2a sidecar). It is idempotent. The model-creds Secret is
// mounted read-only into the proxy pod ONLY (C2/ADR-0006 item 2); the agent
// pod never sees the key. Both objects are controller-owned by the Loop so
// they are garbage-collected with it (a deleted Loop never leaves a live
// proxy holding a key behind).

// modelEndpointValue returns the model endpoint as a full URL for the proxy's
// MODEL_ENDPOINT env var. A bare host:port (no scheme) gets "http://"
// prefixed (httputil.NewSingleHostReverseProxy requires a full URL).
func modelEndpointValue(loop *coxv1alpha1.Loop) string {
	eps := loop.Spec.Agent.ModelEndpoint
	if eps == "" {
		return ""
	}
	if strings.HasPrefix(eps, "http://") || strings.HasPrefix(eps, "https://") {
		return eps
	}
	return "http://" + eps
}

func modelEndpointPort(rawURL string) int {
	// If it looks like a URL (has a scheme), parse it as such.
	if strings.Contains(rawURL, "://") {
		if u, err := neturl.Parse(rawURL); err == nil {
			if p, err2 := strconv.Atoi(u.Port()); err2 == nil {
				return p
			}
			return 0
		}
	}
	// Otherwise treat it as host:port.
	if _, p, err := net.SplitHostPort(rawURL); err == nil {
		if ip, err2 := strconv.Atoi(p); err2 == nil {
			return ip
		}
	}
	return 0
}

// createOrUpdateNP creates or updates a NetworkPolicy.

func intstrPtr32(v int32) *intstr.IntOrString {
	ips := intstr.FromInt32(v)
	return &ips
}

// modelEndpointPort extracts the port from a model endpoint (host:port or URL).

const (
	// netpolAgentComponent is the component label the D34 agent NetworkPolicy
	// selects on (alongside coxswain.io/loop). It aliases the policy package's
	// component constant (R16 I45: one source of truth for the label value,
	// shared with the I45 ValidatingAdmissionPolicy pin).
	netpolAgentComponent = policy.ComponentAgentLabel
	// netpolEgressProxyComponent is the component label value (and the egress
	// proxy container name) the I42b egress proxy pod uses. It aliases the
	// policy package's component constant (R16 I44 item 3: one source of truth
	// for the label value, so a rename cannot desync the selector from the pod).
	netpolEgressProxyComponent = policy.ComponentEgressProxyLabel
	// netpolProxyForLabel is the per-Loop label the D34 proxy NetworkPolicy
	// uses to scope to a specific Loop.
	netpolProxyForLabel = "coxswain.io/proxy-for"
)

// isPodReady reports whether a Pod has the PodReady condition set to True
// (D35a: the sandbox must not be Running until the proxy pod is Ready).
func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// applyEffectivePolicyAndConditions (C6b+I41+D30) resolves the effective
// policy, applies it through the Enforcer, records the hash, and sets the
// PolicyTranslationLossy and PolicyEnforced conditions.
func (r *LoopReconciler) applyEffectivePolicyAndConditions(ctx context.Context, loop *coxv1alpha1.Loop) error {
	effective, err := r.effectivePolicy(ctx, loop)
	if err != nil {
		return err
	}
	if r.Enforcer != nil {
		// I42f review P2 (round 1): a foreign KubeArmorPolicy occupying one of
		// the Loop's policy names is NEVER overwritten (createOrUpdateKapt's
		// errForeignKapt sentinel). The same pattern as I42c's
		// NetworkPolicyConflict: set KubeArmorPolicyConflict=True/
		// ForeignKubeArmorPolicy and hold the sandbox Suspended (the
		// order-independent gate in ensureSandbox reads the live objects).
		// Reconcile does not error-loop.
		applyErr := r.Enforcer.Apply(ctx, loop, effective)
		if applyErr != nil && errors.Is(applyErr, engine.ErrForeignKapt) {
			setCondition(loop, "KubeArmorPolicyConflict", metav1.ConditionTrue, "ForeignKubeArmorPolicy",
				"a KubeArmorPolicy controlled by another controller occupies a coxswain-<loop> name; left untouched")
			return r.Status().Update(ctx, loop)
		}
		// A conflict was resolved this reconcile (a previously foreign policy
		// is gone or now Loop-owned): clear it to False/Resolved when it was
		// previously set (the hadNetpolConflict pattern).
		if hadKaptConflict(loop) {
			setCondition(loop, "KubeArmorPolicyConflict", metav1.ConditionFalse, "Resolved",
				"all per-Loop KubeArmorPolicies are controlled by this Loop")
			if err := r.Status().Update(ctx, loop); err != nil {
				return fmt.Errorf("update Loop status (KubeArmorPolicyConflict resolved): %w", err)
			}
		}
		if applyErr != nil {
			return applyErr
		}
	}
	if lossy := engine.NetworkLossy(effective.Network); len(lossy) > 0 {
		setCondition(loop, PolicyTranslationLossyCondition, metav1.ConditionTrue,
			"KubeArmorDroppedPorts",
			"these network allows lost their port in the KubeArmor translation and are enforced by hostname only (any port) until I42 is resolved: "+strings.Join(lossy, ", "))
	} else {
		setCondition(loop, PolicyTranslationLossyCondition, metav1.ConditionFalse,
			"NoPortLoss", "all network allows are expressible at full precision by the KubeArmor translation")
	}
	if enf, rs := r.enforcementStatus(ctx, loop); enf && rs != engine.ReasonEnforcementDisabled {
		setCondition(loop, string(PolicyEnforcedCondition), metav1.ConditionTrue, "Enforcing",
			"the eBPF engine is enforcing the Loop's effective policy")
	} else {
		msg := "engine not enforcing the Loop policy; sandbox held Suspended (D30 fail-closed)"
		if rs == engine.ReasonEnforcementDisabled {
			msg = "--allow-unenforced is set: the Loop runs but is NOT enforced (dev escape hatch)"
		}
		setCondition(loop, string(PolicyEnforcedCondition), metav1.ConditionFalse, rs, msg)
	}
	// D38: the NetworkEnforced condition (network layer, analogous to
	// PolicyEnforced). The escape hatch is AllowUnenforcedNetwork (separate
	// from AllowUnenforced — the two flags are independent). A condition
	// change emits a Kubernetes Event (design point 3).
	if _, reason, status, msg := r.cniNetworkStatus(); status != "" {
		if r.networkEnforcedChanged(loop, status, string(reason), msg) {
			setCondition(loop, cni.NetworkEnforcedCondition, status, string(reason), msg)
			r.emitNetworkEnforcedEvent(loop, status, string(reason), msg)
		}
	}
	return nil
}

// ensureProxyAndNetPolicies runs the D33 proxy, D34 NetworkPolicy, and D35
// condition snapshot. It returns whether the Loop's conditions changed.
func (r *LoopReconciler) ensureProxyAndNetPolicies(ctx context.Context, loop *coxv1alpha1.Loop) (bool, error) {
	condsBefore := make([]metav1.Condition, len(loop.Status.Conditions))
	copy(condsBefore, loop.Status.Conditions)

	if err := r.ensureProxyOrCleanup(ctx, loop); err != nil {
		return false, err
	}
	// I42b: the egress proxy pod + Service (gated on network allows).
	if err := r.ensureEgressProxy(ctx, loop); err != nil {
		return false, err
	}
	if err := r.ensureNetPoliciesIfConfigured(ctx, loop); err != nil {
		return false, err
	}
	return !equality.Semantic.DeepEqual(condsBefore, loop.Status.Conditions), nil
}

// ensureNetPoliciesIfConfigured creates the per-Loop NetworkPolicies when
// they are expected: a model endpoint is configured (D34: the model proxy and
// the agent NetworkPolicies are only useful when there is a model to reach),
// or the effective policy has network allows (I42c: the egress proxy and its
// NetworkPolicy are expected). A no-op when neither is the case (nothing to
// allow-egress to).
func (r *LoopReconciler) ensureNetPoliciesIfConfigured(ctx context.Context, loop *coxv1alpha1.Loop) error {
	if loop.Spec.Agent.EndpointSecretRef == "" && !needsEgressProxy(ctx, r, loop) {
		return nil
	}
	return r.ensureNetworkPolicy(ctx, loop)
}

// setCondition upserts a condition on the Loop's status. The condition's Type
// is a string (a terminal phase like "Failed", or a non-phase condition type
// like "PolicyEnforced" or "PolicyValid"). It is the operator's audit record
// (ADR-0004); the runner never writes it.
func setCondition(loop *coxv1alpha1.Loop, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == condType {
			if loop.Status.Conditions[i].Reason == reason &&
				loop.Status.Conditions[i].Message == message &&
				loop.Status.Conditions[i].Status == status {
				return // unchanged
			}
			loop.Status.Conditions[i].Status = status
			loop.Status.Conditions[i].Reason = reason
			loop.Status.Conditions[i].Message = message
			loop.Status.Conditions[i].LastTransitionTime = now
			return
		}
	}
	loop.Status.Conditions = append(loop.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

// sandboxImage returns the configured sandbox image, or a sensible Go dev default.
func (r *LoopReconciler) sandboxImage() string {
	if r.SandboxImage != "" {
		return r.SandboxImage
	}
	return "docker.io/library/golang:1.26"
}

// proxyImage returns the model proxy pod image. It is the reconciler's
// ProxyImage field (settable in tests and future slices; a manager flag
// --proxy-image is a candidate for a future C2b), or the forwarding proxy
// stand-in (coxswain-proxy:standin) when unset. The stand-in is a Go reverse
// proxy that reads the model-creds Secret at startup and forwards to
// MODEL_ENDPOINT.
func (r *LoopReconciler) proxyImage() string {
	if r.ProxyImage != "" {
		return r.ProxyImage
	}
	return "coxswain-proxy:standin"
}

// loopPolicyRefsFieldIndex is a field index on Loop.spec.policyRefs, used by
// the AgentPolicy watch's map function to efficiently find the Loops that
// reference a given AgentPolicy (R15 round 4 P2).
const loopPolicyRefsFieldIndex = "spec.policyRefs"

// SetupWithManager sets up the controller with the Manager.
func (r *LoopReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// I42e + I42c: read the cluster's pod/service CIDRs from the environment
	// (set at deployment, e.g. kind/k3s exposes these as --pod-network-cidr /
	// --service-cluster-ip-range flags). Empty values mean the CIDR cases are
	// not checked (the CRD CEL rule still catches .svc / localhost).
	if r.PodCIDR == "" {
		r.PodCIDR = os.Getenv("POD_CIDR")
	}
	if r.ServiceCIDR == "" {
		r.ServiceCIDR = os.Getenv("SERVICE_CIDR")
	}
	// I42c review P2: validate the CIDRs. A v6 or comma-separated dual-stack
	// value, or a typo, would be put into a v4 ipBlock.except entry; the API
	// server would reject the NetworkPolicy and every reconcile would error.
	// Fail fast at startup instead (fail-closed: an invalid value must not be
	// silently dropped into the netpol).
	for name, val := range map[string]string{"POD_CIDR": r.PodCIDR, "SERVICE_CIDR": r.ServiceCIDR} {
		if val == "" {
			continue
		}
		p, err := netip.ParsePrefix(val)
		if err != nil || !p.Addr().Is4() {
			return fmt.Errorf("%s must be a single IPv4 CIDR (got %q; err: %v)", name, val, err)
		}
	}
	// P3 (I42e review): make an unset CIDR config visible. When either is
	// empty the IP-in-pod/service-CIDR cases of findInClusterNetworkAllow are
	// skipped, so an in-cluster allow given as a bare IP literal in that range
	// is not caught by the first layer (only the egress proxy's resolved-IP
	// backstop is). Log once at startup so a misconfigured install is obvious.
	if r.PodCIDR == "" || r.ServiceCIDR == "" {
		logf.Log.Info("POD_CIDR / SERVICE_CIDR unset: the IP-in-pod/service-CIDR in-cluster-allow check is disabled; an in-cluster allow given as a bare IP in that range is caught only by the egress proxy's resolved-IP backstop",
			"podCIDR", r.PodCIDR, "serviceCIDR", r.ServiceCIDR)
	}

	// Field index: Loop.spec.policyRefs (R15 round 4 P2: the AgentPolicy
	// watch's map function uses this index to find the Loops that reference
	// a given AgentPolicy, avoiding a namespace-wide list per event).
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &coxv1alpha1.Loop{}, loopPolicyRefsFieldIndex,
		func(obj client.Object) []string {
			loop, ok := obj.(*coxv1alpha1.Loop)
			if !ok {
				return nil
			}
			return loop.Spec.PolicyRefs
		}); err != nil {
		return fmt.Errorf("index Loop.spec.policyRefs: %w", err)
	}
	b := ctrl.NewControllerManagedBy(mgr).
		For(&coxv1alpha1.Loop{}).
		Owns(&sandboxv1beta1.Sandbox{}).
		// D33: the operator owns the per-Loop proxy pod + Service (ensureProxy).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		// Watch AgentPolicy: when a referenced policy is created, edited, or
		// deleted, re-reconcile the Loops that reference it (R15 round 4 P2:
		// a policy created after its Loop must not leave the Loop stuck at
		// PolicyNotFound; a policy edit must refresh the recorded hash).
		Watches(&coxv1alpha1.AgentPolicy{}, handler.EnqueueRequestsFromMapFunc(
			r.agentPolicyToLoopRequests))
	// D38: watch the CNI probe re-gate source (design point 3a). A probe result
	// change enqueues every Loop so it re-reads the cached result and re-gates.
	// Skipped when nil (envtest: the result is read on each reconcile anyway).
	// D38: watch the CNI probe re-gate source (design point 3a). A probe result
	// change enqueues every Loop so it re-reads the cached result and re-gates.
	// Skipped when nil (envtest: the result is read on each reconcile anyway).
	if r.CNIRegateSource != nil {
		b = b.WatchesRawSource(r.CNIRegateSource)
	}
	return b.Named("loop").
		Complete(r)
}

// agentPolicyToLoopRequests maps an AgentPolicy to the Loops in its namespace
// whose spec.policyRefs contains its name. Used by the AgentPolicy watch to
// re-reconcile the affected Loops when the policy changes (R15 round 4 P2).
// The field index on spec.policyRefs (registered in SetupWithManager and in
// the envtest suite) makes this an O(1) lookup.
func (r *LoopReconciler) agentPolicyToLoopRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	ap, ok := obj.(*coxv1alpha1.AgentPolicy)
	if !ok {
		return nil
	}
	loops := &coxv1alpha1.LoopList{}
	if err := r.List(ctx, loops,
		client.InNamespace(ap.Namespace),
		client.MatchingFields{loopPolicyRefsFieldIndex: ap.Name},
	); err != nil {
		logf.FromContext(ctx).Error(err, "list Loops by policyRefs index for AgentPolicy watch",
			"policy", ap.Name, "namespace", ap.Namespace)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(loops.Items))
	for i := range loops.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: loops.Items[i].Namespace,
				Name:      loops.Items[i].Name,
			},
		})
	}
	return requests
}

// suspendSandboxIfRunning sets the sandbox's operatingMode to Suspended if it
// is currently Running. Called when a Loop's AgentPolicy becomes invalid
// (R15 round 3: an already-running sandbox must be suspended, not left
// as-is). If the sandbox doesn't exist, this is a no-op.
func (r *LoopReconciler) suspendSandboxIfRunning(ctx context.Context, loop *coxv1alpha1.Loop) error {
	sb := &sandboxv1beta1.Sandbox{}
	key := client.ObjectKey{Namespace: loop.Namespace, Name: sandboxName(loop.Name)}
	if err := r.Get(ctx, key, sb); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // no sandbox to suspend
		}
		return fmt.Errorf("get sandbox %s: %w", key, err)
	}
	if sb.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeSuspended {
		return nil // already suspended
	}
	sb.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
	if err := r.Update(ctx, sb); err != nil {
		return fmt.Errorf("suspend sandbox %s: %w", key, err)
	}
	logf.FromContext(ctx).Info("suspended sandbox (policy invalid)", "sandbox", sandboxName(loop.Name), "loop", loop.Name)
	return nil
}

// policyValidationResult is the outcome of validating a Loop's referenced
// AgentPolicies before the sandbox is created or the eBPF engine is reached.
type policyValidationResult struct {
	// valid is true when all referenced policies exist and have canonical
	// exec paths.
	valid bool
	// reason is the metav1.ConditionReason (NonCanonicalExecPath or
	// PolicyNotFound) when valid is false.
	reason string
	// message is the human-readable explanation.
	message            string
	transientReadError bool
}

// findInClusterNetworkAllow (I42e) delegates to the pure policy check. It
// catches an in-cluster network allow — an in-cluster name (mirroring the CRD
// CEL rule, so pre-rule objects and future CRD drift are caught), a loopback /
// unspecified / link-local hostname, or an IP literal inside the operator's
// pod/service CIDR — before the egress proxy is created.
func (r *LoopReconciler) findInClusterNetworkAllow(allows []string) (string, bool) {
	return policy.FindInClusterNetworkAllow(allows, r.PodCIDR, r.ServiceCIDR)
}

// validateAgentPolicies checks that every referenced AgentPolicy exists and
// has canonical exec paths and no in-cluster network allows (I42e).
// Returns a policyValidationResult. The
// transientReadError field is true when the policy could not be read due to a
// transient error (not a NotFound), in which case the caller should requeue.
// R15 round 3: a missing or unreadable referenced policy is treated as
// not-valid (PolicyNotFound), not ignored (fail-closed).
// I42e: an in-cluster network allow (localhost / IP in pod/service CIDR) is
// rejected with reason InClusterAllow (PolicyValid=False + sandbox suspend,
// same fail-closed pattern as C6a's PolicyNotFound).
// Runs BEFORE ensureSandbox so a bad policy never creates a sandbox pod.
func (r *LoopReconciler) validateAgentPolicies(ctx context.Context, loop *coxv1alpha1.Loop) policyValidationResult {
	unionNetwork := make([]string, 0)
	for _, name := range loop.Spec.PolicyRefs {
		ap := &coxv1alpha1.AgentPolicy{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: name}, ap); err != nil {
			if apierrors.IsNotFound(err) {
				return policyValidationResult{valid: false, reason: "PolicyNotFound",
					message: fmt.Sprintf("AgentPolicy %s/%s not found (referenced by policyRefs)", loop.Namespace, name)}
			}
			// Unreadable (transient error): fail closed, requeue.
			return policyValidationResult{valid: false, reason: "PolicyNotFound",
				message:            fmt.Sprintf("AgentPolicy %s/%s could not be read: %v", loop.Namespace, name, err),
				transientReadError: true}
		}
		for _, e := range ap.Spec.Exec {
			if isNonCanonicalPath(e) {
				return policyValidationResult{valid: false, reason: "NonCanonicalExecPath",
					message: fmt.Sprintf("AgentPolicy %s: exec entry %q is non-canonical (no ., .., //, or trailing /)", name, e)}
			}
		}
		unionNetwork = append(unionNetwork, ap.Spec.Network...)
	}
	// I42e: reject in-cluster network allows. The CRD CEL rule handles the
	// .svc / .svc.cluster.local / localhost / 127.0.0.1 name cases at
	// admission; the controller catches the same name cases (mirrored, so pre-
	// rule objects and future CRD drift are caught), the other loopback /
	// unspecified / link-local forms, and the IP-in-pod/service-CIDR cases
	// (which need the operator's CIDR config).
	if offending, ok := r.findInClusterNetworkAllow(unionNetwork); ok {
		return policyValidationResult{valid: false, reason: "InClusterAllow",
			message: fmt.Sprintf("AgentPolicy network allow %q names an in-cluster target (.svc / .svc.cluster.local / cluster.local, localhost, a loopback / unspecified / link-local IP, or an IP in the pod or service CIDR); the agent's external egress is enforced by the egress proxy and an in-cluster target is an SSRF path", offending)}
	}
	return policyValidationResult{valid: true}
}

// isNonCanonicalPath returns true if the path is non-canonical: it contains
// a '.' or '..' segment, a '//' (double slash), or ends with a '/'. The eBPF
// engine and the filesystem normalize such paths, so a non-canonical path
// that looks outside the writable mounts could resolve to one (e.g.
// "/usr/../tmp/git" or "/./tmp/git"). The CRD CEL XValidation catches the
// common case at admission (with items:MaxLength bounding the string length);
// the controller is the second line of defence (catches everything).
func isNonCanonicalPath(p string) bool {
	if strings.Contains(p, "..") {
		return true
	}
	// Catch '.' segments: "/./tmp/git" resolves to "/tmp/git".
	if strings.Contains(p, "/./") {
		return true
	}
	if strings.Contains(p, "//") {
		return true
	}
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		return true
	}
	return false
}
