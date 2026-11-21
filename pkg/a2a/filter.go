// Package a2a — filter chain abstractions.
//
// This is a pure-Go reimplementation of the Envoy Golang filter API semantics
// used in the resume (DecodeHeaders / DecodeData / EncodeHeaders / EncodeData /
// SendLocalReply). The production system used Envoy + cgo plugins; here we
// model the same phases with net/http so the whole stack is offline-buildable,
// and we explicitly fix the SSE truncation bug (§3.4 of the contract).
package a2a

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// HeaderMap — case-insensitive header abstraction (mirrors Envoy HeaderMap)
// ---------------------------------------------------------------------------

// HeaderMap is a case-insensitive multi-value header map.
type HeaderMap struct {
	m map[string][]string
}

// NewHeaderMap builds a HeaderMap from an http.Header.
func NewHeaderMap(h http.Header) *HeaderMap {
	hm := &HeaderMap{m: make(map[string][]string, len(h))}
	for k, vs := range h {
		for _, v := range vs {
			hm.Add(k, v)
		}
	}
	return hm
}

// NewHeaderMapEmpty builds an empty HeaderMap.
func NewHeaderMapEmpty() *HeaderMap { return &HeaderMap{m: map[string][]string{}} }

func canon(k string) string { return strings.ToLower(strings.TrimSpace(k)) }

// Get returns the first value for k (case-insensitive).
func (h *HeaderMap) Get(k string) string {
	vs := h.m[canon(k)]
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}

// Values returns all values for k.
func (h *HeaderMap) Values(k string) []string { return append([]string(nil), h.m[canon(k)]...) }

// Set replaces all values for k.
func (h *HeaderMap) Set(k, v string) *HeaderMap { h.m[canon(k)] = []string{v}; return h }

// Add appends a value for k.
func (h *HeaderMap) Add(k, v string) *HeaderMap {
	c := canon(k)
	h.m[c] = append(h.m[c], v)
	return h
}

// Del removes k.
func (h *HeaderMap) Del(k string) *HeaderMap { delete(h.m, canon(k)); return h }

// Has reports whether k exists.
func (h *HeaderMap) Has(k string) bool { _, ok := h.m[canon(k)]; return ok }

// Clone deep-copies the map.
func (h *HeaderMap) Clone() *HeaderMap {
	out := NewHeaderMapEmpty()
	for k, vs := range h.m {
		out.m[k] = append([]string(nil), vs...)
	}
	return out
}

