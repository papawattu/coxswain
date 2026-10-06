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

const p2eStallNS = "ns-a"

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

// TestApplyStallGateInertWhenCheckNotTerminated: the gate is INERT when the
// failing check's container has not TERMINATED (spec 9/10's terminal gate,
// at the gate level — the I49 in-progress evidence). A check that is
// Running (not Terminated) is in-progress evidence: no StallEntry, no fire,
// no Stalled condition. Mutation: dropping the terminal gate (append a
// StallEntry on a non-terminal verify) must make spec 9 FAIL (an entry
// appears while the check is Running).
func TestApplyStallGateInertWhenCheckNotTerminated(t *testing.T) {
	// The terminal gate (I49, spec 9/10): a check that has not terminated yet
	// is in-progress evidence — the stall gate is inert (no decision, no
	// entry, no fire). The check container's TERMINATION is the terminal
	// evidence; a check that is still Running (not terminated) is NOT
	// terminal evidence, so the gate must not append a StallEntry or fire.
	//
	// Mutation: dropping the terminal gate (append a StallEntry on a
	// non-terminal verify) must make spec 9 FAIL (an entry appears while the
	// check is Running).
	loop := &coxv1alpha1.Loop{}
	loop.Name = "lp"
	loop.Namespace = p2eStallNS
	loop.Status.Phase = coxv1alpha1.LoopPhaseVerifying
	loop.Status.Iteration = 1
	n := int32(1)
	loop.Spec.Loop.StallAfter = &n
	loop.Spec.Loop.StallAction = coxv1alpha1.StallActionFail

	runningState := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: s5aCheck0, State: runningState},
			},
		},
	}
	r := &LoopReconciler{readCheckOutput: func(*corev1.Pod, string) (string, bool) { return "out", true }}
	if fired := r.applyStallGate(context.Background(), loop, pod, s5aCheck0); fired {
		t.Fatalf("a non-terminated check must not fire the stall gate (the terminal gate)")
	}
	if len(loop.Status.StallHistory) != 0 {
		t.Fatalf("a non-terminated check must append NO StallEntry, got %d", len(loop.Status.StallHistory))
	}
	if loop.Status.Phase != coxv1alpha1.LoopPhaseVerifying {
		t.Fatalf("a non-terminated check must not change the phase, got %s", loop.Status.Phase)
	}
	if len(loop.Status.Conditions) != 0 {
		t.Fatalf("a non-terminated check must not set the Stalled condition, got %v", loop.Status.Conditions)
	}

	// A terminated check (the terminal evidence) still works (the gate is not
	// disabled for all evidence — only for in-progress evidence). The check
	// container's State is Terminated (exit 1, a failure): the gate decides.
	terminatedState := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode:   1,
		FinishedAt: metav1.Now(),
	}}
	loop2 := &coxv1alpha1.Loop{}
	loop2.Name = "lp"
	loop2.Namespace = p2eStallNS
	loop2.Status.Phase = coxv1alpha1.LoopPhaseVerifying
	loop2.Status.Iteration = 1
	loop2.Spec.Loop.StallAfter = &n
	loop2.Spec.Loop.StallAction = coxv1alpha1.StallActionFail
	pod2 := &corev1.Pod{
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
	if !r.applyStallGate(context.Background(), loop2, pod2, s5aCheck0) {
		t.Fatalf("a terminated check (N=1) must fire the stall gate")
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
