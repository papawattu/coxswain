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
	"fmt"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// C2 (ADR-0006 item 2): the model proxy contract. The agent holds no model
// key; it talks to the per-Loop proxy on COX_MODEL_BASE_URL. D33
// (REVIEW-PHASE1-R13, owner option c) moved the proxy OUT of the sandbox pod
// into its own operator-owned pod + Service: NetworkPolicy and KubeArmorPolicy
// are pod-scoped and agent-sandbox allows exactly one pod per Sandbox, so a
// sidecar can never be split from the agent for egress (D29).
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
	proxySpecHashAnnotation = "coxswain.io/proxy-spec-hash"
	// modelCredsVolume is the name of the Secret volume that carries the model
	// API key + base URL (mounted read-only into the proxy pod only).
	modelCredsVolume = "model-creds"
	// coxModelBaseURL is the env var the operator sets on the agent so it talks
	// to the proxy (a Loop cannot override it: COX_* names are rejected at
	// admission, I34) so the agent cannot be pointed past the proxy.
	coxModelBaseURL = "COX_MODEL_BASE_URL"
	// proxyPort is where the proxy listens; the per-Loop Service exposes it.
	proxyPort int32 = 8080
	// readOnlyMode is the default file mode for the model-creds Secret volume
	// (0444: the key is read-only, even in the proxy).
	readOnlyMode int32 = 0o444
)

// LoopReconciler reconciles a Loop object.
//
// Phase 0 scope: ensure the Loop's Sandbox exists and log it. The phase state
// machine (Planning → Implementing → Verifying, budgets, stall, checkpoints)
// lands in Phase 1 (docs/PLAN.md).
type LoopReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// SandboxImage is the image the sandbox pod runs. Defaults to a Go dev
	// image; overridable for the smoke test (e.g. the runner image).
	SandboxImage string

	// ProxyImage is the model proxy sidecar image (C2). Defaults to a Go dev
	// stand-in; overridable for the smoke test (e.g. the real proxy image).
	ProxyImage string
}

// +kubebuilder:rbac:groups=coxswain.wattu.com,resources=loops,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coxswain.wattu.com,resources=loops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=coxswain.wattu.com,resources=loops/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes/status,verbs=get
// D33: the operator owns the per-Loop proxy pod + Service (ensureProxy): it
// creates/updates them, watches them (Owns mapping), and lets GC delete them
// with the Loop. Only the verbs the controller actually uses.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update

// Reconcile moves the cluster state closer to the Loop's desired state.
func (r *LoopReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var loop coxv1alpha1.Loop
	if err := r.Get(ctx, req.NamespacedName, &loop); err != nil {
		// Deleted or never existed: nothing to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.ensureSandbox(ctx, &loop); err != nil {
		return ctrl.Result{}, err
	}

	// D33: the per-Loop proxy pod + Service exist only when a model endpoint is
	// configured (P1 parity: no half-configured proxy). Created after the
	// sandbox so the agent's COX_MODEL_BASE_URL target exists in the same pass.
	if loop.Spec.Agent.EndpointSecretRef != "" {
		if err := r.ensureProxy(ctx, &loop); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Combine the two status updates (observedGeneration + phase) into one so a
	// reconcile does at most one Status().Update (P3 tidy-up).
	changed := false
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
			setCondition(&loop, coxv1alpha1.LoopPhaseFailed, metav1.ConditionTrue, TamperedVerifyReason,
				"a protected path changed between baseCommit and verifiedCommit; terminal")
			changed = true
		}
	}
	if changed {
		if err := r.Status().Update(ctx, &loop); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
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
		// C2 (P1): the proxy + model access exist only when a model endpoint is
		// configured. With no endpointSecretRef the agent runs with no model — no
		// proxy container, no key, no COX_MODEL_BASE_URL (never a half-configured
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
		desired.Spec.PodTemplate.Spec.AutomountServiceAccountToken = &falseP
		// C2 (P2): never share the process namespace. The proxy and agent run as
		// different UIDs, but a shared PID namespace would let the (untrusted) agent
		// read /proc/<proxy-pid>/environ and leak the model key. Explicitly false.
		noShare := false
		desired.Spec.PodTemplate.Spec.ShareProcessNamespace = &noShare
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
		// C2 (ADR-0006 item 2): the agent holds no model key. D33: it talks to the
		// per-Loop proxy Service (COX_MODEL_BASE_URL), which holds the key and
		// injects auth. The operator sets this; a Loop cannot override it (COX_*
		// names are rejected at admission, I34) so the agent cannot be pointed
		// past the proxy. Only set when a model endpoint exists (P1: no
		// half-configured proxy).
		if hasModel {
			agentEnv = append(agentEnv, corev1.EnvVar{Name: coxModelBaseURL, Value: proxyServiceURL(loop.Name, loop.Namespace)})
		}
		for _, e := range loop.Spec.Agent.Env {
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
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "workspace", MountPath: "/workspace"},
				{Name: "scratch", MountPath: "/scratch"},
				// P1 (R13): a writable /tmp so go build / mktemp / any tool that
				// honors TMPDIR or hard-codes /tmp works under a read-only rootfs.
				{Name: "tmp", MountPath: "/tmp"},
			},
		}
		// D33: the model proxy is NOT a sidecar container here — it is the
		// per-Loop proxy pod (ensureProxy). The sandbox pod has exactly one
		// container (the agent), so the pod-scoped egress policies of D34/D35
		// split cleanly between the two pods (D29).
		podContainers := []corev1.Container{agentContainer}
		desired.Spec.PodTemplate.Spec.Containers = podContainers
		// P3 (R13, I36): the writable emptyDirs carry explicit sizeLimits that sum
		// under the container's 1Gi ephemeral limit, so a full workspace/scratch/tmp
		// surfaces as a bounded pod eviction (and, after I36, a budget-aware signal)
		// rather than filling the node. 500+350+100 = 950Mi < 1Gi.
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
		// D33: the model-creds Secret volume is NOT on the sandbox pod — it is
		// mounted (read-only) into the per-Loop proxy pod (ensureProxy), the only
		// place the model key lives (C2/ADR-0006 item 2).
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