// ApplyTo writes the map into an http.Header.
func (h *HeaderMap) ApplyTo(dst http.Header) {
	for k, vs := range h.m {
		dst.Del(k)
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// ---------------------------------------------------------------------------
// Filter status & lifecycle
// ---------------------------------------------------------------------------

// FilterStatus is returned by each filter phase.
type FilterStatus int

const (
	// Continue proceeds to the next filter / upstream.
	Continue FilterStatus = iota
	// StopAndBuffer pauses to accumulate the full body (e.g. input guardrail).
	StopAndBuffer
	// StopAndReply emits an immediate HTTP error response. ONLY legal BEFORE
	// the response has started (see §3.4). LocalReply is an alias.
	StopAndReply
	// LocalReply is the Envoy-compatible alias for StopAndReply.
	LocalReply
)

func (s FilterStatus) String() string {
	switch s {
	case Continue:
		return "Continue"
	case StopAndBuffer:
		return "StopAndBuffer"
	case StopAndReply, LocalReply:
		return "StopAndReply"
	}
	return "Unknown"
}

// IsReply reports whether the status requests an immediate HTTP response.
func (s FilterStatus) IsReply() bool { return s == StopAndReply || s == LocalReply }

// DestroyReason describes why a filter chain is torn down.
type DestroyReason string

const (
	ReasonNormal     DestroyReason = "normal"
	ReasonUpstream   DestroyReason = "upstream_error"
	ReasonClientGone DestroyReason = "client_gone"
)

// FilterContext is the per-request state shared by all filters.
type FilterContext struct {
	RequestID      string
	TraceID        string
	Span           *Span
	AppID          string
	AgentID        string
	Model          string
	Method         string
	Path           string
	RequestHeaders *HeaderMap
	ResponseHeaders *HeaderMap
	RequestBody    []byte
	ResponseStarted bool // gate for the SendLocalReply fix
	SpanStore      *SpanStore

	// AppendEncodedData collects bytes to append to the CURRENT response
	// stream (equivalent to Envoy's AddEncodedData). It never writes a new
	// HTTP status line.
	appended []byte
	mu       sync.Mutex
}

// AppendEncodedData queues bytes to be appended to the in-flight response.
// This is the CORRECT way to inject a guardrail error into an SSE stream.
func (c *FilterContext) AppendEncodedData(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.appended = append(c.appended, b...)
}

// TakeAppended returns and clears queued appended bytes.
func (c *FilterContext) TakeAppended() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.appended
	c.appended = nil
	return b
}

// StreamFilter mirrors the Envoy Golang StreamFilter interface.
type StreamFilter interface {
	Name() string
	DecodeHeaders(h *HeaderMap, endStream bool) FilterStatus
	DecodeData(buf []byte, endStream bool) FilterStatus
	EncodeHeaders(h *HeaderMap, endStream bool) FilterStatus
	EncodeData(buf []byte, endStream bool) FilterStatus
	OnDestroy(reason DestroyReason)
	Reset() // clear per-request state before returning to the pool
}

// ---------------------------------------------------------------------------
// FilterChain — pooled filters (contract §3.3, Envoy OOM fix)
// ---------------------------------------------------------------------------

// FilterFactory creates a fresh filter instance (only on pool miss).
type FilterFactory func() StreamFilter

type filterSpec struct {
	name    string
	factory FilterFactory
	pool    sync.Pool
	stats   *PoolStats
}

// PoolStats tracks how many instances were newly allocated vs reused.
type PoolStats struct {
	Gets int64
	New  int64
	Puts int64
}

// FilterChain holds an ordered list of filter specifications.
type FilterChain struct {
	specs   []*filterSpec
	global  *PoolStats
	mu      sync.Mutex
	logWarn func(string)
}

// NewFilterChain creates an empty chain.
func NewFilterChain() *FilterChain {
	return &FilterChain{global: &PoolStats{}, logWarn: func(string) {}}
}

// SetWarnLogger installs a warn logger (for the §3.4 downgrade notice).
func (fc *FilterChain) SetWarnLogger(fn func(string)) { fc.logWarn = fn }

// Register appends a filter spec. Order is preserved (contract §3.2).
func (fc *FilterChain) Register(name string, f FilterFactory) *FilterChain {
	fs := &filterSpec{name: name, factory: f, stats: &PoolStats{}}
	fs.pool.New = func() any {
		atomic.AddInt64(&fs.stats.New, 1)
		atomic.AddInt64(&fc.global.New, 1)
		return f()
	}
	fc.specs = append(fc.specs, fs)
	return fc
}

// acquire gets a pooled instance (never per-request new when possible).
func (fc *FilterChain) acquire(fs *filterSpec) StreamFilter {
	atomic.AddInt64(&fs.stats.Gets, 1)
	atomic.AddInt64(&fc.global.Gets, 1)
	return fs.pool.Get().(StreamFilter)
}

// release resets and returns an instance to its pool.
func (fc *FilterChain) release(fs *filterSpec, f StreamFilter) {
	f.Reset()
	atomic.AddInt64(&fs.stats.Puts, 1)
	atomic.AddInt64(&fc.global.Puts, 1)
	fs.pool.Put(f)
}

// PoolStatsSnapshot is exposed by GET /v1/admin/pool.
type PoolStatsSnapshot struct {
	Gets int64                      `json:"gets"`
	New  int64                      `json:"new"`
	Puts int64                      `json:"puts"`
	Per  map[string]PoolStatsDetail `json:"per"`
}

// PoolStatsDetail is per-filter pool counters.
type PoolStatsDetail struct {
	Gets int64 `json:"gets"`
	New  int64 `json:"new"`
	Puts int64 `json:"puts"`
}

// PoolStats returns aggregated and per-filter counters.
func (fc *FilterChain) PoolStats() PoolStatsSnapshot {
	per := map[string]PoolStatsDetail{}
	for _, fs := range fc.specs {
		per[fs.name] = PoolStatsDetail{
			Gets: atomic.LoadInt64(&fs.stats.Gets),
			New:  atomic.LoadInt64(&fs.stats.New),
			Puts: atomic.LoadInt64(&fs.stats.Puts),
		}
	}
	return PoolStatsSnapshot{
		Gets: atomic.LoadInt64(&fc.global.Gets),
		New:  atomic.LoadInt64(&fc.global.New),
		Puts: atomic.LoadInt64(&fc.global.Puts),
		Per:  per,
	}
}

// ---------------------------------------------------------------------------
// Chain execution
// ---------------------------------------------------------------------------

// Reply is an immediate error response produced before the response starts.
type Reply struct {
	Status int
	Body   []byte
	Header http.Header
}

// RunDecode executes DecodeHeaders then DecodeData across the chain.
// Returns a Reply when a filter legitimately short-circuits before upstream.
func (fc *FilterChain) RunDecode(ctx *FilterContext, body []byte, endStream bool) (*Reply, error) {
	active := make([]*filterSpec, 0, len(fc.specs))
	instances := make([]StreamFilter, 0, len(fc.specs))
	defer func() {
		// release in reverse order
		for i := len(active) - 1; i >= 0; i-- {
			fc.release(active[i], instances[i])
		}
	}()

	for _, fs := range fc.specs {
		f := fc.acquire(fs)
		active = append(active, fs)
		instances = append(instances, f)
		ctx.RequestBody = body
		st := f.DecodeHeaders(ctx.RequestHeaders, endStream && len(body) == 0)
		if st.IsReply() {
			if ctx.ResponseStarted {
				// §3.4: downgrade, never write a second HTTP response.
				fc.logWarn(fmt.Sprintf("[%s] StopAndReply after ResponseStarted downgraded to AppendEncodedData", fs.name))
				ctx.AppendEncodedData([]byte(SSEErrorFrame(CodeInternalError, "reply forbidden after stream start", ctx.RequestID)))
				return nil, nil
			}
			return replyFromFilter(fs.name, ctx), nil
		}
	}
	// DecodeData in order (buffering filters see the full body).
	for i, fs := range active {
		f := instances[i]
		st := f.DecodeData(body, endStream)
		if st.IsReply() {
			if ctx.ResponseStarted {
				fc.logWarn(fmt.Sprintf("[%s] StopAndReply after ResponseStarted downgraded", fs.name))
				ctx.AppendEncodedData([]byte(SSEErrorFrame(CodeInternalError, "reply forbidden after stream start", ctx.RequestID)))
				return nil, nil
			}
			return replyFromFilter(fs.name, ctx), nil
		}
	}
	return nil, nil
}

// RunEncodeHeaders executes all EncodeHeaders phases.
func (fc *FilterChain) RunEncodeHeaders(ctx *FilterContext, h *HeaderMap, endStream bool) {
	ctx.ResponseHeaders = h
	for _, fs := range fc.specs {
		f := fc.acquire(fs)
		f.EncodeHeaders(h, endStream)
		fc.release(fs, f)
	}
}

// RunEncodeData executes all EncodeData phases and returns the (possibly
// modified) buffer plus any appended bytes from §3.4 guardrail handling.
func (fc *FilterChain) RunEncodeData(ctx *FilterContext, buf []byte, endStream bool) ([]byte, []byte) {
	out := buf
	for _, fs := range fc.specs {
		f := fc.acquire(fs)
		st := f.EncodeData(out, endStream)
		if st.IsReply() {
			// HARD RULE §3.4: ignore LocalReply entirely once streaming began.
			fc.logWarn(fmt.Sprintf("[%s] LocalReply ignored (ResponseStarted=%v) — SSE would break", fs.name, ctx.ResponseStarted))
		}
		fc.release(fs, f)
	}
	return out, ctx.TakeAppended()
}

// DestroyAll calls OnDestroy on pooled instances (used for upstream failures).
func (fc *FilterChain) DestroyAll(reason DestroyReason) {
	for _, fs := range fc.specs {
		f := fc.acquire(fs)
		f.OnDestroy(reason)
		fc.release(fs, f)
	}
}

func replyFromFilter(name string, ctx *FilterContext) *Reply {
	code := CodeInvalidRequest
	msg := "rejected by filter " + name
	if v := ctx.RequestHeaders.Get("X-Filter-Error-Code"); v != "" {
		code = ErrorCode(v)
	}
	if v := ctx.RequestHeaders.Get("X-Filter-Error-Message"); v != "" {
		msg = v
	}
	env := NewError(code, msg, ctx.RequestID)
	return &Reply{
		Status: HTTPStatus(code),
		Body:   []byte(JSONLine(env)),
		Header: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
	}
}

// ---------------------------------------------------------------------------
// BaseFilter — embeddable default implementation (all phases = Continue)
// ---------------------------------------------------------------------------

// BaseFilter provides no-op defaults; concrete filters embed it and override.
type BaseFilter struct{}

// Name is overridden by concrete filters.
func (b *BaseFilter) Name() string { return "base" }

// DecodeHeaders defaults to Continue.
func (b *BaseFilter) DecodeHeaders(h *HeaderMap, endStream bool) FilterStatus { return Continue }

// DecodeData defaults to Continue.
func (b *BaseFilter) DecodeData(buf []byte, endStream bool) FilterStatus { return Continue }

// EncodeHeaders defaults to Continue.
func (b *BaseFilter) EncodeHeaders(h *HeaderMap, endStream bool) FilterStatus { return Continue }

// EncodeData defaults to Continue.
func (b *BaseFilter) EncodeData(buf []byte, endStream bool) FilterStatus { return Continue }

// OnDestroy defaults to no-op.
func (b *BaseFilter) OnDestroy(reason DestroyReason) {}

// Reset defaults to no-op.
func (b *BaseFilter) Reset() {}

// ---------------------------------------------------------------------------
// Priority scheduling helper (contract §2.4)
// ---------------------------------------------------------------------------

// Priority is derived from X-Priority.
type Priority int

const (
	PriorityLow    Priority = 0
	PriorityNormal Priority = 1
	PriorityHigh   Priority = 2
)

// ParsePriority maps the header to a queue tier.
func ParsePriority(s string) Priority {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "high":
		return PriorityHigh
	case "low":
		return PriorityLow
	default:
		return PriorityNormal
	}
}

