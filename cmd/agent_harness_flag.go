package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// harnessNameRE matches spec.agent.harness's CRD pattern (D147), so a flag
// entry a Loop could never name is rejected at startup, not silently unused.
var harnessNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// agentHarnessFlag is the repeatable --agent-harness name=image flag (D147,
// docs/PLAN-AGENT-ADAPTERS.md): the agent adapters the operator lists, each
// pinned to an image. A Loop picks one with spec.agent.harness.
type agentHarnessFlag map[string]string

func (f agentHarnessFlag) String() string {
	names := make([]string, 0, len(f))
	for k := range f {
		names = append(names, k)
	}
	slices.Sort(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		parts = append(parts, k+"="+f[k])
	}
	return strings.Join(parts, ",")
}

// Set parses one name=image entry.
func (f agentHarnessFlag) Set(v string) error {
	name, image, ok := strings.Cut(v, "=")
	switch {
	case !ok:
		return fmt.Errorf("--agent-harness %q: want name=image", v)
	case len(name) > 40 || !harnessNameRE.MatchString(name):
		return fmt.Errorf("--agent-harness %q: the name must match %s and be at most 40 characters", v, harnessNameRE)
	case image == "" || strings.ContainsAny(image, " \t\n,"):
		return fmt.Errorf("--agent-harness %q: the image is empty or has whitespace or a comma", v)
	}
	if _, dup := f[name]; dup {
		return fmt.Errorf("--agent-harness %q: %s is listed twice", v, name)
	}
	f[name] = image
	return nil
}
