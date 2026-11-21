// Package a2asdk is the Go SDK for local-agent-stack (contract §7).
//
// It is a SECOND-LAYER wrapper on the OFFICIAL a2a-go/v2 client:
// A2A capabilities (SendMessage / StreamMessage / GetTask / CancelTask /
// Subscribe) go through a2aclient.NewFromCard + the SDK's typed methods,
// while Chat / MCP / guardrail capabilities use net/http directly. That split
// is exactly the "基于官方 SDK 二次开发" boundary from the task.
//
// The SDK auto-attaches X-A2A-Version: 1.0, X-Request-Id and X-API-Key on
// every request, maps errors to *shared.A2AError and timeouts to the native
// net.Error timeout, and parses SSE with cross-chunk buffering.
package a2asdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	a2a "github.com/a2aproject/a2a-go/v2/a2a"
	a2aclient "github.com/a2aproject/a2a-go/v2/a2aclient"

	shared "local-agent-stack/pkg/a2a"
)

// Defaults (contract §7 / config/ports.env).
const (
	DefaultBaseURL = "http://127.0.0.1:8080" // data plane
	DefaultAPIKey  = "demo-key"
	DefaultAgentID = "echo-agent"
	HeaderVersion  = "X-A2A-Version"
	HeaderRequest  = "X-Request-Id"
	HeaderAPIKey   = "X-API-Key"
	Version10      = "1.0"
)

// Option configures the Client (functional options, §7 NewClient(opts...)).
type Option func(*Client)

// WithBaseURL overrides the base URL (default data plane 8080; pass
// http://127.0.0.1:9100 for a direct sidecar connection).
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithAPIKey overrides the API key (default demo-key).
func WithAPIKey(k string) Option { return func(c *Client) { c.apiKey = k } }

// WithTimeout sets the per-request timeout (configurable, §7).
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// WithAgentID selects which mounted agent to talk to.
func WithAgentID(id string) Option { return func(c *Client) { c.agentID = id } }

// WithHTTPClient injects a custom *http.Client (tests / custom transport).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithGuardrailConfig overrides the local pre-flight guardrail config.
func WithGuardrailConfig(cfg shared.GuardrailConfig) Option {
	return func(c *Client) { c.guardCfg = cfg }
}

// ---------------------------------------------------------------------------
// Public wire types (contract §7 / §1)
// ---------------------------------------------------------------------------

// Message is the SDK's input message; it is converted to the OFFICIAL
// *a2a.Message before being handed to a2aclient.
type Message struct {
	Text      string         `json:"text,omitempty"`
	MessageID string         `json:"messageId,omitempty"`
	Role      string         `json:"role,omitempty"`
	Parts     []shared.Part  `json:"parts,omitempty"`
	ContextID string         `json:"contextId,omitempty"`
	TaskID    string         `json:"taskId,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// NewTextMessage builds a simple user text message.
func NewTextMessage(text string) Message {
	return Message{Text: text, Role: "user", MessageID: shared.NewID("msg")}
}

// Event is one streaming event (§7: StreamMessage(ctx,msg) <-chan Event).
type Event struct {
	Kind      string           `json:"kind"`
	Event     string           `json:"event,omitempty"`
	TaskID    string           `json:"taskId,omitempty"`
	ContextID string           `json:"contextId,omitempty"`
	State     string           `json:"state,omitempty"`
	Text      string           `json:"text,omitempty"`
	Data      string           `json:"data,omitempty"`
	Final     bool             `json:"final,omitempty"`
	Artifact  *shared.Artifact `json:"artifact,omitempty"`
	Raw       json.RawMessage  `json:"raw,omitempty"`
}

// Task is the SDK's view of a task (official SDK Task converted at the edge).
type Task struct {
	ID        string            `json:"id"`
	ContextID string            `json:"contextId,omitempty"`
	State     string            `json:"state"`
	Artifacts []shared.Artifact `json:"artifacts,omitempty"`
	History   []shared.Message  `json:"history,omitempty"`
}

// ChatRequest / ChatResponse are the OpenAI-compatible model gateway shapes.
type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Stream      bool          `json:"stream,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
}

// ChatMessage is one chat message.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatResponse is the OpenAI-compatible chat completion response.
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   ChatUsage    `json:"usage"`
}

