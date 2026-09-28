package egress

import (
	"testing"
)

// CheckAllow is exact host (case-insensitive) AND exact port. It is the
// hostname check of the I42 egress proxy (ADR-0007 I42 resolution). The port is
// the enforcement of I41's port loss: a KubeArmor matchDNSQueries rule cannot
// carry a port, but the proxy checks host:port at CONNECT time.
func TestCheckAllow(t *testing.T) {
	// The allows are the effective AgentPolicy network allows as host:port
	// strings (the same form as policy.EffectivePolicy.Network).
	allows := []string{testAllowHost + ":443", testAllowHost2 + ":443"}

	tests := []struct {
		name   string
		host   string
		port   int
		wantOK bool
	}{
		{"allowed host and port", testAllowHost, 443, true},
		{"allowed host, disallowed port", testAllowHost, 8080, false},
		{"disallowed host", "evil.example.com", 443, false},
		{"host is case-insensitive", "PROXY.GOLANG.ORG", 443, true},
		{"empty allows deny everything", testAllowHost, 443, false}, // empty set
		{"second allow is honored", testAllowHost2, 443, true},
		{"second allow wrong port", testAllowHost2, 8443, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			allowSet := allows
			if tc.name == "empty allows deny everything" {
				allowSet = nil
			}
			got := CheckAllow(allowSet, tc.host, tc.port)
			if got != tc.wantOK {
				t.Fatalf("CheckAllow(%q, %d) = %v; want %v", tc.host, tc.port, got, tc.wantOK)
			}
		})
	}
}

// CheckAllow rejects a host with a different suffix (no substring match) —
// "golang.org" must not match an allow of "proxy.golang.org".
func TestCheckAllowNoSubstring(t *testing.T) {
	allowSet := []string{testAllowHost + ":443"}
	if CheckAllow(allowSet, "golang.org", 443) {
		t.Fatal("golang.org must not match an allow of " + testAllowHost)
	}
	if CheckAllow(allowSet, "xproxy.golang.org", 443) {
		t.Fatal("xproxy.golang.org must not match an allow of " + testAllowHost)
	}
}
