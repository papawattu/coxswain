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

// F12 (the #164 review P2): a DELETING Loop with a readable AgentPolicy and a
// KubeArmorPolicy write that returns Forbidden (a terminating namespace) must
// STILL complete its finalizer work — the B1 finalizer is removed and the Loop
// is deleted — rather than blocking in CleaningUp on the Forbidden write. The
// enforcement write is best-effort on the deleting path (F12): it is attempted,
// the Forbidden is logged (an Event is emitted when a Recorder is wired), and
// the pass proceeds to the CleaningUp work (the finalizer removal). The live
// path is unchanged (a Forbidden KubeArmorPolicy write on a LIVE Loop still
// surfaces as the reconcile error).
//
// The envtest cluster has the KubeArmorPolicy CRD loaded (config/crd/external),
// so the write is driven through the reconciler's ApplyEnforcementWritesFn seam
// (F12): the test injects a function that returns Forbidden (the terminating
// namespace's admission), so the best-effort tolerance is exercised without a
// real unstructured round-trip. The B1 finalizer removal (the load-bearing
// assertion) requires the full B1 finalizer machinery — the reconcile passes
// through finalizeB2DeleteStep → reconcileCleaningUp → the B1 GC (no snapshots
// in this test, so the GC is a no-op) → the finalizer removal.
//
// Mutation (the test must FAIL when the fix regresses): make the deleting-path
// enforcement write propagate the error again (restore the old
// applyEffectivePolicyAndConditions call in finalizeB2DeleteStep). Then the
// deleting-path reconcile errors on the Forbidden, the finalizer is NOT
// removed, and the spec's "the B1 checkpoint finalizer must be REMOVED"
// assertion fails.

package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/engine"
	"github.com/papawattu/coxswain/internal/policy"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// forbiddenEnforcer returns Forbidden on Apply (the terminating namespace's
// admission). Enforcing is a no-op (the deleting path does not gate on it).
// The seam (ApplyEnforcementWritesFn) is the load-bearing driver (it returns
// Forbidden directly); the Enforcer's own methods are only used for the
// KaptSpecHash record (a no-op when the write is tolerated).
type forbiddenEnforcer struct{}

func (f *forbiddenEnforcer) Apply(ctx context.Context, l *coxv1alpha1.Loop, ep policy.EffectivePolicy) error {
	return errors.NewForbidden(schema.GroupResource{Group: "security.kubearmor.com", Resource: "kubearmorpolicies"}, "coxswain-"+l.Name,
		fmt.Errorf("admission webhook denied the KubeArmorPolicy (terminating namespace)"))
}
func (f *forbiddenEnforcer) Enforcing(ctx context.Context, l *coxv1alpha1.Loop) (bool, string) {
	return false, engine.ReasonEngineUnavailable
}
func (f *forbiddenEnforcer) KaptSpecHash(l *coxv1alpha1.Loop, ep policy.EffectivePolicy) string {
	return "test-hash"
}
func (f *forbiddenEnforcer) BPFNodeAffinity() *corev1.Affinity { return nil }

