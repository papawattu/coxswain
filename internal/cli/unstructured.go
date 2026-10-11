package cli

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// loopUnstructured casts a runtime.Object (from LoopClient.Get) to an
// *unstructured.Unstructured.
func loopUnstructured(obj runtime.Object) (*unstructured.Unstructured, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("cli: expected *unstructured.Unstructured, got %T", obj)
	}
	return u, nil
}

// getLoop fetches the named Loop and returns it as unstructured.
func getLoop(ctx context.Context, dc Deps, name string) (*unstructured.Unstructured, error) {
	obj, err := dc.Loops.Get(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("get loop %q: %w", name, err)
	}
	return loopUnstructured(obj)
}

// getNested reads a nested field (e.g. "status", "phase") from an
// *unstructured.Unstructured. Returns (nil,false) when absent.
func getNested(u *unstructured.Unstructured, fields ...string) (any, bool) {
	v, found, _ := unstructured.NestedFieldNoCopy(u.Object, fields...)
	return v, found
}

// getNestedString reads a nested string field.
func getNestedString(u *unstructured.Unstructured, fields ...string) (string, bool) {
	v, found, _ := unstructured.NestedString(u.Object, fields...)
	if !found {
		return "", false
	}
	return v, true
}

// getNestedInt reads a nested int field (0 when absent).
func getNestedInt(u *unstructured.Unstructured, fields ...string) int {
	v, found := getNested(u, fields...)
	if !found {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// getNestedBool reads a nested bool field.
func getNestedBool(u *unstructured.Unstructured, fields ...string) (bool, bool) {
	v, found, _ := unstructured.NestedBool(u.Object, fields...)
	return v, found
}

// getNestedSlice reads a nested []any field.
func getNestedSlice(u *unstructured.Unstructured, fields ...string) ([]any, bool) {
	v, found, _ := unstructured.NestedSlice(u.Object, fields...)
	return v, found
}

// getNestedMap reads a nested map[string]any field.
func getNestedMap(u *unstructured.Unstructured, fields ...string) (map[string]any, bool) {
	v, found, _ := unstructured.NestedMap(u.Object, fields...)
	return v, found
}
