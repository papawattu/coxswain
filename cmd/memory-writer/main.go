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
//  3. writes ONLY the eligible lessons to /memory/<sha256>.txt (one file
//     per lesson, named by the trimmed lesson's sha256 — the dedup key; the
//     `lesson-<n>` in the audit lines is the 1-based input index, not the
//     file name);
//  4. reports a summary to stdout and the termination message: the counts
//     (total / written / dropped), plus one line per lesson with its
//     sha256-prefix, written|dropped, and the rule. A lesson that failed the
//     secrets screen is never echoed, but a curator reason is free model text
//     and may quote a kept lesson. (The summary is not size-bounded here;
//     the termination message is capped at 4096 bytes by Kubernetes: F80.)
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
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/papawattu/coxswain/pkg/curator"
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

// curatorBaseURLFromEnv returns the COX_CURATOR_BASE_URL env var (the model
// proxy's base URL for the curator call) or empty (the curator is disabled —
// the Job runs without a model proxy, e.g. in the execution test). When
// empty, the curator is skipped and all static-rule survivors are written
// (the P5b behavior — the curator is a P5c addition).
func curatorBaseURLFromEnv() string {
	return os.Getenv("COX_CURATOR_BASE_URL")
}

// curatorModelFromEnv returns the COX_MODEL env var (the model name for the
// curator call) or empty (the curator is disabled).
func curatorModelFromEnv() string {
	return os.Getenv("COX_MODEL")
}