var _ = Describe("F12 deleting Loop with a Forbidden KubeArmorPolicy write (the #164 review P2)", func() {
	var (
		ns           string
		loopName     string
		apName       string
		r            *LoopReconciler
		wasAttempted bool
	)

	BeforeEach(func() {
		ns = "f12-" + fmt.Sprintf("%d", time.Now().UnixNano()%1000000)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		apName = "f12-ap"
		// The readable AgentPolicy (the precondition: policiesReadable=true).
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: apName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{"/bin/sh"}},
		})).To(Succeed())
		loopName = "f12-loop"
		wasAttempted = false
	})

	AfterEach(func() {
		_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	// forbiddenApply returns Forbidden (the terminating namespace's admission)
	// and records that the write was attempted.
	forbiddenApply := func() func(ctx context.Context, loop *coxv1alpha1.Loop, ep policy.EffectivePolicy) error {
		return func(ctx context.Context, loop *coxv1alpha1.Loop, ep policy.EffectivePolicy) error {
			wasAttempted = true
			return errors.NewForbidden(schema.GroupResource{Group: "security.kubearmor.com", Resource: "kubearmorpolicies"}, "coxswain-"+loop.Name,
				fmt.Errorf("admission webhook denied the KubeArmorPolicy (terminating namespace)"))
		}
	}

	It("removes the B1 finalizer and deletes the Loop despite the Forbidden KubeArmorPolicy write (best-effort, F12)", func() {
		// The Loop with a PolicyRefs to the readable AgentPolicy (the
		// precondition: policiesReadable=true).
		l := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: loopName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "goal",
				Agent:      coxv1alpha1.AgentConfig{Image: "sandbox"},
				PolicyRefs: []string{apName},
				Workspace: coxv1alpha1.Workspace{
					Repo: "https://example.com/repo.git",
				},
			},
		}
		Expect(k8sClient.Create(ctx, l)).To(Succeed())
		// Add the B1 checkpoint finalizer (the finalizeB2DeleteStep path
		// requires it to reach CleaningUp; the real controller adds it on
		// creation, but this spec drives the reconciler directly, so it is
		// added here) and set the DeletionTimestamp (the deleting path).
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(l), l)).To(Succeed())
		l.Finalizers = append(l.Finalizers, loopCheckpointFinalizer)
		Expect(k8sClient.Update(ctx, l)).To(Succeed())
		Expect(k8sClient.Delete(ctx, l)).To(Succeed())
		Eventually(func() bool {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(l), l)).To(Succeed())
			return l.DeletionTimestamp != nil
		}, "5s", "100ms").Should(BeTrue(), "the Loop must have a DeletionTimestamp (the finalizer holds it)")

		// The reconciler with the forbidden Enforcer + seam: the deleting-path
		// enforcement write (Enforcer.Apply) returns Forbidden (the terminating
		// namespace's admission).
		r = &LoopReconciler{
			Client:                   k8sClient,
			Scheme:                   k8sClient.Scheme(),
			Enforcer:                 &forbiddenEnforcer{},
			ApplyEnforcementWritesFn: forbiddenApply(),
		}

		// Reconcile the deleting Loop. The load-bearing assertion: the B1
		// finalizer is REMOVED (the CleaningUp work completed despite the
		// Forbidden KubeArmorPolicy write — best-effort, F12) and the Loop is
		// deleted.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: loopName}})
		Expect(err).NotTo(HaveOccurred(),
			"the deleting-path reconcile must NOT error on the Forbidden KubeArmorPolicy write (best-effort, F12): %v", err)
		Expect(wasAttempted).To(BeTrue(),
			"the deleting-path enforcement write (Enforcer.Apply) must have been attempted (the Forbidden is the precondition)")
		// The B1 finalizer is removed (the CleaningUp work completed).
		Eventually(func() bool {
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(l), l)
			if errors.IsNotFound(err) {
				return true
			}
			Expect(err).NotTo(HaveOccurred())
			for _, f := range l.Finalizers {
				if f == loopCheckpointFinalizer {
					return false
				}
			}
			return true
		}, "5s", "100ms").Should(BeTrue(),
			"the B1 checkpoint finalizer must be REMOVED (the CleaningUp work completed despite the Forbidden KubeArmorPolicy write — best-effort, F12); the Loop must be deleted")
	})

	It("surfaces the Forbidden KubeArmorPolicy write as a reconcile error on the LIVE path (the live path is unchanged)", func() {
		l := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: loopName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "goal",
				Agent:      coxv1alpha1.AgentConfig{Image: "sandbox"},
				PolicyRefs: []string{apName},
				Workspace: coxv1alpha1.Workspace{
					Repo: "https://example.com/repo.git",
				},
			},
		}
		Expect(k8sClient.Create(ctx, l)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(l), l)).To(Succeed())

		r = &LoopReconciler{
			Client:                   k8sClient,
			Scheme:                   k8sClient.Scheme(),
			Enforcer:                 &forbiddenEnforcer{},
			ApplyEnforcementWritesFn: forbiddenApply(),
		}

		// The LIVE path: the forbidden Enforcer write must surface as the
		// reconcile error (the live path is unchanged — it does NOT tolerate
		// the write failure).
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: loopName}})
		Expect(err).To(HaveOccurred(),
			"the LIVE-path reconcile MUST error on the Forbidden KubeArmorPolicy write (the live path is unchanged)")
		Expect(wasAttempted).To(BeTrue(), "the live-path enforcement write (Enforcer.Apply) must have been attempted")
	})
})
