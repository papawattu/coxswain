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

// F42 (review-sandbox-rbac P1-1 + P1-2): the agent NetworkPolicy (ingress
// deny-all, DNS-only egress) is the agent's only fence. It must exist even for
// a Loop with no model endpoint and no network allows, an absent one must hold
// the sandbox Suspended, and a failure to create it must be visible.

import (
	"context"
	"errors"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/policy"
)

// f42Loop is a Loop with a repo and NO spec.agent.endpointSecretRef (the
// shipped sample config/samples/loop-fix-failing-test.yaml shape).
func f42Loop(name string) *coxv1alpha1.Loop {
	return &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: i42cForeignNPNS, UID: "f42-uid"},
		Spec: coxv1alpha1.LoopSpec{
			Goal:      "f42",
			Workspace: coxv1alpha1.Workspace{Repo: "http://gitea.samples.svc:3000/samples/gocli.git"},
		},
	}
}

// Spec 1: a Loop with a repo and no endpoint still gets the agent netpol with
// the ingress deny-all, both policy types, and no model-proxy rule.
func TestF42AgentNetPolExistsWithoutEndpoint(t *testing.T) {
	ctx := context.Background()
	scheme := newI42cForeignNPScheme(t)
	loop := f42Loop("f42-noendpoint")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loop).Build()
	r := &LoopReconciler{CNIProber: testCNIProber(), Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	if err := r.ensureNetPoliciesIfConfigured(ctx, loop); err != nil {
		t.Fatalf("ensureNetPoliciesIfConfigured: %v", err)
	}
	np := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: loop.Name + "-agent-netpol"}, np); err != nil {
		t.Fatalf("a Loop with no endpointSecretRef must still have an agent netpol: %v", err)
	}
	if len(np.Spec.Ingress) != 0 {
		t.Errorf("Ingress = %v, want no ingress rules (deny-all, with Ingress in PolicyTypes)", np.Spec.Ingress)
	}
	if len(np.Spec.PolicyTypes) != 2 ||
		np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress || np.Spec.PolicyTypes[1] != networkingv1.PolicyTypeEgress {
		t.Errorf("PolicyTypes = %v, want [Ingress Egress]", np.Spec.PolicyTypes)
	}
	if n := f42CountComponentPeers(np, policy.ComponentProxyLabel); n != 0 {
		t.Errorf("agent egress has %d model-proxy peer rule(s) without an endpoint, want 0", n)
	}
}

// f42CountComponentPeers counts the agent netpol egress peers whose podSelector
// carries app.kubernetes.io/component=<component> (the model proxy's label;
// proxyLabels has no coxswain.io/loop key).
func f42CountComponentPeers(np *networkingv1.NetworkPolicy, component string) int {
	n := 0
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.PodSelector != nil && peer.PodSelector.MatchLabels[policy.ComponentLabelKey] == component {
				n++
			}
		}
	}
	return n
}

// Positive control for the assertion above: WITH an endpoint the same counter
// finds exactly one model-proxy peer, so a count of 0 is meaningful.
func TestF42ModelProxyRuleExistsWithEndpoint(t *testing.T) {
	ctx := context.Background()
	scheme := newI42cForeignNPScheme(t)
	loop := f42Loop("f42-endpoint")
	loop.Spec.Agent.EndpointSecretRef = "model-creds"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loop).Build()
	r := &LoopReconciler{CNIProber: testCNIProber(), Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}
	if err := r.ensureNetPoliciesIfConfigured(ctx, loop); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	np := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: loop.Name + "-agent-netpol"}, np); err != nil {
		t.Fatalf("get agent netpol: %v", err)
	}
	if n := f42CountComponentPeers(np, policy.ComponentProxyLabel); n != 1 {
		t.Fatalf("model-proxy peers = %d, want 1 with an endpoint", n)
	}
}

// The failure condition is PERSISTED (not just set in memory) by
// persistNetPolFailure, so it survives the error requeue.
func TestF42NetPolFailureConditionIsPersisted(t *testing.T) {
	ctx := context.Background()
	scheme := newI42cForeignNPScheme(t)
	loop := f42Loop("f42-persist")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loop).
		WithStatusSubresource(&coxv1alpha1.Loop{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
					return errors.New("forbidden")
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &LoopReconciler{CNIProber: testCNIProber(), Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	changed, err := r.ensureProxyAndNetPolicies(ctx, loop)
	if err == nil || !changed {
		t.Fatalf("want (changed=true, err), got (%v, %v)", changed, err)
	}
	r.persistNetPolFailure(ctx, loop, changed)

	got := &coxv1alpha1.Loop{}
	if gerr := c.Get(ctx, client.ObjectKeyFromObject(loop), got); gerr != nil {
		t.Fatal(gerr)
	}
	for _, cond := range got.Status.Conditions {
		if cond.Type == agentNetPolReadyCondition && cond.Status == metav1.ConditionFalse {
			return
		}
	}
	t.Fatalf("the stored Loop has no %s=False condition: %v", agentNetPolReadyCondition, got.Status.Conditions)
}

// Spec 2: the absent agent netpol holds the sandbox Suspended; once it exists
// (and is ours) the gate releases.
func TestF42AbsentAgentNetPolHoldsSandboxSuspended(t *testing.T) {
	ctx := context.Background()
	scheme := newI42cForeignNPScheme(t)
	loop := f42Loop("f42-gate")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loop).Build()
	r := &LoopReconciler{CNIProber: testCNIProber(), Client: c, Scheme: scheme, AllowUnenforced: true, Recorder: record.NewFakeRecorder(10)}

	if !r.sandboxSuspendedByGate(ctx, loop) {
		t.Fatal("an absent agent netpol must hold the sandbox Suspended")
	}
	if err := r.ensureNetPoliciesIfConfigured(ctx, loop); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if r.sandboxSuspendedByGate(ctx, loop) {
		t.Fatal("with the agent netpol in place and no other gate, the sandbox must be released")
	}
}

// Spec 3: when the netpol create fails, the sandbox stays Suspended, the error
// is returned, and a condition + Warning Event name it.
func TestF42NetPolCreateFailureIsSurfaced(t *testing.T) {
	ctx := context.Background()
	scheme := newI42cForeignNPScheme(t)
	loop := f42Loop("f42-fail")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loop).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
					return errors.New("forbidden: cannot create networkpolicies")
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	rec := record.NewFakeRecorder(10)
	r := &LoopReconciler{CNIProber: testCNIProber(), Client: c, Scheme: scheme, AllowUnenforced: true, Recorder: rec}

	if err := r.ensureNetPoliciesIfConfigured(ctx, loop); err == nil {
		t.Fatal("the create failure must be returned")
	}
	if !r.sandboxSuspendedByGate(ctx, loop) {
		t.Error("the sandbox must stay Suspended when the agent netpol could not be created")
	}
	var found bool
	for _, cond := range loop.Status.Conditions {
		if cond.Type == agentNetPolReadyCondition && cond.Status == metav1.ConditionFalse &&
			cond.Reason == agentNetPolEnsureFailed {
			found = true
		}
	}
	if !found {
		t.Errorf("want %s=False/%s, got %v", agentNetPolReadyCondition, agentNetPolEnsureFailed, loop.Status.Conditions)
	}
	select {
	case ev := <-rec.Events:
		if ev == "" {
			t.Error("empty event")
		}
	default:
		t.Error("want a Warning Event for the failure")
	}
}
