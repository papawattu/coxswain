// F93a: bound ANY unschedulable Loop pod (the sandbox pod AND the per-Loop
// proxy pods — model, egress, tool — see the addendum to .samples/f93/F93.md
// and docs/OPERATOR.md's C2 note).
//
// A pod the scheduler can never place (PodScheduled=False, Unschedulable)
// wedged Loops with no decision: the sandbox case sat SandboxNotStarting for
// 20h+ (the F48b unmountable-volume bound only counts mount Events and
// CreateContainerConfigError, so it never trips on a FailedScheduling pod);
// and with C2's placement flags a Loop with no matching node holds at
// Planning with ModelProxyNotReady=True forever — the proxy pods are the
// first to go unschedulable, and the sandbox is not even created.
//
// applyUnschedulableLoopPods is a level-based decision that runs on every
// reconcile (before the budget step, so even the D77 budget-hold path — which
// returns before the shared finalize — persists the surface or the terminal
// failure). When at least one Loop pod is unschedulable the operator
// SURFACES it (the PodUnschedulable condition, True, naming the pod(s) and
// the scheduler's own message — I90) and starts the bound's clock (the
// coxswain.io/loop-unschedulable-since metadata annotation; the D85
// metadata-patch pattern — the VAP never sees it). The clock resets the
// moment every Loop pod is schedulable or gone, so it measures a single
// contiguous run, not the Loop's life. Once the run lasts
// unschedulableLoopPodsBound the Loop fails with the PodUnschedulable reason
// (Failed condition, phase Failed, one Warning Event), naming every
// offending pod and the fix (a node added or labelled, or the placement
// flags corrected — the cluster must change; the operator cannot fix it).
//
// The bound is a TIME bound, not a recreate-retry like the F6/F48 sandbox
// bound: recreating an unschedulable pod recreates the same unschedulable
// pod (the scheduler's verdict does not change on the same cluster), so the
// only resolution is a cluster change, and the bound is the operator's
// refusal to wait for one indefinitely (the F93a observation: 20h+ with no
// decision).
package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

const (
	// unschedulableLoopPodsBound is how long the Loop may sit with at least
	// one of its pods unschedulable before the operator fails it with the
	// PodUnschedulable reason. Ten minutes: a pod that is genuinely pending
	// on a slow-but-eventual bind never carries the Unschedulable condition
	// at all (the scheduler only sets it when it has rejected the pod), so
	// the bound only measures pods the scheduler has explicitly refused. 10m
	// is long enough that a node added or re-labelled within minutes resolves
	// the state before the bound trips; short enough that a misconfigured
	// --loop-node-selector (the C2 failure mode) fails the Loop in the same
	// order of minutes the F48 bounds do, not hours (the F93a observation:
	// 20h+ with no decision).
	unschedulableLoopPodsBound = 10 * time.Minute

	// unschedulableRequeueAfter is the requeue interval the caller (Reconcile)
	// applies while the Loop has at least one unschedulable pod: the caller
	// sets verifyRequeue (the bool) so the shared reconcileRequeueAfter returns
	// workRequeue (5s) on the stuck pass. Five seconds matches the repo's
	// other short requeues; the bound is re-checked every pass, and a node
	// added or re-labelled resolves the state well inside the bound.
	unschedulableRequeueAfter = workRequeue

	// unschedulableLoopPodsCondition is the Loop condition that SURFACES an
	// unschedulable Loop pod (I90). True + Unschedulable while any Loop pod
	// is unschedulable (the message names the pod(s) + the scheduler's own
	// message); False + AllSchedulable once every Loop pod is schedulable or
	// gone (the clock reset). The condition's Type is distinct from the
	// Failed condition's REASON (PodUnschedulable), which is the terminal
	// record once the bound trips (the SandboxFailed / AgentFailed split, the
	// F6 precedent).
	unschedulableLoopPodsCondition = "PodUnschedulable"

	// unschedulableReasonUnschedulable is the condition REASON while at least
	// one Loop pod is unschedulable.
	unschedulableReasonUnschedulable = "Unschedulable"

	// unschedulableReasonAllSchedulable is the condition REASON on the clear
	// (False): every Loop pod is schedulable or gone.
	unschedulableReasonAllSchedulable = "AllSchedulable"

	// unschedulableEventReason is the Warning Event reason for the
	// unschedulable surface (one Event per transition to True). The bound's
	// exhaustion has its own Event reason (unschedulableFinalEventReason).
	unschedulableEventReason = "PodUnschedulable"

	// unschedulableFinalEventReason is the Warning Event reason on the
	// bound's exhaustion (the Loop fails with the PodUnschedulable reason).
	unschedulableFinalEventReason = "PodUnschedulableFailed"

	// loopUnschedulableSinceAnnotation is the operator-owned marker that
	// starts the bound's clock. Its VALUE is the RFC3339 time the Loop most
	// recently entered the unschedulable state; it is removed (a metadata
	// patch, JSON null) the moment every Loop pod is schedulable or gone,
	// so the bound counts a single contiguous run. The annotation is on the
	// Loop's METADATA (not status): the D85 VAP (a non-manager may write
	// only PlanApproved/PlanRejected) never applies to it, and the operator
	// is the only writer of the coxswain.io/* markers it sets (the F48f
	// sandboxEventsUnreadableAnnotation pattern, which it also adopts — the
	// value is the operator's own clock, never a pod's).
	loopUnschedulableSinceAnnotation = "coxswain.io/loop-unschedulable-since"
)

