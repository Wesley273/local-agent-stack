package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"local-agent-stack/pkg/a2a"
	"local-agent-stack/services/data-plane/config"
	"local-agent-stack/services/data-plane/filters"
	"local-agent-stack/services/data-plane/modelgw"
)

// Server is the data-plane core: it owns the filter chain, the config poller,
// the model gateway and the shared observability store.
type Server struct {
	Cfg     *config.Store
	Poller  *config.Poller
	Runtime *filters.Runtime
	Chain   *a2a.FilterChain
	Gateway *modelgw.Gateway
	Spans   *a2a.SpanStore
	Limiter *a2a.MemoryLimiter

	// Reverse proxy transport (shared, connection pooling).
	Transport *http.Transport

	// Addresses (from env / ports.env).
	SidecarBase string
	LLMBase     string
	MCPBase     string
	CPBase      string
	Version     string
}

// NewServer wires all dependencies together.
func NewServer(cfg *config.Store, poller *config.Poller,
	sidecar, llm, mcp, cp string) *Server {

	store := a2a.NewSpanStore(2000)
	limiter := a2a.NewMemoryLimiter()
	rt := filters.NewRuntime(cfg, limiter, store)
	rt.RouterTargets = filters.RouterTargets{
		Sidecar: sidecar, LLM: llm, MCP: mcp, LLMPath: llm, MCPPath: mcp,
	}
	chain := filters.BuildChain(rt)
	gw := modelgw.NewGateway(cfg, limiter, 4)

	return &Server{
		Cfg: cfg, Poller: poller, Runtime: rt, Chain: chain,
		Gateway: gw, Spans: store, Limiter: limiter,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 0, // streaming responses must not be capped
		},
		SidecarBase: sidecar, LLMBase: llm, MCPBase: mcp, CPBase: cp,
		Version: "1.0.0",
	}
}

// ---------------------------------------------------------------------------
// Main dispatch
// ---------------------------------------------------------------------------

// ServeHTTP is the single entry point for every governed request.
//
// Flow (contract §3.2):
//  1. build a per-request FilterContext + Span
//  2. run the DECODE chain (Auth → RateLimit → Guardrail(Input) → Router →
//     Transform → Upstream). A StopAndReply here renders an error envelope.
//  3. dispatch to: compat handler / model gateway / transparent reverse proxy
//  4. run the ENCODE chain (EncodeHeaders + per-chunk EncodeData). This is
//     where OutputGuardrailFilter applies the §3.4 SSE fix.
//  5. DestroyAll -> ObservabilityFilter records the span.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := r.Header.Get(filters.HdrRequestID)
	if requestID == "" {
		requestID = a2a.NewID("")
	}
	r.Header.Set(filters.HdrRequestID, requestID)

	// Read the full request body once (governance needs it for input guardrail).
	body, readErr := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if readErr != nil {
		body = []byte{}
	}

	ctx := &a2a.FilterContext{
		RequestID:      requestID,
		TraceID:        requestID,
		Method:         r.Method,
		Path:           r.URL.Path,
		RequestHeaders: a2a.NewHeaderMap(r.Header),
		SpanStore:      s.Spans,
		Span:           a2a.NewSpan(requestID, "", "data-plane.request", "server"),
	}
	ctx.Span.Set("http.method", r.Method).Set("http.path", r.URL.Path)
	ctx.Span.Event("filter.auth")
	// Seed internal pseudo-headers used by the filters.
	ctx.RequestHeaders.Set(filters.HdrPath, r.URL.Path)
	ctx.RequestHeaders.Set(filters.HdrMethod, r.Method)
	rpcMethod := rpcMethodFromBody(body)
	ctx.RequestHeaders.Set(filters.HdrRPCMethod, rpcMethod)
	// Detect streaming EARLY so an input-guardrail hit on a streaming request
	// is rendered as SSE (§3.4/E10) even though the guardrail short-circuits
	// RunDecode before TransformFilter.DecodeData can set the marker.
	if isStreamingRequest(r, body, rpcMethod) {
		ctx.RequestHeaders.Set(filters.HdrStreaming, "1")
	}

	// ---- DECODE chain ----------------------------------------------------
	reply, decErr := s.Runtime.Decode(s.Chain, ctx, body, true)
	if decErr != nil {
		s.writeEnvelope(w, a2a.NewError(a2a.CodeInternalError, decErr.Error(), requestID))
		return
	}
	if reply != nil {
		s.finishSpan(ctx, reply.Status)
		// GUARDRAIL_REJECTED gets special rendering (§3.4 / §0.3):
		// non-stream -> 200 + error envelope; stream -> 200 SSE error frame+DONE.
		if strings.Contains(string(reply.Body), string(a2a.CodeGuardrailRejected)) {
			s.writeGuardrailResponse(w, ctx, string(reply.Body))
		} else {
			retry := ctx.RequestHeaders.Get(filters.HdrRetryAfter)
			if retry != "" {
				w.Header().Set("Retry-After", retry)
			}
			s.writeReply(w, reply)
		}
		return
	}

	// ---- Dispatch --------------------------------------------------------
	local := ctx.RequestHeaders.Get(filters.HdrLocal)
	ep := ctx.RequestHeaders.Get(filters.HdrModelEP)

	switch {
	case local == "compat":
		s.handleCompat(w, r)
		s.finishSpan(ctx, http.StatusOK)
		return
	case local == "models":
		modelgw.WriteModels(w, s.Cfg.Current())
		s.finishSpan(ctx, http.StatusOK)
		return
	case ep == "chat" || ep == "embeddings":
		s.handleModelGateway(w, r, ctx, body, ep)
		return
	default:
		s.handleReverseProxy(w, r, ctx, body)
		return
	}
}

