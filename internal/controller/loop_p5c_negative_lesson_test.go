package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxs "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/pkg/memorywrite"
)

// P2-1 (review #3 at 27014da): the negative lesson embeds the stall hash. The
// full 64-hex hash matches the static secrets screen's long-random-base64
// rule ([A-Za-z0-9+/]{64,}), so a seed that embeds the FULL hash would always
// be dropped by the operator's own screen — the operator's own seed never
// survives. This spec feeds a REAL 64-hex stall hash through negativeLesson
// (the seed composer) and then through the REAL static screen
// (memorywrite.Screen, the same function the memory-writer Job runs) and
// asserts the seed SURVIVES (the 12-char prefix is the fix).
//
// Mutation (the spec must FAIL): restore the full hash in negativeLesson
// (remove the 12-char truncation in loop_p5c_negative_lesson.go). The seed
// then embeds a 64-hex run, memorywrite.Screen drops it as
// "secret: long-random-base64", and this spec fails.
func TestNegativeLessonSurvivesStaticScreenWithReal64HexHash(t *testing.T) {
	// A real 64-char hex stall hash (the verify-failure hash shape: 64 hex
	// chars, e.g. a sha256 hex digest). It is high-entropy: a uniform hex
	// digest has ~4 bits/char and matches long-random-base64.
	const realHash = "9f2c4a7e1d3b5f8c0a2e4d6b8f1c3a5e7d9b2c4f6a8e0d1b3c5f7a9e2d4b6c8f"
	if len(realHash) != 64 {
		t.Fatalf("test fixture: the stall hash must be 64 hex chars, got %d", len(realHash))
	}
	loop := &coxs.Loop{}
	loop.Spec.Loop.StallAction = coxs.StallActionFail
	// The negative-lesson gate (negativeLessonEnabled →
	// loopFailedReasonStalled) reads the Loop's terminal Failed condition
	// (Type=Failed, Status=True, Reason=Stalled). The stall signature is the
	// LAST status.stallHistory entry's hash.
	loop.Status.Conditions = append(loop.Status.Conditions, metav1.Condition{
		Type:   string(coxs.LoopPhaseFailed),
		Status: metav1.ConditionTrue,
		Reason: "Stalled",
	})
	loop.Status.StallHistory = []coxs.StallEntry{{Hash: realHash}}

	seed, ok := negativeLesson(loop)
	if !ok {
		t.Fatalf("negativeLesson: a terminal Failed:Stalled Loop with a stall hash must seed, got no seed")
	}
	// The seed carries the 12-char prefix — NOT the full 64-hex hash (the
	// full hash is a secret-shape token the screen would drop).
	if strings.Contains(seed, realHash) {
		t.Fatalf("the seed must NOT embed the full 64-hex stall hash (the static screen would drop it as long-random-base64); seed=%q", seed)
	}
	if !strings.Contains(seed, realHash[:12]) {
		t.Fatalf("the seed must carry the 12-char hash prefix %q; seed=%q", realHash[:12], seed)
	}
	// The seed must be a single line the screen's shape rule admits.
	if strings.ContainsAny(seed, "\r\n") {
		t.Fatalf("the seed must be a single line; seed=%q", seed)
	}

	// Run the seed through the REAL static screen (the same function the
	// memory-writer Job runs before the curator). It must SURVIVE (survivor,
	// empty reason) — the operator's own seed must not be dropped by the
	// operator's own screen.
	survivors, reasons := memorywrite.Screen([]string{seed})
	if len(survivors) != 1 {
		t.Fatalf("the seed must survive the static screen (survivors=%d, reasons=%q); seed=%q", len(survivors), reasons, seed)
	}
	if reasons[0] != "" {
		t.Fatalf("the seed must not be dropped by any screen rule, got reason %q; seed=%q", reasons[0], seed)
	}
	// Sanity (positive control): a seed that embedded the FULL hash is
	// dropped by long-random-base64 — proving the screen actually rejects a
	// 64-hex run, so the survival above is meaningful.
	fullSeed := "stall: repeated verify failure " + realHash + " (see .coxswain/verify-result.json)"
	fullSurvivors, fullReasons := memorywrite.Screen([]string{fullSeed})
	if len(fullSurvivors) != 0 {
		t.Fatalf("positive control: a seed embedding the full 64-hex hash MUST be dropped by the screen (long-random-base64); seed=%q", fullSeed)
	}
	if fullReasons[0] != "secret: long-random-base64" {
		t.Fatalf("positive control: the full-hash seed must be dropped as 'secret: long-random-base64', got %q", fullReasons[0])
	}
}
