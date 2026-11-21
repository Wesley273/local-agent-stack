// Command go-agent 是基于官方 a2a-go/v2 v2.8.0 的示例 Agent（User Server / NDJSON）。
//
// 双重身份：
//  1. 合规轨：用官方 SDK 起 AgentCard / AgentExecutor / JSONRPCHandler
//     （a2asrv.NewHandler / NewJSONRPCHandler / NewStaticAgentCardHandler），
//     挂在 /a2a 与 /.well-known/agent-card.json 等路径；
//  2. 兼容轨：Sidecar 通过 NDJSON /task（或 /run）连它取流（契约 §1.7）。
//
// 端口 9101（config/ports.env AGENT_GO_PORT）。
//
// 端点（与 Java / Python Agent 完全一致）：
//
//	GET  /health                         → {"status":"UP","service":"go-agent"}
//	POST /task  |  POST /run             → application/x-ndjson 流
//	GET  /.well-known/agent-card.json   → 官方 AgentCard（合规轨）
//	POST /a2a                           → 官方 JSON-RPC handler（合规轨）
//
// NDJSON 流逐行（§1.7）：
//
//	{"type":"status","state":"working","progress":0.5}\n
//	{"type":"artifact","artifactId":"art-1","text":"<回显内容>"}\n
//	{"type":"done","state":"completed"}\n
//
// long:N 输入 → artifact.text 产生 N 个字符的「单行」（E11 验证 >128KB 不截断）。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

const serviceName = "go-agent"

// ---------------------------------------------------------------------------
// 官方 A2A SDK：AgentCard + AgentExecutor（合规轨）
// ---------------------------------------------------------------------------

// echoExecutor 实现官方 a2asrv.AgentExecutor（Execute / Cancel）。
// Execute 返回 iter.Seq2[a2a.Event, error]，yield 事件即由官方 SDK 转成 SSE / Task。
type echoExecutor struct{}

func (e *echoExecutor) Execute(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		userText := ""
		if execCtx.Message != nil && len(execCtx.Message.Parts) > 0 {
			userText = execCtx.Message.Parts[0].Text()
		}
		text := echoText(userText)
		// 探针实测可编译可运行的最简合规结果：官方 a2a.NewMessage(ROLE_AGENT, NewTextPart)。
		// 单个 Message 响应由官方 SDK 直接序列化；task 流式事件（NewSubmittedTask 等）
		// 需要完整 taskstore/queue 装配，故这里采用官方示例同款路径。
		yield(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(text)), nil)
	}
}

func (e *echoExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

// buildAgentCard 用官方 a2a.AgentCard 类型构造卡片
// （SupportedInterfaces 强制非空，NewAgentInterface 用官方 transport 常量）。
func buildAgentCard(baseURL string) *a2a.AgentCard {
	skill := a2a.AgentSkill{
		ID:          "echo",
		Name:        "Echo",
		Description: "原样回显输入文本，可产生超长 artifact 用于验证 64KB 边界",
		Tags:        []string{"text", "test"},
		Examples:    []string{"hello", "long:100000"},
		InputModes:  []string{"text"},
		OutputModes: []string{"text"},
	}
	return &a2a.AgentCard{
		Name:               serviceName,
		Description:        "Go 示例回显 Agent（User Server / NDJSON）",
		Version:            "1.0.0",
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
		Skills:             []a2a.AgentSkill{skill},
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(baseURL+"/a2a", a2a.TransportProtocolJSONRPC),
		},
	}
}

