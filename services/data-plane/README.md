# Data Plane / AI 网关数据面（Go :8080）

`local-agent-stack` 的数据面实现。**治理层 + 透明反向代理**（契约 §0.0）：
只做鉴权 / 限流 / 护栏 / 路由选择 / trace / 配置热更新，然后把 HTTP 请求
**原样转发**给下游，**绝不解析 A2A 协议语义**（`message/send` 与官方 SDK 的
`SendMessage`、`completed` 与 `TASK_STATE_COMPLETED` 两套方法名都透传，
不转换、不校验）。

- module：`local-agent-stack/services/data-plane`（root `go.mod` 统一管理）
- 依赖：仅 Go 标准库 + `local-agent-stack/pkg/a2a`（共享库，只读复用）
- 无任何外部第三方依赖；`CGO_ENABLED=0` 可编译。

## 启动

```powershell
# 从 local-agent-stack/ 根目录
$env:DP_PORT='8080'; $env:CP_PORT='9000'
$env:MECHA_PORT='9100'; $env:LLM_PORT='9200'; $env:MCP_PORT='9300'
go run ./services/data-plane
```

环境变量（都有默认值，来自 `config/ports.env`）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `DP_HOST` / `DP_PORT` | 127.0.0.1 / 8080 | 本服务监听 |
| `CP_HOST` / `CP_PORT` | 127.0.0.1 / 9000 | 控制面 `/v1/config` |
| `MECHA_HOST` / `MECHA_PORT` | 127.0.0.1 / 9100 | Sidecar（`/a2a/*`） |
| `LLM_HOST` / `LLM_PORT` | 127.0.0.1 / 9200 | Mock LLM（`/v1/*`） |
| `MCP_HOST` / `MCP_PORT` | 127.0.0.1 / 9300 | MCP 注册中心（`/mcp/*`） |

启动即 `GET {CP}/v1/config` 拉全量快照，之后每 **5s** 带
`If-None-Match: <revision>` 轮询；304 不更新，revision 变化才热替换
`atomic.Value`。**数据面不读本地手写运行时配置**（§3.5）。控制面不可达时
仅使用编译内置的 bootstrap 兜底快照（不是配置文件）。

## 端点清单

| Method | Path | 处理 |
|---|---|---|
| GET | `/health` | `{"status":"UP","service":"data-plane","version":"1.0.0","revision":N,"now":"..."}` |
| GET | `/v1/admin/traces?limit=100` | `{"spans":[...],"count":N}`，`http.status_code` 整数 |
| GET | `/v1/admin/metrics` | `{"counters":{...}}` |
| GET | `/v1/admin/pool` | `{gets,new,puts,per}`，`new` 远小于 `gets` |
| GET | `/v1/admin/config` | 脱敏快照（**不含 key 明文**）+ revision |
| POST | `/v1/admin/reload` | 强制立即拉 `/v1/config` |
| POST | `/v1/chat/completions` | 模型网关：非流 + `stream:true` SSE |
| POST | `/v1/embeddings` | 模型网关，独立 90s 超时 |
| GET | `/v1/models` | `{"object":"list","data":[...]}` |
| POST/GET | `/a2a/{agentId}...` | 透明反向代理 → Sidecar 9100（A2A 原样透传） |
| POST/GET | `/a2a/v1/...` | 兼容轨（**单前缀 handler + ParseRestPath**，不注册冒号路径） |
|  | `POST /a2a/v1/message:send` | → Task |
|  | `POST /a2a/v1/message:stream` | → SSE（status/artifact/[DONE]） |
|  | `GET /a2a/v1/tasks/{id}` | → Task |
|  | `POST /a2a/v1/tasks/{id}:cancel` | → Task(state=canceled) |
|  | `GET /a2a/v1/tasks/{id}:subscribe` | → SSE |
|  | `GET /a2a/v1/card` | → AgentCard |
| POST/GET | `/mcp` , `/mcp/*` | 透明反向代理 → MCP 9300，响应加 `X-Proxied-By: data-plane` |

错误信封统一 `a2a.NewError`，错误码只用契约 §0.3 表里的，不自造。

## 过滤器链（契约 §3.2，固定顺序）

```
Auth → RateLimit → Guardrail(Input) → Router → Transform → Upstream
                                                  ↓ (响应)
Obs ← Guardrail(Output) ← EncodeHeaders ← EncodeData(逐块) ← Upstream
```

所有实例来自 `a2a.FilterChain` 的 `sync.Pool`（`pkg/a2a/filter.go` 已实现
池化），`Reset()` + `OnDestroy()` 归还，禁止每请求 new 大 buffer。
`GET /v1/admin/pool` 暴露 `gets/new/puts`。

