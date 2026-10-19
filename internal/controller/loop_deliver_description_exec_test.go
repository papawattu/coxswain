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

// A4 (PLAN-ALPHA.md A4): the deliver Job's self-explaining PR description —
// the execution test for the JSON safety of the push container's payload
// builder. The test runs the REAL deliverPushPayloadPython (the python the
// push container's script invokes to build the PR create payload) with the
// operator's facts body and a HOSTILE agent plan (backticks, quotes,
// newlines, a closing brace, and a line that would close a hand-built JSON
// object), and asserts:
//
//  1. the payload is valid JSON the operator's json can decode (a hostile
//     plan must not break the JSON — the python's json.dumps escapes every
//     byte, so a '}' or a quote in the plan is data, not a JSON terminator);
//  2. the plan content survives VERBATIM in the decoded body (the JSON
//     decoding restores the original bytes — the hostile characters are
//     round-tripped, not mangled);
//  3. the fenced, labelled plan section is present (the plan is clearly
//     labelled as agent-authored and untrusted, and fenced in a markdown
//     fence);
//  4. a plan that exceeds the cap is truncated at a whole line with the
//     truncation note appended (the cap + note are in the decoded body);
//  5. the facts body is NOT interleaved with the plan (the facts are a
//     separate section; the plan is appended AFTER the facts, in its own
//     fenced section — the plan cannot alter the facts text).
//
// The python is run as the push container runs it: python3 -c <source> with
// every value in the ENVIRONMENT (the push script's shell locals exported,
// the plan read from disk by the python itself). The test writes the hostile
// plan to a temp file and passes its path as PLAN_FILE, exactly as the
// push script does.
package controller

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// TestDeliverPushPayloadJSONSafety (A4) runs the REAL deliverPushPayloadPython
// with a hostile plan and asserts the payload is safe JSON (the plan's
// hostile characters round-trip verbatim through the JSON encoding/decoding,
// and the fenced labelled plan section is present). A mutation that builds
// the payload with a printf of the plan into a JSON string (or that inlines
// the plan into the python source) fails this test: a '}' or a quote in the
// plan would terminate the JSON or the python, and the decoded body would
// not round-trip the hostile bytes.
func TestDeliverPushPayloadJSONSafety(t *testing.T) {
	facts := renderDeliverDescriptionBody(deliverDescriptionForLoop(a4FactsLoop()))
	// A hostile plan: backticks (a shell fence / a markdown fence), a double
	// quote, a single quote, a newline, a closing brace (the character that
	// would close a hand-built JSON object), and a line that would inject a
	// JSON field if the payload were hand-built.
	plan := "# Plan\n\n`echo hostile`\nquote \" and ' and }\n\n" +
		"\"body\": \"injected\",\n" +
		"some plan text\n" +
		"```fence\nfenced content with `backtick`\n```\n" +
		"a \\backslash and a \nnewline inside the plan\n"

	planFile := filepath.Join(t.TempDir(), "PLAN.md")
	if err := os.WriteFile(planFile, []byte(plan), 0o644); err != nil {
		t.Fatalf("write the hostile plan: %v", err)
	}

	payload, err := runDeliverPushPayloadPython(t, facts, planFile, deliverPlanMaxBytes)
	if err != nil {
		t.Fatalf("the push payload python failed (the push Job would exit non-zero): %v", err)
	}

	// 1. The payload is valid JSON.
	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		t.Fatalf("the PR payload is not valid JSON (a hostile plan must not break the JSON): %v; payload: %q", err, payload)
	}
	body, ok := obj["body"].(string)
	if !ok {
		t.Fatalf("the PR payload has no string body field: %q", obj)
	}
	if obj["head"] != "coxswain/task" || obj["base"] != "main" {
		t.Fatalf("the PR payload's head/base are wrong: %v", obj)
	}

	// 2. The plan content survives VERBATIM (the JSON decoding restores the
	// original bytes — the hostile characters round-trip).
	if !strings.Contains(body, plan) {
		t.Fatalf("the decoded PR body does not round-trip the hostile plan verbatim.\n\nplan:\n%s\n\ndecoded body:\n%s", plan, body)
	}

	// 3. The fenced, labelled plan section is present.
	if !strings.Contains(body, deliverPlanSectionHeader) {
		t.Fatalf("the decoded PR body does not carry the labelled, fenced plan section header %q:\n%s", deliverPlanSectionHeader, body)
	}

	// 5. The facts body is a separate section (the plan is appended AFTER the
	// facts; the plan cannot alter the facts text). The facts body is the
	// operator's text; it must appear BEFORE the plan section.
	factsIdx := strings.Index(body, "# coxswain delivery")
	planIdx := strings.Index(body, deliverPlanSectionHeader)
	if factsIdx < 0 || planIdx < 0 || factsIdx > planIdx {
		t.Fatalf("the decoded PR body does not carry the facts section BEFORE the plan section (facts idx %d, plan idx %d):\n%s", factsIdx, planIdx, body)
	}
}

