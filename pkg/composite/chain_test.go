package composite

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func ctxForTest() *Context {
	return NewContext(http.MethodPost, "/a2a/echo-agent", "", "req-1",
		NewHeaderMap(http.Header{"X-Api-Key": []string{"demo-key"}}))
}

func join(xs []string) string { return strings.Join(xs, ",") }

type TraceFilter struct {
	BaseFilter
	name string
	log  *[]string
}

func (f *TraceFilter) Name() string { return f.name }
func (f *TraceFilter) record(s string) {
	if f.log != nil {
		*f.log = append(*f.log, f.name+"."+s)
	}
}
func (f *TraceFilter) DecodeHeaders(c *Context, end bool) Status { f.record("DH"); return Continue }
func (f *TraceFilter) DecodeData(c *Context, d []byte, end bool) Status {
	f.record("DD")
	return Continue
}
func (f *TraceFilter) EncodeHeaders(c *Context, end bool) Status { f.record("EH"); return Continue }
func (f *TraceFilter) EncodeData(c *Context, d []byte, end bool) Status {
	f.record("ED")
	return Continue
}

func TestOrderDecodeForwardEncodeReverse(t *testing.T) {
	log := []string{}
	chain := NewChain()
	for _, n := range []string{"A", "B", "C"} {
		n := n
		chain.Register(n, func() SubFilter { return &TraceFilter{name: n, log: &log} })
	}

	ctx := ctxForTest()
	s := chain.NewSession(ctx)
	_, _ = s.RunDecodeHeaders(false)
	_, _ = s.RunDecodeData([]byte(`{"prompt":"hi"}`), true)
	h := NewHeaderMap(http.Header{"Content-Type": []string{"application/json"}})
	s.RunEncodeHeaders(h, false)
	_, _ = s.RunEncodeData([]byte(`{"answer":"ok"}`), true)
	s.Close(ReasonNormal)

	want := "A.DH,B.DH,C.DH,A.DD,B.DD,C.DD,C.EH,B.EH,A.EH,C.ED,B.ED,A.ED"
	if got := join(log); got != want {
		t.Fatalf("order mismatch\n got: %s\nwant: %s", got, want)
	}
}

// BodyBufferFilter is the first filter needing the full body. It stops headers,
// then reads the ONE cached parse from ctx and publishes derived state.
type BodyBufferFilter struct {
	BaseFilter
	parseCalls int
}

func (f *BodyBufferFilter) Name() string { return "body-buffer" }
func (f *BodyBufferFilter) DecodeHeaders(c *Context, end bool) Status {
	return StopAndBuffer
}
func (f *BodyBufferFilter) DecodeData(c *Context, d []byte, end bool) Status {
	m := c.ParsedBodyMap()
	if v, ok := m["prompt"].(string); ok {
		c.Set("prompt", v)
	}
	f.parseCalls++
	return Continue
}

type DownstreamFilter struct {
	BaseFilter
	sawParsed bool
	sawPrompt string
	sameInst  bool
	firstInst SubFilter
}

func (f *DownstreamFilter) Name() string { return "downstream" }
func (f *DownstreamFilter) DecodeData(c *Context, d []byte, end bool) Status {
	if f.firstInst == nil {
		f.firstInst = f // remember instance identity at first invocation
	}
	if c.BodyParsed() {
		f.sawParsed = true
	}
	f.sawPrompt = c.GetString("prompt")
	return Continue
}
func (f *DownstreamFilter) EncodeData(c *Context, d []byte, end bool) Status {
	f.sameInst = f.firstInst == f
	return Continue
}

