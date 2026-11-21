# 04 · API 参考

> 所有服务的端点清单。字段定义以 `01-协议契约.md` 为准；`—` 表示无需鉴权。
> 默认 `X-API-Key: demo-key`（→ `app-demo`），`admin-key`（→ `app-admin`）。

---

## 0. 通用

### 请求头

| Header | 必需 | 说明 |
|---|---|---|
| `X-API-Key` | 数据面端点必需 | `demo-key` / `admin-key` |
| `X-A2A-Version` | 建议 | 固定 `1.0` |
| `X-Request-Id` | 可选 | 不传则数据面生成，全链路透传 |
| `X-Priority` | 可选 | 模型网关 `high`/`normal`/`low` |
| `Accept: text/event-stream` | 流式请求 | |

### 统一错误信封

```json
{"error":{"code":"AUTH_REQUIRED","message":"...","requestId":"...","httpStatus":401,"detail":{}}}
```

### 健康检查（所有服务）

`GET /health` → `200`
```json
{"status":"UP","service":"<name>","version":"1.0.0","revision":12,"now":"2026-11-15T10:00:00+08:00"}
```

---

## 1. 数据面 AI 网关 `:8080`

### 1.1 代理类（原样透传，不解析协议语义）

| Method | Path | 下游 | 说明 |
|---|---|---|---|
| `POST` | `/a2a/{agentId}` | Sidecar `:9100` | A2A JSON-RPC（官方 `SendMessage` 等） |
| `GET` | `/.well-known/agent.json` | Sidecar | 兼容轨 AgentCard |
| `GET` | `/.well-known/agent-card.json` | Sidecar | 官方 AgentCard |
| `POST` | `/v1/chat/completions` | Mock LLM `:9200` | OpenAI 兼容，非流 + SSE |
| `POST` | `/v1/embeddings` | Mock LLM | 独立 90s 超时 |
| `GET` | `/v1/models` | Mock LLM | 模型列表 |
| `POST` | `/mcp/*` | MCP 注册中心 `:9300` | 响应追加 `X-Proxied-By: data-plane` |

### 1.2 兼容轨（冒号路径，E12）

| Method | Path | 返回 |
|---|---|---|
| `POST` | `/a2a/v1/message:send` | 契约 §1.3 `Task` |
| `POST` | `/a2a/v1/message:stream` | SSE（`status-update` + `artifact-update` + `[DONE]`） |
| `GET` | `/a2a/v1/tasks/{id}` | `Task` |
| `POST` | `/a2a/v1/tasks/{id}:cancel` | `Task`（state=`canceled`） |
| `GET` | `/a2a/v1/tasks/{id}:subscribe` | SSE（断点续传） |
| `GET` | `/a2a/v1/card` | 契约 §1.1 `AgentCard` |

> 冒号路径**不注册进 `http.ServeMux`**（会 panic），由单前缀 handler + `a2a.ParseRestPath` 解析。

### 1.3 管理端点

| Method | Path | 返回 |
|---|---|---|
| `GET` | `/v1/admin/traces?limit=100` | `{"spans":[Span...]}` |
| `GET` | `/v1/admin/metrics` | `{"counters":{...}}` |
| `GET` | `/v1/admin/pool` | `{"gets":N,"new":N,"puts":N,"per":{filter:...}}` |
| `GET` | `/v1/admin/config` | 当前生效 revision + 脱敏摘要（无 key 明文） |
| `POST` | `/v1/admin/reload` | 立即强制拉控制面配置 |

---

## 2. 控制面 `:9000`（JDK 11 / Jackson）

