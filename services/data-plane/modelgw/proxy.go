package modelgw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"local-agent-stack/pkg/a2a"
	"local-agent-stack/services/data-plane/config"
)

// ---------------------------------------------------------------------------
// Gateway — OpenAI-compatible model gateway (contract §2).
//
// Independent upstream timeouts (§2.4 rule 6 / E15):
//   chat (non-stream)  30s   -> UPSTREAM_TIMEOUT (504)
//   embeddings          90s   -> UPSTREAM_TIMEOUT (504)  [the 124844-byte fix]
//   chat (stream)      no hard cap: a long SSE task must not be killed; the
//                      context is cancelled when the stream ends (E24).
//   models             local, no upstream
//
// Context lifetime: the downstream request context is kept alive until the
// response body is fully consumed. callUpstream therefore RETURNS its
// context.CancelFunc instead of deferring it — deferring it would cancel the
// streaming body before the pump goroutine reads it.
//
// The gateway NORMALISES every backend response into the final OpenAI body
// (or a channel of SSE frames) and returns a *Result. The server relays that
// Result through the response filter chain — where the §3.4 SSE guardrail
// logic lives. Conversion failures are surfaced as UPSTREAM_ERROR, never
// silently dropped (§2.4 rule 4).
// ---------------------------------------------------------------------------

const (
	ChatTimeout      = 30 * time.Second
	EmbeddingTimeout = 90 * time.Second
)

// GateError is a gateway-level failure mapped to a §0.3 error code.
type GateError struct {
	Code       a2a.ErrorCode
	Message    string
	RetryAfter int
}

func (e *GateError) Error() string { return string(e.Code) + ": " + e.Message }

// Result is the normalised (already-converted) response the server relays.
// Exactly one of Body / Stream is set.
type Result struct {
	Status     int
	Header     http.Header
	Body       []byte        // non-stream: full final body
	Stream     <-chan []byte // stream: SSE frames / chunks
	TotalToken int           // usage.total_tokens for span + token settle
}

// Gateway holds the model gateway's long-lived dependencies.
type Gateway struct {
	Router   *Router
	Tokens   *TokenLimiter
	Priority *PriorityGate
	Client   *http.Client
	Cfg      config.Provider
}

// NewGateway builds a Gateway.
func NewGateway(cfg config.Provider, limiter *a2a.MemoryLimiter, workers int) *Gateway {
	return &Gateway{
		Router:   NewRouter(cfg),
		Tokens:   NewTokenLimiter(limiter, cfg),
		Priority: NewPriorityGate(workers),
		Cfg:      cfg,
		Client:   &http.Client{}, // per-request context controls the timeout
	}
}

// Close releases the scheduler workers.
func (g *Gateway) Close() { g.Priority.Close() }

// Execute runs one chat / embeddings / models request end-to-end.
func (g *Gateway) Execute(ctx context.Context, ep, path, requestID, appID string,
	priority a2a.Priority, rawBody []byte, inHeaders http.Header) (*Result, *GateError) {

	switch ep {
	case "chat":
		return g.chat(ctx, path, requestID, appID, priority, rawBody, inHeaders)
	case "embeddings":
		return g.embeddings(ctx, path, requestID, appID, priority, rawBody, inHeaders)
	case "models":
		return &Result{
			Status: http.StatusOK,
			Header: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
			Body:   ListModels(g.Cfg.Current()),
		}, nil
	default:
		return nil, &GateError{Code: a2a.CodeInvalidRequest, Message: "unknown model endpoint " + ep}
	}
}