func (s *Server) finishSpan(ctx *a2a.FilterContext, status int) {
	if ctx.Span != nil {
		ctx.Span.Set("http.status_code", status) // integer (§1.8 / E23)
	}
	// DestroyAll triggers ObservabilityFilter.OnDestroy which Finishes+Adds.
	// ObservabilityFilter marks the span ERROR for non-normal reasons.
	s.Runtime.Destroy(s.Chain, ctx, a2a.ReasonNormal)
}

// ---------------------------------------------------------------------------
// Model gateway dispatch
// ---------------------------------------------------------------------------

func (s *Server) handleModelGateway(w http.ResponseWriter, r *http.Request,
	ctx *a2a.FilterContext, body []byte, ep string) {

	appID := ctx.RequestHeaders.Get(filters.HdrAppID)
	if appID == "" {
		appID = "anonymous"
	}
	priority := a2a.ParsePriority(r.Header.Get(filters.HdrPriority))
	res, ge := s.Gateway.Execute(r.Context(), ep, r.URL.Path, ctx.RequestID,
		appID, priority, body, r.Header)
	if ge != nil {
		if ge.RetryAfter > 0 {
			w.Header().Set("Retry-After", itoa(ge.RetryAfter))
		}
		env := a2a.NewError(ge.Code, ge.Message, ctx.RequestID)
		s.finishSpan(ctx, a2a.HTTPStatus(ge.Code))
		s.writeEnvelope(w, env)
		return
	}

	if res.Stream != nil {
		s.relayStream(w, ctx, res)
	} else {
		s.relayBuffered(w, ctx, res)
	}
}

// ---------------------------------------------------------------------------
// Transparent reverse proxy (A2A / MCP) — contract §0.0 / §5.
//
// The A2A request is forwarded VERBATIM: we never parse message/send vs
// SendMessage, never rewrite JSON-RPC methods, never touch Task states.
// ---------------------------------------------------------------------------

