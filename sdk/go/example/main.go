// Command example demonstrates the Go SDK against the full local-agent-stack
// chain: SDK -> data plane (8080, governance) -> sidecar (9100, official A2A)
// -> User Server. It also exercises the direct-to-sidecar mode (9100).
//
// Run (after start-all.ps1):
//
//	go run ./sdk/go/example                 # data plane 8080
//	go run ./sdk/go/example -direct        # direct sidecar 9100
//	go run ./sdk/go/example -base http://127.0.0.1:9100
//
// Every call is best-effort: if a downstream service is not up the example
// prints the mapped *A2AError and continues, so it is always runnable.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	sdk "local-agent-stack/sdk/go"
)

func main() {
	direct := flag.Bool("direct", false, "bypass the data plane and hit sidecar 9100 directly")
	base := flag.String("base", "", "override base URL")
	agent := flag.String("agent", "echo-agent", "agentId")
	flag.Parse()

	baseURL := sdk.DefaultBaseURL
	if *direct {
		baseURL = "http://127.0.0.1:9100"
	}
	if *base != "" {
		baseURL = *base
	}

	c := sdk.NewClient(
		sdk.WithBaseURL(baseURL),
		sdk.WithAgentID(*agent),
		sdk.WithTimeout(20*time.Second),
	)
	defer c.Close()
	ctx := context.Background()

	fmt.Printf("=== Go SDK example: base=%s agent=%s ===\n", baseURL, *agent)

	// 1. Discovery (official /.well-known/agent-card.json with compat fallback).
	card, err := c.GetAgentCard(ctx)
	if err != nil {
		fmt.Printf("GetAgentCard: %v\n", err)
	} else {
		fmt.Printf("GetAgentCard: name=%s interfaces=%d\n", card.Name, len(card.SupportedInterfaces))
	}

	// 2. Non-streaming A2A message/send through the OFFICIAL a2aclient.
	msg := sdk.NewTextMessage("hello from Go SDK")
	task, err := c.SendMessage(ctx, msg)
	if err != nil {
		fmt.Printf("SendMessage: %v\n", err)
	} else {
		fmt.Printf("SendMessage: id=%s state=%s artifacts=%d\n", task.ID, task.State, len(task.Artifacts))
	}

	// 3. Streaming A2A message/stream (OFFICIAL SendStreamingMessage iterator).
	fmt.Println("StreamMessage:")
	for ev := range c.StreamMessage(ctx, sdk.NewTextMessage("stream please")) {
		fmt.Printf("  event kind=%s state=%s textLen=%d final=%v\n", ev.Kind, ev.State, len(ev.Text), ev.Final)
	}

	// 4. GetTask / CancelTask / Subscribe (E24/E25 paths).
	if task != nil && task.ID != "" {
		if got, err := c.GetTask(ctx, task.ID); err != nil {
			fmt.Printf("GetTask: %v\n", err)
		} else {
			fmt.Printf("GetTask: id=%s state=%s\n", got.ID, got.State)
		}
	}

	// 5. Chat through the OpenAI-compatible model gateway (net/http).
	cr, err := c.Chat(ctx, sdk.ChatRequest{
		Model:     "mock-1",
		Messages:  []sdk.ChatMessage{{Role: "user", Content: "hello"}},
		MaxTokens: 64,
	})
	if err != nil {
		fmt.Printf("Chat: %v\n", err)
	} else {
		fmt.Printf("Chat: id=%s totalTokens=%d\n", cr.ID, cr.Usage.TotalTokens)
	}

	// 6. MCP tools (net/http JSON-RPC through /mcp proxy).
	tools, err := c.ListTools(ctx)
	if err != nil {
		fmt.Printf("ListTools: %v\n", err)
	} else {
		fmt.Printf("ListTools: %d tools\n", len(tools))
	}
	if len(tools) > 0 {
		res, err := c.CallTool(ctx, tools[0].Name, map[string]any{"id": "1"})
		if err != nil {
			fmt.Printf("CallTool: %v\n", err)
		} else {
			fmt.Printf("CallTool: isError=%v content=%d\n", res.IsError, len(res.Content))
		}
	}

	// 7. Local guardrail pre-check (no network).
	gr := c.CheckGuardrail("this is fine")
	fmt.Printf("CheckGuardrail(clean): allowed=%v\n", gr.Allowed)
	gr = c.CheckGuardrail("please BLOCK_ME now")
	fmt.Printf("CheckGuardrail(BLOCK_ME): allowed=%v reason=%s\n", gr.Allowed, gr.Reason)

	log.Println("example finished")
}