// proxyPodName / proxyServiceName return the per-Loop proxy pod and Service
// names: <loop>-proxy. The Loop name is CEL-validated to be a DNS-1035 label of
// at most 55 chars (D20), so both are valid DNS-1035 labels <= 63 chars.
func proxyPodName(loopName string) string {
	return loopName + proxyPodNameSuffix
}
func proxyServiceName(loopName string) string {
	return fmt.Sprintf(proxyServiceNameFmt, loopName)
}

// proxyServiceURL is the in-cluster Service URL the agent's COX_MODEL_BASE_URL
// points at (D33): http://<loop>-proxy.<namespace>.svc:8080. The per-Loop
// Service identity is also what makes the proxy's activity-audit records
// attributable to the Loop (D35).
func proxyServiceURL(loopName, namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", proxyServiceName(loopName), namespace, proxyPort)
}

// proxyLabels is the label set the per-Loop proxy pod carries and the proxy
// Service selects on (D33). The Service selects ONLY these labels, so no other
// pod in the namespace can be reached through <loop>-proxy (D29: this Loop's
// key is only usable by this Loop's agent; D34's NetworkPolicy enforces the
// rest).
func proxyLabels(loopName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/component": "model-proxy",
		"coxswain.io/proxy-for":       loopName,
	}
}

