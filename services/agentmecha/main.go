// Command agentmecha is the AgentMecha Sidecar on port 9100.
//
// It is a dual-role A2A Server+Client built on the OFFICIAL a2a-go/v2 v2.8.0
// SDK (docs/06 §2). Protocol compliance (JSON-RPC method names, TASK_STATE_*
// enums, AgentCard.SupportedInterfaces, SSE framing) is entirely the SDK's
// job; the self-developed value lives in the executor, the NDJSON adapter,
// the 4 guardrails + Hooks, the StateStore/SessionStore decoupling and the
// compat colon-path rail (contract §0.0).
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	a2a "github.com/a2aproject/a2a-go/v2/a2a"
	a2asrv "github.com/a2aproject/a2a-go/v2/a2asrv"

	shared "local-agent-stack/pkg/a2a"
)

// Server bundles the whole sidecar. The fields are the shared, decoupled
// dependencies; the runtimes map holds one AgentRuntime per agentId.
type Server struct {
	store    *FileStore // also implements taskstore.Store for the SDK
	manager  *TaskManager
	spans    *shared.SpanStore
	runtimes map[string]*AgentRuntime
	defs     map[string]AgentDefinition
	agents   map[string]*agentBundle

	hooks   atomic.Pointer[HookChain]
	engines atomic.Pointer[GuardrailEngine]

	// official SDK handlers, mounted on their own internal muxes
	jsonrpc  map[string]http.Handler
	rest     map[string]http.Handler
	cardHTTP map[string]http.Handler
}

// agentBundle groups the per-agent official SDK objects.
type agentBundle struct {
	card    *a2a.AgentCard
	handler a2asrv.RequestHandler
}

// NewServer assembles the sidecar.
//
// OFFICIAL SDK USAGE (hard requirement #1):
//
//	a2asrv.NewHandler(executor, a2asrv.WithTaskStore(store))
//	a2asrv.NewJSONRPCHandler(handler)
//	a2asrv.NewRESTHandler(handler)
//	a2asrv.NewStaticAgentCardHandler(card)
//
// All four are created here (one set per agentId) and mounted in mountRoutes.
func NewServer(dataDir string) (*Server, error) {
	store, err := NewFileStore(dataDir)
	if err != nil {
		return nil, err
	}
	spans := shared.NewSpanStore(1000)
	manager := NewTaskManager(store, store)
	cfg := DefaultGuardrailConfig()
	engine := NewGuardrailEngine(cfg)
	chain := newHookChain(cfg, engine, spans)

	s := &Server{
		store:    store,
		manager:  manager,
		spans:    spans,
		runtimes: map[string]*AgentRuntime{},
		defs:     map[string]AgentDefinition{},
		agents:   map[string]*agentBundle{},
		jsonrpc:  map[string]http.Handler{},
		rest:     map[string]http.Handler{},
		cardHTTP: map[string]http.Handler{},
	}
	s.hooks.Store(chain)
	s.engines.Store(engine)

	for _, def := range DefaultAgents() {
		s.defs[def.ID] = def
		rt := &AgentRuntime{
			def:     def,
			userSrv: NewUserServerClient(def.UserServerURL, 120*time.Second),
			engine:  engine,
			hooks:   chain,
			spans:   spans,
			tasks:   manager,
			requestID: func() string {
				return shared.NewID("req")
			},
		}
		s.runtimes[def.ID] = rt

		// ---- OFFICIAL: RequestHandler + transport handlers + card handler ----
		exec := NewSidecarExecutor(rt)
		handler := a2asrv.NewHandler(exec,
			// The FileStore (through SDKTaskStore) is passed to the OFFICIAL
			// SDK as its taskstore.Store. This is what makes
			// GetTask/CancelTask/SubscribeToTask survive a process restart
			// (E25) and resume live streams (E24).
			a2asrv.WithTaskStore(NewSDKTaskStore(store)),
			a2asrv.WithExtendedAgentCard(BuildAgentCard(def)),
		)
		card := BuildAgentCard(def)
		bundle := &agentBundle{card: card, handler: handler}
		s.agents[def.ID] = bundle
		s.jsonrpc[def.ID] = a2asrv.NewJSONRPCHandler(handler)
		s.rest[def.ID] = a2asrv.NewRESTHandler(handler)
		s.cardHTTP[def.ID] = a2asrv.NewStaticAgentCardHandler(card)
	}
	return s, nil
}

func (s *Server) hasAgent(id string) bool {
	_, ok := s.defs[id]
	return ok
}

func (s *Server) agentDef(id string) (AgentDefinition, bool) {
	d, ok := s.defs[id]
	return d, ok
}

func (s *Server) runtime(id string) (*AgentRuntime, bool) {
	rt, ok := s.runtimes[id]
	return rt, ok
}

// ---------------------------------------------------------------------------
// Routing
//
// HARD RULE (E12): we NEVER register a pattern with a colon on net/http.ServeMux.
// Exactly ONE prefix handler "a2a/" is registered; it cuts out the agentId and
// rewrites the remaining path before delegating to the official SDK handlers
// (which internally use POST /tasks/{idAndAction} + strings.CutSuffix, the
// official fix for the same Go ServeMux panic — docs/06 §2).
// ---------------------------------------------------------------------------

