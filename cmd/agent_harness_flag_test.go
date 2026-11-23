package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestAgentHarnessFlag(t *testing.T) {
	f := newAgentHarnessFlag()
	for _, v := range []string{
		"pi=registry.example/agent-pi@sha256:abc",
		"opencode=local/agent-opencode:dev,memory=2Gi,cpu=1",
	} {
		if err := f.Set(v); err != nil {
			t.Fatalf("Set(%q) = %v", v, err)
		}
	}
	want := "opencode=local/agent-opencode:dev,memory=2Gi,cpu=1;pi=registry.example/agent-pi@sha256:abc"
	if got := f.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if q := f.limits["opencode"][corev1.ResourceMemory]; q.String() != "2Gi" {
		t.Fatalf("opencode memory limit = %s, want 2Gi", q.String())
	}
	if _, ok := f.limits["pi"]; ok {
		t.Fatal("pi has no limits given: it must keep the default")
	}
	for _, bad := range []string{
		"pi",                          // no image
		"pi=",                         // empty image
		"Pi=img",                      // not the CRD's pattern
		"-pi=img",                     // not the CRD's pattern
		"pi=img with space",           // whitespace
		"pi=other",                    // listed twice
		"x=img,memory=lots",           // not a quantity
		"x=img,memory=0",              // not positive
		"x=img,gpu=1",                 // unknown option
		"x=img,memory=1Gi,memory=2Gi", // given twice
		"a234567890123456789012345678901234567890x=img", // 41 characters
	} {
		if err := f.Set(bad); err == nil {
			t.Errorf("Set(%q) = nil; want an error", bad)
		}
	}
}