// TestDeliverPushPayloadPlanTruncated (A4) runs the REAL deliverPushPayloadPython
// with a plan that exceeds deliverPlanMaxBytes and asserts the plan is
// truncated at a whole line with the truncation note appended (the cap is
// enforced by the python, and the note is in the decoded body).
func TestDeliverPushPayloadPlanTruncated(t *testing.T) {
	facts := renderDeliverDescriptionBody(deliverDescriptionForLoop(a4FactsLoop()))
	// A plan that exceeds the cap: many lines, each short enough that the
	// whole-line truncation lands mid-plan.
	plan := ""
	for i := range 2000 {
		plan += fmt.Sprintf("line %04d: a plan line that is long enough to push the total past the cap\n", i)
	}
	planFile := filepath.Join(t.TempDir(), "PLAN.md")
	if err := os.WriteFile(planFile, []byte(plan), 0o644); err != nil {
		t.Fatalf("write the oversized plan: %v", err)
	}

	payload, err := runDeliverPushPayloadPython(t, facts, planFile, deliverPlanMaxBytes)
	if err != nil {
		t.Fatalf("the push payload python failed: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		t.Fatalf("the PR payload is not valid JSON: %v; payload: %q", err, payload)
	}
	body, _ := obj["body"].(string)
	if !strings.Contains(body, "truncated: the plan exceeded the "+fmt.Sprint(deliverPlanMaxBytes)+"-byte cap") {
		t.Fatalf("the decoded PR body does not carry the truncation note for a plan over the %d-byte cap:\n%s", deliverPlanMaxBytes, body)
	}
	// The truncated plan must not carry the LAST line (the truncation cut the
	// plan at a whole line before the end).
	if strings.Contains(body, "line 1999:") {
		t.Fatalf("the decoded PR body carries the last plan line (line 1999) — the plan was not truncated to the cap:\n%s", body)
	}
}

// runDeliverPushPayloadPython runs the REAL deliverPushPayloadPython (the
// python the push container's script invokes) with the operator's facts body
// and a plan file (the untrusted .coxswain/PLAN.md stand-in), every value in
// the ENVIRONMENT (the push script's shell locals exported, the plan read
// from disk by the python itself). Returns the payload JSON on stdout.
func runDeliverPushPayloadPython(t *testing.T, facts, planFile string, planMax int) (string, error) {
	t.Helper()
	cmd := exec.Command("python3", "-c", deliverPushPayloadPython)
	cmd.Env = append(os.Environ(),
		"FACTS="+facts,
		"PLAN_FILE="+planFile,
		"PLAN_MAX="+fmt.Sprint(planMax),
		"BRANCH=coxswain/task",
		"BASE=main",
		"DRAFT=true",
		"TITLE_PREFIX=",
		"PR_TITLE=coxswain: implement",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("python3 -c deliverPushPayloadPython: %w; output: %s", err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

// a4FactsLoop builds a Loop with the spec/status facts a4FactsLoop's body
// should carry (a goal, an iteration count, a pinned commit, a tamper-clean
// verify, a passed last verify, and a budget with a token cap). The body is
// the operator's text; the test asserts the body's exact shape (the facts
// section is built from the Loop's spec/status only).
func a4FactsLoop() *coxv1alpha1.Loop {
	tamper := int32(0)
	maxTokens := int64(1000000)
	return &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "a4task1", Namespace: "a4ns"},
		Spec: coxv1alpha1.LoopSpec{
			Goal:      "Add a rounding function",
			Loop:      coxv1alpha1.LoopSettings{MaxIterations: 5},
			Budget:    &coxv1alpha1.BudgetConfig{MaxTokens: &maxTokens},
			Workspace: coxv1alpha1.Workspace{Repo: "http://gitea.example:3000/samples/gocli.git"},
			Delivery:  &coxv1alpha1.DeliveryConfig{Mode: coxv1alpha1.DeliveryModePullRequest},
		},
		Status: coxv1alpha1.LoopStatus{
			Iteration: 2,
			// The per-iteration verify record (A4): iteration 1 FAILED
			// (the failing check TestRound, the verify Job pod's finish time
			// a4StallTime — operator-recorded), iteration 2 passed (the pinned
			// verifiedCommit below). The facts body must carry both.
			StallHistory: []coxv1alpha1.StallEntry{
				{
					Iteration:            1,
					JobName:              "a4task1-verify-1",
					Hash:                 "0a1b2c3d4e5f60718293a4b5c6d7e8f9",
					NormalisationVersion: "v1",
					Check:                "TestRound",
					At:                   a4StallTime,
				},
			},
			CurrentVerify: &coxv1alpha1.CurrentVerifyStatus{
				VerifiedCommit: "c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4",
			},
			Verify: &coxv1alpha1.VerifyStatus{
				TamperExitCode: &tamper,
			},
			Progress: &coxv1alpha1.ProgressStatus{},
			Budget: &coxv1alpha1.BudgetStatus{
				PromptTokens:     1000,
				CompletionTokens: 500,
				Requests:         7,
				CostUsd:          "0.05",
				ActiveSeconds:    300,
			},
		},
	}
}