func (s *Server) mountRoutes() *http.ServeMux {
	mux := http.NewServeMux()

	// ---- Health / admin (no colons) ----
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /admin/hooks", s.handleAdminHooks)
	mux.HandleFunc("POST /admin/hooks/reload", s.handleAdminHooksReload)
	mux.HandleFunc("GET /admin/tasks", s.handleAdminTasks)
	mux.HandleFunc("GET /v1/admin/traces", s.handleAdminSpans)
	mux.HandleFunc("GET /v1/admin/metrics", s.handleAdminMetrics)
	// "/{$}" matches ONLY the root, so it does not conflict with the
	// method-less "/a2a/" prefix handler below (Go 1.22+ ServeMux rules).
	mux.HandleFunc("GET /{$}", s.handleRoot)

	// ---- Discovery: BOTH cards (contract §1.1 + official §0.0) ----
	mux.HandleFunc("GET /.well-known/agent.json", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.URL.Query().Get("agentId")
		if agentID == "" {
			agentID = AgentEcho
		}
		def, ok := s.agentDef(agentID)
		if !ok {
			writeEnvelope(w, shared.CodeAgentNotFound, "unknown agentId "+agentID, "")
			return
		}
		writeJSON(w, http.StatusOK, BuildCompatAgentCard(def))
	})
	// Method-specific registration: an unqualified "/.well-known/agent-card.json"
	// would conflict with "GET /" on Go 1.22+ ServeMux (different method sets).
	mux.Handle("GET "+a2asrv.WellKnownAgentCardPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentID := r.URL.Query().Get("agentId")
		if agentID == "" {
			agentID = AgentEcho
		}
		h, ok := s.cardHTTP[agentID]
		if !ok {
			writeEnvelope(w, shared.CodeAgentNotFound, "unknown agentId "+agentID, "")
			return
		}
		// OFFICIAL card handler serving the *a2a.AgentCard (SupportedInterfaces).
		h.ServeHTTP(w, r)
	}))

	// ---- ONE prefix handler for the whole /a2a/ namespace ----
	// No method prefix: JSON-RPC (POST) and REST (GET/POST/DELETE) both arrive
	// here, and colon sub-paths are resolved INSIDE the handler (E12).
	mux.Handle("/a2a/", http.HandlerFunc(s.handleA2A))
	return mux
}

// handleA2A is the single /a2a/ prefix handler. It:
//  1. cuts out the agentId,
//  2. validates it (AGENT_NOT_FOUND envelope on miss),
//  3. routes compat /v1/* colon paths to colons.go (never ServeMux),
//  4. otherwise rewrites the path and delegates to the OFFICIAL handlers.
func (s *Server) handleA2A(w http.ResponseWriter, r *http.Request) {
	requestID := r.Header.Get("X-Request-Id")
	if requestID == "" {
		requestID = shared.NewID("req")
	}
	agentID, rest, ok := shared.NormalizePath(r.URL.Path)
	if !ok {
		writeEnvelope(w, shared.CodeInvalidRequest, "bad /a2a path", requestID)
		return
	}

	// Top-level compat rail: /a2a/v1/* (agentId defaults to echo-agent).
	// NormalizePath returns agentID="v1" for these paths because "v1" is the
	// first segment after /a2a/. Handle them BEFORE agent validation.
	if agentID == "v1" {
		s.handleCompatTrack(w, r)
		return
	}
	// Per-agent compat rail: /a2a/{agentId}/v1/... (E12 colon paths).
	if strings.HasPrefix(rest, "/v1") {
		s.handleCompatTrack(w, r)
		return
	}
	if !s.hasAgent(agentID) {
		writeEnvelope(w, shared.CodeAgentNotFound,
			"agentId "+agentID+" is not registered", requestID)
		return
	}

	// ---- Official SDK delegation ----
	// The official handlers expect paths relative to the agent root:
	//   JSON-RPC: POST "/" (any path works, it reads the body)
	//   REST:     "/message:send", "/tasks/{id}:cancel", ...
	// We clone the request and rewrite URL.Path (hard requirement #2:
	// "可包装一层重写 path"). No colon pattern is ever registered on ServeMux.
	r2 := r.Clone(r.Context())
	urlCopy := *r.URL
	urlCopy.Path = rest
	if urlCopy.Path == "" {
		urlCopy.Path = "/"
	}
	r2.URL = &urlCopy

	// REST colon paths (message:send / tasks/{id}:cancel) go to the official
	// REST handler; everything else (JSON-RPC POST, GetTask, ...) to JSON-RPC.
	// The official REST handler internally uses "/tasks/{idAndAction}" +
	// strings.CutSuffix(":cancel" | ":subscribe") — NEVER a colon wildcard.
	if isOfficialRESTPath(rest, r.Method) {
		s.rest[agentID].ServeHTTP(w, r2)
		return
	}
	s.jsonrpc[agentID].ServeHTTP(w, r2)
}

// isOfficialRESTPath reports whether the request targets the official
// HTTP+JSON (REST) binding rather than JSON-RPC.
func isOfficialRESTPath(path, method string) bool {
	p := strings.TrimPrefix(path, "/")
	switch {
	case method == http.MethodGet && (p == "extendedAgentCard" ||
		strings.HasPrefix(p, "tasks/")):
		return true
	case method == http.MethodPost && (strings.HasPrefix(p, "message:") ||
		strings.HasPrefix(p, "tasks/")):
		return true
	case method == http.MethodGet && p == "tasks":
		return true
	case method == http.MethodDelete:
		return true
	case method == http.MethodPut:
		return true
	}
	return false
}

func main() {
	dataDir := os.Getenv("MECHA_DATA_DIR")
	if dataDir == "" {
		dataDir = filepath.Join(".", "data", "agentmecha")
	}
	port := os.Getenv("MECHA_PORT")
	if port == "" {
		port = "9100"
	}
	s, err := NewServer(dataDir)
	if err != nil {
		log.Fatalf("agentmecha: init failed: %v", err)
	}
	mux := s.mountRoutes()
	srv := &http.Server{
		Addr:              "127.0.0.1:" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Printf("agentmecha sidecar listening on %s (agents: echo-agent/java-agent/python-agent)", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	log.Println("agentmecha sidecar stopped")
}
