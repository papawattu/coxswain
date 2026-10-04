// tool-proxy is the D41a tool proxy (ADR-0008): the generic HTTP tool proxy.
// It is a reverse proxy: the agent speaks plain HTTP, origin-form, in-cluster
// to the proxy; the proxy rule-checks, strips agent-supplied auth, injects
// the credential, and originates its own request (TLS, verified) to a single
// fixed upstream. CONNECT and absolute-form URIs are 405'd without a dial.
//
// It does NOT terminate TLS, inspect the payload, or MITM the upstream
// (never decrypt). It holds one secret (the mounted credential file), no SA
// token; read-only rootfs, UID 65535.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/papawattu/coxswain/internal/toolproxy"
)

func main() {
	cfg := toolproxy.Config{
		ToolName:     os.Getenv("TOOL_NAME"),
		UpstreamBase: os.Getenv("TOOL_UPSTREAM"),
		Rules:        toolproxy.ParseRules(os.Getenv("TOOL_RULES_JSON")),
		LoopName:     os.Getenv("LOOP_NAME"),
		Namespace:    os.Getenv("LOOP_NAMESPACE"),
		PolicyHash:   os.Getenv("TOOL_POLICY_HASH"),
		Resolver:     net.DefaultResolver,
	}
	// Operator-configured pod/service CIDRs (kind/k3s exposes these via
	// --pod-network-cidr / --service-cluster-ip-range). Comma-separated; the
	// proxy carves them out of the dialable range alongside the standard
	// private/link-local ranges (the I42a resolved-IP backstop).
	if raw := strings.TrimSpace(os.Getenv("POD_CIDR")); raw != "" {
		cfg.ExtraCIDRs = append(cfg.ExtraCIDRs, raw)
	}
	if raw := strings.TrimSpace(os.Getenv("SERVICE_CIDR")); raw != "" {
		cfg.ExtraCIDRs = append(cfg.ExtraCIDRs, raw)
	}
	// The credential file (a mounted Secret key); optional. Read once at
	// startup: the pod is long-lived and immutable, and the operator rotates
	// by pod replacement.
	if path := strings.TrimSpace(os.Getenv("TOOL_CREDENTIAL_FILE")); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			log.Fatalf("tool-proxy: read credential file %s: %v", path, err)
		}
		cfg.Credential = strings.TrimRight(string(data), "\n")
	}

	proxy, err := toolproxy.New(cfg)
	if err != nil {
		log.Fatalf("tool-proxy: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	port := envOr("TOOL_PROXY_PORT", "8080")
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatalf("tool-proxy: listen :%s: %v", port, err)
	}
	log.Printf("tool-proxy: listening on :%s, tool=%s upstream=%s rules=%d policy=%s\n",
		port, proxy.ToolName, proxy.UpstreamBase, len(proxy.Rules), proxy.PolicyHash)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	srv := &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Fatalf("tool-proxy: serve: %v", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
