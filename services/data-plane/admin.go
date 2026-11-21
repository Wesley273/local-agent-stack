package main

import (
	"net/http"
	"strconv"

	"local-agent-stack/pkg/a2a"
)

// ---------------------------------------------------------------------------
// Local (ungoverned) admin + health endpoints (contract §0.4, §1.8, §3.3,
// §3.5). These are registered directly on the mux in main.go and bypass the
// filter chain.
// ---------------------------------------------------------------------------

// healthHandler implements GET /health (contract §0.4).
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "UP", "service": "data-plane", "version": s.Version,
		"revision": s.Cfg.Revision(), "now": a2a.Now(),
	})
}

// tracesHandler implements GET /v1/admin/traces?limit=100 (contract §1.8).
func (s *Server) tracesHandler(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	spans := s.Spans.List(limit)
	// Defensive: http.status_code must be an integer and durationMs never null
	// (a2a.SpanStore.Add already coerces; assert again for E23).
	for i := range spans {
		if v, ok := spans[i].Attributes["http.status_code"]; ok {
			switch n := v.(type) {
			case int:
				_ = n
			case float64:
				spans[i].Attributes["http.status_code"] = int(n)
			default:
				spans[i].Attributes["http.status_code"] = 0
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"spans": spans, "count": len(spans)})
}

// metricsHandler implements GET /v1/admin/metrics (contract §1.8 / A6).
func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a2a.Metrics{Counters: s.Runtime.Counters()})
}

// poolHandler implements GET /v1/admin/pool (contract §3.3 / E22).
func (s *Server) poolHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Chain.PoolStats())
}

// configHandler implements GET /v1/admin/config — returns the脱敏 snapshot
// (§3.5). It NEVER contains API-key plaintext.
func (s *Server) configHandler(w http.ResponseWriter, r *http.Request) {
	snap := s.Cfg.Current()
	writeJSON(w, http.StatusOK, map[string]any{
		"revision": snap.Revision, "generatedAt": snap.GeneratedAt,
		"snapshot": snap.Sanitize(),
		"poller": map[string]any{
			"cpURL": s.CPBase, "intervalMs": 5000,
			"lastError": errString(s.Poller.LastError()),
		},
	})
}

// reloadHandler implements POST /v1/admin/reload — forces an immediate fetch
// of {CP}/v1/config (§3.5).
func (s *Server) reloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed,
			a2a.NewError(a2a.CodeInvalidRequest, "POST required", ""))
		return
	}
	rev, err := s.Poller.ForceFetch(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"reloaded": false, "revision": rev, "error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reloaded": true, "revision": rev})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
