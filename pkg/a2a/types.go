// Package a2a implements the shared A2A protocol types, JSON-RPC envelope,
// REST colon-path parsing, SSE/NDJSON framing, guardrail errors and tracing
// contracts shared by the data-plane, AgentMecha sidecar and all SDKs.
//
// It uses ONLY the Go standard library and is intentionally offline-buildable.
package a2a

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Error envelope (contract §0.3)
// ---------------------------------------------------------------------------

// ErrorCode is the stable set of error codes shared by every service.
type ErrorCode string

const (
	CodeInvalidRequest     ErrorCode = "INVALID_REQUEST"
	CodeAuthRequired       ErrorCode = "AUTH_REQUIRED"
	CodeForbidden          ErrorCode = "FORBIDDEN"
	CodeAgentNotFound      ErrorCode = "AGENT_NOT_FOUND"
	CodeModelNotFound      ErrorCode = "MODEL_NOT_FOUND"
	CodeMCPToolNotFound    ErrorCode = "MCP_TOOL_NOT_FOUND"
	CodeTaskNotFound       ErrorCode = "TASK_NOT_FOUND"
	CodeRateLimited        ErrorCode = "RATE_LIMITED"
	CodeGuardrailRejected  ErrorCode = "GUARDRAIL_REJECTED"
	CodeUpstreamError      ErrorCode = "UPSTREAM_ERROR"
	CodeUpstreamTimeout    ErrorCode = "UPSTREAM_TIMEOUT"
	CodeStreamClosed       ErrorCode = "STREAM_CLOSED"
	CodeInternalError      ErrorCode = "INTERNAL_ERROR"
)

var errorHTTPStatus = map[ErrorCode]int{
	CodeInvalidRequest:    400,
	CodeAuthRequired:      401,
	CodeForbidden:         403,
	CodeAgentNotFound:     404,
	CodeModelNotFound:     404,
	CodeMCPToolNotFound:   404,
	CodeTaskNotFound:      404,
	CodeRateLimited:       429,
	CodeGuardrailRejected: 200, // stream: SSE error frame; non-stream: 200 envelope
	CodeUpstreamError:     502,
	CodeUpstreamTimeout:   504,
	CodeStreamClosed:      200,
	CodeInternalError:     500,
}

// HTTPStatus returns the canonical HTTP status for a code.
func HTTPStatus(c ErrorCode) int {
	if s, ok := errorHTTPStatus[c]; ok {
		return s
	}
	return 500
}

// ErrorEnvelope is the unified error body for every service.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the inner error object.
type ErrorBody struct {
	Code       ErrorCode      `json:"code"`
	Message    string         `json:"message"`
	RequestID  string         `json:"requestId,omitempty"`
	HTTPStatus int            `json:"httpStatus"`
	Detail     map[string]any `json:"detail,omitempty"`
}

// NewError builds a fully-populated error envelope.
func NewError(code ErrorCode, msg, requestID string) ErrorEnvelope {
	return ErrorEnvelope{Error: ErrorBody{
		Code: code, Message: msg, RequestID: requestID,
		HTTPStatus: HTTPStatus(code), Detail: map[string]any{},
	}}
}

// A2AError is the typed error used by Go SDK / internal callers.
type A2AError struct {
	Code      ErrorCode
	Message   string
	RequestID string
}

func (e *A2AError) Error() string {
	return fmt.Sprintf("%s: %s (requestId=%s)", e.Code, e.Message, e.RequestID)
}

