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
// the moment the body is rendered, whether or not a cap is set.
//
// #166's Loop (dogfood5-df5) had NO spec.budget. Its body said
// "Budget (active time): 0s" because applyBudgetStep accumulated the wall
// clock only when spec.budget.maxWallClock was set — with no cap,
// status.budget.activeSeconds was never written, and the body printed the
// zero value.
//
// The fix (option (a)): the wall clock is always accumulated on every
// non-paused reconcile, whether or not a cap is set. The cap only decides
// enforcement (onExceeded), not recording. The operator's
// ensureDeliverDescriptionConfig calls refreshDeliverDescriptionBudget on
// the loop before rendering (the wall-clock-only half of applyBudgetStep:
// now - lastActiveStamp, floor 0, set stamp) so the body carries the active
// time measured up to this render. deliverDescriptionForLoop is a PURE
// READER (it does NOT refresh) — the refresh is the operator's job, and the
// caller's status write persists the refreshed value. A4 flake (fix/a4-flake):
// the earlier implementation called refreshDeliverDescriptionBudget inside
// deliverDescriptionForLoop (in place), which mutated the caller's loop and
// caused a second mutation when a spec called it again to compute wantBody.
//
// The spec:
//   - (unit, PRIMARY) a Loop with NO spec.budget (the #166 shape) and a
//     lastActiveStamp 3 minutes ago: the operator's refresh (called in the
//     test, mirroring ensureDeliverDescriptionConfig) makes the body carry
//     the 3-minute active time (not 0s);
//   - (unit) a Loop with no lastActiveStamp renders 0s (a fresh
//     accumulation adds 0);
//   - (envtest, same-Loop) a Succeeded Loop with NO spec.budget and a known
//     stamp re-reconciles and the deliver description ConfigMap's body
//     carries the non-zero active time — not 0s.
//
// The mutation (exact diff + exact focus command in the PR): skip the
// refresh in ensureDeliverDescriptionConfig (remove the
// refreshDeliverDescriptionBudget(loop) call) in a scratch worktree and
// re-run the focus command; the primary spec must FAIL with the body
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
// spec.budget is NOT set by default — the #166 shape.
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
	for ln := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(ln, "- **Budget (active time):**") {
			return ln, true
		}
	}
	return "", false
}

// TestI95DeliverDescriptionActiveTimeNoCap (I95, PRIMARY) feeds the
// description a Loop with NO spec.budget (the #166 shape) and a known
// lastActiveStamp 3 minutes ago. It asserts the body carries the 3-minute
// active time, not 0s. Before the fix, applyBudgetStep only accumulated
// the wall clock when a cap was set, so status.budget.activeSeconds was 0
// and the body printed 0s.
func TestI95DeliverDescriptionActiveTimeNoCap(t *testing.T) {
	stamp := metav1.NewTime(time.Now().Add(-3 * time.Minute))
	l := i95LoopWithBudget("i95unit", "i95ns", 0, &stamp)
	// spec.budget is nil (the #166 shape) — confirm it.
	if l.Spec.Budget != nil {
		t.Fatal("the fixture must have no spec.budget (the #166 shape)")
	}
	// A4 flake (fix/a4-flake): the operator's ensureDeliverDescriptionConfig
	// calls refreshDeliverDescriptionBudget on the loop before rendering.
	// The unit test mirrors the operator's behavior (the reader is now a
	// pure reader; the refresh is the operator's job).
	refreshDeliverDescriptionBudget(l)
	body := renderDeliverDescriptionBody(deliverDescriptionForLoop(l))
	line, ok := activeTimeLine(body)
	if !ok {
		t.Fatalf("the body does not carry an active time line:\n%s", body)
	}
	if strings.Contains(line, " 0s") {
		t.Fatalf("the body must not carry 0s active time for a Loop with a lastActiveStamp 3 minutes ago and no spec.budget (I95, #166 shape): %s", line)
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
	// The line is "- **Budget (active time):** 0s (no cap)" (no spec.budget
	// in the fixture); the value rendered must be exactly 0s.
	if !strings.Contains(line, ":** 0s") {
		t.Fatalf("a no-stamp Loop must render 0s active time (a fresh accumulation adds 0): %s", line)
	}
}

// TestI95DeliverDescriptionActiveTimeWithCap (I95) asserts that with a cap
// set, the accumulation still works (regression guard: the cap gate used to
// surround the wall-clock accumulation; moving it out must not break the
// capped case).
func TestI95DeliverDescriptionActiveTimeWithCap(t *testing.T) {
	stamp := metav1.NewTime(time.Now().Add(-3 * time.Minute))
	l := i95LoopWithBudget("i95cap", "i95ns", 0, &stamp)
	l.Spec.Budget = &coxv1alpha1.BudgetConfig{MaxWallClock: "1h"}
	// A4 flake (fix/a4-flake): mirror the operator's refresh before rendering.
	refreshDeliverDescriptionBudget(l)
	body := renderDeliverDescriptionBody(deliverDescriptionForLoop(l))
	line, ok := activeTimeLine(body)
	if !ok {
		t.Fatalf("the body does not carry an active time line:\n%s", body)
	}
	if strings.Contains(line, " 0s") {
		t.Fatalf("the body must not carry 0s active time for a capped Loop with a lastActiveStamp 3 minutes ago (I95): %s", line)
	}
	if !strings.HasPrefix(line, "- **Budget (active time):** 3m") {
		t.Fatalf("the body must carry the 3-minute active time (I95, capped): %s", line)
	}
}

var _ = Describe("I95 deliver description active time (envtest)", func() {
	ctx := context.Background()

	freshNS := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	// I95 (PRIMARY): a Succeeded Loop with NO spec.budget (the #166 shape)
	// and a known lastActiveStamp re-reconciles and the deliver description
	// ConfigMap's body carries the non-zero active time — not 0s. Same-Loop:
	// the spec mutates the Loop's status (the input) and re-reconciles,
	// asserting the ConfigMap's body is UPDATED (the I42c-style same-Loop
	// update).
	It("renders the deliver description ConfigMap body with the Loop's known active time (no cap)", func() {
		ns := freshNS("i95-desc")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		name := "i95task1"
		r := s6Succeeded(name, ns, inClusterRepoURL)

		// The S5->S6 shape: the Loop is Succeeded and delivery is expected,
		// but status.budget has a known stamp from a prior reconcile.
		// NO spec.budget (the #166 shape — the Loop never had a cap set).
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		if loop.Spec.Budget != nil {
			// Ensure the fixture has no spec.budget (the #166 shape).
			loop.Spec.Budget = nil
			Expect(k8sClient.Update(ctx, loop)).To(Succeed())
			loop = &coxv1alpha1.Loop{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		}
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
			"the body must carry the 3-minute active time measured from the stamp to the reconcile (I95, no cap): %s", line)
		Expect(line).ToNot(ContainSubstring(" 0s"),
			"the body must not carry 0s for a Loop with a lastActiveStamp 3 minutes ago and no spec.budget (I95, #166 shape): %s", line)
	})
})
