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

package alerts

import (
	"bufio"
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
)

// Defaults for the relay's timing and tolerance.
const (
	// DefaultDiscoverInterval is how often the relay re-lists the pods whose
	// logs carry alerts (a new sandbox/proxy pod, or a rescheduled agent pod).
	DefaultDiscoverInterval = 30 * time.Second
	// defaultFollowRetry is the re-attach delay for a stream that ended or
	// could not be opened.
	defaultFollowRetry = 5 * time.Second
	// maxDecodeFailures is how many CONSECUTIVE undecodable lines make the
	// stream untrusted. One bad line (a partially written line at a rotation
	// boundary) is tolerated; a run means the format changed or the stream is
	// not the one we think it is.
	maxDecodeFailures = 3
	// maxLineBytes bounds one log line (a KubeArmor alert is ~1 KiB; this is a
	// guard against a rogue line, not a limit anyone should reach).
	maxLineBytes = 256 * 1024
)

// Source opens a FOLLOWING read on a pod container's log. The real
// implementation is LogSource (pods/log); the specs use fakes.
//
// since is the last accepted alert's timestamp when the relay is RE-attaching
// after a rotation or a broken stream, and nil on the first attach. The
// implementation must translate it to the API's sinceTime — a re-attach that
// started from "now" would silently lose the gap window.
type Source interface {
	Follow(ctx context.Context, namespace, pod, container string, since *time.Time) (io.ReadCloser, error)
}

// PeerKind says what a discovered pod is, and therefore what its stream is
// evidence about.
type PeerKind string

const (
	// PeerEngine is the engine's own agent pod: ONE stream per node that
	// carries alerts for every sandbox on it, so its health is evidence about
	// every Loop with a sandbox (not about one Loop).
	PeerEngine PeerKind = "engine"
	// PeerProxy is a per-Loop proxy pod of the operator's own: its stream is
	// evidence about exactly that Loop.
	PeerProxy PeerKind = "proxy"
	// PeerWorkload is a sandbox pod. Its log is NOT followed (the engine
	// writes the alerts); it exists so the relay can say "this Loop has a
	// sandbox and the engine stream is attached", which is what makes a count
	// of 0 publishable instead of "unseen".
	PeerWorkload PeerKind = "workload"
)

// Peer is one pod the relay cares about.
type Peer struct {
	Namespace string
	Pod       string
	Container string
	// Kind decides the health semantics (see PeerKind).
	Kind PeerKind
	// Source is the audit source value the envelope carries for lines read
	// from this pod (SourceKubeArmor for the engine's agent pods, the proxy's
	// own value for the operator's proxy pods).
	Source string
}

// Discoverer lists the pods whose logs carry alerts (and the sandbox pods that
// need a health report). Discover is called periodically, so a pod created
// later is picked up without a watch.
type Discoverer interface {
	Discover(ctx context.Context) ([]Peer, error)
}

// Attributor resolves an alert to the Loop it belongs to. The engine's lines
// name the SANDBOX pod, so the Loop comes from the operator's own label on that
// pod — never from a pod-name prefix (the ADR's attribution rule).
type Attributor interface {
	// PodLoop returns the Loop a pod belongs to (one of the operator's
	// per-Loop labels, with the pod's namespace).
	PodLoop(ctx context.Context, namespace, pod string) (loopNamespace, loopName string, ok bool)
	// LoopUID returns the Loop's UID: the count key. A Loop name is reused
	// across deletes; the UID is not.
	LoopUID(ctx context.Context, namespace, name string) (types.UID, bool)
}

// Snapshot is the relay's in-memory view of one Loop, read by the reconcile.
type Snapshot struct {
	// Count is the number of BLOCKED actions seen for this Loop. Publish it
	// only when Health is HealthHealthy; the controller writes nil otherwise.
	Count int64
	// Health is the stream's state (see Health).
	Health Health
	// Reason is the stable condition reason for a non-healthy state.
	Reason string
	// LatestTarget is the target of the most recent alert (blocked or not), so
	// the condition says WHAT was bounced, not only how often.
	LatestTarget string
	// LatestAt is that alert's own timestamp.
	LatestAt time.Time
	// Unattributed is the relay's process-wide count of alerts it could not
	// attribute to any Loop (a pod without a Loop label, a pod already gone).
	// It is a diagnostic, never a Loop's count.
	Unattributed int64
}

