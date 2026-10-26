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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// scriptedSource answers Follow from a script: the Nth attach returns the Nth
// attachment (the last one repeats). It records the since argument of every
// attach, which is what the gap spec asserts on.
type scriptedSource struct {
	mu       sync.Mutex
	attaches []attachment
	follows  int
	since    []*time.Time
}

type attachment struct {
	lines []string
	err   error
}

func (s *scriptedSource) Follow(_ context.Context, _, _, _ string, since *time.Time) (io.ReadCloser, error) {
	s.mu.Lock()
	idx := s.follows
	s.follows++
	s.since = append(s.since, since)
	s.mu.Unlock()
	if idx >= len(s.attaches) {
		idx = len(s.attaches) - 1
	}
	a := s.attaches[idx]
	if a.err != nil {
		return nil, a.err
	}
	var b strings.Builder
	for _, line := range a.lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return io.NopCloser(strings.NewReader(b.String())), nil
}

func (s *scriptedSource) followCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.follows
}

func (s *scriptedSource) sinceAt(i int) *time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.since) {
		return nil
	}
	return s.since[i]
}

// fixedDiscoverer returns a fixed peer list.
type fixedDiscoverer struct {
	mu    sync.Mutex
	peers []Peer
	err   error
}

func (d *fixedDiscoverer) Discover(context.Context) ([]Peer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.peers, d.err
}

func (d *fixedDiscoverer) setPeers(peers ...Peer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.peers = peers
}

// fakeAttributor maps pods to Loops and Loops to UIDs.
type fakeAttributor struct {
	pods   map[string][2]string // "ns/pod" -> {loopNS, loopName}
	uids   map[string]types.UID // "ns/name" -> uid
	noPods bool
}

func (a *fakeAttributor) PodLoop(_ context.Context, namespace, pod string) (string, string, bool) {
	if a.noPods {
		return "", "", false
	}
	v, ok := a.pods[namespace+"/"+pod]
	if !ok {
		return "", "", false
	}
	return v[0], v[1], true
}

func (a *fakeAttributor) LoopUID(_ context.Context, namespace, name string) (types.UID, bool) {
	uid, ok := a.uids[namespace+"/"+name]
	return uid, ok
}

// kubeArmorBlock builds an engine alert line for the sandbox pod ns/pod.
func kubeArmorBlock(at time.Time, verdict, namespace, pod, resource string) string {
	return `{"UpdatedTime":"` + at.UTC().Format(time.RFC3339Nano) + `","Operation":"Network",` +
		`"NamespaceName":"` + namespace + `","PodName":"` + pod + `","ContainerName":"agent",` +
		`"Action":"` + verdict + `","Resource":"` + resource + `","PolicyName":"p"}`
}

