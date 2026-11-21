// Package composite implements a single Envoy Golang HTTP filter that hosts an
// internal chain of sub-filters (a "composite filter"). It mimics Envoy's native
// HTTP filter chain semantics:
//
//   - Decode (request) runs in registration order: A → B → C
//   - Encode (response) runs in reverse order: C → B → A
//   - DecodeHeaders/EncodeHeaders once; DecodeData/EncodeData per chunk
//   - StopAndBuffer pauses the chain and accumulates full body
//   - Local reply before response start; AppendEncodedData after response start
//
// The key value it adds over Envoy's native chain: ONE shared Context with a
// ONE-TIME body buffer and ONE-TIME JSON parse that every sub-filter consumes.
// Native Envoy filters only see bytes between filters, so each filter that needs
// JSON must buffer and parse again. Here, the first filter that needs the body
// triggers StopAndBuffer; the chain collects the full body, parses it once,
// publishes it into ctx, and then resumes downstream filters which read the
// cached result instead of re-parsing.
package composite

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
)

// Status is the per-phase decision of a sub-filter.
type Status int

const (
	Continue      Status = iota
	StopAndBuffer        // pause chain, buffer the entire body, then resume with ctx.Body ready
	StopAndReply         // emit an immediate local reply (only legal before response started)
)

func (s Status) String() string {
	switch s {
	case Continue:
		return "Continue"
	case StopAndBuffer:
		return "StopAndBuffer"
	case StopAndReply:
		return "StopAndReply"
	}
	return "Unknown"
}

type DestroyReason string

const (
	ReasonNormal        DestroyReason = "normal"
	ReasonUpstreamError DestroyReason = "upstream_error"
	ReasonClientGone    DestroyReason = "client_gone"
)

// ---------------------------------------------------------------------------
// HeaderMap — case-insensitive, mirrors Envoy HeaderMap semantics
// ---------------------------------------------------------------------------

type HeaderMap struct {
	m map[string][]string
}

func NewHeaderMap(h http.Header) *HeaderMap {
	hm := NewHeaderMapEmpty()
	for k, vs := range h {
		for _, v := range vs {
			hm.Add(k, v)
		}
	}
	return hm
}

func NewHeaderMapEmpty() *HeaderMap { return &HeaderMap{m: map[string][]string{}} }

func canon(k string) string { return strings.ToLower(strings.TrimSpace(k)) }

func (h *HeaderMap) Get(k string) string {
	vs := h.m[canon(k)]
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}

func (h *HeaderMap) Values(k string) []string { return append([]string(nil), h.m[canon(k)]...) }

func (h *HeaderMap) Set(k, v string) *HeaderMap { h.m[canon(k)] = []string{v}; return h }

func (h *HeaderMap) Add(k, v string) *HeaderMap {
	c := canon(k)
	h.m[c] = append(h.m[c], v)
	return h
}

func (h *HeaderMap) Del(k string) *HeaderMap { delete(h.m, canon(k)); return h }

func (h *HeaderMap) Has(k string) bool { _, ok := h.m[canon(k)]; return ok }

func (h *HeaderMap) Clone() *HeaderMap {
	out := NewHeaderMapEmpty()
	for k, vs := range h.m {
		out.m[k] = append([]string(nil), vs...)
	}
	return out
}