// stuckPod is one unschedulable Loop pod: its name and the scheduler's own
// message (empty when the condition carried none).
type stuckPod struct {
	name    string
	message string
}

// podUnschedulable reports whether the pod is stuck at the scheduler's door:
// the scheduler has explicitly recorded that it cannot place the pod
// (PodScheduled=False, Unschedulable). A pod that is merely Pending (image
// pull, a slow bind) does NOT qualify — the I49 discipline (a pending pod is
// indistinguishable from a slow-but-eventual start) is what keeps this from
// failing Loops on slow image pulls (the reason F48's surfacing does not
// decide, and the reason the F48b bound needs Events). A pod with no
// conditions yet (just created, not scheduled) is NOT unschedulable: the
// scheduler has said nothing, so there is no decision.
func podUnschedulable(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Type == corev1.PodScheduled {
			return c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable
		}
	}
	return false
}

// podUnschedulableMessage returns the scheduler's own message for the pod
// (the message of the PodScheduled condition, truncated), or "" when there
// is none (a condition without a message is still unschedulable).
func podUnschedulableMessage(pod *corev1.Pod) string {
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return truncate(200, c.Message)
		}
	}
	return ""
}

// loopPodRefs are the per-Loop pods F93a reads: the model proxy, the egress
// proxy, the tool proxies, and the sandbox. The deliver/verify/memory-writer
// Jobs' pods are NOT here: a Job that cannot schedule its pod goes into the
// Job's backoff and the Job's own status is the surface the phase machine
// already reads (the verify Job's bound lives in the I65 verify infra-retry,
// which this slice deliberately does not widen — the F93a scope is the Loop
// PODS: the sandbox + the proxies).
type loopPodRefs struct {
	model, egress, sandbox string
	tools                  []string
}

// loopPodRefsForLoop builds the pod names F93a reads, from the Loop's name
// and its effective tool policies (a tool proxy exists per tool; a read
// error propagates — no tools is the wrong fail-open, the
// effectivePolicyTools fail-closed rule).
func (r *LoopReconciler) loopPodRefsForLoop(ctx context.Context, loop *coxv1alpha1.Loop) (*loopPodRefs, error) {
	refs := &loopPodRefs{
		model:   proxyPodName(loop.Name),
		egress:  egressProxyPodName(loop.Name),
		sandbox: sandboxName(loop.Name),
	}
	tools, err := r.effectivePolicyTools(ctx, loop)
	if err != nil {
		return nil, err
	}
	for i := range tools {
		refs.tools = append(refs.tools, toolProxyPodName(loop.Name, tools[i].Name))
	}
	return refs, nil
}

