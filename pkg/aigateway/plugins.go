package aigateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"local-agent-stack/pkg/composite"
)

// ---------------------------------------------------------------------------
// AsyncGuardChecker — pluggable async backend for input/output moderation.
// The demo implementation sleeps to simulate a remote call; real ones call an
// LLM/moderation service and return a verdict.
// ---------------------------------------------------------------------------

type AsyncGuardChecker interface {
	Check(direction, text string) GuardVerdict
}

// CheckerFunc adapts a function to AsyncGuardChecker.
type CheckerFunc func(direction, text string) GuardVerdict

func (f CheckerFunc) Check(direction, text string) GuardVerdict { return f(direction, text) }

// BlockingChecker simulates a remote moderation service with configurable delay.
func BlockingChecker(delay time.Duration, blockedWords []string) CheckerFunc {
	return func(direction, text string) GuardVerdict {
		time.Sleep(delay)
		v := GuardVerdict{Direction: direction, Allowed: true, CheckedAt: time.Now().UnixMilli()}
		lower := strings.ToLower(text)
		for _, w := range blockedWords {
			if w != "" && strings.Contains(lower, strings.ToLower(w)) {
				v.Allowed = false
				v.Code = "GUARDRAIL_REJECTED"
				v.Reason = fmt.Sprintf("%s guardrail blocked word %q", direction, w)
				return v
			}
		}
		return v
	}
}

// ---------------------------------------------------------------------------
// 1. BodyModelParser — first filter needing full body -> StopAndBuffer.
//    On full body: parse (chain already parsed once), extract model, estimate
//    prompt tokens, init metrics, and START THE ASYNC INPUT GUARDRAIL.
// ---------------------------------------------------------------------------

type BodyModelParser struct {
	composite.BaseFilter
	checker    AsyncGuardChecker
	guardDelay time.Duration
}

func NewBodyModelParser(c AsyncGuardChecker) *BodyModelParser {
	return &BodyModelParser{checker: c}
}

func (f *BodyModelParser) Name() string { return "body-model-parser" }

func (f *BodyModelParser) DecodeHeaders(c *composite.Context, end bool) composite.Status {
	// We need the full body to read "model". Trigger one-time buffering.
	return composite.StopAndBuffer
}

func (f *BodyModelParser) DecodeData(c *composite.Context, data []byte, end bool) composite.Status {
	m := c.ParsedBodyMap()
	model := ""
	if v, ok := m["model"].(string); ok {
		model = v
	}
	if model == "" {
		model = "default"
	}
	c.Set(KModel, model)

	// Prompt tokens estimated ONCE from the cached body (never re-parse).
	prompt := estimateTokens(string(data))
	c.Set(KPromptTokens, prompt)

	// Metrics snapshot for this request.
	c.Set(KMetrics, &MetricSnapshot{
		Model: model, RequestID: c.RequestID, PromptTokens: prompt,
		TotalTokens: prompt, IsStream: false,
	})

	// Start async INPUT guardrail right after body is available.
	in := NewFuture[GuardVerdict]()
	c.Set(KInputFuture, in)
	text := extractText(data)
	checker := f.checker
	go func() {
		v := checker.Check("input", text)
		in.Resolve(v)
	}()
	return composite.Continue
}

// extractText pulls user/agent text out of an OpenAI-style or A2A-style body.
func extractText(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return string(body)
	}
	var sb strings.Builder
	// OpenAI: messages[].content
	if msgs, ok := m["messages"].([]any); ok {
		for _, item := range msgs {
			if mm, ok := item.(map[string]any); ok {
				if c, ok := mm["content"].(string); ok {
					sb.WriteString(c)
					sb.WriteByte(' ')
				}
			}
		}
	}
	// A2A: message.parts[].text
	if msg, ok := m["message"].(map[string]any); ok {
		if parts, ok := msg["parts"].([]any); ok {
			for _, p := range parts {
				if pm, ok := p.(map[string]any); ok {
					if t, ok := pm["text"].(string); ok {
						sb.WriteString(t)
						sb.WriteByte(' ')
					}
				}
			}
		}
	}
	if sb.Len() == 0 {
		if p, ok := m["prompt"].(string); ok {
			sb.WriteString(p)
		}
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// 2. TokenLimiter — reserve prompt tokens in decode; settle completion tokens
//    in encode. Backend is pluggable via Limiter interface.
// ---------------------------------------------------------------------------

type TokenLimiter interface {
	Reserve(tokens int) (bool, error) // deduct prompt tokens up front
	Settle(completion int)            // deduct actual completion tokens
}

type TokenState struct {
	mu    sync.Mutex
	limit int
	used  int
}

// NewFixedWindowLimiter is a simple in-process limiter for the demo.
func NewFixedWindowLimiter(limit int) *TokenState { return &TokenState{limit: limit} }

func (l *TokenState) Reserve(tokens int) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used+tokens > l.limit {
		return false, fmt.Errorf("token quota exceeded: used=%d reserve=%d limit=%d", l.used, tokens, l.limit)
	}
	l.used += tokens
	return true, nil
}
func (l *TokenState) Settle(completion int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.used += completion
}

