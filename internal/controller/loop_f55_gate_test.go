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

// F55 (post-merge review of F42): (1) the sandbox gate waits for the egress
// proxy when the workspace-init clone must traverse it, not only when the
// policy has network allows; (2) the early netpol ensure + gate do not read a
// stale cache, and an upgrade of a Loop with a running sandbox emits an Event.

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

func f55Scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := newI42cForeignNPScheme(t)
	if err := sandboxv1beta1.AddToScheme(s); err != nil {
		t.Fatalf("add sandbox scheme: %v", err)
	}
	return s
}

// f55ExternalRepoLoop is the shipped github sample shape: an external repo, no
// endpoint, no network allows.
func f55ExternalRepoLoop(name string) *coxv1alpha1.Loop {
	l := f42Loop(name)
	l.Spec.Workspace.Repo = "https://github.com/example/repo.git"
	return l
}

func TestF55ExternalRepoNoAllowsWaitsForEgressProxy(t *testing.T) {
	ctx := context.Background()
	scheme := f55Scheme(t)
	loop := f55ExternalRepoLoop("f55-gate")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loop).Build()
	r := &LoopReconciler{CNIProber: testCNIProber(), Client: c, Scheme: scheme, AllowUnenforced: true, Recorder: record.NewFakeRecorder(10)}
	if err := r.ensureNetPoliciesIfConfigured(ctx, loop); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if needsEgressProxy(ctx, r, loop) {
		t.Fatal("precondition: this shape has no allows, so needsEgressProxy must be false")
	}
	if !r.workspaceInitNeedsProxy(loop) {
		t.Fatal("precondition: the init clone of an external repo goes through the proxy")
	}
	if !r.sandboxSuspendedByGate(ctx, loop) {
		t.Fatal("with no egress proxy pod, the sandbox must stay Suspended (the clone would get connection refused)")
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: egressProxyPodName(loop.Name), Namespace: loop.Namespace}}
	if err := controllerutil.SetControllerReference(loop, pod, scheme); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if !r.sandboxSuspendedByGate(ctx, loop) {
		t.Fatal("an egress proxy pod that is not Ready must still hold the sandbox")
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(ctx, pod); err != nil {
		// the fake client may not treat Pod status as a subresource
		if err2 := c.Update(ctx, pod); err2 != nil {
			t.Fatal(err2)
		}
	}
	if r.sandboxSuspendedByGate(ctx, loop) {
		t.Fatal("with the egress proxy Ready the sandbox must be released")
	}
}

// F55 review F1: a repo URL with an EMPTY host (https://:8080/repo.git passes
// admission) has workspaceInitNeedsProxy true but no host for ensureEgressProxy
// to allow, so no proxy is ever created. The gate must not wait for it (it
// would hold Suspended forever with no condition). Gate = creation predicate.
func TestF55EmptyHostRepoIsNotHeldForAProxyThatIsNeverCreated(t *testing.T) {
	ctx := context.Background()
	scheme := f55Scheme(t)
	loop := f42Loop("f55-emptyhost")
	loop.Spec.Workspace.Repo = "https://:8080/repo.git"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loop).Build()
	r := &LoopReconciler{CNIProber: testCNIProber(), Client: c, Scheme: scheme, AllowUnenforced: true, Recorder: record.NewFakeRecorder(10)}
	if err := r.ensureNetPoliciesIfConfigured(ctx, loop); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !r.workspaceInitNeedsProxy(loop) || r.workspaceInitProxyCreated(loop) {
		t.Fatalf("precondition: needs=%v created=%v, want needs=true created=false (the degenerate shape)",
			r.workspaceInitNeedsProxy(loop), r.workspaceInitProxyCreated(loop))
	}
	if r.sandboxSuspendedByGate(ctx, loop) {
		t.Fatal("no egress proxy is created for an empty host, so the gate must not hold the sandbox for one")
	}
}

// Positive control: an in-cluster repo needs no proxy, so no proxy pod is no hold.
func TestF55InClusterRepoDoesNotWaitForEgressProxy(t *testing.T) {
	ctx := context.Background()
	scheme := f55Scheme(t)
	loop := f42Loop("f55-incluster")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loop).Build()
	r := &LoopReconciler{CNIProber: testCNIProber(), Client: c, Scheme: scheme, AllowUnenforced: true, Recorder: record.NewFakeRecorder(10)}
	if err := r.ensureNetPoliciesIfConfigured(ctx, loop); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if r.sandboxSuspendedByGate(ctx, loop) {
		t.Fatal("an in-cluster repo with no allows must not wait for an egress proxy")
	}
}

