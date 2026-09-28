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

// Package policy — I42e first layer of the SSRF defence (ADR-0007 I42
// resolution): a network allow must not name an in-cluster target, because the
// agent's external egress is enforced by the operator's egress proxy (I42a/
// I42b) and an in-cluster "external" allow is the exfiltration path.
//
// This file holds the pure, engine-agnostic checks the controller runs before
// it creates the egress proxy. The CRD CEL rule (AgentPolicy.spec.network)
// rejects the name cases at admission; the controller is the authoritative
// first layer because it catches objects created before the rule and any
// future CRD drift, and it can see the operator's pod/service CIDR config
// (which the CRD cannot, without cluster config).
package policy

import (
	"net"
	"strings"
)

// InClusterHostSuffixes are the in-cluster name suffixes the controller
// re-checks (I42e). The CRD CEL rule rejects these at admission, but the
// controller mirrors the rule's case-insensitive, trailing-dot-aware
// comparison so pre-rule objects and future CRD drift are also caught.
// "cluster.local" (the general form) is checked first so the more specific
// ".svc.cluster.local" is subsumed.
var InClusterHostSuffixes = []string{"cluster.local", ".svc"}

// InClusterHostnames are the literal hostnames that name the local node
// (I42e first layer): "localhost" plus every loopback / unspecified /
// link-local IP form — the well-known members of 127/8, 169.254/16,
// 0.0.0.0, and [::]. The egress proxy's resolved-IP check (I42a) is the
// backstop that also catches rebinding / split-horizon; the first layer
// rejects these well-known forms outright.
var InClusterHostnames = map[string]bool{
	"localhost": true, "127.0.0.1": true, "127.0.0.0": true,
	"127.0.0.2": true, "127.0.0.255": true, "127.255.255.255": true,
	"0.0.0.0": true, "169.254.0.0": true, "169.254.169.254": true,
	"::1": true, "::": true, "fe80::": true, "fc00::": true, "fd00::": true,
}

// IsInClusterHostname reports whether a normalized (lowercase, trailing-dot-
// trimmed) host names an in-cluster target: an in-cluster name suffix
// (.svc / cluster.local, mirroring the CRD CEL rule) or a loopback /
// unspecified / link-local hostname. It is a pure function of the host
// string; it does not consult the operator's CIDR config (that is
// FindInClusterNetworkAllow's job).
func IsInClusterHostname(host string) bool {
	if InClusterHostnames[host] {
		return true
	}
	for _, suffix := range InClusterHostSuffixes {
		// Match both as a whole host (host == suffix) and as a suffix of the
		// host. ".svc" is a dotted suffix ("my-svc.default.svc" ends with ".svc");
		// "cluster.local" may appear as the whole host or dotted
		// ("a.b.cluster.local"). The bare HasSuffix(host, suffix) covers the
		// ".svc" case (a host cannot end in ".svc" without the dot being part
		// of the label separator).
		if host == suffix || strings.HasSuffix(host, "."+suffix) || strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// FindInClusterNetworkAllow returns the first network allow in the union that
// names an in-cluster target (I42e first layer): an in-cluster name (.svc /
// .svc.cluster.local / cluster.local — mirroring the CRD CEL rule), a loopback
// / unspecified / link-local hostname (localhost, 127/8, 169.254/16,
// 0.0.0.0, ::1, etc.), or an IP literal inside podCIDR or serviceCIDR.
// Returns ("", false) when every allow is a legitimate external target.
//
// An empty podCIDR / serviceCIDR means the CIDR cases are skipped (the name
// and loopback checks still run) — the caller is responsible for making an
// unset CIDR config visible (the controller logs a warning at startup).
func FindInClusterNetworkAllow(allows []string, podCIDR, serviceCIDR string) (string, bool) {
	cidrs := make([]string, 0, 2)
	if podCIDR != "" {
		cidrs = append(cidrs, podCIDR)
	}
	if serviceCIDR != "" {
		cidrs = append(cidrs, serviceCIDR)
	}
	for _, a := range allows {
		host := HostPartOfAllow(a)
		if host == "" {
			continue // malformed: the egress proxy rejects it at dial time
		}
		if IsInClusterHostname(host) {
			return a, true
		}
		if ip := net.ParseIP(host); ip != nil && len(cidrs) > 0 {
			for _, cidr := range cidrs {
				if ipInCIDR(ip, cidr) {
					return a, true
				}
			}
		}
	}
	return "", false
}

// HostPartOfAllow returns the host part of a "host:port" network allow (I42e).
// It mirrors the egress proxy's splitHostPort: a bare host (no port) is not a
// valid network allow and yields ""; an IPv6 host is bracketed ("[::1]:443"),
// and the brackets are stripped so the literal can be parsed as an IP. A
// trailing root dot ("kubernetes.default.svc.") is trimmed so the name suffix
// can be matched case-insensitively; the result is lowercased.
func HostPartOfAllow(entry string) string {
	host, _, err := net.SplitHostPort(entry)
	if err != nil {
		return ""
	}
	host = strings.Trim(host, "[]")
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return ""
	}
	return strings.ToLower(host)
}

// ipInCIDR reports whether ip falls within the CIDR. A malformed CIDR never
// matches (a misconfigured CIDR should be fixed at the deployment, not
// silently block every reconcile).
func ipInCIDR(ip net.IP, cidr string) bool {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	return ipnet.Contains(ip)
}
