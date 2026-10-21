// Package dockerguard is a guard test (AGENTS.md test norm: a hack script or
// Go test with an execution test) that fails when a root-module package
// imported by cmd/ is NOT in the operator image's docker build context
// (.dockerignore excludes it). The P5a regression (origin/main 653347f):
// pkg/memkey was added to the root module and imported by internal/controller
// (which cmd imports), but .dockerignore had no !pkg exception, so the docker
// build context lacked pkg/ and the operator image failed to build
// ("no required module provides package .../pkg/memkey"). The guard catches
// that class of breakage: a root-module package imported by cmd/ must be
// included in the docker context.
package dockerguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// modulePath is this repo's root-module path (the go.mod module line). It is
// the prefix of every root-module package's import path.
const modulePath = "github.com/papawattu/coxswain"

// repoRoot returns the repo root (two levels above this test file:
// hack/dockerguard -> hack -> <root>).
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// This test lives in <root>/hack/dockerguard.
	root := filepath.Join(filepath.Dir(wd), "..")
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	return abs
}

// importedRootModulePackages returns the set of root-module packages that cmd
// (the operator entrypoint) transitively imports, as import paths
// (github.com/papawattu/coxswain/...). It uses `go list -deps ./cmd` (the
// real import graph — not a hand-maintained list, so a new import is picked
// up automatically).
func importedRootModulePackages(t *testing.T, root string) map[string]bool {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", "./cmd")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd: %v (stderr: %s)", err, err)
	}
	pkgs := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, modulePath) {
			pkgs[line] = true
		}
	}
	return pkgs
}

// pkgImported returns true when cmd/ imports at least one root-module package
// under pkg/ (i.e. the operator binary depends on the pkg/ directory).
func pkgImported(imported map[string]bool) bool {
	prefix := modulePath + "/pkg/"
	for p := range imported {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// dockerignoreIncludesPkg returns true when the .dockerignore has an
// exception that includes the pkg/ directory in the build context (a `!pkg`
// or `!pkg/**` line). The .dockerignore model is "exclude everything (**),
// then re-include via ! exceptions", so pkg/ is in the context iff a !
// exception matches it.
func dockerignoreIncludesPkg(t *testing.T, root string) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".dockerignore"))
	if err != nil {
		t.Fatalf("read .dockerignore: %v", err)
	}
	reIncludesPkg := regexp.MustCompile(`(?m)^!\s*pkg(\s*$|/|\s)`)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		// The exception must include pkg/ : "!pkg", "!pkg/**", "!pkg/**/*",
		// or "!pkg/...". A comment (#) or an exclusion (no !) does not count.
		if !strings.HasPrefix(line, "!") {
			continue
		}
		if reIncludesPkg.MatchString(line) {
			return true
		}
	}
	return false
}

// TestCmdImportsPkgRequiresDockerignorePkg is the guard: when cmd/ imports a
// root-module package under pkg/, the .dockerignore MUST include pkg/ in the
// docker build context (a !pkg or !pkg/** exception). If pkg/ is imported but
// the !pkg exception is absent (or removed), the operator image's build
// context lacks pkg/ and the image cannot build — this test fails.
//
// Mutation (AGENTS.md norm): remove the !pkg line from .dockerignore and this
// test FAILS (pkg/ is imported by cmd/ but not in the docker context).
func TestCmdImportsPkgRequiresDockerignorePkg(t *testing.T) {
	root := repoRoot(t)
	imported := importedRootModulePackages(t, root)
	if !pkgImported(imported) {
		t.Skip("cmd/ does not import a root-module pkg/ package; the guard is a no-op (nothing to guard)")
	}
	if !dockerignoreIncludesPkg(t, root) {
		t.Fatalf(
			".dockerignore does not include pkg/ in the docker build context, but cmd/ imports a root-module pkg/ package. "+
				"The operator image will fail to build (no required module provides package %s). "+
				"Add a !pkg (or !pkg/**) exception to .dockerignore.",
			modulePath+"/pkg/...",
		)
	}
}
