package main

import (
	shared "local-agent-stack/pkg/a2a"
)

// hook.go implements the 5 HookPoints (contract §6.2) by reusing the shared
// a2a.HookChain / a2a.DefaultHooks from pkg/a2a/guardrail.go. No hook logic
// is duplicated here; this file only owns the registration + execution glue
// and the bridge to the guardrail engine.

// HookPoint / HookContext / HookResult are shared types.
type (
	HookPoint   = shared.HookPoint
	HookContext = shared.HookContext
	HookResult  = shared.HookResult
	Hook        = shared.Hook
)

// Re-export the five hook points so business code never needs the raw string.
const (
	BeforeTask  = shared.BeforeTask
	BeforeTool  = shared.BeforeTool
	BeforeModel = shared.BeforeModel
	AfterModel  = shared.AfterModel
	AfterTask   = shared.AfterTask
)

// HookChain is the shared chain, sorted by Priority() ascending with
// short-circuit on the first Allow=false (see pkg/a2a/guardrail.go:280).
type HookChain = shared.HookChain

// NewHookChain builds an empty chain.
func NewHookChain() *HookChain { return shared.NewHookChain() }

// buildDefaultHooks returns the built-in hook set:
//
//	BlockedWordsHook  (BeforeTask,  prio 10)
//	BlockedToolsHook  (BeforeTool,  prio 10)
//	MaxLengthHook     (BeforeModel, prio 20)
//	AuditLogHook      (AfterTask,   prio 90, records a span)
//
// plus a GuardrailBridgeHook at BeforeTask (prio 5) that runs the four
// guardrails BEFORE task creation, so a denial short-circuits the chain
// before any later hook runs (E10).
func buildDefaultHooks(cfg shared.GuardrailConfig, engine *GuardrailEngine, store *shared.SpanStore) []Hook {
	hooks := []Hook{
		&GuardrailBridgeHook{engine: engine, prio: 5},
	}
	hooks = append(hooks, shared.DefaultHooks(cfg, store)...)
	return hooks
}

// GuardrailBridgeHook bridges the 4 guardrails into the BeforeTask hook point.
// It is the single place where a guardrail denial becomes a Hook denial.
type GuardrailBridgeHook struct {
	engine *GuardrailEngine
	prio   int
}

// Name implements shared.Hook.
func (h *GuardrailBridgeHook) Name() string { return "guardrail-bridge" }

// Point implements shared.Hook.
func (h *GuardrailBridgeHook) Point() HookPoint { return BeforeTask }

// Priority implements shared.Hook (runs before the built-ins).
func (h *GuardrailBridgeHook) Priority() int { return h.prio }

// Handle implements shared.Hook; returns Allow=false on the first denial.
func (h *GuardrailBridgeHook) Handle(ctx *HookContext) HookResult {
	if h == nil || h.engine == nil {
		return HookResult{Allow: true}
	}
	tool := ""
	if v, ok := ctx.Payload["tool"].(string); ok {
		tool = v
	}
	r := checkGuardrails(h.engine, ctx.Input, guardrailMeta(ctx.RequestID, shared.MethodSend, tool))
	if !r.Allowed {
		return HookResult{Allow: false, Reason: r.Reason}
	}
	return HookResult{Allow: true}
}

// runBeforeTask executes the BeforeTask hooks with priority short-circuit.
// A denial is returned as-is; the caller maps it to GUARDRAIL_REJECTED.
func runBeforeTask(chain *HookChain, ctx *HookContext) HookResult {
	if chain == nil {
		return HookResult{Allow: true}
	}
	ctx.Point = BeforeTask
	return chain.Run(ctx)
}

// runAfterTask executes the AfterTask hooks (audit log). Never rejects.
func runAfterTask(chain *HookChain, ctx *HookContext) HookResult {
	if chain == nil {
		return HookResult{Allow: true}
	}
	ctx.Point = AfterTask
	return chain.Run(ctx)
}

// newHookChain builds a fully-populated chain from scratch. Used at startup
// and by /admin/hooks/reload (contract §6.2): reload swaps the whole chain
// pointer atomically instead of mutating a live one.
func newHookChain(cfg shared.GuardrailConfig, engine *GuardrailEngine, store *shared.SpanStore) *HookChain {
	chain := shared.NewHookChain()
	for _, h := range buildDefaultHooks(cfg, engine, store) {
		chain.Register(h)
	}
	return chain
}