| Method | Path | 说明 |
|---|---|---|
| `GET` | `/health` | 健康检查 |
| `GET` | `/v1/config` | **全量快照**（契约 §4），响应头 `ETag: <revision>` |
| `GET` | `/v1/config` + `If-None-Match: <rev>` | 未变 → `304`（空 body） |
| `GET` | `/v1/agents` | 列出全部 agent |
| `POST` | `/v1/agents` | 新增 agent |
| `GET/PUT/DELETE` | `/v1/agents/{id}` | 查 / 改 / 删 |
| `GET/POST/DELETE` | `/v1/skills` `/v1/skills/{id}` | 技能 |
| `GET/POST/PUT/DELETE` | `/v1/policies` | API Key 策略 |
| `GET/POST/DELETE` | `/v1/grants` | app → agent 授权关系 |
| `GET/POST/PUT/DELETE` | `/v1/models` | 模型 backend / 权重 |
| `GET/POST/PUT` | `/v1/ratelimits` | 每 app 的请求/token 配额 |
| `GET/PUT` | `/v1/guardrails` | 护栏词表与开关 |
| `GET/POST/DELETE` | `/v1/webhooks` | Webhook 回调地址 |
| `GET` | `/v1/events` | 最近变更事件（revision 历史） |

**所有写操作** → `revision++` + `generatedAt` 更新 + 持久化。

`GET /v1/config` 响应示例（严格对齐契约 §4）：
```json
{
  "revision": 12,
  "generatedAt": "2026-11-15T10:00:00+08:00",
  "agents": {"echo-agent": {"agentId":"echo-agent","url":"http://127.0.0.1:9100/a2a/echo-agent","enabled":true,"version":"1.0.0","skills":["echo"],"authPolicy":"default","webhooks":[],"metadata":{"userServer":"http://127.0.0.1:9101"}}},
  "policies": {"default":{"type":"apiKey","keys":{"demo-key":"app-demo","admin-key":"app-admin"},"allowAnonymous":false}},
  "grants": [{"appId":"app-demo","agentId":"*","scopes":["*"]}],
  "models": [{"name":"mock-1","backend":"http://127.0.0.1:9200","priority":10,"weight":100,"enabled":true}],
  "rateLimits": {"default":{"requestsPerMinute":120,"tokensPerMinute":60000,"burst":20}},
  "guardrails": {"blockedWords":["BLOCK_ME","secret-leak"],"blockedTools":["dangerous_delete","drop_database"],"maxPromptChars":8000,"enableInput":true,"enableOutput":true}
}
```

---

## 3. AgentMecha Sidecar `:9100`（Go / 官方 a2a-go/v2）

| Method | Path | 说明 |
|---|---|---|
| `GET` | `/health` | 健康检查 |
| `POST` | `/a2a/{agentId}` | 官方 `a2asrv.NewJSONRPCHandler`（`SendMessage` / `SendStreamingMessage` / `GetTask` / `CancelTask` / `SubscribeToTask`） |
| `POST` | `/a2a/{agentId}/` | 官方 `a2asrv.NewRESTHandler`（`/tasks/{idAndAction}` + `CutSuffix`，规避冒号 panic） |
| `GET` | `/.well-known/agent-card.json` | 官方 `NewStaticAgentCardHandler` |
| `GET` | `/.well-known/agent.json` | 兼容轨 AgentCard |
| `POST` | `/a2a/v1/message:send` | 兼容轨 |
| `POST` | `/a2a/v1/tasks/{id}:cancel` | 兼容轨 |
| `GET` | `/a2a/v1/tasks/{id}:subscribe` | 兼容轨 SSE |
| `GET` | `/admin/hooks` | 已注册 Hook 列表 |
| `POST` | `/admin/hooks/reload` | 重新装载护栏配置 |
| `GET` | `/admin/tasks` | 任务列表（E24/E25） |
| `GET` | `/admin/store` | StateStore 摘要 |

**User Server NDJSON 协议**（Sidecar → `:9101/9102/9103`）：
```
POST /task  {"type":"task","taskId":"...","message":{...}}
→ application/x-ndjson
{"type":"status","state":"working","progress":0.5}\n
{"type":"artifact","artifactId":"art-1","text":"..."}\n   ← 可 >128KB 单行
{"type":"done","state":"completed"}\n
```

---

## 4. Mock LLM `:9200`（Python / FastAPI）

| Method | Path | 说明 |
|---|---|---|
| `GET` | `/health` | 健康检查 |
| `POST` | `/v1/chat/completions` | OpenAI 兼容；`stream:false` 返回 `chat.completion`，`stream:true` 返回 SSE chunks + `[DONE]` |
| `POST` | `/v1/embeddings` | 接受超长输入（>8000 字符），返回向量 |
| `GET` | `/v1/models` | `mock-1` / `bge-m3` |
| `POST` | `/tools/echo` | 回显 HTTP 请求（供 MCP `tools/call` 回放） |

