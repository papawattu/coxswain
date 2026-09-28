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

package policy

// I42e (review P2/P3): the controller-side in-cluster-allow check. The name
// suffix cases are mirrored here (case-insensitive, trailing-dot-aware) so
// objects created before the CRD CEL rule and any future CRD drift are caught;
// the loopback / unspecified / link-local forms (P3) are rejected outright at
// the first layer; the IP-in-CIDR cases need the operator's CIDR config.

import (
	"testing"
)

// externalHost is a legitimate external host (never in-cluster) shared by the
// cases that assert it is not flagged.
const externalHost = "proxy.golang.org"

func TestFindInClusterNetworkAllow(t *testing.T) {
	// P2: case-insensitive + trailing-dot name suffixes (the controller
	// mirrors the CRD CEL rule). The first element is the allow, the second
	// whether it must be flagged in-cluster.
	nameCases := []struct {
		allow     string
		inCluster bool
	}{
		// lowercase (baseline)
		{"my-service.default.svc:443", true},
		{"kubernetes.default.svc.cluster.local:443", true},
		// P2: uppercase / mixed case
		{"api.default.SVC:443", true},
		{"Kubernetes.Default.Svc:443", true},
		{"API.DEFAULT.SVC.CLUSTER.LOCAL:443", true},
		// P2: trailing root dot
		{"kubernetes.default.svc.cluster.local.:443", true},
		{"my-svc.default.svc.:443", true},
		{"KUBERNETES.DEFAULT.SVC.:443", true},
		// cluster.local general form (mirrors the CRD's .svc.cluster.local)
		{"my-svc.default.cluster.local:443", true},
		// legitimate external hosts (must NOT be flagged)
		{externalHost + ":443", false},
		{"github.com:443", false},
		{"api.github.com:443", false},
		// a name that contains "svc" but is not a .svc suffix
		{"servicewarehouse.example.com:443", false},
	}
	for _, c := range nameCases {
		got, ok := FindInClusterNetworkAllow([]string{c.allow}, "", "")
		if ok != c.inCluster {
			t.Errorf("FindInClusterNetworkAllow(%q) inCluster = %v, want %v", c.allow, ok, c.inCluster)
		}
		if ok && got != c.allow {
			t.Errorf("FindInClusterNetworkAllow(%q) returned %q, want %q", c.allow, got, c.allow)
		}
	}

	// P3 + P2(range): loopback / unspecified / link-local / CGNAT / NAT64 /
	// reserved IP forms, caught by the range check (egress.IPInCarveOuts), not a
	// fixed string list — so 127.0.0.3, 169.254.1.1, [fe80::1], [fd12::1] are
	// all rejected. Rejected at the first layer regardless of the CIDR config.
	loopbackCases := []string{
		"localhost:8080", "127.0.0.1:443", "127.0.0.0:443",
		"127.0.0.2:80", "127.0.0.3:80", "127.0.0.255:80", "127.255.255.255:80",
		"0.0.0.0:80", "0.1.2.3:80",
		"169.254.0.0:80", "169.254.1.1:80", "169.254.169.254:80",
		"[::1]:443", "[::]:443", "[fe80::1]:443", "[fd12::1]:443",
		"[fc00::abcd]:443", "[64:ff9b::1.1.2.3]:443", "[ff02::1]:443",
		"100.64.1.1:443",                                     // CGNAT
		"192.168.1.10:443", "172.16.5.5:443", "10.0.0.7:443", // RFC1918
		"255.255.255.255:80", "224.0.0.1:443", // broadcast / multicast
	}
	for _, a := range loopbackCases {
		got, ok := FindInClusterNetworkAllow([]string{a}, "", "")
		if !ok {
			t.Errorf("FindInClusterNetworkAllow(%q) must be flagged in-cluster (loopback/unspecified/link-local)", a)
		}
		if ok && got != a {
			t.Errorf("FindInClusterNetworkAllow(%q) returned %q, want %q", a, got, a)
		}
	}

	// IP-in-CIDR cases (need the operator's CIDR config).
	const (
		podCIDR     = "10.244.0.0/16"
		serviceCIDR = "10.96.0.0/12"
	)
	cidrCases := []struct {
		allow     string
		inCluster bool
	}{
		{"10.244.0.5:8080", true},    // pod CIDR
		{"10.244.99.99:443", true},   // pod CIDR
		{"10.96.0.1:443", true},      // service CIDR
		{"10.96.255.255:80", true},   // service CIDR
		{"8.8.8.8:443", false},       // public
		{"1.1.1.1:53", false},        // public
		{"93.184.216.34:443", false}, // public (example.com)
	}
	for _, c := range cidrCases {
		got, ok := FindInClusterNetworkAllow([]string{c.allow}, podCIDR, serviceCIDR)
		if ok != c.inCluster {
			t.Errorf("FindInClusterNetworkAllow(%q) with CIDRs inCluster = %v, want %v", c.allow, ok, c.inCluster)
		}
		if ok && got != c.allow {
			t.Errorf("FindInClusterNetworkAllow(%q) returned %q, want %q", c.allow, got, c.allow)
		}
	}

	// The operator-CIDR-only case is skipped when the operator config is unset.
	// Use an IP in a NON-standard range (TEST-NET-3) that is only flagged via
	// the operator's explicit CIDR — with no CIDRs configured it must not be
	// flagged. (Standard-range IPs like 10.244.0.5 are now always caught by
	// the range check, correctly fail-closed, even without operator CIDRs.)
	_, ok := FindInClusterNetworkAllow([]string{"203.0.113.5:8080"}, "", "")
	if ok {
		t.Error("with no operator CIDRs configured, an IP outside the standard ranges must NOT be flagged")
	}
	// ...and it IS flagged once the operator carves out that range.
	_, ok = FindInClusterNetworkAllow([]string{"203.0.113.5:8080"}, "203.0.113.0/24", "")
	if !ok {
		t.Error("with the operator's pod CIDR configured, an IP in that CIDR must be flagged")
	}

	// Malformed entries (no port) are skipped.
	_, ok = FindInClusterNetworkAllow([]string{externalHost}, "", "")
	if ok {
		t.Error("a bare host (no port) is not a valid network allow; it must be skipped")
	}
}

func TestHostPartOfAllow(t *testing.T) {
	cases := map[string]string{
		externalHost + ":443":                       externalHost,
		"api.default.SVC:443":                       "api.default.svc",
		"kubernetes.default.svc.cluster.local.:443": "kubernetes.default.svc.cluster.local",
		"[::1]:443":                                 "::1",
		"localhost:8080":                            "localhost",
		externalHost:                                "", // malformed: no port
	}
	for entry, want := range cases {
		if got := HostPartOfAllow(entry); got != want {
			t.Errorf("HostPartOfAllow(%q) = %q, want %q", entry, got, want)
		}
	}
}
