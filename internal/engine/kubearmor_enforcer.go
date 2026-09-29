package engine

import (
	"context"
	"fmt"

	"github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/policy"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// ErrForeignKapt is the sentinel createOrUpdateKapt returns when a
// KubeArmorPolicy of the desired name exists and is NOT controlled by the
// Loop (I42f review P2, round 1: the same never-take-over rule as I42c's
// errForeignNetpol, applied to the three KubeArmorPolicy names — the agent's
// and the two proxies'). The foreign object is left untouched; the controller
// maps the sentinel to a KubeArmorPolicyConflict condition and holds the
// sandbox Suspended.
var ErrForeignKapt = fmt.Errorf("KubeArmorPolicy is controlled by another controller (foreign)")

// KubeArmorEnforcer is the production Enforcer: it emits the KubeArmorPolicy for
// the Loop's effective policy (Apply) and reports whether the engine is
// enforcing (Enforcing). It writes the KubeArmorPolicy as an unstructured
// object (no KubeArmor Go type dependency) and owner-refs it to the Loop (P2).
type KubeArmorEnforcer struct {
	Client client.Client
	// proxyFQDN / egressProxyFQDN are the per-Loop proxy Service FQDNs the
	// agent's DNS allowlist carries (built by the controller's reconciler from
	// proxyServiceName / egressProxyServiceName + the cluster domain; R16 I44
	// item 1: the enforcer must not build them from a `-proxy` / `-egress-proxy`
	// literal, and R16 I44 item 2: it must not hard-code the cluster domain).
	// When nil (tests) the defaults below are used.
	proxyFQDN       func(loopName, ns string) string
	egressProxyFQDN func(loopName, ns string) string
	// clusterDomain is the cluster's service DNS domain (default cluster.local)
	// used by the model-proxy policy's bare-host FQDN expansion (R16 I44 item 2).
	clusterDomain string
}

// defaultClusterDomain is the default cluster service DNS domain (R16 I44 item 2).
const defaultClusterDomain = "cluster.local"

// Apply emits (creates or updates) the KubeArmorPolicy for the Loop's effective
// policy, owned by the Loop, PLUS (I42f, D35 part 2) the proxy policies: the
// model proxy's KubeArmorPolicy when a model endpoint is configured (the proxy
// pod is expected) and the egress proxy's KubeArmorPolicy when the effective
// policy has network allows (the egress proxy pod is expected). Each is
// owner-ref'd to the Loop (GC'd with it). The proxy policies are the inner
// fence: the process block allows only the proxy's own binary and the network
// block carries the exact egress the proxy is supposed to have, so a bug in
// the proxy's application-level enforcement is still denied.
func (e *KubeArmorEnforcer) Apply(ctx context.Context, loop *v1alpha1.Loop, p policy.EffectivePolicy) error {
	// I42d: the egress proxy FQDN goes in the agent's DNS allowlist only when
	// the effective policy has network allows — that is exactly when the egress
	// proxy exists and the agent's *_PROXY env points at it. With no allows the
	// agent has no external egress and the name stays out of the allowlist.
	// The FQDNs are passed in by the controller (R16 I44 item 1): the enforcer
	// must not build them from a `-proxy` / `-egress-proxy` literal.
	proxyF := e.proxyFQDN
	if proxyF == nil {
		proxyF = defaultProxyFQDN
	}
	egressF := e.egressProxyFQDN
	if egressF == nil {
		egressF = defaultEgressProxyFQDN
	}
	egressFQDN := ""
	if len(p.Network) > 0 {
		egressFQDN = egressF(loop.Name, loop.Namespace)
	}
	obj := EmitKubeArmorPolicy(loop.Name, loop.Namespace, policy.Translate(p, proxyF(loop.Name, loop.Namespace), egressFQDN))
	if err := e.createOrUpdateKapt(ctx, loop, obj); err != nil {
		return err
	}

	// I42f: the model proxy policy (created only when the model proxy is
	// expected, i.e. a model endpoint is configured). Cleaned up in the else
	// branch when the endpoint is removed (same drift rationale as the egress
	// proxy policy below).
	if loop.Spec.Agent.EndpointSecretRef != "" {
		modelObj := EmitModelProxyKubeArmorPolicy(loop.Name, loop.Namespace, loop.Spec.Agent.ModelEndpoint, e.clusterDomainDefaulted())
		if err := e.createOrUpdateKapt(ctx, loop, modelObj); err != nil {
			return err
		}
	} else if err := e.cleanupModelProxyKapt(ctx, loop); err != nil {
		return err
	}

	// I42f: the egress proxy policy (created only when the egress proxy is
	// expected, i.e. the effective policy has network allows). Cleaned up in
	// the else branch when the allows go away (a stale policy would keep
	// fencing a pod that no longer exists — drift the reconciler must own).
	if len(p.Network) > 0 {
		egressObj := EmitEgressProxyKubeArmorPolicy(loop.Name, loop.Namespace, p.Network)
		return e.createOrUpdateKapt(ctx, loop, egressObj)
	}
	return e.cleanupEgressProxyKapt(ctx, loop)
}

// cleanupEgressProxyKapt deletes the egress proxy KubeArmorPolicy when the
// egress proxy is no longer expected (the network allows went away). A
// FOREIGN policy occupying the name is left alone (I2 never-take-over).
func (e *KubeArmorEnforcer) cleanupEgressProxyKapt(ctx context.Context, loop *v1alpha1.Loop) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(kaptGroupVersion.WithKind(kaptKind))
	err := e.Client.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: "coxswain-" + loop.Name + "-egress-proxy"}, obj)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get egress proxy KubeArmorPolicy: %w", err)
	}
	if !metav1.IsControlledBy(obj, loop) {
		return nil
	}
	if err := e.Client.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete egress proxy KubeArmorPolicy: %w", err)
	}
	return nil
}

