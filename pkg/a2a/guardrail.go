package a2a

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// Guardrails & Hooks (contract §6) — AgentMecha / data-plane shared.
// Four guardrail types from the resume: tool / instruction / data / prompt.
// ---------------------------------------------------------------------------

// GuardrailConfig is the shared guardrail configuration (mirrors the
// "guardrails" section of the control-plane snapshot).
type GuardrailConfig struct {
	BlockedWords    []string `json:"blockedWords"`
	BlockedTools    []string `json:"blockedTools"`
	MaxPromptChars  int      `json:"maxPromptChars"`
	EnableInput     bool     `json:"enableInput"`
	EnableOutput    bool     `json:"enableOutput"`
	SecretPatterns  []string `json:"secretPatterns,omitempty"`
}

// DefaultGuardrailConfig is the demo configuration.
func DefaultGuardrailConfig() GuardrailConfig {
	return GuardrailConfig{
		BlockedWords:   []string{"BLOCK_ME", "secret-leak"},
		BlockedTools:   []string{"dangerous_delete", "drop_database"},
		MaxPromptChars: 8000,
		EnableInput:    true,
		EnableOutput:   true,
		SecretPatterns: []string{`sk-[A-Za-z0-9]{16,}`, `-----BEGIN [A-Z ]*PRIVATE KEY-----`},
	}
}

// GuardrailType enumerates the four kinds.
type GuardrailType string

const (
	GuardrailTool        GuardrailType = "tool"
	GuardrailInstruction GuardrailType = "instruction"
	GuardrailData        GuardrailType = "data"
	GuardrailPrompt      GuardrailType = "prompt"
)

// GuardrailResult is the outcome of one check.
type GuardrailResult struct {
	Allowed   bool          `json:"allowed"`
	Type      GuardrailType `json:"type"`
	Reason    string        `json:"reason"`
	RequestID string        `json:"requestId,omitempty"`
}

// Guardrail is the common interface.
type Guardrail interface {
	Type() GuardrailType
	Check(input string, meta map[string]any) GuardrailResult
}

// ---------------------------------------------------------------------------
// Four concrete guardrails
// ---------------------------------------------------------------------------

// PromptGuardrail checks blocked words and max prompt length.
type PromptGuardrail struct {
	cfg GuardrailConfig
}

// NewPromptGuardrail builds a prompt guardrail.
func NewPromptGuardrail(c GuardrailConfig) *PromptGuardrail { return &PromptGuardrail{cfg: c} }

// Type returns GuardrailPrompt.
func (g *PromptGuardrail) Type() GuardrailType { return GuardrailPrompt }

// Check implements Guardrail.
func (g *PromptGuardrail) Check(input string, meta map[string]any) GuardrailResult {
	r := GuardrailResult{Allowed: true, Type: GuardrailPrompt, RequestID: GetString(meta, "requestId")}
	if !g.cfg.EnableInput {
		return r
	}
	lower := strings.ToLower(input)
	for _, w := range g.cfg.BlockedWords {
		if w != "" && strings.Contains(lower, strings.ToLower(w)) {
			r.Allowed = false
			r.Reason = fmt.Sprintf("blocked word %q in prompt", w)
			return r
		}
	}
	if g.cfg.MaxPromptChars > 0 && len([]rune(input)) > g.cfg.MaxPromptChars {
		r.Allowed = false
		r.Reason = fmt.Sprintf("prompt exceeds %d chars", g.cfg.MaxPromptChars)
		return r
	}
	return r
}

// ToolGuardrail blocks dangerous tool names.
type ToolGuardrail struct {
	cfg GuardrailConfig
}

// NewToolGuardrail builds a tool guardrail.
func NewToolGuardrail(c GuardrailConfig) *ToolGuardrail { return &ToolGuardrail{cfg: c} }

// Type returns GuardrailTool.
func (g *ToolGuardrail) Type() GuardrailType { return GuardrailTool }

// Check checks "tool" in meta, or the raw input if meta has none.
func (g *ToolGuardrail) Check(input string, meta map[string]any) GuardrailResult {
	r := GuardrailResult{Allowed: true, Type: GuardrailTool, RequestID: GetString(meta, "requestId")}
	name := GetString(meta, "tool")
	if name == "" {
		name = input
	}
	for _, t := range g.cfg.BlockedTools {
		if strings.EqualFold(strings.TrimSpace(name), t) {
			r.Allowed = false
			r.Reason = fmt.Sprintf("tool %q is blocked", name)
			return r
		}
	}
	return r
}

// InstructionGuardrail checks privileged instructions and JSON-RPC method allowlist.
type InstructionGuardrail struct {
	allowedMethods map[string]bool
}

