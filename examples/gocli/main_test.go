//go:build task2

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMainJSON covers the task 2 -json flag: the rounded result is emitted as
// a single-line JSON object with the input, places and rounded fields.
//
// The test is behind the `task2` build tag so it does not break `go test`
// for task 1 (which runs without the tag). It is a protected path: the seed
// carries the test (red — roundJSON is not defined until task 2's patch adds
// json.go); task 2 makes it pass without touching it.
func TestMainJSON(t *testing.T) {
	got, err := roundJSON(3.145, 2)
	if err != nil {
		t.Fatalf("roundJSON: %v", err)
	}
	var obj struct {
		Value   float64 `json:"value"`
		Places  int     `json:"places"`
		Rounded float64 `json:"rounded"`
	}
	if err := json.Unmarshal([]byte(got), &obj); err != nil {
		t.Fatalf("output %q is not valid JSON: %v", got, err)
	}
	// Round(3.145, 2) in the seed truncates to 3.14; the -json flag must
	// faithfully emit that value, plus the input and the requested places.
	if obj.Value != 3.145 || obj.Places != 2 || obj.Rounded != 3.14 {
		t.Errorf("json object = %+v, want value=3.145 places=2 rounded=3.14", obj)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("json output is not single-line: %q", got)
	}
}
