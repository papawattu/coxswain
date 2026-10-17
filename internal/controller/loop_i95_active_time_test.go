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

// I95 (R28): the deliver PR body must carry the Loop's active time as of
// the moment the body is rendered.
//
// #166's body said "Budget (active time): 0s" for a run that took several
// minutes from Planning to Delivered. The body is rendered from the Loop's
// in-memory status (deliverDescriptionForLoop) at the point ensureDeliver
// ensures the deliver description ConfigMap, and the applyBudgetStep call
// in the same Reconcile runs on the loop BEFORE the deliver step — so by
// the time the body is rendered, status.budget.lastActiveStamp is the
// current pass's now (the S5->S6 phase flip reconcile's own applyBudget
// already refreshed it), and the render's own now - stamp is 0s. The body
// therefore shows 0s even though the Loop has been active for minutes: the
// accumulated value in status.budget.activeSeconds is stale (it lags by
// the last reconcile-to-reconcile gap, which is minutes between phase
// flips), and the render's refresh adds 0 on top of the stale value.
//
// The fix: deliverDescriptionForLoop accumulates the wall clock into
// status.budget (the same now - lastActiveStamp accumulation
// applyBudgetStep does) BEFORE rendering, so the body carries the time
// measured up to this render. Because applyBudgetStep in the same
// reconcile already set the stamp to now (for a non-paused Loop), the
// body's active time equals status.budget.activeSeconds as of the start
// of this reconcile — the last accumulated value — which is the correct
// "active time up to now" for the run (the render's own sub-second
// elapsed is not material). The stale-by-one-gap behaviour is what made
// #166 print 0s: the S5->S6 first Succeeded reconcile created
// status.budget (a fresh accumulation adds 0), and the body rendered from
// that fresh 0.
//
// The spec:
//   - (unit) a Loop with status.budget.activeSeconds = 0 and a
//     lastActiveStamp N minutes ago, with spec.budget.maxWallClock set,
//     renders a body with the N-minute active time (not 0s);
//   - (unit) a Loop with no lastActiveStamp renders 0s (a fresh
//     accumulation adds 0);
//   - (envtest, same-Loop) a Succeeded Loop whose status.budget was set
//     before delivery (with a known stamp) re-reconciles and the deliver
//     description ConfigMap's body carries the non-zero active time
//     measured from the stamp to the reconcile — not 0s. The spec mutates
//     the Loop's status (the input) and re-reconciles, asserting the
//     ConfigMap's body is UPDATED (the I42c-style same-Loop update).
//
// The mutation (exact diff + exact focus command in the PR): skip the
// refresh in deliverDescriptionForLoop (remove the
// refreshDeliverDescriptionBudget(loop) call) in a scratch worktree and
// re-run the focus command; the envtest spec must FAIL with the body
// showing 0s.
package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// i95LoopWithBudget builds a delivery-expected Loop (Succeeded +
// PullRequest + a pinned verifiedCommit + an in-cluster repo) with a
// status.budget carrying the given activeSeconds and lastActiveStamp.
func i95LoopWithBudget(name, ns string, activeSeconds int64, stamp *metav1.Time) *coxv1alpha1.Loop {
	tamper := int32(0)
	return &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: coxv1alpha1.LoopSpec{
			Goal:      "I95 active-time test",
			Workspace: coxv1alpha1.Workspace{Repo: inClusterRepoURL},
			Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
			Agent: coxv1alpha1.AgentConfig{
				EndpointSecretRef: samplesModelCredSecret,
				ModelEndpoint:     s3ModelEndpoint,
			},
			Delivery: &coxv1alpha1.DeliveryConfig{Mode: coxv1alpha1.DeliveryModePullRequest},
			Budget:   &coxv1alpha1.BudgetConfig{MaxWallClock: "1h"},
		},
		Status: coxv1alpha1.LoopStatus{
			Phase:     coxv1alpha1.LoopPhaseSucceeded,
			Iteration: 1,
			CurrentVerify: &coxv1alpha1.CurrentVerifyStatus{
				VerifiedCommit: s6HeadCommit,
			},
			Verify:   &coxv1alpha1.VerifyStatus{TamperExitCode: &tamper},
			Progress: &coxv1alpha1.ProgressStatus{},
			Budget:   &coxv1alpha1.BudgetStatus{ActiveSeconds: activeSeconds, LastActiveStamp: stamp},
		},
	}
}

// activeTimeLine extracts the "Budget (active time)" line from a body.
func activeTimeLine(body string) (string, bool) {
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(ln, "- **Budget (active time):**") {
			return ln, true
		}
	}
	return "", false
}

