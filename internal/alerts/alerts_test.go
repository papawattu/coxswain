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
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// kubeArmorLine is the engine's alert JSON in the shape KubeArmor writes it
// (PascalCase, one object per line). It carries an extra key this operator
// does not know (`HostPPID`) on purpose: a real line has ~25 keys and gains
// more between releases, so the decode must ignore what it does not need.
//
// NOTE: this is the documented KubeArmor alert shape, NOT a line captured on a
// cluster — no cluster was available (the disk-space pause). Capturing one and
// replacing this constant is the follow-up the PR body names.
const kubeArmorLine = `{"Timestamp":1690000000,"UpdatedTime":"2023-07-22T10:13:20.123456Z",` +
	`"Operation":"Process","ClusterName":"default","HostName":"node-1",` +
	`"NamespaceName":"sandbox","PodName":"agent-pod","ContainerName":"agent",` +
	`"ContainerID":"a1b2c3","ContainerImage":"docker.io/library/golang:1.26",` +
	`"ParentProcessName":"/bin/bash","ProcessName":"/bin/sh","HostPPID":4242,` +
	`"HostPID":4243,"PPID":17,"PID":18,"UID":0,"PolicyName":"coxswain-agent",` +
	`"Severity":"5","Type":"MatchedPolicy","Source":"/bin/sh","Resource":"/etc/passwd",` +
	`"Action":"Block","Data":"syscall=SYS_OPENAT"}`

func TestParseLineKubeArmor(t *testing.T) {
	al, ok := ParseLine([]byte(kubeArmorLine))
	if !ok {
		t.Fatalf("a real KubeArmor alert line must decode")
	}
	if !al.Blocked {
		t.Errorf("Action=Block must read as blocked")
	}
	if al.Source != SourceKubeArmor {
		t.Errorf("source = %q, want %q", al.Source, SourceKubeArmor)
	}
	if al.Namespace != "sandbox" || al.Pod != "agent-pod" {
		t.Errorf("workload = %s/%s, want sandbox/agent-pod (the SANDBOX pod, not the agent pod the line was read from)",
			al.Namespace, al.Pod)
	}
	if al.Target != "/etc/passwd" {
		t.Errorf("target = %q, want the resource the alert is about", al.Target)
	}
	if al.Operation != "Process" {
		t.Errorf("operation = %q, want Process", al.Operation)
	}
	wantTime, _ := time.Parse(time.RFC3339Nano, "2023-07-22T10:13:20.123456Z")
	if !al.Time.Equal(wantTime) {
		t.Errorf("time = %s, want the line's own UpdatedTime %s", al.Time, wantTime)
	}
	if !strings.Contains(al.Detail, "policy=coxswain-agent") {
		t.Errorf("detail = %q, want the policy name for correlation", al.Detail)
	}
}

func TestParseLineKubeArmorTimestampOnly(t *testing.T) {
	// The engine writes Timestamp (Unix seconds) on older lines; without a
	// usable clock the relay cannot order or dedupe the line, so it must still
	// decode rather than drop every old-format alert.
	al, ok := ParseLine([]byte(`{"Timestamp":1690000000,"Operation":"Network","Action":"Allow","NamespaceName":"sandbox","PodName":"p"}`))
	if !ok {
		t.Fatalf("a Timestamp-only line must decode")
	}
	if al.Blocked {
		t.Errorf("Action=Allow must not read as blocked")
	}
	if al.Time.Unix() != 1690000000 {
		t.Errorf("time = %d, want 1690000000", al.Time.Unix())
	}
}

func TestParseLineEgressProxy(t *testing.T) {
	line := `{"time":"2026-10-26T10:00:00Z","loop":"l1","namespace":"ns1","source":"egress-proxy",` +
		`"action":"connect","target":"api.example.com:443","verdict":"blocked","detail":"ip=1.2.3.4, policy=abc"}`
	al, ok := ParseLine([]byte(line))
	if !ok {
		t.Fatalf("an egress-proxy record must decode")
	}
	if !al.Blocked || al.Verdict() != "blocked" {
		t.Errorf("verdict blocked must read as blocked (verdict()=%q)", al.Verdict())
	}
	if al.Loop != "l1" || al.LoopNamespace != "ns1" {
		t.Errorf("loop = %s/%s, want ns1/l1: the proxy record already knows its Loop", al.LoopNamespace, al.Loop)
	}
	if al.Target != "api.example.com:443" {
		t.Errorf("target = %q", al.Target)
	}
	if al.Source != SourceEgress {
		t.Errorf("source = %q", al.Source)
	}
}

func TestParseLineToolProxy(t *testing.T) {
	line := `{"time":"2026-10-26T10:00:00Z","loop":"l1","namespace":"ns1","source":"tool-proxy",` +
		`"action":"request","tool":"git","method":"POST","path":"/repos/x/y/pulls","status":403,"policy":"h"}`
	al, ok := ParseLine([]byte(line))
	if !ok {
		t.Fatalf("a tool-proxy record must decode")
	}
	if !al.Blocked {
		t.Errorf("status 403 must read as blocked")
	}
	if al.Target != "POST /repos/x/y/pulls" {
		t.Errorf("target = %q", al.Target)
	}
	if al.Source != SourceToolProxy {
		t.Errorf("source = %q", al.Source)
	}
}

func TestParseLineRejectsWhatItCannotUse(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"not json":         `time=2026-10-26 level=info msg="starting"`,
		"truncated":        `{"Timestamp":1690000000,"Operation":"Proc`,
		"no timestamp":     `{"Operation":"Process","Action":"Block"}`,
		"unknown json":     `{"some":"other","json":"object"}`,
		"unparseable time": `{"time":"yesterday","loop":"l1","verdict":"blocked"}`,
	}
	for name, line := range cases {
		if _, ok := ParseLine([]byte(line)); ok {
			t.Errorf("%s: %q must not decode as an alert", name, line)
		}
	}
}

// TestWriteRecordEnvelope pins the Q4 envelope (ADR-0007 Q4; CONTEXT.md:129):
// every key present, one line, and the fields the reviewer can grep.
func TestWriteRecordEnvelope(t *testing.T) {
	var buf bytes.Buffer
	rec := Record{
		Time:      "2026-10-26T10:00:00Z",
		Loop:      "l1",
		Namespace: "ns1",
		Iteration: "7",
		Source:    SourceKubeArmor,
		Action:    "Block",
		Target:    "/etc/passwd",
		Verdict:   "blocked",
		Detail:    "policy=p",
	}
	if err := WriteRecord(&buf, rec); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := strings.Count(buf.String(), "\n"); got != 1 {
		t.Fatalf("exactly one line per alert, got %d newlines in %q", got, buf.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("the line must be JSON: %v", err)
	}
	for _, key := range []string{"time", "loop", "namespace", "iteration", "source", "action", "target", "verdict", "detail"} {
		v, ok := decoded[key]
		if !ok {
			t.Errorf("the Q4 envelope must carry %q", key)
			continue
		}
		if s, isString := v.(string); isString && s == "" && key == "iteration" {
			t.Errorf("iteration must never be empty (R31 I107 row 24)")
		}
	}
}

func TestAlertVerdict(t *testing.T) {
	if got := (Alert{Blocked: true}).Verdict(); got != "blocked" {
		t.Errorf("blocked verdict = %q", got)
	}
	if got := (Alert{}).Verdict(); got != "allowed" {
		t.Errorf("allow verdict = %q", got)
	}
}
