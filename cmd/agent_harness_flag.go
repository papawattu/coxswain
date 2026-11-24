package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// harnessNameRE matches spec.agent.harness's CRD pattern (D147), so a flag
// entry a Loop could never name is rejected at startup, not silently unused.
var harnessNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// agentHarnessFlag is the repeatable --agent-harness flag (D147,
// docs/PLAN-AGENT-ADAPTERS.md, docs/AGENT-HARNESSES.md):
//
//	--agent-harness name=image[,memory=<quantity>][,cpu=<quantity>]
//
// It lists the agent adapters the operator runs, each pinned to an image, and
// optionally the agent container's memory and CPU limits for that adapter
// (the default is the reference runner's 512Mi and 500m; some CLIs need more).
type agentHarnessFlag struct {
	images map[string]string
	limits map[string]corev1.ResourceList
}

func newAgentHarnessFlag() *agentHarnessFlag {
	return &agentHarnessFlag{images: map[string]string{}, limits: map[string]corev1.ResourceList{}}
}

func (f *agentHarnessFlag) String() string {
	if f == nil {
		return ""
	}
	names := make([]string, 0, len(f.images))
	for k := range f.images {
		names = append(names, k)
	}
	slices.Sort(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		p := k + "=" + f.images[k]
		if q, ok := f.limits[k][corev1.ResourceMemory]; ok {
			p += ",memory=" + q.String()
		}
		if q, ok := f.limits[k][corev1.ResourceCPU]; ok {
			p += ",cpu=" + q.String()
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ";")
}

// Set parses one name=image[,memory=Q][,cpu=Q] entry.
func (f *agentHarnessFlag) Set(v string) error {
	fields := strings.Split(v, ",")
	name, image, ok := strings.Cut(fields[0], "=")
	switch {
	case !ok:
		return fmt.Errorf("--agent-harness %q: want name=image[,memory=Q][,cpu=Q]", v)
	case len(name) > 40 || !harnessNameRE.MatchString(name):
		return fmt.Errorf("--agent-harness %q: the name must match %s and be at most 40 characters", v, harnessNameRE)
	case image == "" || strings.ContainsAny(image, " \t\n"):
		return fmt.Errorf("--agent-harness %q: the image is empty or has whitespace", v)
	}
	if _, dup := f.images[name]; dup {
		return fmt.Errorf("--agent-harness %q: %s is listed twice", v, name)
	}
	limits := corev1.ResourceList{}
	for _, kv := range fields[1:] {
		k, val, ok := strings.Cut(kv, "=")
		var res corev1.ResourceName
		switch k {
		case "memory":
			res = corev1.ResourceMemory
		case "cpu":
			res = corev1.ResourceCPU
		default:
			return fmt.Errorf("--agent-harness %q: unknown option %q (want memory= or cpu=)", v, kv)
		}
		q, err := resource.ParseQuantity(val)
		if !ok || err != nil || q.Sign() <= 0 {
			return fmt.Errorf("--agent-harness %q: %s needs a positive quantity", v, k)
		}
		if _, dup := limits[res]; dup {
			return fmt.Errorf("--agent-harness %q: %s is given twice", v, k)
		}
		limits[res] = q
	}
	f.images[name] = image
	if len(limits) > 0 {
		f.limits[name] = limits
	}
	return nil
}