// cleanupModelProxyKapt deletes the model proxy KubeArmorPolicy when the model
// proxy is no longer expected (the model endpoint was removed). A FOREIGN
// policy occupying the name is left alone (I2 never-take-over).
func (e *KubeArmorEnforcer) cleanupModelProxyKapt(ctx context.Context, loop *v1alpha1.Loop) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(kaptGroupVersion.WithKind(kaptKind))
	err := e.Client.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: "coxswain-" + loop.Name + "-proxy"}, obj)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get model proxy KubeArmorPolicy: %w", err)
	}
	if !metav1.IsControlledBy(obj, loop) {
		return nil
	}
	if err := e.Client.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete model proxy KubeArmorPolicy: %w", err)
	}
	return nil
}

// createOrUpdateKapt creates or updates a KubeArmorPolicy object (idempotent
// Apply: create when absent, update with the live resourceVersion when
// present). It owner-refs the policy to the Loop so it is GC'd with the Loop
// (P2: and a later same-name Loop does not inherit a stale policy).
//
// I42f review P2 (round 1): a FOREIGN KubeArmorPolicy of the same name is
// never overwritten (the same never-take-over rule as I42c's
// createOrUpdateNP, applied to the three KubeArmorPolicy names the Enforcer
// emits). When the live object exists and is not controlled by the Loop,
// ErrForeignKapt is returned and the object is left untouched; the controller
// maps the sentinel to a KubeArmorPolicyConflict condition and holds the
// sandbox Suspended (fail-closed: a proxy's inner fence must not be someone
// else's policy).
func (e *KubeArmorEnforcer) createOrUpdateKapt(ctx context.Context, loop *v1alpha1.Loop, obj *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(kaptGroupVersion.WithKind(kaptKind))
	err := e.Client.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: obj.GetName()}, existing)
	switch {
	case errors.IsNotFound(err):
		if err := controllerutil.SetControllerReference(loop, obj, e.Client.Scheme()); err != nil {
			return fmt.Errorf("set owner ref on KubeArmorPolicy %s: %w", obj.GetName(), err)
		}
		if err := e.Client.Create(ctx, obj); err != nil {
			return fmt.Errorf("create KubeArmorPolicy %s: %w", obj.GetName(), err)
		}
	case err != nil:
		return fmt.Errorf("get KubeArmorPolicy %s: %w", obj.GetName(), err)
	default:
		// A foreign policy occupying the name is left untouched; the caller
		// maps the sentinel to the KubeArmorPolicyConflict condition.
		if !metav1.IsControlledBy(existing, loop) {
			return fmt.Errorf("KubeArmorPolicy %s/%s: %w", loop.Namespace, obj.GetName(), ErrForeignKapt)
		}
		if err := controllerutil.SetControllerReference(loop, obj, e.Client.Scheme()); err != nil {
			return fmt.Errorf("set owner ref on KubeArmorPolicy %s: %w", obj.GetName(), err)
		}
		obj.SetResourceVersion(existing.GetResourceVersion())
		if err := e.Client.Update(ctx, obj); err != nil {
			return fmt.Errorf("update KubeArmorPolicy %s: %w", obj.GetName(), err)
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

// defaultProxyFQDN / defaultEgressProxyFQDN are the per-Loop proxy Service FQDNs
// the agent's DNS allowlist carries, used when the KubeArmorEnforcer's proxyFQDN
// / egressProxyFQDN fields are unset (tests). The production controller wires
// its own (built from proxyServiceName / egressProxyServiceName + ClusterDomain
// via the reconciler's clusterDomain(); R16 I44 item 1+2). The naming here
// matches the controller's proxyServiceName / egressProxyServiceName so a
// rename cannot desync the allowlist from the URLs the agent dials.
func defaultProxyFQDN(loopName, ns string) string {
	return loopName + "-proxy." + ns + ".svc"
}

func defaultEgressProxyFQDN(loopName, ns string) string {
	return loopName + "-egress-proxy." + ns + ".svc"
}

// clusterDomainDefaulted returns the enforcer's cluster domain (default
// cluster.local when unset, R16 I44 item 2).
func (e *KubeArmorEnforcer) clusterDomainDefaulted() string {
	if e.clusterDomain != "" {
		return e.clusterDomain
	}
	return defaultClusterDomain
}

// SetProxyFQDNs wires the reconciler's proxy Service FQDN naming + cluster
// domain into the enforcer (R16 I44 item 1+2: the enforcer must not build the
// FQDNs from a `-proxy` / `-egress-proxy` literal or hard-code the domain).
// Idempotent (re-asserts the same values each reconcile); the controller calls
// it before Enforcer.Apply. When the functions are nil the enforcer falls back
// to its defaults (tests).
func (e *KubeArmorEnforcer) SetProxyFQDNs(proxy, egress func(loopName, ns string) string, clusterDomain string) {
	e.proxyFQDN = proxy
	e.egressProxyFQDN = egress
	e.clusterDomain = clusterDomain
}