func (g *Gateway) chat(ctx context.Context, path, requestID, appID string,
	priority a2a.Priority, rawBody []byte, inHeaders http.Header) (*Result, *GateError) {

	r, err := ParseChatRequest(rawBody)
	if err != nil {
		return nil, &GateError{Code: a2a.CodeInvalidRequest, Message: err.Error()}
	}
	route, ok := g.Router.Resolve(r.Model)
	if !ok {
		return nil, &GateError{Code: a2a.CodeModelNotFound,
			Message: "model not found or no backend available: " + r.Model}
	}
	backend := g.Router.Pick(r.Model, route.Backends)
	upstreamURL := joinURL(backend, path)

	// Token 预扣 (§2.4 rule 3).
	tokenID, dec := g.Tokens.Reserve(appID, EstimateTokens(r))
	if !dec.Allowed {
		return nil, &GateError{Code: a2a.CodeRateLimited, Message: dec.Reason, RetryAfter: dec.RetryAfter}
	}

	upBody, err := BuildUpstreamBody(r)
	if err != nil {
		g.Tokens.Settle(tokenID, 0)
		return nil, &GateError{Code: a2a.CodeUpstreamError, Message: err.Error()}
	}

	// For non-stream we cap the whole exchange at 30s; for stream we keep the
	// context alive for the whole SSE exchange (no hard cap).
	timeout := ChatTimeout
	if r.Stream {
		timeout = 0
	}

	var doErr *GateError
	var backendResp *http.Response
	var cancelUp context.CancelFunc
	ran := g.Priority.Run(priority, func() {
		backendResp, cancelUp, doErr = g.callUpstream(ctx, timeout, http.MethodPost, upstreamURL, upBody, inHeaders)
	})
	if !ran {
		g.Tokens.Settle(tokenID, 0)
		return nil, &GateError{Code: a2a.CodeInternalError, Message: "priority queue full"}
	}
	if doErr != nil {
		if cancelUp != nil {
			cancelUp()
		}
		g.Tokens.Settle(tokenID, 0)
		return nil, doErr
	}

	if r.Stream {
		return g.streamResult(ctx, backendResp, cancelUp, r.Model, tokenID)
	}
	defer cancelUp()
	defer backendResp.Body.Close()
	return g.nonStreamResult(backendResp, r.Model, tokenID)
}

func (g *Gateway) embeddings(ctx context.Context, path, requestID, appID string,
	priority a2a.Priority, rawBody []byte, inHeaders http.Header) (*Result, *GateError) {

	model, inputCount, err := ParseEmbeddingRequest(rawBody)
	if err != nil {
		return nil, &GateError{Code: a2a.CodeInvalidRequest, Message: err.Error()}
	}
	route, ok := g.Router.Resolve(model)
	if !ok {
		return nil, &GateError{Code: a2a.CodeModelNotFound,
			Message: "model not found or no backend available: " + model}
	}
	backend := g.Router.Pick(model, route.Backends)
	upstreamURL := joinURL(backend, path)

	tokenID, dec := g.Tokens.Reserve(appID, inputCount)
	if !dec.Allowed {
		return nil, &GateError{Code: a2a.CodeRateLimited, Message: dec.Reason, RetryAfter: dec.RetryAfter}
	}

	var doErr *GateError
	var backendResp *http.Response
	var cancelUp context.CancelFunc
	ran := g.Priority.Run(priority, func() {
		// Independent, LONGER timeout (E15).
		backendResp, cancelUp, doErr = g.callUpstream(ctx, EmbeddingTimeout, http.MethodPost, upstreamURL, rawBody, inHeaders)
	})
	if !ran {
		g.Tokens.Settle(tokenID, 0)
		return nil, &GateError{Code: a2a.CodeInternalError, Message: "priority queue full"}
	}
	if doErr != nil {
		if cancelUp != nil {
			cancelUp()
		}
		g.Tokens.Settle(tokenID, 0)
		return nil, doErr
	}
	defer cancelUp()
	defer backendResp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(backendResp.Body, 16<<20))
	if err != nil {
		g.Tokens.Settle(tokenID, 0)
		return nil, &GateError{Code: a2a.CodeUpstreamError, Message: "read upstream body: " + err.Error()}
	}
	if backendResp.StatusCode < 200 || backendResp.StatusCode >= 300 {
		g.Tokens.Settle(tokenID, 0)
		return nil, &GateError{Code: a2a.CodeUpstreamError,
			Message: fmt.Sprintf("embeddings upstream returned HTTP %d", backendResp.StatusCode)}
	}
	out, err := ConvertEmbeddingResponse(raw, model, inputCount)
	if err != nil {
		g.Tokens.Settle(tokenID, 0)
		return nil, &GateError{Code: a2a.CodeUpstreamError, Message: err.Error()}
	}
	g.Tokens.Settle(tokenID, out.Usage.TotalTokens) // 结算回填
	body, _ := json.Marshal(out)
	return &Result{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:   body, TotalToken: out.Usage.TotalTokens,
	}, nil
}

// nonStreamResult buffers the backend chat body, converts it, and settles.
func (g *Gateway) nonStreamResult(resp *http.Response, model, tokenID string) (*Result, *GateError) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		g.Tokens.Settle(tokenID, 0)
		return nil, &GateError{Code: a2a.CodeUpstreamError, Message: "read upstream body: " + err.Error()}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		g.Tokens.Settle(tokenID, 0)
		return nil, &GateError{Code: a2a.CodeUpstreamError,
			Message: fmt.Sprintf("chat upstream returned HTTP %d", resp.StatusCode)}
	}
	out, err := ConvertChatResponse(raw, model)
	if err != nil {
		g.Tokens.Settle(tokenID, 0)
		return nil, &GateError{Code: a2a.CodeUpstreamError, Message: err.Error()}
	}
	g.Tokens.Settle(tokenID, out.Usage.TotalTokens)
	body, _ := json.Marshal(out)
	return &Result{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:   body, TotalToken: out.Usage.TotalTokens,
	}, nil
}

