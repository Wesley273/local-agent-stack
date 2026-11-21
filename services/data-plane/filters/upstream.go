package filters

import (
	"local-agent-stack/pkg/a2a"
)

// UpstreamFilter is the final decode-stage filter (contract §3.2):
//
//	Auth → RateLimit → Guardrail(Input) → Router → Transform → Upstream
//
// Its job is to mark the request as governance-complete and record the
// "upstream.start" span event. The actual bytes are transferred by the server
// (server.go) after RunDecode returns — a net/http handler cannot be "inside"
// a pooled filter — but the phase boundary is explicit on the chain so the
// Envoy semantic mapping (docs/05 §2.1) stays one-to-one.
//
// On the response side it sets the "upstream.end" event on the span.
type UpstreamFilter struct {
	a2a.BaseFilter
	rt *Runtime
}

// Name identifies the filter.
func (f *UpstreamFilter) Name() string { return "upstream" }

// DecodeHeaders marks the decode chain complete.
func (f *UpstreamFilter) DecodeHeaders(h *a2a.HeaderMap, endStream bool) a2a.FilterStatus {
	ctx := f.rt.Active()
	if ctx != nil && ctx.Span != nil {
		ctx.Span.Event("upstream.start")
	}
	f.rt.relays.Add(1)
	return a2a.Continue
}

// EncodeData annotates the span on the last chunk.
func (f *UpstreamFilter) EncodeData(buf []byte, endStream bool) a2a.FilterStatus {
	if endStream {
		if ctx := f.rt.Active(); ctx != nil && ctx.Span != nil {
			ctx.Span.Event("upstream.end")
		}
	}
	return a2a.Continue
}

// Reset clears per-request state.
func (f *UpstreamFilter) Reset() {}
