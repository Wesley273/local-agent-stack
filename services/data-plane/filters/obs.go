package filters

import (
	"local-agent-stack/pkg/a2a"
)

// ObservabilityFilter is filter #6. It finalises and records the per-request
// a2a.Span. The server sets ctx.Span / status code / token attributes before
// the chain is destroyed; here we simply Finish + Add it to the SpanStore.
//
// a2a.SpanStore.Add is PER-SPAN fault tolerant (contract §1.8): one malformed
// span never aborts the whole batch. http.status_code and llm.tokens.* are
// always integers — a2a.SpanStore.Add coerces http.status_code defensively and
// the model gateway only ever writes int values.
type ObservabilityFilter struct {
	a2a.BaseFilter
	rt *Runtime
}

// Name identifies the filter.
func (f *ObservabilityFilter) Name() string { return "observability" }

// OnDestroy records the span (contract §3.2 "Obs on OnDestroy").
func (f *ObservabilityFilter) OnDestroy(reason a2a.DestroyReason) {
	ctx := f.rt.Active()
	if ctx == nil || ctx.Span == nil || f.rt.Spans == nil {
		return
	}
	status := "OK"
	if reason != a2a.ReasonNormal {
		status = "ERROR"
	}
	ctx.Span.Set("filter.destroy_reason", string(reason))
	_ = f.rt.Spans.Add(ctx.Span.Finish(status))
}

// Reset clears per-request state.
func (f *ObservabilityFilter) Reset() {}
