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

// Package tamper runs the TamperedVerify check: it lists the protected paths
// that differ between the operator-pinned baseCommit and verifiedCommit via a
// `git diff --name-only` over a real checkout. The verify image's tamper init
// container runs this as a binary BEFORE any acceptance check or agent code
// runs (ADR-0005 D14/D16).
//
// All preset globs are emitted with git's `:(glob)` magic so that patterns
// like `**/*_test.go` match BOTH nested (pkg/b_test.go) AND repo-root
// (add_test.go) files. Under git's default pathspec, `**/*_test.go` matches
// only nested files and misses root-level tests — exactly where a single-
// package Go repo keeps its tests (D25).
package tamper

import (
	"fmt"
	"os/exec"
	"strings"
)

// Preset is a per-language protected-path glob set name (spec.verify.preset).
type Preset string

const (
	// PresetGo is the default: Go test files, testdata, and the module files
	// a check running `go test` would read.
	PresetGo Preset = "go"
	// PresetNone uses only the operator's explicit protectedPaths.
	PresetNone Preset = "none"
)

// goPresetGlobs is the Go preset, emitted with `:(glob)` magic so repo-root
// files match (D25). Each entry is a full git pathspec magic string.
var goPresetGlobs = []string{
	":(glob)**/*_test.go",   // matches pkg/b_test.go AND root add_test.go
	":(glob)**/testdata/**", // matches pkg/testdata/x AND root testdata/y
	":(glob)go.mod",
	":(glob)go.sum",
}

// ExpandPreset returns the full protected-path glob set for the given preset
// plus the operator's extra protectedPaths. This is the single place the
// preset is expanded (D25.1):
//   - PresetGo (or empty, the CRD default) -> goPresetGlobs + magic-ized extras.
//   - PresetNone or override=true -> only the extras (magic-ized).
//
// Extra paths are passed through as-is if they already carry a git pathspec
// magic (a leading ":"), otherwise wrapped in `:(glob)` so they match root
// files too (e.g. "Makefile" -> ":(glob)Makefile").
func ExpandPreset(preset Preset, extra []string, override bool) []string {
	var base []string
	if preset == PresetNone || override {
		base = nil
	} else {
		base = goPresetGlobs
	}
	globs := make([]string, 0, len(base)+len(extra))
	globs = append(globs, base...)
	for _, e := range extra {
		globs = append(globs, magicGlob(e))
	}
	return globs
}

// magicGlob wraps a user glob in `:(glob)` if it doesn't already carry a git
// pathspec magic (a leading ":"). A bare "Makefile" would otherwise be a
// literal pathspec that only matches the root file; `:(glob)Makefile` is
// explicit and matches the root file. Patterns that are already a magic
// (e.g. ":(top)Makefile" or "Makefile" intended as literal) are returned
// unchanged so the operator keeps full control.
func magicGlob(g string) string {
	if strings.HasPrefix(g, ":") {
		return g // already a git pathspec magic
	}
	return ":(glob)" + g
}

// Changed lists the protected paths that differ between base and verified in
// the repo at repoDir. It runs `git -C repoDir diff --name-only <base>
// <verified> -- <globs...>` and returns the changed paths (empty slice = clean).
// An empty result with a non-zero exit is an error (the diff itself failed);
// a zero exit with an empty list is a clean (no protected path changed).
func Changed(repoDir, base, verified string, globs []string) ([]string, error) {
	args := []string{"-C", repoDir, "diff", "--name-only", base, verified}
	if len(globs) > 0 {
		args = append(args, "--")
		args = append(args, globs...)
	}
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		// git diff returns non-zero on usage/IO errors. A legitimate "no
		// changes" is exit 0 with empty output, so any error here is real.
		return nil, fmt.Errorf("git diff %s..%s: %w: %s", base, verified, err, strings.TrimSpace(string(out)))
	}
	changed := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(changed) == 1 && changed[0] == "" {
		return nil, nil
	}
	return changed, nil
}
