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

// F62: the alert relay's blocked-action count reaches status.policy
// .blockedCount and the PolicyBlocked condition, and it never invents a number:
// an untrusted stream writes UNKNOWN (nil count), and "no news" writes nothing.
//
// The relay itself (following the engine's and the proxies' pod logs, the gap
// and replay rules, the Q4 lines) is specified in internal/alerts; these specs
// cover the RECONCILE's half of the seam — what the Loop object says.

package controller

import (
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/alerts"
)

// f62Audit is a settable AuditReader: the relay's holder, without the streams.
type f62Audit struct {
	mu    sync.Mutex
	snaps map[types.UID]alerts.Snapshot
}

func newF62Audit() *f62Audit { return &f62Audit{snaps: map[types.UID]alerts.Snapshot{}} }

func (f *f62Audit) Set(uid types.UID, snap alerts.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snaps[uid] = snap
}

func (f *f62Audit) Snapshot(uid types.UID) (alerts.Snapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap, ok := f.snaps[uid]
	return snap, ok
}

var _ AuditReader = &f62Audit{}

var _ = Describe("F62: the blocked-action count is relayed into status", func() {
	ctx := context.Background()

	f62Ns := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	f62GetLoop := func(ns, name string) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			Fail("get loop: " + err.Error())
		}
		return loop
	}

	// f62NewLoop creates a Loop and returns its UID (the count key).
	f62NewLoop := func(ns, name string) types.UID {
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())
		return f62GetLoop(ns, name).UID
	}

	f62Reconciler := func(audit AuditReader) *LoopReconciler {
		return &LoopReconciler{
			CNIProber: testCNIProber(),
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			apiReader: k8sClient,
			Recorder:  record.NewFakeRecorder(64),
			Audit:     audit,
		}
	}

	f62Blocks := func(loop *coxv1alpha1.Loop) *metav1.Condition {
		for i := range loop.Status.Conditions {
			if loop.Status.Conditions[i].Type == PolicyBlockedCondition {
				return &loop.Status.Conditions[i]
			}
		}
		return nil
	}

	It("writes the count with a True condition, and updates it on the next reconcile (I43)", func() {
		ns := f62Ns("f62-count")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f62count"
		audit := newF62Audit()
		uid := f62NewLoop(ns, name)
		r := f62Reconciler(audit)

		audit.Set(uid, alerts.Snapshot{
			Count: 2, Health: alerts.HealthHealthy,
			LatestTarget: "/etc/passwd", LatestAt: time.Now().UTC(),
		})
		loop := s5aReconcile(r, ns, name)
		Expect(loop.Status.Policy).NotTo(BeNil())
		Expect(loop.Status.Policy.BlockedCount).NotTo(BeNil())
		Expect(*loop.Status.Policy.BlockedCount).To(Equal(int64(2)))
		cond := f62Blocks(loop)
		Expect(cond).NotTo(BeNil(), "the PolicyBlocked condition must be written")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(policyBlocksObservedReason))
		Expect(cond.Message).To(ContainSubstring("blocked 2 action(s)"))
		Expect(cond.Message).To(ContainSubstring("/etc/passwd"),
			"the message says WHAT was bounced, not only how often")

		// The same Loop, a new count: an update, not a create-only write (I43).
		audit.Set(uid, alerts.Snapshot{
			Count: 3, Health: alerts.HealthHealthy,
			LatestTarget: "/root/.ssh/id_ed25519", LatestAt: time.Now().UTC(),
		})
		loop = s5aReconcile(r, ns, name)
		Expect(loop.Status.Policy.BlockedCount).NotTo(BeNil())
		Expect(*loop.Status.Policy.BlockedCount).To(Equal(int64(3)),
			"the second reconcile must UPDATE the count, not leave the first")
		Expect(f62Blocks(loop).Message).To(ContainSubstring("blocked 3 action(s)"))
	})

	It("writes UNKNOWN and clears the count when the stream cannot be trusted — never 0 (I32)", func() {
		ns := f62Ns("f62-unknown")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f62unknown"
		audit := newF62Audit()
		uid := f62NewLoop(ns, name)
		r := f62Reconciler(audit)

		// A real count first, so the clear is observable.
		audit.Set(uid, alerts.Snapshot{Count: 4, Health: alerts.HealthHealthy, LatestTarget: "/a"})
		loop := s5aReconcile(r, ns, name)
		Expect(loop.Status.Policy.BlockedCount).NotTo(BeNil())

		// The stream re-attached over a window it cannot prove (a gap).
		audit.Set(uid, alerts.Snapshot{Count: 4, Health: alerts.HealthGap, Reason: alerts.ReasonStreamGap})
		loop = s5aReconcile(r, ns, name)
		Expect(loop.Status.Policy.BlockedCount).To(BeNil(),
			"an untrusted stream must clear the count rather than republish the last number or 0")
		cond := f62Blocks(loop)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
		Expect(cond.Reason).To(Equal(alerts.ReasonStreamGap))
		Expect(cond.Message).To(ContainSubstring("UNKNOWN, not zero"))
	})

	It("writes nothing at all when the relay has no news for the Loop", func() {
		ns := f62Ns("f62-nonews")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f62nonews"
		audit := newF62Audit()
		f62NewLoop(ns, name)
		r := f62Reconciler(audit)

		loop := s5aReconcile(r, ns, name)
		Expect(f62Blocks(loop)).To(BeNil(),
			"no stream has attached, so there is no condition — 'none yet' is not Unknown and not 0")
		if loop.Status.Policy != nil {
			Expect(loop.Status.Policy.BlockedCount).To(BeNil())
		}
	})

	It("never fails or pauses the Loop for a block (Q5)", func() {
		ns := f62Ns("f62-q5")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f62q5"
		audit := newF62Audit()
		uid := f62NewLoop(ns, name)
		r := f62Reconciler(audit)

		s5aReconcile(r, ns, name)
		audit.Set(uid, alerts.Snapshot{Count: 9, Health: alerts.HealthHealthy, LatestTarget: "/etc/shadow"})
		after := s5aReconcile(r, ns, name)
		Expect(after.Status.Phase).NotTo(Equal(coxv1alpha1.LoopPhaseFailed),
			"a block is a fact about the agent's traffic, never a phase decision (ADR-0007 Q5)")
		Expect(after.Status.Policy).NotTo(BeNil())
		Expect(after.Status.Policy.BlockedCount).NotTo(BeNil())
		Expect(*after.Status.Policy.BlockedCount).To(Equal(int64(9)),
			"the block IS recorded — Q5 says it just never acts on the Loop")
		for _, c := range after.Status.Conditions {
			Expect(c.Type).NotTo(Equal(string(coxv1alpha1.LoopPhaseFailed)),
				"a block must not write a Failed condition")
			Expect(c.Type).NotTo(Equal(coxv1alpha1.PausedCondition),
				"a block must not pause the Loop")
		}
	})

	It("surfaces the unattributed alerts in the message (never a silently smaller count)", func() {
		ns := f62Ns("f62-unattr")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "f62unattr"
		audit := newF62Audit()
		uid := f62NewLoop(ns, name)
		r := f62Reconciler(audit)

		audit.Set(uid, alerts.Snapshot{Count: 0, Health: alerts.HealthHealthy, Unattributed: 3})
		loop := s5aReconcile(r, ns, name)
		cond := f62Blocks(loop)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Message).To(ContainSubstring("3 audit alert(s)"),
			"alerts the relay could not attribute are visible, so a missing label is not a quietly smaller count")
		Expect(loop.Status.Policy.BlockedCount).NotTo(BeNil())
		Expect(*loop.Status.Policy.BlockedCount).To(Equal(int64(0)),
			"0 is publishable here: the stream is healthy and the engine reported no block")
	})
})