// seededLessonsFromEnv returns the COX_SEEDED_LESSONS env var (a JSON array of
// single-line strings) — the operator's seeded lessons (P5c's negative
// lesson: the stall signature, not the agent's text). "[]" or absent means
// "no seed". A malformed value is treated as "no seed" (the seed is
// best-effort; the runner's lessons are the authoritative input).
func seededLessonsFromEnv() []string {
	v := os.Getenv("COX_SEEDED_LESSONS")
	if v == "" || v == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil
	}
	return out
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

	// P5c: prepend the operator's seeded lessons (the negative lesson — the
	// stall signature, not the agent's text; PLAN-PHASE5.md:213-215). The
	// seed flows through the static rules + the curator exactly like the
	// runner's lessons (the seed's position in the input is index 0, so its
	// sha256-prefix in the bounded summary is stable). The seed is
	// operator-composed (not the agent's text) — the curator's injection
	// resistance still applies to it (the curator reads it as untrusted).
	if seeded := seededLessonsFromEnv(); len(seeded) > 0 {
		lessons = append(seeded, lessons...)
	}

	// Bound the batch (the agent can write lessons.json: the count is untrusted).
	// Lessons past the bound are not processed; the audit counts them.
	overBatch := 0
	if len(lessons) > memorywrite.MaxLessonsPerWrite {
		overBatch = len(lessons) - memorywrite.MaxLessonsPerWrite
		lessons = lessons[:memorywrite.MaxLessonsPerWrite]
	}

	// Screen the proposals (the operator's static write policy). The outcome
	// eligibility is applied FIRST (a non-qualifying outcome drops every
	// lesson, regardless of the per-lesson rules — the operator only invokes
	// the Job on a qualifying outcome, but the Job enforces the rule too, so
	// a mis-invocation writes nothing).
	// P3-1: Screen trims each lesson and the file is named by the TRIMMED text's
	// sha256, so the audit (the sha prefix and the dedup annotation) must hash
	// the trimmed text too, or a written lesson reads "duplicate" and the sha12
	// does not match the file on the PVC.
	for i := range lessons {
		lessons[i] = strings.TrimSpace(lessons[i])
	}
	_, reasons := screenLessons(lessons, eligibleOutcome)
	// P3-2: an identical lesson earlier in the same batch makes a later one a
	// duplicate, so the per-lesson lines, the counts and the metric agree.
	survivors := markBatchDuplicates(lessons, reasons)

	// P5c: the Memory Curator gate (ADR-0002, D70). Only when ALL static
	// rules pass (the survivors) does the curator judge each lesson. The
	// curator call goes to the Loop's model proxy (COX_CURATOR_BASE_URL); a
	// call failure drops the lesson ("curator unavailable").
	//
	// FAIL-CLOSED (D70): a lesson is written only on a true curator verdict.
	// When the curator is NOT configured (COX_CURATOR_BASE_URL empty), every
	// static-rule survivor is dropped with reason "curator unavailable" — the
	// writer writes NOTHING. This is the D70 rule: a curator that can't be
	// asked drops the lesson. The operator always configures the curator for a
	// Loop with a model endpoint (the proxy exists whether or not there is a
	// key Secret); if there is genuinely no model endpoint, no lesson is
	// written. The execution test sets COX_CURATOR_BASE_URL to a stub that
	// returns keep:true, so the test still writes survivors.
	curBase := curatorBaseURLFromEnv()
	if curBase == "" {
		// Fail closed: drop every survivor as "curator unavailable" (D70).
		for j := range lessons {
			if reasons[j] == "" {
				reasons[j] = "curator unavailable"
			}
		}
		survivors = nil
	} else {
		curModel := curatorModelFromEnv()
		curClient := curator.NewClient(curBase, curModel)
		// P2-3: the curator's per-lesson timeout is 60s. With up to 20
		// lessons (MaxLessonsPerWrite), 60s × 20 = 1200s — far
		// beyond the Job's 300s deadline. An overall budget (120s) ensures
		// the curator loop cannot exceed the Job's deadline; lessons that
		// are not judged before the budget expires are dropped as
		// "curator unavailable" (fail-closed: no lesson is written without
		// a true verdict).
		curBudget := 120 * time.Second
		curCtx, cancel := context.WithTimeout(context.Background(), curBudget)
		defer cancel()
		var curated []string
		for _, s := range survivors {
			if curCtx.Err() != nil {
				// The overall budget is exhausted: drop this survivor (and
				// any remaining) as "curator unavailable".
				for j := range lessons {
					if lessons[j] == s && reasons[j] == "" {
						reasons[j] = "curator unavailable"
						break
					}
				}
				continue
			}
			v := curClient.Judge(curCtx, s)
			if v.Keep {
				curated = append(curated, s)
				// ADR-0002: record the curator's reason for keeps too (the
				// audit trail: the verdict, keep or drop, plus a one-line
				// reason, goes into the audit for EVERY lesson). Mark the
				// reason so the summary reports "written curator-keep: <reason>".
				for j := range lessons {
					if lessons[j] == s && reasons[j] == "" {
						reasons[j] = "curator-keep: " + v.Reason
						break
					}
				}
			} else {
				// The curator dropped this survivor: mark it in the reasons
				// so the summary reports the drop reason.
				for j := range lessons {
					if lessons[j] == s && reasons[j] == "" {
						reasons[j] = "curator: " + v.Reason
						break
					}
				}
			}
		}
		survivors = curated
	}

	// Write the eligible lessons to the memory PVC (files named by sha256;
	// a lesson whose sha256 matches an existing file is dropped as a
	// duplicate).
	written, writtenNames, evicted, err := writeLessons(md, survivors)
	if err != nil {
		return 1, fmt.Errorf("write lessons to %s: %w", md, err)
	}

	// Mark the survivors that were NOT written (duplicates) as "dropped
	// duplicate" in the per-lesson reasons (the summary reports them). A
	// survivor that WAS written keeps its "" reason (written).
	annotateDedup(lessons, reasons, writtenNames)

	total := len(lessons)
	dropped := total - written
	audit := make([]string, 0, len(evicted)+1)
	for _, n := range evicted {
		sha := strings.TrimSuffix(n, ".txt")
		audit = append(audit, "evicted "+sha[:min(shaPrefixLen, len(sha))]+" cap")
	}
	if overBatch > 0 {
		audit = append(audit, fmt.Sprintf("truncated %d lessons over the batch cap %d",
			overBatch, memorywrite.MaxLessonsPerWrite))
	}
	emitSummary(summaryLines(lessons, reasons, audit, total, written, dropped))
	return 0, nil
}

// stripReasonCR strips CR/LF from a model-supplied curator reason before it
// enters the line-based bounded summary (P3, R16 review pass 3 at 27014da):
// the reason is free model text, and a \n or \r in it would corrupt the
// one-line-per-lesson summary layout (a two-line reason would read as a
// phantom second lesson line and break the per-lesson parsing the operator
// and the review do against the termination message). The client already
// strips CR/LF (pkg/curator); this is defence in depth at the summary's
// single entry point, so a reason that reaches the summary from any path
// is single-line. Spaces are NOT collapsed otherwise (the reason is
// recorded verbatim except for CR/LF).
func stripReasonCR(reason string) string {
	reason = strings.ReplaceAll(reason, "\r\n", " ")
	reason = strings.ReplaceAll(reason, "\n", " ")
	return strings.ReplaceAll(reason, "\r", " ")
}

