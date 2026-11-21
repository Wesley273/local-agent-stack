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

// RunDecodeHeaders is a compatibility wrapper around a fresh Session.
// Deprecated: use Chain.NewSession(ctx).RunDecodeHeaders(...) so the same
// sub-filter instances serve the whole stream AND so StopAndBuffer is
// propagated to the caller via Decision.
func (c *Chain) RunDecodeHeaders(ctx *Context, endStream bool) (*Reply, error) {
	s := c.NewSession(ctx)
	ctx.Set("_compat_session", s)
	d, err := s.RunDecodeHeaders(endStream)
	if d != nil && d.Action == FlowStopAndReply {
		return d.Reply, err
	}
	return nil, err
}

// RunDecodeData is a compatibility wrapper around the Session created by
// RunDecodeHeaders. Deprecated: use Session.RunDecodeData for the outer
// FlowAction (FlowStopAndBuffer / FlowReady) needed by Envoy.
func (c *Chain) RunDecodeData(ctx *Context, chunk []byte, endStream bool) (*Reply, error) {
	v := ctx.Get("_compat_session")
	if v == nil {
		return nil, nil
	}
	s, ok := v.(*Session)
	if !ok {
		return nil, nil
	}
	d, err := s.RunDecodeData(chunk, endStream)
	if d != nil && d.Action == FlowStopAndReply {
		return d.Reply, err
	}
	if d != nil && d.Action == FlowReady {
		s.Close(ReasonNormal)
	}
	return nil, err
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