// loopUnschedulablePods reads every Loop pod the operator owns and returns
// the ones that are unschedulable (with the scheduler's message), in a
// STABLE order (model, egress, tool-by-name, sandbox) so the condition
// message does not flap between reconciles (the F84c discipline: a message
// that changes every pass re-stamps LastTransitionTime and costs a status
// write per pass). A pod that is GONE (NotFound) is not unschedulable
// (nothing to schedule); a read that failed for another reason is an error
// — no decision (I49: a failed read is never a failure decision). A pod
// that is NOT controlled by the Loop (a stale pod from a deleted Sandbox,
// or a foreign pod with the same name) is skipped: F93a decides on the
// operator's own pods only (the sandbox pod is controlled by the Sandbox,
// the F14 pattern).
func (r *LoopReconciler) loopUnschedulablePods(ctx context.Context, loop *coxv1alpha1.Loop) ([]stuckPod, error) {
	refs, err := r.loopPodRefsForLoop(ctx, loop)
	if err != nil {
		return nil, err
	}
	reader := r.apiReader
	if reader == nil {
		if r.Client == nil {
			return nil, nil
		}
		reader = r
	}
	var stuck []stuckPod
	// checkOne reads one Loop-owned pod by name: it appends to stuck when
	// the pod exists, is Loop-owned, and is unschedulable. NotFound is
	// "not stuck" (no pod to schedule); any other read error propagates.
	checkOne := func(name string) error {
		pod := &corev1.Pod{}
		gerr := reader.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: name}, pod)
		if gerr != nil {
			if apierrors.IsNotFound(gerr) {
				return nil
			}
			return gerr
		}
		if !metav1.IsControlledBy(pod, loop) {
			return nil // a stale/foreign pod: not the operator's
		}
		if podUnschedulable(pod) {
			stuck = append(stuck, stuckPod{name: pod.Name, message: podUnschedulableMessage(pod)})
		}
		return nil
	}
	// model proxy, egress proxy, and the tool proxies (Loop-owned).
	for _, name := range append([]string{refs.model, refs.egress}, refs.tools...) {
		if gerr := checkOne(name); gerr != nil {
			return nil, gerr
		}
	}
	// The sandbox pod (Sandbox-owned, the F14 check).
	sb := &sandboxv1beta1.Sandbox{}
	sbErr := reader.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: refs.sandbox}, sb)
	if sbErr != nil && !apierrors.IsNotFound(sbErr) {
		return nil, sbErr
	}
	pod := &corev1.Pod{}
	podErr := reader.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: refs.sandbox}, pod)
	if podErr != nil {
		if !apierrors.IsNotFound(podErr) {
			return nil, podErr
		}
		return stuck, nil
	}
	if sbErr == nil && metav1.IsControlledBy(pod, sb) && podUnschedulable(pod) {
		stuck = append(stuck, stuckPod{name: pod.Name, message: podUnschedulableMessage(pod)})
	}
	return stuck, nil
}

// formatStuckPods renders the stuck pod list for the condition message:
// every pod named, and every non-empty scheduler message quoted (a stable
// order — the F84c discipline).
func formatStuckPods(stuck []stuckPod) string {
	parts := make([]string, 0, len(stuck))
	for i := range stuck {
		if stuck[i].message != "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", stuck[i].name, stuck[i].message))
		} else {
			parts = append(parts, stuck[i].name)
		}
	}
	return strings.Join(parts, "; ")
}

