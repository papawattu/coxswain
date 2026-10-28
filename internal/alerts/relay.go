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
	// maxDecodeFailures is how many CONSECUTIVE AUDIT-SHAPED lines that fail to
	// decode make the stream untrusted. Ordinary container output (a zap text
	// line, an unrelated JSON object) is not a failure at all — the engine's
	// log starts with startup noise, and counting that as "the format changed"
	// degraded every Loop to Unknown permanently (pi3 E1). One bad audit line
	// (a partially written line at a rotation boundary) is also tolerated.
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
	// every Loop whose sandbox runs on that node (not about one Loop).
	PeerEngine PeerKind = "engine"
	// PeerProxy is a per-Loop proxy pod of the operator's own: its stream is
	// evidence about exactly that Loop.
	PeerProxy PeerKind = "proxy"
	// PeerWorkload is a sandbox pod. Its log is NOT followed (the engine
	// writes the alerts); it exists so the relay can say "this Loop has a
	// sandbox, and the engine stream on its node is attached", which is what
	// makes a count of 0 publishable instead of "unseen".
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
	// Node is the node the pod runs on. An engine pod serves ONE node, so its
	// failure must degrade only the Loops whose sandbox runs there (pi3 E6).
	Node string
	// Created is the pod's creation time. A sandbox pod created BEFORE the
	// relay started cannot have had its whole life covered by the stream, so
	// its count is not published as a complete number (pi3 E8).
	Created time.Time
}

// Discoverer lists the pods whose logs carry alerts (and the sandbox pods that
// need a health report). Discover is called periodically, so a pod created
// later is picked up without a watch, and a pod that went away is pruned.
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

// noteHealthy records that the Loop's stream coverage is established. An
// UNAVAILABLE state may be rebuilt to Healthy (a full re-read after a failure
// re-reads the whole current log, which IS complete evidence for a pod whose
// life started after the relay — pi3 E6); a GAP is never cleared, because the
// count is genuinely incomplete (I32).
func (h *Holder) noteHealthy(uid types.UID) {
	if h == nil || uid == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	snap, ok := h.loops[uid]
	if ok && snap.Health == HealthGap {
		return
	}
	if ok && snap.Health == HealthHealthy {
		return
	}
	snap.Health = HealthHealthy
	snap.Reason = ""
	h.loops[uid] = snap
}

// degrade records that the Loop's stream cannot be trusted. The FIRST degraded
// report wins so the condition reason is stable across retries; a degraded
// state is never upgraded back to Healthy (see noteHealthy for the exception).
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

// noteUnattributed counts an alert the relay could not attribute to a Loop.
func (h *Holder) noteUnattributed() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unattributed++
}

// podAttr is one pod's resolved Loop attribution, cached for the pod's life.
type podAttr struct {
	loopNS   string
	loopName string
	uid      types.UID
	resolved bool
}

// attrCache remembers what discovery already resolved. A pod's Loop label and a
// Loop's UID do not change while the pod exists, so an audit line must not cost
// two uncached API reads (pi3 E5: under audit mode that is hundreds of requests
// per second). The cache is rebuilt every discovery pass — an entry survives
// only while its pod is still discovered, so a deleted pod is forgotten.
type attrCache struct {
	mu    sync.RWMutex
	pods  map[string]podAttr
	loops map[string]types.UID
}

func newAttrCache() *attrCache {
	return &attrCache{pods: map[string]podAttr{}, loops: map[string]types.UID{}}
}

// reset starts a new discovery pass: the next lookups are unresolved until the
// pass (or a live resolve) fills them.
func (c *attrCache) reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pods = map[string]podAttr{}
	c.loops = map[string]types.UID{}
}

