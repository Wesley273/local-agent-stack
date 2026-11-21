# local-agent-stack Go SDK

契约 §7 的 Go 实现：**基于官方 `a2aclient` 二次封装**（A2A 能力）+ `net/http`（Chat / MCP / 护栏能力）。与 Java / Python SDK 同名同语义。

> 官方 SDK：`github.com/a2aproject/a2a-go/v2 v2.8.0`。共享库 `local-agent-stack/pkg/a2a` 直接 import（SSE/NDJSON/护栏/错误类型），不修改、不重复实现。

## 构造

```go
import sdk "local-agent-stack/sdk/go"

c := sdk.NewClient(
    sdk.WithBaseURL("http://127.0.0.1:8080"), // 默认数据面
    sdk.WithAPIKey("demo-key"),              // 默认
    sdk.WithTimeout(30*time.Second),
    sdk.WithAgentID("echo-agent"),
)
defer c.Close()
```

`NewClient(opts...)` 默认值：baseURL=数据面 `8080`、apiKey=`demo-key`、超时 30s。
每个请求自动带 `X-A2A-Version: 1.0`、`X-Request-Id`（本地生成）、`X-API-Key`。

## 统一接口（§7）

| 方法 | 底层 | 说明 |
|---|---|---|
| `GetAgentCard(ctx)` | `net/http` + `a2a.AgentCard` | 官方 `/.well-known/agent-card.json` 优先，兼容 `/.well-known/agent.json` 回退 |
| `SendMessage(ctx, msg)` | **官方** `a2aclient.Client.SendMessage` | 返回 `*Task` |
| `StreamMessage(ctx, msg) <-chan Event` | **官方** `a2aclient.Client.SendStreamingMessage` | SDK 内部解码 SSE |
| `GetTask(ctx, id)` | **官方** `a2aclient.Client.GetTask` | E25 |
| `CancelTask(ctx, id)` | **官方** `a2aclient.Client.CancelTask` | |
| `Subscribe(ctx, id) <-chan Event` | **官方** `a2aclient.Client.SubscribeToTask` | E24 |
| `Chat(ctx, req)` | `net/http` | `/v1/chat/completions` |
| `ListTools(ctx)` / `CallTool(ctx,name,args)` | `net/http` | `/mcp` JSON-RPC |
| `CheckGuardrail(text)` | 本地 `a2a.GuardrailEngine` | 不联网的本地预检 |

额外能力：`StreamChat(ctx, req)`（模型网关 SSE）、`ReadNDJSON`（>64KB 单行）、`ReadSSEStream`（跨 chunk 拼接）、`FeedAllSSE`（测试辅助）、`IsTimeout` / `mapError`。

## 官方 SDK 使用位置

- `client.go:officialClient` → `a2aclient.NewFromCard(ctx, card, WithJSONRPCTransport, WithCallInterceptors(...))`
- `client.go:headerInterceptor` → 实现官方 `a2aclient.CallInterceptor`，把三个必带头注入 `req.ServiceParams`
- `client.go:SendMessage` / `StreamMessage` / `GetTask` / `CancelTask` / `Subscribe` → 官方 `*Client` 方法
- `parseAgentCard` → 复用官方 `*a2a.AgentCard` / `a2a.NewAgentInterface`
- `taskFromResult` / `eventFromOfficial` → 复用官方 `a2a.Task` / `a2a.Event` 类型

## SSE / NDJSON

- **SSE 跨 chunk 拼接**：`sse.go` 复用 `a2a.SSEDecoder`（`pkg/a2a/stream.go`），`Feed()` 增量缓冲直到空行；绝不假设「一次 Read == 一帧」。`TestSSECrossChunkStitching` 用 1 字节切分 90KB 帧验证不截断。
- **NDJSON >64KB**：`sse.go:ReadNDJSON` 复用 `a2a.NewNDJSONScanner`（`Buffer(64K,10M)`）；`TestNDJSONLargeLine` 验证 128000 字符单行。

## 错误与超时

- `errors.go:mapError` 把 HTTP/JSON-RPC/统一错误信封映射成 `*a2a.A2AError`（`pkg/a2a/types.go`）。
- `UPSTREAM_TIMEOUT` / `context.DeadlineExceeded` → 原生 `net.Error`（`TimeoutError`，`Timeout()==true`），`IsTimeout(err)` 可判定。

## Example

```powershell
$env:CGO_ENABLED='0'
go run ./sdk/go/example                 # 打数据面 8080（全链路）
go run ./sdk/go/example -direct        # 直连 sidecar 9100
go run ./sdk/go/example -base http://127.0.0.1:9100 -agent java-agent
```

Example 依次演示：GetAgentCard → SendMessage → StreamMessage → GetTask → Chat → ListTools/CallTool → CheckGuardrail。每个调用都是 best-effort，下游未起时打印映射后的错误并继续，因此**始终可编译可运行**。

## 测试

```powershell
$env:CGO_ENABLED='0'
go test ./sdk/go/... -v
```

覆盖：SSE 跨 chunk、NDJSON 128KB、必带头、错误/超时映射、本地护栏、流式护栏不断流、可配超时。