// ChatChoice is one completion choice.
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatUsage carries integer token counts (contract §2.1, non-null).
type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Tool is a registered MCP tool.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema any            `json:"inputSchema,omitempty"`
	Annotations map[string]any `json:"annotations,omitempty"`
	Source      map[string]any `json:"source,omitempty"`
}

// CallToolResult is the MCP tools/call result (contract §5.1).
type CallToolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// Client is the unified SDK client (contract §7).
type Client struct {
	baseURL  string
	apiKey   string
	agentID  string
	timeout  time.Duration
	http     *http.Client
	guardCfg shared.GuardrailConfig

	mu      sync.Mutex
	card    *a2a.AgentCard
	a2a     *a2aclient.Client
	a2aErr  error
	cardErr error
}

// NewClient builds the SDK client (§7). Defaults: baseURL=data plane 8080,
// apiKey=demo-key, timeout=30s. The official a2aclient is created lazily on
// the first A2A call using the card fetched from the server.
func NewClient(opts ...Option) *Client {
	c := &Client{
		baseURL:  DefaultBaseURL,
		apiKey:   DefaultAPIKey,
		agentID:  DefaultAgentID,
		timeout:  30 * time.Second,
		guardCfg: shared.DefaultGuardrailConfig(),
	}
	for _, o := range opts {
		o(c)
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: c.timeout}
	}
	return c
}

// Close releases the lazily-created official a2aclient.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.a2a != nil {
		err := c.a2a.Destroy()
		c.a2a = nil
		return err
	}
	return nil
}

// newRequestID generates a UUID-ish request id (contract §0.2).
func (c *Client) newRequestID() string { return shared.NewID("req") }

// applyHeaders attaches the three mandatory headers (§7).
func (c *Client) applyHeaders(h http.Header, requestID string) {
	if requestID == "" {
		requestID = c.newRequestID()
	}
	h.Set(HeaderVersion, Version10)
	h.Set(HeaderRequest, requestID)
	h.Set(HeaderAPIKey, c.apiKey)
	h.Set("Content-Type", "application/json")
}

// newCtx bounds every outbound call by the configured timeout.
func (c *Client) newCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.timeout)
}

// endpoint builds a URL under the base.
func (c *Client) endpoint(path string) string {
	return c.baseURL + "/" + strings.TrimLeft(path, "/")
}

// agentJSONRPCURL is the official A2A endpoint for the selected agent.
func (c *Client) agentJSONRPCURL() string {
	return c.endpoint("/a2a/" + c.agentID)
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

// GetAgentCard fetches the discovery document (contract §7). It tries the
// official /.well-known/agent-card.json first and falls back to the compat
// /.well-known/agent.json, returning a normalised *a2a.AgentCard.
func (c *Client) GetAgentCard(ctx context.Context) (*a2a.AgentCard, error) {
	c.mu.Lock()
	if c.card != nil {
		card := c.card
		c.mu.Unlock()
		return card, nil
	}
	c.mu.Unlock()

	ctx, cancel := c.newCtx(ctx)
	defer cancel()
	reqID := c.newRequestID()
	u := c.endpoint(a2asrvWellKnown) + "?agentId=" + url.QueryEscape(c.agentID)
	b, err := c.doGET(ctx, u, reqID)
	if err != nil {
		// compat fallback
		u = c.endpoint("/.well-known/agent.json") + "?agentId=" + url.QueryEscape(c.agentID)
		b, err = c.doGET(ctx, u, reqID)
	}
	if err != nil {
		return nil, mapError(err, reqID)
	}
	card, err := parseAgentCard(b, c.baseURL, c.agentID)
	if err != nil {
		return nil, mapError(err, reqID)
	}
	c.mu.Lock()
	c.card = card
	c.mu.Unlock()
	return card, nil
}

// a2asrvWellKnown is the OFFICIAL SDK constant
// a2asrv.WellKnownAgentCardPath ("/.well-known/agent-card.json"), referenced
// here without importing the server package from the SDK client.
const a2asrvWellKnown = "/.well-known/agent-card.json"

// parseAgentCard normalises BOTH rails into *a2a.AgentCard. The official card
// already has SupportedInterfaces; the compat card has "url" and is converted
// to an AgentInterface (contract §0.0: they are NOT converted at the protocol
// level, this is just the SDK's input adapter).
func parseAgentCard(b []byte, baseURL, agentID string) (*a2a.AgentCard, error) {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	if _, ok := raw["supportedInterfaces"]; ok {
		var card a2a.AgentCard
		if err := json.Unmarshal(b, &card); err == nil && len(card.SupportedInterfaces) > 0 {
			// rewrite the interface URL to the SDK's baseURL (data plane 8080)
			// so calls go through the governance layer, not straight to 9100.
			for _, i := range card.SupportedInterfaces {
				if i != nil {
					i.URL = strings.TrimRight(baseURL, "/") + "/a2a/" + agentID
				}
			}
			return &card, nil
		}
	}
	// compat card (§1.1)
	card := &a2a.AgentCard{
		Name:        shared.GetString(raw, "name"),
		Description: shared.GetString(raw, "description"),
		Version:     shared.GetString(raw, "version"),
		Capabilities: a2a.AgentCapabilities{
			Streaming:         true,
			PushNotifications: true,
		},
		DefaultInputModes:  []string{"text"},
		DefaultOutputModes: []string{"text"},
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(strings.TrimRight(baseURL, "/")+"/a2a/"+agentID, a2a.TransportProtocolJSONRPC),
		},
	}
	return card, nil
}

