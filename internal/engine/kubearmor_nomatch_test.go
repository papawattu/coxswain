package engine

import (
	"context"
	"strings"
	"testing"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/policy"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	// noKaptTestNS / noKaptTestLoopName: the test Loop's namespace and name
	// (kept as constants so the goconst linter doesn't flag the repeated
	// literal across the fixture, the interceptor, and the fixture-honesty pin).
	noKaptTestNS       = "ns-x"
	noKaptTestLoopName = "loop-x"
)

// newNoKaptClient builds a fake client whose scheme has the coxswain + core
// types (so the Loop's owner-reference resolves) but whose interceptor makes
// every Get/Create/Update/Delete of a KubeArmorPolicy GVK object return a
// NoKindMatchError. This is the unit-level stand-in for "the KubeArmor CRDs
// are not installed" (the D38 enforcing-CNI kind cluster deliberately runs no
// KubeArmor, ADR-0007 F2): the real apiserver answers a Get/Create/Update for
// a GVK whose CRD is absent with a NoKindMatchError, and this interceptor
// reproduces exactly that on the fake client without a running apiserver.
//
// The GVK is detected from the object's set GVK (the enforcer sets
// KubeArmorGVK on every unstructured it touches), not by name, so it catches
// the agent, model-proxy, and egress-proxy KubeArmorPolicies alike.
func newNoKaptClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := coxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add coxswain scheme: %v", err)
	}
	// NOTE: no KubeArmor type is registered — the CRD is absent.
	noKindMatch := &meta.NoKindMatchError{GroupKind: KubeArmorGVK.GroupKind(), SearchedVersions: []string{KubeArmorGVK.Version}}
	// apierrors.NewNotFound-like but for a missing CRD; NoKindMatchError is
	// what the real apiserver returns when the CRD is absent.
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if isKaptObj(obj) {
					return noKindMatch
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if isKaptObj(obj) {
					return noKindMatch
				}
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if isKaptObj(obj) {
					return noKindMatch
				}
				return c.Update(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if isKaptObj(obj) {
					return noKindMatch
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()
}

// isKaptObj reports whether the object the client is about to touch is a
// KubeArmorPolicy (detected by its GVK, which the enforcer sets on every
// unstructured it builds). The enforcer always uses *unstructured.Unstructured
// for Kapt objects, so a type + GVK check is exact.
func isKaptObj(obj client.Object) bool {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return false
	}
	return u.GroupVersionKind() == KubeArmorGVK
}

func noKaptTestLoop() *coxv1alpha1.Loop {
	return &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: noKaptTestLoopName, Namespace: noKaptTestNS},
		Spec: coxv1alpha1.LoopSpec{
			Agent: coxv1alpha1.AgentConfig{
				Image:             "busybox:1.36",
				Model:             "local-model",
				EndpointSecretRef: "model-secret",
				ModelEndpoint:     "model.test:8443",
			},
		},
	}
}

func noKaptTestPolicy() policy.EffectivePolicy {
	return policy.EffectivePolicy{
		Exec:    []string{"git"},
		Network: []string{"proxy.golang.org:443"},
		Files:   []string{"/data"},
	}
}

// pinNoMatchError keeps the fixture honest: it pins that the fake client
// really does return a NoKindMatchError for a KubeArmorPolicy GVK. If the
// interceptor behaviour ever changes, the gated-tolerance tests below would
// pass or fail for the wrong reason; this test fails loudly in that case.
func TestPinNoKaptFixtureYieldsNoMatchError(t *testing.T) {
	c := newNoKaptClient(t)
	obj := newKaptUnstructured()
	err := c.Get(context.Background(), client.ObjectKey{Namespace: noKaptTestNS, Name: "coxswain-" + noKaptTestLoopName}, obj)
	if err == nil {
		t.Fatal("expected a Get error for a KubeArmorPolicy object, got nil")
	}
	if !meta.IsNoMatchError(err) {
		t.Fatalf("expected a NoKindMatchError (missing-CRD stand-in), got: %v (type %T)", err, err)
	}
}