var defaultAllowedMethods = map[string]bool{
	MethodSend: true, MethodStream: true, MethodTasksGet: true,
	MethodTasksCancel: true, MethodSubscribe: true, MethodCard: true,
	"initialize": true, "tools/list": true, "tools/call": true,
	"tools/get": true, "ping": true,
}

// NewInstructionGuardrail builds an instruction guardrail.
func NewInstructionGuardrail(extra ...string) *InstructionGuardrail {
	g := &InstructionGuardrail{allowedMethods: map[string]bool{}}
	for k, v := range defaultAllowedMethods {
		g.allowedMethods[k] = v
	}
	for _, e := range extra {
		g.allowedMethods[e] = true
	}
	return g
}

// Type returns GuardrailInstruction.
func (g *InstructionGuardrail) Type() GuardrailType { return GuardrailInstruction }

// Check checks method allowlist and privileged instruction fields.
func (g *InstructionGuardrail) Check(input string, meta map[string]any) GuardrailResult {
	r := GuardrailResult{Allowed: true, Type: GuardrailInstruction, RequestID: GetString(meta, "requestId")}
	if m := GetString(meta, "method"); m != "" && !g.allowedMethods[m] {
		r.Allowed = false
		r.Reason = fmt.Sprintf("method %q is not in the allowlist", m)
		return r
	}
	lower := strings.ToLower(input)
	for _, bad := range []string{"ignore previous instructions", "override system prompt", "disable all guardrails"} {
		if strings.Contains(lower, bad) {
			r.Allowed = false
			r.Reason = "privileged instruction injection detected"
			return r
		}
	}
	return r
}

// DataGuardrail detects secret leakage in artifacts / outputs.
type DataGuardrail struct {
	cfg      GuardrailConfig
	patterns []*regexp.Regexp
}

// NewDataGuardrail compiles the configured secret patterns.
func NewDataGuardrail(c GuardrailConfig) *DataGuardrail {
	g := &DataGuardrail{cfg: c}
	for _, p := range c.SecretPatterns {
		if re, err := regexp.Compile(p); err == nil {
			g.patterns = append(g.patterns, re)
		}
	}
	return g
}

// Type returns GuardrailData.
func (g *DataGuardrail) Type() GuardrailType { return GuardrailData }

// Check implements Guardrail.
func (g *DataGuardrail) Check(input string, meta map[string]any) GuardrailResult {
	r := GuardrailResult{Allowed: true, Type: GuardrailData, RequestID: GetString(meta, "requestId")}
	if !g.cfg.EnableOutput {
		return r
	}
	for _, re := range g.patterns {
		if re.MatchString(input) {
			r.Allowed = false
			r.Reason = "sensitive data pattern in output"
			return r
		}
	}
	return r
}

// ---------------------------------------------------------------------------
// Hook mechanism (contract §6.2, aligned with OpenCode Hook)
// ---------------------------------------------------------------------------

// HookPoint is where a hook fires.
type HookPoint string

const (
	BeforeTask  HookPoint = "before_task"
	BeforeTool  HookPoint = "before_tool"
	BeforeModel HookPoint = "before_model"
	AfterModel  HookPoint = "after_model"
	AfterTask   HookPoint = "after_task"
)

// HookContext carries everything a hook may inspect or modify.
type HookContext struct {
	RequestID string
	TaskID    string
	AgentID   string
	AppID     string
	Point     HookPoint
	Input     string
	Payload   map[string]any
}

// HookResult is the hook decision.
type HookResult struct {
	Allow    bool   `json:"allow"`
	Reason   string `json:"reason,omitempty"`
	Modified any    `json:"modified,omitempty"`
}

// Hook is a pluggable interceptor.
type Hook interface {
	Name() string
	Point() HookPoint
	Priority() int
	Handle(ctx *HookContext) HookResult
}

// HookChain registers hooks and executes them in priority order.
type HookChain struct {
	mu    sync.RWMutex
	hooks []Hook
}

// NewHookChain creates an empty chain.
func NewHookChain() *HookChain { return &HookChain{} }

// Register adds a hook.
func (c *HookChain) Register(h Hook) *HookChain {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hooks = append(c.hooks, h)
	return c
}

// List returns hook descriptors (for GET /admin/hooks).
func (c *HookChain) List() []map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]map[string]any, 0, len(c.hooks))
	for _, h := range c.hooks {
		out = append(out, map[string]any{"name": h.Name(), "point": string(h.Point()), "priority": h.Priority()})
	}
	return out
}

