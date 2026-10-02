package main

import (
	"flag"
	"fmt"
	"os"
)

// main implements the tiny round CLI (docs/SAMPLES-PLAN.md, examples/gocli).
// The seed state only breaks Round (round.go); the CLI plumbing works.
// Task 2 (tasks.md) adds the --json output flag (tasks/2.patch).
func main() {
	value := flag.Float64("value", 0, "the value to round")
	places := flag.Int("places", 0, "decimal places to round to")
	flag.Parse()

	if *value == 0 && *places == 0 {
		fmt.Fprintln(os.Stderr, "usage: gocli -value <v> [-places <n>]")
		os.Exit(2)
	}

	fmt.Printf("%g\n", Round(*value, *places))
}
