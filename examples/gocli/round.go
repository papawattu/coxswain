// gocli is a tiny sample app for coxswain sample Loops (docs/SAMPLES-PLAN.md).
// The seed state on the in-cluster git server is deliberately broken: Round
// returns the wrong value and round_test.go (a protected path) fails.
package main

import "math"

// Round rounds v to n decimal places. n=0 rounds to the nearest integer.
func Round(v float64, n int) float64 {
	// BUG (seed): truncates instead of rounding.
	scale := math.Pow(10, float64(n))
	return math.Trunc(v * scale) / scale
}
