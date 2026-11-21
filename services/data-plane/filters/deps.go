// Package filters contains the six data-plane StreamFilter implementations.
//
// All filter instances are pooled by a2a.FilterChain (sync.Pool). Because the
// StreamFilter interface has no per-request context setter, per-request state
// lives in the shared *a2a.FilterContext, which the filters reach through
// Runtime.Active() (bound briefly around each FilterChain call by the server).
// This is the "request object lifecycle" fix: we never cache a recycled
// request object, and filters never retain per-request state past Reset().
package filters

import (
	"log"
	"sync"
	"sync/atomic"

	"local-agent-stack/pkg/a2a"
	"local-agent-stack/services/data-plane/config"
)

// ---------------------------------------------------------------------------
// Internal pseudo-headers used to pass decisions between the server and the
// pooled filters / between filters. They are stripped before proxying.
// ---------------------------------------------------------------------------

const (
	HdrAppID      = "X-App-Id"
	HdrAPIKey     = "X-API-Key"
	HdrRequestID  = "X-Request-Id"
	HdrTraceSpan  = "X-Trace-Span-Id"
	HdrPriority   = "X-Priority"
	HdrA2AVersion = "X-A2A-Version"

	HdrFilterCode = "X-Filter-Error-Code" // read by a2a.replyFromFilter
	HdrFilterMsg  = "X-Filter-Error-Message"

	HdrUpstream   = "X-DP-Upstream" // chosen downstream base URL
	HdrAgentID    = "X-DP-Agent-Id"
	HdrModel      = "X-DP-Model"
	HdrPath       = "X-DP-Path" // original request path
	HdrMethod     = "X-DP-Method"
	HdrHost       = "X-DP-Host"     // Host rewrite target
	HdrModelEP    = "X-DP-Model-Ep" // "chat" | "embeddings" | "models"
	HdrRetryAfter = "X-DP-Retry-After"
	HdrStreaming  = "X-DP-Streaming"  // "1" when the response is an SSE stream
	HdrRPCMethod  = "X-DP-Rpc-Method" // JSON-RPC method for the instruction guardrail
	HdrLocal      = "X-DP-Local"      // "compat" | "models" -> served locally
	HdrBackend    = "X-DP-Backend"    // "sidecar" | "llm" | "mcp" | "compat"

	HdrGuardRejected = "X-DP-Guard-Rejected" // dedup marker for §3.4

	HdrProxiedBy = "X-Proxied-By"
)

// InternalHeaders is the set of pseudo-headers that must NOT leak downstream.
var InternalHeaders = []string{
	HdrFilterCode, HdrFilterMsg, HdrUpstream, HdrAgentID, HdrModel,
	HdrPath, HdrMethod, HdrHost, HdrModelEP, HdrRetryAfter, HdrStreaming,
	HdrGuardRejected, HdrRPCMethod, HdrLocal, HdrBackend,
}

// ---------------------------------------------------------------------------
// Runtime — long-lived, shared dependencies held by every pooled filter.
// ---------------------------------------------------------------------------

// RouterTargets carries the env-derived downstream base URLs into the pooled
// RouterFilter instances (they are created lazily by sync.Pool).
type RouterTargets struct {
	Sidecar string
	LLM     string
	MCP     string
	LLMPath string
	MCPPath string
}

// Runtime holds the shared, concurrency-safe services filters depend on.
// A single Runtime is shared by all filter instances and all requests.
type Runtime struct {
	Config        config.Provider
	Limiter       *a2a.MemoryLimiter
	Spans         *a2a.SpanStore
	RouterTargets RouterTargets

	// Guardrail engine cache, rebuilt only when the config revision changes
	// (so guardrail hot-reload works too, and regexes aren't recompiled per call).
	engMu  sync.Mutex
	engRev int64
	eng    *a2a.GuardrailEngine

	// Binds the current *a2a.FilterContext for the duration of a FilterChain
	// call. The brief lock serialises only the microseconds-long filter call,
	// never the whole HTTP response.
	bindMu sync.Mutex
	active atomic.Pointer[a2a.FilterContext]

	// Metrics counters (merged with SpanStore counters by the admin endpoint).
	reqTotal   atomic.Int64
	reqErrors  atomic.Int64
	guardHits  atomic.Int64
	downgrades atomic.Int64
	rlHits     atomic.Int64
	relays     atomic.Int64

	Warnf func(format string, args ...any)
}

// NewRuntime builds a Runtime.
func NewRuntime(cfg config.Provider, limiter *a2a.MemoryLimiter, spans *a2a.SpanStore) *Runtime {
	return &Runtime{
		Config: cfg, Limiter: limiter, Spans: spans,
		Warnf: log.Printf,
	}
}

// Active returns the FilterContext bound to the current FilterChain call.
func (rt *Runtime) Active() *a2a.FilterContext { return rt.active.Load() }

// The wrappers below bind ctx ONLY for the (microsecond-long) FilterChain
// call — never for the whole HTTP request or the upstream transfer. Holding
// bindMu for an entire request would serialise all in-flight requests and
// defeat the priority scheduler (E17), so the server always uses these
// wrappers instead of calling the chain directly.

