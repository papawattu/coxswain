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

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
)

// F51 P2-3 (pi3 review #11): a nil CNIProber used to leave the D38 gate OPEN
// ("CNI probe not configured; gate not active"), while D30 fails closed for a
// missing Enforcer. "No prober wired" is no evidence at all, so it must hold
// the sandbox Suspended — with the condition and Event that say why — and the
// only way to run without evidence is the --allow-unenforced-network escape
// hatch. These specs pin both halves.
var _ = Describe("F51 P2-3: no CNI prober wired fails closed", func() {
	var (
		ctx context.Context
		ns  string
	)

	newReconciler := func(rec *record.FakeRecorder, allowNetwork bool) *LoopReconciler {
		return &LoopReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			apiReader: k8sClient,
			// D30 satisfied, so the only possible hold under test is D38.
			AllowUnenforced:        true,
			AllowUnenforcedNetwork: allowNetwork,
			Recorder:               rec,
			// CNIProber deliberately nil: this is the seam under test.
		}
	}

	buildLoop := func() *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "l1", Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "g",
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
			},
		}
	}

	reconcileLoop := func(r *LoopReconciler) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "l1"}})
		Expect(err).NotTo(HaveOccurred(), "the gate waits, it never errors or fails the Loop")
	}

	getLoop := func() *coxv1alpha1.Loop {
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "l1"}, got)).To(Succeed())
		return got
	}

	sandboxMode := func() sandboxv1beta1.SandboxOperatingMode {
		sbx := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "l1-sandbox"}, sbx)).To(Succeed(),
			"the operator must have created the sandbox")
		return sbx.Spec.OperatingMode
	}

	BeforeEach(func() {
		ctx = context.Background()
		ns = "f51-nilprober-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
	})

	It("holds the sandbox Suspended with NetworkEnforced=False/ProbeUnavailable, and emits the Event", func() {
		rec := record.NewFakeRecorder(8)
		r := newReconciler(rec, false)
		Expect(k8sClient.Create(ctx, buildLoop())).To(Succeed())

		reconcileLoop(r)
		reconcileLoop(r) // I43: the state must hold across a second reconcile

		Expect(sandboxMode()).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"a nil CNIProber is no evidence: the sandbox must be held Suspended (fail-closed)")

		cond := networkEnforcedCondition(getLoop().Status.Conditions)
		Expect(cond).NotTo(BeNil(), "the NetworkEnforced condition must record the degraded gate")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(string(cni.ReasonProbeUnavailable)))
		Expect(cond.Message).To(ContainSubstring("no CNI prober wired"))

		Expect(getLoop().Status.Phase).NotTo(Equal(coxv1alpha1.LoopPhaseFailed), "the gate never fails the Loop")

		// I90: the degraded path is surfaced, not silent.
		var events []string
		for {
			select {
			case e := <-rec.Events:
				events = append(events, e)
			default:
				Expect(events).NotTo(BeEmpty(), "a nil prober must emit a NetworkEnforced Event")
				Expect(strings.Join(events, "\n")).To(ContainSubstring("reason=" + string(cni.ReasonProbeUnavailable)))
				return
			}
		}
	})

	It("runs with the --allow-unenforced-network escape hatch, recording EnforcementDisabled", func() {
		rec := record.NewFakeRecorder(8)
		r := newReconciler(rec, true)
		Expect(k8sClient.Create(ctx, buildLoop())).To(Succeed())

		reconcileLoop(r)
		reconcileLoop(r)

		Expect(sandboxMode()).To(Equal(sandboxv1beta1.SandboxOperatingModeRunning),
			"--allow-unenforced-network is the only way to run without network evidence")

		cond := networkEnforcedCondition(getLoop().Status.Conditions)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(string(cni.ReasonEnforcementDisabled)),
			"the condition records that the fence was NOT measured, rather than claiming it passed")
	})
})
