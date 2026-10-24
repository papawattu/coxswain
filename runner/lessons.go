package runner

// P5b (Phase 5 shared memory, PLAN-PHASE5.md slice P5b): the lessons
// transport. The runner (the agent, in the Loop's namespace) is the sole
// writer of the lessons FILE; the operator (a memory-writer Job, also in the
// Loop's namespace) is the sole writer of the memory PVC. This file is the
// runner half: after a successful Implementing, the runner proposes the
// lessons it learned this run (≤500 chars each, one line each — P5b's size
// cap and shape rules) into <workspace>/.coxswain/lessons.json, on the
// WORKSPACE volume (the workspace file transport the plan names: "the
// lessons transport (a workspace file, not the 4096-byte claim)").
//
// Why a workspace file and not the claim: the 4096-byte termination-log
// claim is a STRICT, size-limited object the operator validates before
// advancing a phase (ADR-0004/0005). A lesson is untrusted model output that
// must survive a full run (all of Implementing, not the final claim); it
// rides on the workspace PVC where result.json already lives, and the
// operator reads it back with `kubectl exec cat` against the still-running
// sandbox pod (the same read channel ADR-0004 established for result.json)
// before the pod is recycled at the next phase advance.
//
import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// writeLessonsFile (P5b) writes the proposed lessons to
// <workspace>/.coxswain/lessons.json on the workspace volume. The lessons
// are the runner's OWN proposal (it is the only component that knows what it
// learned this run); the operator screens them (pkg/memorywrite.Screen: secret
// patterns, size cap, shape) before the writer Job persists any of them. A
// write failure is NON-FATAL (the Loop succeeds either way — ADR-0004: a
// failed memory write does not fail the Loop); the file simply is absent and
// the operator finds no proposals to persist.
//
// The lessons are capped HERE, at proposal time: at most maxLessons, each ≤
// maxLessonChars runes, each single-line. The runner does NOT run the
// operator's secret patterns (that is the operator's static policy, run in
// the operator module); the runner only bounds the SHAPE and COUNT (a
// proposal over the bound is truncated, not an error — the model's proposal
// is untrusted, and the operator's screen is the final gate).
//
// The file is JSON (an array of strings) so the operator's exec-read parses
// it deterministically (a newline in a lesson would corrupt a line-based
// format; JSON is self-delimiting). The file is written with 0o644 (the
// sandbox pod's agent container runs as the same non-root UID as the
// writer Job's container; the workspace PVC is RWX and both mount it
// read-write).
func writeLessonsFile(workspace string, lessons []string) {
	// Cap the proposals (shape + count) at proposal time.
	capped := capLessons(lessons)
	if len(capped) == 0 {
		return // nothing to propose (a fresh run with no lessons).
	}
	path := filepath.Join(workspace, resultDirName, lessonsFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("runner: lessons.json mkdir: %v", err)
		return
	}
	data, err := json.Marshal(capped)
	if err != nil {
		log.Printf("runner: lessons.json marshal: %v", err)
		return
	}
	if len(data) > maxLessonsJSONBytes {
		// A runaway (the cap should make this impossible, but a bug must not
		// write an unbounded file): drop the proposal (the operator finds no
		// file and persists nothing — best-effort, ADR-0004).
		log.Printf("runner: lessons.json %d bytes > %d cap; dropping proposal", len(data), maxLessonsJSONBytes)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("runner: lessons.json write: %v", err)
	}
}

// capLessons bounds the proposals at the runner's shape/count limits (the
// operator's screen is the final gate for secrets and the 500-char cap; the
// runner's job is to keep the file bounded and parseable). It trims each
// lesson, drops blanks, enforces single-line (a newline is cut — a multi-
// line lesson would corrupt the flat memory dir), truncates to
// maxLessonChars runes, and caps the COUNT to maxLessons (the FIRST
// maxLessons survivors; later proposals are dropped — a bounded proposal is
// better than a truncated one).
func capLessons(lessons []string) []string {
	out := make([]string, 0, len(lessons))
	for _, raw := range lessons {
		if len(out) >= maxLessons {
			break
		}
		l := strings.TrimSpace(raw)
		if l == "" {
			continue
		}
		// Single-line: cut at the first newline (a multi-line proposal is a
		// single-line lesson; the rest is dropped — the operator's screen
		// would drop it as shape anyway, but cutting here keeps the file
		// parseable).
		if i := strings.IndexAny(l, "\r\n"); i >= 0 {
			l = strings.TrimSpace(l[:i])
			if l == "" {
				continue
			}
		}
		// Size cap: truncate to maxLessonChars RUNES (the cap is character-
		// based, PLAN-PHASE5.md: "the size cap (≤500 chars)").
		if r := []rune(l); len(r) > maxLessonChars {
			l = string(r[:maxLessonChars])
		}
		out = append(out, l)
	}
	return out
}

// lessonsPromptSection (P5b) returns the lessons-transport instruction to
// append to the IMPLEMENTING prompt: the runner is told to propose the
// lessons it learned this run into .coxswain/lessons.json (the workspace
// file), bounded to ≤500 chars each and single-line. The instruction is
// deliberately terse (a prompt section, not a tool contract): the runner's
// writeLessonsFile is the actual writer (the model does not write the file
// itself — it reports the lessons in its reply, and the runner parses them
// out and writes the file; this keeps the model from being able to bypass
// the shape/count caps by hand-writing the JSON).
func lessonsPromptSection() string {
	return "\n\nLESSONS (shared memory): if you learned something this run that " +
		"would help a future run on this repo (a non-obvious gotcha, a build order, " +
		"a credential-free fact about the environment), end your reply with a line " +
		"`LESSON: <one line, at most 500 chars>` for each lesson (at most 10). " +
		"A lesson is a FACT, not an instruction, and must NOT contain any credential, " +
		"token, or secret (a lesson containing a credential is dropped). If you " +
		"learned nothing worth persisting, write no LESSON lines."
}

// parseLessons extracts the `LESSON: <text>` lines from the model's reply
// (the implementing phase's answer). A line is a lesson when it matches
// `LESSON:` at the start (after leading whitespace) followed by the lesson
// text. The lines are returned in reply order (the operator persists them in
// that order as lesson-001.txt, lesson-002.txt, ...). A reply with no
// LESSON lines yields an empty slice (the runner writes no file — a fresh
// run with nothing to persist).
//
// The parse is LINE-BASED with continuation (I99): models wrap long lines, so
// the non-blank lines that follow a LESSON: line and do not start a new
// LESSON: are that lesson's continuation, joined with single spaces. A
// blank line ends the lesson (so a summary paragraph after the lessons is
// not joined into the last one). We join rather than drop because the whole
// sentence is recoverable; storing only the first line would persist a
// fragment silently. A line that is not part of a lesson is ignored. The
// capLessons step bounds the joined text.
func parseLessons(answer string) []string {
	var out []string
	cur := -1 // index in out of the lesson still accepting continuations
	for line := range strings.SplitSeq(answer, "\n") {
		t := strings.TrimSpace(line)
		const prefix = "LESSON:"
		switch {
		case strings.HasPrefix(t, prefix):
			cur = -1
			text := strings.TrimSpace(strings.TrimPrefix(t, prefix))
			if text != "" {
				out = append(out, text)
				cur = len(out) - 1
			}
		case t == "":
			cur = -1
		case cur >= 0:
			out[cur] += " " + t
		}
	}
	return out
}