func (rt *Runtime) bind(ctx *a2a.FilterContext) func() {
	rt.bindMu.Lock()
	rt.active.Store(ctx)
	return func() {
		rt.active.Store(nil)
		rt.bindMu.Unlock()
	}
}

// Decode runs the decode phases with ctx bound.
func (rt *Runtime) Decode(fc *a2a.FilterChain, ctx *a2a.FilterContext, body []byte, end bool) (*a2a.Reply, error) {
	done := rt.bind(ctx)
	defer done()
	return fc.RunDecode(ctx, body, end)
}

// EncodeHeaders runs the encode-headers phases with ctx bound.
func (rt *Runtime) EncodeHeaders(fc *a2a.FilterChain, ctx *a2a.FilterContext, h *a2a.HeaderMap, end bool) {
	done := rt.bind(ctx)
	defer done()
	fc.RunEncodeHeaders(ctx, h, end)
}

// EncodeData runs one encode-data chunk through the chain with ctx bound.
func (rt *Runtime) EncodeData(fc *a2a.FilterChain, ctx *a2a.FilterContext, buf []byte, end bool) ([]byte, []byte) {
	done := rt.bind(ctx)
	defer done()
	return fc.RunEncodeData(ctx, buf, end)
}

// Destroy runs OnDestroy with ctx bound.
func (rt *Runtime) Destroy(fc *a2a.FilterChain, ctx *a2a.FilterContext, reason a2a.DestroyReason) {
	done := rt.bind(ctx)
	defer done()
	fc.DestroyAll(reason)
}

// Engine returns the guardrail engine for the current config revision.
func (rt *Runtime) Engine() *a2a.GuardrailEngine {
	rev := rt.Config.Current().Revision
	rt.engMu.Lock()
	defer rt.engMu.Unlock()
	if rt.eng == nil || rt.engRev != rev {
		rt.eng = a2a.NewGuardrailEngine(rt.Config.Current().GuardrailConfig())
		rt.engRev = rev
	}
	return rt.eng
}

func (rt *Runtime) warnf(format string, args ...any) {
	if rt.Warnf != nil {
		rt.Warnf(format, args...)
	}
}

// Counters returns a snapshot of gateway-level metric counters.
func (rt *Runtime) Counters() map[string]int64 {
	out := rt.Spans.Counters()
	out["dp.requests.total"] = rt.reqTotal.Load()
	out["dp.requests.errors"] = rt.reqErrors.Load()
	out["dp.guardrail.hits"] = rt.guardHits.Load()
	out["dp.sse.downgrades"] = rt.downgrades.Load()
	out["dp.ratelimit.hits"] = rt.rlHits.Load()
	out["dp.relays"] = rt.relays.Load()
	out["dp.limiter"] = int64(len(rt.Limiter.Stats()))
	return out
}

// BuildChain registers the six filters in the FIXED order (contract §3.2):
//
//	Auth → RateLimit → Guardrail(Input) → Router → Transform → Upstream
//	                                                   ↓ (response)
//	Obs ← Guardrail(Output) ← EncodeHeaders ← EncodeData(逐块) ← Upstream
//
// ObservabilityFilter and OutputGuardrailFilter are no-ops on the decode side;
// they act on the response side (OnDestroy / EncodeData). The actual upstream
// transfer is performed by the server after the decode chain completes.
func BuildChain(rt *Runtime) *a2a.FilterChain {
	fc := a2a.NewFilterChain()
	fc.SetWarnLogger(func(msg string) {
		rt.warnf("[FilterChain] WARN %s", msg)
		// Every "LocalReply ignored after stream start" is a §3.4 downgrade.
		rt.downgrades.Add(1)
	})
	fc.Register("auth", func() a2a.StreamFilter { return &AuthFilter{rt: rt} })
	fc.Register("ratelimit", func() a2a.StreamFilter { return &RateLimitFilter{rt: rt} })
	fc.Register("guardrail-input", func() a2a.StreamFilter { return &InputGuardrailFilter{rt: rt} })
	fc.Register("router", func() a2a.StreamFilter {
		return &RouterFilter{
			rt:          rt,
			SidecarBase: rt.RouterTargets.Sidecar,
			LLMBase:     rt.RouterTargets.LLM,
			MCPBase:     rt.RouterTargets.MCP,
			LLMPath:     rt.RouterTargets.LLMPath,
			MCPPath:     rt.RouterTargets.MCPPath,
		}
	})
	fc.Register("transform", func() a2a.StreamFilter { return &TransformFilter{rt: rt} })
	fc.Register("upstream", func() a2a.StreamFilter { return &UpstreamFilter{rt: rt} })
	fc.Register("observability", func() a2a.StreamFilter { return &ObservabilityFilter{rt: rt} })
	fc.Register("guardrail-output", func() a2a.StreamFilter { return &OutputGuardrailFilter{rt: rt} })
	return fc
}
