package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	"local-agent-stack/pkg/a2a"
	"local-agent-stack/services/data-plane/config"
)

// ---------------------------------------------------------------------------
// Shared test scaffolding.
// ---------------------------------------------------------------------------

// newTestServer builds a Server wired to the given (httptest) downstream URLs.
func newTestServer(cpURL, sidecar, llm, mcp string, snap *config.Snapshot) *Server {
	store := config.NewStore(snap)
	poller := config.NewPoller(cpURL, store, 20*time.Millisecond)
	return NewServer(store, poller, sidecar, llm, mcp, cpURL)
}

// captureWriter is a custom ResponseWriter that records how many times
// WriteHeader is called (§3.4 test: assert exactly ONE, never a second).
type captureWriter struct {
	header         http.Header
	body           []byte
	writeHeaderCnt int
	status         int
	flushed        int
	closed         atomic.Bool
}

func newCaptureWriter() *captureWriter {
	return &captureWriter{header: make(http.Header)}
}

func (c *captureWriter) Header() http.Header { return c.header }
func (c *captureWriter) WriteHeader(status int) {
	c.writeHeaderCnt++
	if c.writeHeaderCnt == 1 {
		c.status = status
	}
}
func (c *captureWriter) Write(p []byte) (int, error) {
	c.body = append(c.body, p...)
	return len(p), nil
}
func (c *captureWriter) Flush()       { c.flushed++ }
func (c *captureWriter) Body() string { return string(c.body) }

// mockBackend starts an httptest server that always replies with body /
// contentType and records the received requests.
type mockBackend struct {
	server   *httptest.Server
	body     []byte
	ctype    string
	status   int
	requests chan *http.Request
}

func newMockBackend(body []byte, ctype string, status int) *mockBackend {
	m := &mockBackend{body: body, ctype: ctype, status: status, requests: make(chan *http.Request, 16)}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case m.requests <- r:
		default:
		}
		w.Header().Set("Content-Type", m.ctype)
		w.WriteHeader(m.status)
		_, _ = w.Write(m.body)
	}))
	return m
}

func (m *mockBackend) URL() string { return m.server.URL }
func (m *mockBackend) Close()      { m.server.Close() }

// quietSnapshot returns a default snapshot with unlimited rate limits so
// tests never accidentally hit E16's quota.
func quietSnapshot() *config.Snapshot {
	s := config.DefaultSnapshot()
	unlimited := a2a.LimitRule{RequestsPerMinute: 1000000, TokensPerMinute: 100000000, Burst: 100000}
	s.RateLimits = map[string]a2a.LimitRule{"default": unlimited, "app-demo": unlimited, "app-admin": unlimited}
	return s
}

// keep the time import referenced (poller interval type)
var _ = time.Second
