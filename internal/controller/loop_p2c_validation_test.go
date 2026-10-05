// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// you may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

// P2c (TDD-PHASE2 P2c): the budget / stall-history / pausedReason API.
//
// Spec 1: the effective-config seam (EffectiveStallConfig / EffectiveBudget)
//
// applies the PLAN defaults (stallAfter nil/zero -> 3, stallAction empty ->
// Fail, onExceeded empty -> Pause) — the P2d consumer reads these, so a
// CRD with no `loop`/`budget` blocks still yields the defaults.
//
// Spec 2-4: bad enum / bad minimum / bad decimal string are rejected at
// admission by the CRD's enum + minimum + CEL-pattern rules.
//
// Spec 5: an existing Loop with no new fields reconciles exactly as before
// (the new fields are additive; the existing suspend / iterate / decision
// specs keep passing unchanged).

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// P2c fixture values (goconst: they recur across the specs).
const (
	p2cGoal     = "P2c budget and stall API"
	p2cLoopRepo = "https://example.com/repo.git"
)

var _ = Describe("P2c: budget / stall history / pausedReason API", func() {
	ctx := context.Background()

	newNamespace := func() string {
		ns := "p2c-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	// baseLoop is a minimal unstructured Loop (no budget, no stall fields)
	// so the specs set exactly the field under test.
	baseLoop := func(name, ns string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "coxswain.wattu.com/v1alpha1",
			"kind":       "Loop",
			"metadata":   map[string]any{"name": name, "namespace": ns},
			"spec": map[string]any{
				"goal":      p2cGoal,
				"workspace": map[string]any{"repo": p2cLoopRepo},
				"verify":    map[string]any{"acceptanceChecks": []any{"go test ./..."}},
			},
		}}
	}

	It("1: defaults — the effective config reads stallAfter=3, stallAction=Fail, onExceeded=Pause", func() {
		ns := newNamespace()
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// A Loop with no spec.budget and no spec.loop.stall* is admitted.
		u := baseLoop("p2c-defaults", ns)
		Expect(k8sClient.Create(ctx, u)).To(Succeed())

		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "p2c-defaults"}, got)).To(Succeed())

		// The CRD defaulting: spec.loop is present ({} default) with
		// stallAction defaulted to Fail; stallAfter is absent (the operator
		// seam supplies 3, the CRD does not).
		Expect(got.Spec.Loop.StallAction).To(Equal(coxv1alpha1.StallActionFail),
			"the CRD must default spec.loop.stallAction to Fail")
		Expect(got.Spec.Loop.StallAfter).To(BeNil(),
			"stallAfter is not CRD-defaulted (nil reads as 3 at the operator seam)")
		Expect(got.Spec.Budget).To(BeNil(), "no budget block means no budget caps")

		// The P2d seam: the effective config the decision slices read.
		stall := coxv1alpha1.EffectiveStallConfig(&got.Spec.Loop)
		Expect(stall.StallAfter).To(Equal(int32(3)), "nil stallAfter must read as 3 (PLAN default)")
		Expect(stall.StallAction).To(Equal(coxv1alpha1.StallActionFail), "empty stallAction must read as Fail")

		budget := coxv1alpha1.EffectiveBudget(got.Spec.Budget)
		Expect(budget.Budget).To(BeNil(), "nil spec.budget means no budget caps")

		// Explicit values are honoured over the defaults.
		three := int32(3)
		explicit := &coxv1alpha1.LoopSettings{StallAfter: &three, StallAction: coxv1alpha1.StallActionPause}
		Expect(coxv1alpha1.EffectiveStallConfig(explicit)).To(Equal(coxv1alpha1.StallConfig{
			StallAfter: 3, StallAction: coxv1alpha1.StallActionPause,
		}))
		zero := int32(0)
		zeroCfg := &coxv1alpha1.LoopSettings{StallAfter: &zero}
		Expect(coxv1alpha1.EffectiveStallConfig(zeroCfg).StallAfter).To(Equal(int32(3)),
			"a zero stallAfter reads as the default 3")
		maxTokens := int64(1000)
		explicitBudget := &coxv1alpha1.BudgetConfig{MaxTokens: &maxTokens}
		effBudget := coxv1alpha1.EffectiveBudget(explicitBudget)
		Expect(effBudget.Budget).ToNot(BeNil())
		Expect(effBudget.OnExceeded).To(Equal(coxv1alpha1.BudgetExceededActionPause),
			"an omitted onExceeded defaults to Pause")
		// A sub-second maxWallClock must be rejected by the operator seam
		// (the CEL pattern admits it; the "≥1s when set" rule is the seam's).
		_, werr := coxv1alpha1.ParseMaxWallClock("500ms")
		Expect(werr).To(HaveOccurred(), "a sub-second maxWallClock must be rejected (≥1s when set)")
		d, werr := coxv1alpha1.ParseMaxWallClock("1h30m")
		Expect(werr).ToNot(HaveOccurred())
		Expect(d).To(Equal(90 * time.Minute))
		_, werr = coxv1alpha1.ParseMaxWallClock("")
		Expect(werr).ToNot(HaveOccurred(), "an empty maxWallClock means no wall-clock cap")
		c, cerr := coxv1alpha1.ParseDecimalString("0.5")
		Expect(cerr).ToNot(HaveOccurred())
		Expect(c).To(Equal(0.5))
		_, cerr = coxv1alpha1.ParseDecimalString("1.2.3")
		Expect(cerr).To(HaveOccurred(), "a non-decimal string must be rejected")
	})

	It("2: a bad stallAction enum is rejected at admission (CEL)", func() {
		ns := newNamespace()
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		u := baseLoop("p2c-badenum", ns)
		Expect(unstructured.SetNestedField(u.Object, "Mangle", "spec", "loop", "stallAction")).To(Succeed())
		err := k8sClient.Create(ctx, u)
		Expect(err).To(HaveOccurred(), "stallAction Mangle must be rejected by the enum validation")
	})

	It("3: stallAfter 0 is rejected at admission (CEL minimum)", func() {
		ns := newNamespace()
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		u := baseLoop("p2c-badstallafter", ns)
		Expect(unstructured.SetNestedField(u.Object, int64(0), "spec", "loop", "stallAfter")).To(Succeed())
		err := k8sClient.Create(ctx, u)
		Expect(err).To(HaveOccurred(), "stallAfter 0 must be rejected by the minimum-1 validation")
	})

	It("4: maxCostUsd 'abc' is rejected at admission (CEL pattern)", func() {
		ns := newNamespace()
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		u := baseLoop("p2c-badcost", ns)
		Expect(unstructured.SetNestedField(u.Object, "abc", "spec", "budget", "maxCostUsd")).To(Succeed())
		err := k8sClient.Create(ctx, u)
		Expect(err).To(HaveOccurred(), "maxCostUsd abc must be rejected by the decimal-string pattern")

		// The modelPrices values are pattern-checked too.
		u2 := baseLoop("p2c-badprice", ns)
		Expect(unstructured.SetNestedField(u2.Object, "1.2.3", "spec", "budget", "modelPrices", "promptUsdPerMtok")).To(Succeed())
		err = k8sClient.Create(ctx, u2)
		Expect(err).To(HaveOccurred(), "a malformed promptUsdPerMtok must be rejected by the decimal-string pattern")
	})

	It("5: no budget/stall fields — an existing Loop reconciles exactly as before", func() {
		ns := newNamespace()
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// A pre-P2c-shaped Loop (no new fields) with suspend=true reconciles
		// exactly as the S1 specs do: the sandbox is suspended, the phase
		// does NOT enter Paused (that is P2f), and nothing about the new
		// fields affects the decision.
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "p2c-additive", Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      p2cGoal,
				Workspace: coxv1alpha1.Workspace{Repo: p2cLoopRepo},
				Suspend:   true,
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "p2c-additive"}})
		Expect(err).ToNot(HaveOccurred(), "a pre-P2c Loop must reconcile without error")

		// The sandbox is suspended (the S1 behaviour, unchanged).
		sbx := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "p2c-additive-sandbox"}, sbx)).To(Succeed(),
			"the sandbox <loop>-sandbox must exist")
		Expect(sbx.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"the existing suspend behaviour must be unchanged")

		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "p2c-additive"}, got)).To(Succeed())
		// The new status fields are simply absent (nil) — additive.
		Expect(got.Status.Budget).To(BeNil())
		Expect(got.Status.StallHistory).To(BeEmpty())
		Expect(got.Status.PausedReason).To(BeEmpty())
		Expect(got.Status.PausedFrom).To(BeEmpty())
	})
})

