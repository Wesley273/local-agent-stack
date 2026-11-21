# AgentMecha Sidecar（Go，端口 9100）

基于 **官方 `github.com/a2aproject/a2a-go/v2 v2.8.0`** 的双端 A2A Server/Client Sidecar。
协议合规（JSON-RPC PascalCase 方法名、`TASK_STATE_*` 枚举、`AgentCard.SupportedInterfaces`、SSE 帧）完全交给官方 SDK；自研部分只做简历里的增值能力：NDJSON 适配、4 类护栏 + Hook、状态/会话/任务持久化解耦、冒号路径兼容轨。

> 契约唯一真相源：`docs/01-协议契约.md`（§0.0 双轨制、§1.5 冒号路径、§1.7 NDJSON、§3.4 SSE 不断流、§6 护栏/Hook、§6.3 解耦、§7 SDK、§8 E2E）。
> 共享库 `local-agent-stack/pkg/a2a` 直接 import，**不修改、不重复实现**。

## 启动

```powershell
cd local-agent-stack
$env:CGO_ENABLED='0'
go run ./services/agentmecha                # 监听 127.0.0.1:9100
# 或
go build -o agentmecha.exe ./services/agentmecha
.\agentmecha.exe
```

环境变量：`MECHA_PORT`（默认 9100）、`MECHA_DATA_DIR`（默认 `./data/agentmecha`）。

## 端点

| 方法 | 路径 | 轨道 | 说明 |
|---|---|---|---|
| GET | `/health` | 通用 | §0.4 健康检查 |
| GET | `/` | 通用 | 端点索引 |
| GET | `/.well-known/agent.json` | 兼容轨 | §1.1 AgentCard（`url` 字段） |
| GET | `/.well-known/agent-card.json` | **官方** | `a2asrv.NewStaticAgentCardHandler`（`SupportedInterfaces`，无 URL 字段） |
| POST | `/a2a/{agentId}` | **官方** | `a2asrv.NewJSONRPCHandler`，方法名 `SendMessage`/`GetTask`/`CancelTask`/`SubscribeToTask`/`SendStreamingMessage` |
| GET/POST/DELETE | `/a2a/{agentId}/...` | **官方** | `a2asrv.NewRESTHandler`（`POST /tasks/{idAndAction}` + `CutSuffix`） |
| POST | `/a2a/v1/message:send` | 兼容轨 | E12 冒号路径 |
| POST | `/a2a/v1/message:stream` | 兼容轨 | §1.6 SSE |
| GET | `/a2a/v1/tasks/{id}` | 兼容轨 | `tasks/get` |
| POST | `/a2a/v1/tasks/{id}:cancel` | 兼容轨 | E12 冒号路径 |
| GET | `/a2a/v1/tasks/{id}:subscribe` | 兼容轨 | E24 断点续传 |
| GET | `/a2a/v1/card` | 兼容轨 | AgentCard |
| GET | `/admin/hooks` | 自研 | 列出已注册 Hook（§6.2） |
| POST | `/admin/hooks/reload` | 自研 | 重新装载配置 |
| GET | `/admin/tasks` | 自研 | 持久化任务索引 |
| GET | `/v1/admin/traces?limit=` | 自研 | `a2a.SpanStore`，数值字段非 null |
| GET | `/v1/admin/metrics` | 自研 | counters |

**多 Agent**：单进程挂载 `echo-agent`(9101) / `java-agent`(9102) / `python-agent`(9103)，URL 形态 `/a2a/{agentId}`。`main.go` 只注册 **一个** `/a2a/` 前缀 handler，切出 agentId 后重写 path 交给官方 handler；agentId 不存在返回 `AGENT_NOT_FOUND` 错误信封。

## 关键实现位置

