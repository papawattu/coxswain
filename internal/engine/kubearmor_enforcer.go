package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/policy"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KubeArmorEnforcer is the production Enforcer: it emits the KubeArmorPolicy for
// the Loop's effective policy (Apply) and reports whether the engine is
// enforcing (Enforcing). It is engine-agnostic at the interface boundary — the
// KubeArmorPolicy is written as an unstructured object so this package does not
// import the KubeArmor Go types.
type KubeArmorEnforcer struct {
	Client client.Client
}

const (
	kaptGroup   = "security.kubearmor.com"
	kaptVersion = "v1"
	kaptKind    = "KubeArmorPolicy"
)

var kaptGroupVersion = schema.GroupVersion{Group: kaptGroup, Version: kaptVersion}

// Apply emits (creates or updates) the KubeArmorPolicy for the Loop's effective
// policy, owned by the Loop.
func (e *KubeArmorEnforcer) Apply(ctx context.Context, loop *v1alpha1.Loop, p policy.EffectivePolicy) error {
	kap := EmitKubeArmorPolicy(loop.Name, loop.Namespace, policy.Translate(p))
	// Marshal the typed KubeArmorPolicy to an unstructured object.
	kapU, err := toUnstructured(kap)
	if err != nil {
		return fmt.Errorf("marshal KubeArmorPolicy: %w", err)
	}
	kapU.SetGroupVersionKind(kaptGroupVersion.WithKind(kaptKind))
	// Create-or-update; the KubeArmorPolicy is owned by the Loop.
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(kaptGroupVersion.WithKind(kaptKind))
	err = e.Client.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: kapU.GetName()}, existing)
	switch {
	case errors.IsNotFound(err):
		if err := e.Client.Create(ctx, kapU); err != nil {
			return fmt.Errorf("create KubeArmorPolicy: %w", err)
		}
	case err != nil:
		return fmt.Errorf("get KubeArmorPolicy: %w", err)
	default:
		kapU.SetResourceVersion(existing.GetResourceVersion())
		if err := e.Client.Update(ctx, kapU); err != nil {
			return fmt.Errorf("update KubeArmorPolicy: %w", err)
		}
	}
	return nil
}

// Enforcing reports whether the KubeArmor engine is enforcing the Loop's policy.
// The KubeArmorPolicy CRD has no enforcement status, so the evidence comes from
// the engine's telemetry/alert stream (the I32 relay); this implementation
// returns NodeNotEnforcing until the relay is wired (a conservative fail-closed
// default — the sandbox is held Suspended until the engine is confirmed).
func (e *KubeArmorEnforcer) Enforcing(_ context.Context, _ *v1alpha1.Loop) (bool, string) {
	// TODO(I32): consume the KubeArmor relay alert stream to return positive
	// enforcement evidence. Until then, fail closed (not enforcing).
	return false, ReasonNodeNotEnforcing
}

// toUnstructured converts a typed struct to an unstructured.Unstructured.
func toUnstructured(obj any) (*unstructured.Unstructured, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: m}, nil
}