// PriorityScheduler is a bounded 3-tier queue with weighted 3:2:1 rotation
// so low-priority requests never starve.
type PriorityScheduler struct {
	queues [3]chan func()
	stop   chan struct{}
	wg     sync.WaitGroup
	quota  [3]int
}

// NewPriorityScheduler starts worker goroutines for each tier.
func NewPriorityScheduler(capacity int, workers int) *PriorityScheduler {
	if capacity <= 0 {
		capacity = 64
	}
	if workers <= 0 {
		workers = 4
	}
	ps := &PriorityScheduler{stop: make(chan struct{}), quota: [3]int{1, 2, 3}}
	for i := 0; i < 3; i++ {
		ps.queues[i] = make(chan func(), capacity)
	}
	for w := 0; w < workers; w++ {
		ps.wg.Add(1)
		go ps.worker(w)
	}
	return ps
}

func (ps *PriorityScheduler) worker(id int) {
	defer ps.wg.Done()
	// Weighted rotation: high(2):normal(1):low(0) with quotas 3:2:1.
	tiers := []Priority{PriorityHigh, PriorityHigh, PriorityHigh, PriorityNormal, PriorityNormal, PriorityLow}
	for {
		select {
		case <-ps.stop:
			return
		default:
		}
		got := false
		for _, t := range tiers {
			select {
			case job := <-ps.queues[t]:
				job()
				got = true
			case <-ps.stop:
				return
			default:
			}
			if got {
				break
			}
		}
		if !got {
			// block on the highest tier with work, with a stop escape
			select {
			case job := <-ps.queues[PriorityHigh]:
				job()
			case job := <-ps.queues[PriorityNormal]:
				job()
			case job := <-ps.queues[PriorityLow]:
				job()
			case <-ps.stop:
				return
			}
		}
	}
}

// Submit enqueues a job at its priority. Returns an error if full.
func (ps *PriorityScheduler) Submit(p Priority, job func()) error {
	select {
	case ps.queues[p] <- job:
		return nil
	default:
		return errors.New("priority queue full")
	}
}

// Close stops all workers.
func (ps *PriorityScheduler) Close() {
	close(ps.stop)
	ps.wg.Wait()
}

// SortedFilterNames returns registered filter names (helper for diagnostics).
func (fc *FilterChain) SortedFilterNames() []string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	names := make([]string, 0, len(fc.specs))
	for _, s := range fc.specs {
		names = append(names, s.name)
	}
	sort.Strings(names)
	return names
}
