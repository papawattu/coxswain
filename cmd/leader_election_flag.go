// The operator's leader-election timings, exposed as flags with defaults
// that ride through a multi-second API-server stall.
//
// controller-runtime's defaults (lease 15s, renew deadline 10s, retry 2s)
// give up the lease after ~10s of failed renewals and the manager exits.
// On the homelab cluster (2026-11-20) an etcd member on a slow SATA SSD
// stalled lease reads for several seconds every 20–40 min, and the operator
// exited 124 times in 12 days while Flux (35s/30s/5s) rode the same stalls
// out. The new defaults (60s/40s/5s, cert-manager's lease/renew pair) trade
// a slower takeover after an UNCLEAN leader death (up to the lease duration)
// for not crash-looping on transient API latency. A clean shutdown is
// unaffected: LeaderElectionReleaseOnCancel releases the lease immediately.
package main

import (
	"fmt"
	"time"
)

// leaderJitterFactor mirrors client-go's leaderelection.JitterFactor; the
// elector rejects RenewDeadline <= JitterFactor*RetryPeriod at startup.
const leaderJitterFactor = 1.2

type leaderElectionTimings struct {
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

func defaultLeaderElectionTimings() leaderElectionTimings {
	return leaderElectionTimings{
		LeaseDuration: 60 * time.Second,
		RenewDeadline: 40 * time.Second,
		RetryPeriod:   5 * time.Second,
	}
}

// validate applies client-go's own constraints at flag-parse time, so a bad
// combination is a startup error naming the flag rather than a failure deep
// inside the elector.
func (t leaderElectionTimings) validate() error {
	if t.RetryPeriod <= 0 {
		return fmt.Errorf("--leader-elect-retry-period must be positive (got %s)", t.RetryPeriod)
	}
	if t.LeaseDuration <= t.RenewDeadline {
		return fmt.Errorf("--leader-elect-lease-duration (%s) must be greater than --leader-elect-renew-deadline (%s)",
			t.LeaseDuration, t.RenewDeadline)
	}
	if float64(t.RenewDeadline) <= leaderJitterFactor*float64(t.RetryPeriod) {
		return fmt.Errorf("--leader-elect-renew-deadline (%s) must be greater than %.1f x --leader-elect-retry-period (%s)",
			t.RenewDeadline, leaderJitterFactor, t.RetryPeriod)
	}
	return nil
}
