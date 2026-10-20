// Command memory-write is the P5b memory-writer Job's entrypoint (Phase 5
// shared memory, PLAN-PHASE5.md slice P5b). It is OPERATOR-CONTROLLED code —
// the same trust level as the operator (ADR-0002: the operator is the sole
// writer of the shared memory PVC; the Job runs the operator's binary, never
// model-authored code). The Job mounts the workspace PVC READ-ONLY (the
// runner's proposed lessons are on the workspace:
// /workspace/.coxswain/lessons.json) and the memory PVC READ-WRITE
// (/memory). The entrypoint:
//
//  1. reads /workspace/.coxswain/lessons.json (the runner's proposed
//     lessons — the workspace-file transport, R28 P2-7);
//  2. applies the static write policy (pkg/memorywrite.Screen) — the rules
//     are the operator's deterministic Go (secrets regex, ≤500 chars,
//     single-line, non-blank, the size cap); the Loop's terminal outcome
//     (Succeeded or Failed:Stalled) is passed as the first arg (the
//     eligibility rule: a lesson is eligible only on a qualifying terminal
//     outcome);
//  3. writes ONLY the eligible lessons to /memory/lesson-<n>.txt (one file
//     per lesson, n zero-padded to 3 digits — lesson-001 < lesson-002 < ...);
//  4. reports a BOUNDED summary to stdout (well under 4096 bytes — the
//     termination message cap): the counts (total / written / dropped), plus
//     one line per lesson with its sha256-prefix, written|dropped, and the
//     rule. NEVER the lesson text (a lesson that failed the secrets screen
//     must not be echoed into the Job's logs / termination message).
//
// Exit 0 on success (the write completed, whether or not any lesson was
// eligible — a zero-eligible write is a valid "nothing to write"); exit
// non-zero on a write failure (the memory PVC is not mounted writable, the
// disk is full) or a read failure (the lessons file is malformed). A failed
// write does NOT fail the Loop (ADR-0004): the operator reads the Job's
// termination phase and records MemoryWriteFailed.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/papawattu/coxswain/pkg/memorywrite"
)

const (
	// shaPrefixLen is the sha256-prefix length reported per lesson (the
	// bounded summary never includes the lesson text; a 12-hex-char prefix
	// is enough to identify a lesson in the condition/history without
	// leaking its content).
	shaPrefixLen = 12
)

// lessonsPathFromEnv returns the COX_LESSONS_PATH env var (the workspace
// lessons file) or the default /workspace/.coxswain/lessons.json (the Job's
// mount). The env override lets the execution test point the reader at a
// temp file without a real workspace PVC.
func lessonsPathFromEnv() string {
	if p := os.Getenv("COX_LESSONS_PATH"); p != "" {
		return p
	}
	return "/workspace/.coxswain/lessons.json"
}

// memoryDirFromEnv returns the COX_MEMORY_DIR env var (the memory PVC's
// mount point) or the default /memory (the Job's mount). The env override
// lets the execution test point the writer at a temp dir without a real PVC.
func memoryDirFromEnv() string {
	if d := os.Getenv("COX_MEMORY_DIR"); d != "" {
		return d
	}
	return "/memory"
}

// summaryPathFromEnv returns the COX_SUMMARY_PATH env var (the path the
// binary writes its bounded summary to, IN ADDITION to stdout). The Job
// sets this to the container's termination-message path
// (/tmp/memory-write-summary.txt on the /tmp emptyDir) so the operator can
// read the bounded summary from the pod's container status without a shell
// wrapper (the operator's distroless image has no /bin/sh — the binary is
// the container's direct entrypoint). Empty (the execution test, or a
// non-Job invocation) = stdout only.
func summaryPathFromEnv() string {
	return os.Getenv("COX_SUMMARY_PATH")
}

func main() {
	// The terminal outcome is the first arg: "succeeded" or "failed-stalled".
	// A missing arg is a usage error (exit 2) — the operator always passes it.
	outcome := ""
	if len(os.Args) > 1 {
		outcome = os.Args[1]
	}
	code, err := run(outcome)
	if err != nil {
		// A bounded error (the lesson text is NEVER in the error — the
		// secrets screen already dropped anything with a credential, but the
		// error must not echo a lesson verbatim either).
		fmt.Fprintf(os.Stderr, "memory-write: %v\n", boundedError(err))
	}
	os.Exit(code)
}