// Run executes all hooks for a point; short-circuits on first deny.
func (c *HookChain) Run(ctx *HookContext) HookResult {
	c.mu.RLock()
	hs := append([]Hook(nil), c.hooks...)
	c.mu.RUnlock()
	// stable insertion order for equal priority, priority ascending.
	sortHooks(hs)
	for _, h := range hs {
		if h.Point() != ctx.Point {
			continue
		}
		r := h.Handle(ctx)
		if !r.Allow {
			return r
		}
	}
	return HookResult{Allow: true}
}

func sortHooks(hs []Hook) {
	// simple insertion sort: few hooks, no import cycle with sort
	for i := 1; i < len(hs); i++ {
		for j := i; j > 0 && hs[j].Priority() < hs[j-1].Priority(); j-- {
			hs[j], hs[j-1] = hs[j-1], hs[j]
		}
	}
}

// ---------------------------------------------------------------------------
// Built-in hooks
// ---------------------------------------------------------------------------

// BlockedWordsHook denies on configured blocked words.
type BlockedWordsHook struct{ cfg GuardrailConfig }

// NewBlockedWordsHook builds the hook.
func NewBlockedWordsHook(c GuardrailConfig) *BlockedWordsHook { return &BlockedWordsHook{cfg: c} }

// Name implements Hook.
func (h *BlockedWordsHook) Name() string { return "blocked-words" }

// Point implements Hook.
func (h *BlockedWordsHook) Point() HookPoint { return BeforeTask }

// Priority implements Hook.
func (h *BlockedWordsHook) Priority() int { return 10 }

// Handle implements Hook.
func (h *BlockedWordsHook) Handle(ctx *HookContext) HookResult {
	g := NewPromptGuardrail(h.cfg)
	r := g.Check(ctx.Input, map[string]any{"requestId": ctx.RequestID})
	return HookResult{Allow: r.Allowed, Reason: r.Reason}
}

// BlockedToolsHook denies on dangerous tool calls.
type BlockedToolsHook struct{ cfg GuardrailConfig }

// NewBlockedToolsHook builds the hook.
func NewBlockedToolsHook(c GuardrailConfig) *BlockedToolsHook { return &BlockedToolsHook{cfg: c} }

// Name implements Hook.
func (h *BlockedToolsHook) Name() string { return "blocked-tools" }

// Point implements Hook.
func (h *BlockedToolsHook) Point() HookPoint { return BeforeTool }

// Priority implements Hook.
func (h *BlockedToolsHook) Priority() int { return 10 }

// Handle implements Hook.
func (h *BlockedToolsHook) Handle(ctx *HookContext) HookResult {
	g := NewToolGuardrail(h.cfg)
	r := g.Check(ctx.Input, map[string]any{"tool": ctx.Input, "requestId": ctx.RequestID})
	return HookResult{Allow: r.Allowed, Reason: r.Reason}
}

// MaxLengthHook enforces a length ceiling.
type MaxLengthHook struct{ Max int }

// Name implements Hook.
func (h *MaxLengthHook) Name() string { return "max-length" }

// Point implements Hook.
func (h *MaxLengthHook) Point() HookPoint { return BeforeModel }

// Priority implements Hook.
func (h *MaxLengthHook) Priority() int { return 20 }

// Handle implements Hook.
func (h *MaxLengthHook) Handle(ctx *HookContext) HookResult {
	if h.Max > 0 && len([]rune(ctx.Input)) > h.Max {
		return HookResult{Allow: false, Reason: fmt.Sprintf("input exceeds %d chars", h.Max)}
	}
	return HookResult{Allow: true}
}

// AuditLogHook records hook activity into a SpanStore.
type AuditLogHook struct {
	Point_  HookPoint
	Store   *SpanStore
	Prio    int
}

// Name implements Hook.
func (h *AuditLogHook) Name() string { return "audit-log" }

// Point implements Hook.
func (h *AuditLogHook) Point() HookPoint { return h.Point_ }

// Priority implements Hook.
func (h *AuditLogHook) Priority() int { return h.Prio }

// Handle implements Hook (always allows, records a span).
func (h *AuditLogHook) Handle(ctx *HookContext) HookResult {
	if h.Store != nil {
		s := NewSpan(ctx.RequestID, "", "hook."+string(ctx.Point), "internal")
		s.Set("hook.point", string(ctx.Point)).Set("app.id", ctx.AppID).Set("agent.id", ctx.AgentID)
		_ = h.Store.Add(s.Finish("OK"))
	}
	return HookResult{Allow: true}
}

// DefaultHooks returns the standard hook set for the sidecar.
func DefaultHooks(cfg GuardrailConfig, store *SpanStore) []Hook {
	return []Hook{
		NewBlockedWordsHook(cfg),
		NewBlockedToolsHook(cfg),
		&MaxLengthHook{Max: cfg.MaxPromptChars},
		&AuditLogHook{Point_: AfterTask, Store: store, Prio: 90},
	}
}

