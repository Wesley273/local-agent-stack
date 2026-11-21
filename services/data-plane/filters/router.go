package filters

import (
	"net/url"
	"strings"

	"local-agent-stack/pkg/a2a"
)

// RouterFilter is filter #4 (DecodeHeaders). It selects the downstream purely
// from the HTTP path — it NEVER parses A2A protocol semantics (contract §0.0:
// the data plane is a governance layer + transparent reverse proxy).
//
// Routing table (contract §5 / §1.5 / §2):
//
//	/a2a/v1/*                         -> served locally by compat.go (E12)
//	/a2a/{agentId}[/*]               -> agent url from snapshot, else Sidecar 9100
//	/v1/chat/completions             -> model gateway "chat"     -> LLM 9200
//	/v1/embeddings                   -> model gateway "embeddings"-> LLM 9200
//	/v1/models                       -> model gateway "models" (local)
//	/mcp , /mcp/*                    -> MCP 9300 (+ X-Proxied-By)
//
// Errors: AGENT_NOT_FOUND (404). MODEL_NOT_FOUND is produced by modelgw when
// the requested model name resolves to no enabled backend.
type RouterFilter struct {
	a2a.BaseFilter
	rt *Runtime

	// Defaults come from env at construction; the snapshot may override the
	// per-agent url.
	SidecarBase string
	LLMBase     string
	MCPBase     string
	LLMPath     string
	MCPPath     string
}

// Name identifies the filter.
func (f *RouterFilter) Name() string { return "router" }

// DecodeHeaders implements route selection.
func (f *RouterFilter) DecodeHeaders(h *a2a.HeaderMap, endStream bool) a2a.FilterStatus {
	ctx := f.rt.Active()
	path := h.Get(HdrPath)
	if path == "" && ctx != nil {
		path = ctx.Path
	}
	snap := f.rt.Config.Current()

	switch {
	case path == "/v1/chat/completions":
		h.Set(HdrModelEP, "chat")
		h.Set(HdrUpstream, f.LLMBase)

	case path == "/v1/embeddings":
		h.Set(HdrModelEP, "embeddings")
		h.Set(HdrUpstream, f.LLMBase)

	case path == "/v1/models":
		h.Set(HdrModelEP, "models")
		h.Set(HdrLocal, "models")
		h.Set(HdrUpstream, f.LLMBase)

	case path == "/a2a/v1" || strings.HasPrefix(path, "/a2a/v1/"):
		// Compat track (§0.0 / §1.5). Served locally, never proxied. The
		// colon paths are NEVER registered on http.ServeMux.
		h.Set(HdrLocal, "compat")
		h.Set(HdrAgentID, "") // wildcard grant applies
		if ctx != nil {
			ctx.AgentID = ""
		}

	case path == "/mcp" || strings.HasPrefix(path, "/mcp/"):
		h.Set(HdrUpstream, f.MCPBase)
		h.Set(HdrBackend, "mcp")

	case path == "/a2a" || strings.HasPrefix(path, "/a2a/"):
		agentID := agentFromPath(path)
		if agentID == "" {
			// Bare /a2a without an agent id still proxies to the Sidecar.
			h.Set(HdrUpstream, f.SidecarBase)
			h.Set(HdrBackend, "sidecar")
			return a2a.Continue
		}
		h.Set(HdrAgentID, agentID)
		if ctx != nil {
			ctx.AgentID = agentID
		}
		// AGENT_NOT_FOUND: not registered or disabled (§0.3 / §1.5).
		a, ok := snap.Agents[agentID]
		if !ok || !a.Enabled {
			h.Set(HdrFilterCode, string(a2a.CodeAgentNotFound))
			h.Set(HdrFilterMsg, "agentId not registered or disabled: "+agentID)
			return a2a.StopAndReply
		}
		target := a.URL
		if target == "" {
			target = f.SidecarBase + "/a2a/" + agentID
		}
		h.Set(HdrUpstream, target)
		h.Set(HdrBackend, "sidecar")

	default:
		// Unknown paths still get governance, then a transparent proxy to the
		// Sidecar base (keeps the gateway useful for SDK /a2a/{id} variants).
		h.Set(HdrUpstream, f.SidecarBase)
		h.Set(HdrBackend, "sidecar")
	}

	// Host rewrite target (contract §0.2 "统一地址转换"). TransformFilter uses
	// this to set the downstream :authority/Host.
	if u := h.Get(HdrUpstream); u != "" {
		if parsed, err := url.Parse(u); err == nil && parsed.Host != "" {
			h.Set(HdrHost, parsed.Host)
		}
	}
	if h.Get(HdrBackend) == "" {
		if ep := h.Get(HdrModelEP); ep != "" {
			h.Set(HdrBackend, "llm")
		} else if h.Get(HdrLocal) == "compat" {
			h.Set(HdrBackend, "compat")
		}
	}
	return a2a.Continue
}

// agentFromPath extracts the first path segment of /a2a/{agentId}/...
func agentFromPath(p string) string {
	rest := strings.TrimPrefix(p, "/a2a")
	rest = strings.TrimPrefix(rest, "/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// Reset clears per-request state.
func (f *RouterFilter) Reset() {}
