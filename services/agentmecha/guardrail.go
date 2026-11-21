package main

import (
	shared "local-agent-stack/pkg/a2a"
)

// guardrail.go wires the four guardrail types and the guardrail/hook bridge.
// The four concrete checkers and the aggregation engine are NOT reimplemented
// here — they live in pkg/a2a/guardrail.go (GuardrailEngine / DefaultHooks),
// which this file reuses verbatim per the "shared library" rule.

// GuardrailEngine is the shared 4-in-1 engine (prompt / tool / instruction /
// data). Aliasing keeps executor.go free of a direct pkg import for the type.
type GuardrailEngine = shared.GuardrailEngine

// NewGuardrailEngine builds the four-check engine from the shared config.
func NewGuardrailEngine(cfg shared.GuardrailConfig) *GuardrailEngine {
	return shared.NewGuardrailEngine(cfg)
}

// DefaultGuardrailConfig is the demo config (blockedWords / blockedTools /
// maxPromptChars / secretPatterns), matching contract §4 guardrails.
func DefaultGuardrailConfig() shared.GuardrailConfig {
	return shared.DefaultGuardrailConfig()
}

// checkGuardrails runs all four input checks and returns the first denial.
// meta carries requestId, method and tool so the instruction/tool checkers
// can see the JSON-RPC method allowlist and tool name respectively.
//
// Exception (E11): an explicit "long:N" input is a regression probe that asks
// the User Server to PRODUCE an N-char artifact. The input itself is a tiny
// token, but the executor expands it; applying maxPromptChars to the expanded
// text would be a false positive. For these we apply the same word-only input
// rules as the transparent proxy (sensitive words / tools / injection), not
// the prompt-length ceiling.
func checkGuardrails(engine *GuardrailEngine, text string, meta map[string]any) shared.GuardrailResult {
	if engine == nil {
		return shared.GuardrailResult{Allowed: true, Type: shared.GuardrailPrompt}
	}
	if isLongProbe(text) {
		return engine.CheckInputWordsOnly(text, meta)
	}
	return engine.CheckInput(text, meta)
}

// isLongProbe reports whether text is an explicit long-artifact probe such as
// "long:128000". Kept deliberately narrow so normal prompts are unaffected.
func isLongProbe(text string) bool {
	const prefix = "long:"
	if len(text) < len(prefix)+1 || len(text) > len(prefix)+12 {
		return false
	}
	if text[:len(prefix)] != prefix {
		return false
	}
	for _, c := range text[len(prefix):] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// checkGuardrailOutput runs the OUTPUT-side checks on a generated artifact.
//
// Contract §6.1: DataGuardrail checks artifacts / file parts / output text for
// secret patterns; PromptGuardrail checks the INPUT prompt (blocked words +
// maxPromptChars). Applying the 8000-char INPUT ceiling to a 128000-char
// artifact (E11) would be a false positive, so here we deliberately run ONLY
// the DataGuardrail. This wrapper does NOT modify pkg/a2a; it chooses which
// of the four shared checkers applies to which direction.
func checkGuardrailOutput(engine *GuardrailEngine, text string, meta map[string]any) shared.GuardrailResult {
	if engine == nil {
		return shared.GuardrailResult{Allowed: true, Type: shared.GuardrailData}
	}
	return shared.NewDataGuardrail(DefaultGuardrailConfig()).Check(text, meta)
}

// guardrailMeta builds the meta map passed to the shared engine.
func guardrailMeta(requestID, method, tool string) map[string]any {
	m := map[string]any{"requestId": requestID}
	if method != "" {
		m["method"] = method
	}
	if tool != "" {
		m["tool"] = tool
	}
	return m
}

// guardrailRejection converts a denial into the unified error envelope
// (contract §0.3, code GUARDRAIL_REJECTED). Non-stream callers use this.
func guardrailRejection(r shared.GuardrailResult) shared.ErrorEnvelope {
	return r.AsErrorEnvelope()
}

// guardrailErrorFrame is the SSE `event: error` frame used when a guardrail
// hits DURING a stream (contract §3.4 / §1.6). It never carries an HTTP
// status and the connection stays 200 until [DONE].
func guardrailErrorFrame(code shared.ErrorCode, msg, requestID string) string {
	return shared.SSEErrorFrame(code, msg, requestID)
}
