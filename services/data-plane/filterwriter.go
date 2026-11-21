package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"local-agent-stack/pkg/a2a"
	"local-agent-stack/services/data-plane/filters"
)

// ---------------------------------------------------------------------------
// filterWriter — the response-side bridge between net/http and the ENCODE
// filter chain (contract §3.2 / §3.4).
//
// It wraps the real ResponseWriter and runs EncodeHeaders once (on the first
// WriteHeader) plus EncodeData on every Write. Most importantly it enforces
// the §3.4 SSE 断流修复 for the transparent reverse-proxy path:
//
//   * After the response has started (ctx.ResponseStarted == true), a
//     StopAndReply/LocalReply from a filter is IGNORED and downgraded to
//     AppendEncodedData + Continue (the a2a.FilterChain already does this for
//     RunEncodeData; here we additionally GUARANTEE no second WriteHeader).
//   * A guardrail hit mid-stream writes the appended SSE error frame + [DONE]
//     and then KEEPS draining upstream writes until a clean end — never a new
//     HTTP status line, never RST/FIN, never close.
//   * Every frame is Flush()ed; Content-Length is removed for SSE.
// ---------------------------------------------------------------------------

type filterWriter struct {
	w      http.ResponseWriter
	s      *Server
	ctx    *a2a.FilterContext
	status int
	// encode-chain already ran on headers
	headersDone bool
	// SSE stream detected
	isSSE bool
	// guardrail terminal frames already emitted
	errDone bool
	// buffer non-stream bodies until finalise (so output guardrail can replace)
	buffered bytes.Buffer
	flusher  http.Flusher
}

func newFilterWriter(w http.ResponseWriter, s *Server, ctx *a2a.FilterContext) *filterWriter {
	f, _ := w.(http.Flusher)
	return &filterWriter{w: w, s: s, ctx: ctx, status: 0, flusher: f}
}

// Header returns the underlying header map (httputil.ReverseProxy sets headers
// here before WriteHeader).
func (fw *filterWriter) Header() http.Header { return fw.w.Header() }

// WriteHeader runs EncodeHeaders once and commits exactly ONE status line.
func (fw *filterWriter) WriteHeader(status int) {
	if fw.headersDone {
		// §3.4: never write a second HTTP status line on a started stream.
		fw.s.Runtime.Warnf("[filterWriter] WARN §3.4: second WriteHeader(%d) ignored "+
			"(ResponseStarted=true); downgrading to AppendEncodedData", status)
		return
	}
	fw.headersDone = true
	fw.status = status
	ctx := fw.ctx

	// Build a HeaderMap from the response headers, run the encode chain.
	hm := a2a.NewHeaderMap(fw.w.Header())
	fw.s.Runtime.EncodeHeaders(fw.s.Chain, ctx, hm, false)

	ct := hm.Get("Content-Type")
	fw.isSSE = strings.Contains(strings.ToLower(ct), "text/event-stream") ||
		ctx.RequestHeaders.Get(filters.HdrStreaming) == "1"
	if fw.isSSE {
		hm.Set("Content-Type", "text/event-stream; charset=utf-8")
		hm.Del("Content-Length") // §3.4 HARD RULE 2
		hm.Set("Cache-Control", "no-cache")
		hm.Set("Connection", "keep-alive")
		hm.Set("X-Accel-Buffering", "no")
	}
	hm.ApplyTo(fw.w.Header())

	fw.w.WriteHeader(status)
	ctx.ResponseStarted = true // gate closes here
}

// Write runs EncodeData on each chunk.
func (fw *filterWriter) Write(p []byte) (int, error) {
	if !fw.headersDone {
		fw.WriteHeader(http.StatusOK)
	}
	ctx := fw.ctx

	out, appended := fw.s.Runtime.EncodeData(fw.s.Chain, ctx, p, false)

	// §3.4: appended bytes (SSE error frame + [DONE] from a mid-stream
	// guardrail hit) are written to the CURRENT stream; no new status line.
	if len(appended) > 0 {
		fw.w.Write(appended) //nolint:errcheck
		if fw.flusher != nil {
			fw.flusher.Flush()
		}
		fw.errDone = true
		// Skip the offending chunk; keep the connection open (no close/RST).
		return len(p), nil
	}
	if fw.errDone {
		// Terminal error already emitted: drain upstream bytes silently so the
		// stream ends cleanly for the client (curl exit 0, no (18)).
		return len(p), nil
	}

	if fw.isSSE {
		if len(out) > 0 {
			fw.w.Write(out) //nolint:errcheck
			if fw.flusher != nil {
				fw.flusher.Flush() // every frame
			}
		}
		return len(p), nil
	}

	// Non-stream: buffer until finalise so OutputGuardrailFilter can replace
	// the whole body with a 200 GUARDRAIL_REJECTED envelope.
	fw.buffered.Write(out)
	return len(p), nil
}

// Flush forwards an explicit flush (httputil.ReverseProxy calls this with
// FlushInterval=-1). For SSE we already flush per Write; for buffered
// non-stream we flush the accumulated body.
func (fw *filterWriter) Flush() {
	if !fw.headersDone {
		return
	}
	if fw.isSSE {
		if fw.flusher != nil {
			fw.flusher.Flush()
		}
		return
	}
	if fw.buffered.Len() > 0 {
		fw.w.Write(fw.buffered.Bytes()) //nolint:errcheck
		fw.buffered.Reset()
		if fw.flusher != nil {
			fw.flusher.Flush()
		}
	}
}

// finalise is called after ReverseProxy.ServeHTTP returns.
func (fw *filterWriter) finalise() {
	ctx := fw.ctx
	// Last EncodeData with endStream=true (adds "upstream.end" span event).
	_, appended := fw.s.Runtime.EncodeData(fw.s.Chain, ctx, nil, true)
	if len(appended) > 0 && fw.isSSE {
		fw.w.Write(appended) //nolint:errcheck
		if fw.flusher != nil {
			fw.flusher.Flush()
		}
		fw.errDone = true
	}

	if !fw.isSSE {
		body := fw.buffered.Bytes()
		fw.buffered.Reset()
		// Non-stream output guardrail: 200 + GUARDRAIL_REJECTED envelope.
		if ctx.RequestHeaders.Get(filters.HdrGuardRejected) == "1" {
			env := a2a.NewError(a2a.CodeGuardrailRejected, "blocked by output guardrail", ctx.RequestID)
			b, _ := json.Marshal(env)
			body = b
			fw.w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		if len(body) > 0 {
			fw.w.Write(body) //nolint:errcheck
		}
		if fw.flusher != nil {
			fw.flusher.Flush()
		}
	} else if !fw.errDone {
		// Upstream closed without a terminal frame: emit a clean [DONE].
		// NEVER RST/FIN (§3.4 HARD RULE 3).
		log.Printf("[filterWriter] WARN §3.4: stream ended without [DONE]; "+
			"emitting clean data: [DONE] (no RST). requestId=%s", ctx.RequestID)
		fw.w.Write([]byte(a2a.SSEDoneFrame())) //nolint:errcheck
		if fw.flusher != nil {
			fw.flusher.Flush()
		}
	}

	status := fw.status
	if status == 0 {
		status = http.StatusOK
	}
	if ctx.Span != nil {
		ctx.Span.Set("http.status_code", status) // integer (§1.8 / E23)
	}
	fw.s.Runtime.Destroy(fw.s.Chain, ctx, a2a.ReasonNormal)
}