// Holder is the thread-safe in-memory state the relay writes and the Loop
// controller reads. The reconcile NEVER writes here and the relay NEVER writes
// status: the holder is the whole seam between the two.
type Holder struct {
	mu           sync.RWMutex
	loops        map[types.UID]Snapshot
	unattributed int64
}

// NewHolder returns an empty holder.
func NewHolder() *Holder {
	return &Holder{loops: map[types.UID]Snapshot{}}
}

// Snapshot returns the Loop's view. The second result is false when the relay
// has no news for the Loop at all — the controller then writes NOTHING (a nil
// count, no condition): "none yet" is not "zero", and it is not "unknown"
// either (a Loop with no sandbox pod yet has no stream to judge).
func (h *Holder) Snapshot(uid types.UID) (Snapshot, bool) {
	if h == nil || uid == "" {
		return Snapshot{}, false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	snap, ok := h.loops[uid]
	if !ok {
		return Snapshot{}, false
	}
	snap.Unattributed = h.unattributed
	return snap, true
}

// Unattributed returns the process-wide count of alerts the relay could not
// attribute to any Loop (a diagnostic: the controller puts it in the condition
// message so an operator can see that labels are missing rather than seeing a
// silently smaller count).
func (h *Holder) Unattributed() int64 {
	if h == nil {
		return 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.unattributed
}

// observe records one alert for a Loop.
func (h *Holder) observe(uid types.UID, al Alert) {
	if h == nil || uid == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	snap := h.loops[uid]
	if al.Blocked {
		snap.Count++
	}
	snap.LatestTarget = al.Target
	snap.LatestAt = al.Time
	h.loops[uid] = snap
}

// noteHealthy records that the Loop's stream attached (or that its sandbox
// exists on an attached engine stream). It never clears a degraded state: a gap
// that happened is not undone by the next successful attach (the count would
// then look complete when it is not — I32).
func (h *Holder) noteHealthy(uid types.UID) {
	if h == nil || uid == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	snap, ok := h.loops[uid]
	if ok && snap.Health != HealthUnknown {
		return
	}
	snap.Health = HealthHealthy
	h.loops[uid] = snap
}

// degrade records that the Loop's stream cannot be trusted. The FIRST degraded
// report wins so the condition reason is stable across retries; a degraded
// state is never upgraded back to Healthy.
func (h *Holder) degrade(uid types.UID, health Health, reason string) {
	if h == nil || uid == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	snap := h.loops[uid]
	if snap.Health == HealthGap || snap.Health == HealthUnavailable {
		return
	}
	snap.Health = health
	snap.Reason = reason
	h.loops[uid] = snap
}

// degradeAll degrades every Loop the relay already knows. The engine stream is
// shared by every sandbox on a node, so its failure is evidence about all of
// them — a per-pod blame would leave every other Loop publishing a stale count.
func (h *Holder) degradeAll(health Health, reason string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for uid, snap := range h.loops {
		if snap.Health == HealthGap || snap.Health == HealthUnavailable {
			continue
		}
		snap.Health = health
		snap.Reason = reason
		h.loops[uid] = snap
	}
}

// noteUnattributed counts an alert the relay could not attribute to a Loop.
func (h *Holder) noteUnattributed() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unattributed++
}

// Relay is the leader-elected manager Runnable that follows the audit streams,
// keeps the per-Loop Holder, and emits the F63 Q4 lines.
type Relay struct {
	// Source opens the pod-log follows (LogSource in the operator; a fake in
	// the specs). Required.
	Source Source
	// Discoverer lists the pods to follow. Required.
	Discoverer Discoverer
	// Attributor resolves a line to its Loop. Required.
	Attributor Attributor
	// Holder is where the per-Loop view is written. Required.
	Holder *Holder
	// Emit is the F63 output (os.Stdout in the operator). nil disables the
	// Q4 lines — only a test, or a deliberately silent install, has nil.
	Emit io.Writer
	// Iteration joins the F63 envelope's iteration from the Loop status at
	// emit time (the proxies cannot fill it). A nil function or an unreadable
	// Loop yields "unknown": the field is never empty (R31 I107 row 24).
	Iteration func(namespace, name string) string
	// Interval is the re-discovery period (default 30s).
	Interval time.Duration
	// Retry is the re-attach delay for a stream that ended or could not be
	// opened (default 5s). The specs shorten it.
	Retry time.Duration
	// Log is the relay's logger.
	Log logr.Logger

	// mu guards the engine stream's state and the workload set: every follower
	// goroutine reports on the engine stream, and discovery reads it when it
	// sees a sandbox pod.
	mu sync.Mutex
	// engineHealthy is "an engine agent stream is attached": what makes a
	// count of 0 a fact rather than an absence.
	engineHealthy bool
	// engineFailed is "the engine stream could not be read, or lost its
	// window": a sandbox pod's count is then UNKNOWN, never 0 (I32).
	engineFailed bool
	// workloads are the sandbox pod UIDs discovered so far: when the engine
	// attaches, every one of them becomes countable.
	workloads map[types.UID]struct{}
}

// Start implements manager.Runnable. It returns when ctx is done.
func (r *Relay) Start(ctx context.Context) error {
	if r.Source == nil || r.Discoverer == nil || r.Attributor == nil || r.Holder == nil {
		// Not configured: there is nothing to follow. The operator always
		// configures it (cmd/main.go); a bare test reconciler does not.
		return nil
	}
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultDiscoverInterval
	}
	// followed is owned by this goroutine only: every stream is started from
	// here, and a stream that ends stays marked (its follower re-attaches; it
	// only goes away with the context).
	followed := map[string]struct{}{}
	discover := func() {
		peers, err := r.Discoverer.Discover(ctx)
		if err != nil {
			// The streams already followed are unaffected, and the peers that
			// WERE listed are still used: a permissions problem on one source
			// must not blind the other two.
			r.log().Error(err, "alert relay: pod discovery was incomplete")
		}
		var enginePeers int
		for _, peer := range peers {
			if peer.Kind == PeerEngine {
				enginePeers++
			}
		}
		for _, peer := range peers {
			if peer.Kind == PeerWorkload {
				r.workloadSeen(ctx, peer, enginePeers > 0)
				continue
			}
			key := peer.Namespace + "/" + peer.Pod + "/" + peer.Container
			if _, ok := followed[key]; ok {
				continue
			}
			followed[key] = struct{}{}
			go r.follow(ctx, peer)
		}
	}
	discover()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			discover()
		}
	}
}