// ensureProxy creates the Loop's per-Loop model-proxy pod + Service (D33,
// replaces the C2a sidecar). It is idempotent. The model-creds Secret is
// mounted read-only into the proxy pod ONLY (C2/ADR-0006 item 2); the agent
// pod never sees the key. Both objects are controller-owned by the Loop so
// they are garbage-collected with it (a deleted Loop never leaves a live
// proxy holding a key behind).
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

	// --- proxy pod ---
	proxySpecHash := proxyPodSpecHash(loop)
	podDesired := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxyPodName(loopName),
			Namespace: ns,
			Labels:    proxyLabels(loopName),
		},
	}
	podOp, err := controllerutil.CreateOrUpdate(ctx, r.Client, podDesired, func() error {
		falseP := false
		trueP := true
		readOnlyRootfs := true
		// D33: the same hardening as the agent (I36 parity): non-root own UID
		// (distinct from the agent's 65532), read-only rootfs, drop ALL caps,
		// seccomp runtime default, no SA token, limits.
		proxyUID := int64(65533)
		proxyGID := int64(65533)
		podDesired.Spec.AutomountServiceAccountToken = &falseP
		podDesired.Spec.SecurityContext = &corev1.PodSecurityContext{
			RunAsUser:  &proxyUID,
			RunAsGroup: &proxyGID,
		}
		podDesired.Spec.Containers = []corev1.Container{{
			Name:  "proxy",
			Image: r.proxyImage(),
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
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			// D33 kind-run acceptance: the stand-in must prove at runtime that it
			// CAN read the 0444 model-creds Secret file (UID 65533, read-only
			// rootfs). It logs the read to stdout (-> the pod log) and then keeps
			// the pod alive. A permission error would surface as a crash / an
			// empty log, which the acceptance run checks for.
			Command: []string{
				"sh", "-c",
				"echo 'proxy-stand-in: checking model-creds'; " +
					"if head -c 64 /model-creds/* >/dev/null 2>&1; then " +
					"echo 'proxy: model-creds readable (0444 secret file present)'; " +
					"else " +
					"echo 'proxy: model-creds NOT readable' && exit 1; " +
					"fi; " +
					"exec sleep infinity",
			},
			VolumeMounts: []corev1.VolumeMount{
				// The model key lives ONLY here (the proxy pod), never the agent (C2).
				// P2: delivered as a read-only file, never env (env leaks to child
				// processes, crash dumps, /proc/<pid>/environ).
				{Name: modelCredsVolume, MountPath: "/model-creds", ReadOnly: true},
			},
		}}
		// The model-creds Secret volume: read-only file mount (0444), the only
		// place the model key lives in the cluster (C2/ADR-0006 item 2).
		secretMode := readOnlyMode
		podDesired.Spec.Volumes = []corev1.Volume{{
			Name: modelCredsVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  loop.Spec.Agent.EndpointSecretRef,
					DefaultMode: &secretMode,
				},
			},
		}}
		podDesired.Annotations = map[string]string{proxySpecHashAnnotation: proxySpecHash}
		return controllerutil.SetControllerReference(loop, podDesired, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensure proxy pod %s/%s: %w", ns, proxyPodName(loopName), err)
	}
	// P2 (R15): if the existing pod's spec hash doesn't match the desired
	// hash, the operator upgrade changed the pod spec. A bare Pod's spec is
	// immutable, so delete and recreate.
	if podOp == controllerutil.OperationResultUpdated {
		existing := &corev1.Pod{}
		if getErr := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: proxyPodName(loopName)}, existing); getErr == nil {
			if existing.Annotations[proxySpecHashAnnotation] != proxySpecHash {
				log.Info("proxy pod spec drift detected, deleting for recreation",
					"proxy", proxyPodName(loopName), "loop", loopName)
				if delErr := r.Delete(ctx, existing); delErr != nil {
					return fmt.Errorf("delete drifted proxy pod %s/%s: %w", ns, proxyPodName(loopName), delErr)
				}
			}
		}
	}

	if svcOp == controllerutil.OperationResultNone && podOp == controllerutil.OperationResultNone {
		log.V(1).Info("ensured loop proxy",
			"proxy", proxyPodName(loopName),
			"namespace", ns,
			"operation", "none",
			"loop", loopName,
		)
	} else {
		log.Info("ensured loop proxy",
			"proxy", proxyPodName(loopName),
			"namespace", ns,
			"operation", podOp,
			"loop", loopName,
		)
	}
	return nil
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

// setCondition upserts a condition on the Loop's status. The condition's Type
// is the terminal phase (e.g. "Failed") so each terminal outcome is recorded
// once with its reason (e.g. TamperedVerify). It is the operator's audit record
// (ADR-0004); the runner never writes it.
func setCondition(loop *coxv1alpha1.Loop, condType coxv1alpha1.LoopPhase, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == string(condType) {
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
		Type:               string(condType),
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

// proxyImage returns the model proxy sidecar image. It is the reconciler's
// ProxyImage field (set from a manager flag in cmd/main.go, like SandboxImage),
// or a dev stand-in (sleep infinity) when unset. The real proxy binary (auth
// injection, forward-only-to-endpoint, metering) is C2b.
func (r *LoopReconciler) proxyImage() string {
	if r.ProxyImage != "" {
		return r.ProxyImage
	}
	return "docker.io/library/golang:1.26"
}

// SetupWithManager sets up the controller with the Manager.
func (r *LoopReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&coxv1alpha1.Loop{}).
		Owns(&sandboxv1beta1.Sandbox{}).
		// P2 (R15): watch the per-Loop proxy pod and Service so a
		// deleted/evicted proxy triggers reconciliation and is recreated.
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Named("loop").
		Complete(r)
}

// proxyPodSpecHash computes a stable hash of the proxy pod's desired spec
// (P2, R15). The hash covers the fields that change across operator releases
// or Loop updates. Since endpointSecretRef is immutable (P2, R15), the only
// drift source is operator upgrades (image, command, limits). The hash is
// stored on the pod as the proxySpecHashAnnotation; on a mismatch the operator
// deletes and recreates the pod (spec.volumes is immutable on a bare Pod).
func proxyPodSpecHash(loop *coxv1alpha1.Loop) string {
	h := sha256.New()
	h.Write([]byte("proxy-spec-hash-v1|"))
	h.Write([]byte(loop.Spec.Agent.EndpointSecretRef))
	h.Write([]byte("|stand-in-v1"))
	return hex.EncodeToString(h.Sum(nil))
}