func TestStopAndBufferParseOnceAndShare(t *testing.T) {
	var downstream *DownstreamFilter
	chain := NewChain()
	chain.Register("body-buffer", func() SubFilter { return &BodyBufferFilter{} })
	chain.Register("downstream", func() SubFilter {
		downstream = &DownstreamFilter{}
		return downstream
	})

	ctx := ctxForTest()
	s := chain.NewSession(ctx)
	d0, err := s.RunDecodeHeaders(false)
	if err != nil {
		t.Fatalf("headers err: %v", err)
	}
	// The INNER StopAndBuffer must be propagated OUT to Envoy.
	if d0.Action != FlowStopAndBuffer {
		t.Fatalf("headers decision want FlowStopAndBuffer, got %s", d0.Action)
	}
	if ctx.BodyState != BodyCollecting {
		t.Fatalf("want BodyCollecting, got %s", ctx.BodyState)
	}
	d1, _ := s.RunDecodeData([]byte(`{"prompt":"hel`), false)
	if d1.Action != FlowStopAndBuffer {
		t.Fatalf("partial chunk decision want FlowStopAndBuffer, got %s", d1.Action)
	}
	if ctx.BodyParsed() {
		t.Fatal("body must not be parsed before final chunk")
	}
	d2, _ := s.RunDecodeData([]byte(`lo"}`), true)
	if d2.Action != FlowReady {
		t.Fatalf("final chunk decision want FlowReady, got %s", d2.Action)
	}
	if string(d2.ReleaseBody) != `{"prompt":"hello"}` {
		t.Fatalf("released body mismatch: %s", d2.ReleaseBody)
	}

	if ctx.BodyState != BodyParsed || !ctx.BodyParsed() {
		t.Fatalf("want BodyParsed, got %s", ctx.BodyState)
	}
	if string(ctx.Body) != `{"prompt":"hello"}` {
		t.Fatalf("buffered body mismatch: %s", ctx.Body)
	}
	st := ctx.Stats()
	if st["parseCount"] != 1 || st["bufferChunks"] != 2 {
		t.Fatalf("want parse=1 chunks=2, got %v", st)
	}
	if !downstream.sawParsed {
		t.Fatal("downstream must see parsed body via ctx")
	}
	if downstream.sawPrompt != "hello" {
		t.Fatalf("downstream prompt want hello, got %q", downstream.sawPrompt)
	}

	// Same filter instance across phases (Envoy per-stream instance semantics).
	s.RunEncodeHeaders(NewHeaderMap(http.Header{"Content-Type": []string{"application/json"}}), false)
	_, _ = s.RunEncodeData([]byte(`{}`), true)
	if !downstream.sameInst {
		t.Fatal("sub-filter instance must be stable across decode and encode phases")
	}
	s.Close(ReasonNormal)
}

func TestParsedBodyIsCached(t *testing.T) {
	ctx := ctxForTest()
	ctx.BeginRequestBodyBuffer()
	_ = ctx.AppendRequestChunk([]byte(`{"a":1}`))
	_ = ctx.FinishRequestBodyParse()

	a := ctx.ParsedBody()
	b := ctx.ParsedBody()
	ma, _ := a.(map[string]any)
	mb, _ := b.(map[string]any)
	ma["injected"] = "yes"
	if mb["injected"] != "yes" {
		t.Fatal("ParsedBody must return the cached object, not a fresh parse")
	}
	if ctx.Stats()["parseCount"] != 1 {
		t.Fatal("parse must happen exactly once")
	}
}

type RejectFilter struct {
	BaseFilter
	name string
	code string
}

func (f *RejectFilter) Name() string { return f.name }
func (f *RejectFilter) DecodeHeaders(c *Context, end bool) Status {
	c.Reject(f.code, "blocked by "+f.name)
	return StopAndReply
}

func TestStopAndReplyFromMiddleSkipsDownstream(t *testing.T) {
	log := []string{}
	chain := NewChain()
	chain.Register("A", func() SubFilter { return &TraceFilter{name: "A", log: &log} })
	chain.Register("B", func() SubFilter { return &RejectFilter{name: "B", code: "AUTH_REQUIRED"} })
	chain.Register("C", func() SubFilter { return &TraceFilter{name: "C", log: &log} })

	ctx := ctxForTest()
	s := chain.NewSession(ctx)
	d, err := s.RunDecodeHeaders(true)
	if err != nil || d.Action != FlowStopAndReply || d.Reply == nil {
		t.Fatalf("expected FlowStopAndReply, got decision=%v err=%v", d, err)
	}
	if d.Reply.Status != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", d.Reply.Status)
	}
	if join(log) != "A.DH" {
		t.Fatalf("C must be skipped after B local reply; got %s", join(log))
	}
	s.Close(ReasonNormal)
}