// P2c status round-trip: every new status field is written by the client and
// survives a Get (structural-scheme round-trip — a field dropped from the CRD
// schema would be pruned and the assertions below would fail).
var _ = Describe("P2c: status field round-trip", func() {
	ctx := context.Background()

	It("round-trips status.budget, status.stallHistory, pausedFrom and pausedReason", func() {
		ns := "p2c-rt-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "coxswain.wattu.com/v1alpha1",
			"kind":       "Loop",
			"metadata":   map[string]any{"name": "p2c-rt", "namespace": ns},
			"spec": map[string]any{
				"goal":      p2cGoal,
				"workspace": map[string]any{"repo": p2cLoopRepo},
			},
			"status": map[string]any{
				"phase":        "Implementing",
				"pausedFrom":   "Implementing",
				"pausedReason": "Budget",
				"budget": map[string]any{
					"promptTokens":          int64(1000),
					"completionTokens":      int64(500),
					"requests":              int64(42),
					"unmeteredRequests":     int64(1),
					"costUsd":               "0.0021",
					"activeSeconds":         int64(3600),
					"exceeded":              true,
					"exceededReason":        "Tokens",
					"lastBootID":            "boot-abc123",
					"lastPromptTokens":      int64(1000),
					"lastCompletionTokens":  int64(500),
					"lastRequests":          int64(42),
					"lastUnmeteredRequests": int64(1),
					"bootIDChanged":         true,
					"lastActiveStamp":       "2026-10-04T12:00:00Z",
				},
				"stallHistory": []any{
					map[string]any{
						"iteration":            2,
						"jobName":              "p2c-rt-verify-2",
						"hash":                 "9f86d081884c7d65",
						"normalisationVersion": "v1",
						"check":                "go test ./...",
						"at":                   "2026-10-04T11:59:00Z",
					},
				},
			},
		}}

		// (Status updates need the stored resourceVersion — the create response
		// is discarded, so re-Get it.)
		Expect(k8sClient.Create(ctx, u)).To(Succeed())

		stored := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "p2c-rt"}, stored)).To(Succeed())

		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "p2c-rt", Namespace: ns, ResourceVersion: stored.ResourceVersion},
			Spec:       coxv1alpha1.LoopSpec{Goal: p2cGoal, Workspace: coxv1alpha1.Workspace{Repo: p2cLoopRepo}},
			Status: coxv1alpha1.LoopStatus{
				Phase:        coxv1alpha1.LoopPhaseImplementing,
				PausedFrom:   coxv1alpha1.LoopPhaseImplementing,
				PausedReason: coxv1alpha1.PausedReasonBudget,
				Budget: &coxv1alpha1.BudgetStatus{
					PromptTokens:          1000,
					CompletionTokens:      500,
					Requests:              42,
					UnmeteredRequests:     1,
					CostUsd:               "0.0021",
					ActiveSeconds:         3600,
					Exceeded:              true,
					ExceededReason:        coxv1alpha1.BudgetExceededTokens,
					LastBootID:            "boot-abc123",
					LastPromptTokens:      1000,
					LastCompletionTokens:  500,
					LastRequests:          42,
					LastUnmeteredRequests: 1,
					BootIDChanged:         true,
					LastActiveStamp:       &metav1.Time{Time: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)},
				},
				StallHistory: []coxv1alpha1.StallEntry{{
					Iteration:            2,
					JobName:              "p2c-rt-verify-2",
					Hash:                 "9f86d081884c7d65",
					NormalisationVersion: "v1",
					Check:                "go test ./...",
					At:                   metav1.Time{Time: time.Date(2026, 10, 4, 11, 59, 0, 0, time.UTC)},
				}},
			},
		}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed(),
			"the status subresource must accept the P2c status fields")

		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "p2c-rt"}, got)).To(Succeed())

		b := got.Status.Budget
		Expect(b).ToNot(BeNil(), "status.budget must survive the round-trip")
		Expect(b.PromptTokens).To(Equal(int64(1000)))
		Expect(b.CompletionTokens).To(Equal(int64(500)))
		Expect(b.Requests).To(Equal(int64(42)))
		Expect(b.UnmeteredRequests).To(Equal(int64(1)), "unmeteredRequests (the cumulative count, item 3) must survive")
		Expect(b.CostUsd).To(Equal("0.0021"))
		Expect(b.ActiveSeconds).To(Equal(int64(3600)))
		Expect(b.Exceeded).To(BeTrue())
		Expect(b.ExceededReason).To(Equal(coxv1alpha1.BudgetExceededTokens))
		Expect(b.LastBootID).To(Equal("boot-abc123"), "the bootID baseline (item 2) must survive")
		Expect(b.LastPromptTokens).To(Equal(int64(1000)))
		Expect(b.LastCompletionTokens).To(Equal(int64(500)))
		Expect(b.LastRequests).To(Equal(int64(42)))
		Expect(b.LastUnmeteredRequests).To(Equal(int64(1)))
		Expect(b.BootIDChanged).To(BeTrue())
		Expect(b.LastActiveStamp).ToNot(BeNil(), "lastActiveStamp (item E) must survive")
		Expect(b.LastActiveStamp.Time).To(BeTemporally("==", time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)))

		Expect(got.Status.StallHistory).To(HaveLen(1))
		e := got.Status.StallHistory[0]
		Expect(e.Iteration).To(Equal(2))
		Expect(e.JobName).To(Equal("p2c-rt-verify-2"), "jobName (the dedup key, item 6) must survive")
		Expect(e.Hash).To(Equal("9f86d081884c7d65"))
		Expect(e.NormalisationVersion).To(Equal("v1"), "normalisationVersion must survive")
		Expect(e.Check).To(Equal("go test ./..."))
		Expect(e.At.Time).To(BeTemporally("==", time.Date(2026, 10, 4, 11, 59, 0, 0, time.UTC)))

		Expect(got.Status.PausedFrom).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		Expect(got.Status.PausedReason).To(Equal(coxv1alpha1.PausedReasonBudget),
			"pausedReason (item 4) must survive the round-trip")
	})
})
