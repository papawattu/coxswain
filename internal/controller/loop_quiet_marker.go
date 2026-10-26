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

// F83f: one implementation of the "announce at most once per quiet window"
// marker.
//
// Two surfaces gate a repeat Warning Event on an operator-owned annotation
// whose VALUE is the last Event's time: the sandbox Events-unreadable surface
// (F48b/F48f, surfaceEventsUnreadable) and the foreign-verify-Job surface
// (F83b, surfaceForeignVerifyJob). F83b copied F48f's gate-patch-refresh body
// nearly verbatim and reused the sandbox-named parser, so a third copy (or a
// change to one of the two) would drift: an Event that stops re-announcing
// after --event-ttl (1h) is exactly the silent degradation R26 I90 forbids.
// This file holds the one implementation both call.
//
// Not folded in, deliberately:
//
//   - the F44 metering gate (loop_p2d_budget.go, meteringEventQuiet) uses a
//     DIFFERENT input — the condition's LastTransitionTime, not an annotation
//     — so it shares the idea, not the plumbing.
//
// Folded in later: the memory-PVC surface (loop_p5a_memory.go) used a boolean
// one-shot marker until R39 I144, which is the pattern F48f's comment names as
// its model and F48f replaced for the Events surface. It now calls this helper
// (surfaceMemoryPVCUnbound).
package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// quietMarker names one operator-owned "announce once per quiet window"
// marker and the Event it gates.
type quietMarker struct {
	// key is the operator-owned annotation on the LOOP (not on the sandbox
	// pod: the Loop is the reconcile's own object, so the marker travels with
	// the same patch/status pass as the conditions). All current keys are
	// coxswain.io/-prefixed; a new caller should be too.
	key string
	// quiet is how long the marker mutes a repeat Event.
	quiet time.Duration
	// reason is the Warning Event reason (a stable string the runbooks name).
	reason string
	// message is the Event message (the caller formats it; the helper never
	// inspects it).
	message string
}

// quietMarkerNotifiedAt parses a marker value: the time of the last Event
// (RFC3339), or ok=false when the marker is absent or is not a time. Both of
// those mean "not recently notified", so nothing is ever muted forever — a
// pre-F48f `true` marker, a garbage value, or a marker left by an older
// operator version simply announces once and is rewritten as a time.
//
// A marker dated AHEAD of the operator's clock (a wall-clock step back, a VM
// restore) mutes the surface until the clock passes it plus the window; the gate
// is deliberately not clamped to the present, because the alternative (treating
// a future marker as "not notified") would announce on every pass while the
// clock stays behind it, turning one straggling marker into an Event storm. The
// window is 5 minutes, so the mute is bounded by the size of the clock step.
func quietMarkerNotifiedAt(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// markAndSurfaceQuietly emits m.reason as a Warning Event on the Loop at most
// once per m.quiet window, and records the emission time in the operator-owned
// annotation m.key.
//
// The marker value is a TIME, not a bool (F48f): a condition that persists
// re-announces once per window instead of going silent when its Event expires
// at --event-ttl (1h), and the F44-style "did this happen before" question is
// answered by the window, not by presence.
//
// Best-effort: a failed annotation patch never blocks the reconcile (the
// caller's decision or condition path is unaffected), it just means the Event
// may repeat on the next pass.
func (r *LoopReconciler) markAndSurfaceQuietly(ctx context.Context, loop *coxv1alpha1.Loop, m quietMarker) {
	now := r.operatorNow()
	if at, ok := quietMarkerNotifiedAt(loop.Annotations[m.key]); ok && now.Sub(at) < m.quiet {
		return
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(loop, corev1.EventTypeWarning, m.reason, "%s", m.message)
	}
	// Patch a COPY (a full Update would write the server's stale status back
	// into `loop`, dropping this reconcile's conditions — the S4/S6 lesson).
	notified := now.UTC().Format(time.RFC3339)
	patched := loop.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = make(map[string]string)
	}
	patched.Annotations[m.key] = notified
	if err := r.Patch(ctx, patched, client.MergeFrom(loop.DeepCopy())); err != nil {
		logf.FromContext(ctx).Error(err, "mark quiet-window Event notified", "loop", loop.Name, "annotation", m.key)
		return
	}
	if loop.Annotations == nil {
		loop.Annotations = make(map[string]string)
	}
	loop.Annotations[m.key] = notified
	// Refresh the caller's resourceVersion (the patch bumped it): this
	// reconcile's trailing Status().Update sends loop's RV, and a stale one is
	// a 409 conflict — a lost pass, an error log, and the conditions set
	// earlier in the pass only persisted on the requeue. The caller's
	// in-memory status is intact (the patch was on a copy). A NotFound leaves
	// the RV alone: the Loop is going away, so there is nothing to refresh
	// for.
	fresh := &coxv1alpha1.Loop{}
	if gerr := r.Get(ctx, client.ObjectKeyFromObject(loop), fresh); gerr != nil {
		if !apierrors.IsNotFound(gerr) {
			logf.FromContext(ctx).Error(gerr, "re-get Loop after the quiet-window Event mark", "loop", loop.Name)
		}
		return
	}
	loop.ResourceVersion = fresh.ResourceVersion
}