// annotateDedup marks the survivors that were NOT written (duplicates) as
// "dropped duplicate" in the reasons slice (the per-lesson drop reason for
// the summary). The survivors are the input positions whose reason is ""
// (Screen preserves input order; a screen-drop has a non-empty reason). For
// each survivor, its sha256 file name is computed from the LESSON TEXT
// (lessons[i]); if that name is NOT in writtenNames (the files actually
// written), the lesson was a duplicate (its sha256 matched an existing file)
// and its reason is set to "duplicate". A survivor that WAS written keeps its
// "" reason (the summary reports it as "written").
func annotateDedup(lessons, reasons []string, writtenNames []string) {
	if len(writtenNames) == 0 && countSurvivors(reasons) == 0 {
		return
	}
	writtenSet := make(map[string]bool, len(writtenNames))
	for _, n := range writtenNames {
		writtenSet[n] = true
	}
	for i := range reasons {
		if reasons[i] != "" && !strings.HasPrefix(reasons[i], "curator-keep: ") {
			continue // screen-dropped or curator-dropped (not a survivor)
		}
		// A survivor (reason == "" or "curator-keep: ..."): compute its
		// sha256 file name from the lesson text. A curator-keep reason is
		// a survivor (the curator kept it); a dedup override must replace
		// it with "dropped duplicate" (the file was not written).
		// A survivor: compute its sha256 file name from the lesson text.
		sum := sha256.Sum256([]byte(lessons[i]))
		name := hex.EncodeToString(sum[:]) + ".txt"
		if !writtenSet[name] {
			reasons[i] = "duplicate"
		}
	}
}

// countSurvivors returns the number of lessons whose reason is "" (the
// survivors).
func countSurvivors(reasons []string) int {
	n := 0
	for _, r := range reasons {
		// A survivor has an empty reason (no curator yet) or a curator-keep
		// reason (the curator kept it — still a survivor, not yet deduped).
		if r == "" || strings.HasPrefix(r, "curator-keep: ") {
			n++
		}
	}
	return n
}

