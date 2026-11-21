package a2a

import (
	"encoding/json"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Tracing (contract §1.8). All numeric fields are non-pointer so they can
// NEVER be null — this directly addresses the resume bug where getDuration()
// returned null and unboxing NPE made a whole batch insert fail.
// ---------------------------------------------------------------------------

// Span is one traced operation.
type Span struct {
	TraceID    string         `json:"traceId"`
	SpanID     string         `json:"spanId"`
	ParentID   string         `json:"parentId"`
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	StartMs    int64          `json:"startMs"`
	DurationMs int64          `json:"durationMs"` // always an int, never null
	Status     string         `json:"status"`     // "OK" | "ERROR"
	Attributes map[string]any `json:"attributes"`
	Events     []SpanEvent    `json:"events"`
}

// SpanEvent is a timestamped annotation inside a span.
type SpanEvent struct {
	Name string `json:"name"`
	AtMs int64  `json:"atMs"`
}

// NewSpan starts a span.
func NewSpan(traceID, parentID, name, kind string) *Span {
	return &Span{
		TraceID: traceID, SpanID: NewID("s"), ParentID: parentID,
		Name: name, Kind: kind, StartMs: time.Now().UnixMilli(),
		Status: "OK", Attributes: map[string]any{}, Events: []SpanEvent{},
	}
}

// Set records an attribute with type coercion for status codes.
func (s *Span) Set(k string, v any) *Span {
	s.Attributes[k] = v
	return s
}

// Event annotates the span.
func (s *Span) Event(name string) *Span {
	s.Events = append(s.Events, SpanEvent{Name: name, AtMs: time.Now().UnixMilli()})
	return s
}

// Finish closes the span. durationMs is always computed (never null).
func (s *Span) Finish(status string) *Span {
	d := time.Now().UnixMilli() - s.StartMs
	if d < 0 {
		d = 0
	}
	s.DurationMs = d
	s.Status = status
	return s
}

// SpanStore is a bounded, concurrency-safe in-memory span collector.
// Insertion is PER-SPAN fault tolerant: one malformed span must never abort
// the whole batch (resume bug: "batchInsert Log enter, size=4" all failed).
type SpanStore struct {
	mu     sync.RWMutex
	spans  []Span
	limit  int
	counts map[string]int64
}

// NewSpanStore creates a store retaining at most limit spans.
func NewSpanStore(limit int) *SpanStore {
	if limit <= 0 {
		limit = 1000
	}
	return &SpanStore{limit: limit, spans: make([]Span, 0, limit), counts: map[string]int64{}}
}

// Add appends a span with defensive copying; returns an error only for that span.
func (st *SpanStore) Add(s *Span) error {
	if s == nil {
		return nil
	}
	// Defensive: normalise the fields that historically caused NPEs.
	if s.Attributes == nil {
		s.Attributes = map[string]any{}
	}
	if s.Events == nil {
		s.Events = []SpanEvent{}
	}
	if s.Status == "" {
		s.Status = "OK"
	}
	if s.DurationMs < 0 {
		s.DurationMs = 0
	}
	// Force http.status_code to be an integer if present but wrong type.
	if v, ok := s.Attributes["http.status_code"]; ok {
		switch n := v.(type) {
		case int:
			_ = n
		case float64:
			s.Attributes["http.status_code"] = int(n)
		default:
			s.Attributes["http.status_code"] = 0
		}
	}
	// Validate serialisability; a single bad span must not break the batch.
	if _, err := json.Marshal(s); err != nil {
		return err
	}
	cp := *s
	cp.Attributes = CloneMap(s.Attributes)
	cp.Events = append([]SpanEvent(nil), s.Events...)

	st.mu.Lock()
	defer st.mu.Unlock()
	// bounded ring: drop oldest when full
	if len(st.spans) >= st.limit {
		copy(st.spans, st.spans[1:])
		st.spans = st.spans[:len(st.spans)-1]
	}
	st.spans = append(st.spans, cp)
	st.counts["total"]++
	st.counts[s.Name]++
	if s.Status == "ERROR" {
		st.counts["errors"]++
	}
	return nil
}

// AddBatch inserts each span independently. Returns the number accepted.
func (st *SpanStore) AddBatch(spans []*Span) (int, []error) {
	accepted := 0
	errs := []error{}
	for _, s := range spans {
		if err := st.Add(s); err != nil {
			errs = append(errs, err)
			continue // keep going — the whole point
		}
		accepted++
	}
	return accepted, errs
}

// List returns the most recent n spans (newest last).
func (st *SpanStore) List(n int) []Span {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if n <= 0 || n > len(st.spans) {
		n = len(st.spans)
	}
	out := make([]Span, n)
	copy(out, st.spans[len(st.spans)-n:])
	return out
}

// Counters returns a snapshot of metric counters.
func (st *SpanStore) Counters() map[string]int64 {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make(map[string]int64, len(st.counts))
	for k, v := range st.counts {
		out[k] = v
	}
	return out
}

// Metrics is the shape returned by GET /v1/admin/metrics.
type Metrics struct {
	Counters map[string]int64 `json:"counters"`
}
