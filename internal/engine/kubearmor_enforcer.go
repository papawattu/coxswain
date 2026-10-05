package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/policy"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

// ErrNoProxyFQDNs is returned by Apply when the enforcer's proxy FQDN
// functions were not wired at construction (R16 I44 item 1: the enforcer must
// not build them from a `-proxy` / `-egress-proxy` literal, so a missing wiring
// is a configuration error, not a silent fallback).
var ErrNoProxyFQDNs = fmt.Errorf("KubeArmorEnforcer: proxy FQDN functions are not configured (wire them at construction, R16 I44 item 1)")

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
	// They are REQUIRED: Apply returns an error when either is nil. Wired ONCE
	// at construction (cmd/main.go and the envtest suites), never per reconcile.
	ProxyFQDN       func(loopName, ns string) string
	EgressProxyFQDN func(loopName, ns string) string
	// ToolProxyFQDN is the per-Loop tool proxy Service FQDN (D41d, ADR-0008):
	// built by the controller from toolProxyServiceName + the cluster domain.
	// The FQDN (not FQDN:port) is what KubeArmor matchDNSQueries compares
	// against. REQUIRED: Apply returns an error when it is nil (same rule as
	// ProxyFQDN/EgressProxyFQDN).
	ToolProxyFQDN func(loopName, ns, toolName string) string
	// ClusterDomain is the cluster's service DNS domain (default
	// policy.DefaultClusterDomain when empty) used by the model-proxy policy's
	// bare-host FQDN expansion (R16 I44 item 2). Set at construction.
	ClusterDomain string
	// AllowUnenforced mirrors the manager's --allow-unenforced flag (the D30
	// dev escape hatch). When true, an ABSENT KubeArmor CRD is tolerated (the
	// no-match is a no-op): the D38 enforcing-CNI kind cluster deliberately
	// runs no KubeArmor (ADR-0007 F2) and the CNI polices the egress. When
	// false (production), a missing KubeArmor CRD is a loud error, not a
	// silent no-op — a misinstall must not silently disable the inner fence.
	AllowUnenforced bool
}

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
	// The FQDNs are wired at construction by the controller (R16 I44 item 1:
	// the enforcer must not build them from a `-proxy` / `-egress-proxy`
	// literal). They are required — a missing wiring is a configuration error.
	if e.ProxyFQDN == nil || e.EgressProxyFQDN == nil || e.ToolProxyFQDN == nil {
		return ErrNoProxyFQDNs
	}
	egressFQDN := ""
	if len(p.Network) > 0 {
		egressFQDN = e.EgressProxyFQDN(loop.Name, loop.Namespace)
	}
	obj := EmitKubeArmorPolicy(loop.Name, loop.Namespace, policy.Translate(p, e.ProxyFQDN(loop.Name, loop.Namespace), egressFQDN))
	if err := e.createOrUpdateKapt(ctx, loop, obj); err != nil {
		return err
	}

	// I42f: the model proxy policy (created only when the model proxy is
	// expected, i.e. a model endpoint is configured). Cleaned up in the else
	// branch when the endpoint is removed (same drift rationale as the egress
	// proxy policy below).
	if loop.Spec.Agent.EndpointSecretRef != "" {
		modelObj := EmitModelProxyKubeArmorPolicy(loop.Name, loop.Namespace, loop.Spec.Agent.ModelEndpoint, e.clusterDomain())
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
		if err := e.createOrUpdateKapt(ctx, loop, egressObj); err != nil {
			return err
		}
	} else if err := e.cleanupEgressProxyKapt(ctx, loop); err != nil {
		return err
	}

	// D41d: the tool proxy policies (one per tool in the effective policy).
	// Created when the tool is in the union, cleaned up when it is not (the
	// egress proxy policy's drift rationale: a stale policy would keep fencing
	// a pod that no longer exists).
	expected := make(map[string]bool, len(p.Tools))
	for _, t := range p.Tools {
		expected[t.Name] = true
		toolObj := EmitToolProxyKubeArmorPolicy(loop.Name, loop.Namespace, t.Name, t.Upstream)
		if err := e.createOrUpdateKapt(ctx, loop, toolObj); err != nil {
			return err
		}
	}
	return e.cleanupStaleToolKapt(ctx, loop, expected)
}