func (s *Server) handleReverseProxy(w http.ResponseWriter, r *http.Request,
	ctx *a2a.FilterContext, body []byte) {

	target := ctx.RequestHeaders.Get(filters.HdrUpstream)
	if target == "" {
		target = s.SidecarBase
	}
	u, err := url.Parse(target)
	if err != nil {
		s.upstreamError(w, ctx, a2a.CodeUpstreamError, "bad upstream url: "+err.Error())
		return
	}

	// Build a single-shot reverse proxy with FlushInterval=-1 for streaming.
	rp := &httputil.ReverseProxy{
		Transport:     s.Transport,
		FlushInterval: -1,
		Director: func(out *http.Request) {
			out.URL.Scheme = u.Scheme
			out.URL.Host = u.Host
			// Keep the ORIGINAL path (transparent proxy). If the snapshot gave
			// a full agent url with its own path, use that path as the base.
			if u.Path != "" && u.Path != "/" {
				out.URL.Path = singleJoin(u.Path, r.URL.Path)
			} else {
				out.URL.Path = r.URL.Path
			}
			out.URL.RawQuery = r.URL.RawQuery
			out.Host = u.Host // :authority / Host rewrite (§0.2)
			out.Method = r.Method
			out.Body = io.NopCloser(bytes.NewReader(body))
			out.ContentLength = int64(len(body))
			// Forward standard headers, strip internal pseudo-headers.
			out.Header = r.Header.Clone()
			for _, k := range filters.InternalHeaders {
				out.Header.Del(k)
			}
			out.Header.Set(filters.HdrRequestID, ctx.RequestID)
			out.Header.Set(filters.HdrAppID, ctx.RequestHeaders.Get(filters.HdrAppID))
			out.Header.Set(filters.HdrA2AVersion, "1.0")
			// 官方 a2a-go/v2 v2.8.0 读取的是不带 X- 前缀的 "A2A-Version"
			// （缺失时返回 -32009 VERSION_NOT_SUPPORTED: requested "0.3", served "1.0"）。
			// 契约 §0.2 的 X-A2A-Version 是我们的兼容轨头，两个都发。
			out.Header.Set("A2A-Version", "1.0")
			out.Header.Set(filters.HdrTraceSpan, ctx.Span.SpanID)
		},
		ModifyResponse: func(resp *http.Response) error {
			// MCP marker (§5.3). Also captured by TransformFilter's EncodeHeaders.
			if ctx.RequestHeaders.Get(filters.HdrBackend) == "mcp" {
				resp.Header.Set(filters.HdrProxiedBy, "data-plane")
			}
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			// Streaming already started -> §3.4: emit SSE error frames, never a
			// new status line / RST. For non-stream, UPSTREAM_ERROR/504.
			if ctx.ResponseStarted {
				s.streamUpstreamError(rw, ctx, a2a.CodeStreamClosed, "upstream connection closed: "+err.Error())
				return
			}
			code := a2a.CodeUpstreamError
			if strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline") {
				code = a2a.CodeUpstreamTimeout
			}
			s.upstreamError(rw, ctx, code, err.Error())
		},
	}

	// Wrap the ResponseWriter so the ENCODE chain (OutputGuardrailFilter) sees
	// every chunk and applies the §3.4 rule. We only intercept WriteHeader /
	// Write; Flush is forwarded for SSE.
	fw := newFilterWriter(w, s, ctx)
	rp.ServeHTTP(fw, r)
	fw.finalise()
}

func singleJoin(base, p string) string {
	if p == "" || p == "/" {
		return base
	}
	if len(base) > 0 && base[len(base)-1] == '/' && p[0] == '/' {
		return base[:len(base)-1] + p
	}
	return base + p
}

func (s *Server) upstreamError(w http.ResponseWriter, ctx *a2a.FilterContext, code a2a.ErrorCode, msg string) {
	s.finishSpan(ctx, a2a.HTTPStatus(code))
	s.writeEnvelope(w, a2a.NewError(code, msg, ctx.RequestID))
}