## §3.4 SSE 断流修复（核心）

**问题**：护栏在 `encodeData` 里用 `SendLocalReply` 掐断 SSE，导致
`curl: (18) transfer closed`。根因是 HTTP/1.1 一个连接只有一次请求-响应，
流中注入新响应破坏 `Transfer-Encoding: chunked` 边界。

**本实现的三条硬规则**：

1. `ctx.ResponseStarted == true` 之后，所有 `StopAndReply` / `LocalReply`
   被忽略并降级为 `AppendEncodedData` + `Continue`，记 WARN。
   - `filters/guardrail.go` `OutputGuardrailFilter.EncodeData`
   - `filters/deps.go` `FilterChain.SetWarnLogger`
   - `pkg/a2a/filter.go` `RunDecode` / `RunEncodeData`（共享库已实现）
2. 流式响应 `Content-Type: text/event-stream`、每帧 `Flush()`、不设
   `Content-Length`（`filters/transform.go` `EncodeHeaders`、
   `server.go` `relayStream`、`filterwriter.go`）。
3. 上游超时/断连：发 `event: error` + `data: [DONE]`，**绝不** RST/FIN/close
   （`server.go` `streamUpstreamError`、`filterwriter.go` `finalise`）。

流式护栏命中 → `event: error` 帧 + `data: [DONE]`，连接正常结束（HTTP 仍 200）。
非流式护栏命中 → 200 + `GUARDRAIL_REJECTED` 错误信封。

## 模型网关（契约 §2）

- 统一路由：`modelgw/router.go` 按 model 名/前缀 + 多 backend `weight` 加权轮询。
- 优先级调度：`modelgw/priority.go` 用 `a2a.PriorityScheduler`（3:2:1，channel 容量 64），
  `X-Priority: high|normal|low`。
- Token 限流：`modelgw/limiter.go` 复用同一个 `a2a.MemoryLimiter`，
  `ReserveTokens` 预扣 + `SettleTokens` 结算；超限 429 `RATE_LIMITED` + `Retry-After`。
  请求数 + token 数双维度。
- 协议转换：`modelgw/transform.go` OpenAI Chat Completions / embeddings，
  `usage` 三个字段均为整数；非 OpenAI 后端（`{"text":"..."}`）转成标准结构；
  转换失败返回 `UPSTREAM_ERROR`，不静默丢弃。
- 独立超时：`modelgw/proxy.go` chat 30s / embeddings 90s（E15）。SSE 流不设硬上限。

## 观测（契约 §1.8 / E23）

每个请求产生一个 `a2a.Span`：`http.status_code` 整数、`llm.tokens.*` 整数，
`durationMs` 非 null。`a2a.SpanStore.AddBatch` 逐条容错，一条失败不影响整批。
`/v1/admin/traces` 与 `/v1/admin/metrics` 暴露。

## 单元测试

```powershell
go test ./services/data-plane/... -v
```

| 测试 | 覆盖 |
|---|---|
| `TestStreamingInputGuardrailDoesNotCutConnection` | (a) §3.4 流式护栏：无二次 `WriteHeader`、200、`event: error`+`[DONE]`、无 `Content-Length`、连接正常结束 |
| `TestNonStreamingInputGuardrailReturnsEnvelope` | §3.4 回归 2：非流 200 + `GUARDRAIL_REJECTED` 信封 |
| `TestStreamingOutputGuardrailAppendsErrorFrame` | (a) 输出护栏流式中途命中：降级 `AppendEncodedData`，连接不掐断 |
| `TestNDJSONScanner_128KBSingleLine` | (b) E11 128KB 单行不报 token too long、不丢数据 |
| `TestParseRestPath_ColonPaths` / `TestColonPathNotRegisteredOnServeMux` | (c) E12 冒号路径 / 含冒号 id / `%3A` / ServeMux 不 panic |
| `TestFilterInstancesArePooled` | (d) E22 100 次请求后 `new` << `gets` |
| `TestConfigHotReload` | (e) E21 5s 轮询 + 304 不更新 + revision 变化热替换 + reload 强制拉 |
| `TestAdminConfigIsSanitized` | §3.5 `/v1/admin/config` 不含 key 明文 |

## 构建与验证

```powershell
cd local-agent-stack
go build ./...      # OK (exit 0)
go test ./...       # OK (exit 0)
go vet ./services/data-plane/...   # OK
```

## 只在本目录写文件

本服务只在 `services/data-plane/` 下新增文件；`go.mod` / `go.sum` 由集成方
（lead）单点管理，数据面**不引入任何新依赖**，因此无需改 go.mod。