func (h *HeaderMap) ApplyTo(dst http.Header) {
	for k, vs := range h.m {
		dst.Del(k)
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// ---------------------------------------------------------------------------
// Request body buffer state machine
// ---------------------------------------------------------------------------

// BodyBufferState is the lifecycle of the request body inside the composite.
//
//	None      -> no filter has requested buffering yet
//	Collecting-> a filter called StopAndBuffer; we accumulate raw bytes
//	Parsed    -> full body received, parsed once, published into ctx
type BodyBufferState int

const (
	BodyNone BodyBufferState = iota
	BodyCollecting
	BodyParsed
)

func (s BodyBufferState) String() string {
	switch s {
	case BodyNone:
		return "None"
	case BodyCollecting:
		return "Collecting"
	case BodyParsed:
		return "Parsed"
	}
	return "Unknown"
}

// ErrBodyTooLarge is returned when the request body exceeds the configured cap.
var ErrBodyTooLarge = errors.New("composite: buffered request body too large")

// ---------------------------------------------------------------------------
// Context — the shared per-request state (the main reason to composite)
// ---------------------------------------------------------------------------

type Context struct {
	RequestID string
	Method    string
	Path      string
	RawQuery  string

	RequestHeaders  *HeaderMap
	ResponseHeaders *HeaderMap

	// ---- request body (buffered exactly once) ----
	BodyState  BodyBufferState
	Body       []byte
	bodyParsed bool
	bodyJSON   any
	bodyErr    error
	MaxBody    int

	// ---- response body (only for NON-stream responses) ----
	IsStream        bool // decided at EncodeHeaders
	ResponseStarted bool // set true once EncodeHeaders ran
	ResponseBody    []byte
	responseParsed  bool
	responseJSON    any
	responseErr     error
	MaxResponse     int

	// ---- local reply / appended stream data ----
	RejectCode   string
	RejectReason string
	RejectDetail map[string]any

	appended [][]byte
	appendMu sync.Mutex

	// responseOverride lets a sub-filter REPLACE the current EncodeData chunk.
	// nil = pass through; non-nil (including empty) = use override bytes.
	// This is how a guardrail can withhold a rejected chunk while still
	// appending an SSE error frame (data must not leak before the verdict).
	respOverride    []byte
	respOverrideSet bool
	overrideMu      sync.Mutex

	// ---- shared values between sub-filters ----
	Values map[string]any

	// ---- metrics for tests/admin ----
	statsMu      sync.Mutex
	parseCount   int
	bufferChunks int
}

func NewContext(method, path, rawQuery, requestID string, h *HeaderMap) *Context {
	if h == nil {
		h = NewHeaderMapEmpty()
	}
	return &Context{
		RequestID:       requestID,
		Method:          method,
		Path:            path,
		RawQuery:        rawQuery,
		RequestHeaders:  h.Clone(),
		ResponseHeaders: NewHeaderMapEmpty(),
		BodyState:       BodyNone,
		MaxBody:         8 << 20, // 8MB
		MaxResponse:     8 << 20,
		Values:          map[string]any{},
	}
}

func (c *Context) Set(k string, v any) { c.Values[k] = v }
func (c *Context) Get(k string) any    { return c.Values[k] }
func (c *Context) GetString(k string) string {
	if s, ok := c.Values[k].(string); ok {
		return s
	}
	return ""
}

// BeginRequestBodyBufferswitches the context into Collecting mode. Called by
// the chain when a sub-filter returns StopAndBuffer during DecodeHeaders or
// DecodeData.
func (c *Context) BeginRequestBodyBuffer() {
	if c.BodyState == BodyNone {
		c.BodyState = BodyCollecting
		c.Body = c.Body[:0]
	}
}

// AppendRequestChunk accumulates raw request bytes. Returns ErrBodyTooLarge if
// the configured cap is exceeded.
func (c *Context) AppendRequestChunk(chunk []byte) error {
	c.statsMu.Lock()
	c.bufferChunks++
	c.statsMu.Unlock()
	if c.MaxBody > 0 && len(c.Body)+len(chunk) > c.MaxBody {
		return ErrBodyTooLarge
	}
	c.Body = append(c.Body, chunk...)
	return nil
}

// FinishRequestBodyParse parses the full buffer EXACTLY ONCE and publishes it
// into the context. Valid for both JSON objects and arrays; non-JSON bodies
// still yield a raw string under the "raw" key so downstream filters can read
// them without touching bytes again.
func (c *Context) FinishRequestBodyParse() error {
	if c.BodyParsed() {
		return c.bodyErr
	}
	c.bodyParsed = true
	c.BodyState = BodyParsed
	c.statsMu.Lock()
	c.parseCount++
	c.statsMu.Unlock()

	trimmed := bytes.TrimSpace(c.Body)
	if len(trimmed) == 0 {
		c.bodyJSON = map[string]any{}
		c.bodyErr = nil
		c.Set("parsedBody", c.bodyJSON)
		return nil
	}
	var v any
	if err := json.Unmarshal(trimmed, &v); err == nil {
		c.bodyJSON = v
		c.bodyErr = nil
		c.Set("parsedBody", v)
		return nil
	}
	// Non-JSON: publish raw text, do not fail chain.
	c.bodyErr = nil
	c.bodyJSON = map[string]any{"raw": string(trimmed)}
	c.Set("parsedBody", c.bodyJSON)
	return nil
}

// BodyParsed reports whether the full request body has been parsed and is
// available via ParsedBody / Get("parsedBody").
func (c *Context) BodyParsed() bool { return c.BodyState == BodyParsed && c.bodyParsed }

// ParsedBody returns the cached parsed request body. It NEVER re-parses; if the
// chain did not buffer, it returns nil and the caller must read c.Body directly.
func (c *Context) ParsedBody() any {
	if !c.BodyParsed() {
		return nil
	}
	return c.bodyJSON
}

// ParsedBodyMap is a convenience cast for JSON objects.
func (c *Context) ParsedBodyMap() map[string]any {
	m, _ := c.ParsedBody().(map[string]any)
	return m
}

// Stats returns parse/buffer counters (used by tests and admin endpoints).
func (c *Context) Stats() map[string]any {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	return map[string]any{
		"parseCount":   c.parseCount,
		"bufferChunks": c.bufferChunks,
		"bodyState":    c.BodyState.String(),
	}
}

// ---------------------------------------------------------------------------
// Response helpers — NON-stream only
// ---------------------------------------------------------------------------

// DetectStream must be called in EncodeHeaders. It uses Content-Type plus an
// explicit business hint ("is_stream" in Values, set from request stream:true).
func (c *Context) DetectStream() bool {
	if v, ok := c.Values["is_stream"].(bool); ok && v {
		c.IsStream = true
		return true
	}
	ct := strings.ToLower(c.ResponseHeaders.Get("content-type"))
	if strings.HasPrefix(ct, "text/event-stream") || strings.Contains(ct, "application/x-ndjson") {
		c.IsStream = true
		return true
	}
	return false
}

// AppendResponseChunk buffers a NON-stream response chunk. Stream responses
// are deliberately ignored here: they must be inspected chunk-by-chunk and
// never retained in full (would break SSE semantics and grow unbounded).
func (c *Context) AppendResponseChunk(chunk []byte) error {
	if c.IsStream {
		return nil
	}
	if c.MaxResponse > 0 && len(c.ResponseBody)+len(chunk) > c.MaxResponse {
		return ErrBodyTooLarge
	}
	c.ResponseBody = append(c.ResponseBody, chunk...)
	return nil
}

// ResponseJSON parses the non-stream response body once and caches it.
func (c *Context) ResponseJSON() (any, error) {
	if c.IsStream {
		return nil, errors.New("composite: cannot fully parse a streaming response")
	}
	if c.responseParsed {
		return c.responseJSON, c.responseErr
	}
	c.responseParsed = true
	trimmed := bytes.TrimSpace(c.ResponseBody)
	if len(trimmed) == 0 {
		c.responseJSON = map[string]any{}
		return c.responseJSON, nil
	}
	var v any
	c.responseErr = json.Unmarshal(trimmed, &v)
	c.responseJSON = v
	c.Set("parsedResponse", v)
	return c.responseJSON, c.responseErr
}

func (c *Context) ParsedResponse() any {
	if !c.responseParsed {
		return nil
	}
	return c.responseJSON
}

// ---------------------------------------------------------------------------
// Appended encoded data (the SSE 断流 fix)
// ---------------------------------------------------------------------------

func (c *Context) AppendEncodedData(b []byte) {
	if len(b) == 0 {
		return
	}
	cp := append([]byte(nil), b...)
	c.appendMu.Lock()
	c.appended = append(c.appended, cp)
	c.appendMu.Unlock()
}

// ReplaceResponseData overrides the current EncodeData chunk. Pass an empty
// (but non-nil) slice to withhold the chunk entirely while still appending
// frames via AppendEncodedData.
func (c *Context) ReplaceResponseData(b []byte) {
	c.overrideMu.Lock()
	defer c.overrideMu.Unlock()
	c.respOverride = append([]byte(nil), b...)
	c.respOverrideSet = true
}

// TakeResponseOverride returns the override and whether one was set.
func (c *Context) TakeResponseOverride() ([]byte, bool) {
	c.overrideMu.Lock()
	defer c.overrideMu.Unlock()
	if !c.respOverrideSet {
		return nil, false
	}
	c.respOverrideSet = false
	b := c.respOverride
	c.respOverride = nil
	return b, true
}

func (c *Context) TakeAppended() []byte {
	c.appendMu.Lock()
	defer c.appendMu.Unlock()
	if len(c.appended) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, b := range c.appended {
		buf.Write(b)
	}
	c.appended = nil
	return buf.Bytes()
}

func (c *Context) Reject(code, reason string) {
	c.RejectCode = code
	c.RejectReason = reason
}

// ---------------------------------------------------------------------------
// Reset — for sync.Pool reuse of the Context itself
// ---------------------------------------------------------------------------

func (c *Context) Reset() {
	c.RequestID, c.Method, c.Path, c.RawQuery = "", "", "", ""
	c.RequestHeaders = NewHeaderMapEmpty()
	c.ResponseHeaders = NewHeaderMapEmpty()
	c.BodyState = BodyNone
	c.Body = c.Body[:0]
	c.bodyParsed, c.bodyJSON, c.bodyErr = false, nil, nil
	c.IsStream, c.ResponseStarted = false, false
	c.ResponseBody = c.ResponseBody[:0]
	c.responseParsed, c.responseJSON, c.responseErr = false, nil, nil
	c.RejectCode, c.RejectReason, c.RejectDetail = "", "", nil
	c.appended = nil
	c.overrideMu.Lock()
	c.respOverride, c.respOverrideSet = nil, false
	c.overrideMu.Unlock()
	c.Values = map[string]any{}
	c.statsMu.Lock()
	c.parseCount, c.bufferChunks = 0, 0
	c.statsMu.Unlock()
}
