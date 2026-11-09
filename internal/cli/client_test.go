package cli

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func cmWithOwnerRefs(t *testing.T, refs []map[string]any) *unstructured.Unstructured {
	t.Helper()
	arr := make([]any, 0, len(refs))
	for _, r := range refs {
		arr = append(arr, r)
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"ownerReferences": arr},
	}}
	return u
}

func ref(kind, uid string, controller bool) map[string]any {
	name := "loop-a"
	m := map[string]any{"kind": kind, "name": name, "uid": uid}
	if controller {
		m["controller"] = true
	} else {
		m["controller"] = false
	}
	return m
}

func TestLearnSummaryOwnedBy(t *testing.T) {
	const loopName = "loop-a"
	const loopUID = "uid-1"

	tt := []struct {
		name string
		cm   runtime.Object
		want bool
	}{
		{
			name: "right controller ref",
			cm:   cmWithOwnerRefs(t, []map[string]any{ref("Loop", loopUID, true)}),
			want: true,
		},
		{
			name: "right name but different uid",
			cm:   cmWithOwnerRefs(t, []map[string]any{ref("Loop", "uid-2", true)}),
			want: false,
		},
		{
			name: "controller false",
			cm:   cmWithOwnerRefs(t, []map[string]any{ref("Loop", loopUID, false)}),
			want: false,
		},
		{
			name: "no owner refs",
			cm:   cmWithOwnerRefs(t, nil),
			want: false,
		},
		{
			name: "deployment ref with same name",
			cm:   cmWithOwnerRefs(t, []map[string]any{ref("Deployment", loopUID, true)}),
			want: false,
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			if got := learnSummaryOwnedBy(tc.cm, loopUID, loopName); got != tc.want {
				t.Errorf("learnSummaryOwnedBy() = %v, want %v", got, tc.want)
			}
		})
	}
}
