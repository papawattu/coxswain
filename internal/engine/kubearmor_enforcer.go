package engine

import (
	"context"
	"fmt"

	"github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/policy"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// KubeArmorEnforcer is the production Enforcer: it emits the KubeArmorPolicy for
// the Loop's effective policy (Apply) and reports whether the engine is
// enforcing (Enforcing). It writes the KubeArmorPolicy as an unstructured
// object (no KubeArmor Go type dependency) and owner-refs it to the Loop (P2).
type KubeArmorEnforcer struct {
	Client client.Client
}

// Apply emits (creates or updates) the KubeArmorPolicy for the Loop's effective
// policy, owned by the Loop.
func (e *KubeArmorEnforcer) Apply(ctx context.Context, loop *v1alpha1.Loop, p policy.EffectivePolicy) error {
	obj := EmitKubeArmorPolicy(loop.Name, loop.Namespace, policy.Translate(p, proxyServiceFQDN(loop.Name, loop.Namespace)))
	// P2: owner-ref the KubeArmorPolicy to the Loop so it is GC'd when the Loop
	// is deleted (and a later same-name Loop doesn't inherit a stale policy).
	if err := controllerutil.SetControllerReference(loop, obj, e.Client.Scheme()); err != nil {
		return fmt.Errorf("set owner ref on KubeArmorPolicy: %w", err)
	}
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(kaptGroupVersion.WithKind(kaptKind))
	err := e.Client.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: obj.GetName()}, existing)
	switch {
	case errors.IsNotFound(err):
		if err := e.Client.Create(ctx, obj); err != nil {
			return fmt.Errorf("create KubeArmorPolicy: %w", err)
		}
	case err != nil:
		return fmt.Errorf("get KubeArmorPolicy: %w", err)
	default:
		obj.SetResourceVersion(existing.GetResourceVersion())
		if err := e.Client.Update(ctx, obj); err != nil {
			return fmt.Errorf("update KubeArmorPolicy: %w", err)
		}
	}
	return nil
}

// Enforcing reports whether the KubeArmor engine is enforcing the Loop's policy.
// The KubeArmorPolicy CRD has no enforcement status, so the evidence comes from
// the engine's telemetry/alert stream (the I32 relay). Until the relay is
// wired this fails closed (not enforcing) — the sandbox is held Suspended.
// P1 (merge): this hard-coded false means no Loop can run; the escape hatch is
// the off-by-default --allow-unenforced manager flag (EnforcementDisabled), and
// real evidence (DaemonSet ready + node BPF-LSM + policy exists) is the fix.
func (e *KubeArmorEnforcer) Enforcing(_ context.Context, _ *v1alpha1.Loop) (bool, string) {
	// TODO(I32): consume the KubeArmor relay alert stream for positive evidence.
	// Until then, fail closed.
	return false, ReasonNodeNotEnforcing
}

// KubeArmorGVK is the GroupVersionKind of a KubeArmorPolicy.
var KubeArmorGVK = schema.GroupVersionKind{Group: kaptGroup, Version: kaptVersion, Kind: kaptKind}

// proxyServiceFQDN returns the per-Loop proxy Service FQDN that the agent
// resolves via DNS to reach the model proxy (D33: <loop>-proxy.<ns>.svc).
func proxyServiceFQDN(loopName, ns string) string {
	return loopName + "-proxy." + ns + ".svc"
}
