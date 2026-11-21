// Command data-plane is the AI gateway data plane (contract §0.0 / §3).
//
// It is a governance layer + transparent reverse proxy. It NEVER parses A2A
// protocol semantics: /a2a/* is forwarded verbatim to the Sidecar, /v1/* to
// the model gateway / Mock LLM, /mcp/* to the MCP registry.
//
// Env (with defaults from config/ports.env):
//
//	DP_PORT=8080 CP_PORT=9000 MECHA_PORT=9100 LLM_PORT=9200 MCP_PORT=9300
//	DP_HOST / CP_HOST / MECHA_HOST / LLM_HOST / MCP_HOST = 127.0.0.1
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"local-agent-stack/services/data-plane/config"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func baseURL(hostKey, portKey, defPort string) string {
	host := env(hostKey, "127.0.0.1")
	port := env(portKey, defPort)
	return "http://" + host + ":" + port
}

func main() {
	// UTF-8 console (中文 Windows 控制台乱码对策).
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	dpHost := env("DP_HOST", "127.0.0.1")
	dpPort := env("DP_PORT", "8080")
	cp := baseURL("CP_HOST", "CP_PORT", "9000")
	sidecar := baseURL("MECHA_HOST", "MECHA_PORT", "9100")
	llm := baseURL("LLM_HOST", "LLM_PORT", "9200")
	mcp := baseURL("MCP_HOST", "MCP_PORT", "9300")

	cfgStore := config.NewStore(nil) // compiled-in bootstrap fallback (§3.5)
	poller := config.NewPoller(cp, cfgStore, 5*time.Second)
	srv := NewServer(cfgStore, poller, sidecar, llm, mcp, cp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Initial full snapshot fetch, then 5s polling with If-None-Match.
	if _, err := poller.FetchOnce(ctx); err != nil {
		log.Printf("[startup] WARN control plane unreachable (%v); using bootstrap snapshot", err)
	}
	go poller.Start(ctx)

	mux := http.NewServeMux()

	// --- local, ungoverned endpoints ---
	mux.HandleFunc("/health", srv.healthHandler)
	mux.HandleFunc("/v1/admin/traces", srv.tracesHandler)
	mux.HandleFunc("/v1/admin/metrics", srv.metricsHandler)
	mux.HandleFunc("/v1/admin/pool", srv.poolHandler)
	mux.HandleFunc("/v1/admin/config", srv.configHandler)
	mux.HandleFunc("/v1/admin/reload", srv.reloadHandler)

	// --- colon-path compat track (§1.5 / E12) ---
	// CRITICAL: register ONLY the "/a2a/v1/" PREFIX. Registering
	// "/tasks/{id}:cancel" directly would panic ("bad wildcard segment").
	mux.HandleFunc("/a2a/v1/", srv.ServeHTTP)
	mux.HandleFunc("/a2a/v1", srv.ServeHTTP)

	// --- everything else goes through the filter chain ---
	mux.HandleFunc("/", srv.ServeHTTP)

	httpServer := &http.Server{
		Addr:              dpHost + ":" + dpPort,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Port-in-use pre-check with a clear message (the E08 "8080 占用" story).
	go func() {
		log.Printf("[startup] data-plane listening on %s (CP=%s sidecar=%s llm=%s mcp=%s)",
			httpServer.Addr, cp, sidecar, llm, mcp)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[startup] FATAL listen %s: %v (port already in use?)", httpServer.Addr, err)
		}
	}()

	// Graceful shutdown on SIGINT / SIGTERM.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Printf("[shutdown] signal received; draining ...")
	cancel()
	shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("[shutdown] WARN %v", err)
	}
	srv.Gateway.Close()
	log.Printf("[shutdown] done")
}
