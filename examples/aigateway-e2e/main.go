// Command aigateway-e2e demonstrates the full AI-gateway composite plugin
// pipeline: request-body model extraction -> model-based routing -> async
// input/output guardrails awaited in EncodeData -> token limiting -> metrics.
//
// Run: go run ./examples/aigateway-e2e
package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"local-agent-stack/pkg/aigateway"
	"local-agent-stack/pkg/composite"
)

type printSink struct{}

func (printSink) Record(m aigateway.MetricSnapshot) {
	fmt.Printf("  [metrics] model=%s backend=%s tokens(p/c/t)=%d/%d/%d gateWait=%dms status=%s reject=%d\n",
		m.Model, short(m.Backend), m.PromptTokens, m.CompletionTokens, m.TotalTokens,
		m.GateWaitedMs, m.Status, m.GuardRejects)
}

func short(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func runOnce(chain *composite.Chain, label, body, response string, stream bool) {
	fmt.Printf("\n=== %s ===\n", label)
	ctx := composite.NewContext(http.MethodPost, "/v1/chat/completions", "", "req-"+label,
		composite.NewHeaderMap(http.Header{"Content-Type": []string{"application/json"}}))
	if stream {
		ctx.Set("is_stream", true)
	}
	s := chain.NewSession(ctx)
	defer s.Close(composite.ReasonNormal)

	fmt.Println("  [decode] DH")
	reply, err := s.RunDecodeHeaders(false)
	if err != nil {
		fmt.Println("  err:", err)
	}
	if reply != nil {
		fmt.Printf("  [local-reply] status=%d body=%s\n", reply.Status, reply.Body)
		return
	}
	// Feed body in two chunks to show buffer/parse happens once.
	mid := len(body) / 2
	_, _ = s.RunDecodeData([]byte(body[:mid]), false)
	reply, _ = s.RunDecodeData([]byte(body[mid:]), true)
	if reply != nil {
		fmt.Printf("  [local-reply] status=%d body=%s\n", reply.Status, reply.Body)
		return
	}
	fmt.Printf("  [decode] body parsed once: model=%q stats=%v\n",
		ctx.Get(aigateway.KModel), ctx.Stats())

	h := composite.NewHeaderMap(http.Header{"Content-Type": []string{"application/json"}})
	s.RunEncodeHeaders(h, false)
	fmt.Println("  [encode] EH")
	out, appended := s.RunEncodeData([]byte(response), true)
	if len(appended) > 0 {
		fmt.Printf("  [encode] appended frames: %s\n", strings.TrimSpace(string(appended)))
	}
	if !stream && len(out) > 0 {
		fmt.Printf("  [encode] released response (%d bytes)\n", len(out))
	}
	if g, ok := ctx.Get(aigateway.KGateVerdict).(*aigateway.GateVerdict); ok {
		fmt.Printf("  [gate] allowed=%v timeout=%v input=%v output=%v reason=%s\n",
			g.Allowed, g.TimedOut, g.Input.Allowed, g.Output.Allowed, g.Reason)
	}
	fmt.Printf("  [encode] routed-backend header=%s\n", h.Get("x-routed-backend"))
}

func main() {
	checker := aigateway.BlockingChecker(30*time.Millisecond, []string{"BLOCK_ME", "secret-leak"})
	cfg := aigateway.DefaultConfig()
	cfg.GuardChecker = checker
	cfg.MetricsSink = printSink{}
	cfg.GuardTimeout = 500 * time.Millisecond
	cfg.GuardFailOpen = false
	chain := aigateway.BuildChain(cfg)

	fmt.Println("AI Gateway Composite Pipeline E2E")
	fmt.Println("plugins: body-model-parser -> token-limiter -> route-by-model -> async-guardrail -> metrics")

	// 1. Happy path: model extracted from body, routed, async guards pass.
	runOnce(chain, "happy",
		`{"model":"mock-1","messages":[{"role":"user","content":"hello world"}]}`,
		`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		false)

	// 2. Input guardrail blocks a word in the request.
	runOnce(chain, "input-blocked",
		`{"model":"mock-1","messages":[{"role":"user","content":"BLOCK_ME please"}]}`,
		`{"choices":[],"usage":{}}`,
		false)

	// 3. Streaming with output guardrail hit -> appended error frame, no disconnect.
	runOnce(chain, "stream-output-blocked",
		`{"model":"mock-1","stream":true,"messages":[{"role":"user","content":"hello"}]}`,
		"data: secret-leak detected\n\n",
		true)
}