// cleanupStaleToolKapt deletes the tool proxy KubeArmorPolicies for each tool
// name the enforcer previously owned that is NO LONGER in the effective union
// (D41d cleanup; the egress proxy policy's drift rationale). A FOREIGN policy
// occupying the name is left alone (I2 never-take-over).
func (e *KubeArmorEnforcer) cleanupStaleToolKapt(ctx context.Context, loop *v1alpha1.Loop, expected map[string]bool) error {
	for name := range e.listToolKaptNames(ctx, loop) {
		// The lister returns the full policy names; the tool name is the part
		// after "coxswain-<loop>-tool-" (the expected map is keyed by tool
		// name).
		toolName := strings.TrimPrefix(name, "coxswain-"+loop.Name+"-tool-")
		if expected[toolName] {
			continue
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(kaptGroupVersion.WithKind(kaptKind))
		if err := e.Client.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: name}, obj); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			if meta.IsNoMatchError(err) && e.AllowUnenforced {
				continue
			}
			return fmt.Errorf("get tool proxy KubeArmorPolicy %s: %w", name, err)
		}
		if !metav1.IsControlledBy(obj, loop) {
			// Foreign object: leave it alone (I2 never-take-over).
			continue
		}
		if err := e.Client.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete tool proxy KubeArmorPolicy %s: %w", name, err)
		}
	}
	return nil
}

// listToolKaptNames returns the names of the tool proxy KubeArmorPolicies the
// enforcer sees for the Loop (discovered by the tool proxy's DISJOINT label
// set on the policy's spec.selector.matchLabels — the emitter stamps them,
// mirroring the pod/Service label set). A List error (e.g. a missing scoped
// cache, the podBlindClient envtest behaviour) leaves the set empty (nothing
// cleaned up this pass) — never a reconcile error.
func (e *KubeArmorEnforcer) listToolKaptNames(ctx context.Context, loop *v1alpha1.Loop) map[string]struct{} {
	names := make(map[string]struct{})
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(kaptGroupVersion.WithKind("KubeArmorPolicyList"))
	if err := e.Client.List(ctx, list, client.InNamespace(loop.Namespace)); err != nil {
		return names
	}
	for i := range list.Items {
		item := &list.Items[i]
		sel, _, _ := unstructured.NestedMap(item.Object, KaptSpecKey, KaptSelectorKey)
		raw, _ := sel[KaptMatchLabelsKey].(map[string]any)
		if raw[policy.ComponentLabelKey] != policy.ComponentToolProxyLabel || raw["coxswain.io/tool-proxy-for"] != loop.Name {
			continue
		}
		if tool, ok := raw["coxswain.io/tool"].(string); ok && tool != "" {
			names[item.GetName()] = struct{}{}
		}
	}
	return names
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
	// KubeArmor CRD absent: tolerated only under the --allow-unenforced dev
	// escape hatch (D38). In production a missing KubeArmor CRD is a loud
	// error, not a silent no-op (reviewer P1 on b25f77e).
	if meta.IsNoMatchError(err) && e.AllowUnenforced {
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
	// KubeArmor CRD absent: tolerated only under --allow-unenforced (D38);
	// a loud error in production (reviewer P1 on b25f77e).
	if meta.IsNoMatchError(err) && e.AllowUnenforced {
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
			// KubeArmor CRD absent: a no-op only under --allow-unenforced
			// (D38); a loud create error in production (reviewer P1 on
			// b25f77e).
			if meta.IsNoMatchError(err) && e.AllowUnenforced {
				return nil
			}
			return fmt.Errorf("create KubeArmorPolicy %s: %w", obj.GetName(), err)
		}
	case meta.IsNoMatchError(err):
		// KubeArmor CRD absent: no-op only under --allow-unenforced (D38);
		// in production the missing CRD is a loud error so a misinstall does
		// not silently disable the inner fence (reviewer P1 on b25f77e).
		if e.AllowUnenforced {
			return nil
		}
		return fmt.Errorf("get KubeArmorPolicy %s: KubeArmor CRD not installed (and --allow-unenforced is not set): %w", obj.GetName(), err)
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
// the engine's telemetry/alert stream (the I32 relay). Until the relay is wired
// (I32) the operator has OBSERVED nothing: this returns (false,
// ReasonEnforcementUnverified) so the PolicyEnforced condition is
// Unknown/EnforcementUnverified — it names the missing probe rather than
// claiming the engine is not enforcing (I46: the condition must describe the
// cluster, not the flag). The gate is still fail-closed: without
// --allow-unenforced the sandbox is held Suspended on the false result; the
// flag only lets it run while the condition says so.
func (e *KubeArmorEnforcer) Enforcing(_ context.Context, _ *v1alpha1.Loop) (bool, string) {
	// TODO(I32): consume the KubeArmor relay alert stream for positive evidence.
	// Until then, report that no enforcement probe exists (I46).
	return false, ReasonEnforcementUnverified
}

// KubeArmorGVK is the GroupVersionKind of a KubeArmorPolicy.
var KubeArmorGVK = schema.GroupVersionKind{Group: kaptGroup, Version: kaptVersion, Kind: kaptKind}

// clusterDomain returns the enforcer's cluster domain, defaulting to
// policy.DefaultClusterDomain when unset (R16 I44 item 2: one source of truth
// for the default).
func (e *KubeArmorEnforcer) clusterDomain() string {
	if e.ClusterDomain != "" {
		return e.ClusterDomain
	}
	return policy.DefaultClusterDomain
}
