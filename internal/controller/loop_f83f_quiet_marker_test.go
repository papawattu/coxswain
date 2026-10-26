// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// F83f: the quiet-window marker has one implementation, and its specs are
// VALUE-based.
//
// Before this, the foreign-verify-Job spec asserted only that the marker
// ANNOTATION existed, so a marker holding "true" (the pre-F48f shape), a
// garbage value, or the wrong time all passed — while the marker's whole
// purpose is the value: it is the last Event's time, and the gate compares
// now - that time against the window. These specs pin the value and the window
// on the verify surface; the sandbox surface's value assertion is in
// loop_f48b_mount_envtest_test.go ("runs the trailing status write after the
// marker patch"), and both surfaces now run the same helper
// (loop_quiet_marker.go).
package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// TestF83fQuietMarkerNotifiedAt pins the parse rule the gate depends on: only
// an RFC3339 value is "recently notified". Everything else (absent, the
// pre-F48f `true`, garbage) must read as NOT notified, so a marker can never
// mute a surface forever.
func TestF83fQuietMarkerNotifiedAt(t *testing.T) {
	valid := time.Date(2026, 10, 25, 21, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		value   string
		wantOK  bool
		wantAt  time.Time
		comment string
	}{
		{"absent", "", false, time.Time{}, "no marker yet: announce"},
		{"pre-F48f bool", "true", false, time.Time{}, "the old boolean marker must not mute anything"},
		{"garbage", "not-a-time", false, time.Time{}, "an unreadable marker re-announces and is rewritten"},
		{"valid", valid.Format(time.RFC3339), true, valid, "the marker value is the last Event's time"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at, ok := quietMarkerNotifiedAt(tc.value)
			if ok != tc.wantOK {
				t.Fatalf("quietMarkerNotifiedAt(%q) ok = %v, want %v (%s)", tc.value, ok, tc.wantOK, tc.comment)
			}
			if ok && !at.Equal(tc.wantAt) {
				t.Fatalf("quietMarkerNotifiedAt(%q) = %s, want %s", tc.value, at, tc.wantAt)
			}
		})
	}
}

// f83fForeignFixture builds the foreign-verify-Job fixture with a controllable
// operator clock. The Loop and a foreign Job (not owned, JobFailed) are in the
// fake client, the pod list is empty (no decision), so applyVerifyOutcome holds
// at Verifying and surfaces the refusal. advance moves the clock.
func f83fForeignFixture(t *testing.T) (r *LoopReconciler, loop *coxv1alpha1.Loop, rec *record.FakeRecorder, advance func(time.Duration)) {
	t.Helper()
	scheme := newI42cForeignNPScheme(t)
	loop = f83bLoop()
	foreign := f83bJob(loop, f83bPin, true) // never owned: no controller reference
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign, loop).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok {
				return nil // the Job's pod is not created yet: no decision
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
	rec = record.NewFakeRecorder(16)
	r = &LoopReconciler{CNIProber: testCNIProber(), Client: c, Scheme: scheme, apiReader: c, Recorder: rec}
	base := time.Date(2026, 10, 25, 21, 0, 0, 0, time.UTC)
	offset := time.Duration(0)
	r.now = func() metav1.Time { return metav1.NewTime(base.Add(offset)) }
	return r, loop, rec, func(d time.Duration) { offset += d }
}

// f83fMarker parses the verify marker and fails the test when it is absent or
// not a time (the presence-only assertion this replaces).
func f83fMarker(t *testing.T, loop *coxv1alpha1.Loop) time.Time {
	t.Helper()
	raw, ok := loop.Annotations[verifyJobForeignAnnotation]
	if !ok {
		t.Fatalf("the Event must be gated by the %s marker", verifyJobForeignAnnotation)
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("the marker must record the last Event's TIME (RFC3339), got %q: %v", raw, err)
	}
	return at
}

// TestF83fForeignVerifyJobReannouncesOncePerQuietWindow pins the two halves the
// marker exists for, on the surface F83b added: a second pass inside the window
// emits nothing and leaves the marker alone; a pass past the window re-announces
// and moves the marker. The pre-F83f spec asserted neither.
func TestF83fForeignVerifyJobReannouncesOncePerQuietWindow(t *testing.T) {
	ctx := context.Background()
	r, loop, rec, advance := f83fForeignFixture(t)

	step := func(msg string) { t.Helper(); t.Logf("step: %s", msg) }

	step("first pass: one Event, and the marker is the operator's clock")
	if _, requeue := r.applyVerifyOutcome(ctx, loop); !requeue {
		t.Fatal("a foreign Job is no decision: the pass must requeue")
	}
	if n := f83bEventCount(drainEvents(rec)); n != 1 {
		t.Fatalf("want 1 %s Event on the first refusal, got %d", verifyJobForeignEventReason, n)
	}
	marked := f83fMarker(t, loop)
	if want := r.operatorNow().UTC().Truncate(time.Second); !marked.Equal(want) {
		t.Fatalf("the marker must be the operator's clock: marker %s, clock %s", marked, want)
	}

	step("second pass INSIDE the quiet window: no Event, and the marker does not move")
	advance(verifyJobForeignQuiet - time.Minute)
	if _, requeue := r.applyVerifyOutcome(ctx, loop); !requeue {
		t.Fatal("the Loop must still hold at Verifying")
	}
	if n := f83bEventCount(drainEvents(rec)); n != 0 {
		t.Fatalf("a repeat inside the %s window must emit NOTHING, got %d Event(s)", verifyJobForeignQuiet, n)
	}
	if got := f83fMarker(t, loop); !got.Equal(marked) {
		t.Fatalf("the marker must not move while the window mutes the Event: was %s, now %s", marked, got)
	}

	step("third pass PAST the window: the Event re-announces and the marker moves")
	advance(2 * time.Minute) // now > window since the marked time
	if _, requeue := r.applyVerifyOutcome(ctx, loop); !requeue {
		t.Fatal("the Loop must still hold at Verifying")
	}
	if n := f83bEventCount(drainEvents(rec)); n != 1 {
		t.Fatalf("a condition that persists past the window must re-announce: want 1 Event, got %d", n)
	}
	moved := f83fMarker(t, loop)
	if !moved.Equal(r.operatorNow().UTC().Truncate(time.Second)) {
		t.Fatalf("the re-announce must move the marker to the new clock: %s", moved)
	}
	if !moved.After(marked) {
		t.Fatalf("the marker must move forward: was %s, now %s", marked, moved)
	}
}

