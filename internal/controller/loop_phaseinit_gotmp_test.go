/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// TestPhaseInitEmptiesGOTMPDir executes the REAL generated phase-init script
// (AGENTS.md: "Any shell script embedded in Go ... has an execution test: it
// runs the real generated script with only path or host constants
// substituted"). It plants a leftover go-build* dir under
// /workspace/.coxswain/gotmp (the F13 GOTMPDIR on the workspace PVC) and
// asserts the phase-init REMOVES it and recreates the empty dir: on the fixed
// 1Gi PVC (workspacePVCSpec), a run of leftover go-build* dirs from a killed /
// timed-out / `go test -work` run would otherwise grow to ENOSPC the agent in
// the repo itself — worse than the /tmp eviction F13 replaces. Go's temp dirs
// from an earlier phase are always garbage, so the rm-then-mkdir bounds them
// at every phase start.
//
// Mutation: drop the `rm -rf` line from the phase-init — a planted
// go-build* dir survives, and this test FAILS.
func TestPhaseInitEmptiesGOTMPDir(t *testing.T) {
	// The real generated script, both the empty-phase and non-empty-phase
	// branches (both carry the rm-then-mkdir).
	for _, tc := range []struct {
		name string
		s    string
	}{
		{"empty-phase", phaseInitScript("", 0, 0, "", "")},
		{"implementing", phaseInitScript(coxv1alpha1.LoopPhaseImplementing, 1, 0, "", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The script must rm the dir strictly before it mkdirs it.
			idxRm := strings.Index(tc.s, `rm -rf "${DEST}/.coxswain/gotmp"`)
			if idxRm < 0 {
				t.Fatalf("phase-init (case %s) does not rm .coxswain/gotmp; got:\n%s", tc.name, tc.s)
			}
			idxMkdir := strings.Index(tc.s, `mkdir -p "${DEST}/.coxswain/gotmp"`)
			if idxMkdir <= idxRm {
				t.Fatalf("phase-init (case %s): the mkdir of .coxswain/gotmp must come AFTER the rm (rm at %d, mkdir at %d); got:\n%s",
					tc.name, idxRm, idxMkdir, tc.s)
			}

			// Execute the REAL generated script (only the DEST path constant
			// substituted, per AGENTS.md). Plant a leftover go-build* dir
			// under the gotmp path and a file OUTSIDE gotmp the init must keep.
			dest := t.TempDir()
			gotmp := filepath.Join(dest, ".coxswain", "gotmp")
			leftover := filepath.Join(gotmp, "go-build123")
			if err := os.MkdirAll(leftover, 0o755); err != nil {
				t.Fatalf("planting leftover go-build dir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(leftover, "a.log"), []byte("leftover"), 0o644); err != nil {
				t.Fatalf("planting leftover file: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dest, ".coxswain", "desired-phase"), []byte("Planning"), 0o644); err != nil {
				t.Fatalf("planting desired-phase file: %v", err)
			}

			script := strings.Replace(tc.s, "DEST=/workspace", "DEST="+dest, 1)
			if !strings.Contains(script, "DEST="+dest) {
				t.Fatalf("the DEST constant was not substituted (case %s)", tc.name)
			}
			out, err := exec.Command("sh", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("phase-init script exited non-zero: %v\noutput:\n%s", err, out)
			}

			// The planted go-build dir is GONE and gotmp is recreated empty.
			if _, err := os.Stat(leftover); !os.IsNotExist(err) {
				t.Fatalf("leftover go-build dir %q survived the phase-init (err=%v); the rm must remove it (F13 P1 round 2)", leftover, err)
			}
			entries, err := os.ReadDir(gotmp)
			if err != nil {
				t.Fatalf(".coxswain/gotmp was not recreated by the phase-init (err=%v); the mkdir must run", err)
			}
			if len(entries) != 0 {
				t.Fatalf(".coxswain/gotmp is not empty after the phase-init: %v (a planted go-build dir must be gone)", entries)
			}
			// The phase-init only empties gotmp — it must NOT delete .coxswain
			// itself or the operator's other files (the desired-phase file).
			if _, err := os.Stat(filepath.Join(dest, ".coxswain", "desired-phase")); err != nil {
				t.Fatalf("the phase-init deleted .coxswain/desired-phase (it must only empty gotmp): %v", err)
			}
		})
	}
}
