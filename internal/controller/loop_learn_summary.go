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

package controller

// D119 step 2: the controller persists the relay's per-Loop learn summary
// (held in memory by alerts.Holder since step 1, D119 = (b), R51 I236) into a
// controller-owned ConfigMap so it survives an operator restart and is
// readable by `cox policy suggest` without depending on log retention.
//
// The ConfigMap is <loop>-learn-summary in the Loop's namespace. It carries:
//   - records.jsonl: one alerts.WriteRecord JSON line per distinct record,
//     sorted by alerts.LearnKey (the relay's own key).
//   - dropped: the maximum dropped count ever observed (monotonically
//     non-decreasing; records trimmed at the 900 KiB size bound here add
//     to it).
//
// The ConfigMap is updated (not re-created) on every reconcile for a
// learn-mode Loop. The merge is idempotent: existing records are keyed by
// the same alerts.LearnKey, and the count for each key is max(existing,
// new) — the relay's Count is cumulative, so a new reconcile that sees the
// same record with a higher Count wins.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/alerts"
)

// learnSummaryCMName is the ConfigMap name for one Loop's learn summary.
func learnSummaryCMName(loopName string) string {
	return loopName + "-learn-summary"
}

// learnSummaryRecordsKey is the records.jsonl data key.
const learnSummaryRecordsKey = "records.jsonl"

// learnSummaryDroppedKey is the dropped data key (decimal string).
const learnSummaryDroppedKey = "dropped"

// learnSummarySizeBoundBytes caps the rendered records.jsonl at 900 KiB.
// A record that cannot fit is dropped (lowest-count records first; ties by
// key); the number dropped is added to the `dropped` field. The bound keeps
// the ConfigMap small enough for `kubectl get cm -o json` to be fast and for
// the etcd default 1 MiB per-object cap not to be a realistic concern.
const learnSummarySizeBoundBytes = 900 * 1024

// recordLearnSummary is the D119 step 2 reconcile step: read the relay's
// in-memory summary for the Loop, merge it into the owned ConfigMap (union
// by key, max counts), apply the size bound, and persist. It is a no-op
// when the relay is not installed or the Loop is not in learn mode
// (PolicyEnforced=False / reason AuditOnly). It NEVER fails the reconcile:
// on any error it logs and emits a Warning Event LearnSummaryWriteFailed so
// the failure is visible (the AGENTS test norm: a fallback surfaces its
// failure, R26 I90).
func (r *LoopReconciler) recordLearnSummary(ctx context.Context, loop *coxv1alpha1.Loop, learnMode bool) {
	if r.Audit == nil {
		return
	}
	if !learnMode {
		return
	}
	recs, newDropped, ok := r.Audit.LearnSummary(loop.UID)
	if !ok {
		// No learns-relevant records observed yet: write nothing.
		return
	}
	cmName := learnSummaryCMName(loop.Name)
	if r.learnSummaryNameTaken(ctx, loop, cmName) {
		// A foreign object owns the name: keep not writing and surface it
		// (at most one Event per Loop, plus a log) instead of degrading
		// silently (R26 I90: a fallback surfaces its failure).
		return
	}
	existingCM, existing := r.readExistingLearnSummary(ctx, loop, cmName)
	existingDropped := existingDroppedField(existingCM)
	merged := mergeLearnRecords(existing, recs)
	boundRecs, boundDropped := boundLearnRecords(merged, learnSummarySizeBoundBytes)
	dropped := maxDroppedInt64(existingDropped, maxDroppedInt64(newDropped, boundDropped))
	data := map[string]string{
		learnSummaryRecordsKey: renderLearnRecordsJSONL(boundRecs),
		learnSummaryDroppedKey: strconv.FormatInt(dropped, 10),
	}
	if err := r.upsertLearnSummaryCM(ctx, loop, cmName, data); err != nil {
		logf.FromContext(ctx).Error(err, "LearnSummary write failed", "loop", loop.Name, "configmap", cmName)
		r.emitLearnSummaryEvent(loop, err)
		// Never fail the reconcile (the ConfigMap is best-effort; the
		// relay's in-memory summary is re-read on the next reconcile).
	}
}

// isLearnMode reports whether the Loop's PolicyEnforced condition is
// False / reason AuditOnly (the P6b learn mode the condition is set to in
// applyEffectivePolicyAndConditions).
func isLearnMode(loop *coxv1alpha1.Loop) bool {
	for i := range loop.Status.Conditions {
		c := &loop.Status.Conditions[i]
		if c.Type == string(PolicyEnforcedCondition) {
			return c.Status == metav1.ConditionFalse && c.Reason == "AuditOnly"
		}
	}
	return false
}