func startTestRelay(t *testing.T, relay *Relay) *Holder {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := relay.Start(ctx); err != nil {
			t.Errorf("relay.Start: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return relay.Holder
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

const (
	f62LoopNS   = "ns1"
	f62LoopName = "l1"
	f62Sandbox  = "sb"
	f62SandboxP = "agent-pod"
)

func f62NewRelay(src Source) *Relay {
	return &Relay{
		Source: src,
		Discoverer: &fixedDiscoverer{peers: []Peer{
			{Namespace: "kubearmor", Pod: "agent-node-1", Kind: PeerEngine, Source: SourceKubeArmor},
			{Namespace: f62Sandbox, Pod: f62SandboxP, Kind: PeerWorkload},
		}},
		Attributor: &fakeAttributor{
			pods: map[string][2]string{
				f62Sandbox + "/" + f62SandboxP: {f62LoopNS, f62LoopName},
			},
			uids: map[string]types.UID{f62LoopNS + "/" + f62LoopName: f62UID},
		},
		Holder:   NewHolder(),
		Interval: time.Hour, // discovery runs once; the streams do the work
		Retry:    5 * time.Millisecond,
	}
}

// f62UID is the count key every fixture uses.
const f62UID = types.UID("uid-l1")

func f62Fixture(t *testing.T, src Source) *Holder {
	t.Helper()
	return startTestRelay(t, f62NewRelay(src))
}

// TestRelayCountsBlockedAlerts is F62 spec 1: two blocked alerts (plus an
// allowed one, which must not count) give count 2 and a healthy stream with the
// latest target.
//
// Mutation: drop the status write / the observe call → no count.
func TestRelayCountsBlockedAlerts(t *testing.T) {
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	src := &scriptedSource{attaches: []attachment{{lines: []string{
		kubeArmorBlock(base.Add(1*time.Second), "Block", f62Sandbox, f62SandboxP, "/etc/passwd"),
		kubeArmorBlock(base.Add(2*time.Second), "Allow", f62Sandbox, f62SandboxP, "api.example.com:443"),
		kubeArmorBlock(base.Add(3*time.Second), "Block", f62Sandbox, f62SandboxP, "/root/.ssh/id_ed25519"),
	}}}}
	holder := f62Fixture(t, src)

	var snap Snapshot
	waitFor(t, "a healthy count of 2", func() bool {
		s, ok := holder.Snapshot(f62UID)
		if !ok || s.Health != HealthHealthy || s.Count != 2 {
			return false
		}
		snap = s
		return true
	})
	if snap.LatestTarget != "/root/.ssh/id_ed25519" {
		t.Errorf("latest target = %q, want the last alert's target", snap.LatestTarget)
	}
	if snap.LatestAt.IsZero() {
		t.Errorf("latest alert time must be recorded")
	}
}

// TestRelayUnavailableWhenTheStreamNeverAttaches is F62 spec 2: a source that
// errors before any attach is UNKNOWN, never 0 (the I32 defect this replaces).
//
// Mutation: treat an outage as 0 → Unknown becomes a zero count.
func TestRelayUnavailableWhenTheStreamNeverAttaches(t *testing.T) {
	src := &scriptedSource{attaches: []attachment{{err: errors.New("forbidden: pods/log is not granted")}}}
	holder := f62Fixture(t, src)

	var snap Snapshot
	waitFor(t, "an unavailable stream", func() bool {
		s, ok := holder.Snapshot(f62UID)
		if !ok || s.Health != HealthUnavailable {
			return false
		}
		snap = s
		return true
	})
	if snap.Reason != ReasonStreamUnavailable {
		t.Errorf("reason = %q, want %q", snap.Reason, ReasonStreamUnavailable)
	}
	if snap.Count != 0 {
		t.Errorf("count = %d; an unavailable stream may not publish a number at all (the controller writes nil)", snap.Count)
	}
}

// TestRelayGapAfterReattach is F62 spec 3: the stream ends, the re-attach feeds
// sinceTime = the last accepted alert, and a first line LATER than that is a
// gap — Unknown, not a smaller number.
//
// Mutation: keep the last count on a gap → Health stays Healthy.
func TestRelayGapAfterReattach(t *testing.T) {
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	first := kubeArmorBlock(base.Add(time.Second), "Block", f62Sandbox, f62SandboxP, "/a")
	second := kubeArmorBlock(base.Add(30*time.Second), "Block", f62Sandbox, f62SandboxP, "/b")
	src := &scriptedSource{attaches: []attachment{
		{lines: []string{first}},
		{lines: []string{second}},
	}}
	holder := f62Fixture(t, src)

	waitFor(t, "a gap after the re-attach", func() bool {
		s, ok := holder.Snapshot(f62UID)
		return ok && s.Health == HealthGap && s.Reason == ReasonStreamGap
	})
	waitFor(t, "a second attach", func() bool { return src.followCount() >= 2 })
	if since := src.sinceAt(1); since == nil || !since.Equal(base.Add(time.Second)) {
		t.Errorf("the re-attach must pass sinceTime = the last accepted alert (%s), got %v",
			base.Add(time.Second), since)
	}
}

// TestRelayReplayedLineIsNotCountedTwice is F62 spec 6's second half: sinceTime
// is inclusive, so the line at the boundary comes back — it must not count
// twice, and a non-Block line must not count at all.
//
// Mutation: remove the dedup/replay check → the count is 3.
func TestRelayReplayedLineIsNotCountedTwice(t *testing.T) {
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	line := kubeArmorBlock(base.Add(time.Second), "Block", f62Sandbox, f62SandboxP, "/a")
	allow := kubeArmorBlock(base.Add(2*time.Second), "Allow", f62Sandbox, f62SandboxP, "api")
	src := &scriptedSource{attaches: []attachment{{lines: []string{line, line, allow}}}}
	holder := f62Fixture(t, src)

	waitFor(t, "a count of 1", func() bool {
		s, ok := holder.Snapshot(f62UID)
		return ok && s.Count == 1 && s.Health == HealthHealthy
	})
	// Give the (single) stream time to finish before asserting it stayed at 1.
	time.Sleep(20 * time.Millisecond)
	if s, _ := holder.Snapshot(f62UID); s.Count != 1 {
		t.Errorf("count = %d after a replayed Block and an Allow line, want 1", s.Count)
	}
}

// TestRelayUnattributedAlertIsCountedNotGuessed is F62 spec 5: an alert for a
// pod without the operator's Loop label is never attributed by namespace, and
// it is visible as unattributed.
//
// Mutation: attribute by namespace → some Loop gets a count it did not earn.
func TestRelayUnattributedAlertIsCountedNotGuessed(t *testing.T) {
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	src := &scriptedSource{attaches: []attachment{{lines: []string{
		kubeArmorBlock(base, "Block", "some-other-ns", "unlabelled-pod", "/etc/shadow"),
	}}}}
	relay := f62NewRelay(src)
	relay.Attributor = &fakeAttributor{noPods: true}
	holder := startTestRelay(t, relay)

	waitFor(t, "an unattributed alert", func() bool { return holder.Unattributed() == 1 })
	if _, ok := holder.Snapshot(f62UID); ok {
		t.Errorf("an unattributed alert must not create a count for a Loop")
	}
}

// TestRelayEmitsOneEnvelopeLinePerAlert is F63 spec 8: exactly one Q4 line per
// alert, every key present, iteration joined and non-empty, and the allowed
// alert says so.
//
// Mutation: omit iteration (or leave it empty) → the assertion fails.
func TestRelayEmitsOneEnvelopeLinePerAlert(t *testing.T) {
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	src := &scriptedSource{attaches: []attachment{{lines: []string{
		kubeArmorBlock(base.Add(time.Second), "Block", f62Sandbox, f62SandboxP, "/a"),
		kubeArmorBlock(base.Add(2*time.Second), "Allow", f62Sandbox, f62SandboxP, "api"),
	}}}}
	relay := f62NewRelay(src)
	var buf bytes.Buffer
	var mu sync.Mutex
	relay.Emit = &lockedWriter{w: &buf, mu: &mu}
	relay.Iteration = func(namespace, name string) string {
		if namespace != f62LoopNS || name != f62LoopName {
			t.Errorf("iteration joined for %s/%s, want %s/%s", namespace, name, f62LoopNS, f62LoopName)
		}
		return "7"
	}
	startTestRelay(t, relay)

	waitFor(t, "two audit lines", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Count(buf.String(), "\n") >= 2
	})
	mu.Lock()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("got %d lines for 2 alerts, want exactly one line each: %v", len(lines), lines)
	}
	verdicts := make([]string, 0, len(lines))
	for _, line := range lines {
		var rec map[string]string
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line is not a JSON object: %v (%s)", err, line)
		}
		for _, key := range []string{"time", "loop", "namespace", "iteration", "source", "action", "target", "verdict", "detail"} {
			if _, ok := rec[key]; !ok {
				t.Errorf("the Q4 envelope is missing %q: %s", key, line)
			}
		}
		if rec["iteration"] != "7" {
			t.Errorf("iteration = %q, want the joined status value 7: %s", rec["iteration"], line)
		}
		if rec["loop"] != f62LoopName || rec["namespace"] != f62LoopNS {
			t.Errorf("loop/namespace = %s/%s, want %s/%s", rec["namespace"], rec["loop"], f62LoopNS, f62LoopName)
		}
		if rec["source"] != SourceKubeArmor {
			t.Errorf("source = %q, want %q", rec["source"], SourceKubeArmor)
		}
		verdicts = append(verdicts, rec["verdict"])
	}
	if verdicts[0] != "blocked" || verdicts[1] != "allowed" {
		t.Errorf("verdicts = %v, want [blocked allowed]", verdicts)
	}
}

