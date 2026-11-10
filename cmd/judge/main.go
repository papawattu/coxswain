// Command judge is the Phase 7 Judge Job's entrypoint (PLAN-PHASE7.md slice
// J1 step c1, ADR-0011). It is OPERATOR-CONTROLLED code — the same trust
// level as the operator: the Judge Job runs the operator's own binary and
// sees the agent's change only as the diff text handed in via
// COX_JUDGE_DIFF_PATH (ADR-0011 "Where it runs": the Judge is not in the
// sandbox pod and executes no agent code).
//
// The binary asks the Judge — a second, ADVISORY model surface (the first is
// the Memory Curator, ADR-0002) — "does this change meet the goal without
// gaming the acceptance?", and records the fixed-shape answer through
// pkg/judge (Client.Judge: prompt, strict parse, the model-proxy call). It
// never interprets free text: the reply is either the fixed verdict JSON or
// "unavailable" (ADR-0011 "Output" / "Unavailable").
//
// All inputs come from env (the Job's container has no args contract and the
// distroless image has no shell — the memory-writer's same pattern):
//
//	COX_JUDGE_BASE_URL         the model proxy's base URL (required)
//	COX_JUDGE_MODEL            the Loop's model name
//	COX_JUDGE_MAX_TOKENS       the Loop's Judge token cap (int; required)
//	COX_JUDGE_GOAL             the Loop's goal
//	COX_JUDGE_PLAN             the Loop's PLAN.md
//	COX_JUDGE_CHECKS           a JSON array of {"name","exitCode"} (the D14 gate values)
//	COX_JUDGE_DIFF_PATH        the diff file (default /judge/diff.patch)
//	COX_JUDGE_RESULT_PATH      the container's termination-message path; empty = stdout only
//	COX_JUDGE_TIMEOUT_SECONDS  the overall call timeout (default 120)
//
// The output is EXACTLY one JSON line (the termination message is a single
// bounded line the operator strictly validates):
//
//	{"verdict":true|false,"score":N,"reason":"…","truncated":bool}  on success
//	{"unavailable":"<short cause, at most 120 chars, one line>","truncated":bool}  on ANY failure
//
// It is written to COX_JUDGE_RESULT_PATH (if set) AND stdout, and the binary
// ALWAYS exits 0 (ADR-0011: the operator reads the termination message; the
// exit code is not the verdict — a failure line IS the "unavailable" verdict,
// and JudgeVerdict=Unknown is recorded by the operator, never by the exit
// code). Nothing from the env is printed (no secrets are expected, but
// nothing from it is ever emitted as input; the result line is the binary's
// own output).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/papawattu/coxswain/pkg/judge"
)

// diffPathDefault is where the Job's diff volume is mounted (the operator
// writes baseCommit..verifiedCommit before the Judge Job starts).
const diffPathDefault = "/judge/diff.patch"

// timeoutDefaultSeconds is the overall budget for the Judge call (the
// operator's 120s budget: the retry ladder plus the HTTP call must fit in a
// bounded window inside the Job's deadline).
const timeoutDefaultSeconds = 120

// unavailableCauseMax caps the "unavailable" cause at 120 characters (the
// termination message must stay one short, bounded line).
const unavailableCauseMax = 120

func main() {
	line := run()
	// A broken stdout is the last thing we can do anything about; the
	// termination-message file (if any) is still written below and the exit
	// code stays 0 either way.
	if _, err := os.Stdout.WriteString(line); err != nil {
		fmt.Fprintf(os.Stderr, "judge: could not write the result line to stdout: %v\n", err)
	}
	if p := os.Getenv("COX_JUDGE_RESULT_PATH"); p != "" {
		// Non-fatal: stdout already carries the line; the file copy is
		// best-effort (the same pattern as the memory-writer's summary
		// file). A write failure is logged to stderr with NO input value
		// (the path is not echoed) and does not change the exit code.
		if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "judge: could not write the result line to the termination-message path")
		}
	}
	// ALWAYS exit 0: the operator reads the message; the exit code is not
	// the verdict.
	os.Exit(0)
}

