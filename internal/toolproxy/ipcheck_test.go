package toolproxy

import (
	"net"
	"testing"
)

func TestIPInCarveOuts(t *testing.T) {
	cases := []struct {
		ip     string
		extra  []string
		carved bool
	}{
		{"10.0.0.5", nil, true},
		{"172.16.0.5", nil, true},
		{"192.168.1.5", nil, true},
		{"169.254.169.254", nil, true},
		{"127.0.0.1", nil, true},
		{"100.64.0.1", nil, true},
		{"0.0.0.0", nil, true},
		{"224.0.0.5", nil, true},
		{"240.0.0.5", nil, true},
		{"255.255.255.255", nil, true},
		{"::1", nil, true},
		{"::", nil, true},
		{"fc00::1", nil, true},
		{"fd12::1", nil, true},
		{"fe80::1", nil, true},
		{"64:ff9b::1", nil, true},
		{"1.1.1.1", nil, false},
		{"8.8.8.8", nil, false},
		{"2001:4860:4860::8888", nil, false},
		{"1.2.3.4", []string{"1.2.0.0/16"}, true},
		{"1.2.3.4", []string{"9.9.9.9/32"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			got := IPInCarveOuts(net.ParseIP(tc.ip), tc.extra)
			if got != tc.carved {
				t.Fatalf("IPInCarveOuts(%s, %v) = %v, want %v", tc.ip, tc.extra, got, tc.carved)
			}
		})
	}
	// A malformed value fails closed.
	if !IPInCarveOuts(net.IP("not-an-ip"), nil) {
		t.Fatal("unparseable IP must be carved out (fail closed)")
	}
}
