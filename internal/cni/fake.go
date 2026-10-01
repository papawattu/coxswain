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

package cni

import (
	"context"
	"sync"
)

// FakeProber is the CNIProber used by envtests (D38 plan: "the fake in
// envtests is a struct that returns a configurable result"). It records the
// calls it was made so a spec can assert the Runnable actually probed.
type FakeProber struct {
	mu sync.Mutex
	// NextResult is returned by Probe.
	NextResult CNIProbeResult
	// NextErr is returned by Probe (a real API failure, not a probe
	// unavailable — ProbeUnavailable is expressed via NextResult.Reason).
	NextErr error
	// Calls counts Probe invocations.
	Calls int
}

// NewFakeProber returns a FakeProber whose default result is Unknown (no
// probe has run yet).
func NewFakeProber() *FakeProber {
	return &FakeProber{NextResult: CNIProbeResult{Reason: ReasonUnknown}}
}

// Probe implements CNIProber.
func (f *FakeProber) Probe(_ context.Context) (CNIProbeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	return f.NextResult, f.NextErr
}

// SetResult sets the result the next Probe returns.
func (f *FakeProber) SetResult(r CNIProbeResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.NextResult = r
}

// LatestResult implements CNIProber: the fake returns its configured result
// (what the specs drive), never runs a probe.
func (f *FakeProber) LatestResult() CNIProbeResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.NextResult
}

// CallsCount returns how many times Probe was called.
func (f *FakeProber) CallsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Calls
}