type StreamGuardFilter struct {
	BaseFilter
	name string
}

func (f *StreamGuardFilter) Name() string { return f.name }
func (f *StreamGuardFilter) EncodeData(c *Context, d []byte, end bool) Status {
	if c.IsStream && bytes.Contains(d, []byte("secret-leak")) {
		c.AppendEncodedData([]byte("event: error\ndata: {\"error\":{\"code\":\"GUARDRAIL_REJECTED\"}}\n\ndata: [DONE]\n\n"))
	}
	return Continue
}

func TestStreamResponseNotBufferedAndAppendFrame(t *testing.T) {
	chain := NewChain()
	chain.Register("guard", func() SubFilter { return &StreamGuardFilter{name: "guard"} })
	ctx := ctxForTest()
	s := chain.NewSession(ctx)
	s.RunEncodeHeaders(NewHeaderMap(http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}}), false)
	if !ctx.IsStream {
		t.Fatal("stream must be detected in EncodeHeaders")
	}
	out, appended := s.RunEncodeData([]byte("data: secret-leak\n\n"), false)
	if string(out) != "data: secret-leak\n\n" {
		t.Fatalf("stream chunk must pass through unchanged, got %q", out)
	}
	if !bytes.Contains(appended, []byte("event: error")) || !bytes.Contains(appended, []byte("[DONE]")) {
		t.Fatalf("expected appended error+done, got %q", appended)
	}
	if len(ctx.ResponseBody) != 0 {
		t.Fatal("streaming response must NOT be retained in ctx.ResponseBody")
	}
	s.Close(ReasonNormal)
}

func TestNonStreamResponseParsedOnce(t *testing.T) {
	chain := NewChain()
	ctx := ctxForTest()
	s := chain.NewSession(ctx)
	s.RunEncodeHeaders(NewHeaderMap(http.Header{"Content-Type": []string{"application/json"}}), false)
	_, _ = s.RunEncodeData([]byte(`{"ans`), false)
	_, _ = s.RunEncodeData([]byte(`wer":"ok"}`), true)
	v, err := ctx.ResponseJSON()
	if err != nil {
		t.Fatalf("ResponseJSON err: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok || m["answer"] != "ok" {
		t.Fatalf("parsed response mismatch: %#v", v)
	}
	s.Close(ReasonNormal)
}

type OverrideFilter struct {
	BaseFilter
}

func (f *OverrideFilter) Name() string { return "override" }
func (f *OverrideFilter) EncodeData(c *Context, d []byte, end bool) Status {
	c.ReplaceResponseData([]byte("replaced"))
	return Continue
}

func TestResponseOverride(t *testing.T) {
	chain := NewChain()
	chain.Register("override", func() SubFilter { return &OverrideFilter{} })
	ctx := ctxForTest()
	s := chain.NewSession(ctx)
	s.RunEncodeHeaders(NewHeaderMap(http.Header{"Content-Type": []string{"application/json"}}), false)
	out, _ := s.RunEncodeData([]byte("original"), true)
	if string(out) != "replaced" {
		t.Fatalf("override failed, got %q", out)
	}
	s.Close(ReasonNormal)
}

func TestPoolReuse(t *testing.T) {
	chain := NewChain()
	chain.Register("A", func() SubFilter { return &TraceFilter{name: "A"} })
	for i := 0; i < 100; i++ {
		s := chain.NewSession(ctxForTest())
		s.Close(ReasonNormal)
	}
	st := chain.PoolStats()
	if st.New > 2 {
		t.Fatalf("filters should be pooled; new=%d gets=%d", st.New, st.Gets)
	}
	if st.Gets != 100 {
		t.Fatalf("gets want 100 (one acquire per Session), got %d", st.Gets)
	}
}
