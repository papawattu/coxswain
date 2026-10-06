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

// Unit tests for the PURE stall decision (P2e item 2). The envtest specs
// (loop_p2e_stall_envtest_test.go) drive the gate through the reconcile; these
// pin the decision arithmetic in isolation (the gate-mutation coverage the
// plan calls for, in the scratch worktree).
package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

const (
	p2eStallNS = "ns-a"
	// The seam's fixed check output (the stall gate's hash source in the
	// gate-level unit tests — the envtest specs use the real
	// terminationMessage; these pin the arithmetic in isolation with a stable
	// raw value so the normalised hash is deterministic).
	p2eStallSeamOutput = "out"
	// Seeded history JobNames for the gate-level unit tests (a DIFFERENT-Job
	// entry that makes the just-appended verify Job a NEW Job — the per-Job
	// sticky rule fires only on a different-JobName trailing entry).
	p2eStallSeededJob0 = "lp-verify-0"
	p2eStallSeededJob1 = "lp-verify-1"
)

// TestStallDecisionFiresAfterNConsecutive: N consecutive identical hashes
// (same version) fire; N-1 do not. The run counts the just-appended entry +
// the matching trailing history.
func TestStallDecisionFiresAfterNConsecutive(t *testing.T) {
	mkEntry := func(h, v string) coxv1alpha1.StallEntry {
		return coxv1alpha1.StallEntry{Hash: h, NormalisationVersion: v}
	}
	cases := []struct {
		name       string
		history    []coxv1alpha1.StallEntry
		newEntry   coxv1alpha1.StallEntry
		stallAfter int32
		wantFire   bool
		wantRun    int
	}{
		{"one of N=3", nil, mkEntry("h", "v1"), 3, false, 1},
		{"two of N=3", []coxv1alpha1.StallEntry{mkEntry("h", "v1")}, mkEntry("h", "v1"), 3, false, 2},
		{"three of N=3 fires", []coxv1alpha1.StallEntry{mkEntry("h", "v1"), mkEntry("h", "v1")}, mkEntry("h", "v1"), 3, true, 3},
		{"N=2 fires at two", []coxv1alpha1.StallEntry{mkEntry("h", "v1")}, mkEntry("h", "v1"), 2, true, 2},
		{"N=1 fires at one", nil, mkEntry("h", "v1"), 1, true, 1},
		{"different hash resets", []coxv1alpha1.StallEntry{mkEntry("h", "v1"), mkEntry("g", "v1")}, mkEntry("f", "v1"), 3, false, 1},
		{"version change resets", []coxv1alpha1.StallEntry{mkEntry("h", "v1")}, mkEntry("h", "v2"), 3, false, 1},
		{"run does not cross a different hash", []coxv1alpha1.StallEntry{mkEntry("g", "v1"), mkEntry("h", "v1")}, mkEntry("h", "v1"), 2, true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fired, run := stallDecision(tc.history, tc.newEntry, tc.stallAfter)
			if fired != tc.wantFire {
				t.Fatalf("fire = %v, want %v", fired, tc.wantFire)
			}
			if run != tc.wantRun {
				t.Fatalf("run = %d, want %d", run, tc.wantRun)
			}
		})
	}
}

// TestStallDecisionZeroStallAfterDefaultsToThree: a resolved stallAfter of 0
// (malformed — the CRD rejects <1) is treated as 3 (the code default) so a
// single entry never fires.
func TestStallDecisionZeroStallAfterDefaultsToThree(t *testing.T) {
	e := coxv1alpha1.StallEntry{Hash: "h", NormalisationVersion: "v1"}
	if fired, _ := stallDecision(nil, e, 0); fired {
		t.Fatalf("a single entry fired with stallAfter=0 (should default to 3)")
	}
}

// TestAppendStallEntryDedupsByJobName: a re-read of the same verify Job
// (same jobName) appends NO entry (item 6).
func TestAppendStallEntryDedupsByJobName(t *testing.T) {
	loop := &coxv1alpha1.Loop{}
	loop.Status.Iteration = 1
	appendStallEntry(loop, "lp-verify-1", "h1", "check-0", &metav1.Time{Time: time.Now()})
	appendStallEntry(loop, "lp-verify-1", "h1", "check-0", &metav1.Time{Time: time.Now()}) // dedup
	if len(loop.Status.StallHistory) != 1 {
		t.Fatalf("history len = %d, want 1 (dedup by jobName)", len(loop.Status.StallHistory))
	}
	// A NEW iteration (new jobName) appends.
	loop.Status.Iteration = 2
	appendStallEntry(loop, "lp-verify-2", "h1", "check-0", &metav1.Time{Time: time.Now()})
	if len(loop.Status.StallHistory) != 2 {
		t.Fatalf("history len = %d, want 2", len(loop.Status.StallHistory))
	}
}

