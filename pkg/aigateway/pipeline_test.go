package aigateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"local-agent-stack/pkg/composite"
)

type collectingSink struct {
	mu   sync.Mutex
	recs []MetricSnapshot
}

func (s *collectingSink) Record(m MetricSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, m)
}
func (s *collectingSink) all() []MetricSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]MetricSnapshot(nil), s.recs...)
}

func newCtx(body string) *composite.Context {
	return composite.NewContext(http.MethodPost, "/v1/chat/completions", "", "req-e2e-1",
		composite.NewHeaderMap(http.Header{"Content-Type": []string{"application/json"}}))
}

// runRequest drives one full request through the chain and returns the reply,
// the response header map and the final gate verdict.
func runRequest(t *testing.T, chain *composite.Chain, body, response string) (*composite.Reply, *composite.HeaderMap, *GateVerdict) {
	t.Helper()
	ctx := newCtx(body)
	s := chain.NewSession(ctx)
	reply, err := s.RunDecodeHeaders(false)
	if err != nil {
		t.Fatalf("decode headers err: %v", err)
	}
	if reply == nil {
		reply, err = s.RunDecodeData([]byte(body), true)
		if err != nil {
			t.Fatalf("decode data err: %v", err)
		}
	}
	h := composite.NewHeaderMap(http.Header{"Content-Type": []string{"application/json"}})
	if reply == nil {
		s.RunEncodeHeaders(h, false)
		_, _ = s.RunEncodeData([]byte(response), true)
	}
	g, _ := ctx.Get(KGateVerdict).(*GateVerdict)
	s.Close(composite.ReasonNormal)
	return reply, h, g
}