// ---------------------------------------------------------------------------
// Response relay through the ENCODE chain (§3.4 core).
// ---------------------------------------------------------------------------

// relayBuffered runs the encode chain on a full body. If OutputGuardrailFilter
// set the dedup marker, the whole body is replaced by a 200 GUARDRAIL_REJECTED
// envelope; otherwise the (possibly appended) bytes are written once.
func (s *Server) relayBuffered(w http.ResponseWriter, ctx *a2a.FilterContext, res *modelgw.Result) {
	respHdr := a2a.NewHeaderMap(res.Header.Clone())
	s.Runtime.EncodeHeaders(s.Chain, ctx, respHdr, false)

	buf, appended := s.Runtime.EncodeData(s.Chain, ctx, res.Body, true)
	out := append(append([]byte(nil), buf...), appended...)

	status := res.Status
	if status == 0 {
		status = http.StatusOK
	}
	// Non-stream output guardrail: 200 + error envelope (§3.4 regression 2).
	if ctx.RequestHeaders.Get(filters.HdrGuardRejected) == "1" {
		env := a2a.NewError(a2a.CodeGuardrailRejected,
			"blocked by output guardrail", ctx.RequestID)
		out, _ = json.Marshal(env)
		status = http.StatusOK
		respHdr.Set("Content-Type", "application/json; charset=utf-8")
		respHdr.Del("Content-Length")
	}
	respHdr.ApplyTo(w.Header())
	w.WriteHeader(status)
	_, _ = w.Write(out)

	if res.TotalToken > 0 {
		ctx.Span.Set("llm.tokens.total", res.TotalToken)
	}
	s.finishSpan(ctx, status)
}

// relayStream streams Result.Stream through the encode chain, Flush()ing every
// frame, never setting Content-Length. §3.4: if a guardrail hits mid-stream,
// the appended SSE error frames are written and the loop continues to drain
// the upstream channel to [DONE] — no new status line, no RST/FIN, no close.
func (s *Server) relayStream(w http.ResponseWriter, ctx *a2a.FilterContext, res *modelgw.Result) {
	respHdr := a2a.NewHeaderMap(res.Header.Clone())
	s.Runtime.EncodeHeaders(s.Chain, ctx, respHdr, false)
	// §3.4 HARD RULE 2: SSE headers, no Content-Length.
	respHdr.Set("Content-Type", "text/event-stream; charset=utf-8")
	respHdr.Del("Content-Length")
	respHdr.Set("Cache-Control", "no-cache")
	respHdr.Set("Connection", "keep-alive")
	respHdr.Set("X-Accel-Buffering", "no")
	respHdr.ApplyTo(w.Header())

	flusher, _ := w.(http.Flusher)
	w.WriteHeader(res.Status) // ONE status line, before anything else
	ctx.ResponseStarted = true

	doneWritten := false
	for chunk := range res.Stream {
		// Per-chunk guardrail inspection (§3.4 / §2.4 rule 5).
		out, appended := s.Runtime.EncodeData(s.Chain, ctx, chunk, false)
		if len(appended) > 0 {
			// Guardrail hit: emit error frame + [DONE], then STOP forwarding
			// upstream bytes (but keep draining the channel; no close/RST).
			_, _ = w.Write(appended)
			if flusher != nil {
				flusher.Flush()
			}
			doneWritten = true
			// Skip the offending chunk itself.
			continue
		}
		if doneWritten {
			// Already emitted the terminal error+DONE; drain silently.
			continue
		}
		if len(out) > 0 {
			_, _ = w.Write(out)
			if flusher != nil {
				flusher.Flush() // every frame
			}
			if bytes.Contains(out, []byte("[DONE]")) {
				doneWritten = true
			}
		}
	}
	// If upstream closed early without [DONE], emit a normal DONE so the client
	// sees a clean end (curl exit 0). NEVER RST/FIN (§3.4 HARD RULE 3).
	if !doneWritten {
		_, _ = w.Write([]byte(a2a.SSEDoneFrame()))
		if flusher != nil {
			flusher.Flush()
		}
	}
	if res.TotalToken > 0 {
		ctx.Span.Set("llm.tokens.total", res.TotalToken)
	}
	s.finishSpan(ctx, res.Status)
}