// TestAppendStallEntryCapsRing: the ring holds the last 10 entries (the older
// ones evict).
func TestAppendStallEntryCapsRing(t *testing.T) {
	loop := &coxv1alpha1.Loop{}
	for i := 1; i <= 12; i++ {
		loop.Status.Iteration = i
		appendStallEntry(loop, "lp-verify-"+itoa(i), "h"+itoa(i), "check-0", &metav1.Time{Time: time.Now()})
	}
	if len(loop.Status.StallHistory) != 10 {
		t.Fatalf("history len = %d, want 10 (ring cap)", len(loop.Status.StallHistory))
	}
	// The oldest (verify-1, verify-2) evicted; the newest (verify-12) present.
	if loop.Status.StallHistory[0].JobName != "lp-verify-3" {
		t.Fatalf("oldest = %s, want lp-verify-3", loop.Status.StallHistory[0].JobName)
	}
	if loop.Status.StallHistory[9].JobName != "lp-verify-12" {
		t.Fatalf("newest = %s, want lp-verify-12", loop.Status.StallHistory[9].JobName)
	}
}

// TestStallDecisionPerJobSticky: the fire rule is per NEW verify Job (item
// C's consistent model). The history's trailing entry with the SAME jobName
// as the just-appended entry is NOT a new Job (a re-read, a requeue, a stale
// read) — the detector must not re-evaluate on it (no fire, no second
// Event). Mutation: firing whenever k >= stallAfter on ANY reconcile (drop
// the per-Job rule) must make spec 11 FAIL (a re-fire on a re-reconcile).
//
// The reviewer-strengthened case (M6): call stallDecision TWICE with the SAME
// JobName where BOTH calls have k >= stallAfter. The first (a new Job at the
// threshold) fires; the second (a re-read of that same Job, k still >=
// stallAfter) must return false (no re-fire). Dropping the per-Job sticky
// makes the second call fire — the mutation FAILS this test.
func TestStallDecisionPerJobSticky(t *testing.T) {
	mkEntry := func(jobName, h, v string) coxv1alpha1.StallEntry {
		return coxv1alpha1.StallEntry{JobName: jobName, Hash: h, NormalisationVersion: v}
	}
	// A NEW job (verify-2) after one identical entry (verify-1), N=2: fires.
	if fired, _ := stallDecision(
		[]coxv1alpha1.StallEntry{mkEntry("lp-verify-1", "h", "v1")},
		mkEntry("lp-verify-2", "h", "v1"), 2); !fired {
		t.Fatalf("a new Job at k=2 (N=2) must fire")
	}
	// The SAME job re-read (a requeue before the iterate advances): the
	// just-appended entry's jobName matches the trailing history entry — no
	// new Job, no re-evaluation (no fire, even though k would be 2).
	if fired, _ := stallDecision(
		[]coxv1alpha1.StallEntry{mkEntry("lp-verify-1", "h", "v1")},
		mkEntry("lp-verify-1", "h", "v1"), 2); fired {
		t.Fatalf("a re-read of the SAME Job must not re-fire (per-Job rule)")
	}
	// A re-read on an already-fired run (N=1, k=1, trailing entry same job)
	// must not fire either (the terminal Failed case: no second Event).
	if fired, _ := stallDecision(
		[]coxv1alpha1.StallEntry{mkEntry("lp-verify-1", "h", "v1")},
		mkEntry("lp-verify-1", "h", "v1"), 1); fired {
		t.Fatalf("a re-read of the SAME Job on a fired run must not re-fire (per-Job rule)")
	}
	// M6 (reviewer): two calls, SAME JobName, k >= stallAfter BOTH times.
	// Call 1: a NEW Job (lp-verify-2) after one identical entry (lp-verify-1),
	// k=2, N=2 → fires (k >= stallAfter).
	if fired, _ := stallDecision(
		[]coxv1alpha1.StallEntry{mkEntry("lp-verify-1", "h", "v1")},
		mkEntry("lp-verify-2", "h", "v1"), 2); !fired {
		t.Fatalf("M6 call 1: a new Job at k=2 (N=2) must fire (k >= stallAfter)")
	}
	// Call 2: a RE-READ of that same Job (the trailing history entry is now
	// lp-verify-2, the same JobName as the just-appended entry), k=2, N=2 —
	// k is STILL >= stallAfter, but it is not a NEW Job → must NOT fire.
	if fired, _ := stallDecision(
		[]coxv1alpha1.StallEntry{mkEntry("lp-verify-1", "h", "v1"), mkEntry("lp-verify-2", "h", "v1")},
		mkEntry("lp-verify-2", "h", "v1"), 2); fired {
		t.Fatalf("M6 call 2: a re-read of the SAME Job (k still >= stallAfter) must NOT re-fire (per-Job rule)")
	}
}

