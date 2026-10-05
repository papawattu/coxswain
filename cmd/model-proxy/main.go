// Command model-proxy is the metering model proxy (P2b, ADR-0009). It is the
// D33/D34 model proxy (the agent's model calls traverse it: origin-form plain
// HTTP in-cluster, single upstream, the credential injected from the mounted
// model-creds Secret, CONNECT / absolute-form 405'd) PLUS per-Loop cumulative
// usage metering and the operator-only GET /coxswain/usage endpoint (9090).
//
// Env:
//
//	MODEL_ENDPOINT      (required) the upstream model endpoint.
//	MODEL_CRED_FILE     (required) the mounted model-creds Secret key file; the
//	                    credential is read from it at startup (exit 1 if no
//	                    readable key file). The file is NOT read at startup as
//	                    part of any other path — it is the proxy's credential.
//	PROXY_PORT          (optional, default 8080) the model proxy port the agent
//	                    dials (COX_MODEL_BASE_URL points here).
//	PROXY_PORT_USAGE    (optional, default 9090) the usage endpoint port (the
//	                    operator reads it; the <loop>-proxy netpol allows the
//	                    operator namespace / controller-manager on this port,
//	                    never the agent).
//	PROXY_USAGE_FILE    (optional) the path to the cumulative usage counter
//	                    file (an emptyDir). A new boot (file absent) generates a
//	                    bootID; a restart reuses the file's bootID + counters.
//	LOOP_NAME           (optional) the Loop name (reported in the usage
//	                    reading + the audit line).
//	LOOP_NAMESPACE      (optional) the Loop namespace.
//
// The proxy dials exactly once per request (no retry), strips the agent's
// Accept-Encoding (steering-proof: a gzip body cannot hide the usage), forces
// stream_options.include_usage on streaming requests, and meters the usage
// object (or counts it unmetered). The audit line (stdout) is observability
// only — not the gate's input. The credential is never logged.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/papawattu/coxswain/internal/proxy"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envPort(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func main() {
	// --- startup check: the model endpoint + the model-creds Secret file ---
	modelEndpoint := os.Getenv("MODEL_ENDPOINT")
	if modelEndpoint == "" {
		log.Fatal("MODEL_ENDPOINT not set; the model proxy has no upstream")
	}
	upstream, err := url.Parse(modelEndpoint)
	if err != nil || upstream.Host == "" {
		log.Fatalf("MODEL_ENDPOINT %q is not a valid URL (must be http:// or https://): %v", modelEndpoint, err)
	}

	modelCredFile := os.Getenv("MODEL_CRED_FILE")
	var credential string
	var credErr error
	if modelCredFile != "" {
		// The operator sets MODEL_CRED_FILE to the key file in the mounted
		// model-creds Secret (e.g. /model-creds/.data/model-key — the Kubernetes
		// atomic Secret-mount dir, per the D33 convention). Depending on the
		// kubelet/containerd version, the Secret is mounted as a plain dir with
		// the keys at /model-creds/<key> (the ..data symlink is the atomic-mount
		// mechanism, sometimes materialized, sometimes not). Resolve the key
		// flexibly: the exact path first, then the key name in the Secret mount
		// dir. The credential is never part of any other path.
		credBytes, err := os.ReadFile(modelCredFile)
		if err != nil {
			// Fallback: the key name in the Secret mount dir (the ..data dir is
			// the atomic mount; the key sits next to it, or in it, per the
			// kubelet version).
			key := path.Base(modelCredFile)
			for _, candidate := range []string{
				path.Join(path.Dir(modelCredFile), "..", key), // the ..data sibling
				"/model-creds/" + key,                         // the plain Secret mount
				path.Dir(modelCredFile) + "/" + key,           // the ..data dir itself
			} {
				if b, e := os.ReadFile(candidate); e == nil && len(b) > 0 {
					credBytes, err = b, nil
					break
				}
			}
		}
		if err == nil && len(credBytes) > 0 {
			credential = strings.TrimRight(string(credBytes), "\n")
		} else {
			credErr = err
			if credential == "" && err == nil {
				credErr = errString("model-creds key file is empty")
			}
		}
	} else {
		// Default: the mounted Secret directory (the proxy-standin convention).
		credential, credErr = proxy.ReadModelCreds("/model-creds")
	}
	if credErr != nil {
		// A missing/unreadable model-creds Secret: the proxy CANNOT run (it has
		// no credential) and MUST exit 1 (D33: the agent would be left
		// un-credentialed). The operator recreates the pod on a Secret change.
		log.Fatalf("model-creds Secret not found at %s (exit 1): %v", modelCredFile, credErr)
	}

	// --- the meter (the cumulative counters + bootID, persisted atomically) ---
	usageFile := envOr("PROXY_USAGE_FILE", "/var/lib/proxy-usage/usage.json")
	since := time.Now().UTC()
	model := modelEndpoint // the model is identified by the upstream endpoint
	meter, err := proxy.NewMeter(usageFile, model, since)
	if err != nil {
		log.Fatalf("init meter at %s: %v", usageFile, err)
	}

	// --- the audit channel (stdout; observability only) ---
	// os.Stdout directly (unbuffered): a bufio.NewWriter(os.Stdout) that is
	// never flushed loses the audit lines (the proxy pod's log would show only
	// the log.Printf startup lines, not the audit JSON). The audit line is
	// written per-request by the metering RoundTripper (proxy.go: audit).

	// --- the metering proxy (the agent's 8080 listener) ---
	cfg := proxy.Config{
		Upstream:   upstream,
		Model:      model,
		LoopName:   os.Getenv("LOOP_NAME"),
		Namespace:  os.Getenv("LOOP_NAMESPACE"),
		Credential: credential,
		Meter:      meter,
		Audit:      os.Stdout, // unbuffered (a bufio.NewWriter never flushed loses the audit lines)
		Since:      since,
	}
	proxyInst, err := proxy.NewMetered(cfg)
	if err != nil {
		log.Fatalf("build metering proxy: %v", err)
	}

	proxyPort := envPort("PROXY_PORT", 8080)
	usagePort := envPort("PROXY_PORT_USAGE", 9090)

	// --- the two listeners ---
	proxySrv := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", proxyPort),
		Handler: proxyInst.Handler(),
	}
	usageSrv := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", usagePort),
		Handler: proxyInst.UsageHandler(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	go func() {
		log.Printf("metering model proxy listening on 0.0.0.0:%d (upstream %s, usage on :%d)", proxyPort, upstream, usagePort)
		errCh <- proxySrv.ListenAndServe()
	}()
	go func() {
		log.Printf("usage endpoint listening on 0.0.0.0:%d", usagePort)
		errCh <- usageSrv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Printf("shutdown signal; closing the proxy + usage listeners")
	case err := <-errCh:
		if err != nil {
			log.Fatalf("server error: %v", err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = proxySrv.Shutdown(shutdownCtx)
	_ = usageSrv.Shutdown(shutdownCtx)
}

// errString is a tiny error type so the metering proxy's cred-read error is
// distinct from a sentinel (the log.Fatalf message names the path it tried).
type errString string

func (e errString) Error() string { return string(e) }