// applyUnschedulableLoopPods is the F93a level-based decision (see the
// package comment). It returns whether the Loop's status was mutated (the
// caller ORs it into its own changed flag and, on a true return, also sets
// its verifyRequeue bool to true so the shared reconcileRequeueAfter returns
// the short workRequeue while the state is unschedulable — the bound is
// re-checked every pass, and a node added or re-labelled resolves the state
// well inside the bound). It never returns an error: a read error is logged
// and the caller requeues (I49: no decision on a failed read — the caller
// sets verifyRequeue too, so a read error also requeues), and the terminal
// failure is written to the Loop's status in place (the caller persists it).
//
// It is inert (never fails, never sets the condition) while the Loop is
// Paused (the owner can be fixing the node pool; a paused Loop's pods are
// the owner's to manage) and in terminal phases (a Succeeded/Failed Loop's
// leftover proxy pod — the C2 side effect the R46 addendum names — is not a
// reason to fail the Loop a second time; the terminal clear is the F84c
// class, clearModelProxyHoldForTerminal's precedent).
func (r *LoopReconciler) applyUnschedulableLoopPods(ctx context.Context, loop *coxv1alpha1.Loop) bool {
	if loop.DeletionTimestamp != nil ||
		loop.Status.Phase == coxv1alpha1.LoopPhasePaused ||
		isTerminalPhase(loop.Status.Phase) {
		return false
	}
	stuck, err := r.loopUnschedulablePods(ctx, loop)
	if err != nil {
		// I49: a failed read is no decision. The caller requeues (verifyRequeue
		// is set on any true return; a read error returns false — the caller
		// requeues via the next reconcile's own watch, and the bound's clock
		// annotation (if set) keeps running). The condition (if set) stays as
		// it was — it does not describe a pod state the operator can no longer
		// read.
		logf.FromContext(ctx).Error(err, "F93a unschedulable-pod read failed; no decision", "loop", loop.Name)
		return false
	}
	if len(stuck) == 0 {
		// Every Loop pod is schedulable or gone: clear the surface (if set)
		// and reset the bound's clock (remove the annotation, if present).
		cleared := false
		if conditionTruePresent(loop, unschedulableLoopPodsCondition) {
			setCondition(loop, unschedulableLoopPodsCondition, metav1.ConditionFalse,
				unschedulableReasonAllSchedulable,
				"every Loop pod is schedulable or gone; the unschedulable bound's clock resets")
			cleared = true
		}
		if loop.Annotations[loopUnschedulableSinceAnnotation] != "" {
			newRV := r.patchUnschedulableSinceAnnotation(ctx, loop, nil)
			cleared = true
			if newRV == "" {
				// A failed patch: the clock will over-count by at most this
				// pass; the next pass re-evaluates the clear (level-based).
				// Not a controller error — the bound is a safety valve.
				logf.FromContext(ctx).V(1).Info("F93a failed to clear the unschedulable-since annotation; requeueing",
					"loop", loop.Name)
			} else {
				loop.ResourceVersion = newRV
			}
		}
		return cleared
	}

	// At least one Loop pod is unschedulable: surface it (the condition is
	// True with every stuck pod named + the scheduler's message; one Warning
	// Event on the transition to True — the message is FIXED for a given
	// stuck set, so no per-pass status write; the F84c discipline).
	msg := fmt.Sprintf("Loop pod(s) unschedulable: %s; the bound is %s — check the node pool and the placement flags (--loop-node-selector / --loop-tolerations)",
		formatStuckPods(stuck), unschedulableLoopPodsBound)
	wasTrue := conditionTruePresent(loop, unschedulableLoopPodsCondition)
	setCondition(loop, unschedulableLoopPodsCondition, metav1.ConditionTrue,
		unschedulableReasonUnschedulable, msg)
	if !wasTrue && r.Recorder != nil {
		r.Recorder.Eventf(loop, corev1.EventTypeWarning, unschedulableEventReason, "%s", msg)
	}

	// Start (or keep) the bound's clock. The annotation is stamped on the
	// transition into the stuck state (absent) and left alone afterwards, so
	// the clock measures a contiguous run. A failed patch does not stop the
	// surface (the condition above was written in this pass); it only means
	// the clock does not start this pass — the requeue retries it.
	// The patch's result (the new resourceVersion) is applied to the
	// in-memory loop so the trailing status write does NOT conflict (the
	// patch bumped the resourceVersion; the status write needs the CURRENT
	// one).
	if _, ok := loop.Annotations[loopUnschedulableSinceAnnotation]; !ok {
		since := r.loopUnschedulableSince()
		newRV := r.patchUnschedulableSinceAnnotation(ctx, loop, &since)
		if newRV == "" {
			logf.FromContext(ctx).V(1).Info("F93a failed to stamp the unschedulable-since annotation; the bound's clock does not start this pass",
				"loop", loop.Name)
		} else {
			// Update the in-memory loop's resourceVersion to the patch's
			// result so the trailing status write does NOT conflict (the
			// patch bumped the resourceVersion; the status write needs the
			// CURRENT one). The patch wrote the annotation (metadata); the
			// status write writes the status subresource (the condition +
			// the phase change). No conflict: the status write's base is
			// CURRENT.
			loop.ResourceVersion = newRV
		}
	}

	// The bound: once the stuck state has lasted unschedulableLoopPodsBound,
	// fail the Loop with the PodUnschedulable reason. The decision is a pure
	// function of (annotation time, operator clock, stuck set), so it is
	// idempotent across reconciles and safe under a concurrent status writer
	// (the trailing status write is the single persist).
	if since, ok := parseUnschedulableSince(loop); ok &&
		r.operatorNow().Sub(since.Time) >= unschedulableLoopPodsBound {
		return r.failLoopUnschedulable(loop, stuck)
	}
	return true
}

// loopUnschedulableSince is the RFC3339 value the clock annotation carries
// (the operator's own clock — the decision clock must stay the operator's,
// the F48b discipline: never a pod's timestamp).
func (r *LoopReconciler) loopUnschedulableSince() string {
	return r.operatorNow().UTC().Format(time.RFC3339)
}

