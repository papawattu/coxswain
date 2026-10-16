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
func newF11Loop(name, ns string) *coxv1alpha1.Loop {
	return &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: "uid-1"},
	}
}

// TestF11MutateEgressProxyServicePreservesAssignedClusterIP: the mutateFn must
// NOT reset the apiserver-assigned ClusterIP on the UPDATE path (the Service
// has a non-empty resourceVersion). Under the F11 revert (always reset
// ClusterIP=""), ClusterIP becomes "" and this spec FAILS.
func TestF11MutateEgressProxyServicePreservesAssignedClusterIP(t *testing.T) {
	scheme := newF11Scheme(t)
	loop := newF11Loop("storm-loop", "f11-ns")
	// The existing Service has the apiserver-assigned ClusterIP and a
	// non-empty resourceVersion (the UPDATE path).
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            egressProxyServiceName("storm-loop"),
			Namespace:       "f11-ns",
			ResourceVersion: "100", // non-empty → the update path
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "10.96.100.145", // the apiserver-assigned IP
		},
	}
	if err := mutateEgressProxyService(scheme, loop, svc); err != nil {
		t.Fatalf("mutateEgressProxyService: %v", err)
	}
	if got := svc.Spec.ClusterIP; got != "10.96.100.145" {
		t.Errorf("the mutateFn must NOT reset the apiserver-assigned ClusterIP on the update path; got ClusterIP=%q, want %q (the F11 fix: only reset on the create path)", got, "10.96.100.145")
	}
}

// TestF11MutateEgressProxyServiceResetsClusterIPOnCreate: on the CREATE path
// (the Service has no resourceVersion), the mutateFn MUST reset ClusterIP=""
// (so a pre-existing headless Service does not leak a defaulted ClusterIP
// into the operator's ClusterIP Service).
func TestF11MutateEgressProxyServiceResetsClusterIPOnCreate(t *testing.T) {
	scheme := newF11Scheme(t)
	loop := newF11Loop("create-loop", "f11-ns")
	// The create path: the Service has no resourceVersion, but it carries a
	// leaked headless ClusterIP that the operator must reset.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      egressProxyServiceName("create-loop"),
			Namespace: "f11-ns",
			// ResourceVersion is empty → the create path
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "10.96.0.55", // a leaked headless ClusterIP
		},
	}
	if err := mutateEgressProxyService(scheme, loop, svc); err != nil {
		t.Fatalf("mutateEgressProxyService: %v", err)
	}
	if got := svc.Spec.ClusterIP; got != "" {
		t.Errorf("the mutateFn must reset the leaked ClusterIP on the create path; got ClusterIP=%q, want \"\"", got)
	}
}

// TestF11MutateModelProxyServicePreservesAssignedClusterIP: the model proxy
// Service mutateFn (D33) must NOT reset the apiserver-assigned ClusterIP on
// the UPDATE path (the same F11 storm). Under the F11 revert, ClusterIP
// becomes "" and this spec FAILS.
func TestF11MutateModelProxyServicePreservesAssignedClusterIP(t *testing.T) {
	scheme := newF11Scheme(t)
	loop := newF11Loop("storm-loop", "f11-ns")
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            proxyServiceName("storm-loop"),
			Namespace:       "f11-ns",
			ResourceVersion: "100",
		},
		Spec: corev1.ServiceSpec{ClusterIP: "10.96.100.200"},
	}
	if err := mutateModelProxyService(scheme, loop, svc); err != nil {
		t.Fatalf("mutateModelProxyService: %v", err)
	}
	if got := svc.Spec.ClusterIP; got != "10.96.100.200" {
		t.Errorf("the model proxy mutateFn must NOT reset the apiserver-assigned ClusterIP on the update path; got ClusterIP=%q, want %q (F11)", got, "10.96.100.200")
	}
}
