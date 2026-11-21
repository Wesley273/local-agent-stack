package filters

import (
	"local-agent-stack/pkg/a2a"
)

// RateLimitFilter is filter #2 (DecodeHeaders). It enforces the request-count
// dimension with a2a.MemoryLimiter.AllowRequest (contract §2.4 / §3.2).
//
// The token dimension (ReserveTokens / SettleTokens) is applied by the model
// gateway for /v1/chat/completions & /v1/embeddings; the router marks those
// requests so this filter can still record the pre-flight. Both dimensions
// use the SAME MemoryLimiter instance (dual dimension, one source of truth).
type RateLimitFilter struct {
	a2a.BaseFilter
	rt *Runtime
}

// Name identifies the filter.
func (f *RateLimitFilter) Name() string { return "ratelimit" }

// DecodeHeaders implements the request-rate phase.
func (f *RateLimitFilter) DecodeHeaders(h *a2a.HeaderMap, endStream bool) a2a.FilterStatus {
	snap := f.rt.Config.Current()
	appID := h.Get(HdrAppID)
	if appID == "" {
		appID = "anonymous"
	}
	rule := snap.RateRuleFor(appID)
	dec := f.rt.Limiter.AllowRequest(appID, rule)
	if !dec.Allowed {
		f.rt.rlHits.Add(1)
		h.Set(HdrFilterCode, string(a2a.CodeRateLimited))
		h.Set(HdrFilterMsg, dec.Reason)
		if dec.RetryAfter > 0 {
			h.Set(HdrRetryAfter, itoa(dec.RetryAfter))
		}
		return a2a.StopAndReply
	}
	return a2a.Continue
}

// Reset clears per-request state.
func (f *RateLimitFilter) Reset() {}

// itoa avoids strconv import in this hot file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
