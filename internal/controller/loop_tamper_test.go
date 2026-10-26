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
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestVerifyOutcomeTamperExit (F39): the tamper init's exit code decides the
// outcome: 1 (a protected path changed) is verifyTampered, the script's own
// git failure (2) is verifyInfraFailed (fail closed, but not blamed on the
// agent), and a tamper init that has not terminated is no decision.
func TestVerifyOutcomeTamperExit(t *testing.T) {
	pod := func(st corev1.ContainerState) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
			{Name: verifyCloneBaseInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}},
			{Name: verifyImportAgentInit, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}},
			{Name: verifyTamperInit, State: st},
		}}}
	}
	term := func(c int32) corev1.ContainerState {
		return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: c}}
	}
	// termReason builds a terminated state with the kubelet's reason (F83c:
	// an OOMKill is the kubelet's kill, not a protected-path diff).
	termReason := func(c int32, reason string) corev1.ContainerState {
		return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: c, Reason: reason}}
	}
	cases := []struct {
		name string
		st   corev1.ContainerState
		want int
	}{
		{"exit 1 -> tampered", term(1), verifyTampered},
		{"git failure exit -> infra failed", term(tamperGitErrorExit), verifyInfraFailed},
		{"OOMKilled (137) -> infra failed, not tampered", termReason(137, "OOMKilled"), verifyInfraFailed},
		{"OOMKilled with another code -> infra failed too", termReason(1, "OOMKilled"), verifyInfraFailed},
		{"running -> no decision", corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, verifyNoDecision},
		{"waiting -> no decision", corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}, verifyNoDecision},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _, _ := verifyOutcome(pod(tc.st), 1, false, false)
			if got != tc.want {
				t.Errorf("verifyOutcome = %d, want %d", got, tc.want)
			}
		})
	}
}
