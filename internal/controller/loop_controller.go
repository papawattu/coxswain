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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
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
	// B2 (D10): at Verifying, the operator's own tamper evidence is the gate.
	// A non-zero tamper-check exit code (a protected path differs between the
	// two operator-pinned SHAs) ends the Loop Failed:TamperedVerify, TERMINAL,
	// before any acceptance check runs. This is independent of the runner's
	// result.json claim (tamperVerdict never reads it). In envtest the B-slice
	// tests set status.verify.tamperExitCode directly (no Job controller); in a
	// real cluster B3 reads it from the verify Job pod's initContainerStatuses.
	if loop.Status.Phase == coxv1alpha1.LoopPhaseVerifying &&
		loop.Status.Verify != nil &&
		loop.Status.Verify.TamperExitCode > 0 {
		if tampered, reason := tamperVerdict(loop.Status.Verify.TamperExitCode, false); tampered {
			loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
			setCondition(&loop, coxv1alpha1.LoopPhaseFailed, metav1.ConditionTrue, reason,
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
		// Honor spec.suspend: a suspended Loop must not run a Running sandbox
		// (S1). Running is the default for a normal Loop.
		if loop.Spec.Suspend {
			desired.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
		} else {
			desired.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
		}
		desired.Spec.PodTemplate.Spec.Containers = []corev1.Container{
			{
				Name:  "agent",
				Image: r.sandboxImage(),
				// Keep the container alive until the phase driver (Phase 1)
				// takes over. sleep infinity is a stand-in.
				Command: []string{"sh", "-c", "sleep infinity"},
			},
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

// tamperVerdict is the operator's TamperedVerify decision at Verifying (B2).
//
// It is a pure function of the tamper-check container's exit code and the
// runner's claim from result.json:
//   - tamperExitCode != 0 (a protected path differs between the two operator-
//     pinned SHAs, per the Job's `git diff --name-only <baseCommit>
//     <verifiedCommit> -- <globs>`) ⇒ TamperedVerify, terminal, BEFORE any
//     check runs. This is independent of resultClaimsSuccess — the anti-gaming
//     property (D10): a runner that edited a protected file and reported
//     status=success still ends Failed:TamperedVerify.
//   - tamperExitCode == 0 ⇒ not tampered; the verify proceeds to the check
//     containers (B3). resultClaimsSuccess is irrelevant to the tamper
//     decision (it is claims-grade, never a gate).
func tamperVerdict(tamperExitCode int, resultClaimsSuccess bool) (tampered bool, terminalReason string) {
	_ = resultClaimsSuccess // deliberately unused: the decision never reads a claim
	if tamperExitCode != 0 {
		return true, TamperedVerifyReason
	}
	return false, ""
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

// SetupWithManager sets up the controller with the Manager.
func (r *LoopReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&coxv1alpha1.Loop{}).
		Owns(&sandboxv1beta1.Sandbox{}).
		Named("loop").
		Complete(r)
}
