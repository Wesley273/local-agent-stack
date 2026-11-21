package composite

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
)

// ---------------------------------------------------------------------------
// Chain — a single Envoy Golang HTTP filter hosting N sub-filters.
// ---------------------------------------------------------------------------

type Factory func() SubFilter

type spec struct {
	name    string
	factory Factory
	pool    sync.Pool
	mu      sync.Mutex
	gets    int64
	news    int64
	puts    int64
}

// Chain executes sub-filters with Envoy semantics:
//
//	Decode: forward (registration order)   A → B → C
//	Encode: reverse                        C → B → A
//
// plus one big addition: request body buffering/parsing happens ONCE in the
// chain, then the parsed result is published into Context for all downstream
// sub-filters to consume.
type Chain struct {
	specs     []*spec
	warnf     func(string, ...any)
	maxBuffer int
}

func NewChain() *Chain {
	return &Chain{
		warnf:     func(string, ...any) {},
		maxBuffer: 8 << 20,
	}
}

func (c *Chain) SetWarnLogger(fn func(string, ...any)) {
	if fn != nil {
		c.warnf = fn
	}
}

func (c *Chain) SetMaxBodyBuffer(n int) *Chain {
	if n > 0 {
		c.maxBuffer = n
	}
	return c
}

func (c *Chain) Register(name string, f Factory) *Chain {
	s := &spec{name: name, factory: f}
	s.pool.New = func() any {
		s.mu.Lock()
		s.news++
		s.mu.Unlock()
		return f()
	}
	c.specs = append(c.specs, s)
	return c
}

func (c *Chain) Names() []string {
	out := make([]string, 0, len(c.specs))
	for _, s := range c.specs {
		out = append(out, s.name)
	}
	return out
}

func (c *Chain) SortedNames() []string {
	n := c.Names()
	sort.Strings(n)
	return n
}

type runState struct {
	specs []*spec
	insts []SubFilter
}

func (c *Chain) acquireAll() *runState {
	rs := &runState{
		specs: make([]*spec, 0, len(c.specs)),
		insts: make([]SubFilter, 0, len(c.specs)),
	}
	for _, s := range c.specs {
		s.mu.Lock()
		s.gets++
		s.mu.Unlock()
		f := s.pool.Get().(SubFilter)
		rs.specs = append(rs.specs, s)
		rs.insts = append(rs.insts, f)
	}
	return rs
}

func (c *Chain) releaseAll(rs *runState, ctx *Context, reason DestroyReason) {
	for i := len(rs.specs) - 1; i >= 0; i-- {
		rs.insts[i].OnDestroy(ctx, reason)
		rs.insts[i].Reset()
		s := rs.specs[i]
		s.mu.Lock()
		s.puts++
		s.mu.Unlock()
		s.pool.Put(rs.insts[i])
	}
}

// ---------------------------------------------------------------------------
// Request path: DecodeHeaders + DecodeData with ONE-TIME buffer & parse
// ---------------------------------------------------------------------------

// RunDecodeHeaders executes DecodeHeaders in FORWARD order.
//
// If a sub-filter returns StopAndBuffer, the chain marks the start index and
// stops iterating further headers. Callers should then feed body chunks via
// RunDecodeData; the chain will accumulate until endStream, parse once, publish
// to ctx, and resume the remaining sub-filters (including the buffering one).
func (c *Chain) RunDecodeHeaders(ctx *Context, endStream bool) (*Reply, error) {
	rs := c.acquireAll()
	defer c.releaseAll(rs, ctx, ReasonNormal)

	if ctx.MaxBody <= 0 {
		ctx.MaxBody = c.maxBuffer
	}

	for i, f := range rs.insts {
		st := f.DecodeHeaders(ctx, endStream && !c.needsBodyProbe(rs.insts[i:]))
		switch st {
		case StopAndReply:
			return c.makeReply(ctx), nil
		case StopAndBuffer:
			ctx.BeginRequestBodyBuffer()
			ctx.Set("_buffer_from", i)
			c.warnf("[%s] StopAndBuffer -> buffering request body, parse once, publish to ctx", f.Name())
			// Envoy semantics: subsequent decode filters do not see headers yet.
			return nil, nil
		}
	}
	return nil, nil
}