// follow keeps ONE pod container's stream attached: it reads lines until the
// stream ends (a rotation, a broken read), re-attaches from the last accepted
// alert, and retries a follow that could not be opened at all.
func (r *Relay) follow(ctx context.Context, peer Peer) {
	log := r.log().WithValues("pod", peer.Namespace+"/"+peer.Pod, "container", peer.Container)
	retry := r.Retry
	if retry <= 0 {
		retry = defaultFollowRetry
	}
	var since *time.Time
	attached := false
	for {
		if ctx.Err() != nil {
			return
		}
		rc, err := r.Source.Follow(ctx, peer.Namespace, peer.Pod, peer.Container, since)
		if err != nil {
			// The stream never opened. Before the first attach that is
			// UNKNOWN for the affected Loop(s) — never 0 (I32).
			if !attached {
				r.degraded(ctx, peer, HealthUnavailable, ReasonStreamUnavailable)
			}
			log.V(1).Info("alert relay: follow failed; retrying", "error", err.Error(), "attached", attached)
			if !sleepCtx(ctx, retry) {
				return
			}
			continue
		}
		attached = true
		r.attachedNow(ctx, peer)
		// gapPending: this is a RE-attach (since != nil) and the first line
		// that is not a replay decides whether the window is provable. The rule
		// (design F62): a first line later than the last accepted alert is a
		// gap → Unknown, never a smaller number.
		gapPending := since != nil
		failures := 0
		scanner := bufio.NewScanner(rc)
		scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
		for scanner.Scan() {
			al, ok := ParseLine(scanner.Bytes())
			if !ok {
				failures++
				if failures >= maxDecodeFailures {
					r.degraded(ctx, peer, HealthGap, ReasonUndecodable)
				}
				continue
			}
			failures = 0
			if since != nil {
				if !al.Time.After(*since) {
					// A replayed line at the attach boundary (sinceTime is
					// inclusive): it is not a new alert, and counting it twice
					// would inflate the count.
					continue
				}
				if gapPending {
					r.degraded(ctx, peer, HealthGap, ReasonStreamGap)
					gapPending = false
				}
			}
			ts := al.Time
			since = &ts
			r.accept(ctx, peer, al)
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			log.V(1).Info("alert relay: stream read ended", "error", err.Error())
		}
		_ = rc.Close()
		if !sleepCtx(ctx, retry) {
			return
		}
	}
}

