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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// D38: the operator-side CNI self-test gate. The CNIProber seam (internal/cni)
// reports whether the cluster's CNI polices pod -> host-network egress; the
// gate holds every Loop Suspended (fail-closed) when it does not — or when the
// probe can't run — unless --allow-unenforced-network is set. The NetworkEnforced
// condition records the result on every Loop.

var _ = Describe("D38 CNI self-test gate (D38s1)", func() {
	var (
		ctx context.Context
		r   *LoopReconciler
	)

	newReconciler := func(prober *cni.FakeProber, allowNetwork bool) *LoopReconciler {
		return &LoopReconciler{
			Client:                 k8sClient,
			Scheme:                 k8sClient.Scheme(),
			CNIProber:              prober,
			AllowUnenforced:        true, // let the eBPF gate pass so only the D38 gate is under test
			AllowUnenforcedNetwork: allowNetwork,
		}
	}

	buildLoop := func(ns string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "l1", Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "g",
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
			},
		}
	}
	getLoop := func(ns, name string) *coxv1alpha1.Loop {
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, got)).To(Succeed())
		return got
	}
	getSandboxMode := func(ns, name string) sandboxv1beta1.SandboxOperatingMode {
		sbx := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, sbx)).To(Succeed())
		return sbx.Spec.OperatingMode
	}
	reconcileLoop := func(ns, name string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred()) // the gate waits, never errors / never fails the Loop
	}
	reconcileTwice := func(ns, name string) {
		reconcileLoop(ns, name)
		reconcileLoop(ns, name) // I43: a second reconcile of the same Loop must keep the state
	}

	It("holds the sandbox Suspended + NetworkEnforced=False (Unknown) before the first probe result (fail-closed from startup)", func() {
		ctx = context.Background()
		prober := cni.NewFakeProber() // default result: Unknown
		r = newReconciler(prober, false)

		ns := "d38s1-unknown"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		Expect(k8sClient.Create(ctx, buildLoop(ns))).To(Succeed())

		reconcileLoop(ns, "l1")

		Eventually(func() bool {
			return getSandboxMode(ns, "l1-sandbox") == sandboxv1beta1.SandboxOperatingModeSuspended
		}, "10s").Should(BeTrue(), "Unknown (no probe result yet) must hold the sandbox Suspended (fail-closed)")

		cond := networkEnforcedCondition(getLoop(ns, "l1").Status.Conditions)
		Expect(cond).NotTo(BeNil(), "the NetworkEnforced condition must exist")
		Expect(cond.Reason).To(Equal("Unknown"))
		Expect(getLoop(ns, "l1").Status.Phase).NotTo(Equal(coxv1alpha1.LoopPhaseFailed), "the Loop is never failed by the gate")
	})

	It("runs the sandbox + NetworkEnforced=True (CNIEnforced) when the probe passes", func() {
		ctx = context.Background()
		prober := cni.NewFakeProber()
		prober.SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced})
		r = newReconciler(prober, false)

		ns := "d38s1-enforced"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		Expect(k8sClient.Create(ctx, buildLoop(ns))).To(Succeed())

		reconcileTwice(ns, "l1") // I43 same-Loop: the state must hold across a second reconcile

		Eventually(func() bool {
			return getSandboxMode(ns, "l1-sandbox") == sandboxv1beta1.SandboxOperatingModeRunning
		}, "10s").Should(BeTrue(), "an enforcing CNI must let the sandbox run")

		cond := networkEnforcedCondition(getLoop(ns, "l1").Status.Conditions)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("CNIEnforced"))
	})

	It("holds the sandbox Suspended + NetworkEnforced=False (CNIUnenforced) when the probe sees a REACHABLE target", func() {
		ctx = context.Background()
		prober := cni.NewFakeProber()
		prober.SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIUnenforced, Detail: "APISERVER_SVC"})
		r = newReconciler(prober, false)

		ns := "d38s1-unenforced"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		Expect(k8sClient.Create(ctx, buildLoop(ns))).To(Succeed())

		reconcileLoop(ns, "l1")

		Eventually(func() bool {
			return getSandboxMode(ns, "l1-sandbox") == sandboxv1beta1.SandboxOperatingModeSuspended
		}, "10s").Should(BeTrue(), "a non-enforcing CNI must hold the sandbox Suspended")

		cond := networkEnforcedCondition(getLoop(ns, "l1").Status.Conditions)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("CNIUnenforced"))
	})

	It("holds the sandbox Suspended (fail-closed) when the probe is unavailable — NOT Failed, NOT Running", func() {
		ctx = context.Background()
		prober := cni.NewFakeProber()
		prober.SetResult(cni.CNIProbeResult{Reason: cni.ReasonProbeUnavailable, Detail: "pod not Ready in timeout"})
		r = newReconciler(prober, false)

		ns := "d38s1-unavailable"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		Expect(k8sClient.Create(ctx, buildLoop(ns))).To(Succeed())

		reconcileLoop(ns, "l1")

		Eventually(func() bool {
			return getSandboxMode(ns, "l1-sandbox") == sandboxv1beta1.SandboxOperatingModeSuspended
		}, "10s").Should(BeTrue(), "ProbeUnavailable must hold the sandbox Suspended (fail-closed)")

		loop := getLoop(ns, "l1")
		cond := networkEnforcedCondition(loop.Status.Conditions)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("ProbeUnavailable"))
		Expect(loop.Status.Phase).NotTo(Equal(coxv1alpha1.LoopPhaseFailed), "an unavailable probe must never fail the Loop")
	})

	It("runs with NetworkEnforced=False (EnforcementDisabled) when --allow-unenforced-network is set", func() {
		ctx = context.Background()
		prober := cni.NewFakeProber()
		prober.SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIUnenforced, Detail: "EXTERNAL"})
		r = newReconciler(prober, true) // escape hatch set

		ns := "d38s1-escape"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		Expect(k8sClient.Create(ctx, buildLoop(ns))).To(Succeed())

		reconcileLoop(ns, "l1")

		Eventually(func() bool {
			return getSandboxMode(ns, "l1-sandbox") == sandboxv1beta1.SandboxOperatingModeRunning
		}, "10s").Should(BeTrue(), "AllowUnenforcedNetwork lets the Loop run")

		cond := networkEnforcedCondition(getLoop(ns, "l1").Status.Conditions)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("EnforcementDisabled"))
	})

	It("re-gates a RUNNING sandbox to Suspended when the result flips (Unknown -> CNIUnenforced) on the next reconcile", func() {
		ctx = context.Background()
		prober := cni.NewFakeProber() // starts Unknown
		r = newReconciler(prober, false)

		ns := "d38s1-flip"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		Expect(k8sClient.Create(ctx, buildLoop(ns))).To(Succeed())

		// First, an enforcing CNI: the sandbox runs.
		prober.SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced})
		reconcileLoop(ns, "l1")
		Eventually(func() bool {
			return getSandboxMode(ns, "l1-sandbox") == sandboxv1beta1.SandboxOperatingModeRunning
		}, "10s").Should(BeTrue())

		// The probe result flips (the probe Runnable would push this via the
		// channel; the next reconcile of the Loop must pick it up).
		prober.SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIUnenforced, Detail: "KUBELET_NODE"})
		reconcileLoop(ns, "l1")

		Eventually(func() bool {
			return getSandboxMode(ns, "l1-sandbox") == sandboxv1beta1.SandboxOperatingModeSuspended
		}, "10s").Should(BeTrue(), "a flipped result must re-gate the running sandbox to Suspended")
		Expect(networkEnforcedCondition(getLoop(ns, "l1").Status.Conditions).Reason).To(Equal("CNIUnenforced"))
	})

	It("keeps the sandbox Running when the result is CNIEnforced across re-reconciles (I43 same-Loop)", func() {
		ctx = context.Background()
		prober := cni.NewFakeProber()
		prober.SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced})
		r = newReconciler(prober, false)

		ns := "d38s1-stable"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		Expect(k8sClient.Create(ctx, buildLoop(ns))).To(Succeed())

		reconcileTwice(ns, "l1")

		Eventually(func() bool {
			return getSandboxMode(ns, "l1-sandbox") == sandboxv1beta1.SandboxOperatingModeRunning
		}, "10s").Should(BeTrue(), "an enforcing result must keep the sandbox Running across reconciles")
	})
})

// networkEnforcedCondition finds the NetworkEnforced condition.
func networkEnforcedCondition(conds []metav1.Condition) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == cni.NetworkEnforcedCondition {
			return &conds[i]
		}
	}
	return nil
}