func TestEndToEndHappyPath(t *testing.T) {
	sink := &collectingSink{}
	cfg := DefaultConfig()
	cfg.MetricsSink = sink
	chain := BuildChain(cfg)

	body := `{"model":"mock-1","messages":[{"role":"user","content":"hello world"}]}`
	response := `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	reply, h, g := runRequest(t, chain, body, response)

	if reply != nil {
		t.Fatalf("unexpected local reply: %+v", reply)
	}
	if got := h.Get("x-routed-backend"); !strings.Contains(got, "9200") {
		t.Fatalf("route header missing backend, got %q", got)
	}
	if g == nil || !g.Allowed {
		t.Fatalf("gate should allow, got %+v", g)
	}
	recs := sink.all()
	if len(recs) != 1 {
		t.Fatalf("want 1 metric record, got %d", len(recs))
	}
	m := recs[0]
	if m.Model != "mock-1" || !strings.Contains(m.Backend, "9200") {
		t.Fatalf("metric model/backend wrong: %+v", m)
	}
	if m.TotalTokens != 5 {
		t.Fatalf("want total tokens 5, got %d", m.TotalTokens)
	}
	if m.GateWaitedMs < 0 {
		t.Fatal("gate wait time must be recorded")
	}
}

func TestRouteByModelUnknownFallsBack(t *testing.T) {
	cfg := DefaultConfig()
	chain := BuildChain(cfg)
	body := `{"model":"unknown-model","messages":[{"role":"user","content":"x"}]}`
	_, h, _ := runRequest(t, chain, body, `{"choices":[],"usage":{}}`)
	if got := h.Get("x-routed-backend"); !strings.Contains(got, "9200") {
		t.Fatalf("fallback route expected 9200, got %q", got)
	}
}

func TestInputGuardrailRejects(t *testing.T) {
	sink := &collectingSink{}
	cfg := DefaultConfig()
	cfg.MetricsSink = sink
	chain := BuildChain(cfg)

	body := `{"model":"mock-1","messages":[{"role":"user","content":"BLOCK_ME please"}]}`
	_, _, g := runRequest(t, chain, body, `{"choices":[]}`)
	// Per the requested design, the input guard verdict is awaited in EncodeData.
	// Once the response has started we must NOT emit a second HTTP response, so
	// the rejection is expressed via the gate verdict + metrics (and, for SSE,
	// an appended error frame). We assert those instead of a local reply.
	if g == nil || g.Allowed {
		t.Fatalf("gate should reject, got %+v", g)
	}
	if g.Code != "GUARDRAIL_REJECTED" {
		t.Fatalf("want GUARDRAIL_REJECTED, got %q", g.Code)
	}
	if !strings.Contains(g.Reason, "input") {
		t.Fatalf("rejection should come from input guard, got %q", g.Reason)
	}
	recs := sink.all()
	if len(recs) != 1 || recs[0].GuardRejects != 1 {
		t.Fatalf("metric should record guard reject: %+v", recs)
	}
}

func TestAsyncGuardrailTimeoutFailOpenAndFailClosed(t *testing.T) {
	slow := BlockingChecker(300*time.Millisecond, nil)

	for _, tc := range []struct {
		name      string
		failOpen  bool
		wantAllow bool
	}{
		{"fail-open", true, true},
		{"fail-closed", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &collectingSink{}
			cfg := DefaultConfig()
			cfg.GuardChecker = slow
			cfg.GuardTimeout = 20 * time.Millisecond
			cfg.GuardFailOpen = tc.failOpen
			cfg.MetricsSink = sink
			chain := BuildChain(cfg)
			body := `{"model":"mock-1","messages":[{"role":"user","content":"hi"}]}`
			_, _, g := runRequest(t, chain, body, `{"choices":[],"usage":{}}`)
			if g == nil || !g.TimedOut {
				t.Fatalf("gate should report timeout, got %+v", g)
			}
			if g.Allowed != tc.wantAllow {
				t.Fatalf("gate.Allowed=%v want %v", g.Allowed, tc.wantAllow)
			}
			recs := sink.all()
			if len(recs) != 1 {
				t.Fatalf("want 1 metric, got %d", len(recs))
			}
			if !tc.wantAllow && recs[0].GuardRejects != 1 {
				t.Fatalf("fail-closed should record guard reject: %+v", recs[0])
			}
		})
	}
}

func TestTokenLimitRejects(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TokenLimit = 1 // any prompt >1 token will be rejected
	chain := BuildChain(cfg)
	body := `{"model":"mock-1","messages":[{"role":"user","content":"this is a long prompt with many characters"}]}`
	reply, _, _ := runRequest(t, chain, body, `{}`)
	if reply == nil {
		t.Fatal("expected RATE_LIMITED local reply")
	}
	var env map[string]any
	_ = json.Unmarshal(reply.Body, &env)
	e, _ := env["error"].(map[string]any)
	if e["code"] != "RATE_LIMITED" {
		t.Fatalf("want RATE_LIMITED, got %v", e)
	}
}

func TestStreamGuardrailAppendsFrameNeverLocalReply(t *testing.T) {
	cfg := DefaultConfig()
	chain := BuildChain(cfg)

	ctx := newCtx(`{"model":"mock-1","messages":[{"role":"user","content":"hello"}]}`)
	s := chain.NewSession(ctx)
	_, _ = s.RunDecodeHeaders(false)
	_, _ = s.RunDecodeData([]byte(`{"model":"mock-1","messages":[{"role":"user","content":"hello"}]}`), true)
	// Mark as stream before EncodeHeaders (business hint), like request stream:true.
	ctx.Set("is_stream", true)
	h := composite.NewHeaderMap(http.Header{"Content-Type": []string{"text/event-stream"}})
	s.RunEncodeHeaders(h, false)
	// Output checker blocks on "secret-leak".
	out, appended := s.RunEncodeData([]byte("data: secret-leak\n\n"), false)
	if len(out) != 0 {
		t.Fatalf("rejected stream chunk must be withheld, got %q", out)
	}
	if !strings.Contains(string(appended), "event: error") || !strings.Contains(string(appended), "[DONE]") {
		t.Fatalf("expected appended SSE error+done, got %q", appended)
	}
	g, _ := ctx.Get(KGateVerdict).(*GateVerdict)
	if g != nil && g.Allowed {
		t.Fatal("gate should reject stream content")
	}
	s.Close(composite.ReasonNormal)
}

func TestParseHappensOnceAcrossPipeline(t *testing.T) {
	cfg := DefaultConfig()
	chain := BuildChain(cfg)
	ctx := newCtx("")
	s := chain.NewSession(ctx)
	_, _ = s.RunDecodeHeaders(false)
	// feed in two chunks
	part1 := `{"model":"mock-1","messages":[{"role":"user",`
	part2 := `"content":"hi"}]}`
	_, _ = s.RunDecodeData([]byte(part1), false)
	_, _ = s.RunDecodeData([]byte(part2), true)
	if ctx.Stats()["parseCount"] != 1 {
		t.Fatalf("parse must happen exactly once, stats=%v", ctx.Stats())
	}
	if modelOf(ctx) != "mock-1" {
		t.Fatalf("model extraction failed, got %q", modelOf(ctx))
	}
	s.Close(composite.ReasonNormal)
}