// TestResolveStallAfterConfigMap: the effective stallAfter resolution
// (plan spec 12's precedence: Loop field > ConfigMap > built-in 3). The
// ConfigMap is coxswain-stall-defaults in the operator namespace (nil
// OperatorNamespace defaults to coxswain-system); a missing/malformed
// ConfigMap falls back to the built-in 3 (the stall detector is NOT
// fail-closed on a missing ConfigMap).
func TestResolveStallAfterConfigMap(t *testing.T) {
	ctx := context.Background()
	loop := &coxv1alpha1.Loop{}
	loop.Namespace = p2eStallNS

	// No spec field, no ConfigMap (nil client — the read is skipped): the
	// built-in default 3.
	if got := resolveStallAfter(ctx, nil, loop); got != 3 {
		t.Fatalf("no ConfigMap (nil client): stallAfter = %d, want 3", got)
	}
}

// TestApplyStallGateDecidesOnTerminalFailure: the stall gate DECIDES on a
// terminal verify failure (a check container Terminated non-zero — the B3
// evidence). The terminal gate that keeps an IN-PROGRESS check (still
// Running) from reaching this gate is the CALLER's: verifyOutcome returns
// (verifyNoDecision, requeue=true) for a non-terminated check, and
// applyVerifyOutcome requeues before it reaches the verifyIterate branch that
// calls this gate (M4: the redundant inner checkNotTerminated was removed —
// the caller's in-progress requeue is the real terminal gate, so a Running
// check never reaches this gate; the gate is reached only on a terminal
// failure). This test pins the supported contract at the gate level:
//   - below the threshold (k < N) the gate decides but does NOT fire and
//     appends the terminal-failure StallEntry (the B3 evidence — the gate
//     records the failure even when it does not yet fire), leaving the phase
//     unchanged;
//   - at the threshold (k == N, a NEW Job after one or more identical
//     entries) the gate fires; stallAction=Fail → the phase is Failed and
//     the Stalled condition is set.
//
// A fresh loop (empty history, or a same-JobName trailing entry) does NOT
// fire at the threshold — that is the per-Job sticky rule (a re-read of the
// same Job is not a new Job), not a gate bug. Seeding a DIFFERENT-JobName
// trailing entry makes the just-appended entry a NEW Job that can fire.
func TestApplyStallGateDecidesOnTerminalFailure(t *testing.T) {
	// The check output's normalised hash (the seam returns "out").
	_, h := normalizeCheckOutput(p2eStallSeamOutput)
	r := &LoopReconciler{readCheckOutput: func(*corev1.Pod, string) (string, bool) { return p2eStallSeamOutput, true }}
	terminatedState := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode:   1,
		FinishedAt: metav1.Now(),
	}}
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: s5aCheck0, State: terminatedState,
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode:   1,
						Message:    "out",
						FinishedAt: metav1.Now(),
					}}},
			},
		},
	}

	// Below the threshold: seed ONE different-Job entry (the 1st identical
	// failure), the just-appended entry is a NEW Job at k=2, N=3 → no fire.
	{
		n := int32(3)
		loop := &coxv1alpha1.Loop{}
		loop.Name = "lp"
		loop.Namespace = p2eStallNS
		loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		loop.Status.Iteration = 1
		loop.Spec.Loop.StallAfter = &n
		loop.Spec.Loop.StallAction = coxv1alpha1.StallActionFail
		loop.Status.StallHistory = append(loop.Status.StallHistory,
			coxv1alpha1.StallEntry{JobName: p2eStallSeededJob0, Hash: h, NormalisationVersion: "v1"})
		if r.applyStallGate(context.Background(), loop, pod, s5aCheck0) {
			t.Fatalf("k=2 < N=3 must NOT fire (the gate decides but does not fire below the threshold)")
		}
		// The terminal-failure StallEntry is appended (B3 evidence): the ring
		// holds the seeded verify-0 + the just-appended verify-1.
		if len(loop.Status.StallHistory) != 2 {
			t.Fatalf("a terminal verify failure must append a StallEntry (B3 evidence), got %d entries", len(loop.Status.StallHistory))
		}
		if loop.Status.Phase != coxv1alpha1.LoopPhaseVerifying {
			t.Fatalf("a non-fire must not change the phase, got %s", loop.Status.Phase)
		}
	}

	// At the threshold: seed TWO different-Job entries (the 1st + 2nd
	// identical failures), the just-appended entry is a NEW Job at k=3 ==
	// N=3 → fires (stallAction=Fail → Failed + Stalled).
	{
		n := int32(3)
		loop := &coxv1alpha1.Loop{}
		loop.Name = "lp"
		loop.Namespace = p2eStallNS
		loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
		loop.Status.Iteration = 2
		loop.Spec.Loop.StallAfter = &n
		loop.Spec.Loop.StallAction = coxv1alpha1.StallActionFail
		loop.Status.StallHistory = append(loop.Status.StallHistory,
			coxv1alpha1.StallEntry{JobName: p2eStallSeededJob0, Hash: h, NormalisationVersion: "v1"},
			coxv1alpha1.StallEntry{JobName: p2eStallSeededJob1, Hash: h, NormalisationVersion: "v1"})
		if !r.applyStallGate(context.Background(), loop, pod, s5aCheck0) {
			t.Fatalf("a new Job at k=N (N=3) must fire the stall gate")
		}
		if loop.Status.Phase != coxv1alpha1.LoopPhaseFailed {
			t.Fatalf("stallAction=Fail must set the phase to Failed, got %s", loop.Status.Phase)
		}
		stalled := false
		for i := range loop.Status.Conditions {
			if loop.Status.Conditions[i].Type == string(coxv1alpha1.StalledCondition) {
				stalled = loop.Status.Conditions[i].Status == metav1.ConditionTrue
				break
			}
		}
		if !stalled {
			t.Fatalf("stallAction=Fail must set the Stalled condition to True, got %v", loop.Status.Conditions)
		}
	}
}