func (c *attrCache) pod(key string) (podAttr, bool) {
	if c == nil {
		return podAttr{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	a, ok := c.pods[key]
	return a, ok
}

func (c *attrCache) setPod(key string, a podAttr) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pods[key] = a
}

func (c *attrCache) loop(key string) (types.UID, bool) {
	if c == nil {
		return "", false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	uid, ok := c.loops[key]
	return uid, ok
}

func (c *attrCache) setLoop(key string, uid types.UID) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loops[key] = uid
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

	// mu guards the engine stream's state, the workload set and the follower
	// table: every follower goroutine reports on the engine stream, and
	// discovery reads the table when a pod appears or goes away.
	mu sync.Mutex
	// engineHealthy is "an engine agent stream is attached": what makes a
	// count of 0 a fact rather than an absence.
	engineHealthy bool
	// engineFailed is "the engine stream could not be read on the node(s) we
	// know": a sandbox pod on such a node is UNKNOWN, never 0 (I32).
	engineFailedByNode map[string]bool
	// workloads maps a sandbox pod's Loop UID to the node it runs on, so an
	// engine pod's failure degrades only the Loops it serves (pi3 E6).
	workloads map[types.UID]string
	// followers holds the cancel func of every running stream, keyed
	// ns/pod/container. Discovery prunes a follower whose pod is gone (pi3 E2:
	// without it every deleted pod left a goroutine retrying forever).
	followers map[string]context.CancelFunc
	// startedAt is when the relay started: a sandbox pod created before it
	// cannot have had its whole life covered (pi3 E8).
	startedAt time.Time
	// attr caches the pod→Loop and Loop→UID resolutions (pi3 E5).
	attr *attrCache
}

// Start implements manager.Runnable. It returns when ctx is done.
func (r *Relay) Start(ctx context.Context) error {
	if r.Source == nil || r.Discoverer == nil || r.Attributor == nil || r.Holder == nil {
		// Not configured: there is nothing to follow. The operator always
		// configures it (cmd/main.go exits if it cannot); a bare test
		// reconciler does not.
		return nil
	}
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultDiscoverInterval
	}
	r.mu.Lock()
	if r.attr == nil {
		r.attr = newAttrCache()
	}
	if r.followers == nil {
		r.followers = map[string]context.CancelFunc{}
	}
	if r.engineFailedByNode == nil {
		r.engineFailedByNode = map[string]bool{}
	}
	if r.workloads == nil {
		r.workloads = map[types.UID]string{}
	}
	r.startedAt = time.Now()
	r.mu.Unlock()

	discover := func() {
		peers, err := r.Discoverer.Discover(ctx)
		if err != nil {
			// The streams already followed are unaffected, and the peers that
			// WERE listed are still used: a permissions problem on one source
			// must not blind the other two.
			//
			// R1 (F62 review round 2): an INCOMPLETE pass must not PRUNE. Discover
			// returns the peers of the sources that listed successfully, so a
			// transient List error on one source makes every pod of that source
			// look absent. Pruning on that cancels live followers, and the next
			// complete pass restarts them with since = nil — the replay filter
			// only runs when since != nil — so each restarted follower re-reads
			// TailLines and counts every Block again, then publishes Healthy. A
			// wrong number is the one thing this relay exists to prevent.
			//
			// The per-pass attribution cache is left alone for the same reason:
			// it is only rebuilt from a full read, and clearing it here would
			// throw away resolutions that are still valid. The next COMPLETE
			// pass resets it and prunes as usual.
			r.log().Error(err, "alert relay: pod discovery was incomplete (following what was listed; not pruning this pass)")
		} else {
			r.attr.reset()
		}
		var enginePeers int
		for _, peer := range peers {
			if peer.Kind == PeerEngine {
				enginePeers++
			}
		}
		current := map[string]struct{}{}
		for _, peer := range peers {
			key := peer.Namespace + "/" + peer.Pod + "/" + peer.Container
			current[key] = struct{}{}
			r.resolvePeer(ctx, peer)
			if peer.Kind == PeerWorkload {
				r.workloadSeen(ctx, peer, enginePeers > 0)
				continue
			}
			r.startFollower(ctx, key, peer)
		}
		if err != nil {
			// R1: a partial pass leaves every follower it did not see running.
			// They are counted from their own streams (which are still attached),
			// so nothing is lost by not pruning them here.
			return
		}
		r.pruneFollowers(current)
	}
	discover()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.pruneFollowers(nil)
			return nil
		case <-ticker.C:
			discover()
		}
	}
}

// startFollower launches the stream for a peer that is not being followed yet.
func (r *Relay) startFollower(ctx context.Context, key string, peer Peer) {
	r.mu.Lock()
	if _, ok := r.followers[key]; ok {
		r.mu.Unlock()
		return
	}
	fctx, cancel := context.WithCancel(ctx)
	r.followers[key] = cancel
	r.mu.Unlock()
	go r.follow(fctx, peer)
}

