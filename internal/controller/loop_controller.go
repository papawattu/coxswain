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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
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
	log := logf.FromContext(ctx)

	var loop coxv1alpha1.Loop
	if err := r.Get(ctx, req.NamespacedName, &loop); err != nil {
		// Deleted or never existed: nothing to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.ensureSandbox(ctx, &loop, log); err != nil {
		return ctrl.Result{}, err
	}

	// Record that we observed this generation.
	if loop.Status.ObservedGeneration != loop.Generation {
		loop.Status.ObservedGeneration = loop.Generation
		if err := r.Status().Update(ctx, &loop); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Phase 0: a fresh Loop is Pending. (Phase 1 drives the full phase machine.)
	if loop.Status.Phase == "" {
		loop.Status.Phase = coxv1alpha1.LoopPhasePending
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
func (r *LoopReconciler) ensureSandbox(ctx context.Context, loop *coxv1alpha1.Loop, log interface {
	Info(string, ...any)
}) error {
	desired := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      loop.Name + "-sandbox",
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
	log.Info("ensured loop sandbox",
		"sandbox", desired.Name,
		"operation", op,
		"loop", loop.Name,
	)
	return nil
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
