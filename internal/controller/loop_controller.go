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

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// C2 (ADR-0006 item 2): the model proxy sidecar contract. The agent holds no
// model key; it talks to the local proxy on COX_MODEL_BASE_URL, which holds the
// key (mounted only into the proxy) and injects auth.
const (
	// proxyContainerName is the name of the model proxy sidecar container.
	proxyContainerName = "proxy"
	// modelCredsVolume is the name of the Secret volume that carries the model
	// API key + base URL (mounted read-only into the proxy only).
	modelCredsVolume = "model-creds"
	// coxModelBaseURL is the env var the operator sets on the agent so it talks
	// to the local proxy (a Loop cannot override it: COX_* names are rejected
	// at admission, I34).
	coxModelBaseURL = "COX_MODEL_BASE_URL"
	// localhostProxyBaseURL is where the proxy listens on the sandbox pod's
	// loopback interface. The agent reaches it over localhost, not the network.
	localhostProxyBaseURL = "http://localhost:8080"
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
		// C2 (P2): the proxy runs as its OWN non-root UID, distinct from the agent,
		// so a future shared PID namespace or shared volume cannot expose the model
		// key to the untrusted agent.
		proxyUID := int64(65533)
		proxyGID := int64(65533)
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
		// C2 (ADR-0006 item 2): the agent holds no model key. It talks to the local
		// proxy sidecar (COX_MODEL_BASE_URL), which holds the key and injects auth.
		// The operator sets this; a Loop cannot override it (COX_* names are
		// rejected at admission, I34) so the agent cannot be pointed past the proxy.
		// Only set when a model endpoint exists (P1: no half-configured proxy).
		if hasModel {
			agentEnv = append(agentEnv, corev1.EnvVar{Name: coxModelBaseURL, Value: localhostProxyBaseURL})
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
		podContainers := []corev1.Container{agentContainer}
		if hasModel {
			// C2 (ADR-0006 item 2): the model proxy sidecar. It holds the model key
			// (mounted ONLY here, from the model-creds Secret, as a read-only FILE —
			// not env, P2), injects the auth header, forwards only to the configured
			// endpoint, and meters tokens (the Phase 2 metering sidecar, built now as
			// the credential boundary). The agent reaches it on localhost:8080 and
			// holds no key. Runs as its OWN UID (P2). sleep infinity is a stand-in
			// until the proxy binary exists (C2b).
			podContainers = append(podContainers, corev1.Container{
				Name:    proxyContainerName,
				Image:   r.proxyImage(),
				Command: []string{"sh", "-c", "sleep infinity"},
				// P2: the key is delivered ONLY as the /model-creds file mount below,
				// never via env (env leaks to child processes, crash dumps, and
				// /proc/<pid>/environ).
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
				VolumeMounts: []corev1.VolumeMount{
					// The model key lives ONLY here (the proxy), never the agent (C2).
					{Name: modelCredsVolume, MountPath: "/model-creds", ReadOnly: true},
				},
			})
		}
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
		// C2 (ADR-0006 item 2): the model-creds Secret volume, mounted (read-only)
		// only into the proxy container above. It carries the model API key +
		// base URL; the agent never sees it.
		if loop.Spec.Agent.EndpointSecretRef != "" {
			secretMode := readOnlyMode
			desired.Spec.PodTemplate.Spec.Volumes = append(desired.Spec.PodTemplate.Spec.Volumes,
				corev1.Volume{
					Name: modelCredsVolume,
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName:  loop.Spec.Agent.EndpointSecretRef,
							DefaultMode: &secretMode,
						},
					},
				})
		}
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
		Named("loop").
		Complete(r)
}