// AsA2AError extracts a typed A2AError from any error.
func AsA2AError(err error) (*A2AError, bool) {
	var ae *A2AError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// Core A2A domain types (contract §1.1–§1.3)
// ---------------------------------------------------------------------------

// Part is a single content part of a Message or Artifact.
// Kind is one of "text", "data", "file". Exactly one payload field is set.
type Part struct {
	Kind string         `json:"kind"`
	Text string         `json:"text,omitempty"`
	Data map[string]any `json:"data,omitempty"`
	File *FilePart      `json:"file,omitempty"`
}

// FilePart carries inline (base64) or URI file content.
type FilePart struct {
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Bytes    string `json:"bytes,omitempty"` // base64
	URI      string `json:"uri,omitempty"`
}

// Message is an A2A message.
type Message struct {
	Kind      string         `json:"kind"`
	MessageID string         `json:"messageId"`
	Role      string         `json:"role"` // "user" | "agent"
	Parts     []Part         `json:"parts"`
	ContextID string         `json:"contextId,omitempty"`
	TaskID    string         `json:"taskId,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// NewTextMessage is a convenience constructor.
func NewTextMessage(id, role, text string) Message {
	return Message{
		Kind: "message", MessageID: id, Role: role,
		Parts: []Part{{Kind: "text", Text: text}},
		Metadata: map[string]any{},
	}
}

// Text concatenates all text parts (used by guardrails and echo agents).
func (m Message) Text() string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Kind == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// TaskState is the fixed state enumeration from contract §1.3.
type TaskState string

const (
	StateSubmitted      TaskState = "submitted"
	StateWorking        TaskState = "working"
	StateInputRequired  TaskState = "input-required"
	StateAuthRequired   TaskState = "auth-required"
	StateCompleted      TaskState = "completed"
	StateFailed         TaskState = "failed"
	StateCanceled       TaskState = "canceled"
	StateRejected       TaskState = "rejected"
	StateUnknown        TaskState = "unknown"
)

// IsTerminal reports whether a task reached a final state.
func (s TaskState) IsTerminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCanceled, StateRejected:
		return true
	}
	return false
}

// TaskStatus is the mutable status of a task.
type TaskStatus struct {
	State     TaskState `json:"state"`
	Message   *Message  `json:"message,omitempty"`
	Timestamp string    `json:"timestamp,omitempty"`
}

// Artifact is a produced output of a task.
type Artifact struct {
	ArtifactID  string         `json:"artifactId"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parts       []Part         `json:"parts"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Extensions  []string       `json:"extensions,omitempty"`
}

// ArtifactText concatenates text parts of an artifact.
func (a Artifact) ArtifactText() string {
	var b strings.Builder
	for _, p := range a.Parts {
		if p.Kind == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// Task is the central A2A object.
type Task struct {
	Kind      string         `json:"kind"`
	ID        string         `json:"id"`
	ContextID string         `json:"contextId,omitempty"`
	Status    TaskStatus     `json:"status"`
	Artifacts []Artifact     `json:"artifacts,omitempty"`
	History   []Message      `json:"history,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// NewTask creates a task in "submitted" state.
func NewTask(id, contextID string, msg Message) *Task {
	return &Task{
		Kind: "task", ID: id, ContextID: contextID,
		Status: TaskStatus{State: StateSubmitted, Timestamp: Now()},
		History: []Message{msg},
		Artifacts: []Artifact{},
		Metadata: map[string]any{},
	}
}

// AgentCard is the discovery document (contract §1.1).
type AgentCard struct {
	ProtocolVersion   string            `json:"protocolVersion"`
	Name              string            `json:"name"`
	Description       string            `json:"description"`
	URL               string            `json:"url"`
	Version           string            `json:"version"`
	Capabilities      Capabilities      `json:"capabilities"`
	DefaultInputModes []string          `json:"defaultInputModes"`
	DefaultOutputModes []string         `json:"defaultOutputModes"`
	Skills            []Skill           `json:"skills"`
	SecuritySchemes   map[string]any    `json:"securitySchemes,omitempty"`
	Security          []map[string]any  `json:"security,omitempty"`
	Provider          *Provider         `json:"provider,omitempty"`
}

// Capabilities advertises A2A features.
type Capabilities struct {
	Streaming              bool `json:"streaming"`
	PushNotifications      bool `json:"pushNotifications"`
	StateTransitionHistory bool `json:"stateTransitionHistory"`
}

// Skill describes one capability of an Agent.
type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples,omitempty"`
	InputModes  []string `json:"inputModes,omitempty"`
	OutputModes []string `json:"outputModes,omitempty"`
}

// Provider identifies the serving organization.
type Provider struct {
	Organization string `json:"organization"`
	URL          string `json:"url,omitempty"`
}

// Now returns an RFC3339 timestamp in local zone.
func Now() string { return time.Now().Format(time.RFC3339) }

// ---------------------------------------------------------------------------
// JSON-RPC 2.0 envelope (contract §1.4)
// ---------------------------------------------------------------------------

// RPCRequest is a JSON-RPC 2.0 request.
type RPCRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

// RPCError is the JSON-RPC error member.
type RPCError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

// RPCResponse is a JSON-RPC 2.0 response.
type RPCResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

const (
	RPCParseError     = -32700
	RPCInvalidRequest = -32600
	RPCMethodNotFound = -32601
	RPCInvalidParams  = -32602
	RPCInternalError  = -32603
)

// Method constants (JSON-RPC names).
const (
	MethodSend        = "message/send"
	MethodStream      = "message/stream"
	MethodTasksGet    = "tasks/get"
	MethodTasksCancel = "tasks/cancel"
	MethodSubscribe   = "tasks/subscribe"
	MethodCard        = "agent/getAuthenticatedExtendedCard"
)

// RPCErrorResponse builds a JSON-RPC error response.
func RPCErrorResponse(id any, code int, msg string, ec ErrorCode) RPCResponse {
	return RPCResponse{JSONRPC: "2.0", ID: id, Error: &RPCError{
		Code: code, Message: msg, Data: map[string]any{"code": string(ec)},
	}}
}

// DecodeRPCRequest parses a JSON-RPC request from raw bytes.
func DecodeRPCRequest(b []byte) (*RPCRequest, error) {
	var r RPCRequest
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParse, err)
	}
	if r.JSONRPC != "2.0" || r.Method == "" {
		return nil, ErrInvalidRequest
	}
	if r.Params == nil {
		r.Params = map[string]any{}
	}
	return &r, nil
}