// needsBodyProbe checks whether any remaining filter declared (via BaseFilter
// override convention) interest in the body. We keep this simple: if any
// remaining filter's DecodeData is non-trivial it will be called after headers.
// This is only used to pass an accurate endStream hint to DecodeHeaders.
func (c *Chain) needsBodyProbe(insts []SubFilter) bool {
	return len(insts) > 0
}

// RunDecodeData feeds request body bytes through the chain.
//
// Two modes:
//  1. Buffering mode (ctx.BodyState == BodyCollecting):
//     - accumulate chunks until endStream
//     - on the final chunk: parse ONCE, set ctx.BodyState=BodyParsed,
//     publish ctx.Get("parsedBody"), then call DecodeData ONCE with the full
//     body for every filter from the buffering index onward.
//  2. Pass-through mode:
//     - call DecodeData on every filter in forward order with the live chunk.
func (c *Chain) RunDecodeData(ctx *Context, chunk []byte, endStream bool) (*Reply, error) {
	rs := c.acquireAll()
	defer c.releaseAll(rs, ctx, ReasonNormal)

	if ctx.BodyState == BodyCollecting {
		if err := ctx.AppendRequestChunk(chunk); err != nil {
			ctx.Reject("INVALID_REQUEST", err.Error())
			return c.makeReply(ctx), err
		}
		if !endStream {
			return nil, nil
		}
		// Final chunk: parse exactly once and publish.
		if err := ctx.FinishRequestBodyParse(); err != nil {
			ctx.Reject("INVALID_REQUEST", err.Error())
			return c.makeReply(ctx), err
		}
		ctx.BodyState = BodyParsed
		ctx.Set("_buffer_released", true)

		from := 0
		if v, ok := ctx.Get("_buffer_from").(int); ok {
			from = v
		}
		for i := from; i < len(rs.insts); i++ {
			st := rs.insts[i].DecodeData(ctx, ctx.Body, true)
			if st == StopAndReply {
				return c.makeReply(ctx), nil
			}
			if st == StopAndBuffer {
				c.warnf("[%s] nested StopAndBuffer after body parsed; treating as Continue", rs.insts[i].Name())
			}
		}
		return nil, nil
	}

	// Pass-through: no filter asked to buffer.
	for _, f := range rs.insts {
		st := f.DecodeData(ctx, chunk, endStream)
		if st == StopAndReply {
			return c.makeReply(ctx), nil
		}
		if st == StopAndBuffer {
			// A filter requested buffering mid-stream: start collecting from
			// the NEXT chunk (this chunk already passed earlier filters).
			ctx.BeginRequestBodyBuffer()
			_ = ctx.AppendRequestChunk(chunk)
			ctx.Set("_buffer_from", 0)
			c.warnf("[%s] StopAndBuffer in DecodeData -> collecting remaining chunks", f.Name())
			if endStream {
				if err := ctx.FinishRequestBodyParse(); err != nil {
					ctx.Reject("INVALID_REQUEST", err.Error())
					return c.makeReply(ctx), err
				}
				ctx.BodyState = BodyParsed
			}
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// Response path: reverse order, stream-safe
// ---------------------------------------------------------------------------

// RunEncodeHeaders executes EncodeHeaders in REVERSE order and detects stream.
func (c *Chain) RunEncodeHeaders(ctx *Context, h *HeaderMap, endStream bool) {
	ctx.ResponseHeaders = h
	ctx.ResponseStarted = true
	ctx.DetectStream()

	rs := c.acquireAll()
	defer c.releaseAll(rs, ctx, ReasonNormal)

	for i := len(rs.insts) - 1; i >= 0; i-- {
		st := rs.insts[i].EncodeHeaders(ctx, endStream)
		if st == StopAndReply {
			// §SSE hard rule: once headers are out, a second status line is illegal.
			c.warnf("[%s] StopAndReply ignored (response already started)", rs.insts[i].Name())
		}
	}
}

// RunEncodeData executes EncodeData in REVERSE order.
//
// Returns (outBuffer, appendedFrames). For non-stream responses the chain also
// accumulates into ctx.ResponseBody so ResponseJSON() can parse once and later
// filters (and the caller) can read the same result.
func (c *Chain) RunEncodeData(ctx *Context, data []byte, endStream bool) ([]byte, []byte) {
	rs := c.acquireAll()
	defer c.releaseAll(rs, ctx, ReasonNormal)

	if !ctx.IsStream {
		_ = ctx.AppendResponseChunk(data)
		if endStream {
			_, _ = ctx.ResponseJSON() // parse once at the end
		}
	}

	out := data
	for i := len(rs.insts) - 1; i >= 0; i-- {
		st := rs.insts[i].EncodeData(ctx, out, endStream)
		switch st {
		case StopAndReply:
			if ctx.ResponseStarted {
				c.warnf("[%s] StopAndReply downgraded -> AppendEncodedData (SSE must not break)", rs.insts[i].Name())
			}
		case StopAndBuffer:
			c.warnf("[%s] StopAndBuffer in EncodeData not supported; use non-stream ctx.ResponseJSON()", rs.insts[i].Name())
		}
	}
	return out, ctx.TakeAppended()
}

// RunDestroy calls OnDestroy on all filters in reverse order.
func (c *Chain) RunDestroy(ctx *Context, reason DestroyReason) {
	rs := c.acquireAll()
	c.releaseAll(rs, ctx, reason)
}

// ---------------------------------------------------------------------------
// Local reply
// ---------------------------------------------------------------------------

type Reply struct {
	Status  int
	Headers http.Header
	Body    []byte
}

func (c *Chain) makeReply(ctx *Context) *Reply {
	status := http.StatusInternalServerError
	switch ctx.RejectCode {
	case "AUTH_REQUIRED":
		status = http.StatusUnauthorized
	case "FORBIDDEN":
		status = http.StatusForbidden
	case "RATE_LIMITED":
		status = http.StatusTooManyRequests
	case "AGENT_NOT_FOUND", "MODEL_NOT_FOUND", "TASK_NOT_FOUND":
		status = http.StatusNotFound
	case "GUARDRAIL_REJECTED", "STREAM_CLOSED":
		status = http.StatusOK
	}
	detail := map[string]any{}
	if ctx.RejectDetail != nil {
		detail = ctx.RejectDetail
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":       ctx.RejectCode,
			"message":    ctx.RejectReason,
			"requestId":  ctx.RequestID,
			"httpStatus": status,
			"detail":     detail,
		},
	})
	return &Reply{
		Status:  status,
		Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:    body,
	}
}

// ---------------------------------------------------------------------------
// Pool stats
// ---------------------------------------------------------------------------

type PoolStats struct {
	Gets int64                      `json:"gets"`
	New  int64                      `json:"new"`
	Puts int64                      `json:"puts"`
	Per  map[string]PoolStatsDetail `json:"per"`
}

type PoolStatsDetail struct {
	Gets int64 `json:"gets"`
	New  int64 `json:"new"`
	Puts int64 `json:"puts"`
}

func (c *Chain) PoolStats() PoolStats {
	out := PoolStats{Per: map[string]PoolStatsDetail{}}
	for _, s := range c.specs {
		s.mu.Lock()
		d := PoolStatsDetail{Gets: s.gets, New: s.news, Puts: s.puts}
		s.mu.Unlock()
		out.Gets += d.Gets
		out.New += d.New
		out.Puts += d.Puts
		out.Per[s.name] = d
	}
	return out
}

func (c *Chain) String() string {
	return fmt.Sprintf("composite.Chain{filters:%v}", c.Names())
}
