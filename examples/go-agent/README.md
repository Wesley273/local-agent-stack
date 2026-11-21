# Go 示例 Agent（User Server / NDJSON）· 端口 9101

基于**官方 `github.com/a2aproject/a2a-go/v2 v2.8.0`** 的示例 Agent，同时暴露 Sidecar 需要的 NDJSON 协议。

> go.mod / go.sum 由 lead 单点管理（root `module local-agent-stack` 已 require `a2a-go/v2 v2.8.0`）。
> 本目录**不放嵌套 go.mod**，编译在 root 跑 `go build ./...`（会覆盖本目录）。

## 构建 / 启动

```powershell
Push-Location local-agent-stack
go build ./examples/go-agent          # 或 go build ./...
.\go-agent.exe 9101                  # 二进制落在 root
# 或：go run ./examples/go-agent 9101
```

## 官方 SDK 使用位置（`main.go`）

| 官方 API | 用途 | 位置 |
|---|---|---|
| `a2asrv.AgentExecutor`（`Execute`/`Cancel` 返回 `iter.Seq2[a2a.Event,error]`） | 真实 A2A Executor | `echoExecutor` (L54–L71) |
| `a2a.NewMessage` / `a2a.NewTextPart` / `a2a.MessageRoleAgent` | yield 官方 Message 事件（回显） | `Execute` (L64) |
| `a2a.NewStatusUpdateEvent` / `a2a.TaskStateCanceled` | Cancel 事件 | `Cancel` (L70) |
| `a2a.AgentCard` / `AgentCapabilities` / `AgentSkill` / `AgentInterface` | 构造卡片，`SupportedInterfaces` 非空 | `buildAgentCard` (L76–L97) |
| `a2a.NewAgentInterface(url, a2a.TransportProtocolJSONRPC)` | 官方 transport 常量 | `buildAgentCard` (L95) |
| `a2asrv.NewHandler(executor)` | 传输无关 RequestHandler | `main` (L224) |
| `a2asrv.NewJSONRPCHandler(handler)` | 官方 JSON-RPC http.Handler，挂 `/a2a` | `main` (L228) |
| `a2asrv.NewStaticAgentCardHandler(card)` | 官方卡片 handler | `main` (L229–L231) |

### Go 官方 SDK 用法验证（已实跑）

另外用官方 `a2aclient.NewFromCard` + `client.SendMessage` + `client.SendStreamingMessage`
对运行中的 9101 做了真实往返，输出：
```
message/send -> Go 官方 SDK 用法验证 OK
streaming events:
  *a2a.Message
Go 官方 a2a-go/v2 SDK 客户端往返验证通过
```
（探针 `_probe/go/a2a_example/main.go` 的同款 API，v2.8.0 真实签名。）

## 端点（与 Java / Python Agent 完全一致）

| 方法 | 路径 | 返回 |
|---|---|---|
| GET | `/health` | `{"status":"UP","service":"go-agent"}` |
| POST | `/task` 或 `/run` | `application/x-ndjson` 流 |
| GET | `/.well-known/agent-card.json`、`/.well-known/agent.json` | 官方 AgentCard |
| POST | `/a2a` | 官方 JSON-RPC handler |

NDJSON 流（三行）：
```
{"type":"status","state":"working","progress":0.5}\n
{"type":"artifact","artifactId":"art-1","text":"<回显内容>"}\n
{"type":"done","state":"completed"}\n
```

- 普通输入：artifact.text = 用户文本原样（UTF-8，中文不乱码）
- `long:N`：artifact.text = N 个 `'A'` 的**单行**，`N=128000` 时单行 >128KB，写出时不截断（E11）
- 写出用 `bufio.Writer` + 每行 `Flush()` + `http.Flusher`，不设 Content-Length

## 坑

- 冒号路径（`/tasks/{id}:cancel`）**不注册进 ServeMux**（Go 1.26 会 panic）；本 Agent 无此需求，官方 SDK 内部处理。
- 合规轨用官方 SDK 的 **PascalCase `SendMessage`** 方法 + `A2A-Version: 1.0` 头（契约 §0.0 双轨制）；`message/send` 是兼容轨，官方 handler 会返回 `method not found`，属预期。
- Executor 多事件（NewSubmittedTask/NewArtifactEvent）路径需要完整 taskstore/queue 装配，否则官方 SDK 报 `gob: encodeArray: nil element`；故采用探针实测可运行的**单 Message 响应**路径（L64）。
- 官方 v2.8.0 的 `a2a.NewSubmittedTask(execCtx, nil)` 是 2 参数（探针注释里的 3 参数是旧签名）。
- NDJSON 流必须 `WriteHeader(200)` 后逐行写并 flush，不能先收集完再一次性写（Sidecar 是按行读的）。
