// Package egress is the I42 egress proxy's enforcement core (ADR-0007 I42
// resolution). It is a pure, testable set of checks the proxy handler applies
// to every connection: the host:port policy check, the resolved-IP SSRF
// backstop, the SNI check, and the Q4 audit record. The proxy itself
// (cmd/egress-proxy) wires these into an HTTP/HTTPS forward proxy; the checks
// here are unit-tested without a network.
package egress

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// CheckAllow reports whether host:port is in the effective network allows. The
// allows are the effective AgentPolicy network allows in the same "host:port"
// form as policy.EffectivePolicy.Network. The host match is case-insensitive
// and exact (no substring, no wildcard in Phase 1); the port is the literal
// number. Checking host AND port here is what retires I41's port loss: a
// KubeArmor matchDNSQueries rule cannot carry a port, but the proxy checks
// the full pair at CONNECT time.
func CheckAllow(allows []string, host string, port int) bool {
	wantHost := strings.ToLower(host)
	for _, a := range allows {
		h, p, err := splitHostPort(a)
		if err != nil {
			continue
		}
		if strings.ToLower(h) != wantHost {
			continue
		}
		if p == port {
			return true
		}
	}
	return false
}

// splitHostPort splits a "host:port" allow entry. The host may be a literal
// IPv6 (bracketed, e.g. "[::1]:443"); the port is a decimal number. A bare
// host with no port is not a valid network allow (the port is required — it is
// the thing I41's port loss used to drop).
func splitHostPort(entry string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(entry)
	if err != nil {
		return "", 0, err
	}
	if host == "" {
		return "", 0, fmt.Errorf("empty host in %q", entry)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("bad port %q in %q: %w", portStr, entry, err)
	}
	if p < 0 || p > 65535 {
		return "", 0, fmt.Errorf("port out of range in %q", entry)
	}
	return host, p, nil
}

// carveOutCIDRs are the standard non-allowlisted ranges the proxy must never
// dial, in addition to any operator-supplied pod/service CIDRs. They cover the
// private, link-local, loopback, and CGNAT ranges the SSRF defence exists to
// stop (kube-apiserver, cloud metadata 169.254.169.254, node ports, and the
// in-cluster pod/service ranges).
var carveOutCIDRs = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
	"127.0.0.0/8",
	"100.64.0.0/10",
	// 0.0.0.0/8: dialing 0.0.0.0 on Linux reaches the local host (the
	// proxy pod itself).
	"0.0.0.0/8",
	// 224.0.0.0/4 multicast and 240.0.0.0/4 reserved (incl. 255.255.255.255
	// broadcast): not routable destinations.
	"224.0.0.0/4",
	"240.0.0.0/4",
	"::1/128",
	// The unspecified address: on Linux, dialing :: behaves like 0.0.0.0
	// (local host).
	"::/128",
	"fc00::/7",
	"fe80::/10",
	// 64:ff9b::/96 (NAT64 well-known prefix): can map a private IPv4 behind
	// a public-looking IPv6. The IETF well-known NAT64 prefix is 64:ff9b::/96.
	"64:ff9b::/96",
}

// CarveOutCIDRsV4 returns the standard non-allowlisted IPv4 ranges (the v4
// subset of carveOutCIDRs). Callers build NetworkPolicy ipBlock except lists
// from this so the netpol layer cannot drift from the egress binary's
// resolved-IP carve-outs (I42c review: a second hand-written v4 list had
// already lost 100.64.0.0/10).
func CarveOutCIDRsV4() []string {
	out := make([]string, 0, len(carveOutCIDRs))
	for _, c := range carveOutCIDRs {
		if p, err := netip.ParsePrefix(c); err == nil && p.Addr().Is4() {
			out = append(out, c)
		}
	}
	return out
}

// CarveOutCIDRsV6 returns the standard non-allowlisted IPv6 ranges (the v6
// subset of carveOutCIDRs), for the same drift-free netpol except lists.
func CarveOutCIDRsV6() []string {
	out := make([]string, 0, len(carveOutCIDRs))
	for _, c := range carveOutCIDRs {
		if p, err := netip.ParsePrefix(c); err == nil && !p.Addr().Is4() {
			out = append(out, c)
		}
	}
	return out
}

// IPInCarveOuts reports whether ip falls in the standard non-allowlisted
// (in-cluster / private / loopback / link-local / CGNAT) ranges — the same
// list the egress proxy uses to reject resolved IPs (CheckResolvedIP) — plus
// any operator-supplied extraCIDRs (the operator's pod/service CIDRs). It is
// the authoritative "is this IP an in-cluster target" range check, shared so
// the controller's I42e AgentPolicy validation (internal/policy) and the
// egress proxy's runtime resolved-IP check reject the same set.
//
// An unparseable IP is rejected (fail-closed): it is reported as in-cluster.
func IPInCarveOuts(ip net.IP, extraCIDRs []string) bool {
	return inCarveOuts(ip, append(append([]string{}, carveOutCIDRs...), extraCIDRs...))
}

// CheckResolvedIP reports whether the proxy may dial the resolved IP: it is
// dialable only if it is NOT in a standard private/link-local/loopback/CGNAT
// range. This is the resolved-IP check (SSRF defence) from the ADR — the
// hostname check (CheckAllow) is not enough because a name can resolve to a
// private IP (DNS rebinding, split-horizon). The dial is to the same IP that
// is checked (no TOCTOU re-resolution), enforced in the handler.
func CheckResolvedIP(ip net.IP) bool {
	return !inCarveOuts(ip, carveOutCIDRs)
}

// CheckResolvedIPWithCarveOuts is CheckResolvedIP plus the operator's
// pod/service CIDRs. The operator passes these from its config (kind/k3s
// exposes them via --pod-network-cidr / --service-cluster-ip-range); they are
// NOT auto-discovered per Loop. A kind pod CIDR can be a non-RFC1918 range on
// some configs, so the operator must be able to carve it out explicitly.
func CheckResolvedIPWithCarveOuts(ip net.IP, extraCIDRs []string) bool {
	return !inCarveOuts(ip, append(append([]string{}, carveOutCIDRs...), extraCIDRs...))
}

// inCarveOuts reports whether ip falls in any of the given CIDRs (parsed once
// here; the caller's hot path passes the parsed prefix list for efficiency —
// but Phase 1 does a small fixed set, so re-parsing is acceptable).
func inCarveOuts(ip net.IP, cidrs []string) bool {
	parsed := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			continue
		}
		parsed = append(parsed, p)
	}
	addr, ok := netipAddrFromStdlib(ip)
	if !ok {
		return true // unparseable IP is rejected (fail-closed)
	}
	for _, p := range parsed {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// netipAddrFromStdlib converts a net.IP to a netip.Addr. Both IPv4 (4-byte and
// 16-byte forms) and IPv6 are handled.
func netipAddrFromStdlib(ip net.IP) (netip.Addr, bool) {
	if v4 := ip.To4(); v4 != nil {
		return netipAddrFrom4(v4), true
	}
	// IPv6: the net.IP is 16 bytes.
	b := ip.To16()
	if b == nil {
		return netip.Addr{}, false
	}
	var a [16]byte
	copy(a[:], b)
	return netip.AddrFrom16(a), true
}

func netipAddrFrom4(v4 net.IP) netip.Addr {
	var a [4]byte
	copy(a[:], v4)
	return netip.AddrFrom4(a)
}
