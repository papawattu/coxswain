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

// The sandbox is Suspended for the whole Verifying phase.
//
// Found on the homelab (2026-11-27, coxswain v0.3.6, Ceph RBD workspace):
// on the Implementing -> Verifying advance the S4 recycle rebuilt the sandbox
// Running. The runner has nothing to execute at Verifying (it idles: "Verifying
// is operator-owned"), but its pod kept the RWO workspace PVC attached on
// cox2. The verify Job mounts the same PVC (read-only, for import-agent) and
// was scheduled on cox1: "Multi-Attach error ... Volume is already used by
// pod(s) <loop>-sandbox", so it sat in Init until its 900s deadline
// (VerifyInfraRetry). It works on kind/local-path only because a local PV's
// node affinity puts every consumer on the same node.
//
// Suspending the sandbox at Verifying leaves the workspace PVC to the verify
// Job (and to the Judge and deliver Jobs of the P5f holds, which are
// sub-states of Verifying). A verify failure moves the Loop back to
// Implementing, and the S4 recycle rebuilds the sandbox Running.
//
// Mutation (recorded in the PR): drop the Verifying arm from
// loopSandboxStopped and TestSandboxGateSuspendedAtVerifying plus the envtest
// spec fail.

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
)

// TestSandboxGateSuspendedAtVerifying: the suspension gate is the single
// decision point for the sandbox's OperatingMode. Verifying (plain, and the
// P5f Delivering / Judging holds) must be Suspended; the runner's own phases
// stay Running.
func TestSandboxGateSuspendedAtVerifying(t *testing.T) {
	r := &LoopReconciler{CNIProber: testCNIProber()}

	verifying := &coxv1alpha1.Loop{}
	verifying.Status.Phase = coxv1alpha1.LoopPhaseVerifying
	if got := r.sandboxOperatingMode(verifying, false); got != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Fatalf("sandboxOperatingMode(Verifying) = %s, want Suspended (the idle sandbox pod holds the RWO workspace PVC the verify Job mounts)", got)
	}

	for _, reason := range []string{coxv1alpha1.ReasonVerifyingDelivering, coxv1alpha1.ReasonVerifyingJudging} {
		held := &coxv1alpha1.Loop{}
		held.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		held.Status.Conditions = []metav1.Condition{{
			Type: string(coxv1alpha1.LoopPhaseVerifying), Status: metav1.ConditionTrue, Reason: reason,
		}}
		if got := r.sandboxOperatingMode(held, false); got != sandboxv1beta1.SandboxOperatingModeSuspended {
			t.Fatalf("sandboxOperatingMode(Verifying/%s) = %s, want Suspended", reason, got)
		}
	}

	// Negative control: the phases the runner executes stay Running.
	for _, phase := range []coxv1alpha1.LoopPhase{coxv1alpha1.LoopPhasePlanning, coxv1alpha1.LoopPhaseImplementing} {
		l := &coxv1alpha1.Loop{}
		l.Status.Phase = phase
		if got := r.sandboxOperatingMode(l, false); got != sandboxv1beta1.SandboxOperatingModeRunning {
			t.Fatalf("sandboxOperatingMode(%s) = %s, want Running (the runner executes this phase)", phase, got)
		}
	}
}

var _ = Describe("The sandbox is Suspended at Verifying (the verify Job owns the RWO workspace PVC)", func() {
	ctx := context.Background()

	It("suspends the sandbox on the Verifying advance, keeps it Suspended, and runs it again back at Implementing", func() {
		ns := "vsusp-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer deleteNS(ctx, ns)

		r := &LoopReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			AllowUnenforced: true,
			CNIProber:       cni.NewFakeProber(),
			Recorder:        record.NewFakeRecorder(64),
			readPhaseClaim:  func(context.Context, *coxv1alpha1.Loop) (*PhaseClaim, error) { return nil, nil },
		}
		r.CNIProber.(*cni.FakeProber).SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced})

		name := "vsusp"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "the sandbox is Suspended at Verifying",
				Workspace: coxv1alpha1.Workspace{Repo: testRepoURL},
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: p2fModelSecret,
					ModelEndpoint:     p2fModelEndpoint,
				},
			},
		})).To(Succeed())

		getLoop := func() *coxv1alpha1.Loop {
			l := &coxv1alpha1.Loop{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, l)).To(Succeed())
			return l
		}
		reconcileOnce := func() {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
			Expect(err).NotTo(HaveOccurred())
		}
		setPhase := func(phase coxv1alpha1.LoopPhase) {
			l := getLoop()
			l.Status.Phase = phase
			l.Status.DesiredPhase = phase
			Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
		}
		// sandboxMode returns the Sandbox's OperatingMode, or "" when the S4
		// recycle has deleted it and the next pass has not rebuilt it yet.
		sandboxMode := func() string {
			sb := &sandboxv1beta1.Sandbox{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: sandboxName(name)}, sb); err != nil {
				if apierrors.IsNotFound(err) {
					return ""
				}
				Expect(err).NotTo(HaveOccurred())
			}
			return string(sb.Spec.OperatingMode)
		}
		// reconcileUntilBuilt runs passes until the Sandbox exists again (the
		// recycle deletes it on a phase advance; a later pass rebuilds it).
		reconcileUntilBuilt := func() string {
			for range 4 {
				reconcileOnce()
				if m := sandboxMode(); m != "" {
					return m
				}
			}
			return sandboxMode()
		}

		// The D35a proxy gate's peer: the model secret plus a Ready proxy pod.
		_ = k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: p2fModelSecret, Namespace: ns},
			StringData: map[string]string{modelAPIKey: "vsusp-dummy", modelBaseURL: p2fModelEndpoint},
		})
		Expect(r.ensureProxy(ctx, getLoop())).To(Succeed())
		proxy := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: proxyPodName(name), Namespace: ns}, proxy)).To(Succeed())
		proxy.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()}}
		Expect(k8sClient.Status().Update(ctx, proxy)).To(Succeed())

		reconcileOnce()
		Expect(sandboxMode()).To(Equal(string(sandboxv1beta1.SandboxOperatingModeRunning)),
			"baseline: the sandbox runs at Planning")

		By("the Loop advances to Verifying: the sandbox is not Running")
		setPhase(coxv1alpha1.LoopPhaseImplementing)
		Expect(reconcileUntilBuilt()).To(Equal(string(sandboxv1beta1.SandboxOperatingModeRunning)),
			"baseline: the sandbox runs at Implementing")
		setPhase(coxv1alpha1.LoopPhaseVerifying)
		Expect(reconcileUntilBuilt()).To(Equal(string(sandboxv1beta1.SandboxOperatingModeSuspended)),
			"at Verifying the sandbox must be Suspended: a Running sandbox pod holds the RWO workspace PVC the verify Job mounts (Multi-Attach)")

		By("a re-reconcile keeps it Suspended")
		reconcileOnce()
		Expect(sandboxMode()).NotTo(Equal(string(sandboxv1beta1.SandboxOperatingModeRunning)))

		By("a verify failure sends the Loop back to Implementing: the sandbox runs again")
		setPhase(coxv1alpha1.LoopPhaseImplementing)
		Expect(reconcileUntilBuilt()).To(Equal(string(sandboxv1beta1.SandboxOperatingModeRunning)),
			"back at Implementing the runner has work again, so the sandbox must be Running")
	})
})