// summaryLines builds the bounded summary's lines (one line per lesson with
// its sha256-prefix, written|dropped, and rule, plus the counts line).
// It returns ONLY lines — the lesson text is NEVER in a line. This is the
// pure content of the summary (the emission — stdout + the termination
// message file — is in emitSummary).
func summaryLines(lessons, reasons, audit []string, total, written, dropped int) []string {
	out := make([]string, 0, len(lessons)+len(audit)+1)
	// Order matters: the Loop's bounded audit text keeps whole lines from the
	// START of the summary up to ~1000 bytes (memoryWriteAuditText). So the
	// COUNTS line is FIRST (the one line every other line is checked against,
	// F78), then the audit lines ("evicted <sha12> cap", batch truncation),
	// then the per-lesson lines, which are the ones cut first.
	out = append(out, summaryLine(total, written, dropped))
	out = append(out, audit...)
	for i, lesson := range lessons {
		status := "written"
		rule := "eligible"
		if i < len(reasons) && reasons[i] != "" {
			// A non-empty reason is either a drop (curator/screen/duplicate)
			// or a keep with a curator reason (curator-keep: ...).
			// A keep reason does NOT change the status to "dropped".
			if !strings.HasPrefix(reasons[i], "curator-keep: ") {
				status = "dropped"
			}
			// The reason is model- or screen-supplied text; CR/LF in a
			// curator reason would corrupt the line-based summary (P3).
			rule = stripReasonCR(reasons[i])
		}
		out = append(out, fmt.Sprintf("lesson-%03d %s %s %s", i+1, shaPrefix(lesson), status, rule))
	}
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

// writeFile is os.WriteFile; a seam so a spec can fail a write after others succeeded.
var writeFile = os.WriteFile

// writeLessons writes each survivor to <memoryDir>/<sha256>.txt (the file is
// named by the lesson's SHA-256 — the plan: "writes each curator-approved
// lesson as one file named by its SHA-256"). It creates the directory if
// absent (the memory PVC is mounted at /memory; a missing dir is a write
// failure — the mount is not writable).
//
// Dedup (the plan: "a lesson whose SHA-256 matches an existing lesson is
// dropped"): before writing, the existing files in the memory dir are listed;
// a lesson whose sha256 matches an existing file's name (<sha256>.txt) is
// dropped as "duplicate" (it is already in memory). The dedup is against the
// FILE NAMES (which are the sha256), not the file contents (the content IS
// the lesson; the name IS its hash).
//
// Returns (written, dupes, error). written is the number of lessons written;
// dupes is the number of survivors that were dropped as duplicates (their
// sha256 matched an existing file). The caller uses dupes to mark the
// duplicate lessons in the summary.
func writeLessons(dir string, survivors []string) (int, []string, []string, error) {
	if len(survivors) == 0 {
		return 0, nil, nil, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, nil, nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	// List the existing files (the dedup set: the sha256 names already in
	// memory). A read failure of the dir is a write failure (the mount is
	// not readable — treat it as a write error).
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("list %s: %w", dir, err)
	}
	existing := make(map[string]bool, len(entries))
	for _, e := range entries {
		existing[e.Name()] = true
	}
	written := 0
	writtenNames := make([]string, 0, len(survivors))
	for _, lesson := range survivors {
		sum := sha256.Sum256([]byte(lesson))
		name := hex.EncodeToString(sum[:]) + ".txt"
		if existing[name] {
			// A lesson whose sha256 matches an existing file is a duplicate:
			// dropped (the plan: "a lesson whose SHA-256 matches an existing
			// lesson is dropped"). The summary reports it as a duplicate drop.
			continue
		}
		// The lesson is single-line (the screen enforces that) and ≤500
		// chars (the size cap), so a single-line write is safe. A trailing
		// newline is appended (the memory-reader reads the file's content;
		// the trailing newline is a conventional text-file terminator and
		// the reader trims it).
		path := dir + "/" + name
		if err := writeFile(path, []byte(lesson+"\n"), 0o644); err != nil {
			return written, nil, nil, fmt.Errorf("write %s: %w", name, err)
		}
		existing[name] = true
		written++
		writtenNames = append(writtenNames, name)
	}
	// D71 (a): the per-PVC lesson cap. Evict the oldest (by mtime) AFTER every
	// write succeeded, so a failed write never leaves lessons deleted with no
	// audit (a write error returns above, before anything is evicted). The
	// pre-write snapshot is the candidate set, so a lesson written just now is
	// never evicted.
	evicted, err := evictOldest(dir, entries, survivors, memorywrite.MaxLessons)
	if err != nil {
		// Non-fatal and loud: the lessons are written; some evictions may have
		// happened and are still audited (evicted holds the removed names).
		fmt.Fprintf(os.Stderr, "memory-write: %v (non-fatal; the lessons were written)\n", boundedError(err))
	}
	return written, writtenNames, evicted, nil
}

// evictOldest enforces the D71 cap: if the existing lesson files plus the new
// (non-duplicate) survivors would exceed max, it removes the oldest existing
// lessons by mtime (ties broken by name, so it is deterministic) until they
// fit. A file whose name equals a survivor's is never evicted (that survivor is
// a duplicate and its file is the lesson). Only <sha256>.txt regular files
// count. Returns the evicted file names.
func evictOldest(dir string, entries []os.DirEntry, survivors []string, max int) ([]string, error) {
	type lf struct {
		name string
		mod  time.Time
	}
	survivorNames := make(map[string]bool, len(survivors))
	for _, l := range survivors {
		sum := sha256.Sum256([]byte(l))
		survivorNames[hex.EncodeToString(sum[:])+".txt"] = true
	}
	var files []lf
	existingSet := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir() || !isLessonFileName(e.Name()) {
			continue // only <sha256>.txt files are lessons: anything else is never evicted
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		existingSet[e.Name()] = true
		if !survivorNames[e.Name()] {
			files = append(files, lf{e.Name(), info.ModTime()})
		}
	}
	newCount := 0
	for n := range survivorNames {
		if !existingSet[n] {
			newCount++
		}
	}
	over := len(existingSet) + newCount - max
	if over <= 0 {
		return nil, nil
	}
	slices.SortFunc(files, func(a, b lf) int {
		if c := a.mod.Compare(b.mod); c != 0 {
			return c
		}
		return cmp.Compare(a.name, b.name)
	})
	var evicted []string
	for _, f := range files {
		if len(evicted) >= over {
			break
		}
		if err := os.Remove(dir + "/" + f.name); err != nil {
			return evicted, fmt.Errorf("evict %s: %w", f.name, err)
		}
		evicted = append(evicted, f.name)
	}
	return evicted, nil
}

// shaPrefix returns the first shaPrefixLen hex chars of the lesson's
// sha256 (the bounded identifier: enough to correlate a lesson in the
// condition/history without leaking its content).
func shaPrefix(lesson string) string {
	sum := sha256.Sum256([]byte(lesson))
	return hex.EncodeToString(sum[:])[:shaPrefixLen]
}

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

// markBatchDuplicates marks every survivor (reason "") whose text already
// appeared as an earlier survivor in the batch as "duplicate", and returns the
// remaining survivors in input order (the lessons are already trimmed).
func markBatchDuplicates(lessons, reasons []string) []string {
	seen := make(map[string]bool, len(lessons))
	out := make([]string, 0, len(lessons))
	for i, l := range lessons {
		if i >= len(reasons) || reasons[i] != "" {
			continue
		}
		if seen[l] {
			// "duplicate" of the earlier copy: marked before the curator judges
			// the original, so if the curator drops it this reads "duplicate"
			// of a lesson that was never written. The counts still agree.
			reasons[i] = "duplicate"
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

// isLessonFileName reports whether name is a lesson file: 64 lowercase hex
// characters plus ".txt" (the writer's own naming).
func isLessonFileName(name string) bool {
	h, ok := strings.CutSuffix(name, ".txt")
	if !ok || len(h) != 64 {
		return false
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