type TokenLimiterFilter struct {
	composite.BaseFilter
	limiter TokenLimiter
}

func NewTokenLimiter(l TokenLimiter) *TokenLimiterFilter {
	return &TokenLimiterFilter{limiter: l}
}
func (f *TokenLimiterFilter) Name() string { return "token-limiter" }

func (f *TokenLimiterFilter) DecodeData(c *composite.Context, data []byte, end bool) composite.Status {
	prompt, _ := c.Get(KPromptTokens).(int)
	if prompt > 0 {
		ok, err := f.limiter.Reserve(prompt)
		if !ok {
			c.Reject("RATE_LIMITED", err.Error())
			return composite.StopAndReply
		}
		c.Set(KTokenReserved, true)
	}
	return composite.Continue
}

func (f *TokenLimiterFilter) EncodeData(c *composite.Context, data []byte, end bool) composite.Status {
	if !end {
		return composite.Continue
	}
	// Settle actual completion tokens once at the end. When the backend returns
	// an OpenAI usage block, use ITS numbers (authoritative) rather than our
	// offline estimate, so metrics and quota stay consistent with the model.
	comp := 0
	prompt := 0
	if v, ok := c.Get(KCompletionToks).(int); ok {
		comp = v
	}
	if !c.IsStream {
		if m := ParsedResponseMap(c); m != nil {
			if u, ok := m["usage"].(map[string]any); ok {
				if pt, ok := u["prompt_tokens"].(float64); ok {
					prompt = int(pt)
				}
				if ct, ok := u["completion_tokens"].(float64); ok {
					comp = int(ct)
				}
			}
		}
	}
	if comp > 0 {
		f.limiter.Settle(comp)
		if m := metricsOf(c); m != nil {
			if prompt > 0 {
				m.setTokens(prompt, comp) // authoritative usage overrides estimate
			} else {
				m.addTokens(0, comp)
			}
		}
	}
	return composite.Continue
}

// ---------------------------------------------------------------------------
// 3. RouteByModel — reads cached model, picks backend, writes into context
//    and a response header so downstream/upstream can observe the decision.
// ---------------------------------------------------------------------------

type RouteByModel struct {
	composite.BaseFilter
	routes   map[string]string // model -> backend
	fallback string
}

func NewRouteByModel(routes map[string]string, fallback string) *RouteByModel {
	return &RouteByModel{routes: routes, fallback: fallback}
}
func (f *RouteByModel) Name() string { return "route-by-model" }

func (f *RouteByModel) DecodeData(c *composite.Context, data []byte, end bool) composite.Status {
	model := modelOf(c)
	backend, ok := f.routes[model]
	if !ok {
		backend = f.fallback
	}
	c.Set(KRouteBackend, backend)
	c.Set(KRouteMatched, ok)
	if m := metricsOf(c); m != nil {
		m.mu.Lock()
		m.Backend = backend
		m.mu.Unlock()
	}
	return composite.Continue
}

func (f *RouteByModel) EncodeHeaders(c *composite.Context, end bool) composite.Status {
	c.ResponseHeaders.Set("X-Routed-Backend", c.GetString(KRouteBackend))
	return composite.Continue
}

// ---------------------------------------------------------------------------
// 4. AsyncGuardrail — starts OUTPUT guard in EncodeHeaders; in EncodeData it
//    waits ONCE for both input & output futures before releasing the chunk.
//    Stream-safe: rejection in a stream appends an error frame, never LocalReply.
// ---------------------------------------------------------------------------

type AsyncGuardrail struct {
	composite.BaseFilter
	checker  AsyncGuardChecker
	timeout  time.Duration
	failOpen bool // on timeout: true=allow, false=reject
}

func NewAsyncGuardrail(checker AsyncGuardChecker, timeout time.Duration, failOpen bool) *AsyncGuardrail {
	return &AsyncGuardrail{checker: checker, timeout: timeout, failOpen: failOpen}
}
func (f *AsyncGuardrail) Name() string { return "async-guardrail" }

func (f *AsyncGuardrail) EncodeHeaders(c *composite.Context, end bool) composite.Status {
	c.Set(KStreamPolicy, map[bool]string{true: "fail-open", false: "fail-closed"}[f.failOpen])
	// NOTE: output guardrail is started in the first EncodeData, because that's
	// when response text actually exists (ResponseBody is still empty here).
	return composite.Continue
}

