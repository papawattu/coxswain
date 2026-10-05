// probe is a minimal static connect-probe used by the D41e kind e2e's
// assertion 4c live blocked-connect check.
//
// The tool proxy's KubeArmorPolicy process allowlist permits ONLY
// /usr/local/bin/tool-proxy. A labeled stand-in "fence" pod (carrying the
// tool proxy's labels, so the netpol's podSelector matches) can therefore run
// only that binary. The probe is built to that exact path so the allowlist
// permits it, letting the fence pod perform a real network-layer connect test
// from the tool proxy's network position.
//
// Usage:
//
//	/usr/local/bin/tool-proxy <host> <port>
//
// It dials host:port with a short timeout (net.DialTimeout, TCP). On success
// it prints "connected" and exits 0. On failure it prints the error and exits
// 1. This is a pure connect (no payload, no HTTP) — it proves the network
// layer permits or refuses the connection, which is exactly what the netpol
// governs.
package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: tool-proxy <host> <port>")
		os.Exit(2)
	}
	host := os.Args[1]
	port := os.Args[2]
	d := net.Dialer{Timeout: 4 * time.Second}
	conn, err := d.Dial("tcp", net.JoinHostPort(host, port))
	if err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
	// The dial is the point; a close error after a successful dial is not a
	// connection failure. Checked to satisfy errcheck.
	if cerr := conn.Close(); cerr != nil {
		fmt.Fprintf(os.Stderr, "close: %v\n", cerr)
	}
	fmt.Println("connected")
}
