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

	// P3: loopback / unspecified / link-local IP forms (rejected at the first
	// layer regardless of the CIDR config).
	loopbackCases := []string{
		"localhost:8080", "127.0.0.1:443", "127.0.0.0:443",
		"127.0.0.2:80", "127.0.0.255:80", "127.255.255.255:80",
		"0.0.0.0:80", "169.254.0.0:80", "169.254.169.254:80",
		"[::1]:443", "[::]:443",
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
		{"10.244.0.5:8080", true},  // pod CIDR
		{"10.244.99.99:443", true}, // pod CIDR
		{"10.96.0.1:443", true},    // service CIDR
		{"10.96.255.255:80", true}, // service CIDR
		{"8.8.8.8:443", false},     // public
		{"1.1.1.1:53", false},      // public
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

	// CIDR cases are skipped when the operator config is unset (the name and
	// loopback checks still run).
	_, ok := FindInClusterNetworkAllow([]string{"10.244.0.5:8080"}, "", "")
	if ok {
		t.Error("with no CIDRs configured, an IP-in-CIDR allow must NOT be flagged (the CRD CEL rule + egress proxy backstop cover it)")
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