// TestI95DeliverDescriptionActiveTimeKnown (I95) feeds the description a
// Loop with a known active time (status.budget.lastActiveStamp 3 minutes
// ago, activeSeconds 0 — the shape the S5->S6 first Succeeded reconcile
// leaves) and asserts the body carries the 3-minute active time, not 0s.
// The body is a pure function of spec/status, so the test calls
// deliverDescriptionForLoop directly (no cluster needed).
func TestI95DeliverDescriptionActiveTimeKnown(t *testing.T) {
	stamp := metav1.NewTime(time.Now().Add(-3 * time.Minute))
	l := i95LoopWithBudget("i95unit", "i95ns", 0, &stamp)
	body := renderDeliverDescriptionBody(deliverDescriptionForLoop(l))
	line, ok := activeTimeLine(body)
	if !ok {
		t.Fatalf("the body does not carry an active time line:\n%s", body)
	}
	if strings.Contains(line, " 0s") {
		t.Fatalf("the body must not carry 0s active time for a Loop with a lastActiveStamp 3 minutes ago (I95): %s", line)
	}
	if !strings.HasPrefix(line, "- **Budget (active time):** 3m") {
		t.Fatalf("the body must carry the 3-minute active time (I95): %s", line)
	}
}

// TestI95DeliverDescriptionActiveTimeNoStamp (I95) asserts the no-stamp
// case: a Loop whose status.budget has no lastActiveStamp yet (a fresh
// accumulation) renders 0s — the accumulation adds 0, not a negative
// value, not the render's clock.
func TestI95DeliverDescriptionActiveTimeNoStamp(t *testing.T) {
	l := i95LoopWithBudget("i95nostamp", "i95ns", 0, nil)
	body := renderDeliverDescriptionBody(deliverDescriptionForLoop(l))
	line, ok := activeTimeLine(body)
	if !ok {
		t.Fatalf("the body does not carry an active time line:\n%s", body)
	}
	// The line is "- **Budget (active time):** 0s of 1h0m0s cap" (a 1h cap
	// is set in the fixture); the value rendered must be exactly 0s (the
	// cap suffix may follow).
	if !strings.Contains(line, ":** 0s") {
		t.Fatalf("a no-stamp Loop must render 0s active time (a fresh accumulation adds 0): %s", line)
	}
}

// TestI95DeliverDescriptionActiveTimeNoCap (I95) asserts the no-cap case:
// a Loop with no spec.budget has no wall clock to accumulate — the body's
// active time line is 0s (applyBudgetStep records the wall clock only when
// maxWallClock is set; the body must not invent a budget status).
func TestI95DeliverDescriptionActiveTimeNoCap(t *testing.T) {
	l := i95LoopWithBudget("i95nocap", "i95ns", 0, nil)
	l.Spec.Budget = nil
	body := renderDeliverDescriptionBody(deliverDescriptionForLoop(l))
	line, ok := activeTimeLine(body)
	if !ok {
		t.Fatalf("the body does not carry an active time line:\n%s", body)
	}
	if !strings.Contains(line, ":** 0s") {
		t.Fatalf("a no-cap Loop must render 0s active time (no wall clock to read): %s", line)
	}
}

var _ = Describe("I95 deliver description active time (envtest)", func() {
	ctx := context.Background()

	freshNS := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	// I95: a Succeeded Loop with a known active time (status.budget set
	// before delivery, lastActiveStamp a few minutes ago) re-reconciles and
	// the deliver description ConfigMap's body carries the non-zero active
	// time — not 0s. Same-Loop: the spec mutates the Loop's status (the
	// input) and re-reconciles, asserting the ConfigMap's body is UPDATED
	// (the I42c-style same-Loop update).
	It("renders the deliver description ConfigMap body with the Loop's known active time", func() {
		ns := freshNS("i95-desc")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "i95task1"
		r := s6Succeeded(name, ns, inClusterRepoURL)

		// The S5->S6 shape: the Loop is Succeeded and delivery is expected,
		// but status.budget was set before delivery with a known stamp.
		// s6Loop does not set a spec.budget (no cap), so set it (the wall
		// clock only records when maxWallClock is set) and the budget with
		// a 3-minute-old stamp and 0 accumulated seconds (the first
		// Succeeded reconcile's fresh accumulation — the shape #166 left).
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		loop.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxWallClock: "1h"}
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		stamp := metav1.NewTime(time.Now().Add(-3 * time.Minute))
		loop.Status.Budget = &coxv1alpha1.BudgetStatus{ActiveSeconds: 0, LastActiveStamp: &stamp}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		// Re-reconcile (the deliver step re-ensures the ConfigMap from the
		// refreshed budget — the body is UPDATED, not just created).
		s6Reconcile(r, ns, name)

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: deliverDescriptionConfigName(name)}, cm)).To(Succeed())
		body := cm.Data[deliverDescriptionDataKey]
		line, ok := activeTimeLine(body)
		Expect(ok).To(BeTrue(), "the body must carry an active time line:\n%s", body)
		Expect(line).To(HavePrefix("- **Budget (active time):** 3m"),
			"the body must carry the 3-minute active time measured from the stamp to the reconcile (I95): %s", line)
		Expect(line).ToNot(ContainSubstring(" 0s"),
			"the body must not carry 0s for a Loop with a lastActiveStamp 3 minutes ago (I95): %s", line)
	})
})
