package a2asdk

import (
	shared "local-agent-stack/pkg/a2a"
)

// guardrail.go — local pre-flight guardrail (contract §7 / §6.1).
//
// The four concrete checkers are NOT reimplemented here: they live in the
// shared pkg/a2a/guardrail.go and are reused through GuardrailEngine. This is
// the SDK's LOCAL check — it never calls the network, so callers can reject
// blocked words / dangerous tools / overlong prompts before spending a
// request on the data plane.

// newLocalEngine builds a fresh shared GuardrailEngine from a config.
func newLocalEngine(cfg shared.GuardrailConfig) *shared.GuardrailEngine {
	return shared.NewGuardrailEngine(cfg)
}

// GuardrailCheck runs the four input checks locally.
func GuardrailCheck(cfg shared.GuardrailConfig, text string) shared.GuardrailResult {
	return newLocalEngine(cfg).CheckInput(text, map[string]any{"requestId": shared.NewID("req")})
}

// GuardrailOutputCheck runs the output-side (data + prompt) checks locally.
func GuardrailOutputCheck(cfg shared.GuardrailConfig, text string) shared.GuardrailResult {
	return newLocalEngine(cfg).CheckOutput(text, map[string]any{"requestId": shared.NewID("req")})
}

// GuardrailRejection converts a denial into the unified §0.3 error envelope.
func GuardrailRejection(r shared.GuardrailResult) shared.ErrorEnvelope {
	return r.AsErrorEnvelope()
}
