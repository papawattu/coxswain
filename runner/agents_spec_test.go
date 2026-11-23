package runner

import (
	"path/filepath"
	"testing"
)

// Every agent image's engine spec (agents/<name>/engine.json) loads and
// validates, so a broken spec fails the runner's tests, not a kind run.
func TestAgentEngineSpecsLoad(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "agents", "*", "engine.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"pi": true, "opencode": true, "codex": true, "claude-code": true, "hermes": true}
	if len(paths) != len(want) {
		t.Fatalf("found %d engine specs %v; want %d", len(paths), paths, len(want))
	}
	for _, p := range paths {
		s, err := LoadEngineSpec(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		dir := filepath.Base(filepath.Dir(p))
		if s.Name != dir || !want[dir] {
			t.Errorf("%s: name %q; want the directory name, one of %v", p, s.Name, want)
		}
	}
}
