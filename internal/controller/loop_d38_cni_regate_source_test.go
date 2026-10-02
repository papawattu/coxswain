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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
)

// D38s3 regression (Bug C) + R16 I43 gate norm: the re-gate source must
// enqueue the Loop it receives, not the Loop's controller owner. The probe
// (ProbeRunnable.retage) sends one GenericEvent per Loop — the Loop itself,
// not an owned object — so cni.RegateSource must wrap the channel in
// EnqueueRequestForObject. The buggy EnqueueRequestForOwner(OnlyControllerOwner)
// looks for a controller owner reference ON the event object and filters to
// the Loop kind: a Loop has no Loop controller owner, so no request is ever
// enqueued and the re-gate never reconciles any Loop. Verified live: the
// operator logged "CNI probe result changed" and emitted the per-Loop Events,
// but no reconcile followed and the Loops kept their stale NetworkEnforced
// conditions.
//
// This spec drives cni.RegateSource — the exact construction AddProbeRunnable
// returns — with a GenericEvent for a Loop and asserts the handler behavior
// directly. It fails when RegateSource regresses to
// EnqueueRequestForOwner(OnlyControllerOwner) (I43: a spec that built its own
// handler would still pass while the operator regressed). The
// controller-side half (request -> reconcile -> condition update) is the
// ordinary reconcile path already covered by loop_d38_cni_gate_test.go; this
// spec is the wiring regression the live run exposed.
var _ = Describe("D38 CNI re-gate source (cni.RegateSource)", func() {
	var (
		scheme *runtime.Scheme
		loop   *coxv1alpha1.Loop
		queue  workqueue.TypedRateLimitingInterface[reconcile.Request]
	)

	BeforeEach(func() {
		scheme = runtime.NewScheme()
		Expect(coxv1alpha1.AddToScheme(scheme)).To(Succeed())
		loop = &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "d38-regate-src", Namespace: "default"},
			Spec:       coxv1alpha1.LoopSpec{Goal: "g", Workspace: testWorkspace()},
		}
		queue = workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	})

	AfterEach(func() {
		queue.ShutDown()
	})

	// drained returns the number of requests currently in the queue, removing
	// them so the counter is stable across repeated calls. It must NOT call
	// queue.Get() on an empty queue (Get blocks until an item arrives), so it
	// peeks with Len() first.
	drained := func() int {
		n := 0
		for queue.Len() > 0 {
			item, _ := queue.Get()
			n++
			queue.Done(item)
		}
		return n
	}

	It("EnqueueRequestForOwner(OnlyControllerOwner) enqueues NO request for a Loop (the bug RegateSource must not use)", func() {
		// Characterizes the bug: the ForOwner handler resolves the controller
		// owner of the event object, filtered to Loop, and a Loop has no Loop
		// controller owner — so the re-gate never reconciles. This pins the
		// bug shape so the second spec's contrast stays meaningful.
		ch := make(chan event.TypedGenericEvent[client.Object], 4)
		src := cni.Channel(ch, handler.EnqueueRequestForOwner(
			scheme, nil, &coxv1alpha1.Loop{}, handler.OnlyControllerOwner(),
		))
		Expect(src).NotTo(BeNil())

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		Expect(src.Start(ctx, queue)).To(Succeed())

		// retage sends a GenericEvent whose Object is the Loop.
		ch <- event.GenericEvent{Object: loop}

		// No request is ever produced (the Loop has no controller
		// owner). This is the live failure: the re-gate Events fired, but no
		// reconcile followed.
		Consistently(func() int { return drained() }, 300*time.Millisecond).Should(BeZero(),
			"EnqueueRequestForOwner(OnlyControllerOwner) must produce no request for a Loop — the re-gate bug")
	})

	It("cni.RegateSource enqueues the Loop itself", func() {
		// I43: drive the exact construction AddProbeRunnable returns, not a
		// hand-built copy. RED when RegateSource uses
		// EnqueueRequestForOwner(OnlyControllerOwner) instead of
		// EnqueueRequestForObject (mutation-check: reverted RegateSource to the
		// buggy handler -> this spec failed -> reverted back).
		ch := make(chan event.TypedGenericEvent[client.Object], 4)
		src := cni.RegateSource(ch)
		Expect(src).NotTo(BeNil())

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		Expect(src.Start(ctx, queue)).To(Succeed())

		ch <- event.GenericEvent{Object: loop}

		// GREEN: exactly one request, for the Loop itself. drainOne is
		// non-blocking (queue.Get() on an empty queue blocks), so this cannot
		// hang the suite — it fails fast if the request never arrives.
		drainOne := func() (reconcile.Request, bool) {
			if queue.Len() == 0 {
				return reconcile.Request{}, false
			}
			item, _ := queue.Get()
			queue.Done(item)
			return item, true
		}
		var req reconcile.Request
		Eventually(func() bool {
			var ok bool
			req, ok = drainOne()
			return ok
		}, 2*time.Second).Should(BeTrue(),
			"EnqueueRequestForObject must produce a request for the Loop the probe re-gates")
		Expect(req).To(Equal(reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: "default", Name: "d38-regate-src",
		}}))
	})
})
