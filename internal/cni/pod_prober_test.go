// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on the "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cni

import (
	"strings"
	"testing"
)

// D38s2: the probe pod is the D38 property measured from inside a pod with an
// agent-shaped allow (DNS only). The probe must be runnable on the default
// image (python:3-alpine — no bash, no /dev/tcp), must emit exactly
// expectedResultLen RESULT lines plus DONE on stdout (the termination
// message), and must target the D38 property's endpoints.
func TestProbeCommandShape(t *testing.T) {
	p := NewPodProber(PodProberConfig{
		Namespace:  "coxswain-cni-probe",
		ProbeImage: "python:3-alpine",
	})
	cmd := p.probeCommand()
	if len(cmd) != 3 || cmd[0] != "python3" || cmd[1] != "-c" {
		t.Fatalf("the probe must run as python3 -c on the default image (no bash), got: %v", cmd)
	}
	script := cmd[2]
	for _, want := range []string{
		"RESULT %s REACHABLE", "RESULT %s BLOCKED", "DONE",
		"kubernetes.default.svc.cluster.local", "10250", "1.1.1.1", "99.99.99.99",
		"settimeout",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("probe script missing %q", want)
		}
	}
	// The four target labels the parse() validator expects.
	for _, label := range []string{"APISERVER_SVC", "KUBELET_NODE", "EXTERNAL", "BLOCK_ONLY"} {
		if !strings.Contains(script, label) {
			t.Errorf("probe script missing target label %q", label)
		}
	}
}

// D38 design point 4: strict validation. A termination message with the wrong
// line count, an unknown label, an unknown verdict, or a non-zero pod exit is
// ProbeUnavailable — never a false pass.
func TestProbeParseStrictValidation(t *testing.T) {
	p := NewPodProber(PodProberConfig{Namespace: "coxswain-cni-probe", ProbeImage: "python:3-alpine"})
	allBlocked := "RESULT APISERVER_SVC BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT BLOCK_ONLY BLOCKED\nDONE"
	res, err := p.parse(allBlocked, pod(allBlocked, 0))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.Reason != ReasonCNIEnforced {
		t.Fatalf("all BLOCKED -> CNIEnforced, got %v", res)
	}

	oneReachable := "RESULT APISERVER_SVC REACHABLE\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT BLOCK_ONLY BLOCKED\nDONE"
	res, err = p.parse(oneReachable, pod(oneReachable, 0))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.Reason != ReasonCNIUnenforced {
		t.Fatalf("one REACHABLE -> CNIUnenforced, got %v", res)
	}

	cases := map[string]struct {
		msg  string
		exit int32
	}{
		"missing RESULT line": {"RESULT APISERVER_SVC BLOCKED\nDONE", 0},
		"unknown label":       {"RESULT APISERVER_SVC BLOCKED\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT WHATEVER BLOCKED\nDONE", 0},
		"unknown verdict":     {"RESULT APISERVER_SVC MEDIUM\nRESULT KUBELET_NODE BLOCKED\nRESULT EXTERNAL BLOCKED\nRESULT BLOCK_ONLY BLOCKED\nDONE", 0},
		"non-zero exit":       {allBlocked, 1},
		"empty message":       {"", 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := p.parse(tc.msg, pod(tc.msg, tc.exit))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if res.Reason != ReasonProbeUnavailable {
				t.Fatalf("want ProbeUnavailable, got %v (detail=%q)", res.Reason, res.Detail)
			}
		})
	}
}
