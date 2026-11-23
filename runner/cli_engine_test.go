package runner

// T1 (docs/PLAN-AGENT-ADAPTERS.md): the phase contract with the CLI engine.
// The CLI is a fake (a shell script), so these specs need no model and no
// network: they assert what the adapter does around the CLI (PLAN.md, the
// commit, lessons.json, the claim, blocked reasons, killing the CLI's process
// group), which is the same for every agent.

import (
	"context"
	"encoding/json"
	"maps"

	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeCLI writes an executable shell script and returns its path. The script
// records its argv (one per line) and cwd under $FAKE_REC, then runs body.
func fakeCLI(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-agent")
	script := "#!/bin/sh\n" +
		`[ -n "$FAKE_REC" ] && { pwd > "$FAKE_REC/cwd"; ` +
		`for a in "$@"; do printf '%s\n--\n' "$a"; done > "$FAKE_REC/argv"; }` + "\n" +
		body + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// cliEngine builds a CLIEngine around a fake CLI. rec is where the fake
// records its argv; home is the engine's $HOME.
func cliEngine(t *testing.T, cli, rec, home string, mutate ...func(*EngineSpec)) CLIEngine {
	t.Helper()
	spec := EngineSpec{
		Name:    "fake",
		Command: []string{cli, "--model", "${COX_MODEL}", "${COX_PROMPT}"},
		Env:     map[string]string{"FAKE_REC": rec, "HOME": "${HOME}"},
	}
	for _, m := range mutate {
		m(&spec)
	}
	if err := spec.validate(); err != nil {
		t.Fatalf("spec: %v", err)
	}
	env := map[string]string{"COX_MODEL": "m1", "COX_MODEL_BASE_URL": "http://proxy:8080", "HOME": home}
	return CLIEngine{Spec: spec, Getenv: func(k string) string { return env[k] }}
}

func phaseRunWith(t *testing.T, ws, phase string, eng Engine, stop chan any) (Result, map[string]any) {
	t.Helper()
	claimPath, cleanup := s4ClaimPath(t)
	t.Cleanup(cleanup)
	writeDesiredPhase(t, ws, phase)
	if stop == nil {
		stop = make(chan any)
	}
	res := PhaseRun(PhaseConfig{Workspace: ws, Goal: "fix round", Model: "m1",
		PollInterval: 5 * time.Millisecond, Engine: eng}, stop)
	var claim map[string]any
	if data, err := os.ReadFile(claimPath); err == nil {
		_ = json.Unmarshal(data, &claim)
	}
	return res, claim
}

func TestCLIEnginePlanningWritesPlanFromStdout(t *testing.T) {
	rec, home, ws := t.TempDir(), t.TempDir(), t.TempDir()
	cli := fakeCLI(t, `echo "1. read round.go"; echo "2. fix Round"`)
	res, claim := phaseRunWith(t, ws, PhasePlanning, cliEngine(t, cli, rec, home), nil)

	if res.Status != statusSuccess || claim["status"] != statusSuccess || claim["observedPhase"] != PhasePlanning {
		t.Fatalf("Planning: status %q, claim %v (notes %q)", res.Status, claim, res.VerificationNotes)
	}
	plan, err := os.ReadFile(filepath.Join(ws, resultDirName, planFileName))
	if err != nil || string(plan) != "1. read round.go\n2. fix Round" {
		t.Fatalf("PLAN.md = %q, %v; want the CLI's stdout", plan, err)
	}
	argv, _ := os.ReadFile(filepath.Join(rec, "argv"))
	args := strings.Split(strings.TrimSuffix(string(argv), "\n--\n"), "\n--\n")
	if len(args) != 3 || args[0] != "--model" || args[1] != "m1" ||
		!strings.Contains(args[2], "PLANNING phase") || !strings.Contains(args[2], "fix round") {
		t.Fatalf("argv = %q; want --model m1 <the planning prompt with the goal> as one argument", args)
	}
	cwd, _ := os.ReadFile(filepath.Join(rec, "cwd"))
	if got, _ := filepath.EvalSymlinks(strings.TrimSpace(string(cwd))); got != mustEval(t, ws) {
		t.Fatalf("the CLI ran in %q; want the workspace %q", got, ws)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCLIEngineImplementingCommitsTheCLIsChangeAndLessons(t *testing.T) {
	rec, home, ws := t.TempDir(), t.TempDir(), t.TempDir()
	initTestRepo(t, ws)
	base := gitIn(t, ws, "rev-parse", "HEAD")
	// The CLI edits a file, stages an artifact itself (the adapter must reset
	// that index), writes into .coxswain (must not be committed), and ends its
	// answer with a LESSON line.
	cli := fakeCLI(t, `echo 'package main // fixed' > main.go
echo junk > artifact.bin && git add artifact.bin && rm artifact.bin
echo x > .coxswain/scratch.txt
echo "Changed main.go."
echo "LESSON: main.go needs the fixed comment"`)
	res, claim := phaseRunWith(t, ws, PhaseImplementing, cliEngine(t, cli, rec, home), nil)

	if res.Status != statusSuccess || claim["status"] != statusSuccess {
		t.Fatalf("Implementing: status %q claim %v (notes %q)", res.Status, claim, res.VerificationNotes)
	}
	head := gitIn(t, ws, "rev-parse", "HEAD")
	if head == base {
		t.Fatal("the adapter must commit the CLI's change")
	}
	files := gitIn(t, ws, "show", "--name-only", "--format=", "HEAD")
	if files != "main.go" {
		t.Fatalf("committed files = %q; want only main.go (no .coxswain, no staged artifact)", files)
	}
	lessons, err := os.ReadFile(filepath.Join(ws, resultDirName, lessonsFileName))
	if err != nil || !strings.Contains(string(lessons), "main.go needs the fixed comment") {
		t.Fatalf("lessons.json = %q, %v; want the CLI answer's LESSON line", lessons, err)
	}
}

func TestCLIEngineNonZeroExitBlocksWithTheCLIsError(t *testing.T) {
	rec, home, ws := t.TempDir(), t.TempDir(), t.TempDir()
	initTestRepo(t, ws)
	base := gitIn(t, ws, "rev-parse", "HEAD")
	cli := fakeCLI(t, `echo 'package main // half' > main.go; echo "starting" >&2; echo "provider error: 401" >&2; exit 3`)
	res, claim := phaseRunWith(t, ws, PhaseImplementing, cliEngine(t, cli, rec, home), nil)

	if res.Status != statusBlocked || claim["status"] != statusBlocked {
		t.Fatalf("a failed CLI must block: status %q claim %v", res.Status, claim)
	}
	if want := "EngineFailed: fake exit 3: provider error: 401"; res.VerificationNotes != want {
		t.Fatalf("blocked reason = %q; want %q", res.VerificationNotes, want)
	}
	if gitIn(t, ws, "rev-parse", "HEAD") != base {
		t.Fatal("a blocked Implementing must not commit")
	}
}

// pidAlive reports whether pid exists (signal 0).
func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// waitGone waits for the pid recorded at path to be dead.
func waitGone(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the fake CLI's child recorded no pid: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	deadline := time.Now().Add(10 * time.Second)
	for pidAlive(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the CLI's child %d is still alive: the whole process group must be killed", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The fake starts a background child (a tool command the agent left running)
// and records its pid, then hangs.
const hangingCLI = `sleep 300 & echo $! > "$FAKE_REC/child"; sleep 300`

func TestCLIEngineTimeoutBlocksAndKillsTheProcessGroup(t *testing.T) {
	rec, home, ws := t.TempDir(), t.TempDir(), t.TempDir()
	initTestRepo(t, ws)
	eng := cliEngine(t, fakeCLI(t, hangingCLI), rec, home, func(s *EngineSpec) { s.Timeout = "500ms" })
	res, _ := phaseRunWith(t, ws, PhaseImplementing, eng, nil)

	if res.Status != statusBlocked || res.VerificationNotes != "EngineTimeout: fake did not finish in 500ms" {
		t.Fatalf("timeout: status %q notes %q", res.Status, res.VerificationNotes)
	}
	waitGone(t, filepath.Join(rec, "child"))
}

func TestCLIEngineStopEndsThePhaseWithoutAClaim(t *testing.T) {
	rec, home, ws := t.TempDir(), t.TempDir(), t.TempDir()
	initTestRepo(t, ws)
	stop := make(chan any)
	go func() {
		// Close stop once the CLI is running (its child pid is recorded).
		for range 500 {
			if _, err := os.Stat(filepath.Join(rec, "child")); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		close(stop)
	}()
	res, claim := phaseRunWith(t, ws, PhaseImplementing, cliEngine(t, fakeCLI(t, hangingCLI), rec, home), stop)

	if res.Status != "" || claim != nil {
		t.Fatalf("a stop during the CLI run must end the phase with no claim: result %+v claim %v", res, claim)
	}
	waitGone(t, filepath.Join(rec, "child"))
}

func TestCLIEngineRendersConfigFilesUnderHome(t *testing.T) {
	rec, home, ws := t.TempDir(), t.TempDir(), t.TempDir()
	cli := fakeCLI(t, `cat "$HOME/.agent/config.json"`)
	eng := cliEngine(t, cli, rec, home, func(s *EngineSpec) {
		s.Files = []EngineFile{{Path: "${HOME}/.agent/config.json",
			Content: `{"baseURL":"${COX_MODEL_BASE_URL}/v1","model":"${COX_MODEL}","key":"unused"}`}}
	})
	res, _ := phaseRunWith(t, ws, PhasePlanning, eng, nil)
	if want := `{"baseURL":"http://proxy:8080/v1","model":"m1","key":"unused"}`; res.Summary != want {
		t.Fatalf("rendered config = %q; want %q (notes %q)", res.Summary, want, res.VerificationNotes)
	}
	if fi, err := os.Stat(filepath.Join(home, ".agent", "config.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the rendered file must be 0600: %v %v", fi, err)
	}
}

func TestCLIEngineMisconfigurationBlocks(t *testing.T) {
	cases := map[string]struct {
		mutate func(*EngineSpec)
		env    map[string]string
		want   string
	}{
		"a quote in a value rendered into a file": {
			mutate: func(s *EngineSpec) {
				s.Files = []EngineFile{{Path: "${HOME}/c.json", Content: `{"model":"${COX_MODEL}"}`}}
			},
			env:  map[string]string{"COX_MODEL": `m1","apiKey":"x`},
			want: "EngineMisconfigured: fake: file ",
		},
		"a file outside HOME and /tmp": {
			mutate: func(s *EngineSpec) { s.Files = []EngineFile{{Path: "/etc/evil", Content: "x"}} },
			want:   "EngineMisconfigured: fake: file path \"/etc/evil\" is not under $HOME or /tmp",
		},
		"an unknown variable": {
			mutate: func(s *EngineSpec) { s.Command = append(s.Command, "${OPENAI_API_KEY}") },
			want:   "EngineMisconfigured: fake: command \"${OPENAI_API_KEY}\": unknown variable ${OPENAI_API_KEY}",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec, home, ws := t.TempDir(), t.TempDir(), t.TempDir()
			eng := cliEngine(t, fakeCLI(t, `echo ran > "$FAKE_REC/ran"; echo plan`), rec, home, tc.mutate)
			env := map[string]string{"COX_MODEL": "m1", "COX_MODEL_BASE_URL": "http://proxy:8080", "HOME": home}
			maps.Copy(env, tc.env)
			eng.Getenv = func(k string) string { return env[k] }
			res, _ := phaseRunWith(t, ws, PhasePlanning, eng, nil)
			if res.Status != statusBlocked || !strings.HasPrefix(res.VerificationNotes, tc.want) {
				t.Fatalf("status %q notes %q; want blocked with prefix %q", res.Status, res.VerificationNotes, tc.want)
			}
			if _, err := os.Stat(filepath.Join(rec, "ran")); err == nil {
				t.Fatal("a misconfigured spec must not run the CLI")
			}
		})
	}
}

func TestCLIEngineAnswerFromFileIgnoresAStaleFile(t *testing.T) {
	rec, home, ws := t.TempDir(), t.TempDir(), t.TempDir()
	answer := filepath.Join(home, "last.txt")
	if err := os.WriteFile(answer, []byte("stale plan from an earlier run"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := func(s *EngineSpec) { s.Answer, s.AnswerPath = "file", "${HOME}/last.txt" }

	// A CLI that writes the answer file: its content is the answer, not stdout.
	eng := cliEngine(t, fakeCLI(t, `echo noise; echo "fresh plan" > "$HOME/last.txt"`), rec, home, spec)
	if res, _ := phaseRunWith(t, ws, PhasePlanning, eng, nil); res.Summary != "fresh plan" {
		t.Fatalf("answer = %q; want the answer file's content", res.Summary)
	}

	// A CLI that exits 0 without writing it: blocked, never the stale file.
	if err := os.WriteFile(answer, []byte("stale plan from an earlier run"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws2 := t.TempDir()
	eng = cliEngine(t, fakeCLI(t, `echo noise`), rec, home, spec)
	res, _ := phaseRunWith(t, ws2, PhasePlanning, eng, nil)
	if res.Status != statusBlocked || !strings.Contains(res.VerificationNotes, "wrote no answer file") {
		t.Fatalf("status %q notes %q; want blocked: no answer file", res.Status, res.VerificationNotes)
	}
}

func TestCLIEnginePromptTooLargeForArgvBlocks(t *testing.T) {
	eng := CLIEngine{Spec: EngineSpec{Name: "fake", Command: []string{"true", "${COX_PROMPT}"}},
		Getenv: func(string) string { return "" }}
	r := eng.Run(context.Background(), EngineRequest{Phase: PhasePlanning,
		Prompt: strings.Repeat("x", maxPromptArgBytes+1), Workspace: t.TempDir()})
	if !strings.HasPrefix(r.Err, "EngineMisconfigured: fake: the prompt is ") {
		t.Fatalf("err = %q", r.Err)
	}
}

func TestLoadEngineSpecRejects(t *testing.T) {
	cases := map[string]string{
		"unknown field":              `{"name":"a","command":["x"],"commnd":["y"]}`,
		"empty command":              `{"name":"a","command":[]}`,
		"prompt in a file":           `{"name":"a","command":["x"],"files":[{"path":"/tmp/p","content":"${COX_PROMPT}"}]}`,
		"prompt in env":              `{"name":"a","command":["x"],"env":{"P":"${COX_PROMPT}"}}`,
		"answer file without a path": `{"name":"a","command":["x"],"answer":"file"}`,
		"bad timeout":                `{"name":"a","command":["x"],"timeout":"soon"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "engine.json")
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadEngineSpec(p); err == nil {
				t.Fatalf("LoadEngineSpec(%s) = nil error", body)
			}
		})
	}
	p := filepath.Join(t.TempDir(), "engine.json")
	ok := `{"name":"pi","command":["pi","-p","${COX_PROMPT}"],"timeout":"30m"}`
	if err := os.WriteFile(p, []byte(ok), 0o644); err != nil {
		t.Fatal(err)
	}
	if s, err := LoadEngineSpec(p); err != nil || s.Name != "pi" {
		t.Fatalf("a valid spec: %+v %v", s, err)
	}

}
