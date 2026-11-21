package filters

import (
	"local-agent-stack/pkg/a2a"
)

// ---------------------------------------------------------------------------
// Guardrail filters — contract §2.4 rule 5, §3.2, §3.4.
//
// InputGuardrailFilter (DecodeData) checks the request body with the shared
// a2a.GuardrailEngine. On a hit it returns StopAndReply with
// GUARDRAIL_REJECTED. a2a.HTTPStatus(CodeGuardrailRejected) == 200, so the
// resulting Reply is already a 200 — the server then renders it either as a
// plain JSON error envelope (non-stream) or as a 200 SSE stream consisting of
// `event: error` + `data: [DONE]` (stream). See server.writeGuardrailResponse.
//
// OutputGuardrailFilter (EncodeData) checks each response chunk. This is where
// the §3.4 SSE 断流修复 lives: once ctx.ResponseStarted == true we MUST NOT
// write any new HTTP status line / RST / close. Instead we append SSE error
// frames to the CURRENT stream (AppendEncodedData) and return Continue.
// ---------------------------------------------------------------------------

// InputGuardrailFilter is filter #3. It buffers the full body (the server
// always passes the complete body to DecodeData) and runs the input engine.
type InputGuardrailFilter struct {
	a2a.BaseFilter
	rt *Runtime
}

// Name identifies the filter.
func (f *InputGuardrailFilter) Name() string { return "guardrail-input" }

// DecodeData runs the four input guardrails over the request body text.
func (f *InputGuardrailFilter) DecodeData(buf []byte, endStream bool) a2a.FilterStatus {
	ctx := f.rt.Active()
	if ctx == nil {
		return a2a.Continue
	}
	snap := f.rt.Config.Current()
	if !snap.Guardrails.EnableInput {
		return a2a.Continue
	}
	// /v1/embeddings 的 input 是待向量化的文档（可 >8000 字符，契约 §2.2、
	// E15 的 124844 字符用例），不是 prompt，不应套用 maxPromptChars / 敏感词
	// 输入护栏。否则超长文档会被误判为 GUARDRAIL_REJECTED，导致 embedding
	// 请求返回错误信封。输出侧护栏仍然保留。
	if ctx.Path == "/v1/embeddings" {
		return a2a.Continue
	}
	if len(buf) == 0 {
		return a2a.Continue
	}
	meta := map[string]any{"requestId": ctx.RequestID}
	// 契约 §0.0：数据面是治理层 + 透明反向代理，不解析 A2A 协议语义。
	// 用 CheckInputWordsOnly：只查敏感词/工具/注入，不查 body 长度（含 JSON
	// framing 与 128KB artifact）、不校验 JSON-RPC method（官方 SDK 用
	// PascalCase SendMessage，兼容轨用 message/send）。长度与方法白名单分别
	// 属于模型网关与 Sidecar/应用层职责。
	res := f.rt.Engine().CheckInputWordsOnly(string(buf), meta)
	if res.Allowed {
		return a2a.Continue
	}
	f.rt.guardHits.Add(1)
	ctx.RequestHeaders.Set(HdrFilterCode, string(a2a.CodeGuardrailRejected))
	ctx.RequestHeaders.Set(HdrFilterMsg, res.Reason)
	// Return StopAndReply; the a2a.FilterChain turns this into a Reply whose
	// HTTP status is 200 (GUARDRAIL_REJECTED). The server decides SSE vs JSON
	// based on the streaming flag — it NEVER writes a second status line.
	return a2a.StopAndReply
}

// Reset clears per-request state.
func (f *InputGuardrailFilter) Reset() {}

// OutputGuardrailFilter is filter #7 (response side). See §3.4 above.
type OutputGuardrailFilter struct {
	a2a.BaseFilter
	rt *Runtime
}

// Name identifies the filter.
func (f *OutputGuardrailFilter) Name() string { return "guardrail-output" }

// EncodeData checks one response chunk. Three cases:
//
//  1. Already rejected (dedup marker on ctx.RequestHeaders) -> Continue.
//  2. Streaming (ctx.ResponseStarted == true): on a hit, append
//     SSEErrorFrame + SSEDoneFrame via AppendEncodedData, set the dedup
//     marker, log WARN, and return Continue. NEVER StopAndReply, NEVER a
//     second header, NEVER close. The server skips forwarding the offending
//     chunk and writes only the appended frames. [§3.4 HARD RULE 1 & 2]
//  3. Non-streaming (response not yet started): on a hit, set the dedup
//     marker; the server (which is buffering) replaces the whole body with a
//     200 GUARDRAIL_REJECTED JSON envelope.
func (f *OutputGuardrailFilter) EncodeData(buf []byte, endStream bool) a2a.FilterStatus {
	ctx := f.rt.Active()
	if ctx == nil || len(buf) == 0 {
		return a2a.Continue
	}
	// Dedup: we emit the error frame once per stream. The marker lives on the
	// per-request HeaderMap, NOT on this pooled filter instance, so Reset()
	// never has to clean it and pooled instances carry no request state.
	if ctx.RequestHeaders.Get(HdrGuardRejected) == "1" {
		return a2a.Continue
	}
	snap := f.rt.Config.Current()
	if !snap.Guardrails.EnableOutput {
		return a2a.Continue
	}
	// Output side: blocked words + secret patterns only. maxPromptChars is an
	// INPUT rule and must not reject a legitimately large 128KB artifact (E11).
	res := f.rt.Engine().CheckOutputWordsOnly(string(buf), map[string]any{"requestId": ctx.RequestID})
	if res.Allowed {
		return a2a.Continue
	}
	f.rt.guardHits.Add(1)
	ctx.RequestHeaders.Set(HdrGuardRejected, "1")

	if ctx.ResponseStarted {
		// ---- §3.4 SSE 断流修复（核心）----------------------------------
		// 响应已经开始：任何 StopAndReply / LocalReply 都必须被忽略并降级为
		// AppendEncodedData + Continue。绝不写新 HTTP 状态行 / RST / FIN / close。
		frames := a2a.SSEErrorFrame(a2a.CodeGuardrailRejected, res.Reason, ctx.RequestID) +
			a2a.SSEDoneFrame()
		ctx.AppendEncodedData([]byte(frames))
		f.rt.warnf("[OutputGuardrail] WARN §3.4: guardrail hit mid-stream; "+
			"LocalReply downgraded to AppendEncodedData+Continue (response stays 200, no RST). reason=%q requestId=%s",
			res.Reason, ctx.RequestID)
		// Return Continue — NOT StopAndReply. This is the whole fix.
		return a2a.Continue
	}

	// Non-streaming: the server buffers the full body; it sees the dedup
	// marker and renders a 200 GUARDRAIL_REJECTED envelope instead.
	return a2a.Continue
}

// Reset clears per-request state.
func (f *OutputGuardrailFilter) Reset() {}
