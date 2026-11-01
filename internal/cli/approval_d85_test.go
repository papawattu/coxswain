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

// D85 (owner decision, R34): cox approve / cox reject stop patching the
// Loop's status subresource. They patch the Loop OBJECT's metadata with a
// one-shot annotation (coxswain.io/approve: <plan sha256> /
// coxswain.io/reject: <reason>) on the MAIN resource, and the operator turns
// the annotation into the PlanApproved / PlanRejected condition. The editor
// role's loops/status grant goes back to get-only (the operator is now the
// ONLY writer of the two conditions).
//
// These tests assert on the fake client's recorded requests: approve and
// reject issue exactly ONE metadata patch on the main resource (subresource
// "" — a JSON merge patch of metadata.annotations) and NO request to the
// /status subresource.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

// d85Patch asserts the recorded Patches: exactly one, on the MAIN resource
// (subresource ""), and returns its body. It also asserts NO Update was
// recorded (the main-resource write is the merge patch, not a whole-object
// PUT).
func d85Patch(t *testing.T, f *fakeLoopClient) map[string]any {
	t.Helper()
	if len(f.updates) != 0 {
		t.Fatalf("%d whole-object Update(s) recorded; D85: approve/reject must not PUT the whole object", len(f.updates))
	}
	if len(f.patches) != 1 {
		t.Fatalf("%d Patch call(s) recorded, want exactly 1 (one metadata merge patch on the main resource)", len(f.patches))
	}
	p := f.patches[0]
	if p.subresource != "" {
		t.Fatalf("patch subresource = %q, want \"\" (the main resource — NOT the /status subresource)", p.subresource)
	}
	var body map[string]any
	if err := json.Unmarshal(p.patch, &body); err != nil {
		t.Fatalf("patch body is not JSON: %v (body %q)", err, p.patch)
	}
	return body
}

// d85Annotation returns the patch body's metadata.annotations value for the
// given key (t.Fatalf when absent).
func d85Annotation(t *testing.T, body map[string]any, key string) string {
	t.Helper()
	meta, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("patch body has no metadata object: %v", body)
	}
	ann, ok := meta["annotations"].(map[string]any)
	if !ok {
		t.Fatalf("metadata has no annotations object: %v", meta)
	}
	v, ok := ann[key].(string)
	if !ok {
		t.Fatalf("annotation %q not found (or not a string): %v", key, ann)
	}
	return v
}

// TestApproveLoop_MetadataAnnotationOnly verifies D85: approve patches the
// Loop object's metadata.annotations with coxswain.io/approve: <plan
// sha256> (the same hash it put in the condition's message before D85) on
// the MAIN resource, with no request to /status.
func TestApproveLoop_MetadataAnnotationOnly(t *testing.T) {
	const plan = "step one\nstep two\n"
	const loopName = "d85-approve"

	f := newFakeLoopClient()
	f.setLoop(newFakeLoop(loopName, map[string]any{}))
	dc := Deps{
		Loops:     f,
		Workspace: &fakeWorkspace{plan: plan},
	}
	var out bytes.Buffer
	if err := approveLoop(context.Background(), dc, "ns", loopName, &out); err != nil {
		t.Fatalf("approveLoop: %v", err)
	}
	body := d85Patch(t, f)
	got := d85Annotation(t, body, "coxswain.io/approve")
	want := planSHA256([]byte(plan))
	if got != want {
		t.Errorf("coxswain.io/approve annotation = %q, want the plan's SHA-256 %q", got, want)
	}
	if !bytes.Contains(out.Bytes(), []byte("Approved the plan")) {
		t.Errorf("output = %q, want to contain 'Approved the plan'", out.String())
	}
}

// TestRejectLoop_MetadataAnnotationOnly verifies D85: reject patches the
// Loop object's metadata.annotations with coxswain.io/reject: <reason> on
// the MAIN resource, with no request to /status.
func TestRejectLoop_MetadataAnnotationOnly(t *testing.T) {
	const reason = "the plan misses the edge case"
	const loopName = "d85-reject"

	f := newFakeLoopClient()
	f.setLoop(newFakeLoop(loopName, map[string]any{}))
	var out bytes.Buffer
	if err := rejectLoop(context.Background(), Deps{Loops: f}, loopName, reason, &out); err != nil {
		t.Fatalf("rejectLoop: %v", err)
	}
	body := d85Patch(t, f)
	if got := d85Annotation(t, body, "coxswain.io/reject"); got != reason {
		t.Errorf("coxswain.io/reject annotation = %q, want %q", got, reason)
	}
	if !bytes.Contains(out.Bytes(), []byte("Rejected the plan")) {
		t.Errorf("output = %q, want to contain 'Rejected the plan'", out.String())
	}
}