// readExistingLearnSummary returns the existing ConfigMap's records (parsed
// and key-sorted) or nil when the ConfigMap is absent or is a foreign object
// (the same name, not owned by the Loop — the operator never absorbs a
// foreign object's records; the upsert step re-checks ownership before
// writing).
func (r *LoopReconciler) readExistingLearnSummary(ctx context.Context, loop *coxv1alpha1.Loop, name string) (*corev1.ConfigMap, []alerts.Record) {
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: loop.Namespace, Name: name}, cm); err != nil {
		return nil, nil
	}
	if !metav1.IsControlledBy(cm, loop) {
		// A foreign object of the same name: treat as absent so the merge
		// does not absorb the foreign object's records.
		return nil, nil
	}
	return cm, parseLearnRecords(cm.Data[learnSummaryRecordsKey])
}

// existingDroppedField reads the dropped field from a (possibly nil) existing
// ConfigMap's data. Returns 0 when the CM is nil or the field is absent or
// unparseable.
func existingDroppedField(cm *corev1.ConfigMap) int64 {
	if cm == nil {
		return 0
	}
	d, err := strconv.ParseInt(cm.Data[learnSummaryDroppedKey], 10, 64)
	if err != nil {
		return 0
	}
	return d
}

// parseLearnRecords parses a records.jsonl body into a slice of
// alerts.Record, sorted by alerts.LearnKey. An empty body returns nil. A
// malformed line is silently skipped (the relay's WriteRecord is the only
// writer; a corrupted line is a best-effort degrade, not a crash).
func parseLearnRecords(body string) []alerts.Record {
	if body == "" {
		return nil
	}
	var recs []alerts.Record
	for line := range strings.SplitSeq(body, "\n") {
		if line == "" {
			continue
		}
		var rec alerts.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		recs = append(recs, rec)
	}
	sortByLearnKey(recs)
	return recs
}

// sortByLearnKey sorts records by alerts.LearnKey (stable for equal keys).
func sortByLearnKey(recs []alerts.Record) {
	slices.SortStableFunc(recs, func(a, b alerts.Record) int {
		return strings.Compare(alerts.LearnKey(a), alerts.LearnKey(b))
	})
}

// mergeLearnRecords returns the union of existing and new records, keyed by
// alerts.LearnKey. For a shared key: Count = max(existing, new); Time = the
// later of the two (the RFC3339Nano strings compare lexicographically, which
// is chronological for the same layout). A key present in only one side is
// copied verbatim. The result is sorted by key.
func mergeLearnRecords(existing, new_ []alerts.Record) []alerts.Record {
	m := map[string]alerts.Record{}
	for _, rec := range existing {
		m[alerts.LearnKey(rec)] = rec
	}
	for _, rec := range new_ {
		prev, ok := m[alerts.LearnKey(rec)]
		if !ok {
			m[alerts.LearnKey(rec)] = rec
			continue
		}
		if rec.Count > prev.Count {
			prev.Count = rec.Count
		}
		if rec.Time > prev.Time {
			prev.Time = rec.Time
		}
		m[alerts.LearnKey(rec)] = prev
	}
	out := make([]alerts.Record, 0, len(m))
	for _, rec := range m {
		out = append(out, rec)
	}
	sortByLearnKey(out)
	return out
}

// boundLearnRecords trims the records so the rendered records.jsonl fits the
// size bound. Candidates are dropped in lowest-count-first order (ties by
// key); the number dropped is returned. The kept records stay in key order.
func boundLearnRecords(recs []alerts.Record, bound int) ([]alerts.Record, int64) {
	if bound <= 0 {
		return nil, int64(len(recs))
	}
	sizes := make([]int, 0, len(recs))
	total := 0
	for _, rec := range recs {
		var buf bytes.Buffer
		if err := alerts.WriteRecord(&buf, rec); err != nil {
			sizes = append(sizes, 0)
			continue
		}
		sizes = append(sizes, buf.Len())
		total += buf.Len()
	}
	if total <= bound {
		return recs, 0
	}
	type cand struct {
		idx  int
		cnt  int64
		key  string
		size int
	}
	cands := make([]cand, 0, len(recs))
	for i, rec := range recs {
		cnt := rec.Count
		if cnt == 0 {
			cnt = 1
		}
		cands = append(cands, cand{idx: i, cnt: cnt, key: alerts.LearnKey(rec), size: sizes[i]})
	}
	slices.SortFunc(cands, func(a, b cand) int {
		if a.cnt != b.cnt {
			if a.cnt < b.cnt {
				return -1
			}
			return 1
		}
		return strings.Compare(a.key, b.key)
	})
	dropSet := map[int]bool{}
	dropped := int64(0)
	for _, c := range cands {
		if total <= bound {
			break
		}
		total -= c.size
		dropSet[c.idx] = true
		dropped++
	}
	kept := make([]alerts.Record, 0, len(recs)-int(dropped))
	for i, rec := range recs {
		if !dropSet[i] {
			kept = append(kept, rec)
		}
	}
	return kept, dropped
}