// f55StaleNetPolClient returns NotFound for NetworkPolicy reads, as an informer
// cache that has not yet seen a just-created object does. The apiReader (a
// direct client over the same store) sees the truth, so a gate that trusts the
// cache alone is the mutation that fails.
func f55StaleNetPolClient(t *testing.T, loop *coxv1alpha1.Loop, scheme *runtime.Scheme) (client.Client, client.Client) {
	t.Helper()
	direct := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loop).Build()
	stale := interceptor.NewClient(direct, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
				return errors.NewNotFound(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, key.Name)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	return stale, direct
}

func TestF55GateDoesNotTrustAStaleCacheForTheAgentNetPol(t *testing.T) {
	ctx := context.Background()
	scheme := f55Scheme(t)
	loop := f42Loop("f55-stale")
	stale, direct := f55StaleNetPolClient(t, loop, scheme)
	r := &LoopReconciler{CNIProber: testCNIProber(), Client: stale, apiReader: direct, Scheme: scheme, AllowUnenforced: true, Recorder: record.NewFakeRecorder(10)}
	// The netpol exists in the store (just created) but the cache says NotFound.
	if err := (&LoopReconciler{CNIProber: testCNIProber(), Client: direct, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}).ensureNetPoliciesIfConfigured(ctx, loop); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if agentNetPolMissing(ctx, r, loop) {
		t.Fatal("a stale cached NotFound must be confirmed against the apiReader: the netpol exists")
	}
	// Positive control: without the apiReader the same stale client reports missing.
	r2 := &LoopReconciler{CNIProber: testCNIProber(), Client: stale, Scheme: scheme}
	if !agentNetPolMissing(ctx, r2, loop) {
		t.Fatal("control: the stale client alone must read as missing, or this spec proves nothing")
	}
	// And a netpol that is truly absent is still missing through the apiReader.
	loop2 := f42Loop("f55-absent")
	stale2, direct2 := f55StaleNetPolClient(t, loop2, scheme)
	r3 := &LoopReconciler{CNIProber: testCNIProber(), Client: stale2, apiReader: direct2, Scheme: scheme}
	if !agentNetPolMissing(ctx, r3, loop2) {
		t.Fatal("a truly absent netpol must still hold the gate")
	}
}

func TestF55UpgradeEmitsFencedEventForExistingSandbox(t *testing.T) {
	ctx := context.Background()
	scheme := f55Scheme(t)
	loop := f42Loop("f55-event")
	sb := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: sandboxName(loop.Name), Namespace: loop.Namespace}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loop, sb).Build()
	rec := record.NewFakeRecorder(10)
	r := &LoopReconciler{CNIProber: testCNIProber(), Client: c, apiReader: c, Scheme: scheme, Recorder: rec}

	r.ensureAgentNetPolBeforeSandbox(ctx, loop)
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "Normal AgentEgressFenced") {
			t.Fatalf("event = %q, want Normal AgentEgressFenced", ev)
		}
	default:
		t.Fatal("want one Normal Event when the netpol is first created for a Loop with a sandbox")
	}
	// One-shot: the second reconcile finds the netpol and is silent.
	r.ensureAgentNetPolBeforeSandbox(ctx, loop)
	select {
	case ev := <-rec.Events:
		t.Fatalf("second ensure emitted %q, want none", ev)
	default:
	}
}

// Positive control for the one-shot: a fresh Loop (no sandbox yet) gets the
// netpol with no Event.
func TestF55FreshLoopNoFencedEvent(t *testing.T) {
	ctx := context.Background()
	scheme := f55Scheme(t)
	loop := f42Loop("f55-fresh")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loop).Build()
	rec := record.NewFakeRecorder(10)
	r := &LoopReconciler{CNIProber: testCNIProber(), Client: c, apiReader: c, Scheme: scheme, Recorder: rec}
	r.ensureAgentNetPolBeforeSandbox(ctx, loop)
	select {
	case ev := <-rec.Events:
		t.Fatalf("a fresh Loop emitted %q, want none", ev)
	default:
	}
}
