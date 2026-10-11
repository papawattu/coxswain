package cli

import (
	"bytes"
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// makeLoop builds a minimal *unstructured.Unstructured Loop for the tests.
func makeLoop(name, phase string, iteration int) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "coxswain.wattu.com/v1alpha1",
			"kind":       "Loop",
			"metadata":   map[string]any{"name": name, "namespace": "samples"},
			"spec":       map[string]any{"goal": "make the thing work", "suspend": false},
			"status": map[string]any{
				"phase":     phase,
				"iteration": iteration,
			},
		},
	}
}

// unstructuredSet sets a nested status-level field on a test Loop.
func unstructuredSet(u *unstructured.Unstructured, key string, value any) {
	if err := unstructured.SetNestedField(u.Object, value, "status", key); err != nil {
		panic(err)
	}
}

// runCmd invokes a command's core action with a captured output buffer.
func runCmd(t *testing.T, deps Deps, run func(ctx context.Context, dc Deps, ns, name string, out *bytes.Buffer) error, args ...string) string {
	t.Helper()
	out := &bytes.Buffer{}
	if err := run(context.Background(), deps, "samples", args[0], out); err != nil {
		t.Fatalf("command %v: %v", args, err)
	}
	return out.String()
}