`MOCK_MODE=slow` 让 embeddings 延迟 2s（超时测试用）。

---

## 5. MCP 注册中心 `:9300`（JDK 11 / Jackson）

| Method | Path | 说明 |
|---|---|---|
| `GET` | `/health` | 健康检查 |
| `POST` | `/mcp` | JSON-RPC 2.0：`initialize` / `tools/list` / `tools/call` / `tools/get` / `ping` |
| `POST` | `/registry/import` | **OpenAPI 3.0 → MCP Tool**（`$ref` 递归展开、allOf 合并、path/query/body 参数） |
| `POST` | `/registry/validate` | 只校验不导入 |
| `GET` | `/registry/tools` | 已注册工具 |
| `DELETE` | `/registry/tools/{name}` | 删除工具 |
| `GET` | `/registry/specs` | 已导入 OpenAPI 文档 |

`POST /registry/import` 请求：
```json
{"spec": { "...OpenAPI 3.0..." }, "baseUrl": "http://127.0.0.1:9200", "source": "petstore", "replace": false}
```
响应：
```json
{"imported": 2, "tools": ["getUserById", "createUser_post"], "skipped": [{"path":"/health","method":"get","reason":"no operationId"}]}
```

`tools/call` 回放：用 `source.path/method/baseUrl` 构造真实 HTTP 请求，命中 `/tools/echo`，
返回 `{"content":[{"type":"text","text":"<HTTP 响应体>"}],"isError":false}`。

---

## 6. 三语言 SDK 统一接口（契约 §7）

| 能力 | Go | Java | Python |
|---|---|---|---|
| 构造 | `NewClient(opts...)` | `new A2AClient.Builder()...build()` | `A2AClient(base_url, api_key=...)` |
| 发现 | `GetAgentCard(ctx)` | `getAgentCard()` | `get_agent_card()` |
| 发消息 | `SendMessage(ctx,msg)` | `sendMessage(Message)` | `send_message(message)` |
| 流式 | `StreamMessage(ctx,msg) <-chan Event` | `streamMessage(Message, Consumer<Event>)` | `stream_message(message)` (generator) |
| 查任务 | `GetTask(ctx,id)` | `getTask(String)` | `get_task(task_id)` |
| 取消 | `CancelTask(ctx,id)` | `cancelTask(String)` | `cancel_task(task_id)` |
| 订阅 | `Subscribe(ctx,id)` | `subscribe(String, Consumer<Event>)` | `subscribe(task_id)` |
| 模型 | `Chat(ctx,req)` | `chat(ChatRequest)` | `chat(**kwargs)` |
| MCP | `ListTools` / `CallTool` | `listTools()` / `callTool(String,Map)` | `list_tools()` / `call_tool(name,**kwargs)` |
| 护栏 | `CheckGuardrail(text)` | `checkGuardrail(String)` | `check_guardrail(text)` |

默认 baseURL = `http://127.0.0.1:8080`（数据面），可直连 Sidecar `:9100`。

---

## 7. 错误码速查（契约 §0.3）

| code | HTTP |
|---|---|
| `INVALID_REQUEST` | 400 |
| `AUTH_REQUIRED` | 401 |
| `FORBIDDEN` | 403 |
| `AGENT_NOT_FOUND` / `MODEL_NOT_FOUND` / `MCP_TOOL_NOT_FOUND` / `TASK_NOT_FOUND` | 404 |
| `RATE_LIMITED` | 429 |
| `GUARDRAIL_REJECTED` | 200（流式 SSE error 帧 / 非流信封） |
| `UPSTREAM_ERROR` | 502 |
| `UPSTREAM_TIMEOUT` | 504 |
| `STREAM_CLOSED` / `INTERNAL_ERROR` | 200 / 500 |

JSON-RPC 错误码：`-32700` 解析、`-32600` 非法请求、`-32601` 方法不存在、`-32602` 参数无效、`-32603` 内部错误。