// run performs the read-screen-write and reports the bounded summary.
// Returns (exitCode, error). exitCode is 0 on a successful write (including
// the zero-eligible case) and 1 on a read/write failure; error is non-nil
// when exitCode is 1.
func run(outcome string) (int, error) {
	// The eligibility rule (PLAN-PHASE5.md: a lesson is eligible only on a
	// qualifying terminal outcome): the outcome must be Succeeded or
	// Failed:Stalled. Anything else (including an empty arg — a usage error)
	// is ineligible: the Job writes nothing and exits 0 (a non-qualifying
	// outcome is not a failure of the write; it is the operator's eligibility
	// decision, and the operator only invokes the Job on a qualifying
	// outcome).
	eligibleOutcome := outcome == "succeeded" || outcome == "failed-stalled"

	// Read the paths FRESH at call time (NOT the package-level vars, which
	// are captured at package-load — before the execution test's t.Setenv
	// overrides take effect). The env override (COX_LESSONS_PATH /
	// COX_MEMORY_DIR) lets the execution test point the reader/writer at
	// temp paths without a real PVC.
	lp := lessonsPathFromEnv()
	md := memoryDirFromEnv()

	// Read the proposed lessons from the workspace file. A missing file (the
	// runner proposed nothing — a fresh run) is the "no lessons" path: zero
	// total, zero written, exit 0.
	data, err := os.ReadFile(lp)
	if err != nil {
		if os.IsNotExist(err) {
			// No lessons file (the runner proposed nothing): a valid
			// "nothing to write". Report the bounded summary and exit 0.
			emitSummary([]string{summaryLine(0, 0, 0)})
			return 0, nil
		}
		return 1, fmt.Errorf("read %s: %w", lp, err)
	}
	var lessons []string
	if err := json.Unmarshal(data, &lessons); err != nil {
		// A malformed lessons file (the runner wrote invalid JSON): the
		// read failed. Do NOT echo the raw data (it may contain a
		// credential the screen would have dropped); report the bounded
		// error.
		return 1, fmt.Errorf("parse %s: %w (malformed lessons JSON)", lp, err)
	}

	// Screen the proposals (the operator's static write policy). The outcome
	// eligibility is applied FIRST (a non-qualifying outcome drops every
	// lesson, regardless of the per-lesson rules — the operator only invokes
	// the Job on a qualifying outcome, but the Job enforces the rule too, so
	// a mis-invocation writes nothing).
	survivors, reasons := screenLessons(lessons, eligibleOutcome)

	// Write the eligible lessons to the memory PVC.
	written, err := writeLessons(md, survivors)
	if err != nil {
		return 1, fmt.Errorf("write lessons to %s: %w", md, err)
	}

	// The bounded summary: total (len(lessons)), written (len(survivors)),
	// dropped (total - written), and one line per lesson with its
	// sha256-prefix, written|dropped, and rule. NEVER the lesson text.
	total := len(lessons)
	dropped := total - written
	emitSummary(summaryLines(lessons, reasons, total, written, dropped))
	return 0, nil
}

// summaryLines builds the bounded summary's lines (one line per lesson with
// its sha256-prefix, written|dropped, and rule, plus the final counts line).
// It returns ONLY lines — the lesson text is NEVER in a line. This is the
// pure content of the summary (the emission — stdout + the termination
// message file — is in emitSummary).
func summaryLines(lessons, reasons []string, total, written, dropped int) []string {
	out := make([]string, 0, len(lessons)+1)
	for i, lesson := range lessons {
		status := "written"
		rule := "eligible"
		if i < len(reasons) && reasons[i] != "" {
			status = "dropped"
			rule = reasons[i]
		}
		out = append(out, fmt.Sprintf("lesson-%03d %s %s %s", i+1, shaPrefix(lesson), status, rule))
	}
	out = append(out, summaryLine(total, written, dropped))
	return out
}