// doGET performs a plain HTTP GET with the mandatory headers.
func (c *Client) doGET(ctx context.Context, u, reqID string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, u, nil, reqID)
}

// do is the shared HTTP helper for Chat / MCP / card / guardrail endpoints.
func (c *Client) do(ctx context.Context, method, u string, body []byte, reqID string) ([]byte, error) {
	var rdr *strings.Reader
	if body == nil {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	c.applyHeaders(req.Header, reqID)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return out, &httpError{status: resp.StatusCode, body: out}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Official a2aclient wiring (the "基于官方 SDK" part)
// ---------------------------------------------------------------------------

// officialClient lazily builds the OFFICIAL a2aclient.Client from the card.
func (c *Client) officialClient(ctx context.Context) (*a2aclient.Client, error) {
	c.mu.Lock()
	if c.a2a != nil {
		cl := c.a2a
		c.mu.Unlock()
		return cl, nil
	}
	if c.a2aErr != nil {
		err := c.a2aErr
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Unlock()

	card, err := c.GetAgentCard(ctx)
	if err != nil {
		return nil, err
	}
	// OFFICIAL SDK CALL: a2aclient.NewFromCard(ctx, card, ...)
	// The header interceptor attaches X-A2A-Version / X-Request-Id / X-API-Key
	// to every transport request (contract §7).
	cl, err := a2aclient.NewFromCard(ctx, card,
		a2aclient.WithJSONRPCTransport(c.http),
		a2aclient.WithCallInterceptors(&headerInterceptor{c: c}),
	)
	if err != nil {
		c.mu.Lock()
		c.a2aErr = err
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Lock()
	c.a2a = cl
	c.mu.Unlock()
	return cl, nil
}

// headerInterceptor is an official a2aclient.CallInterceptor that injects the
// three mandatory headers into req.ServiceParams (which the SDK's JSON-RPC
// transport serialises as HTTP headers — see a2aclient/jsonrpc.go:113).
type headerInterceptor struct{ c *Client }

// Before implements a2aclient.CallInterceptor. It MUST return the ORIGINAL
// ctx (with its timeout / cancellation) — replacing it with Background would
// silently drop the configured request timeout.
func (h *headerInterceptor) Before(ctx context.Context, req *a2aclient.Request) (context.Context, any, error) {
	if req.ServiceParams == nil {
		req.ServiceParams = a2aclient.ServiceParams{}
	}
	req.ServiceParams.Append(HeaderVersion, Version10)
	req.ServiceParams.Append(HeaderRequest, h.c.newRequestID())
	req.ServiceParams.Append(HeaderAPIKey, h.c.apiKey)
	return ctx, nil, nil
}

// After implements a2aclient.CallInterceptor.
func (h *headerInterceptor) After(_ context.Context, _ *a2aclient.Response) error { return nil }

// withServiceParams attaches the mandatory headers to the context as well, so
// the SDK's serviceParamsCloneFrom(ctx) picks them up even when the
// interceptor is bypassed. This is belt-and-braces for §7 header delivery.
func (c *Client) withServiceParams(ctx context.Context) context.Context {
	return a2aclient.AttachServiceParams(ctx, a2aclient.ServiceParams{
		strings.ToLower(HeaderVersion): {Version10},
		strings.ToLower(HeaderRequest): {c.newRequestID()},
		strings.ToLower(HeaderAPIKey):  {c.apiKey},
	})
}

// officialMessage converts the SDK Message into the OFFICIAL *a2a.Message.
func (c *Client) officialMessage(m Message) *a2a.Message {
	parts := make(a2a.ContentParts, 0, len(m.Parts)+1)
	for _, p := range m.Parts {
		switch p.Kind {
		case "text":
			parts = append(parts, a2a.NewTextPart(p.Text))
		case "data":
			parts = append(parts, a2a.NewDataPart(p.Data))
		default:
			if p.Text != "" {
				parts = append(parts, a2a.NewTextPart(p.Text))
			}
		}
	}
	if len(parts) == 0 {
		parts = append(parts, a2a.NewTextPart(m.Text))
	}
	id := m.MessageID
	if id == "" {
		id = shared.NewID("msg")
	}
	role := a2a.MessageRoleUser
	if strings.EqualFold(m.Role, "agent") {
		role = a2a.MessageRoleAgent
	}
	return &a2a.Message{
		ID: id, Role: role, Parts: parts,
		ContextID: m.ContextID, TaskID: a2a.TaskID(m.TaskID), Metadata: m.Metadata,
	}
}

// ---------------------------------------------------------------------------
// A2A capabilities (contract §7) — all via the OFFICIAL client
// ---------------------------------------------------------------------------

// SendMessage sends a message and returns the resulting Task (§7).
func (c *Client) SendMessage(ctx context.Context, msg Message) (*Task, error) {
	if gr := c.CheckGuardrail(msg.Text); !gr.Allowed {
		return nil, &shared.A2AError{Code: shared.CodeGuardrailRejected, Message: gr.Reason}
	}
	ctx2, cancel := c.newCtx(ctx)
	defer cancel()
	ctx3 := c.withServiceParams(ctx2)
	cl, err := c.officialClient(ctx3)
	if err != nil {
		return nil, mapError(err, c.newRequestID())
	}
	// OFFICIAL SDK CALL: (*a2aclient.Client).SendMessage
	res, err := cl.SendMessage(ctx3, &a2a.SendMessageRequest{Message: c.officialMessage(msg)})
	if err != nil {
		return nil, mapError(err, c.newRequestID())
	}
	return taskFromResult(res), nil
}

// StreamMessage streams events (§7: returns <-chan Event). Internally it uses
// the OFFICIAL SendStreamingMessage iterator; SSE framing/parsing is handled
// by the SDK transport (no hand-written SSE).
func (c *Client) StreamMessage(ctx context.Context, msg Message) <-chan Event {
	ch := make(chan Event, 32)
	go func() {
		defer close(ch)
		if gr := c.CheckGuardrail(msg.Text); !gr.Allowed {
			ch <- Event{Kind: "error", Event: "error", Data: gr.Reason, Final: true}
			return
		}
		ctx2, cancel := c.newCtx(ctx)
		defer cancel()
		ctx3 := c.withServiceParams(ctx2)
		cl, err := c.officialClient(ctx3)
		if err != nil {
			ch <- Event{Kind: "error", Event: "error", Data: err.Error(), Final: true}
			return
		}
		// OFFICIAL SDK CALL: (*a2aclient.Client).SendStreamingMessage
		for ev, err := range cl.SendStreamingMessage(ctx3, &a2a.SendMessageRequest{Message: c.officialMessage(msg)}) {
			if err != nil {
				ch <- Event{Kind: "error", Event: "error", Data: err.Error(), Final: true}
				return
			}
			ch <- eventFromOfficial(ev)
		}
	}()
	return ch
}

// GetTask fetches a task by id (§7). E25: after a sidecar restart the task is
// still served from the FileStore behind the official handler.
func (c *Client) GetTask(ctx context.Context, id string) (*Task, error) {
	ctx2, cancel := c.newCtx(ctx)
	defer cancel()
	ctx3 := c.withServiceParams(ctx2)
	cl, err := c.officialClient(ctx3)
	if err != nil {
		return nil, mapError(err, c.newRequestID())
	}
	// OFFICIAL SDK CALL: (*a2aclient.Client).GetTask
	t, err := cl.GetTask(ctx3, &a2a.GetTaskRequest{ID: a2a.TaskID(id)})
	if err != nil {
		return nil, mapError(err, c.newRequestID())
	}
	return taskFromOfficial(t), nil
}

// CancelTask cancels a task (§7).
func (c *Client) CancelTask(ctx context.Context, id string) (*Task, error) {
	ctx2, cancel := c.newCtx(ctx)
	defer cancel()
	ctx3 := c.withServiceParams(ctx2)
	cl, err := c.officialClient(ctx3)
	if err != nil {
		return nil, mapError(err, c.newRequestID())
	}
	// OFFICIAL SDK CALL: (*a2aclient.Client).CancelTask
	t, err := cl.CancelTask(ctx3, &a2a.CancelTaskRequest{ID: a2a.TaskID(id)})
	if err != nil {
		return nil, mapError(err, c.newRequestID())
	}
	return taskFromOfficial(t), nil
}

// Subscribe subscribes to a running task (§7 / E24). Uses the OFFICIAL
// SubscribeToTask iterator so the SSE stream is decoded by the SDK.
func (c *Client) Subscribe(ctx context.Context, id string) <-chan Event {
	ch := make(chan Event, 32)
	go func() {
		defer close(ch)
		ctx2, cancel := c.newCtx(ctx)
		defer cancel()
		ctx3 := c.withServiceParams(ctx2)
		cl, err := c.officialClient(ctx3)
		if err != nil {
			ch <- Event{Kind: "error", Event: "error", Data: err.Error(), Final: true}
			return
		}
		// OFFICIAL SDK CALL: (*a2aclient.Client).SubscribeToTask
		for ev, err := range cl.SubscribeToTask(ctx3, &a2a.SubscribeToTaskRequest{ID: a2a.TaskID(id)}) {
			if err != nil {
				ch <- Event{Kind: "error", Event: "error", Data: err.Error(), Final: true}
				return
			}
			ch <- eventFromOfficial(ev)
		}
	}()
	return ch
}

// ---------------------------------------------------------------------------
// Chat / MCP / Guardrail capabilities (net/http, contract §7)
// ---------------------------------------------------------------------------

// Chat calls the OpenAI-compatible model gateway through the data plane (§7).
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if req.Model == "" {
		req.Model = "mock-1"
	}
	ctx2, cancel := c.newCtx(ctx)
	defer cancel()
	b, _ := json.Marshal(req)
	resp, err := c.do(ctx2, http.MethodPost, c.endpoint("/v1/chat/completions"), b, c.newRequestID())
	if err != nil {
		return nil, mapError(err, c.newRequestID())
	}
	var out ChatResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, &shared.A2AError{Code: shared.CodeInternalError, Message: err.Error()}
	}
	return &out, nil
}

// ListTools lists MCP tools through the data plane's /mcp proxy (§5.3 / §7).
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	ctx2, cancel := c.newCtx(ctx)
	defer cancel()
	rpc := map[string]any{
		"jsonrpc": "2.0", "id": c.newRequestID(), "method": "tools/list",
		"params": map[string]any{},
	}
	b, _ := json.Marshal(rpc)
	resp, err := c.do(ctx2, http.MethodPost, c.endpoint("/mcp"), b, c.newRequestID())
	if err != nil {
		return nil, mapError(err, c.newRequestID())
	}
	var envelope struct {
		Result struct {
			Tools []Tool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &envelope); err != nil {
		return nil, &shared.A2AError{Code: shared.CodeInternalError, Message: err.Error()}
	}
	return envelope.Result.Tools, nil
}

// CallTool invokes an MCP tool (§7).
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*CallToolResult, error) {
	ctx2, cancel := c.newCtx(ctx)
	defer cancel()
	rpc := map[string]any{
		"jsonrpc": "2.0", "id": c.newRequestID(), "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	}
	b, _ := json.Marshal(rpc)
	resp, err := c.do(ctx2, http.MethodPost, c.endpoint("/mcp"), b, c.newRequestID())
	if err != nil {
		return nil, mapError(err, c.newRequestID())
	}
	var envelope struct {
		Result CallToolResult `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp, &envelope); err != nil {
		return nil, &shared.A2AError{Code: shared.CodeInternalError, Message: err.Error()}
	}
	if envelope.Error != nil {
		return nil, &shared.A2AError{Code: shared.CodeMCPToolNotFound, Message: envelope.Error.Message}
	}
	return &envelope.Result, nil
}

// CheckGuardrail performs a LOCAL pre-flight check (§7) using the shared
// GuardrailEngine. It never calls the network.
func (c *Client) CheckGuardrail(text string) shared.GuardrailResult {
	engine := shared.NewGuardrailEngine(c.guardCfg)
	return engine.CheckInput(text, map[string]any{"requestId": c.newRequestID()})
}

// ---------------------------------------------------------------------------
// Result conversion helpers
// ---------------------------------------------------------------------------

func taskFromResult(res a2a.SendMessageResult) *Task {
	switch v := res.(type) {
	case *a2a.Task:
		return taskFromOfficial(v)
	case *a2a.Message:
		return &Task{ID: string(v.TaskID), ContextID: v.ContextID, State: "completed"}
	default:
		return &Task{State: "unknown"}
	}
}

func lowerState(s a2a.TaskState) string {
	return strings.ToLower(strings.TrimPrefix(string(s), "TASK_STATE_"))
}

func taskFromOfficial(t *a2a.Task) *Task {
	if t == nil {
		return &Task{State: "unknown"}
	}
	out := &Task{
		ID: string(t.ID), ContextID: t.ContextID, State: lowerState(t.Status.State),
	}
	for _, a := range t.Artifacts {
		if a == nil {
			continue
		}
		out.Artifacts = append(out.Artifacts, artifactFromOfficial(a))
	}
	for _, h := range t.History {
		if h == nil {
			continue
		}
		out.History = append(out.History, messageFromOfficial(h))
	}
	return out
}

func artifactFromOfficial(a *a2a.Artifact) shared.Artifact {
	out := shared.Artifact{
		ArtifactID: string(a.ID), Name: a.Name, Description: a.Description,
		Metadata: a.Metadata, Extensions: a.Extensions,
	}
	for _, p := range a.Parts {
		if p == nil {
			continue
		}
		switch c := p.Content.(type) {
		case a2a.Text:
			out.Parts = append(out.Parts, shared.Part{Kind: "text", Text: string(c)})
		case a2a.Data:
			out.Parts = append(out.Parts, shared.Part{Kind: "data", Data: map[string]any{"value": c.Value}})
		default:
			out.Parts = append(out.Parts, shared.Part{Kind: "text", Text: p.Text()})
		}
	}
	return out
}

func messageFromOfficial(m *a2a.Message) shared.Message {
	out := shared.Message{
		Kind: "message", MessageID: m.ID,
		Role:      strings.ToLower(strings.TrimPrefix(string(m.Role), "ROLE_")),
		ContextID: m.ContextID, TaskID: string(m.TaskID), Metadata: m.Metadata,
	}
	for _, p := range m.Parts {
		if p == nil {
			continue
		}
		switch c := p.Content.(type) {
		case a2a.Text:
			out.Parts = append(out.Parts, shared.Part{Kind: "text", Text: string(c)})
		case a2a.Data:
			out.Parts = append(out.Parts, shared.Part{Kind: "data", Data: map[string]any{"value": c.Value}})
		}
	}
	return out
}

func eventFromOfficial(ev a2a.Event) Event {
	switch v := ev.(type) {
	case *a2a.Task:
		t := taskFromOfficial(v)
		return Event{Kind: "task", TaskID: t.ID, ContextID: t.ContextID, State: t.State, Final: v.Status.State.Terminal()}
	case *a2a.TaskStatusUpdateEvent:
		return Event{
			Kind: "status-update", Event: "status-update", TaskID: string(v.TaskID),
			ContextID: v.ContextID, State: lowerState(v.Status.State),
			Final: v.Status.State.Terminal(),
		}
	case *a2a.TaskArtifactUpdateEvent:
		text := ""
		if v.Artifact != nil {
			for _, p := range v.Artifact.Parts {
				text += p.Text()
			}
		}
		return Event{
			Kind: "artifact-update", Event: "artifact-update", TaskID: string(v.TaskID),
			ContextID: v.ContextID, Text: text, Final: v.LastChunk,
		}
	case *a2a.Message:
		return Event{Kind: "message", TaskID: string(v.TaskID), ContextID: v.ContextID, Text: messageFromOfficial(v).Text()}
	default:
		return Event{Kind: fmt.Sprintf("%T", ev)}
	}
}