// streamResult turns the upstream response into a channel of SSE frames.
//   - Upstream already streams text/event-stream: pass chunks through.
//   - Legacy JSON backend: convert to ChatResponse, split into chunk frames
//   - [DONE] (§2.1 / E14).
//
// cancelUp is invoked when the pump finishes so the request context is not
// cancelled prematurely.
func (g *Gateway) streamResult(ctx context.Context, resp *http.Response,
	cancelUp context.CancelFunc, model, tokenID string) (*Result, *GateError) {

	ch := make(chan []byte, 32)
	isSSE := containsCT(resp.Header.Get("Content-Type"), "text/event-stream")

	if !isSSE {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			cancelUp()
			close(ch)
			g.Tokens.Settle(tokenID, 0)
			return nil, &GateError{Code: a2a.CodeUpstreamError, Message: err.Error()}
		}
		frames, err := SplitSSEChunks(raw, model)
		if err != nil {
			cancelUp()
			close(ch)
			g.Tokens.Settle(tokenID, 0)
			return nil, &GateError{Code: a2a.CodeUpstreamError, Message: err.Error()}
		}
		go func() {
			defer close(ch)
			defer cancelUp()
			total := 0
			for _, f := range frames {
				total += len([]rune(f))
				select {
				case ch <- []byte(f):
				case <-ctx.Done():
					g.Tokens.Settle(tokenID, 0)
					return
				}
			}
			g.Tokens.Settle(tokenID, total/4) // rough chat-token estimate for streams
		}()
		return &Result{
			Status: http.StatusOK,
			Header: http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
			Stream: ch,
		}, nil
	}

	go func() {
		defer close(ch)
		defer cancelUp()
		defer resp.Body.Close()
		buf := make([]byte, 4096)
		var acc bytes.Buffer
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				acc.Write(chunk)
				select {
				case ch <- chunk:
				case <-ctx.Done():
					g.Tokens.Settle(tokenID, 0)
					return
				}
			}
			if err != nil {
				break
			}
		}
		total := ParseUsageFromSSE(acc.Bytes())
		g.Tokens.Settle(tokenID, total)
	}()
	return &Result{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
		Stream: ch,
	}, nil
}

// callUpstream performs the downstream HTTP request with a per-endpoint
// timeout. timeout<=0 means "no hard deadline" (streaming SSE). It returns
// the context.CancelFunc so the caller can keep the context alive until the
// body is fully consumed — deferring cancel here would break streaming.
func (g *Gateway) callUpstream(parent context.Context, timeout time.Duration, method, url string,
	body []byte, inHeaders http.Header) (*http.Response, context.CancelFunc, *GateError) {

	var ctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, nil, &GateError{Code: a2a.CodeUpstreamError, Message: "build upstream request: " + err.Error()}
	}
	copyForwardHeaders(req, inHeaders)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.ContentLength = int64(len(body))

	resp, err := g.Client.Do(req)
	if err != nil {
		cancel()
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
			return nil, nil, &GateError{Code: a2a.CodeUpstreamTimeout, Message: "upstream timeout after " + timeout.String()}
		}
		return nil, nil, &GateError{Code: a2a.CodeUpstreamError, Message: "upstream connection failed: " + err.Error()}
	}
	return resp, cancel, nil
}

// copyForwardHeaders forwards standard headers (§0.2) and strips data-plane
// internal pseudo-headers.
func copyForwardHeaders(dst *http.Request, src http.Header) {
	internal := map[string]bool{
		"x-filter-error-code": true, "x-filter-error-message": true,
		"x-dp-upstream": true, "x-dp-agent-id": true, "x-dp-model": true,
		"x-dp-path": true, "x-dp-method": true, "x-dp-host": true,
		"x-dp-model-ep": true, "x-dp-retry-after": true, "x-dp-streaming": true,
		"x-dp-guard-rejected": true, "x-dp-rpc-method": true,
		"x-dp-local": true, "x-dp-backend": true,
	}
	for k, vs := range src {
		lk := lower(k)
		if internal[lk] || lk == "host" || lk == "content-length" {
			continue
		}
		for _, v := range vs {
			dst.Header.Add(k, v)
		}
	}
}

func joinURL(base, path string) string {
	if len(base) > 0 && base[len(base)-1] == '/' && len(path) > 0 && path[0] == '/' {
		return base[:len(base)-1] + path
	}
	return base + path
}

func containsCT(s, sub string) bool { return len(sub) == 0 || indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