// TestF83fPreF48fBoolMarkerStillAnnounces pins the upgrade path the shared
// parse helper owns: an operator that upgrades with a pre-F48f `true` marker
// (or a marker written by an older build with another shape) must announce on
// the next pass and rewrite the marker as a time — never stay muted.
func TestF83fPreF48fBoolMarkerStillAnnounces(t *testing.T) {
	ctx := context.Background()
	r, loop, rec, _ := f83fForeignFixture(t)
	if loop.Annotations == nil {
		loop.Annotations = map[string]string{}
	}
	loop.Annotations[verifyJobForeignAnnotation] = resumeAnnotationTrue // "true": the old boolean marker

	if _, requeue := r.applyVerifyOutcome(ctx, loop); !requeue {
		t.Fatal("a foreign Job is no decision: the pass must requeue")
	}
	if n := f83bEventCount(drainEvents(rec)); n != 1 {
		t.Fatalf("a pre-F48f %q marker must NOT mute the surface: want 1 Event, got %d", resumeAnnotationTrue, n)
	}
	raw := loop.Annotations[verifyJobForeignAnnotation]
	if raw == resumeAnnotationTrue {
		t.Fatal("the pass must rewrite the boolean marker as a time")
	}
	if _, err := time.Parse(time.RFC3339, raw); err != nil {
		t.Fatalf("the rewritten marker must be RFC3339, got %q: %v", raw, err)
	}
}

// TestF83fNotFoundAfterTheMarkerKeepsTheCallersResourceVersion pins the one
// behaviour change this refactor made (F83f, review P2-1). The pre-F83f
// sandbox copy refreshed the caller's resourceVersion unconditionally after the
// marker patch: `loop.ResourceVersion = fresh.ResourceVersion`. On a NotFound
// re-get that assigns the EMPTY resourceVersion of the missing object, so the
// reconcile's trailing Status().Update becomes an unconditional write against a
// Loop that is being deleted (or recreated). The helper returns without
// touching it; this spec is the only thing that pins that half.
func TestF83fNotFoundAfterTheMarkerKeepsTheCallersResourceVersion(t *testing.T) {
	ctx := context.Background()
	scheme := newI42cForeignNPScheme(t)
	loop := f83bLoop()
	loop.ResourceVersion = "42"
	notFound := apierrors.NewNotFound(coxv1alpha1.GroupVersion.WithResource("loops").GroupResource(), loop.Name)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loop).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*coxv1alpha1.Loop); ok {
				return notFound // the re-get after the marker patch finds nothing
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()
	rec := record.NewFakeRecorder(8)
	r := &LoopReconciler{CNIProber: testCNIProber(), Client: c, Scheme: scheme, apiReader: c, Recorder: rec}
	fixed := metav1.NewTime(time.Date(2026, 10, 25, 21, 0, 0, 0, time.UTC))
	r.now = func() metav1.Time { return fixed }

	r.markAndSurfaceQuietly(ctx, loop, quietMarker{
		key:     verifyJobForeignAnnotation,
		quiet:   verifyJobForeignQuiet,
		reason:  verifyJobForeignEventReason,
		message: "a foreign verify Job (NotFound re-get spec)",
	})

	if n := f83bEventCount(drainEvents(rec)); n != 1 {
		t.Fatalf("the marker patch succeeded, so the surface must announce: want 1 Event, got %d", n)
	}
	raw, ok := loop.Annotations[verifyJobForeignAnnotation]
	if !ok {
		t.Fatal("the in-memory marker must still be set when the re-get fails (the patch already succeeded)")
	}
	if at, err := time.Parse(time.RFC3339, raw); err != nil || !at.Equal(fixed.UTC().Truncate(time.Second)) {
		t.Fatalf("the in-memory marker must be the Event's time, got %q (err %v)", raw, err)
	}
	if got := loop.ResourceVersion; got != "42" {
		t.Fatalf("a NotFound re-get must leave the caller's resourceVersion ALONE (a stale or empty RV must never be written into it), got %q", got)
	}
}
