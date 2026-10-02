package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
)

// main implements the tiny sample web API (docs/SAMPLES-PLAN.md,
// examples/webapi). Seed state: /healthz works, /api/v1/ping is missing
// (task 1), /api/v1/echo is missing (task 2).
func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if p, err := strconv.Atoi(port); err != nil || p <= 0 {
		fmt.Fprintln(os.Stderr, "PORT must be a positive integer, got", port)
		os.Exit(2)
	}

	fmt.Printf("webapi listening on :%s\n", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}
