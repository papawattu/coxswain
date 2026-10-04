package toolproxy

import (
	"net"
	"net/netip"
)

// carveOutCIDRs are the standard non-allowlisted ranges the tool proxy must
// never dial, in addition to any operator-supplied pod/service CIDRs: the
// private, link-local, loopback, ULA and CGNAT ranges plus the in-cluster
// pod/service ranges (the I42a resolved-IP backstop).
var carveOutCIDRs = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
	"127.0.0.0/8",
	"100.64.0.0/10",
	"0.0.0.0/8",
	"224.0.0.0/4",
	"240.0.0.0/4",
	"::1/128",
	"::/128",
	"fc00::/7",
	"fe80::/10",
	"64:ff9b::/96",
}

// IPInCarveOuts reports whether ip falls in a carved-out range: one of the
// standard ranges or any operator-supplied pod/service CIDR. An
// unparseable value is carved out (fail closed).
func IPInCarveOuts(ip net.IP, extraCIDRs []string) bool {
	p, err := netip.ParseAddr(ip.String())
	if err != nil {
		return true
	}
	all := append(append([]string{}, carveOutCIDRs...), extraCIDRs...)
	for _, c := range all {
		if prefix, err := netip.ParsePrefix(c); err == nil && prefix.Contains(p) {
			return true
		}
	}
	return false
}