// renderLearnRecordsJSONL renders the records as one WriteRecord line each,
// in the order given (the caller passes a key-sorted slice).
func renderLearnRecordsJSONL(recs []alerts.Record) string {
	if len(recs) == 0 {
		return ""
	}
	var buf bytes.Buffer
	for _, rec := range recs {
		_ = alerts.WriteRecord(&buf, rec)
	}
	return buf.String()
}

// upsertLearnSummaryCM creates or updates the ConfigMap. The merge has
// already folded the existing data in; this step only persists. A foreign
// ConfigMap (the same name, not owned by the Loop) is left alone.
func (r *LoopReconciler) upsertLearnSummaryCM(ctx context.Context, loop *coxv1alpha1.Loop, name string, data map[string]string) error {
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Namespace: loop.Namespace, Name: name}, cm)
	if apierrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: loop.Namespace,
				Labels: map[string]string{
					managedByLabel: partOfCoxswain,
					loopLabelKey:   loop.Name,
				},
			},
			Data: data,
		}
		if err := controllerutil.SetControllerReference(loop, cm, r.Scheme); err != nil {
			return fmt.Errorf("set owner ref on learn-summary ConfigMap %s: %w", name, err)
		}
		if err := r.Create(ctx, cm); err != nil {
			return fmt.Errorf("create learn-summary ConfigMap %s: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get learn-summary ConfigMap %s/%s: %w", loop.Namespace, name, err)
	}
	if !metav1.IsControlledBy(cm, loop) {
		// A foreign object of the same name: leave it alone.
		return nil
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	changed := false
	for k, v := range data {
		if cm.Data[k] != v {
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}
	cm.Data = data
	if err := r.Update(ctx, cm); err != nil {
		return fmt.Errorf("update learn-summary ConfigMap %s: %w", name, err)
	}
	return nil
}

// learnSummaryNameTaken reports whether the learn-summary name is already
// taken by a ConfigMap the Loop does NOT control (a foreign object). When it
// is, the operator logs it and emits a Warning Event LearnSummaryNameTaken —
// at most once per Loop (loopNameTakenSummaryEvents, keyed by Loop UID; the
// recordAuditOnlyIgnored "status changed" pattern cannot fit here because
// nothing in the Loop's status records the collision, and the AGENTS test
// norm is that a fallback surfaces its failure). It is checked BEFORE any
// write, so the foreign object is never touched.
func (r *LoopReconciler) learnSummaryNameTaken(ctx context.Context, loop *coxv1alpha1.Loop, name string) bool {
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Namespace: loop.Namespace, Name: name}, cm)
	if err != nil {
		return false // absent or unreadable: the upsert step reports it
	}
	if metav1.IsControlledBy(cm, loop) {
		return false // ours: normal create/update path
	}
	logf.FromContext(ctx).Info("Learn summary name is taken by a foreign ConfigMap; the operator will not write the summary there",
		"loop", loop.Name, "configmap", name)
	// At most one Event per Loop (R26 I90: surface once, not every reconcile).
	// Load-then-Store: the Store succeeds on every reconcile (re-arming is
	// harmless because the Event already went out), and Load reports whether
	// this reconcile is the first to see the UID.
	firstSeen, _ := r.loopNameTakenSummaryEvents.Load(loop.UID)
	r.loopNameTakenSummaryEvents.Store(loop.UID, struct{}{})
	if r.Recorder != nil && firstSeen == nil {
		r.Recorder.Eventf(loop, corev1.EventTypeWarning, "LearnSummaryNameTaken",
			"ConfigMap %s/%s already exists and is not controlled by this Loop; the operator will not write the learn summary there", loop.Namespace, name)
	}
	return true
}

// emitLearnSummaryEvent emits a Warning Event LearnSummaryWriteFailed with the
// error. A nil Recorder (most unit tests) is a no-op.
func (r *LoopReconciler) emitLearnSummaryEvent(loop *coxv1alpha1.Loop, err error) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(loop, corev1.EventTypeWarning, "LearnSummaryWriteFailed",
		"could not persist the learn summary: %v", err)
}

// maxDroppedInt64 returns the larger of two int64s.
func maxDroppedInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// forgetAuditState is the D119 step 2 deleting-path step: forget the deleted
// Loop's relay state (both the learn summary and the F62 snapshot — R52 I249
// notes neither was ever pruned before; a deleted Loop's in-memory state
// lived on until the operator restarted). It is a no-op when the relay is not
// installed (r.Audit nil). No return value: Reconcile is at its gocyclo
// budget, so the call site is a bare line.
func (r *LoopReconciler) forgetAuditState(loop *coxv1alpha1.Loop) {
	if r.Audit == nil {
		return
	}
	r.Audit.Forget(loop.UID)
}
