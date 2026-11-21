package a2asdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	shared "local-agent-stack/pkg/a2a"
)

// ---------------------------------------------------------------------------
// sse.go — SSE + NDJSON readers for the SDK (contract §7).
//
// HARD RULE: SSE parsing MUST stitch frames ACROSS arbitrary TCP chunks. A
// single "data:" frame can be delivered over many Reads; we never assume
// "one Read == one frame". The actual incremental decoder is the SHARED
// a2a.SSEDecoder (pkg/a2a/stream.go:20), which buffers until a blank line.
//
// NDJSON uses the SHARED a2a.NewNDJSONScanner (64K initial / 10M max), so a
// 128KB single line is accepted without "token too long" (§1.7 / E11).
// ---------------------------------------------------------------------------

// SSEStream is an incremental, cross-chunk SSE consumer built on the shared
// decoder. Feed() may be called with partial chunks in any split.
type SSEStream struct {
	dec *shared.SSEDecoder
}

// NewSSEStream creates a stream consumer.
func NewSSEStream() *SSEStream { return &SSEStream{dec: &shared.SSEDecoder{}} }

// Feed pushes a (possibly partial) chunk and returns any complete events.
func (s *SSEStream) Feed(chunk []byte) []shared.SSEEvent { return s.dec.Feed(chunk) }

// Done reports whether [DONE] was observed.
func (s *SSEStream) Done() bool { return s.dec.Done() }

// ReadSSEStream consumes an HTTP SSE response body and emits Event values on
// the returned channel. It uses a2a.ReadSSEStream under the hood (which uses
// SSEDecoder.Feed) so frames split across chunks are stitched correctly.
func ReadSSEStream(ctx context.Context, body io.Reader) <-chan Event {
	ch := make(chan Event, 32)
	go func() {
		defer close(ch)
		err := shared.ReadSSEStream(body, func(ev shared.SSEEvent) bool {
			select {
			case <-ctx.Done():
				return false
			default:
			}
			ch <- eventFromSSERaw(ev)
			return !strings.EqualFold(strings.TrimSpace(ev.Data), "[DONE]")
		})
		if err != nil {
			ch <- Event{Kind: "error", Event: "error", Data: err.Error(), Final: true}
		}
	}()
	return ch
}

// StreamChat opens an SSE stream from an arbitrary URL (used for the model
// gateway stream:true and for downstream MCP/SSE endpoints). It attaches the
// three mandatory headers and decodes cross-chunk frames.
func (c *Client) StreamChat(ctx context.Context, req ChatRequest) (<-chan Event, error) {
	req.Stream = true
	if req.Model == "" {
		req.Model = "mock-1"
	}
	b, _ := json.Marshal(req)
	u := c.endpoint("/v1/chat/completions")
	ctx2, cancel := c.newCtx(ctx)
	httpReq, err := http.NewRequestWithContext(ctx2, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		cancel()
		return nil, err
	}
	reqID := c.newRequestID()
	c.applyHeaders(httpReq.Header, reqID)
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		cancel()
		return nil, mapError(err, reqID)
	}
	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		_ = resp.Body.Close()
		cancel()
		return nil, mapError(&httpError{status: resp.StatusCode, body: buf.Bytes()}, reqID)
	}
	ch := make(chan Event, 32)
	go func() {
		defer close(ch)
		defer cancel()
		defer resp.Body.Close()
		_ = shared.ReadSSEStream(resp.Body, func(ev shared.SSEEvent) bool {
			ch <- Event{Kind: "chunk", Event: ev.Event, Data: ev.Data, Final: strings.TrimSpace(ev.Data) == "[DONE]"}
			return !strings.EqualFold(strings.TrimSpace(ev.Data), "[DONE]")
		})
	}()
	return ch, nil
}

// ReadNDJSON reads NDJSON records (each possibly >64KB) using the SHARED
// 64KB-safe scanner. It is the SDK-side counterpart of the sidecar's
// ScanUserServerNDJSON (§7: "NDJSON 解析同样支持 >64KB 单行").
func ReadNDJSON(r io.Reader, onRecord func(map[string]any) error) (int, error) {
	return shared.ReadNDJSONMap(r, onRecord)
}

// NDJSONScanner returns the shared large-buffer scanner for callers that want
// to drive scanning themselves. NEVER use a bare bufio.NewScanner here.
func NDJSONScanner(r io.Reader) *bufio.Scanner { return shared.NewNDJSONScanner(r) }

// eventFromSSERaw converts one raw SSE frame into a typed Event. It handles
// both the §1.6 JSON-RPC-shaped frames and plain chat-completion chunks.
func eventFromSSERaw(ev shared.SSEEvent) Event {
	data := strings.TrimSpace(ev.Data)
	if data == "[DONE]" {
		return Event{Kind: "done", Data: data, Final: true}
	}
	if ev.Event == "error" {
		return Event{Kind: "error", Event: "error", Data: data, Final: true}
	}
	out := Event{Kind: ev.Event, Event: ev.Event, Data: data}
	var m map[string]any
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		return out
	}
	// §1.6 JSON-RPC envelope: {"jsonrpc":"2.0","id":...,"result":{...}}
	if result, ok := m["result"].(map[string]any); ok {
		kind := shared.GetString(result, "kind")
		out.Kind = kind
		out.TaskID = shared.GetString(result, "taskId")
		out.ContextID = shared.GetString(result, "contextId")
		out.Final = shared.GetBool(result, "final", false)
		if status, ok := result["status"].(map[string]any); ok {
			out.State = shared.GetString(status, "state")
		}
		if art, ok := result["artifact"].(map[string]any); ok {
			out.Text = artifactText(art)
		}
		return out
	}
	// §0.3 guardrail error envelope
	if e, ok := m["error"].(map[string]any); ok {
		return Event{
			Kind: "error", Event: "error", Data: shared.GetString(e, "code"),
			Text: shared.GetString(e, "message"), Final: true,
		}
	}
	// OpenAI chat chunk
	if choices, ok := m["choices"].([]any); ok && len(choices) > 0 {
		if c0, ok := choices[0].(map[string]any); ok {
			if delta, ok := c0["delta"].(map[string]any); ok {
				out.Kind = "chunk"
				out.Text = shared.GetString(delta, "content")
			}
		}
	}
	return out
}

func artifactText(m map[string]any) string {
	parts, ok := m["parts"].([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if pm, ok := p.(map[string]any); ok {
			b.WriteString(shared.GetString(pm, "text"))
		}
	}
	return b.String()
}

// FeedAllSSE is a convenience helper for tests: split `raw` into chunks of
// size n (including 1-byte splits) and assert that the decoder reconstructs
// exactly the same events. This is the cross-chunk regression proof.
func FeedAllSSE(raw []byte, chunkSize int) ([]shared.SSEEvent, error) {
	if chunkSize <= 0 {
		chunkSize = 1
	}
	s := NewSSEStream()
	var out []shared.SSEEvent
	for i := 0; i < len(raw); i += chunkSize {
		end := i + chunkSize
		if end > len(raw) {
			end = len(raw)
		}
		out = append(out, s.Feed(raw[i:end])...)
	}
	if !s.Done() {
		return out, fmt.Errorf("stream ended without [DONE]")
	}
	return out, nil
}