// lockedWriter serialises the relay's concurrent writes for the buffer.
type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestRelayRunOfUndecodableLinesIsUnknown is F62's "one bad line is tolerable"
// rule: a single non-JSON line is skipped, a run makes the stream untrusted.
//
// Mutation: treat an undecodable line as an alert / ignore the run → Unknown
// never appears.
func TestRelayRunOfUndecodableLinesIsUnknown(t *testing.T) {
	// pi3 E1: the engine's log starts with ordinary startup output. Three noise
	// lines must NOT be read as "the audit format changed" — only AUDIT-SHAPED
	// lines that fail to decode count toward the budget.
	noise := []string{
		`time=2026-10-26T10:00:00Z level=info msg="starting kubearmor"`,
		`{"level":"info","msg":"watching for events","component":"runtime"}`,
		`{"level":"debug","msg":"loaded policy","policy":"p"}`,
	}
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	good := kubeArmorBlock(base.Add(time.Second), "Block", f62Sandbox, f62SandboxP, "/a")

	// Half 1: noise only (+ one good alert) — healthy.
	quiet := &scriptedSource{attaches: []attachment{{lines: append(append([]string{}, noise...), good)}}}
	holder := f62Fixture(t, quiet)
	waitFor(t, "a healthy stream behind startup noise", func() bool {
		s, ok := holder.Snapshot(f62UID)
		return ok && s.Health == HealthHealthy && s.Count == 1
	})
	time.Sleep(20 * time.Millisecond)
	if s, _ := holder.Snapshot(f62UID); s.Health != HealthHealthy {
		t.Fatalf("startup noise degraded the stream to %q/%q; ordinary output is not a decode failure (E1)",
			s.Health, s.Reason)
	}

	// Half 2: the same noise, then a run of AUDIT-SHAPED lines that do not
	// decode — that is a real failure.
	src := &scriptedSource{attaches: []attachment{{lines: append(append([]string{}, noise...),
		good,
		`{"Operation":"Process",`,
		`{"Operation":"File","PodName":`,
		`{"Operation":"Network","NamespaceName":`,
	)}}}
	holder = f62Fixture(t, src)
	waitFor(t, "a stream with an undecodable audit-shaped run", func() bool {
		s, ok := holder.Snapshot(f62UID)
		if !ok {
			return false
		}
		return s.Health == HealthGap && s.Reason == ReasonUndecodable
	})
}

