package filters

import (
	"encoding/json"

	"local-agent-stack/pkg/a2a"
)

// TransformFilter is filter #5. It handles header/body transformation:
//
//	DecodeHeaders — rewrite Host / :authority to the chosen downstream host
//	                (contract §0.2 "统一地址转换", the E08 story).
//	DecodeData    — detect `stream:true` in a chat request and mark the
//	                request streaming so the response keeps SSE framing.
//	EncodeHeaders — for SSE responses set Content-Type: text/event-stream,
//	                Cache-Control/Connection/X-Accel-Buffering and REMOVE
//	                Content-Length (§3.4 HARD RULE 2). Append X-Proxied-By
//	                for MCP responses (§5.3).
//	EncodeData    — pass through (the guardrail/final framing is done by the
//	                OutputGuardrailFilter and the server's §3.4 handling).
//
// The OpenAI request/response CONVERSION itself lives in modelgw/transform.go;
// this filter only decides transport-level framing and address rewrite.
type TransformFilter struct {
	a2a.BaseFilter
	rt *Runtime
}

// Name identifies the filter.
func (f *TransformFilter) Name() string { return "transform" }

// DecodeHeaders rewrites the Host header to the downstream authority.
func (f *TransformFilter) DecodeHeaders(h *a2a.HeaderMap, endStream bool) a2a.FilterStatus {
	if host := h.Get(HdrHost); host != "" {
		// Envoy :authority rewrite equivalent. The proxy layer uses the
		// HdrHost value as the outbound Host and strips it before forwarding.
		h.Set("Host", host)
	}
	return a2a.Continue
}

// DecodeData detects a streaming chat request.
func (f *TransformFilter) DecodeData(buf []byte, endStream bool) a2a.FilterStatus {
	ctx := f.rt.Active()
	if ctx == nil || len(buf) == 0 {
		return a2a.Continue
	}
	ep := ctx.RequestHeaders.Get(HdrModelEP)
	if ep == "chat" {
		var probe struct {
			Stream bool `json:"stream"`
		}
		if json.Unmarshal(buf, &probe) == nil && probe.Stream {
			ctx.RequestHeaders.Set(HdrStreaming, "1")
		}
	}
	// A2A message/stream / tasks/subscribe are SSE by contract (§1.6). These
	// are transparently proxied; the upstream decides Content-Type. The
	// EncodeHeaders hook below turns any text/event-stream response into a
	// properly-framed stream regardless of the requested method.
	return a2a.Continue
}

// EncodeHeaders normalises response headers (SSE framing + proxy headers).
func (f *TransformFilter) EncodeHeaders(h *a2a.HeaderMap, endStream bool) a2a.FilterStatus {
	ctx := f.rt.Active()
	ct := h.Get("Content-Type")
	isSSE := contains(ct, "text/event-stream") ||
		(ctx != nil && ctx.RequestHeaders.Get(HdrStreaming) == "1")

	if isSSE {
		// §3.4 HARD RULE 2: SSE must be text/event-stream, no Content-Length,
		// no intermediate buffering; the server Flush()es every frame.
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Del("Content-Length")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		if ctx != nil {
			ctx.RequestHeaders.Set(HdrStreaming, "1")
		}
	}

	// MCP proxy marker (contract §5.3).
	if ctx != nil && ctx.RequestHeaders.Get(HdrBackend) == "mcp" {
		h.Set(HdrProxiedBy, "data-plane")
	}
	return a2a.Continue
}

// EncodeData passes the buffer through. Per-chunk guardrail inspection is done
// by OutputGuardrailFilter (§3.4) and applied by the server relay.
func (f *TransformFilter) EncodeData(buf []byte, endStream bool) a2a.FilterStatus {
	return a2a.Continue
}

// Reset clears per-request state.
func (f *TransformFilter) Reset() {}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
