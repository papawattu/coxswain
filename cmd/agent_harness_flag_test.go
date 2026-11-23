package main

import "testing"

func TestAgentHarnessFlag(t *testing.T) {
	f := agentHarnessFlag{}
	for _, v := range []string{"pi=registry.example/agent-pi@sha256:abc", "claude-code=local/agent-claude-code:dev"} {
		if err := f.Set(v); err != nil {
			t.Fatalf("Set(%q) = %v", v, err)
		}
	}
	want := "claude-code=local/agent-claude-code:dev,pi=registry.example/agent-pi@sha256:abc"
	if got := f.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	for _, bad := range []string{
		"pi",                // no image
		"pi=",               // empty image
		"Pi=img",            // not the CRD's pattern
		"-pi=img",           // not the CRD's pattern
		"pi=img with space", // whitespace
		"pi=a,b",            // a comma
		"pi=other",          // listed twice
		"a234567890123456789012345678901234567890x=img", // 41 characters
	} {
		if err := f.Set(bad); err == nil {
			t.Errorf("Set(%q) = nil; want an error", bad)
		}
	}
}