// accept attributes one alert and records it: the per-Loop count (F62) and the
// Q4 line (F63). An alert that cannot be attributed is counted as unattributed
// and still emitted (with no Loop), never guessed.
func (r *Relay) accept(ctx context.Context, peer Peer, al Alert) {
	al.Source = firstNonEmpty(al.Source, peer.Source)
	loopNS, loopName := al.LoopNamespace, al.Loop
	if loopName == "" && al.Pod != "" {
		// The engine's line names the SANDBOX pod; the Loop comes from the
		// operator-set label on that pod.
		loopNS, loopName, _ = r.Attributor.PodLoop(ctx, al.Namespace, al.Pod)
	}
	var uid types.UID
	if loopName != "" {
		if id, ok := r.Attributor.LoopUID(ctx, loopNS, loopName); ok {
			uid = id
		}
	}
	if uid == "" {
		r.Holder.noteUnattributed()
		r.emit(al, "", "", unattributedDetail(loopName, al))
		return
	}
	r.Holder.observe(uid, al)
	r.emit(al, loopName, loopNS, al.Detail)
}

// unattributedDetail renders the F63 detail for an alert the relay could not
// attribute: the line is still emitted (every alert is), with the reason.
func unattributedDetail(loopName string, al Alert) string {
	detail := "unattributed"
	if loopName != "" {
		detail += ": loop " + loopName + " not found"
	}
	if al.Namespace != "" || al.Pod != "" {
		detail += ": workload " + al.Namespace + "/" + al.Pod
	}
	if al.Detail != "" {
		detail += "; " + al.Detail
	}
	return detail
}

// emit writes the F63 line for one alert. iteration is joined from the Loop
// status at this moment (the proxies cannot fill it); an empty join is written
// as "unknown" so the field is never empty (R31 I107 row 24).
func (r *Relay) emit(al Alert, loopName, loopNS, detail string) {
	if r.Emit == nil {
		return
	}
	iteration := "unknown"
	if loopName != "" && r.Iteration != nil {
		if it := r.Iteration(loopNS, loopName); it != "" {
			iteration = it
		}
	}
	rec := Record{
		Time:      al.Time.UTC().Format(time.RFC3339Nano),
		Loop:      loopName,
		Namespace: firstNonEmpty(loopNS, al.Namespace),
		Iteration: iteration,
		Source:    al.Source,
		Action:    al.Action,
		Target:    al.Target,
		Verdict:   al.Verdict(),
		Detail:    detail,
	}
	if err := WriteRecord(r.Emit, rec); err != nil {
		r.log().V(1).Info("alert relay: could not write the audit line", "error", err.Error())
	}
}