var (
	ErrParse          = errors.New("json parse error")
	ErrInvalidRequest = errors.New("invalid json-rpc request")
)

// MessageFromParams extracts a Message from JSON-RPC params.
func MessageFromParams(p map[string]any) (Message, error) {
	raw, ok := p["message"]
	if !ok {
		return Message{}, fmt.Errorf("missing params.message")
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return Message{}, err
	}
	var m Message
	if err := json.Unmarshal(b, &m); err != nil {
		return Message{}, err
	}
	if m.Kind == "" {
		m.Kind = "message"
	}
	if m.MessageID == "" {
		m.MessageID = NewID("msg")
	}
	if m.Role == "" {
		m.Role = "user"
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// REST colon-path parsing (contract §1.5) — the Go ServeMux panic fix
// ---------------------------------------------------------------------------

// RestPath is the parsed form of an A2A REST path.
type RestPath struct {
	Resource string // "message" | "tasks" | "card" | "v1"
	ID       string // task id when Resource=="tasks"
	Action   string // "send" | "stream" | "cancel" | "subscribe" | ""
	Raw      string // original path
}

// ParseRestPath parses A2A REST paths WITHOUT registering them on net/http.ServeMux.
//
// Go 1.22+ ServeMux panics on patterns such as "/tasks/{id}:cancel":
//   panic: parsing "/tasks/{id}:cancel": bad wildcard segment
// A2A requires colon-suffixed method names, so we register a single "/a2a/"
// prefix handler and resolve the rest manually here.
//
// Supported (after the "/a2a/{agentId}/v1" prefix is stripped):
//   /message:send          -> {message, "", send}
//   /message:stream        -> {message, "", stream}
//   /tasks/{id}            -> {tasks, id, ""}
//   /tasks/{id}:cancel     -> {tasks, id, cancel}
//   /tasks/{id}:subscribe  -> {tasks, id, subscribe}
//   /card                  -> {card, "", ""}
func ParseRestPath(path string) (RestPath, error) {
	rp := RestPath{Raw: path}
	// Normalise: strip query, leading slash, and %3A/%3a encoded colons.
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	path = strings.TrimPrefix(path, "/")
	path = strings.ReplaceAll(path, "%3A", ":")
	path = strings.ReplaceAll(path, "%3a", ":")
	// Strip optional version prefix.
	path = strings.TrimPrefix(path, "v1/")
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) == 0 || segs[0] == "" {
		return rp, fmt.Errorf("empty rest path")
	}
	first := segs[0]
	// first may be "message:send" / "message:stream" / "tasks" / "card".
	if strings.HasPrefix(first, "message:") {
		rp.Resource = "message"
		rp.Action = strings.TrimPrefix(first, "message:")
		return rp, nil
	}
	if first == "message" && len(segs) > 1 && strings.HasPrefix(segs[1], ":") {
		rp.Resource, rp.Action = "message", strings.TrimPrefix(segs[1], ":")
		return rp, nil
	}
	if first == "card" || first == "agent.json" || first == ".well-known/agent.json" {
		rp.Resource = "card"
		return rp, nil
	}
	if first == "tasks" {
		rp.Resource = "tasks"
		if len(segs) < 2 {
			return rp, fmt.Errorf("tasks path requires an id")
		}
		idAction := segs[1]
		// Split LAST colon: ids are allowed to contain colons (e.g. "long:128000").
		if idx := strings.LastIndexByte(idAction, ':'); idx >= 0 {
			rp.ID = idAction[:idx]
			rp.Action = idAction[idx+1:]
		} else {
			rp.ID = idAction
		}
		if rp.Action != "" && rp.Action != "cancel" && rp.Action != "subscribe" {
			return rp, fmt.Errorf("unknown task action %q", rp.Action)
		}
		return rp, nil
	}
	return rp, fmt.Errorf("unknown rest resource %q", first)
}

// MethodForRest maps a parsed REST path to its JSON-RPC method name.
func (rp RestPath) MethodForRest() string {
	switch {
	case rp.Resource == "card":
		return MethodCard
	case rp.Resource == "message" && rp.Action == "stream":
		return MethodStream
	case rp.Resource == "message":
		return MethodSend
	case rp.Resource == "tasks" && rp.Action == "cancel":
		return MethodTasksCancel
	case rp.Resource == "tasks" && rp.Action == "subscribe":
		return MethodSubscribe
	case rp.Resource == "tasks":
		return MethodTasksGet
	}
	return ""
}

// ---------------------------------------------------------------------------
// SSE / NDJSON framing (contract §1.6 / §1.7)
// ---------------------------------------------------------------------------

// SSEEvent is one Server-Sent Event frame.
type SSEEvent struct {
	Event string // optional event name
	Data  string // single-line JSON or "[DONE]"
}

// SSEFrame serialises an event into a spec-compliant frame (ends with blank line).
func SSEFrame(e SSEEvent) string {
	var b strings.Builder
	if e.Event != "" {
		b.WriteString("event: " + e.Event + "\n")
	}
	// data must be single-line: escape stray newlines.
	line := strings.ReplaceAll(e.Data, "\r", " ")
	line = strings.ReplaceAll(line, "\n", "\\n")
	b.WriteString("data: " + line + "\n\n")
	return b.String()
}

// SSEStatusFrame builds a TaskStatusUpdateEvent frame.
func SSEStatusFrame(reqID string, t *Task, final bool) string {
	body := map[string]any{
		"jsonrpc": "2.0", "id": reqID,
		"result": map[string]any{
			"kind": "status-update", "taskId": t.ID, "contextId": t.ContextID,
			"status": t.Status, "final": final,
		},
	}
	b, _ := json.Marshal(body)
	return SSEFrame(SSEEvent{Event: "status-update", Data: string(b)})
}

// SSEArtifactFrame builds a TaskArtifactUpdateEvent frame.
func SSEArtifactFrame(reqID string, t *Task, a Artifact, append_, lastChunk, final bool) string {
	body := map[string]any{
		"jsonrpc": "2.0", "id": reqID,
		"result": map[string]any{
			"kind": "artifact-update", "taskId": t.ID, "contextId": t.ContextID,
			"artifact": a, "append": append_, "lastChunk": lastChunk, "final": final,
		},
	}
	b, _ := json.Marshal(body)
	return SSEFrame(SSEEvent{Event: "artifact-update", Data: string(b)})
}

// SSEErrorFrame builds the guardrail error frame (§3.4).
// It NEVER carries an HTTP status; the connection stays 200 and remains open.
func SSEErrorFrame(code ErrorCode, msg, requestID string) string {
	e := NewError(code, msg, requestID)
	b, _ := json.Marshal(map[string]any{"error": e.Error, "final": true})
	return SSEFrame(SSEEvent{Event: "error", Data: string(b)})
}

// SSEDoneFrame terminates a stream.
func SSEDoneFrame() string { return SSEFrame(SSEEvent{Data: "[DONE]"}) }

// NDJSONLine serialises one NDJSON record (single line + '\n').
func NDJSONLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ---------------------------------------------------------------------------
// IDs and helpers
// ---------------------------------------------------------------------------

var idCounter uint64

// NewID produces a process-unique-ish id with the given prefix.
// Uses crypto/rand for the random component; no external deps.
func NewID(prefix string) string {
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UnixNano()%1e9, randHex(4))
}

func randHex(n int) string {
	const hextable = "0123456789abcdef"
	b := make([]byte, n)
	if _, err := cryptoRead(b); err != nil {
		for i := range b {
			b[i] = byte(time.Now().UnixNano() + int64(i)*7919)
		}
	}
	out := make([]byte, n*2)
	for i, v := range b {
		out[i*2] = hextable[v>>4]
		out[i*2+1] = hextable[v&0x0f]
	}
	return string(out)
}

// JSONLine marshals v to a single-line JSON string (no HTML escaping issues).
func JSONLine(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// GetString safely reads a string from a map[string]any.
func GetString(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

// GetBool safely reads a bool from a map[string]any.
func GetBool(m map[string]any, k string, def bool) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	return def
}

// GetFloat safely reads a float64 from a map[string]any.
func GetFloat(m map[string]any, k string, def float64) float64 {
	if v, ok := m[k].(float64); ok {
		return v
	}
	return def
}
