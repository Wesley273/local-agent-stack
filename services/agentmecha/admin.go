package main

import (
	"net/http"
	"runtime"
	"strconv"
	"time"

	shared "local-agent-stack/pkg/a2a"
)

// admin.go — /admin/hooks, /admin/hooks/reload, /admin/tasks, /health,
// plus the span store exposure (contract §1.8, numeric fields non-null).

// healthResponse is the §0.4 health shape.
type healthResponse struct {
	Status   string `json:"status"`
	Service  string `json:"service"`
	Version  string `json:"version"`
	Revision int    `json:"revision"`
	Now      string `json:"now"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{
		Status: "UP", Service: "agentmecha-sidecar", Version: "1.0.0",
		Revision: 1, Now: shared.Now(),
	})
}

// handleAdminHooks lists registered hooks (contract §6.2) with their points
// and priorities. The chain is read from an atomic pointer so /reload can
// swap it without a data race.
func (s *Server) handleAdminHooks(w http.ResponseWriter, _ *http.Request) {
	chain := s.hooks.Load()
	list := []map[string]any{}
	if chain != nil {
		list = chain.List()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hooks": list, "count": len(list),
		"points": []string{
			string(BeforeTask), string(BeforeTool), string(BeforeModel),
			string(AfterModel), string(AfterTask),
		},
	})
}

// handleAdminHooksReload re-arms the hook chain from the current config
// (contract §6.2: POST /admin/hooks/reload 重新装载配置).
func (s *Server) handleAdminHooksReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeEnvelope(w, shared.CodeInvalidRequest, "POST required", "")
		return
	}
	cfg := DefaultGuardrailConfig()
	engine := NewGuardrailEngine(cfg)
	chain := newHookChain(cfg, engine, s.spans)
	s.hooks.Store(chain)
	s.engines.Store(engine)
	for _, rt := range s.runtimes {
		rt.hooks = chain
		rt.engine = engine
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "reloaded", "hooks": chain.List(), "now": shared.Now(),
	})
}

// handleAdminTasks lists tasks from the StateStore index (E25 visibility).
func (s *Server) handleAdminTasks(w http.ResponseWriter, _ *http.Request) {
	ids := s.store.AllTaskIDs()
	type item struct {
		ID      string `json:"id"`
		State   string `json:"state"`
		Version int64  `json:"version"`
		AgentID string `json:"agentId,omitempty"`
	}
	out := make([]item, 0, len(ids))
	for _, id := range ids {
		t, ok := s.store.GetTask(id)
		if !ok || t == nil {
			continue
		}
		agentID := ""
		if v, ok := t.Metadata["agentId"].(string); ok {
			agentID = v
		}
		out = append(out, item{
			ID: id, State: string(t.Status.State), Version: s.store.VersionOf(id),
			AgentID: agentID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": out, "count": len(out)})
}

// handleAdminSpans exposes the bounded SpanStore (contract §1.8). All numeric
// fields are non-pointer ints so they can NEVER be null.
func (s *Server) handleAdminSpans(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"spans": s.spans.List(limit)})
}

// handleAdminMetrics exposes span counters (§1.8).
func (s *Server) handleAdminMetrics(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, shared.Metrics{Counters: s.spans.Counters()})
}

// handleRoot is a tiny index of available endpoints (operational convenience).
func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "agentmecha-sidecar",
		"version": "1.0.0",
		"go":      runtime.Version(),
		"time":    time.Now().Format(time.RFC3339),
		"endpoints": []string{
			"GET /health",
			"GET /.well-known/agent.json",
			"GET /.well-known/agent-card.json",
			"POST /a2a/{agentId}            (official JSON-RPC, PascalCase methods)",
			"POST /a2a/{agentId}/v1/message:send       (compat colon path)",
			"POST /a2a/{agentId}/v1/tasks/{id}:cancel   (compat colon path)",
			"GET  /a2a/{agentId}/v1/tasks/{id}:subscribe (compat SSE)",
			"GET /admin/hooks", "POST /admin/hooks/reload",
			"GET /admin/tasks", "GET /v1/admin/traces", "GET /v1/admin/metrics",
		},
		"agents": []string{AgentEcho, AgentJava, AgentPython},
	})
}
