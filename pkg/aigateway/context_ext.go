package aigateway

import (
	"sync"

	"local-agent-stack/pkg/composite"
)

// Keys published into composite.Context by the pipeline plugins.
// Using constants avoids typo-driven "why is route empty" bugs.
const (
	KModel          = "gw.model"            // string: model name from request body
	KRouteBackend   = "gw.routeBackend"     // string: chosen backend URL
	KRouteMatched   = "gw.routeMatched"     // bool
	KPromptTokens   = "gw.promptTokens"     // int
	KCompletionToks = "gw.completionTokens" // int
	KTokenReserved  = "gw.tokenReserved"    // bool
	KGateDone       = "gw.gateDone"         // bool: encode gate already evaluated
	KGateVerdict    = "gw.gateVerdict"      // *GateVerdict
	KInputFuture    = "gw.inputFuture"      // *Future[GuardVerdict]
	KOutputFuture   = "gw.outputFuture"     // *Future[GuardVerdict]
	KMetrics        = "gw.metrics"          // *MetricSnapshot
	KStreamPolicy   = "gw.streamPolicy"     // string: "fail-open"|"fail-closed"
)

// GateVerdict is the combined result of waiting for all async guards.
type GateVerdict struct {
	Allowed  bool
	Code     string
	Reason   string
	Input    GuardVerdict
	Output   GuardVerdict
	TimedOut bool
}

// MetricSnapshot is the per-request metric record written by MetricsFilter.
type MetricSnapshot struct {
	mu sync.Mutex

	Model            string
	Backend          string
	RequestID        string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CompletionCount  int
	GuardRejects     int
	GateWaitedMs     int64
	Status           string
	IsStream         bool
}

// Snapshot returns a copy for reporting (thread-safe).
func (m *MetricSnapshot) Snapshot() MetricSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return MetricSnapshot{
		Model: m.Model, Backend: m.Backend, RequestID: m.RequestID,
		PromptTokens: m.PromptTokens, CompletionTokens: m.CompletionTokens,
		TotalTokens: m.TotalTokens, CompletionCount: m.CompletionCount,
		GuardRejects: m.GuardRejects, GateWaitedMs: m.GateWaitedMs,
		Status: m.Status, IsStream: m.IsStream,
	}
}

func (m *MetricSnapshot) addTokens(prompt, completion int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PromptTokens += prompt
	m.CompletionTokens += completion
	m.TotalTokens = m.PromptTokens + m.CompletionTokens
}

// setTokens overwrites token counts with authoritative backend usage numbers.
func (m *MetricSnapshot) setTokens(prompt, completion int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PromptTokens = prompt
	m.CompletionTokens = completion
	m.TotalTokens = prompt + completion
}

func (m *MetricSnapshot) addChunk() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CompletionCount++
}

func (m *MetricSnapshot) setStatus(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Status = s
}

// ---- typed context helpers (no changes to composite.Context) ----

func modelOf(c *composite.Context) string { return c.GetString(KModel) }

func inputFutureOf(c *composite.Context) *Future[GuardVerdict] {
	f, _ := c.Get(KInputFuture).(*Future[GuardVerdict])
	return f
}
func outputFutureOf(c *composite.Context) *Future[GuardVerdict] {
	f, _ := c.Get(KOutputFuture).(*Future[GuardVerdict])
	return f
}
func metricsOf(c *composite.Context) *MetricSnapshot {
	m, _ := c.Get(KMetrics).(*MetricSnapshot)
	return m
}
func gateDone(c *composite.Context) bool {
	v, _ := c.Get(KGateDone).(bool)
	return v
}

// estimateTokens is a deliberately simple, dependency-free token estimator
// (~4 chars/token) so the example runs offline. Production would call a tokenizer.
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	n := len(s) / 4
	if n < 1 {
		n = 1
	}
	return n
}