// pruneFollowers stops the streams whose pod is no longer discovered (pi3 E2).
// Without this a deleted proxy pod (recreated per Loop, constantly) left a
// goroutine retrying a log read that can only ever return NotFound.
func (r *Relay) pruneFollowers(current map[string]struct{}) {
	r.mu.Lock()
	var stale []context.CancelFunc
	for key, cancel := range r.followers {
		if _, ok := current[key]; ok {
			continue
		}
		stale = append(stale, cancel)
		delete(r.followers, key)
	}
	r.mu.Unlock()
	for _, cancel := range stale {
		cancel()
	}
}

// follow keeps ONE pod container's stream attached: it reads lines until the
// stream ends (a rotation, a broken read), re-attaches from the last accepted
// alert, and retries a follow that could not be opened at all. It returns when
// the pod's follower is pruned or the manager stops.
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
			if ctx.Err() != nil {
				return
			}
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
			al, kind := ClassifyLine(scanner.Bytes())
			if kind == LineNoise {
				// Ordinary container output (startup noise, unrelated JSON):
				// not an audit record, so not a decode failure (pi3 E1).
				continue
			}
			if kind == LineMalformed {
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

// resolvePeer caches a discovered pod's attribution for this pass (pi3 E5).
func (r *Relay) resolvePeer(ctx context.Context, peer Peer) {
	ns, name, ok := r.Attributor.PodLoop(ctx, peer.Namespace, peer.Pod)
	key := peer.Namespace + "/" + peer.Pod
	if !ok || name == "" {
		r.attr.setPod(key, podAttr{resolved: true})
		return
	}
	uid, haveUID := r.loopUID(ctx, ns, name)
	r.attr.setPod(key, podAttr{loopNS: ns, loopName: name, uid: uid, resolved: haveUID})
}

// loopUID resolves and caches a Loop's UID.
func (r *Relay) loopUID(ctx context.Context, namespace, name string) (types.UID, bool) {
	key := namespace + "/" + name
	if uid, ok := r.attr.loop(key); ok {
		return uid, uid != ""
	}
	uid, ok := r.Attributor.LoopUID(ctx, namespace, name)
	if !ok {
		// Cache the miss for this pass so a burst of alerts for a Loop that is
		// gone does not re-read the API per line.
		uid = ""
	}
	r.attr.setLoop(key, uid)
	return uid, uid != ""
}

// accept attributes one alert and records it: the per-Loop count (F62) and the
// Q4 line (F63). An alert that cannot be attributed is counted as unattributed
// and still emitted (with no Loop), never guessed.
func (r *Relay) accept(ctx context.Context, peer Peer, al Alert) {
	al.Source = firstNonEmpty(al.Source, peer.Source)
	loopNS, loopName := al.LoopNamespace, al.Loop
	var uid types.UID
	switch {
	case loopName != "":
		// The proxy records already know their Loop.
		uid, _ = r.loopUID(ctx, loopNS, loopName)
	case al.Pod != "":
		// The engine's line names the SANDBOX pod; the Loop comes from the
		// operator-set label on that pod, resolved once per pod (E5).
		key := al.Namespace + "/" + al.Pod
		attr, ok := r.attr.pod(key)
		if !ok {
			r.resolvePeer(ctx, Peer{Namespace: al.Namespace, Pod: al.Pod})
			attr, _ = r.attr.pod(key)
		}
		loopNS, loopName, uid = attr.loopNS, attr.loopName, attr.uid
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

// attachedNow records a successful attach: the engine stream's state for the
// sandbox pods on its node, or one proxy pod's Loop.
func (r *Relay) attachedNow(ctx context.Context, peer Peer) {
	if peer.Kind == PeerEngine {
		r.mu.Lock()
		// The engine log carries every sandbox's alerts ON ITS NODE, so a live
		// engine stream is what lets such a Loop's count be 0 rather than
		// unknown. A Loop whose stream was already degraded to a GAP stays
		// degraded (fail-closed); an Unavailable one may recover (a full
		// re-read is complete evidence — pi3 E6).
		r.engineHealthy = true
		delete(r.engineFailedByNode, peer.Node)
		uids := make([]types.UID, 0, len(r.workloads))
		for uid, node := range r.workloads {
			if peer.Node == "" || node == "" || node == peer.Node {
				uids = append(uids, uid)
			}
		}
		r.mu.Unlock()
		for _, uid := range uids {
			r.Holder.noteHealthy(uid)
		}
		return
	}
	if peer.Kind == PeerWorkload {
		return
	}
	// A proxy stream is NOT evidence about the engine (pi3 E3): its being
	// readable says nothing about whether blocks reached us, so it must not on
	// its own make a Loop healthy. It only feeds the count of proxy-side blocks.
	if r.engineOK(peer.Node) {
		if uid := r.peerLoopUID(ctx, peer); uid != "" {
			r.Holder.noteHealthy(uid)
		}
	}
}

// degraded records a stream the relay cannot trust.
func (r *Relay) degraded(ctx context.Context, peer Peer, health Health, reason string) {
	if peer.Kind == PeerEngine {
		r.mu.Lock()
		r.engineFailedByNode[peer.Node] = true
		// The engine pod serves ONE node: degrade only the Loops whose sandbox
		// runs there, not every Loop in the cluster (pi3 E6a).
		var uids []types.UID
		for uid, node := range r.workloads {
			if peer.Node == "" || node == "" || node == peer.Node {
				uids = append(uids, uid)
			}
		}
		r.mu.Unlock()
		for _, uid := range uids {
			r.Holder.degrade(uid, health, reason)
		}
		return
	}
	if uid := r.peerLoopUID(ctx, peer); uid != "" {
		r.Holder.degrade(uid, health, reason)
	}
}

// engineOK reports whether an engine stream is attached for the given node.
func (r *Relay) engineOK(node string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.engineHealthy {
		return false
	}
	return node == "" || !r.engineFailedByNode[node]
}

// workloadSeen records a sandbox pod: with the engine stream attached on its
// node, this is the evidence that makes "0 blocked actions" a fact rather than
// an absence. Without it, the Loop's count is UNKNOWN (the engine cannot be
// read), and a sandbox that predates the relay is never claimed as complete
// (pi3 E8).
func (r *Relay) workloadSeen(ctx context.Context, peer Peer, engineDiscovered bool) {
	uid := r.peerLoopUID(ctx, peer)
	if uid == "" {
		return
	}
	r.mu.Lock()
	r.workloads[uid] = peer.Node
	engineHealthy := r.engineHealthy
	failed := r.engineFailedByNode[peer.Node]
	if peer.Node == "" {
		failed = len(r.engineFailedByNode) > 0
	}
	r.mu.Unlock()
	switch {
	case engineHealthy && !failed:
		if !peer.Created.IsZero() && peer.Created.Before(r.startedAt) {
			// The sandbox existed before the relay did: its earlier life was
			// not observed, so this is a gap, not a complete count (pi3 E8).
			r.Holder.degrade(uid, HealthGap, ReasonStreamGap)
			return
		}
		r.Holder.noteHealthy(uid)
	case failed:
		// The engine stream on this node was found and could not be read: the
		// count for this Loop is UNKNOWN, never 0 (I32).
		r.Holder.degrade(uid, HealthUnavailable, ReasonStreamUnavailable)
	case engineDiscovered:
		// An engine pod exists and its stream has not attached yet: "no news
		// yet" — writing nothing is right (and the attach, or its failure,
		// decides on a later pass).
	default:
		// Sandbox pods but no engine pod at all: the engine is absent, which is
		// a fact about the count, not an absence of information.
		r.Holder.degrade(uid, HealthUnavailable, ReasonEngineUnavailable)
	}
}

// peerLoopUID resolves a discovered pod's Loop UID through the cached
// attribution (E5), then the Loop object.
func (r *Relay) peerLoopUID(ctx context.Context, peer Peer) types.UID {
	if peer.Pod == "" || r.Attributor == nil {
		return ""
	}
	key := peer.Namespace + "/" + peer.Pod
	attr, ok := r.attr.pod(key)
	if !ok {
		r.resolvePeer(ctx, peer)
		attr, _ = r.attr.pod(key)
	}
	if attr.uid != "" {
		return attr.uid
	}
	if attr.loopName == "" {
		return ""
	}
	uid, _ := r.loopUID(ctx, attr.loopNS, attr.loopName)
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