// TestApplyStallGateContinueLeavesPhaseUnchanged: a stallAction=Continue fire
// (M11) must NOT change the phase — the gate sets the Stalled condition and
// returns false (keep iterating); the CALLER proceeds to the iterate, which
// is what sets the phase to Implementing. This is the gate-level contract the
// envtest spec 5 cannot isolate (spec 5 reads the FINAL phase, which the
// caller's iterate sets regardless of what the gate does). Calling the gate
// DIRECTLY at the threshold pins it: the phase the gate LEAVES is the phase
// it was given (Verifying — unchanged), NOT Paused.
//
// The loop is seeded with a pre-existing DIFFERENT-JobName entry (the real
// "Nth consecutive identical failure" state): the per-Job sticky rule fires
// only when the trailing history entry's JobName differs from the just-
// appended one, so a fresh loop (empty history, or a same-JobName trailing
// entry) does NOT fire at the threshold — that is the per-Job rule, not a
// gate bug. Seeding a different JobName (verify-0) makes the new verify-1
// job the kth consecutive failure that fires.
//
// Mutation: a Continue fire that sets the phase to Paused (M11) must FAIL this
// test (the phase is no longer Verifying/unchanged).
func TestApplyStallGateContinueLeavesPhaseUnchanged(t *testing.T) {
	n := int32(2) // threshold: a new Job after one identical entry fires
	// The check output's normalised hash (the seam returns "out").
	_, h := normalizeCheckOutput(p2eStallSeamOutput)
	r := &LoopReconciler{readCheckOutput: func(*corev1.Pod, string) (string, bool) { return p2eStallSeamOutput, true }}

	terminatedState := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode:   1,
		FinishedAt: metav1.Now(),
	}}
	loop := &coxv1alpha1.Loop{}
	loop.Name = "lp"
	loop.Namespace = p2eStallNS
	loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying // the phase the gate is given
	loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseVerifying
	loop.Status.Iteration = 1
	loop.Spec.Loop.StallAfter = &n
	loop.Spec.Loop.StallAction = coxv1alpha1.StallActionContinue
	// Seed the history with a DIFFERENT JobName (verify-0) carrying the same
	// hash — the 1st consecutive identical failure. The just-appended entry is
	// verify-1 (a NEW Job), k=2 == N=2 → fires.
	loop.Status.StallHistory = append(loop.Status.StallHistory,
		coxv1alpha1.StallEntry{JobName: p2eStallSeededJob0, Hash: h, NormalisationVersion: "v1"})
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: s5aCheck0, State: terminatedState,
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode:   1,
						Message:    "out",
						FinishedAt: metav1.Now(),
					}}},
			},
		},
	}
	// A new Job at k=N fires (stallAction=Continue). The gate returns false
	// for a Continue fire (keep iterating — the caller proceeds to the iterate
	// which sets the phase to Implementing); it returns true ONLY for a
	// terminal outcome (Fail/Pause). So the fire is detected by the Stalled
	// condition being set, NOT by the return value.
	_ = r.applyStallGate(context.Background(), loop, pod, s5aCheck0)
	// The gate LEAVES the phase unchanged (Verifying — the phase it was
	// given). A Continue fire does NOT set Paused (that is the caller's
	// iterate, which the gate defers to by returning false).
	if loop.Status.Phase != coxv1alpha1.LoopPhaseVerifying {
		t.Fatalf("a Continue fire must leave the phase unchanged (Verifying), got %s (M11: must not set Paused)", loop.Status.Phase)
	}
	if loop.Status.DesiredPhase != coxv1alpha1.LoopPhaseVerifying {
		t.Fatalf("a Continue fire must leave the desired phase unchanged, got %s", loop.Status.DesiredPhase)
	}
	// The Stalled condition IS set (the detector's record of the fire).
	stalled := false
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == string(coxv1alpha1.StalledCondition) {
			stalled = loop.Status.Conditions[i].Status == metav1.ConditionTrue
			break
		}
	}
	if !stalled {
		t.Fatalf("a Continue fire must set the Stalled condition to True, got %v", loop.Status.Conditions)
	}
}