func (f *AsyncGuardrail) EncodeData(c *composite.Context, data []byte, end bool) composite.Status {
	if gateDone(c) {
		return composite.Continue // 门闩只执行一次
	}
	// Start async OUTPUT guardrail now that we have response text.
	out := NewFuture[GuardVerdict]()
	c.Set(KOutputFuture, out)
	outText := string(data)
	if !c.IsStream && len(c.ResponseBody) > 0 {
		outText = string(c.ResponseBody)
	}
	checker := f.checker
	go func() { out.Resolve(checker.Check("output", outText)) }()

	start := time.Now()
	verdict := f.waitAndCombine(c)
	if m := metricsOf(c); m != nil {
		m.mu.Lock()
		m.GateWaitedMs = time.Since(start).Milliseconds()
		m.IsStream = c.IsStream
		m.mu.Unlock()
	}
	c.Set(KGateDone, true)
	c.Set(KGateVerdict, verdict)

	if verdict.Allowed {
		return composite.Continue
	}
	if m := metricsOf(c); m != nil {
		m.mu.Lock()
		m.GuardRejects++
		m.mu.Unlock()
	}
	// Rejection: stream -> append error frame (never break connection);
	// non-stream -> local reply (response body not released to client yet).
	if c.IsStream {
		// Withhold the offending chunk: the client must NOT see blocked content
		// before the verdict. Only the error frame (+ [DONE]) is released.
		c.ReplaceResponseData([]byte{})
		c.AppendEncodedData([]byte(SSEErrorFrame(verdict.Code, verdict.Reason, c.RequestID)))
		return composite.Continue
	}
	c.Reject(verdict.Code, verdict.Reason)
	// Response headers (200) are already on the wire; we cannot emit a new
	// status line. Replace the buffered body with an error envelope instead of
	// releasing the upstream content.
	env, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code": verdict.Code, "message": verdict.Reason,
			"requestId": c.RequestID, "httpStatus": 200,
		},
	})
	c.ReplaceResponseData(env)
	return composite.StopAndReply
}

func (f *AsyncGuardrail) waitAndCombine(c *composite.Context) *GateVerdict {
	g := &GateVerdict{Allowed: true}
	in, out := inputFutureOf(c), outputFutureOf(c)
	if in != nil {
		v, err := in.Wait(f.timeout)
		if err != nil {
			g.TimedOut = true
			g.Allowed = f.failOpen
			if !f.failOpen {
				g.Code, g.Reason = "GUARDRAIL_TIMEOUT", "input guardrail timeout"
			}
		} else {
			g.Input = v
			if !v.Allowed {
				g.Allowed, g.Code, g.Reason = false, v.Code, v.Reason
			}
		}
	}
	if out != nil && g.Allowed {
		v, err := out.Wait(f.timeout)
		if err != nil {
			g.TimedOut = true
			if !f.failOpen {
				g.Allowed, g.Code, g.Reason = false, "GUARDRAIL_TIMEOUT", "output guardrail timeout"
			}
		} else {
			g.Output = v
			if !v.Allowed {
				g.Allowed, g.Code, g.Reason = false, v.Code, v.Reason
			}
		}
	}
	return g
}

// SSEErrorFrame builds a spec-compliant SSE error + done frame.
func SSEErrorFrame(code, reason, requestID string) string {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"code": code, "message": reason, "requestId": requestID},
		"final": true,
	})
	return "event: error\ndata: " + string(b) + "\n\ndata: [DONE]\n\n"
}

// ---------------------------------------------------------------------------
// 5. MetricsFilter — final (outermost) filter: on destroy emit the snapshot.
//    Also writes standard metrics headers in EncodeHeaders.
// ---------------------------------------------------------------------------

type MetricsSink interface {
	Record(m MetricSnapshot)
}

type MetricsFilter struct {
	composite.BaseFilter
	sink MetricsSink
}

func NewMetricsFilter(sink MetricsSink) *MetricsFilter { return &MetricsFilter{sink: sink} }
func (f *MetricsFilter) Name() string                  { return "metrics" }

func (f *MetricsFilter) EncodeHeaders(c *composite.Context, end bool) composite.Status {
	if m := metricsOf(c); m != nil {
		m.setStatus("UPSTREAM")
	}
	return composite.Continue
}

func (f *MetricsFilter) OnDestroy(c *composite.Context, reason composite.DestroyReason) {
	m := metricsOf(c)
	if m == nil {
		return
	}
	snap := m.Snapshot()
	if snap.Status == "" || snap.Status == "UPSTREAM" {
		if g, ok := c.Get(KGateVerdict).(*GateVerdict); ok && !g.Allowed {
			snap.Status = "GUARD_REJECTED"
		} else {
			snap.Status = string(reason)
		}
	}
	if f.sink != nil {
		f.sink.Record(snap)
	}
}

func (f *MetricsFilter) addChunk(c *composite.Context) {
	if m := metricsOf(c); m != nil {
		m.addChunk()
	}
}

// ParsedResponseMap is a small helper on Context via Values (defined here to
// avoid touching composite.Context).
func ParsedResponseMap(c *composite.Context) map[string]any {
	m, _ := c.Get("parsedResponse").(map[string]any)
	return m
}
