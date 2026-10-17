// F11 (dogfood P1): the egress proxy Service mutateFn must NOT reset the
// apiserver-assigned ClusterIP on the UPDATE path (the reconcile storm). The
// storm: the operator's mutateFn used to reset Spec.ClusterIP="" on every
// pass; on the update path the existing Service has the assigned ClusterIP, so
// the before/after objects differed and CreateOrUpdate updated the Service on
// EVERY reconcile, firing the Owns(&Service) watch (83 "op:updated" in 2 min
// on the live kind cluster).
//
// These are FAST unit tests (no envtest) that call the mutateFn directly on a
// Service with an apiserver-assigned ClusterIP, so they CATCH the mutation
// (reverting the fix → ClusterIP reset to "" → the spec fails). The test
// norms: every gate spec must FAIL when the gate is disabled.
package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// newF11Scheme builds a scheme with the cox types AND the core types
// (corev1, needed for the Service) registered.
func newF11Scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := coxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme coxv1alpha1: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}
	return scheme
}

// newF11Loop is a minimal Loop for the mutateFn tests.
func newF11Loop(name string) *coxv1alpha1.Loop {
	return &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "f11-ns", UID: "uid-1"},
	}
}

// TestF11MutateEgressProxyServicePreservesAssignedClusterIP: the mutateFn must
// NOT reset the apiserver-assigned ClusterIP on the UPDATE path (the Service
// has a non-empty resourceVersion). Under the F11 revert (always reset
// ClusterIP=""), ClusterIP becomes "" and this spec FAILS.
func TestF11MutateEgressProxyServicePreservesAssignedClusterIP(t *testing.T) {
	scheme := newF11Scheme(t)
	loop := newF11Loop("storm-loop")
	// The existing Service has the apiserver-assigned ClusterIP and the
	// apiserver-defaulted IPFamilies, plus a non-empty resourceVersion (the
	// UPDATE path).
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            egressProxyServiceName("storm-loop"),
			Namespace:       "f11-ns",
			ResourceVersion: "100", // non-empty → the update path
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:  "10.96.100.145",                        // the apiserver-assigned IP
			IPFamilies: []corev1.IPFamily{corev1.IPv4Protocol}, // the apiserver-defaulted value
		},
	}
	if err := mutateEgressProxyService(scheme, loop, svc); err != nil {
		t.Fatalf("mutateEgressProxyService: %v", err)
	}
	if got := svc.Spec.ClusterIP; got != "10.96.100.145" {
		t.Errorf("the mutateFn must NOT reset the apiserver-assigned ClusterIP on the update path; got ClusterIP=%q, want %q (the F11 fix: only reset on the create path)", got, "10.96.100.145")
	}
	// The REAL storm cause: the apiserver-defaulted IPFamilies. The operator must
	// NOT reset it to nil on the update path (the stored Service has
	// ipFamilies=["IPv4"]; resetting it to nil makes the before/after differ and
	// CreateOrUpdate updates the Service on every reconcile — the storm).
	if len(svc.Spec.IPFamilies) != 1 || svc.Spec.IPFamilies[0] != corev1.IPv4Protocol {
		t.Errorf("the mutateFn must NOT reset the apiserver-defaulted IPFamilies on the update path; got IPFamilies=%v, want [IPv4] (the F11 fix: the storm was the IPFamilies reset, not the ClusterIP)", svc.Spec.IPFamilies)
	}
}

// TestF11MutateEgressProxyServiceResetsClusterIPOnCreate: on the CREATE path
// (the Service has no resourceVersion), the mutateFn MUST reset ClusterIP="",
// IPFamilies=nil, and set IPFamilyPolicy=PreferDualStack (so a pre-existing
// headless Service does not leak a defaulted ClusterIP or a headless
// IPFamilyPolicy/IPFamilies into the operator's ClusterIP Service).
func TestF11MutateEgressProxyServiceResetsClusterIPOnCreate(t *testing.T) {
	scheme := newF11Scheme(t)
	loop := newF11Loop("create-loop")
	// The create path: the Service has no resourceVersion, but it carries a
	// leaked headless ClusterIP + IPFamilies that the operator must reset.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      egressProxyServiceName("create-loop"),
			Namespace: "f11-ns",
			// ResourceVersion is empty → the create path
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:  "10.96.0.55",                                                // a leaked headless ClusterIP
			IPFamilies: []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}, // a leaked headless IPFamilies
		},
	}
	if err := mutateEgressProxyService(scheme, loop, svc); err != nil {
		t.Fatalf("mutateEgressProxyService: %v", err)
	}
	if got := svc.Spec.ClusterIP; got != "" {
		t.Errorf("the mutateFn must reset the leaked ClusterIP on the create path; got ClusterIP=%q, want \"\"", got)
	}
	if len(svc.Spec.IPFamilies) != 0 {
		t.Errorf("the mutateFn must reset the leaked IPFamilies to nil on the create path; got %v, want nil", svc.Spec.IPFamilies)
	}
	if svc.Spec.IPFamilyPolicy == nil || *svc.Spec.IPFamilyPolicy != corev1.IPFamilyPolicyPreferDualStack {
		t.Errorf("the mutateFn must set IPFamilyPolicy=PreferDualStack on the create path; got %v", svc.Spec.IPFamilyPolicy)
	}
}

