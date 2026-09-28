// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use the file except in compliance with the License
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

// I42b (P2 review): needsEgressProxy must fail CLOSED on a policy read
// error. The sandbox gate uses its result as "require the egress proxy to be
// Ready"; if a transient Get error made it return false, the gate would be
// skipped and the sandbox could be set Running without the egress proxy.
// This unit test (fake client + interceptor, not envtest) pins the
// fail-closed contract: a Get error on a referenced AgentPolicy makes
// needsEgressProxy return true.

import (
	"context"
	"errors"
	"testing"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newI42bFakeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	if err := coxv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add coxswain scheme: %v", err)
	}
	if err := sandboxv1beta1.AddToScheme(s); err != nil {
		t.Fatalf("add sandbox scheme: %v", err)
	}
	return s
}

const i42bGateNS = "default"

func newI42bFakeLoop(name string, policyName string) *coxv1alpha1.Loop {
	const ns = i42bGateNS
	return &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: coxv1alpha1.LoopSpec{
			Goal:       "i42b gate test",
			Workspace:  coxv1alpha1.Workspace{Repo: i42bTestRepo, Ref: loopRef},
			PolicyRefs: []string{policyName},
			Agent:      coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
		},
	}
}

func newI42bFakeAgentPolicy(name, ns string, network []string) *coxv1alpha1.AgentPolicy {
	return &coxv1alpha1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       coxv1alpha1.AgentPolicySpec{Network: network},
	}
}

// TestNeedsEgressProxyFailsClosed pins the P2 contract: when the effective
// policy cannot be read (a transient Get error on a referenced AgentPolicy),
// needsEgressProxy must return true so the sandbox gate requires the egress
// proxy (fail-closed), not skip it (fail-open).
func TestNeedsEgressProxyFailsClosed(t *testing.T) {
	ctx := context.Background()
	const loopName = "gate-loop"
	const policyName = "gate-pol"

	scheme := newI42bFakeScheme(t)
	loop := newI42bFakeLoop(loopName, policyName)
	// The AgentPolicy exists and HAS network allows, but the interceptor makes
	// every Get on an AgentPolicy fail, simulating a transient API error.
	ap := newI42bFakeAgentPolicy(policyName, i42bGateNS, []string{i42eExternalHost})
	errBoom := errors.New("transient api error")

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(loop, ap).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*coxv1alpha1.AgentPolicy); ok {
					return errBoom
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	r := &LoopReconciler{Client: fakeClient, Scheme: scheme}

	if got := needsEgressProxy(ctx, r, loop); !got {
		t.Fatalf("needsEgressProxy = false on AgentPolicy Get error; want true (fail-closed): a read error must require the egress proxy, not skip the gate")
	}
}

// TestNeedsEgressProxyHappyPath covers the two non-error cases: a policy with
// network allows (true) and a policy without (false).
func TestNeedsEgressProxyHappyPath(t *testing.T) {
	ctx := context.Background()
	const withName = "gate-with"
	const withoutName = "gate-without"
	const polWith = "pol-with"
	const polWithout = "pol-without"

	scheme := newI42bFakeScheme(t)
	withLoop := newI42bFakeLoop(withName, polWith)
	withoutLoop := newI42bFakeLoop(withoutName, polWithout)
	apWith := newI42bFakeAgentPolicy(polWith, i42bGateNS, []string{i42eExternalHost})
	apWithout := newI42bFakeAgentPolicy(polWithout, i42bGateNS, nil)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(withLoop, withoutLoop, apWith, apWithout).
		Build()

	r := &LoopReconciler{Client: fakeClient, Scheme: scheme}

	if got := needsEgressProxy(ctx, r, withLoop); !got {
		t.Errorf("needsEgressProxy = false for a policy with network allows; want true")
	}
	if got := needsEgressProxy(ctx, r, withoutLoop); got {
		t.Errorf("needsEgressProxy = true for a policy with no network allows; want false")
	}
}

// TestNeedsEgressProxyMissingPolicy: a referenced AgentPolicy that does not
// exist is a Get error (NotFound) and must also fail closed (true).
func TestNeedsEgressProxyMissingPolicy(t *testing.T) {
	ctx := context.Background()
	const loopName = "gate-missing"
	const policyName = "missing-pol"

	scheme := newI42bFakeScheme(t)
	loop := newI42bFakeLoop(loopName, policyName)
	// Note: no AgentPolicy is seeded.

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(loop).
		Build()

	r := &LoopReconciler{Client: fakeClient, Scheme: scheme}

	if got := needsEgressProxy(ctx, r, loop); !got {
		t.Fatalf("needsEgressProxy = false for a missing referenced AgentPolicy; want true (fail-closed)")
	}
}
