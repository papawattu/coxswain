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

// F70 (post-merge review of F55): the F55 gate waits for the egress proxy when
// the workspace-init clone must traverse it, but a COMPLETED FORK never runs
// that clone (its workspace comes from the source's snapshot and already
// carries the init marker, so the script exits before cloning). On an install
// where the proxy cannot come up the fork was held Suspended for ever with no
// condition naming it.

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// f70ForkLoop is the shipped github sample shape (an external repo — so the
// workspace-init clone goes through the egress proxy — with no network allows
// and no model endpoint), forked from a source Loop's checkpoint.
func f70ForkLoop(name string) *coxv1alpha1.Loop {
	l := f55ExternalRepoLoop(name)
	l.Spec.ForkOf = &coxv1alpha1.ForkOf{Loop: "src", Iteration: 0}
	return l
}

// f70MarkForkRestored writes the Restore condition the operator writes when a
// fork's workspace PVC has been created from the snapshot: True/Restored with
// the target-keyed message (the F35 marker restoreCompletedForSpec reads).
func f70MarkForkRestored(loop *coxv1alpha1.Loop) {
	setRestoreCondition(loop, true, coxv1alpha1.RestoreReasonRestored, restoredMessage(loop, &b2RestoreTarget{
		snapshot:  checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: loop.Spec.ForkOf.Loop}}, loop.Spec.ForkOf.Iteration),
		kind:      b2KindFork,
		iteration: loop.Spec.ForkOf.Iteration,
	}))
}

// f70Suspended is the gate call the reconcile makes, with the same reconciler
// shape the F55 gate specs use (AllowUnenforced so the D30 gate does not
// decide for us).
func f70Suspended(t *testing.T, loop *coxv1alpha1.Loop, objs ...client.Object) bool {
	t.Helper()
	ctx := context.Background()
	scheme := f55Scheme(t)
	all := append([]client.Object{loop.DeepCopy()}, objs...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(all...).Build()
	r := &LoopReconciler{CNIProber: testCNIProber(), Client: c, Scheme: scheme, AllowUnenforced: true, Recorder: record.NewFakeRecorder(10)}
	if err := r.ensureNetPoliciesIfConfigured(ctx, loop); err != nil {
		t.Fatalf("ensureNetPoliciesIfConfigured: %v", err)
	}
	return r.sandboxSuspendedByGate(ctx, loop)
}

// f70ReadyEgressProxy is a Ready, Loop-owned egress proxy pod.
func f70ReadyEgressProxy(t *testing.T, loop *coxv1alpha1.Loop) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: egressProxyPodName(loop.Name), Namespace: loop.Namespace}}
	if err := controllerutil.SetControllerReference(loop, pod, f55Scheme(t)); err != nil {
		t.Fatalf("set owner on the egress proxy pod: %v", err)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	return pod
}

// The item: a fork whose restore completed for the CURRENT spec target is not
// held for a workspace-init clone that will never run. Without the fix the
// gate holds it (no egress proxy pod exists in this install).
func TestF70CompletedForkIsNotHeldForACloneItWillNotRun(t *testing.T) {
	loop := f70ForkLoop("f70-fork")
	f70MarkForkRestored(loop)
	if !r70WorkspaceInitNeedsProxy(t, loop) {
		t.Fatal("precondition: the init clone of an external repo would go through the egress proxy")
	}
	if f70Suspended(t, loop) {
		t.Fatal("a completed fork must not be held Suspended for an egress proxy the workspace-init clone does not use (it exits on the init marker before cloning)")
	}
}

// Control: the SAME shape without a completed restore IS held until the proxy
// is Ready — so the fix cannot pass by deleting the workspace-init term (the
// clone really does traverse the proxy for a fresh Loop).
func TestF70FreshLoopIsStillHeldUntilTheEgressProxyIsReady(t *testing.T) {
	loop := f70ForkLoop("f70-fresh")
	loop.Spec.ForkOf = nil // the same external repo, no restore
	if !r70WorkspaceInitNeedsProxy(t, loop) {
		t.Fatal("precondition: the init clone of an external repo goes through the egress proxy")
	}
	if !f70Suspended(t, loop) {
		t.Fatal("a fresh external-repo Loop must stay Suspended until its egress proxy is Ready (the first clone would get connection refused)")
	}
	if f70Suspended(t, loop, f70ReadyEgressProxy(t, loop)) {
		t.Fatal("with the egress proxy Ready the fresh Loop must be released")
	}
}

// The term is the F35 TARGET-keyed marker, not "ForkOf is set": a fork whose
// restore has not completed for the current target is still held, and a
// completed restore for a DIFFERENT target does not count.
func TestF70ForkWhoseRestoreIsNotCompleteForTheTargetIsStillHeld(t *testing.T) {
	inherited := f70ForkLoop("f70-fork-pending")
	f70MarkForkRestored(inherited)
	if restoreCompletedForSpec(inherited) != true {
		t.Fatal("fixture: the marker must match the fork target")
	}
	// Same Loop, different target: a spec edit a client could make by
	// bypassing the CRD rule (forkOf is immutable) — the marker no longer
	// covers it, so the clone is expected to run and the gate holds.
	inherited.Spec.ForkOf = &coxv1alpha1.ForkOf{Loop: "src", Iteration: 1}
	if restoreCompletedForSpec(inherited) {
		t.Fatal("fixture: the marker must NOT cover a different fork target")
	}
	if !f70Suspended(t, inherited) {
		t.Fatal("a fork whose restore did not complete for its current target must still wait for the egress proxy")
	}
}

// r70WorkspaceInitNeedsProxy is the fixture precondition, kept separate so a
// failure reads as "the fixture stopped modelling the shape".
func r70WorkspaceInitNeedsProxy(t *testing.T, loop *coxv1alpha1.Loop) bool {
	t.Helper()
	scheme := f55Scheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loop.DeepCopy()).Build()
	r := &LoopReconciler{CNIProber: testCNIProber(), Client: c, Scheme: scheme, AllowUnenforced: true, Recorder: record.NewFakeRecorder(10)}
	return r.workspaceInitNeedsProxy(loop) && r.workspaceInitProxyCreated(loop)
}
