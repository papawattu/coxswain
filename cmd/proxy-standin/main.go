// proxy-standin is the D33/D34 stand-in model proxy. It checks the
// model-creds Secret at startup (D33 acceptance) and forwards HTTP
// requests to MODEL_ENDPOINT. It is a single Go binary with no
// dependencies, built into a distroless image.
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
)

func main() {
	// D33 acceptance: prove the proxy can read a REAL key file from the
	// model-creds Secret. Read every entry under /model-creds that is a
	// regular file (skip the ..data symlink directory that Kubernetes
	// uses for atomic Secret updates). Exit 1 if no readable file exists.
	found := false
	if entries, err := os.ReadDir("/model-creds"); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			path := "/model-creds/" + e.Name()
			f, err := os.Open(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "proxy: cannot open %s: %v\n", path, err)
				continue
			}
			buf, _ := io.ReadAll(io.LimitReader(f, 64))
			if err := f.Close(); err != nil {
				log.Printf("proxy: cannot close %s: %v", path, err)
			}
			if len(buf) > 0 {
				msg := fmt.Sprintf("proxy: model-creds readable (%s, %d bytes)\n", e.Name(), len(buf))
				if _, err := fmt.Fprint(os.Stdout, msg); err != nil {
					log.Printf("proxy: cannot write to stdout: %v", err)
				}
				found = true
			}
		}
	}
	if !found {
		log.Fatal("proxy: no readable model-creds Secret file found")
	}

	// Require MODEL_ENDPOINT — a Loop with endpointSecretRef but no
	// modelEndpoint is rejected at admission (CEL), so this should not
	// be empty. If it is, fail loudly rather than crash-loop silently.
	target := os.Getenv("MODEL_ENDPOINT")
	if target == "" {
		log.Fatal("MODEL_ENDPOINT not set")
	}
	u, err := url.Parse(target)
	if err != nil {
		log.Fatalf("bad MODEL_ENDPOINT %q: %v", target, err)
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	if _, err := fmt.Fprintf(os.Stdout, "proxy-stand-in: forwarding to %s\n", target); err != nil {
		log.Printf("proxy: cannot write to stdout: %v", err)
	}
	log.Fatal(http.ListenAndServe(":8080", proxy))
}
