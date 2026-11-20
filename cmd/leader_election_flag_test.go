package main

import (
	"strings"
	"testing"
	"time"
)

// The defaults must ride through a multi-second API-server stall. On the
// homelab (2026-11-20) an etcd member with a slow disk stalled lease reads
// for several seconds every 20–40 min; controller-runtime's 15s/10s/2s
// defaults lost the lease and exited the operator 124 times in 12 days,
// while Flux (35s/30s/5s) rode through the same stalls.
func TestDefaultLeaderElectionTimingsRideThroughStalls(t *testing.T) {
	d := defaultLeaderElectionTimings()
	if err := d.validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	if d.RenewDeadline < 30*time.Second {
		t.Errorf("default renew deadline %s is below 30s; a short API stall would cost the lease", d.RenewDeadline)
	}
}

func TestLeaderElectionTimingsValidate(t *testing.T) {
	const s = time.Second
	cases := []struct {
		name    string
		in      leaderElectionTimings
		wantErr string
	}{
		{"ok", leaderElectionTimings{60 * s, 40 * s, 5 * s}, ""},
		{"lease not above renew", leaderElectionTimings{40 * s, 40 * s, 5 * s}, "--leader-elect-lease-duration"},
		// client-go requires RenewDeadline > JitterFactor(1.2) * RetryPeriod.
		{"renew not above jittered retry", leaderElectionTimings{60 * s, 12 * s, 10 * s}, "--leader-elect-renew-deadline"},
		{"zero retry", leaderElectionTimings{60 * s, 40 * s, 0}, "--leader-elect-retry-period"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error naming %q, got %v", tc.wantErr, err)
			}
		})
	}
}
