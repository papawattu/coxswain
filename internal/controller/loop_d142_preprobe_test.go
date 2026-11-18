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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// holderProber is the real wiring in miniature: the reconciler reads the
// shared cni holder (what PodProber.LatestResult does), and the probe
// Runnable's result comes from the embedded fake.
type holderProber struct{ *cni.FakeProber }

func (holderProber) LatestResult() cni.CNIProbeResult { return cni.Holder().Result() }

// D142(a) (I273): before a new leader's first CNI probe result, a sandbox that
// already runs with NetworkEnforced=True recorded keeps running; creation is
// still held; the recorded True is not rewritten; and the first result reaches
// every running Loop even when the re-gate channel is full.
var _ = Describe("D142 new leader before its first CNI probe (I273)", func() {
	var (
		ctx context.Context
		r   *LoopReconciler
	)

	newLoop := func(ns, name string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "g",
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
			},
		}
	}
	mode := func(ns, name string) sandboxv1beta1.SandboxOperatingMode {
		sbx := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name + "-sandbox"}, sbx)).To(Succeed())
		return sbx.Spec.OperatingMode
	}
	cond := func(ns, name string) *metav1.Condition {
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, got)).To(Succeed())
		return networkEnforcedCondition(got.Status.Conditions)
	}
	reconcile1 := func(ns, name string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())
	}
	setup := func(ns string, names ...string) {
		ctx = context.Background()
		cni.ResetForTest()
		DeferCleanup(cni.ResetForTest)
		r = &LoopReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			CNIProber:       holderProber{cni.NewFakeProber()},
			AllowUnenforced: true, // only the D38 gate is under test
		}
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })
		// The previous leader's world: probe enforced, sandbox running, True recorded.
		cni.Holder().Set(cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced})
		for _, n := range names {
			Expect(k8sClient.Create(ctx, newLoop(ns, n))).To(Succeed())
			reconcile1(ns, n)
			reconcile1(ns, n)
			Expect(mode(ns, n)).To(Equal(sandboxv1beta1.SandboxOperatingModeRunning))
			Expect(cond(ns, n).Status).To(Equal(metav1.ConditionTrue))
		}
		// The new leader: holder back to Unknown (no result yet).
		cni.ResetForTest()
	}

	It("keeps a running sandbox with NetworkEnforced=True running across TWO reconciles before the first probe, and does not rewrite the condition", func() {
		setup("d142-keep", "l1")

		reconcile1("d142-keep", "l1")
		Expect(mode("d142-keep", "l1")).To(Equal(sandboxv1beta1.SandboxOperatingModeRunning),
			"first reconcile before the first probe must not Suspend a running enforced sandbox")
		reconcile1("d142-keep", "l1") // the second reconcile reads what the first wrote
		Expect(mode("d142-keep", "l1")).To(Equal(sandboxv1beta1.SandboxOperatingModeRunning),
			"second reconcile must not Suspend either")
		c := cond("d142-keep", "l1")
		Expect(c.Status).To(Equal(metav1.ConditionTrue), "the recorded True must not be replaced by Unknown before the first result")
		Expect(c.Reason).To(Equal("CNIEnforced"))
	})

	It("still holds a sandbox that does not exist yet before the first probe (fail-closed for new pods)", func() {
		setup("d142-new")
		Expect(k8sClient.Create(ctx, newLoop("d142-new", "fresh"))).To(Succeed())
		reconcile1("d142-new", "fresh")
		Expect(mode("d142-new", "fresh")).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended))
		Expect(cond("d142-new", "fresh").Reason).To(Equal("Unknown"))
	})

	It("does not keep a sandbox running when the recorded condition is not True (nothing was proven)", func() {
		setup("d142-nottrue", "l1")
		// Overwrite the record as the old code would have (Unknown).
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "d142-nottrue", Name: "l1"}, got)).To(Succeed())
		setCondition(got, cni.NetworkEnforcedCondition, metav1.ConditionUnknown, "Unknown", "x")
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		reconcile1("d142-nottrue", "l1")
		Expect(mode("d142-nottrue", "l1")).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended))
	})

	It("the first CNIUnenforced result still Suspends every running Loop when the re-gate channel is full", func() {
		setup("d142-full", "a", "b", "c")

		prober := cni.NewFakeProber()
		prober.SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIUnenforced, Detail: "APISERVER_SVC"})
		ch := make(chan event.TypedGenericEvent[client.Object], 1)
		// Fill the channel (capacity 1) so every non-blocking send would drop.
		filler := &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: "filler", Namespace: "d142-full"}}
		ch <- event.GenericEvent{Object: filler}
		runnable := &cni.ProbeRunnable{
			Prober: prober, Client: k8sClient, Interval: time.Hour, Timeout: 5 * time.Second, Ch: ch,
		}
		rctx, cancel := context.WithCancel(ctx)
		DeferCleanup(cancel)
		var wg sync.WaitGroup
		// Stop and join both goroutines before the namespace is deleted (a
		// reconcile racing a terminating namespace gets Forbidden).
		DeferCleanup(func() { cancel(); wg.Wait() })
		wg.Add(2)
		go func() { defer wg.Done(); _ = runnable.Start(rctx) }()

		// The probe result lands; the channel is still full (nobody drains yet).
		Eventually(func() cni.Reason { return cni.Holder().Result().Reason }, "10s").Should(Equal(cni.ReasonCNIUnenforced))
		Consistently(func() int { return len(ch) }, "500ms").Should(Equal(1))

		// Now the controller starts draining: each event reconciles its Loop.
		var mu sync.Mutex
		seen := map[string]bool{}
		go func() {
			defer wg.Done()
			for {
				select {
				case <-rctx.Done():
					return
				case ev := <-ch:
					// The runnable lists every Loop in the shared envtest
					// cluster; only this spec's namespace is under test.
					if ev.Object.GetNamespace() != "d142-full" || ev.Object.GetName() == "filler" {
						continue
					}
					mu.Lock()
					seen[ev.Object.GetName()] = true
					mu.Unlock()
					_, _ = r.Reconcile(rctx, reconcile.Request{NamespacedName: types.NamespacedName{
						Namespace: ev.Object.GetNamespace(), Name: ev.Object.GetName()}})
				}
			}
		}()
		for _, n := range []string{"a", "b", "c"} {
			Eventually(func() sandboxv1beta1.SandboxOperatingMode { return mode("d142-full", n) }, "15s").
				Should(Equal(sandboxv1beta1.SandboxOperatingModeSuspended), "Loop %s must be re-gated by the first result", n)
		}
		mu.Lock()
		defer mu.Unlock()
		Expect(seen).To(HaveLen(3))
	})
})