| 能力 | 文件 | 说明 |
|---|---|---|
| 官方 SDK 装配 | `main.go:NewServer` | `a2asrv.NewHandler(exec, a2asrv.WithTaskStore(NewSDKTaskStore(store)), a2asrv.WithExtendedAgentCard(...))` + `NewJSONRPCHandler` + `NewRESTHandler` + `NewStaticAgentCardHandler` |
| 官方 Executor | `executor.go` | `iter.Seq2[a2a.Event,error]`：submitted → working → `NewArtifactEvent` → completed；护栏命中仍 yield 到终态（不断流） |
| NDJSON 64KB 修复 | `userserver.go:ScanUserServerNDJSON` | 调用 `shared.NewNDJSONScanner`（`Buffer(64K,10M)`），128000 字符单行不报 `token too long` |
| 冒号路径规避 | `colons.go` / `main.go:handleA2A` | 绝不注册 `POST /tasks/{id}:cancel` 到 ServeMux；用 `a2a.ParseRestPath` 手动解析 |
| 状态解耦 | `store.go` | `StateStore` / `SessionStore` 接口 + `FileStore`（JSON 文件 + 内存索引 + 原子写 + 重启重建索引）+ `MemoryStore` |
| 官方 SDK 持久化桥 | `store.go:SDKTaskStore` | 实现官方 `taskstore.Store`（Create/Update/Get/List + OCC），传给 `WithTaskStore` → E25 重启恢复 |
| 断点续传 | `taskmanager.go:taskBus` | 记录 frames + 广播给 live subscriber；`tasks/subscribe` 先 replay 再续收到 completed（E24） |
| 4 类护栏 | `guardrail.go` | 复用 `a2a.GuardrailEngine`；输出侧只跑 `DataGuardrail`（避免 8000 字符输入上限误杀 128KB artifact） |
| 5 HookPoint | `hook.go` | 复用 `a2a.HookChain` / `DefaultHooks`，优先级升序短路；`GuardrailBridgeHook` 在 BeforeTask 联动 |
| span | `executor.go` / `admin.go` | `a2a.SpanStore`，`http.status_code`/`durationMs` 均为整数 |

## 协议双轨制（§0.0）

- **合规轨**（官方 SDK）：JSON-RPC 方法名 `SendMessage`、状态 `TASK_STATE_COMPLETED`、卡片 `SupportedInterfaces`、SSE 由 SDK 内部 `sse.NewWriter` 产生。
- **兼容轨**（本仓 `colons.go`）：`message/send`、`state:"completed"`、`AgentCard.url`、自研 §1.6 SSE 帧。
- 两者**不互相转换**、不互相校验。兼容轨只是 E12/E24 的回归端点。

## E2E 覆盖

| 用例 | 单测覆盖 | 备注 |
|---|---|---|
| E06 `message/send` | ✅ `TestOfficialSendMessageReturnsTask` | 官方 a2aclient 真实调用，返回 `TASK_STATE_COMPLETED` + artifact |
| E09 `message/stream` | ✅ `TestOfficialStreamingMessage` | ≥2 status + artifact |
| E10 流式护栏 | ✅ `TestSSEGuardrailErrorFrameThenDone` / `TestHookRejectionShortCircuit` | error 帧 + `[DONE]`，不写新 HTTP 状态 |
| E11 NDJSON 128KB | ✅ `TestNDJSON128KBSingleLine` / `TestOfficialSendMessage128KB` | 128000 字符 artifact 无截断 |
| E12 冒号路径 | ✅ `TestParseRestPathColonCases` / `TestColonPathNeverRegistersOnServeMux` | 含冒号 id、`%3A`、未知 action、无 panic |
| E24 断点续传 | ✅ `TestBreakpointResumeWorkingToCompleted` | working → completed + artifact |
| E25 重启恢复 | ✅ `TestFileStoreRestartRecovery` / `TestSDKTaskStoreRestartRecovery` / `TestOfficialGetTaskAfterRestart` | 官方 GetTask 在重建实例后仍可读 |

需要下游服务（数据面 8080 / 控制面 9000 / User Server 9101-9103 / Mock LLM 9200 / MCP 9300）起来后才能跑的：E06/E09 走数据面的链路、Chat/MCP 相关、配置热更新。单测已用 `httptest` 直接驱动官方 handler，证明链路本身正确。

## 测试

```powershell
$env:CGO_ENABLED='0'
go test ./services/agentmecha -v
```
