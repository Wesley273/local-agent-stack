package modelgw

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"local-agent-stack/pkg/a2a"
)

// ---------------------------------------------------------------------------
// OpenAI Chat Completions / Embeddings conversion (contract §2.4 rule 4).
//
// The gateway accepts a standard OpenAI request, forwards a backend-friendly
// form, and converts the backend response back into a standard OpenAI shape.
// A backend that already speaks OpenAI passes through unchanged; a legacy
// backend such as {"text":"..."} is converted. Conversion FAILURE returns
// UPSTREAM_ERROR (502) — it is never silently dropped.
// ---------------------------------------------------------------------------

var (
	// ErrConversion is returned when a backend response cannot be converted
	// into the OpenAI shape. The caller maps it to UPSTREAM_ERROR.
	ErrConversion = errors.New("upstream response conversion failed")
	// ErrInvalidRequest is returned for a malformed OpenAI request.
	ErrInvalidRequest = errors.New("invalid openai request")
)

// ChatRequest is the OpenAI chat/completions request we accept.
type ChatRequest struct {
	Model       string          `json:"model"`
	Messages    []ChatMessage   `json:"messages"`
	Stream      bool            `json:"stream"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	Raw         json.RawMessage `json:"-"`
}

// ChatMessage is one chat message (content may be a string or array).
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Usage is the OpenAI token usage; all three fields are integers (§2.1, E13/E23).
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatChoice is one completion choice.
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatResponse is the standard non-streaming OpenAI response.
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   Usage        `json:"usage"`
}

// EmbeddingData is one embedding vector.
type EmbeddingData struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

// EmbeddingResponse is the standard embeddings response (§2.2).
type EmbeddingResponse struct {
	Object string          `json:"object"`
	Data   []EmbeddingData `json:"data"`
	Model  string          `json:"model"`
	Usage  Usage           `json:"usage"`
}

// ParseChatRequest validates and normalises an OpenAI chat request.
func ParseChatRequest(body []byte) (*ChatRequest, error) {
	var probe map[string]any
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if _, ok := probe["model"]; !ok {
		return nil, fmt.Errorf("%w: missing model", ErrInvalidRequest)
	}
	if _, ok := probe["messages"]; !ok {
		return nil, fmt.Errorf("%w: missing messages", ErrInvalidRequest)
	}
	var r ChatRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if r.Raw == nil {
		r.Raw = append(json.RawMessage(nil), body...)
	}
	return &r, nil
}

// ParseEmbeddingRequest extracts model + input count from an embeddings request.
func ParseEmbeddingRequest(body []byte) (model string, inputs int, err error) {
	var m map[string]any
	if err = json.Unmarshal(body, &m); err != nil {
		return "", 0, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	model, _ = m["model"].(string)
	if model == "" {
		return "", 0, fmt.Errorf("%w: missing model", ErrInvalidRequest)
	}
	switch v := m["input"].(type) {
	case string:
		inputs = 1
	case []any:
		inputs = len(v)
	default:
		return "", 0, fmt.Errorf("%w: invalid input", ErrInvalidRequest)
	}
	return model, inputs, nil
}

// EstimateTokens derives the pre-deduction estimate from max_tokens.
// Falls back to a small constant so a request without max_tokens still counts.
func EstimateTokens(r *ChatRequest) int {
	if r != nil && r.MaxTokens > 0 {
		return r.MaxTokens
	}
	return 128
}

// BuildUpstreamBody produces the body forwarded to the backend. It is the
// OpenAI request itself plus a gateway marker; backends that need a different
// shape can key off X-Forwarded-By (contract §2.4 "协议转换").
func BuildUpstreamBody(r *ChatRequest) ([]byte, error) {
	if r == nil {
		return nil, ErrInvalidRequest
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConversion, err)
	}
	return b, nil
}

// ConvertChatResponse turns a backend chat response into the standard OpenAI
// shape. Handles:
//   - already-OpenAI responses (choices + usage) -> normalised usage ints
//   - legacy {"text":"..."} / {"content":"..."} / {"output":"..."} -> wrapped
//   - anything else -> ErrConversion (caller maps to UPSTREAM_ERROR)
func ConvertChatResponse(backend []byte, model string) (*ChatResponse, error) {
	var m map[string]any
	if err := json.Unmarshal(backend, &m); err != nil {
		return nil, fmt.Errorf("%w: invalid json: %v", ErrConversion, err)
	}
	if _, ok := m["choices"]; ok {
		var r ChatResponse
		if err := json.Unmarshal(backend, &r); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrConversion, err)
		}
		if r.Object == "" {
			r.Object = "chat.completion"
		}
		if r.Created == 0 {
			r.Created = time.Now().Unix()
		}
		if r.Model == "" {
			r.Model = model
		}
		if r.ID == "" {
			r.ID = "chatcmpl-" + a2a.NewID("")
		}
		normaliseUsage(&r.Usage)
		return &r, nil
	}
	// Legacy / non-OpenAI backend.
	text := firstString(m, "text", "content", "output", "response")
	if text == "" {
		return nil, fmt.Errorf("%w: backend returned neither choices nor text", ErrConversion)
	}
	usage := Usage{
		PromptTokens:     0,
		CompletionTokens: len([]rune(text)),
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	return &ChatResponse{
		ID: "chatcmpl-" + a2a.NewID(""), Object: "chat.completion",
		Created: time.Now().Unix(), Model: model,
		Choices: []ChatChoice{{Index: 0, Message: ChatMessage{Role: "assistant", Content: text}, FinishReason: "stop"}},
		Usage:   usage,
	}, nil
}

// ConvertEmbeddingResponse turns a backend embeddings response into the
// standard shape, including legacy {"embedding":[...]} / {"data":[...]}.
func ConvertEmbeddingResponse(backend []byte, model string, inputCount int) (*EmbeddingResponse, error) {
	var m map[string]any
	if err := json.Unmarshal(backend, &m); err != nil {
		return nil, fmt.Errorf("%w: invalid json: %v", ErrConversion, err)
	}
	if _, ok := m["data"]; ok {
		var r EmbeddingResponse
		if err := json.Unmarshal(backend, &r); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrConversion, err)
		}
		if r.Object == "" {
			r.Object = "list"
		}
		if r.Model == "" {
			r.Model = model
		}
		normaliseUsage(&r.Usage)
		return &r, nil
	}
	// Legacy single-vector response.
	if vec, ok := toFloats(m["embedding"]); ok {
		return &EmbeddingResponse{
			Object: "list", Model: model,
			Data:  []EmbeddingData{{Object: "embedding", Index: 0, Embedding: vec}},
			Usage: Usage{PromptTokens: inputCount, TotalTokens: inputCount},
		}, nil
	}
	return nil, fmt.Errorf("%w: backend returned neither data nor embedding", ErrConversion)
}

func normaliseUsage(u *Usage) {
	// Force integers (JSON numbers decode to float64 via map; the struct decode
	// already yields ints, but clamp negative / fill total defensively).
	if u.PromptTokens < 0 {
		u.PromptTokens = 0
	}
	if u.CompletionTokens < 0 {
		u.CompletionTokens = 0
	}
	if u.TotalTokens <= 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func toFloats(v any) ([]float64, bool) {
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]float64, 0, len(arr))
	for _, e := range arr {
		f, ok := e.(float64)
		if !ok {
			return nil, false
		}
		out = append(out, f)
	}
	return out, true
}

// SSEChunk builds one OpenAI chat.completion.chunk SSE frame (§2.1 stream).
func SSEChunk(id, model, content, role string, finish *string) string {
	delta := map[string]any{}
	if role != "" {
		delta["role"] = role
	}
	if content != "" {
		delta["content"] = content
	}
	body := map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	b, _ := json.Marshal(body)
	return a2a.SSEFrame(a2a.SSEEvent{Data: string(b)})
}

// SplitSSEChunks converts a (possibly legacy) backend chat response into a
// sequence of OpenAI SSE chunk frames ending with data: [DONE].
func SplitSSEChunks(backend []byte, model string) ([]string, error) {
	r, err := ConvertChatResponse(backend, model)
	if err != nil {
		return nil, err
	}
	text := ""
	if len(r.Choices) > 0 {
		text = r.Choices[0].Message.Content
	}
	id := r.ID
	frames := []string{SSEChunk(id, model, "", "assistant", nil)}
	// Emit content in a few chunks so E14 sees multiple chunk frames.
	runes := []rune(text)
	for i := 0; i < len(runes); i += 16 {
		end := i + 16
		if end > len(runes) {
			end = len(runes)
		}
		frames = append(frames, SSEChunk(id, model, string(runes[i:end]), "", nil))
	}
	frames = append(frames, SSEChunk(id, model, "", "", strPtr("stop")))
	frames = append(frames, a2a.SSEDoneFrame())
	return frames, nil
}

func strPtr(s string) *string { return &s }

// ParseUsageFromSSE scans a streamed body for the last usage object (used to
// settle the token reservation after a streaming response).
func ParseUsageFromSSE(b []byte) int {
	var total int
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" || payload == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(payload), &m) != nil {
			continue
		}
		if u, ok := m["usage"].(map[string]any); ok {
			if t, ok := u["total_tokens"].(float64); ok {
				total = int(t)
			}
		}
	}
	return total
}
