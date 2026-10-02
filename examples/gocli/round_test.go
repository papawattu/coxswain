package main

import "testing"

// Protected path (verify.protectedPaths, preset go): the acceptance tests are
// the gate to Succeeded; an agent that weakens them trips the tamper gate.

func TestRound(t *testing.T) {
	cases := []struct {
		in     float64
		places int
		want   float64
	}{
		{2.5, 0, 3},
		{2.4, 0, 2},
		{3.14159, 2, 3.14},
		{3.145, 2, 3.15},
		{-1.5, 0, -2},
	}
	for _, c := range cases {
		got := Round(c.in, c.places)
		if got != c.want {
			t.Errorf("Round(%v, %d) = %v, want %v", c.in, c.places, got, c.want)
		}
	}
}

func TestRoundZeroPlaces(t *testing.T) {
	if got := Round(7.9, 0); got != 8 {
		t.Errorf("Round(7.9, 0) = %v, want 8", got)
	}
}