// run performs the Judge call and returns the single JSON line (with a
// trailing newline). Every failure class (bad env, unreadable diff,
// transport, HTTP status, parse, timeout) becomes the "unavailable" line —
// run never returns an error and never panics into a non-zero exit.
func run() string {
	timeoutSec := timeoutDefaultSeconds
	if v := os.Getenv("COX_JUDGE_TIMEOUT_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			// A bad timeout value is a bad env: unavailable. The default is
			// NOT applied (a misconfigured Job must not silently judge with
			// a different budget).
			return unavailableLine("bad COX_JUDGE_TIMEOUT_SECONDS: must be a positive integer", false)
		}
		timeoutSec = n
	}

	// Build the client from the env. A bad or missing required value is
	// "unavailable" — the operator records JudgeVerdict=Unknown (ADR-0011
	// "Unavailable": the Loop proceeds).
	baseURL := os.Getenv("COX_JUDGE_BASE_URL")
	if baseURL == "" {
		return unavailableLine("COX_JUDGE_BASE_URL is not set", false)
	}
	maxTokens, badMaxTokens := 0, false
	if v := os.Getenv("COX_JUDGE_MAX_TOKENS"); v == "" {
		badMaxTokens = true
	} else if n, err := strconv.Atoi(v); err != nil || n <= 0 {
		badMaxTokens = true
	} else {
		maxTokens = n
	}
	if badMaxTokens {
		return unavailableLine("COX_JUDGE_MAX_TOKENS is not set or is not a positive integer", false)
	}

	// Read the diff with a limit of MaxDiffBytes+1: a huge diff is never
	// loaded whole, and the extra byte is the truncation marker (ADR-0011
	// "Input": capped at 256 KiB and the input marked truncated). BuildPrompt
	// re-caps to exactly MaxDiffBytes, so reading one byte more only costs
	// one byte of memory.
	diffPath := os.Getenv("COX_JUDGE_DIFF_PATH")
	if diffPath == "" {
		diffPath = diffPathDefault
	}
	diff, truncated, err := readDiffBounded(diffPath)
	if err != nil {
		// The diff path is operator-configured, not a secret, but is NOT
		// echoed here either — the failure class alone says what failed.
		return unavailableLine("the diff file could not be read", false)
	}

	// Parse the checks (the D14 gate values the operator already read).
	var checks []judge.Check
	if v := os.Getenv("COX_JUDGE_CHECKS"); v != "" {
		if err := json.Unmarshal([]byte(v), &checks); err != nil {
			// A malformed checks value is a bad env: unavailable. The raw
			// value is NOT echoed (only that it did not parse).
			return unavailableLine("COX_JUDGE_CHECKS did not parse as a JSON array of {name,exitCode}", false)
		}
	}

	in := judge.Input{
		Goal:   os.Getenv("COX_JUDGE_GOAL"),
		Plan:   os.Getenv("COX_JUDGE_PLAN"),
		Diff:   diff,
		Checks: checks,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()
	client := judge.New(baseURL, os.Getenv("COX_JUDGE_MODEL"), int64(maxTokens))
	v, callTruncated, err := client.Judge(ctx, in)
	if err != nil {
		// ANY failure (transport, HTTP status, parse, timeout) is
		// "unavailable" (ADR-0011 "Unavailable": record Unknown and proceed
		// — the operator never interprets prose and never reads a failure
		// as a verdict).
		return unavailableLine("judge call: "+err.Error(), truncated || callTruncated)
	}
	return verdictLine(v, truncated || callTruncated)
}

// readDiffBounded reads the diff file with io.LimitReader at
// judge.MaxDiffBytes+1, so a huge file never loads whole. It returns the
// full diff when it is at or under the cap, or the first MaxDiffBytes and
// truncated=true when it exceeds the cap.
func readDiffBounded(path string) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, judge.MaxDiffBytes+1))
	if err != nil {
		return "", false, err
	}
	if len(data) > judge.MaxDiffBytes {
		return string(data[:judge.MaxDiffBytes]), true, nil
	}
	return string(data), false, nil
}

// verdictLine renders the success line: the fixed-shape verdict JSON (ADR-
// 0011 "Output").
func verdictLine(v judge.Verdict, truncated bool) string {
	b, err := json.Marshal(map[string]any{
		"verdict":   v.Verdict,
		"score":     v.Score,
		"reason":    v.Reason,
		"truncated": truncated,
	})
	if err != nil { // cannot happen (fixed keys, plain values) — be safe.
		b = []byte(`{"unavailable":"internal: could not render the verdict line","truncated":true}`)
	}
	return string(b) + "\n"
}

// unavailableLine renders the failure line:
// {"unavailable":"<cause>","truncated":bool} with the cause bounded to
// unavailableCauseMax characters and one line. Nothing from the env is
// echoed (the cause names the failure class, not the input).
var proxyURLRe = regexp.MustCompile(`(Post|Get) "[^"]*": `)

func unavailableLine(cause string, truncated bool) string {
	cause = strings.ReplaceAll(cause, "\r", " ")
	cause = strings.ReplaceAll(cause, "\n", " ")
	cause = strings.ReplaceAll(cause, "\t", " ")
	// I268: a Go url.Error cause embeds the full proxy URL, which can use up
	// the entire 120-rune budget and lose the actual error. Before the cap,
	// replace the quoted URL with the bare token <proxy> so the cause keeps
	// the error, not the URL.
	cause = proxyURLRe.ReplaceAllString(cause, "$1 <proxy>: ")
	if r := []rune(cause); len(r) > unavailableCauseMax {
		cause = string(r[:unavailableCauseMax])
	}
	b, err := json.Marshal(map[string]any{
		"unavailable": cause,
		"truncated":   truncated,
	})
	if err != nil { // cannot happen — be safe with a fixed, bounded line.
		b = []byte(`{"unavailable":"internal: could not render the unavailable line","truncated":false}`)
	}
	return string(b) + "\n"
}