// TestRelayStopsFollowingADeletedPod is pi3 E2: the follower table is pruned on
// re-discovery, so a deleted pod does not leave a goroutine retrying a log read
// that can only return NotFound — forever, for every proxy pod a Loop ever had.
//
// Mutation: drop pruneFollowers (or never cancel) → the follow count keeps
// growing after the pod is gone.
func TestRelayStopsFollowingADeletedPod(t *testing.T) {
	src := &scriptedSource{attaches: []attachment{{err: errors.New("NotFound")}}}
	disc := &fixedDiscoverer{peers: []Peer{
		{Namespace: "kubearmor", Pod: "agent-node-1", Kind: PeerEngine, Source: SourceKubeArmor},
		{Namespace: f62Sandbox, Pod: f62SandboxP, Kind: PeerWorkload},
	}}
	relay := f62NewRelay(src)
	relay.Discoverer = disc
	relay.Interval = 10 * time.Millisecond
	relay.Retry = 2 * time.Millisecond
	startTestRelay(t, relay)

	waitFor(t, "the engine stream to be attempted", func() bool { return src.followCount() >= 1 })

	// The engine pod is gone (a rescheduled agent, an uninstalled engine).
	disc.setPeers(Peer{Namespace: f62Sandbox, Pod: f62SandboxP, Kind: PeerWorkload})
	// Let a discovery pass (Interval 10ms) prune the follower. One attempt may
	// still be in flight when it is cancelled, hence the small allowance; the
	// bug this pins grew the count by ~50 in the same window.
	time.Sleep(50 * time.Millisecond)
	settled := src.followCount()
	time.Sleep(100 * time.Millisecond)
	if got := src.followCount(); got > settled+2 {
		t.Fatalf("the follow count grew from %d to %d after the pod was pruned: the follower is still retrying (E2)",
			settled, got)
	}
}