// streamUpstreamError emits an SSE error frame + [DONE] on a stream that has
// ALREADY started. This is the §3.4 HARD RULE 3 path (upstream timeout /
// disconnect). It never writes a new HTTP status line.
func (s *Server) streamUpstreamError(w http.ResponseWriter, ctx *a2a.FilterContext,
	code a2a.ErrorCode, msg string) {
	frames := a2a.SSEErrorFrame(code, msg, ctx.RequestID) + a2a.SSEDoneFrame()
	_, _ = w.Write([]byte(frames))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	log.Printf("[server] WARN §3.4: upstream error on started stream; "+
		"emitted SSE error frame + [DONE] (no new status line / RST). code=%s requestId=%s",
		code, ctx.RequestID)
}

// ---------------------------------------------------------------------------
// Error rendering
// ---------------------------------------------------------------------------

func (s *Server) writeReply(w http.ResponseWriter, reply *a2a.Reply) {
	for k, vs := range reply.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(reply.Status)
	_, _ = w.Write(reply.Body)
}

func (s *Server) writeEnvelope(w http.ResponseWriter, env a2a.ErrorEnvelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(env.Error.HTTPStatus)
	b, _ := json.Marshal(env)
	_, _ = w.Write(b)
}

// writeGuardrailResponse renders a GUARDRAIL_REJECTED decision (§0.3 / §3.4):
//   - streaming request -> 200 SSE: event: error + data: [DONE]  (no status
//     rewrite after start, no RST)
//   - non-stream        -> 200 + JSON error envelope
func (s *Server) writeGuardrailResponse(w http.ResponseWriter, ctx *a2a.FilterContext, replyBody string) {
	// Extract the message for a friendlier frame.
	msg := "blocked by input guardrail"
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(replyBody), &parsed) == nil && parsed.Error.Message != "" {
		msg = parsed.Error.Message
	}

	isStream := ctx.RequestHeaders.Get(filters.HdrStreaming) == "1" ||
		strings.Contains(strings.ToLower(ctx.RequestHeaders.Get("Accept")), "text/event-stream") ||
		ctx.RequestHeaders.Get(filters.HdrRPCMethod) == a2a.MethodStream ||
		ctx.RequestHeaders.Get(filters.HdrRPCMethod) == a2a.MethodSubscribe

	if isStream {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		h.Del("Content-Length")
		w.WriteHeader(http.StatusOK) // single status line, 200
		ctx.ResponseStarted = true
		frames := a2a.SSEErrorFrame(a2a.CodeGuardrailRejected, msg, ctx.RequestID) +
			a2a.SSEDoneFrame()
		_, _ = w.Write([]byte(frames))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	s.writeEnvelope(w, a2a.NewError(a2a.CodeGuardrailRejected, msg, ctx.RequestID))
}

func rpcMethodFromBody(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	if s, ok := m["method"].(string); ok {
		return s
	}
	return ""
}

// isStreamingRequest reports whether a request expects an SSE response:
// Accept: text/event-stream, stream:true in a chat body, or a JSON-RPC
// message/stream / tasks/subscribe method (contract §1.6 / §2.1).
func isStreamingRequest(r *http.Request, body []byte, rpcMethod string) bool {
	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream") {
		return true
	}
	if rpcMethod == a2a.MethodStream || rpcMethod == a2a.MethodSubscribe {
		return true
	}
	var probe struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Stream {
		return true
	}
	return false
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