// attachedNow records a successful attach: the engine stream's state for every
// Loop it serves, or one proxy pod's Loop.
func (r *Relay) attachedNow(ctx context.Context, peer Peer) {
	if peer.Kind == PeerEngine {
		r.mu.Lock()
		// The engine log carries every sandbox's alerts, so a live engine
		// stream is what lets a Loop's count be 0 rather than unknown. A Loop
		// whose stream was already degraded stays degraded (fail-closed:
		// noteHealthy never clears a gap).
		r.engineHealthy = true
		r.engineFailed = false
		for uid := range r.workloads {
			r.Holder.noteHealthy(uid)
		}
		r.mu.Unlock()
		return
	}
	if uid := r.peerLoopUID(ctx, peer); uid != "" {
		r.Holder.noteHealthy(uid)
	}
}

// degraded records a stream the relay cannot trust.
func (r *Relay) degraded(ctx context.Context, peer Peer, health Health, reason string) {
	if peer.Kind == PeerEngine {
		r.mu.Lock()
		r.engineHealthy = false
		r.engineFailed = true
		// Every Loop with a sandbox on that node depends on the stream. The
		// discovered workloads are degraded explicitly as well as through
		// degradeAll: a workload may have been registered after the last
		// attempt and so have no entry of its own yet.
		uids := make([]types.UID, 0, len(r.workloads))
		for uid := range r.workloads {
			uids = append(uids, uid)
		}
		r.mu.Unlock()
		r.Holder.degradeAll(health, reason)
		for _, uid := range uids {
			r.Holder.degrade(uid, health, reason)
		}
		return
	}
	if uid := r.peerLoopUID(ctx, peer); uid != "" {
		r.Holder.degrade(uid, health, reason)
	}
}

// workloadSeen records a sandbox pod: with the engine stream attached, this is
// the evidence that makes "0 blocked actions" a fact rather than an absence.
// Without it, the Loop's count is UNKNOWN (the engine cannot be read).
func (r *Relay) workloadSeen(ctx context.Context, peer Peer, engineDiscovered bool) {
	uid := r.peerLoopUID(ctx, peer)
	if uid == "" {
		return
	}
	r.mu.Lock()
	if r.workloads == nil {
		r.workloads = map[types.UID]struct{}{}
	}
	r.workloads[uid] = struct{}{}
	engineHealthy, engineFailed := r.engineHealthy, r.engineFailed
	r.mu.Unlock()
	switch {
	case engineHealthy:
		r.Holder.noteHealthy(uid)
	case engineFailed:
		// The engine stream was found and could not be read: the count for
		// this Loop is UNKNOWN, never 0 (I32).
		r.Holder.degrade(uid, HealthUnavailable, ReasonStreamUnavailable)
	case engineDiscovered:
		// An engine pod exists and its stream has not attached yet: "no news
		// yet" — writing nothing is right (and the attach, or its failure,
		// decides below on the next pass).
	default:
		// Sandbox pods but no engine pod at all: the engine is absent, which is
		// a fact about the count, not an absence of information.
		r.Holder.degrade(uid, HealthUnavailable, ReasonEngineUnavailable)
	}
}

// peerLoopUID resolves a discovered pod's Loop UID through the operator's
// per-Loop label on it (PodLoop), then the Loop object (LoopUID).
func (r *Relay) peerLoopUID(ctx context.Context, peer Peer) types.UID {
	if peer.Pod == "" || r.Attributor == nil {
		return ""
	}
	ns, name, ok := r.Attributor.PodLoop(ctx, peer.Namespace, peer.Pod)
	if !ok || name == "" {
		return ""
	}
	uid, _ := r.Attributor.LoopUID(ctx, ns, name)
	return uid
}

// log returns the relay's logger.
func (r *Relay) log() logr.Logger {
	if r.Log.GetSink() == nil {
		return logr.Discard()
	}
	return r.Log
}

// sleepCtx sleeps for d unless ctx ends first (true when the sleep completed).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ErrNotConfigured is returned by the concrete sources when their client is
// missing (a wiring mistake, surfaced instead of silently following nothing).
var ErrNotConfigured = errors.New("alerts: source is not configured")
