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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/deliverytitle"
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

// TestDeliverPushPayloadPlanFenceOutlivesThePlan (F3) runs the REAL
// deliverPushPayloadPython with a plan that carries its own ``` fence and
// asserts the operator's fence is LONGER, so the plan stays inside it. Before
// F3 the fence was a fixed ```: a plan line of three backticks closed it, and
// everything after it (operator-looking headings, remote images, links,
// @mentions) rendered as ordinary markdown in the delivered PR body. The test
// asserts containment, not just the fence's length: the injected heading must
// appear BETWEEN the opening fence and the closing one. Mutation: restore the
// fixed ``` fence -> the fence is no longer 4 backticks and the injected
// heading lands outside the fence -> this test FAILS.
func TestDeliverPushPayloadPlanFenceOutlivesThePlan(t *testing.T) {
	facts := renderDeliverDescriptionBody(deliverDescriptionForLoop(a4FactsLoop()))
	// The plan closes the fence with its own ``` line and then writes what an
	// attacker would want in the PR body: a heading that imitates the operator's
	// sections, an @mention (GitHub notifies from body text, not from inside a
	// code fence) and a remote image (a tracking pixel).
	plan := "# Plan\n\n```sh\n## Injected operator-looking heading\n@someone please approve this\n" +
		"![](https://attacker.example/pixel.png)\n```\n"
	planFile := filepath.Join(t.TempDir(), "PLAN.md")
	if err := os.WriteFile(planFile, []byte(plan), 0o644); err != nil {
		t.Fatalf("write the fenced plan: %v", err)
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

	// The plan is verbatim inside the section (the JSON half was already
	// covered by TestDeliverPushPayloadJSONSafety).
	if !strings.Contains(body, deliverPlanSectionHeader) {
		t.Fatalf("the decoded body does not carry the plan section label %q:\n%s", deliverPlanSectionHeader, body)
	}
	open := "\n````markdown\n"
	if !strings.Contains(body, open) {
		t.Fatalf("the plan carries a ``` run, so the operator's fence must be LONGER than three backticks (want %q):\n%s", open, body)
	}
	rest := body[strings.Index(body, open)+len(open):]
	closing := "\n````\n"
	if !strings.Contains(rest, closing) {
		t.Fatalf("the operator's fence is not closed by a matching ```` line:\n%s", body)
	}
	inside := rest[:strings.Index(rest, closing)]
	for _, want := range []string{"## Injected operator-looking heading", "@someone please approve this", "```sh"} {
		if !strings.Contains(inside, want) {
			t.Fatalf("the plan line %q escaped the fence (it must stay between the opening and closing fence):\n%s", want, body)
		}
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
				// F83e: the record carries the pin it was recorded under (F83c
				// writes them together; this fixture predated that guard). Without
				// it the record is a legacy shape and the body must render "no
				// tamper evidence" — which is a different (correct) answer, not
				// the one this fixture is about.
				VerifiedCommit: "c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4",
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

// P5e (R28 I94, #174 P1): the payload's title equals TITLE_PREFIX + PR_TITLE
// (the commit subject and the PR title match). The PR_TITLE is the
// deliverytitle.Title(spec.goal) the operator sets on both the agent
// (COX_DELIVERY_TITLE) and the deliver Job (PR_TITLE). Mutation: set PR_TITLE
// to a different string — this test FAILS.
func TestDeliverPushPayloadTitleMatchesCommitSubject(t *testing.T) {
	goal := "Add a TODO comment at the top of README.md"
	expectedTitle := deliverytitle.Title(goal)

	facts := renderDeliverDescriptionBody(deliverDescriptionForLoop(a4FactsLoop()))
	planFile := filepath.Join(t.TempDir(), "PLAN.md")
	if err := os.WriteFile(planFile, []byte("# Plan\n\ndo the thing\n"), 0o644); err != nil {
		t.Fatalf("write the plan file: %v", err)
	}

	cmd := exec.Command("python3", "-c", deliverPushPayloadPython)
	cmd.Env = append(os.Environ(),
		"FACTS="+facts,
		"PLAN_FILE="+planFile,
		"PLAN_MAX="+fmt.Sprint(deliverPlanMaxBytes),
		"BRANCH=coxswain/task",
		"BASE=main",
		"DRAFT=true",
		"TITLE_PREFIX=",
		"PR_TITLE="+expectedTitle,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the push payload python failed: %v\noutput: %s", err, out)
	}

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("the PR payload is not valid JSON: %v; payload: %q", err, out)
	}

	title, ok := obj["title"].(string)
	if !ok {
		t.Fatalf("the PR payload has no string title field: %v", obj)
	}

	// TITLE_PREFIX is empty, so title == PR_TITLE == the commit subject.
	if title != expectedTitle {
		t.Fatalf("the payload title = %q, want %q (the commit subject and PR title must match)", title, expectedTitle)
	}
}

// P5e (R28 I94, #174 P1): deliverytitle.Title derives the title from
// spec.goal (first line, trimmed, capped at 72 runes). The old behavior was
// a fixed "coxswain: " + BRANCH. Mutation: restore the fixed title — this
// test FAILS.
func TestDeliveryTitleDerivedFromGoal(t *testing.T) {
	// A single-line goal.
	if got := deliverytitle.Title("Add a TODO comment at the top of README.md"); got != "coxswain: Add a TODO comment at the top of README.md" {
		t.Fatalf("Title(%q) = %q", "Add a TODO comment at the top of README.md", got)
	}

	// A multi-line goal: only the first line is used.
	if got := deliverytitle.Title("First line.\nSecond line.\nThird line."); got != "coxswain: First line." {
		t.Fatalf("Title(multi-line) = %q, want %q", got, "coxswain: First line.")
	}

	// A blank goal: the fallback.
	if got := deliverytitle.Title(""); got != "coxswain: implement" {
		t.Fatalf("Title(\"\") = %q, want %q", got, "coxswain: implement")
	}

	// A whitespace-only first line: the fallback.
	if got := deliverytitle.Title("   \nSecond line."); got != "coxswain: implement" {
		t.Fatalf("Title(whitespace) = %q, want %q", got, "coxswain: implement")
	}

	// A long first line: cut to 71 runes + "…".
	longLine := strings.Repeat("a", 80)
	expected := "coxswain: " + strings.Repeat("a", 71) + "…"
	if got := deliverytitle.Title(longLine); got != expected {
		t.Fatalf("Title(long) = %q (len %d), want %q (len %d)", got, len(got), expected, len(expected))
	}
}

// f83ePin is the pin a4FactsLoop pins (the delivery's verified commit).
const f83ePin = "c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"

// F83e: the deliver description reads only the pin's OWN evidence.
//
// deliverDescriptionForLoop read status.currentVerify.verifiedCommit OUTSIDE
// the nil guard it uses a few lines above it (a latent nil deref: every live
// caller sits behind deliveryExpected, which needs a pin, so it could not fire
// — but the read itself was unguarded), and it took status.verify's tamper code
// whenever that code was non-nil, without checking the record belongs to the
// pin being delivered. loops/status is user-writable (D85), so a forged or
// pre-F83d status could carry a tamper verdict for a different commit and have
// it printed as this delivery's evidence.
var _ = DescribeTable("F83e: the deliver description reads only the pin's own evidence",
	func(mutate func(*coxv1alpha1.Loop), wantCommit string, wantTamper int32, wantBody, notBody []string) {
		l := a4FactsLoop()
		mutate(l)
		var d deliverDescription
		Expect(func() { d = deliverDescriptionForLoop(l) }).NotTo(Panic(),
			"a Loop with no status.currentVerify must not be dereferenced")
		Expect(d.VerifiedCommit).To(Equal(wantCommit))
		Expect(d.TamperClean).To(Equal(wantTamper))
		body := renderDeliverDescriptionBody(d)
		for _, want := range wantBody {
			Expect(body).To(ContainSubstring(want))
		}
		for _, not := range notBody {
			Expect(body).NotTo(ContainSubstring(not))
		}
	},
	Entry("no pin and no verify record: no deref, no evidence",
		func(l *coxv1alpha1.Loop) {
			l.Status.CurrentVerify = nil
			l.Status.Verify = nil
		}, "", int32(-1),
		[]string{"(none pinned)", "no tamper evidence"}, nil),
	Entry("no pin but a verify record: the record speaks for nothing",
		func(l *coxv1alpha1.Loop) {
			l.Status.CurrentVerify = nil
			l.Status.Verify = &coxv1alpha1.VerifyStatus{TamperExitCode: &a4TamperDirty}
		}, "", int32(-1),
		[]string{"(none pinned)", "no tamper evidence"}, nil),
	Entry("a record for a DIFFERENT commit: no evidence for this pin",
		func(l *coxv1alpha1.Loop) {
			l.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: f83ePin}
			l.Status.Verify = &coxv1alpha1.VerifyStatus{
				VerifiedCommit: "bbbbbbbbbbbb1111111111222222222233333333",
				TamperExitCode: &a4TamperDirty,
			}
		}, f83ePin, int32(-1),
		[]string{"no tamper evidence"}, []string{"a protected path changed"}),
	Entry("a record for THIS pin, tampered (positive control)",
		func(l *coxv1alpha1.Loop) {
			l.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: f83ePin}
			l.Status.Verify = &coxv1alpha1.VerifyStatus{
				VerifiedCommit: f83ePin,
				TamperExitCode: &a4TamperDirty,
			}
		}, f83ePin, int32(1),
		[]string{"exit 1 (a protected path changed)"}, []string{"no tamper evidence"}),
	Entry("a record for THIS pin, clean (positive control)",
		func(l *coxv1alpha1.Loop) { /* a4FactsLoop is already this shape */ },
		f83ePin, int32(0),
		[]string{"clean (no protected path changed)"}, []string{"no tamper evidence"}),
)