// emitSummary writes the bounded summary's lines to stdout AND, if
// COX_SUMMARY_PATH is set, to that file (the container's termination-message
// path — the operator reads it from the pod's container status). A write
// failure to the summary file is non-fatal (the summary is best-effort
// evidence; the Lesson write already succeeded): it is logged to stderr but
// does not change the exit code. A bounded summary must never echo a lesson's
// text — the lines are built by summaryLines, which uses only the sha prefix.
func emitSummary(lines []string) {
	var buf bytes.Buffer
	for _, line := range lines {
		fmt.Fprintln(&buf, line)
	}
	// stdout (always).
	if _, werr := os.Stdout.Write(buf.Bytes()); werr != nil {
		fmt.Fprintf(os.Stderr, "memory-write: could not write the summary to stdout: %v (non-fatal)\n", werr)
	}
	// The termination-message file (if set). A failure here is non-fatal.
	if p := summaryPathFromEnv(); p != "" {
		if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
			fmt.Fprintf(
				os.Stderr,
				"memory-write: could not write the summary to %s: %v (non-fatal; the lesson write succeeded)\n",
				p, err,
			)
		}
	}
}

// screenLessons applies the eligibility + per-lesson static policy. It
// returns the survivors (the eligible lessons, in input order) and the
// reasons (one per input lesson: "" for a survivor, the drop rule for a
// dropped lesson). The outcome eligibility is the first rule (a
// non-qualifying outcome drops every lesson).
func screenLessons(lessons []string, eligibleOutcome bool) (survivors, reasons []string) {
	survivors = make([]string, 0, len(lessons))
	reasons = make([]string, 0, len(lessons))
	if !eligibleOutcome {
		for range lessons {
			reasons = append(reasons, "ineligible-outcome")
		}
		return survivors, reasons
	}
	// The operator's static write policy (pkg/memorywrite.Screen): the
	// secrets regex, the ≤500-char cap, single-line, non-blank.
	survivors, screenReasons := memorywrite.Screen(lessons)
	// Reconcile Screen's per-input reasons with the input order. Screen
	// returns one reason per input lesson (a survivor's reason is "").
	reasons = screenReasons
	return survivors, reasons
}

// writeLessons writes each survivor to <memoryDir>/lesson-<n>.txt (n
// zero-padded to 3 digits, in input order — lesson-001 < lesson-002 < ...).
// It creates the directory if absent (the memory PVC is mounted at
// /memory; a missing dir is a write failure — the mount is not writable).
// Returns the number of lessons written.
func writeLessons(dir string, survivors []string) (int, error) {
	if len(survivors) == 0 {
		// Nothing to write (the screen dropped everything, or the runner
		// proposed nothing): a valid zero-write. Ensure the dir is writable
		// is NOT required (no file is written).
		return 0, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	for i, lesson := range survivors {
		// The lesson is single-line (the screen enforces that) and ≤500
		// chars (the size cap), so a single-line write is safe. A trailing
		// newline is appended (the memory-reader reads the file's content;
		// the trailing newline is a conventional text-file terminator and
		// the reader trims it).
		name := fmt.Sprintf("%s/lesson-%03d.txt", dir, i+1)
		if err := os.WriteFile(name, []byte(lesson+"\n"), 0o644); err != nil {
			return i, fmt.Errorf("write %s: %w", name, err)
		}
	}
	return len(survivors), nil
}

// shaPrefix returns the first shaPrefixLen hex chars of the lesson's
// sha256 (the bounded identifier: enough to correlate a lesson in the
// condition/history without leaking its content).
func shaPrefix(lesson string) string {
	sum := sha256.Sum256([]byte(lesson))
	return hex.EncodeToString(sum[:])[:shaPrefixLen]
}

// printSummary emits one line per lesson (the bounded per-lesson record):
//
//	lesson-<n> <sha256-prefix> written|dropped <rule>
//
// The rule is the drop reason for a dropped lesson (a survivor's rule is
// "eligible"). NEVER the lesson text. The lines are in input order (n is
// the 1-based input index, so a dropped lesson's n is its input position,
// not the written file's n — the written-file n is only assigned to
// survivors, in survivor order).

// summaryLine is the final bounded summary line (the counts): the operator
// parses this from the termination message.
func summaryLine(total, written, dropped int) string {
	return fmt.Sprintf("summary: total=%d written=%d dropped=%d", total, written, dropped)
}

// boundedError returns a short, bounded representation of err (the Job's
// stderr / termination message is bounded to 4096 bytes; a large error
// string — e.g. one that includes file content — would blow the cap). It
// truncates to 200 bytes. It does NOT include lesson text (the error is a
// read/write diagnostic, not a lesson echo).
func boundedError(err error) string {
	s := err.Error()
	const max = 200
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