// The NoMatch tolerance must be gated on AllowUnenforced (reviewer P1 on
// b25f77e: an unconditional no-op would turn a production misinstall into a
// silent fence removal). When AllowUnenforced is FALSE and the KubeArmor CRD
// is absent, Apply must return a loud error. When TRUE, it must be a no-op.
func TestApplyNoKaptCRDGatedOnAllowUnenforced(t *testing.T) {
	fqdn := func(loopName, ns string) string { return "coxswain-" + loopName + "-proxy." + ns + ".svc" }
	egressFqdn := func(loopName, ns string) string { return "coxswain-" + loopName + "-egress-proxy." + ns + ".svc" }
	toolFqdn := func(loopName, ns, toolName string) string {
		return "coxswain-" + loopName + "-tool-" + toolName + "." + ns + ".svc"
	}

	t.Run("AllowUnenforced=false: missing KubeArmor CRD is a loud error", func(t *testing.T) {
		e := &KubeArmorEnforcer{
			Client:          newNoKaptClient(t),
			ProxyFQDN:       fqdn,
			EgressProxyFQDN: egressFqdn,
			ToolProxyFQDN:   toolFqdn,
			AllowUnenforced: false,
		}
		loop := noKaptTestLoop()
		err := e.Apply(context.Background(), loop, noKaptTestPolicy())
		if err == nil {
			t.Fatal("Apply must ERROR when the KubeArmor CRD is absent and --allow-unenforced is not set (a misinstall must not silently disable the inner fence)")
		}
		if !strings.Contains(err.Error(), "KubeArmorPolicy") {
			t.Fatalf("error should name the KubeArmorPolicy, got: %v", err)
		}
	})

	t.Run("AllowUnenforced=true: missing KubeArmor CRD is a no-op", func(t *testing.T) {
		e := &KubeArmorEnforcer{
			Client:          newNoKaptClient(t),
			ProxyFQDN:       fqdn,
			EgressProxyFQDN: egressFqdn,
			ToolProxyFQDN:   toolFqdn,
			AllowUnenforced: true,
		}
		loop := noKaptTestLoop()
		if err := e.Apply(context.Background(), loop, noKaptTestPolicy()); err != nil {
			t.Fatalf("Apply must be a NO-OP (nil) when the KubeArmor CRD is absent and --allow-unenforced is set, got: %v", err)
		}
	})
}

// The cleanup path (cleanupEgressProxyKapt / cleanupModelProxyKapt) must be
// gated the same way: no-op under --allow-unenforced, loud error otherwise.
func TestCleanupNoKaptCRDGatedOnAllowUnenforced(t *testing.T) {
	t.Run("AllowUnenforced=false: missing KubeArmor CRD is a loud error", func(t *testing.T) {
		e := &KubeArmorEnforcer{Client: newNoKaptClient(t), AllowUnenforced: false}
		loop := noKaptTestLoop()
		if err := e.cleanupEgressProxyKapt(context.Background(), loop); err == nil {
			t.Fatal("cleanupEgressProxyKapt must ERROR when the KubeArmor CRD is absent and --allow-unenforced is not set")
		}
	})
	t.Run("AllowUnenforced=true: missing KubeArmor CRD is a no-op", func(t *testing.T) {
		e := &KubeArmorEnforcer{Client: newNoKaptClient(t), AllowUnenforced: true}
		loop := noKaptTestLoop()
		if err := e.cleanupEgressProxyKapt(context.Background(), loop); err != nil {
			t.Fatalf("cleanupEgressProxyKapt must be a NO-OP (nil) when the KubeArmor CRD is absent and --allow-unenforced is set, got: %v", err)
		}
	})
}

// newKaptUnstructured builds an unstructured with the KubeArmorPolicy GVK set
// (as the enforcer does before every client call).
func newKaptUnstructured() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(KubeArmorGVK)
	return obj
}
