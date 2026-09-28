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

	"github.com/papawattu/coxswain/internal/egress"
)

// InClusterHostSuffixes are the in-cluster name suffixes the controller
// re-checks (I42e). The CRD CEL rule rejects these at admission, but the
// controller mirrors the rule's case-insensitive, trailing-dot-aware
// comparison so pre-rule objects and future CRD drift are also caught.
// "cluster.local" (the general form) is checked first so the more specific
// ".svc.cluster.local" is subsumed.
var InClusterHostSuffixes = []string{"cluster.local", ".svc"}

// inClusterHostnames are the non-IP literal hostnames that name the local
// node: "localhost" (and its well-known aliases). IP literals are handled by
// the range check in isIPInCluster, not a fixed string list — a fixed list
// (e.g. only "127.0.0.1") would miss 127.0.0.3, 169.254.1.1, [fe80::1], etc.
var inClusterHostnames = map[string]bool{
	"localhost":     true,
	"ip6-localhost": true,
	"ip6-loopback":  true,
	"ip4-loopback":  true,
	"ip4-localhost": true,
}

// IsInClusterHostname reports whether a normalized (lowercase, trailing-dot-
// trimmed) host names an in-cluster target: an in-cluster name suffix
// (.svc / cluster.local, mirroring the CRD CEL rule), a well-known local
// hostname (localhost & friends), or an IP literal in a non-allowlisted range.
// It is a pure function of the host string; it does not consult the operator's
// CIDR config (that is FindInClusterNetworkAllow's job).
func IsInClusterHostname(host string) bool {
	if inClusterHostnames[host] {
		return true
	}
	for _, suffix := range InClusterHostSuffixes {
		// Match both as a whole host (host == suffix) and as a suffix of the
		// host. ".svc" is a dotted suffix ("my-svc.default.svc" ends with
		// ".svc"); "cluster.local" may appear as the whole host or dotted
		// ("a.b.cluster.local").
		if host == suffix || strings.HasSuffix(host, "."+suffix) || strings.HasSuffix(host, suffix) {
			return true
		}
	}
	// If the host is an IP literal, test it against the standard
	// non-allowlisted (in-cluster) ranges — the same list the egress proxy
	// rejects resolved IPs against — plus the Go range predicates, rather than
	// a fixed string list, so 127.0.0.3, 169.254.1.1, [fe80::1], [fd12::1],
	// 100.64.1.1, [ff02::1] (site-local multicast), ... are all caught.
	if ip := net.ParseIP(host); ip != nil {
		return ipIsInCluster(ip)
	}
	return false
}

// ipIsInCluster reports whether an IP literal is a non-allowlisted
// (in-cluster) target. It reuses the egress proxy's authoritative carve-out
// list (CheckResolvedIP / IPInCarveOuts) and ORs in the Go range predicates
// (loopback / unspecified / link-local-unicast / link-local-multicast /
// private) so the union covers every range the standard predicates name —
// including IPv6 site-local multicast (ff02::/16), which is a non-routable
// multicast destination but not in the egress CIDR list.
func ipIsInCluster(ip net.IP) bool {
	if egress.IPInCarveOuts(ip, nil) {
		return true
	}
	return ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsPrivate()
}

// FindInClusterNetworkAllow returns the first network allow in the union that
// names an in-cluster target (I42e first layer): an in-cluster name (.svc /
// .svc.cluster.local / cluster.local — mirroring the CRD CEL rule), a
// well-known local hostname (localhost & friends), an IP literal in a
// non-allowlisted range (127/8, 169.254/16, 0/8, ::1, fc00::/7, fe80::/10,
// CGNAT 100.64/10, NAT64, multicast/reserved — the egress carve-out list), or
// an IP literal inside podCIDR or serviceCIDR. Returns ("", false) when every
// allow is a legitimate external target.
//
// An empty podCIDR / serviceCIDR only skips the operator-CIDR case; the name,
// hostname, and standard-range checks always run. The caller is responsible
// for making an unset operator-CIDR config visible (the controller logs a
// warning at startup).
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
		// An IP literal inside the operator's pod/service CIDR (the only case
		// that needs operator config). The standard-range check above already
		// covers the RFC1918 / loopback / link-local / CGNAT ranges, so an
		// in-range pod-CIDR IP is caught here only when the CIDRs are set.
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
