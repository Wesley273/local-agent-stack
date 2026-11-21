package filters

import (
	"local-agent-stack/pkg/a2a"
)

// AuthFilter is filter #1 (DecodeHeaders). It resolves X-API-Key → appId via
// the control-plane snapshot's policies, then checks grants for the target
// agent (contract §0.2 / §0.3 / §3.2).
//
// Errors: AUTH_REQUIRED (401) / FORBIDDEN (403). These are produced BEFORE the
// response starts, so StopAndReply is legal here.
type AuthFilter struct {
	a2a.BaseFilter
	rt *Runtime
}

// Name identifies the filter.
func (f *AuthFilter) Name() string { return "auth" }

// DecodeHeaders implements the auth phase.
func (f *AuthFilter) DecodeHeaders(h *a2a.HeaderMap, endStream bool) a2a.FilterStatus {
	ctx := f.rt.Active()
	snap := f.rt.Config.Current()

	// A2A version is fixed to 1.0; a missing value is treated as 1.0 (§0.2).
	if h.Get(HdrA2AVersion) == "" {
		h.Set(HdrA2AVersion, "1.0")
	}

	key := h.Get(HdrAPIKey)
	appID, ok := snap.AppForKey(key)
	if !ok {
		// Allow anonymous only if the policy explicitly permits it.
		if p, exists := snap.Policies["default"]; exists && p.AllowAnonymous && key == "" {
			appID = "anonymous"
			h.Set(HdrAppID, appID)
			return a2a.Continue
		}
		h.Set(HdrFilterCode, string(a2a.CodeAuthRequired))
		h.Set(HdrFilterMsg, "missing or invalid X-API-Key")
		return a2a.StopAndReply
	}
	h.Set(HdrAppID, appID)
	if ctx != nil {
		ctx.AppID = appID
		f.rt.reqTotal.Add(1)
	}

	// Grants check: the agentId is extracted from the path by the server and
	// passed via X-DP-Agent-Id. Empty (e.g. /v1/* model or /mcp/*) means the
	// wildcard grant applies.
	agentID := h.Get(HdrAgentID)
	if !snap.Allowed(appID, agentID) {
		h.Set(HdrFilterCode, string(a2a.CodeForbidden))
		h.Set(HdrFilterMsg, "app is not granted access to this agent/model")
		return a2a.StopAndReply
	}
	return a2a.Continue
}

// Reset clears per-request state before the instance returns to the pool.
func (f *AuthFilter) Reset() {}
