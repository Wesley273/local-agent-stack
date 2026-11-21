package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"local-agent-stack/pkg/a2a"
	"local-agent-stack/services/data-plane/modelgw"
)

// ---------------------------------------------------------------------------
// (a) §3.4 SSE 断流修复回归：流式护栏命中不掐断连接。
//
// 断言：
//   * 只有一次 WriteHeader（无二次状态行）
//   * 状态 200
//   * 含 event: error 与 data: [DONE]
//   * Content-Type: text/event-stream，且没有 Content-Length
//   * 每帧 Flush、连接正常结束（无 close/RST）
//   * 输出中绝不出现第二个 HTTP 状态行
// ---------------------------------------------------------------------------

func TestStreamingInputGuardrailDoesNotCutConnection(t *testing.T) {
	snap := quietSnapshot()
	be := newMockBackend([]byte(`{"ok":true}`), "application/json", 200)
	defer be.Close()

	srv := newTestServer("http://127.0.0.1:1", "http://127.0.0.1:1", be.URL(), "http://127.0.0.1:1", snap)
	defer srv.Gateway.Close()

	// Streaming chat request whose prompt contains a blocked word. The input
	// guardrail short-circuits BEFORE the upstream; §3.4 requires an SSE error
	// frame + [DONE] on a 200 stream, never a new HTTP error response.
	body := `{"model":"mock-1","messages":[{"role":"user","content":"please BLOCK_ME now"}],"stream":true,"max_tokens":16}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-API-Key", "demo-key")
	req.Header.Set("Accept", "text/event-stream")

	cw := newCaptureWriter()
	srv.ServeHTTP(cw, req)

	if cw.writeHeaderCnt != 1 {
		t.Fatalf("§3.4 VIOLATION: WriteHeader called %d times (want exactly 1, no second status line)", cw.writeHeaderCnt)
	}
	if cw.status != http.StatusOK {
		t.Fatalf("streaming guardrail must keep HTTP 200, got %d", cw.status)
	}
	out := cw.Body()
	for _, want := range []string{"event: error", "GUARDRAIL_REJECTED", "[DONE]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("streaming guardrail output missing %q\n--- output ---\n%s", want, out)
		}
	}
	if ct := cw.header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type must be text/event-stream, got %q", ct)
	}
	if cl := cw.header.Get("Content-Length"); cl != "" {
		t.Fatalf("§3.4: SSE must NOT set Content-Length, got %q", cl)
	}
	if cw.flushed == 0 {
		t.Fatal("§3.4: every frame must be Flush()ed (flushed==0)")
	}
	if cw.closed.Load() {
		t.Fatal("§3.4 VIOLATION: connection was closed/RST")
	}
	if strings.Contains(out, "HTTP/1.1") || strings.Contains(out, "HTTP/2") {
		t.Fatalf("§3.4 VIOLATION: a new HTTP status line was injected into the stream:\n%s", out)
	}
}

// (a2) §3.4 回归 2：非流式护栏命中 → 200 + GUARDRAIL_REJECTED 错误信封。
func TestNonStreamingInputGuardrailReturnsEnvelope(t *testing.T) {
	snap := quietSnapshot()
	be := newMockBackend([]byte(`{"ok":true}`), "application/json", 200)
	defer be.Close()
	srv := newTestServer("http://127.0.0.1:1", "http://127.0.0.1:1", be.URL(), "http://127.0.0.1:1", snap)
	defer srv.Gateway.Close()

	body := `{"model":"mock-1","messages":[{"role":"user","content":"secret-leak"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-API-Key", "demo-key")

	cw := newCaptureWriter()
	srv.ServeHTTP(cw, req)

	if cw.status != http.StatusOK {
		t.Fatalf("non-stream guardrail must be HTTP 200, got %d", cw.status)
	}
	var env a2a.ErrorEnvelope
	if err := json.Unmarshal([]byte(cw.Body()), &env); err != nil {
		t.Fatalf("response is not a JSON error envelope: %v\n%s", err, cw.Body())
	}
	if env.Error.Code != a2a.CodeGuardrailRejected {
		t.Fatalf("want GUARDRAIL_REJECTED, got %s", env.Error.Code)
	}
	if env.Error.HTTPStatus != 200 {
		t.Fatalf("error envelope httpStatus must be 200, got %d", env.Error.HTTPStatus)
	}
}

// (a3) §3.4 输出护栏在流式中途命中：relayStream 必须发 error 帧 + [DONE]，
// 只写一次状态行，连接正常结束。这里直接驱动 relayStream，构造上游 SSE 块
// 含 blocked word 的场景（modelgw.Result.Stream）。
func TestStreamingOutputGuardrailAppendsErrorFrame(t *testing.T) {
	snap := quietSnapshot()
	srv := newTestServer("http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1", snap)
	defer srv.Gateway.Close()

	ctx := &a2a.FilterContext{
		RequestID:      "req-out-guard",
		TraceID:        "req-out-guard",
		RequestHeaders: a2a.NewHeaderMapEmpty(),
		Span:           a2a.NewSpan("req-out-guard", "", "data-plane.request", "server"),
	}
	ctx.RequestHeaders.Set("X-DP-Streaming", "1")

	ch := make(chan []byte, 4)
	ch <- []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
	ch <- []byte("data: {\"choices\":[{\"delta\":{\"content\":\"BLOCK_ME\"}}]}\n\n") // guardrail hit
	ch <- []byte("data: [DONE]\n\n")
	close(ch)

	res := &modelgw.Result{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Stream: ch,
	}
	cw := newCaptureWriter()
	srv.relayStream(cw, ctx, res)

	if cw.writeHeaderCnt != 1 {
		t.Fatalf("§3.4 VIOLATION: WriteHeader called %d times (want 1)", cw.writeHeaderCnt)
	}
	if cw.status != 200 {
		t.Fatalf("status must stay 200, got %d", cw.status)
	}
	out := cw.Body()
	if !strings.Contains(out, "event: error") || !strings.Contains(out, "[DONE]") {
		t.Fatalf("must contain event: error + [DONE]\n--- output ---\n%s", out)
	}
	if cw.closed.Load() {
		t.Fatal("§3.4 VIOLATION: connection closed/RST")
	}
}
