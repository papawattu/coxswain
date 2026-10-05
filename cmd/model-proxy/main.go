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
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
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
	credDir := ""
	if modelCredFile == "" {
		// Default: the mounted Secret directory (the proxy-standin convention).
		credDir = "/model-creds"
	} else {
		// The file may be a direct file or inside a Secret mount directory.
		// ReadModelCreds reads every regular file under a dir; if MODEL_CRED_FILE
		// points at a file, its parent is the dir. If the parent is the Secret
		// mount (..data is skipped), the key is under a subdir.
		credDir = modelCredFile
		if isDir(credDir) {
			// Treat as a directory to scan.
		} else {
			// A file path: scan its parent (the Secret mount) for the key.
			idx := len(credDir)
			for i := len(credDir) - 1; i >= 0; i-- {
				if credDir[i] == '/' {
					idx = i
					break
				}
			}
			credDir = credDir[:idx]
			if credDir == "" {
				credDir = "/"
			}
		}
	}
	credential, err := proxy.ReadModelCreds(credDir)
	if err != nil {
		// A missing/unreadable model-creds Secret: the proxy CANNOT run (it has
		// no credential) and MUST exit 1 (D33: the agent would be left
		// un-credentialed). The operator recreates the pod on a Secret change.
		log.Fatalf("model-creds Secret not found at %s (exit 1): %v", credDir, err)
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
	auditWriter := bufio.NewWriter(os.Stdout)

	// --- the metering proxy (the agent's 8080 listener) ---
	cfg := proxy.Config{
		Upstream:   upstream,
		Model:      model,
		LoopName:   os.Getenv("LOOP_NAME"),
		Namespace:  os.Getenv("LOOP_NAMESPACE"),
		Credential: credential,
		Meter:      meter,
		Audit:      auditWriter,
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

// isDir reports whether path is a directory (the MODEL_CRED_FILE may be a file
// or a directory to scan for the key).
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