// ---------------------------------------------------------------------------
// User Server 端点（NDJSON，契约 §1.7）
// ---------------------------------------------------------------------------

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"UP","service":"` + serviceName + `"}`))
}

// taskRequest 是 Sidecar → User Server 的 NDJSON 任务对象（§1.7）。
type taskRequest struct {
	Type    string          `json:"type"`
	TaskID  string          `json:"taskId"`
	Message json.RawMessage `json:"message"`
	Context json.RawMessage `json:"context"`
}

// message 是契约 §1.2 的简化结构，用于提取用户文本。
type message struct {
	Text    string `json:"text"`
	Content string `json:"content"`
	Parts   []struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
	} `json:"parts"`
}

// extractText 从 message 提取文本（兼容 parts[].text / text / content）。
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	if m.Text != "" {
		return m.Text
	}
	if m.Content != "" {
		return m.Content
	}
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Kind == "text" || p.Kind == "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// echoText 回显规则：普通输入原样（UTF-8，中文不乱码）；
// long:N → N 个字符的「单行」，N=128000 时单行 >128KB，写出时不截断。
func echoText(input string) string {
	if strings.HasPrefix(input, "long:") {
		n, err := strconv.Atoi(strings.TrimSpace(input[len("long:"):]))
		if err == nil && n >= 0 {
			return strings.Repeat("A", n)
		}
	}
	return input
}

// ndjsonHandler 处理 POST /task 或 /run，返回 application/x-ndjson 流。
func ndjsonHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":{"code":"INVALID_REQUEST","message":"method not allowed"}}`,
			http.StatusMethodNotAllowed)
		return
	}
	// 读取端不假设 64KB：用 io.ReadAll 读完整 body（long:N 请求体很小，但保持通用）
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeNDJSONError(w, "INVALID_REQUEST", "read body: "+err.Error())
		return
	}
	var req taskRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeNDJSONError(w, "INVALID_REQUEST", "JSON parse failed: "+err.Error())
		return
	}
	artifactText := echoText(extractText(req.Message))

	w.Header().Set("Content-Type", "application/x-ndjson; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK) // 不设 Content-Length，逐行 flush
	flusher, _ := w.(http.Flusher)
	bw := bufio.NewWriterSize(w, 64*1024)

	writeLine := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = bw.Write(b)
		_ = bw.WriteByte('\n')
		_ = bw.Flush()
		if flusher != nil {
			flusher.Flush()
		}
	}
	writeLine(map[string]any{"type": "status", "state": "working", "progress": 0.5})
	writeLine(map[string]any{"type": "artifact", "artifactId": "art-1", "text": artifactText})
	writeLine(map[string]any{"type": "done", "state": "completed"})
}

func writeNDJSONError(w http.ResponseWriter, code, msg string) {
	w.Header().Set("Content-Type", "application/x-ndjson; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"type":"error","code":%q,"message":%q}`+"\n", code, msg)
}

// ---------------------------------------------------------------------------
// main：装配官方 SDK handler + NDJSON User Server
// ---------------------------------------------------------------------------

func main() {
	port := "9101"
	if len(os.Args) > 1 {
		port = os.Args[1]
	}
	addr := "127.0.0.1:" + port
	baseURL := "http://" + addr

	// 合规轨：官方 SDK 的 RequestHandler + JSON-RPC + 静态卡片 handler
	handler := a2asrv.NewHandler(&echoExecutor{})
	card := buildAgentCard(baseURL)

	mux := http.NewServeMux()
	mux.Handle("/a2a", a2asrv.NewJSONRPCHandler(handler))
	mux.Handle("/.well-known/agent-card.json", a2asrv.NewStaticAgentCardHandler(card))
	// 同时提供契约 §1.1 的 agent.json 别名（Sidecar 可能用这个路径）
	mux.Handle("/.well-known/agent.json", a2asrv.NewStaticAgentCardHandler(card))

	// 兼容轨：User Server NDJSON 端点（与 Java / Python Agent 完全一致）
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/task", ndjsonHandler)
	mux.HandleFunc("/run", ndjsonHandler)

	log.Printf("[%s] listening on %s (A2A /a2a + NDJSON /task)", serviceName, addr)
	srv := &http.Server{Addr: addr, Handler: mux}
	log.Fatal(srv.ListenAndServe())
}