// TestF11MutateModelProxyServicePreservesAssignedClusterIP: the model proxy
// Service mutateFn (D33) must NOT reset the apiserver-assigned ClusterIP on
// the UPDATE path (the same F11 storm). Under the F11 revert, ClusterIP
// becomes "" and this spec FAILS.
func TestF11MutateModelProxyServicePreservesAssignedClusterIP(t *testing.T) {
	scheme := newF11Scheme(t)
	loop := newF11Loop("storm-loop")
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            proxyServiceName("storm-loop"),
			Namespace:       "f11-ns",
			ResourceVersion: "100",
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:  "10.96.100.200",
			IPFamilies: []corev1.IPFamily{corev1.IPv4Protocol}, // the apiserver-defaulted value
		},
	}
	if err := mutateModelProxyService(scheme, loop, svc); err != nil {
		t.Fatalf("mutateModelProxyService: %v", err)
	}
	if got := svc.Spec.ClusterIP; got != "10.96.100.200" {
		t.Errorf("the model proxy mutateFn must NOT reset the apiserver-assigned ClusterIP on the update path; got ClusterIP=%q, want %q (F11)", got, "10.96.100.200")
	}
	if len(svc.Spec.IPFamilies) != 1 || svc.Spec.IPFamilies[0] != corev1.IPv4Protocol {
		t.Errorf("the model proxy mutateFn must NOT reset the apiserver-defaulted IPFamilies on the update path; got %v, want [IPv4] (the F11 storm)", svc.Spec.IPFamilies)
	}
}

// TestF11MutateEgressProxyServiceResetsHeadlessOnAdoption: the D33 headless-
// adoption case. A pre-existing HEADLESS Service (ClusterIP="None", non-empty
// resourceVersion) with the egress-proxy name is adopted by the operator, which
// MUST reset ClusterIP, IPFamilyPolicy, and IPFamilies (the headless Service's
// apiserver-defaulted IPFamilyPolicy may be RequireDualStack, invalid on a
// single-stack cluster, so it must be reset to PreferDualStack on adoption).
// Under the F11 revert (the guard only on ResourceVersion=="", NOT
// ClusterIP=="None"), the headless-adoption path would leave the bad
// IPFamilyPolicy in place and this spec FAILS (IPFamilyPolicy stays nil /
// RequireDualStack, not PreferDualStack).
func TestF11MutateEgressProxyServiceResetsHeadlessOnAdoption(t *testing.T) {
	scheme := newF11Scheme(t)
	loop := newF11Loop("adopt-loop")
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            egressProxyServiceName("adopt-loop"),
			Namespace:       "f11-ns",
			ResourceVersion: "200", // non-empty → NOT the create path
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None", // HEADLESS → the adoption case
			// IPFamilyPolicy is unset (the apiserver defaulted it to
			// RequireDualStack on create, which the operator must reset to
			// PreferDualStack on adoption).
		},
	}
	if err := mutateEgressProxyService(scheme, loop, svc); err != nil {
		t.Fatalf("mutateEgressProxyService: %v", err)
	}
	if got := svc.Spec.ClusterIP; got != "" {
		t.Errorf("the mutateFn must reset the headless ClusterIP to \"\" on adoption; got ClusterIP=%q, want \"\"", got)
	}
	if svc.Spec.IPFamilyPolicy == nil || *svc.Spec.IPFamilyPolicy != corev1.IPFamilyPolicyPreferDualStack {
		t.Errorf("the mutateFn must set IPFamilyPolicy=PreferDualStack on headless adoption; got %v (the headless Service's RequireDualStack default must be reset)", svc.Spec.IPFamilyPolicy)
	}
	if len(svc.Spec.IPFamilies) != 0 {
		t.Errorf("the mutateFn must reset IPFamilies to nil on headless adoption; got %v", svc.Spec.IPFamilies)
	}
}
