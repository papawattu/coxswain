// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

// I42c (round 3): foreignNetPols must fail CLOSED on a real read error. It is
// the order-independent sandbox gate in ensureSandbox: it reads the live
// per-Loop NetworkPolicy objects and returns true (hold Suspended) when any of
// them exists and is NOT controlled by the Loop. A real read error (not
// NotFound) must ALSO return true — if it returned false, a transient API
// error would let the sandbox go Running while its netpol is unknown, which is
// the fail-open the gate exists to prevent (consistent with
// needsEgressProxy). This unit test (fake client + interceptor, not envtest)
// pins that contract.

import (
	"context"
	"errors"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

const i42cForeignNPNS = "default"

// newI42cForeignNPLoop builds a minimal Loop for the foreignNetPols unit
// tests.
func newI42cForeignNPLoop(name string) *coxv1alpha1.Loop {
	return &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: i42cForeignNPNS},
		Spec:       coxv1alpha1.LoopSpec{Goal: "i42c foreignnetpols test"},
	}
}

// newI42cForeignNPScheme builds a scheme with the core + coxswain types.
func newI42cForeignNPScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	if err := coxv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add coxswain scheme: %v", err)
	}
	return s
}

// TestForeignNetPolsFailsClosedOnGetError pins the fail-closed contract: when
// a per-Loop NetworkPolicy Get returns a real error (not NotFound),
// foreignNetPols must return true so the sandbox gate holds it Suspended.
func TestForeignNetPolsFailsClosedOnGetError(t *testing.T) {
	ctx := context.Background()
	const loopName = "fnp-loop"
	scheme := newI42cForeignNPScheme(t)
	loop := newI42cForeignNPLoop(loopName)

	errBoom := errors.New("transient api error")
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(loop).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, c client.WithWatch, _ client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
					return errBoom
				}
				return c.Get(ctx, client.ObjectKey{}, obj, opts...)
			},
		}).
		Build()

	r := &LoopReconciler{Client: fakeClient, Scheme: scheme}

	if got := foreignNetPols(ctx, r, loop); !got {
		t.Fatalf("foreignNetPols = false on NetworkPolicy Get error; want true (fail-closed): a real read error must hold the sandbox Suspended, not let it run with an unknown netpol")
	}
}

// TestForeignNetPolsAbsentIsNotConflict: an absent (NotFound) per-Loop
// NetworkPolicy is NOT a conflict — ensureNetworkPolicy creates it, so the
// gate must not hold the sandbox Suspended for a name that simply doesn't
// exist yet.
func TestForeignNetPolsAbsentIsNotConflict(t *testing.T) {
	ctx := context.Background()
	const loopName = "fnp-absent"
	scheme := newI42cForeignNPScheme(t)
	loop := newI42cForeignNPLoop(loopName)

	// No NetworkPolicies seeded: every Get returns NotFound.
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(loop).
		Build()

	r := &LoopReconciler{Client: fakeClient, Scheme: scheme}

	if got := foreignNetPols(ctx, r, loop); got {
		t.Fatalf("foreignNetPols = true when all per-Loop netpols are absent; want false (absent is not a conflict — ensureNetworkPolicy creates them)")
	}
}

// TestForeignNetPolsOwnedIsNotConflict: a per-Loop NetworkPolicy that IS
// controlled by the Loop is not foreign.
func TestForeignNetPolsOwnedIsNotConflict(t *testing.T) {
	ctx := context.Background()
	const loopName = "fnp-owned"
	scheme := newI42cForeignNPScheme(t)
	loop := newI42cForeignNPLoop(loopName)

	ownedNP := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: loopName + "-agent-netpol", Namespace: i42cForeignNPNS},
		Spec:       networkingv1.NetworkPolicySpec{},
	}
	// Set the Loop as the controller ref (the real path uses
	// controllerutil.SetControllerReference, which needs a scheme; here we set
	// it directly to avoid pulling in the reconciler's scheme setup).
	loopUID := types.UID("fnp-owned-uid")
	loop.UID = loopUID
	ownedNP.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "coxswain.wattu.com/v1alpha1",
		Kind:       "Loop",
		Name:       loopName,
		UID:        loopUID,
		Controller: ptrToBool(),
	}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(loop, ownedNP).
		Build()

	r := &LoopReconciler{Client: fakeClient, Scheme: scheme}

	if got := foreignNetPols(ctx, r, loop); got {
		t.Fatalf("foreignNetPols = true for a netpol controlled by the Loop; want false")
	}
}

// TestForeignNetPolsForeignIsConflict: a per-Loop NetworkPolicy that is NOT
// controlled by the Loop is foreign.
func TestForeignNetPolsForeignIsConflict(t *testing.T) {
	ctx := context.Background()
	const loopName = "fnp-foreign"
	scheme := newI42cForeignNPScheme(t)
	loop := newI42cForeignNPLoop(loopName)

	foreignNP := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: loopName + "-agent-netpol", Namespace: i42cForeignNPNS},
		Spec:       networkingv1.NetworkPolicySpec{},
	}
	// No owner refs (unowned) — the "foreign" case.

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(loop, foreignNP).
		Build()

	r := &LoopReconciler{Client: fakeClient, Scheme: scheme}

	if got := foreignNetPols(ctx, r, loop); !got {
		t.Fatalf("foreignNetPols = false for an unowned netpol occupying the name; want true")
	}
}

func ptrToBool() *bool {
	b := true
	return &b
}
