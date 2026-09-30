package controller

import (
	"context"
	"testing"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestForeignKaptPoliciesGatedOnAllowUnenforced is the I43 companion to
// internal/engine/kubearmor_nomatch_test.go for the controller-side gate:
// with the KubeArmor CRD absent, foreignKaptPolicies must return true (hold
// the sandbox Suspended) unless AllowUnenforced is set.
func TestForeignKaptPoliciesGatedOnAllowUnenforced(t *testing.T) {
	noKindMatch := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "security.kubearmor.com", Kind: "KubeArmorPolicy"}, SearchedVersions: []string{"v1"}}
	c := fake.NewClientBuilder().
		WithScheme(func() *runtime.Scheme {
			s := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(s)
			_ = coxv1alpha1.AddToScheme(s)
			return s
		}()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				u, ok := obj.(*unstructured.Unstructured)
				if ok && u.GetAPIVersion() == "security.kubearmor.com/v1" {
					return noKindMatch
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "loop-x", Namespace: "ns-x"},
	}
	t.Run("AllowUnenforced=false: missing KubeArmor CRD fails closed (hold Suspended)", func(t *testing.T) {
		r := &LoopReconciler{Client: c, AllowUnenforced: false}
		if !foreignKaptPolicies(context.Background(), r, loop) {
			t.Fatal("foreignKaptPolicies must return true (hold Suspended) when the KubeArmor CRD is absent and --allow-unenforced is not set")
		}
	})
	t.Run("AllowUnenforced=true: missing KubeArmor CRD is not a conflict", func(t *testing.T) {
		r := &LoopReconciler{Client: c, AllowUnenforced: true}
		if foreignKaptPolicies(context.Background(), r, loop) {
			t.Fatal("foreignKaptPolicies must return false when the KubeArmor CRD is absent and --allow-unenforced is set")
		}
	})
}
