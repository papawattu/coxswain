package egress

import (
	"net"
	"testing"
)

// CheckResolvedIP is the SSRF backstop (ADR-0007 I42 resolution): before the
// proxy dials, every resolved IP must fall outside the non-allowlisted
// private, link-local, loopback, and cluster-internal ranges. The dial goes to
// the same IP that is checked (no TOCTOU re-resolution) — enforced in the
// proxy handler, asserted here at the unit level as "is this IP dialable at
// all".
func TestCheckResolvedIP(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool // true = dialable (public), false = rejected
	}{
		{"public v4", "151.101.0.223", true},
		{"public v6", "2606:2800:220:1:248:1893:25c8:1946", true},
		{"RFC1918 10", "10.244.0.5", false},
		{"RFC1918 172.16", "172.16.0.1", false},
		{"RFC1918 192.168", "192.168.1.1", false},
		{"link-local (metadata)", "169.254.169.254", false},
		{"loopback", "127.0.0.1", false},
		{"CGNAT 100.64", "100.64.0.1", false},
		{"v6 loopback", "::1", false},
		{"v6 link-local", "fe80::1", false},
		{"v6 ULA", "fc00::1", false},
		// 0.0.0.0/8: dialing 0.0.0.0 on Linux reaches the local host.
		{"0.0.0.0/8 (local host)", "0.0.0.0", false},
		{"0.0.0.0/8 (x.x.x.x)", "0.1.2.3", false},
		// 224.0.0.0/4 multicast.
		{"multicast 224.0.0.0/4", "224.0.0.1", false},
		{"multicast 239.255.255.255", "239.255.255.255", false},
		// 240.0.0.0/4 reserved (incl. 255.255.255.255 broadcast).
		{"reserved 240.0.0.0/4", "240.0.0.1", false},
		{"broadcast 255.255.255.255", "255.255.255.255", false},
		// 64:ff9b::/96 NAT64 well-known prefix: a NAT64 address mapping to a
		// private IPv4 is rejected by the containment check. The last 4 bytes
		// embed the v4 (ac10:1 = 172.16.0.1, private).
		{"NAT64 well-known prefix (private v4)", "64:ff9b:0:0:0:0:ac10:1", false},
		// ::/128 unspecified: dialing :: behaves like 0.0.0.0 (local host).
		{"unspecified ::/128", "::", false},
		// The mapped form ::ffff:a.b.c.d is handled via To4 — a mapped
		// private IPv4 is rejected by the v4 carve-outs, a mapped public
		// IPv4 is dialable.
		{"mapped private v4 (via To4)", "::ffff:10.0.0.1", false},
		{"mapped public v4 (via To4)", "::ffff:151.101.0.223", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("bad test IP %q", tc.ip)
			}
			if got := CheckResolvedIP(ip); got != tc.want {
				t.Fatalf("CheckResolvedIP(%s) = %v; want %v", tc.ip, got, tc.want)
			}
		})
	}
}

// CIDR carve-outs: the operator passes the pod and service CIDRs; an IP inside
// either is rejected even if it is not in a standard private range (a kind pod
// CIDR may be a public-looking range on some configs, so the operator must be
// able to carve it out explicitly).
func TestCheckResolvedIPCIDRCarveOuts(t *testing.T) {
	carve := []string{"10.200.0.0/16", "10.96.0.0/12"} // pod + service CIDRs
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		{"in pod CIDR", "10.200.1.5", false},
		{"in service CIDR", "10.96.0.1", false},
		{"just outside carve-outs (public)", "8.8.8.8", true},
		{"10.0.0.0/8 but not in a named carve-out", "10.30.0.1", false}, // still RFC1918
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if got := CheckResolvedIPWithCarveOuts(ip, carve); got != tc.want {
				t.Fatalf("CheckResolvedIPWithCarveOuts(%s) = %v; want %v", tc.ip, got, tc.want)
			}
		})
	}
}