// TestDefaultReadCheckOutput pins the check-output read's field order: a
// non-restarted check init (restartCount 0, the real production path) carries
// its terminationMessage in state.terminated.message, NOT lastState -- a real
// kubelet only populates lastState for a RESTARTED container. The original
// code read ONLY lastState, so it found nothing for a non-restarted check and
// the stall gate went inert (no StallEntry in production -- the P2e kind run
// exposed this). Reading state.terminated.message FIRST (falling back to
// lastState) is the fix. Cases:
//
//	(a) state.terminated.message set, NO lastState -> returns (message, true).
//	(b) restarted container: ONLY lastState.terminated.message -> returns (message, true).
//	(c) no terminated record at all -> returns ("", false).
func TestDefaultReadCheckOutput(t *testing.T) {
	r := &LoopReconciler{}
	const check = "check-0"

	// (a) the production path: a non-restarted check that just terminated.
	// The message is in state.terminated.message; lastState is EMPTY.
	podA := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
		{Name: check, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "real-output"}}},
	}}}
	if got, ok := r.defaultReadCheckOutput(podA, check); !ok || got != "real-output" {
		t.Fatalf("(a) a non-restarted check's state.terminated.message must be read: got (%q, %v)", got, ok)
	}

	// (b) a restarted container: ONLY lastState.terminated.message is set
	// (state.terminated is empty because the container restarted).
	podB := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
		{Name: check, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "last-output"}}},
	}}}
	if got, ok := r.defaultReadCheckOutput(podB, check); !ok || got != "last-output" {
		t.Fatalf("(b) a restarted check's lastState.terminated.message must be read: got (%q, %v)", got, ok)
	}

	// (c) no terminated record at all (the check never ran) -> ("", false).
	podC := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
		{Name: check},
	}}}
	if got, ok := r.defaultReadCheckOutput(podC, check); ok || got != "" {
		t.Fatalf("(c) no terminated record must be (\"\", false): got (%q, %v)", got, ok)
	}

	// state takes precedence over lastState when BOTH are set (a restarted
	// container whose CURRENT state also terminated with a message).
	podD := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
		{Name: check,
			State:                corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "current"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "last"}},
		},
	}}}
	if got, _ := r.defaultReadCheckOutput(podD, check); got != "current" {
		t.Fatalf("state.terminated.message must take precedence over lastState: got %q", got)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