// ---------------------------------------------------------------------------
// GuardrailEngine runs all four checks and returns the first denial.
// ---------------------------------------------------------------------------

// GuardrailEngine aggregates the four guardrail types.
type GuardrailEngine struct {
	cfg      GuardrailConfig
	checkers []Guardrail
}

// NewGuardrailEngine builds the engine.
func NewGuardrailEngine(c GuardrailConfig) *GuardrailEngine {
	return &GuardrailEngine{cfg: c, checkers: []Guardrail{
		NewPromptGuardrail(c),
		NewToolGuardrail(c),
		NewInstructionGuardrail(),
		NewDataGuardrail(c),
	}}
}

// CheckInput runs all four checks against user input.
func (e *GuardrailEngine) CheckInput(text string, meta map[string]any) GuardrailResult {
	for _, g := range e.checkers {
		r := g.Check(text, meta)
		if !r.Allowed {
			return r
		}
	}
	return GuardrailResult{Allowed: true, Type: GuardrailPrompt, RequestID: GetString(meta, "requestId")}
}

// CheckInputWordsOnly runs the input checks that are meaningful on a RAW HTTP
// body at the transparent-proxy layer: blocked words, blocked tools and
// instruction-injection patterns. It deliberately skips:
//   - maxPromptChars: a raw body contains JSON framing and long artifact
//     payloads (e.g. the 128KB NDJSON regression), so body length is NOT the
//     prompt length. Length enforcement belongs to the model gateway, which
//     can see the actual prompt field.
//   - the JSON-RPC method allowlist: the data plane is protocol-agnostic
//     (contract §0.0); official a2a-go/v2 uses PascalCase "SendMessage" while
//     the compat track uses "message/send". Method validation is the
//     Sidecar/application's responsibility.
//
// This is what the data plane's InputGuardrailFilter must use.
func (e *GuardrailEngine) CheckInputWordsOnly(text string, meta map[string]any) GuardrailResult {
	words := NewPromptGuardrail(GuardrailConfig{
		BlockedWords: e.cfg.BlockedWords, EnableInput: e.cfg.EnableInput,
		MaxPromptChars: 0, // disabled
	})
	for _, g := range []Guardrail{words, NewToolGuardrail(e.cfg), NewInstructionGuardrail()} {
		r := g.Check(text, meta)
		if !r.Allowed {
			return r
		}
	}
	return GuardrailResult{Allowed: true, Type: GuardrailPrompt, RequestID: GetString(meta, "requestId")}
}

// CheckOutputWordsOnly checks generated output for blocked words and secret
// patterns WITHOUT applying the INPUT prompt-length ceiling. A 128000-char
// artifact (E11) is legitimate output; maxPromptChars is an input rule.
// This is the correct output-side check for the transparent proxy and sidecar.
func (e *GuardrailEngine) CheckOutputWordsOnly(text string, meta map[string]any) GuardrailResult {
	words := NewPromptGuardrail(GuardrailConfig{
		BlockedWords: e.cfg.BlockedWords, EnableInput: e.cfg.EnableOutput,
		MaxPromptChars: 0,
	})
	for _, g := range []Guardrail{NewDataGuardrail(e.cfg), words} {
		r := g.Check(text, meta)
		if !r.Allowed {
			return r
		}
	}
	return GuardrailResult{Allowed: true, Type: GuardrailData, RequestID: GetString(meta, "requestId")}
}

// CheckOutput checks generated output (data guardrail + blocked words).
// Deprecated for proxy/stream use: prefer CheckOutputWordsOnly, which does not
// apply the input length ceiling to large artifacts.
func (e *GuardrailEngine) CheckOutput(text string, meta map[string]any) GuardrailResult {
	for _, t := range []GuardrailType{GuardrailData, GuardrailPrompt} {
		for _, g := range e.checkers {
			if g.Type() != t {
				continue
			}
			r := g.Check(text, meta)
			if !r.Allowed {
				return r
			}
		}
	}
	return GuardrailResult{Allowed: true, Type: GuardrailData, RequestID: GetString(meta, "requestId")}
}

// AsErrorEnvelope converts a denial into the unified error envelope.
func (r GuardrailResult) AsErrorEnvelope() ErrorEnvelope {
	return NewError(CodeGuardrailRejected, r.Reason, r.RequestID)
}

// GuardrailErrorJSON is a compact JSON line for SSE error frames.
func (r GuardrailResult) GuardrailErrorJSON() string {
	b, _ := json.Marshal(r)
	return string(b)
}
