package controller

// P5c (PLAN-PHASE5.md slice P5c, PLAN.md:82): the negative lesson. A stall
// can seed a NEGATIVE lesson (configurable; default on for Fail/Pause, off
// for Continue). The negative lesson is the stall signature (the repeated
// verify-failure hash — the last status.stallHistory entry's hash), not the
// agent's text: it is operator-composed and is passed to the memory-writer
// Job as a seeded lesson IN ADDITION to the runner's proposed lessons (the
// Job's COX_SEEDED_LESSONS env). The seed is deterministic (the operator's
// code, not the agent's): the same stall always yields the same seeded text.
//
// The memory-writer Job (cmd/memory-write) receives the seed via the
// COX_SEEDED_LESSONS env var (a single-line JSON array — the operator
// serialises it). The Job prepends the seed to the lessons it reads from the
// workspace file, so the seed flows through the static rules + the curator
// exactly like the runner's lessons (the seed's sha256 is what lands in the
// audit trail; the seed's text is NOT the agent's text — the injection
// resistance applies: the curator can drop it).

import (
	"encoding/json"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// negativeLessonEnabled reports whether the operator seeds a negative lesson
// for this Loop (the plan: default on for Fail/Pause, off for Continue). A
// seed requires a terminal Failed:Stalled outcome (the eligibility trigger);
// a non-stalled terminal Loop never seeds. A Continue stall never reaches
// terminal (the loop keeps iterating), so a terminal Failed:Stalled Loop
// only happens under the Fail action — but the spec pins the Continue case
// explicitly: even when a Continue-stalled Loop is later marked
// Failed:Stalled (a budget-exceeded kill after a Continue fire), the seed is
// OFF (PLAN.md:82's "off for Continue").
func negativeLessonEnabled(loop *coxv1alpha1.Loop) bool {
	if !loopFailedReasonStalled(loop) {
		return false
	}
	return loop.Spec.Loop.StallAction != coxv1alpha1.StallActionContinue
}

// negativeLesson returns the seeded negative lesson for a stalled Loop (the
// stall signature — the repeated verify-failure hash — as a single line the
// static write policy admits: it names the verify-result file path and the
// failure hash). Returns ("", false) when the Loop does not qualify (not a
// terminal Failed:Stalled, or a Continue stall, or no recorded failure hash).
//
// The failure hash is the LAST status.stallHistory entry's hash (the stall
// detector's repeated identical verify failures; appendStallEntry dedups by
// jobName so the ring's tail is the most recent failing verify run). A
// terminal Failed:Stalled Loop always has at least one stall entry (the
// fire that set the Failed condition ran the gate, which appended the entry)
// — but the guard returns ("", false) on an empty ring rather than panicking
// (a malformed status must not crash the reconcile).
func negativeLesson(loop *coxv1alpha1.Loop) (string, bool) {
	if !negativeLessonEnabled(loop) {
		return "", false
	}
	var sig string
	if n := len(loop.Status.StallHistory); n > 0 {
		sig = loop.Status.StallHistory[n-1].Hash
	}
	if sig == "" {
		return "", false
	}
	// A single line the static write policy admits: a file-path reference
	// (.coxswain/verify-result.json) + a SHORT hash prefix (12 hex chars).
	// The "stall —" marker distinguishes it from the agent's lessons in the
	// audit trail.
	//
	// P2-1: the full 64-hex stall hash matches the static secrets screen's
	// long-random-base64 rule ([A-Za-z0-9+/]{64,}), so the operator's own
	// seed would ALWAYS be dropped. A 12-char prefix is short enough to pass
	// the screen and long enough to be a useful identifier in the audit
	// trail (12 hex = 48 bits — more than enough to distinguish one stall
	// from another in practice).
	hashPrefix := sig
	if len(hashPrefix) > 12 {
		hashPrefix = hashPrefix[:12]
	}
	return "stall: repeated verify failure " + hashPrefix + " (see .coxswain/verify-result.json)", true
}

// seededLessonsEnvValue serialises the seeded lessons for the memory-writer
// Job's COX_SEEDED_LESSONS env var (a JSON array of single-line strings;
// "[]" when the operator seeds nothing — the Job treats an empty array as
// "no seed"). The seed is operator-composed: the curator's injection
// resistance still applies (the curator reads the seeded text as untrusted
// and can drop it — the plan's mitigation (a)+(b) cover the seed: the static
// rules run first, and the curator's strict yes/no gates the write).
func seededLessonsEnvValue(lessons []string) string {
	if len(lessons) == 0 {
		return "[]"
	}
	out, err := json.Marshal(lessons)
	if err != nil {
		// A marshalling failure is impossible for []string; the caller must
		// treat this as "no seed" rather than a broken Job env.
		return "[]"
	}
	return string(out)
}
