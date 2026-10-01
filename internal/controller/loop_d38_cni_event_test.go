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
	"k8s.io/client-go/tools/record"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// D38 design point 3: a Kubernetes Event is emitted on every Loop when its
// NetworkEnforced condition CHANGES. This spec drives a real (fake) recorder:
// the first condition (initial Unknown, fail-closed) is a change and emits an
// Event; a reason flip emits another; an unchanged result emits nothing
// (change-only semantics).

var _ = Describe("D38 NetworkEnforced condition-change Event", func() {
	var (
		ctx      context.Context
		ns       string
		recorder *record.FakeRecorder
		suffix   int
	)

	BeforeEach(func() {
		ctx = context.Background()
		suffix++
		ns = "d38-event-" + string(rune('a'+suffix%26))
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		recorder = record.NewFakeRecorder(64)
	})

	AfterEach(func() {
		_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	buildLoop := func(ns string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "ev1", Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "g",
				Workspace: coxv1alpha1.Workspace{Repo: "https://example.com/x.git"},
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
			},
		}
	}

	reconcileLoop := func(r *LoopReconciler, ns, name string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())
	}

	It("emits an Event on the initial condition and on a reason flip, and not on an unchanged result", func() {
		prober := cni.NewFakeProber() // default: Unknown
		r := &LoopReconciler{
			Client:                 k8sClient,
			Scheme:                 k8sClient.Scheme(),
			CNIProber:              prober,
			AllowUnenforced:        true, // let the eBPF gate pass
			AllowUnenforcedNetwork: false,
			Recorder:               recorder,
		}
		Expect(k8sClient.Create(ctx, buildLoop(ns))).To(Succeed())

		// Initial Unknown condition is a change from nothing: an Event.
		reconcileLoop(r, ns, "ev1")
		Eventually(recorder.Events).Should(
			ContainElement(ContainSubstring("NetworkEnforced=Unknown reason=Unknown")),
			"the initial fail-closed Unknown condition must emit an Event")

		// Flip the probe result to CNIUnenforced: the condition changes and a
		// second Event with the new reason is emitted.
		prober.SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIUnenforced, Detail: "EXTERNAL"})
		reconcileLoop(r, ns, "ev1")
		Eventually(recorder.Events).Should(
			ContainElement(ContainSubstring("reason=CNIUnenforced")),
			"a reason flip must emit an Event")

		// An unchanged result emits no further NetworkEnforced Event
		// (change-only semantics).
		count := len(recorder.Events)
		reconcileLoop(r, ns, "ev1")
		Consistently(func() int { return len(recorder.Events) }, "2s").
			Should(Equal(count), "an unchanged result must not re-emit the Event")
	})
})