// patchUnschedulableSinceAnnotation stamps (or, when value is nil, removes)
// the bound's clock annotation on the Loop's metadata. It returns true when
// the patch did NOT land (the caller requeues; never a controller error —
// the bound is a safety valve and a failed patch must not wedge the
// reconcile the way a failed delete of the sandbox-for-retry does).
//
// The patch is a metadata-ONLY JSON merge patch (the D85 annotation pattern:
// approve/reject patch the Loop's METADATA, not status, so the VAP never
// sees it). It re-reads the Loop fresh so the patch carries a current
// resourceVersion (the in-memory loop is stale: this reconcile's own status
// changes are not yet persisted, and an optimistic-concurrency patch on a
// stale resourceVersion would 409 against the trailing status write).
func (r *LoopReconciler) patchUnschedulableSinceAnnotation(ctx context.Context, loop *coxv1alpha1.Loop, value *string) string {
	// A metadata-ONLY JSON merge patch (the D85 annotation pattern: approve/
	// reject patch the Loop's METADATA, not status, so the VAP never sees
	// it). client.MergeFrom(base) captures base as the pre-mutation state;
	// the patch renders the diff between base and the (mutated) target. The
	// base must be a SEPARATE copy (mutating the same object before calling
	// MergeFrom would make the diff empty — the base IS the already-mutated
	// state). The fresh read means the patch carries a current resourceVersion
	// (the in-memory loop is stale: this reconcile's own status changes are
	// not yet persisted).
	base := &coxv1alpha1.Loop{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(loop), base); err != nil {
		if apierrors.IsNotFound(err) {
			return "" // gone: nothing to patch, nothing to requeue for
		}
		return "" // a failed read: retry next pass
	}
	// Mutate a copy of base to produce the patch's target: base + the one
	// annotation change (set or remove — a JSON-merge-patch null removes the
	// key, which is how MergeFrom renders a delete).
	target := base.DeepCopy()
	if value == nil {
		delete(target.Annotations, loopUnschedulableSinceAnnotation)
	} else {
		if target.Annotations == nil {
			target.Annotations = map[string]string{}
		}
		target.Annotations[loopUnschedulableSinceAnnotation] = *value
	}
	if err := r.Patch(ctx, target, client.MergeFrom(base)); err != nil {
		// A conflict or a transient apiserver error: retry next pass (the
		// requeue covers it). A NotFound (the Loop deleted mid-reconcile):
		// nothing to patch.
		return ""
	}
	// The patch succeeded: the annotation is persisted. The patch's result
	// (target) has the new resourceVersion. Return it so the caller can
	// update the in-memory loop's resourceVersion (the trailing status
	// write needs the CURRENT one).
	return target.ResourceVersion
}

// parseUnschedulableSince reads the bound's clock from the annotation. It
// returns (time, ok); a missing or malformed annotation is (zero, false) —
// no decision (the clock has not started, or the value is corrupted; the
// next pass re-stamps or the surface carries on without the bound).
func parseUnschedulableSince(loop *coxv1alpha1.Loop) (metav1.Time, bool) {
	raw, ok := loop.Annotations[loopUnschedulableSinceAnnotation]
	if !ok {
		return metav1.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return metav1.Time{}, false
	}
	return metav1.NewTime(t), true
}

// failLoopUnschedulable fails the Loop with the PodUnschedulable reason
// (the F93a terminal). It mirrors the F6 exhaustion path: the Failed
// condition carries the durable record (every stuck pod named + the
// scheduler's message + the fix), the surface condition is cleared (the
// Failed condition supersedes it), the phase flips to Failed, and one
// Warning Event is emitted. It returns whether the Loop's status changed.
func (r *LoopReconciler) failLoopUnschedulable(loop *coxv1alpha1.Loop, stuck []stuckPod) bool {
	if isTerminalPhase(loop.Status.Phase) {
		return false
	}
	msg := fmt.Sprintf("Loop pod(s) unschedulable for over %s: %s; the node pool and the placement flags (--loop-node-selector / --loop-tolerations) must be fixed before a new Loop can run (the scheduler must accept the pod; the operator cannot fix the cluster)",
		unschedulableLoopPodsBound, formatStuckPods(stuck))
	setCondition(loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue,
		"PodUnschedulable", msg)
	clearCondition(loop, unschedulableLoopPodsCondition)
	loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
	loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
	bumpLoopCompletion("failed", "unschedulable")
	if r.Recorder != nil {
		r.Recorder.Eventf(loop, corev1.EventTypeWarning, unschedulableFinalEventReason, "%s", msg)
	}
	return true
}