// TestRelayProxyStreamAloneIsNotHealthy is pi3 E3: a readable egress-proxy log
// says nothing about whether the engine's blocks reached us, so it must not on
// its own publish a count (which would read "the engine blocked 0 action(s)").
//
// Mutation: noteHealthy on a proxy attach regardless of the engine → Healthy.
func TestRelayProxyStreamAloneIsNotHealthy(t *testing.T) {
	disc := &fixedDiscoverer{peers: []Peer{
		{Namespace: f62LoopNS, Pod: "l1-egress-proxy", Kind: PeerProxy, Source: SourceEgress},
	}}
	src := &scriptedSource{attaches: []attachment{{lines: []string{
		`{"time":"2026-10-26T10:00:00Z","loop":"l1","namespace":"ns1","source":"egress-proxy",` +
			`"action":"connect","target":"api:443","verdict":"blocked"}`,
	}}}}
	relay := f62NewRelay(src)
	relay.Discoverer = disc
	relay.Attributor = &fakeAttributor{
		pods: map[string][2]string{f62LoopNS + "/l1-egress-proxy": {f62LoopNS, f62LoopName}},
		uids: map[string]types.UID{f62LoopNS + "/" + f62LoopName: f62UID},
	}
	holder := startTestRelay(t, relay)

	waitFor(t, "the proxy line to be counted", func() bool {
		s, ok := holder.Snapshot(f62UID)
		return ok && s.Count == 1
	})
	time.Sleep(30 * time.Millisecond)
	if s, _ := holder.Snapshot(f62UID); s.Health == HealthHealthy {
		t.Fatalf("a proxy stream alone made the Loop Healthy; its count would read as a complete engine count (E3)")
	}
}

// TestRelayRecoversFromUnavailableOnAFullReattach is pi3 E6(b): a transient
// engine failure may recover. A full re-read (since == nil after a failure)
// re-reads the whole current log, which IS complete evidence for a sandbox that
// started after the relay; a GAP stays sticky (the count is genuinely
// incomplete).
//
// Mutation: make noteHealthy refuse Unavailable too → the Loop stays Unknown.
func TestRelayRecoversFromUnavailableOnAFullReattach(t *testing.T) {
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	src := &scriptedSource{attaches: []attachment{
		{err: errors.New("temporary apiserver error")},
		{lines: []string{kubeArmorBlock(base.Add(time.Second), "Block", f62Sandbox, f62SandboxP, "/a")}},
	}}
	holder := f62Fixture(t, src)

	waitFor(t, "the unavailable state", func() bool {
		s, ok := holder.Snapshot(f62UID)
		return ok && s.Health == HealthUnavailable
	})
	waitFor(t, "recovery to Healthy after the full re-read", func() bool {
		s, ok := holder.Snapshot(f62UID)
		return ok && s.Health == HealthHealthy
	})
}

// TestRelayEngineAbsentIsUnavailable is pi3 E10's EngineUnavailable decision: a
// sandbox pod with no engine pod at all is a fact about the count (there is no
// engine), not an absence of information.
func TestRelayEngineAbsentIsUnavailable(t *testing.T) {
	disc := &fixedDiscoverer{peers: []Peer{
		{Namespace: f62Sandbox, Pod: f62SandboxP, Kind: PeerWorkload},
	}}
	relay := f62NewRelay(&scriptedSource{attaches: []attachment{{err: errors.New("unused")}}})
	relay.Discoverer = disc
	holder := startTestRelay(t, relay)

	waitFor(t, "EngineUnavailable", func() bool {
		s, ok := holder.Snapshot(f62UID)
		return ok && s.Health == HealthUnavailable && s.Reason == ReasonEngineUnavailable
	})
}
