/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"errors"
	"strconv"
	"time"
)

// DefaultStallAfter is the stall detector's default consecutive-failure
// threshold (PLAN.md): a nil or zero spec.loop.stallAfter reads as 3.
const DefaultStallAfter int32 = 3

// StallConfig is the operator's effective stall-detector configuration
// (P2c): the CRD defaults applied on top of spec.loop.stall*. It is the P2d/
// P2e seam — the decision slices read these values, never the raw spec.
type StallConfig struct {
	// StallAfter is the consecutive-failure threshold (nil/zero spec reads
	// as DefaultStallAfter = 3).
	StallAfter int32
	// StallAction is the action on a stall fire (spec.loop.stallAction;
	// the CRD defaults it to Fail).
	StallAction StallAction
}

// EffectiveStallConfig applies the stall defaults (the P2d/P2e consumer
// seam, P2c): a nil or zero spec.loop.stallAfter reads as 3, and an empty
// spec.loop.stallAction reads as Fail.
func EffectiveStallConfig(s *LoopSettings) StallConfig {
	cfg := StallConfig{StallAction: StallActionFail}
	if s != nil {
		if s.StallAfter != nil && *s.StallAfter > 0 {
			cfg.StallAfter = *s.StallAfter
		} else {
			cfg.StallAfter = DefaultStallAfter
		}
		if s.StallAction != "" {
			cfg.StallAction = s.StallAction
		}
	}
	return cfg
}

// BudgetConfigEffective is the operator's effective budget configuration
// (P2c): the spec budget with the onExceeded default applied. A nil spec
// budget means no budget caps (Budget nil).
type BudgetConfigEffective struct {
	// Budget is the effective caps + action; nil means no budget caps.
	Budget *BudgetConfig
	// OnExceeded is the effective onExceeded (the spec value, or Pause when
	// the spec omits it). Only meaningful when Budget is non-nil.
	OnExceeded BudgetExceededAction
}

// EffectiveBudget applies the budget default (the P2d consumer seam, P2c):
// onExceeded defaults to Pause. A nil spec.budget stays nil (no caps).
func EffectiveBudget(b *BudgetConfig) BudgetConfigEffective {
	if b == nil {
		return BudgetConfigEffective{}
	}
	eff := *b
	if eff.OnExceeded == "" {
		eff.OnExceeded = BudgetExceededActionPause
	}
	return BudgetConfigEffective{Budget: &eff, OnExceeded: eff.OnExceeded}
}

// ParseMaxWallClock parses spec.budget.maxWallClock (a Go duration string).
// It returns an error when the value is absent, malformed, or under 1s
// (the plan's "≥1s when set" — a sub-second wall-clock cap would fire on
// the first reconcile).
func ParseMaxWallClock(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < time.Second {
		return 0, errWallClockTooSmall
	}
	return d, nil
}

// errWallClockTooSmall is returned by ParseMaxWallClock for sub-second caps
// (the plan's "≥1s when set": a sub-second wall-clock cap would fire on the
// first reconcile).
var errWallClockTooSmall = errors.New("maxWallClock must be at least 1s")

// ParseDecimalString validates and parses a decimal string (the CEL pattern
// ^\d+(\.\d+)?$ already guarantees non-negative decimal form).
func ParseDecimalString(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseFloat(s, 64)
}
