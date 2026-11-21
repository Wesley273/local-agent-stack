# Python SDK（`las_sdk`，契约 §7）

`local-agent-stack` 三语言对齐的 Python SDK。**基于官方 `a2a-sdk` / `mcp` 二次开发**，
不是完全手写协议。统一接口与 Go / Java SDK 同名同语义。

> 本目录与 `services/mock-llm/`、`requirements.txt` 由同一个 Python 子任务负责；不修改其他模块。

## 1. 依赖版本决策（诚实记录，别假装用了最新版）

按 `docs/06-依赖与版本.md` §4 与探针 agent-3 实测：

| 库 | PyPI 最新 | **本 SDK 实际锁定** | 原因 |
|---|---|---|---|
| `a2a-sdk` | 1.3.0 | **0.3.26**（`[http-server]` extra） | **因 1.x breaking change 固定为 0.3.26**：1.x 删除 `a2a.server.apps`（`ModuleNotFoundError`）、`a2a.types` 改 protobuf，无法 `from a2a.server.apps import A2AStarletteApplication`。0.3.26 实测 `a2a.types` / `a2a.client` / `a2a.server.apps` 全可 import，`message/send` 等方法名与契约 §1.4 一致。 |
| `mcp` | 2.3.0 | **1.30.0** | **因 2.x breaking change 固定为 1.30.0**：2.x 把 `FastMCP` 改名 `MCPServer`、字段蛇形化，`from mcp.server.fastmcp import FastMCP` 抛异常。1.30.0 实测 `FastMCP` / `@mcp.tool()` / `await list_tools()` / `await call_tool()` 可用，本 SDK 用其 `mcp.types` 结果模型。 |
| `pydantic` | — | 2.11.3 | `a2a-sdk==0.3.26` 要求 `pydantic>=2.11.3`（2.10.4 会 `ResolutionImpossible`）。 |
| `fastapi`/`uvicorn`/`httpx` | — | 0.115.6 / 0.34.0 / 0.28.1 | 固定版本。 |

**实测 import 验证结果**（`.venv\Scripts\python.exe _sdk_probe*.py`，输出见交付报告）：
- `a2a.types.AgentCard / Task / Message / TaskStatus / AgentCapabilities / AgentSkill / TaskStatusUpdateEvent / TaskArtifactUpdateEvent / SendMessageRequest / GetTaskRequest / CancelTaskRequest / TaskResubscriptionRequest` 全部存在；
- `a2a.client.A2AClient / A2ACardResolver / ClientFactory` 全部存在；
- `a2a.server.apps.A2AStarletteApplication(...).build()` 实测能构造 Starlette（路由 `/`、`/.well-known/agent-card.json`、`/.well-known/agent.json`）；
- `mcp.server.fastmcp.FastMCP` 可 import，`await list_tools()` / `await call_tool()` 实测可用；
- `mcp.types.Tool / CallToolResult / ListToolsResult / TextContent` 全部存在。

## 2. 官方 SDK 的具体使用位置（文件:行号）

| 官方依赖 | 位置 | 用法 |
|---|---|---|
| `a2a.types` | `las_sdk/client.py:29-52` | import 官方协议类型（AgentCard / Message / Task / Part / TextPart / Role / SendMessageRequest / GetTaskRequest / CancelTaskRequest / TaskResubscriptionRequest / TaskStatusUpdateEvent / TaskArtifactUpdateEvent 等） |
| `a2a.types` | `las_sdk/client.py:87-115` | `_normalize_message()` 把 str/dict/Message 统一构造成官方 `a2a.types.Message`，出门 JSON 由官方 pydantic 模型生成 |
| `a2a.types` | `las_sdk/client.py:78-84` | `_to_dict()` 用 `model_dump(by_alias=True, exclude_none=True, mode="json")` 生成契约 camelCase JSON |
| `a2a.types` | `las_sdk/client.py:276-296` | `get_agent_card()` 用官方 `AgentCard.model_validate()` 反序列化 |
| `a2a.types` | `las_sdk/client.py:301-338` | `send_message()` 用官方 `SendMessageRequest` + `MessageSendParams` + `MessageSendConfiguration` 构造 JSON-RPC；`_coerce_task_or_message()`（:327-337）用官方 `Task.model_validate` / `Message.model_validate` 反序列化结果 |
| `a2a.types` | `las_sdk/client.py:340-365` | `stream_message()` 用官方 `SendStreamingMessageRequest` 构造 `message/stream` |
| `a2a.types` | `las_sdk/client.py:367-399` | `get_task()` / `cancel_task()` / `subscribe()` 用官方 `GetTaskRequest` / `CancelTaskRequest` / `TaskResubscriptionRequest` + `TaskQueryParams` / `TaskIdParams` |
| `a2a.client` | `las_sdk/client.py:54-56` | import 官方 `A2AClient` / `ClientFactory` / `A2ACardResolver`；`client.py:258-271` 的 `_official_a2a_client()` 构造官方底层客户端（证据：SDK 内部确实持有官方客户端） |
| `mcp.types` | `las_sdk/mcp.py:22-23` | import 官方 `ListToolsResult` / `CallToolResult` / `Tool` / `TextContent` / `JSONRPCError` |
| `mcp.types` | `las_sdk/mcp.py:49-71` | `parse_tools_list()` / `parse_call_tool_result()` 用官方模型反序列化 `tools/list` / `tools/call` result |
| `mcp.types` | `las_sdk/mcp.py:94-127` | `MCPClient.list_tools()` 返回官方 `mcp.types.Tool` 列表；`call_tool()` 返回官方 `CallToolResult`；`texts()` 抽 `TextContent.text` |
| `mcp.types` | `las_sdk/client.py:60-61`、`las_sdk/__init__.py:25` | 官方 MCP 类型从包顶层透出 |

> 行号基于本次提交的最终文件内容（UTF-8，无 BOM）。

## 3. 契约差异说明（不隐瞒）

**为什么用官方类型 + httpx 传输，而不是直接 `A2AClient.send_message()` 全托管？**

契约 §0.0「协议双轨制」明确：数据面是「治理层 + 透明反向代理」，SDK 必须：
1. 统一打 **8080**（而非直连 Agent 9100），由数据面按 agentId 路由；
2. 自动带 `X-A2A-Version: 1.0`、`X-Request-Id`（uuid4）、`X-API-Key`（§0.2）；
3. 处理契约 §0.3 统一错误信封（不是纯 JSON-RPC error）、流式 `event: error` 帧（§3.4 SSE 不断流）、`UPSTREAM_TIMEOUT` → 原生 `TimeoutError`（§7）；
4. 支持兼容轨冒号路径、SSE 跨 chunk 拼接、NDJSON >64KB 单行；
5. `chat()` 走 `/v1/chat/completions`、`list_tools/call_tool` 走单条 `POST /mcp` JSON-RPC（§5.3 反向代理），而官方 `mcp` 1.30.0 是 stdio/SSE/streamable-http 会话传输。

这些「企业级封装」官方 SDK 不提供，正是自研价值（§0 总体规划 v2 第 3 层）。
因此：**协议模型/序列化用官方 `a2a.types`，MCP 结果模型用官方 `mcp.types`，
传输层用 httpx 直发** —— 不是完全手写协议，也不是纯官方托管，README 如实标注差异。
`_official_a2a_client()` 额外保留构造纯官方底层客户端的能力，供合规轨直连使用。

## 4. 接口一览（契约 §7）

```python
from las_sdk import A2AClient, A2AError, A2ATimeoutError, Event, check_guardrail

c = A2AClient("http://127.0.0.1:8080", api_key="demo-key", timeout=30)

c.get_agent_card()                 # -> 官方 a2a.types.AgentCard
c.send_message("hello")            # -> 官方 a2a.types.Task / Message（JSON-RPC message/send）
for ev in c.stream_message("hi"):  # generator, yield sse.Event（message/stream）
    print(ev.event, ev.data)
c.get_task("task-1")               # tasks/get
c.cancel_task("task-1")            # tasks/cancel
for ev in c.subscribe("task-1"):   # generator（tasks/resubscribe SSE）
    ...
c.chat(model="mock-1", messages=[{"role":"user","content":"hi"}])        # 非流 -> dict
for ch in c.chat(model="mock-1", messages=[...], stream=True):             # generator -> chunk
    ...
c.list_tools()                     # POST /mcp tools/list -> list[mcp.types.Tool]
c.call_tool("echo", text="hi")     # POST /mcp tools/call -> mcp.types.CallToolResult
c.check_guardrail("BLOCK_ME")      # 本地护栏 -> GuardrailResult(allowed=False, code=GUARDRAIL_REJECTED)
```

- 自动请求头：`X-A2A-Version: 1.0`、`X-Request-Id: <uuid4 hex>`、`X-API-Key`。
- 超时：`A2ATimeoutError` 是 `A2AError` + **原生 `TimeoutError`** 双继承（契约 §7）；连接失败是 `A2AConnectionError`（原生 `ConnectionError` 子类）。
- 错误映射：统一错误信封 / JSON-RPC `error.data.code` → `A2AError(code, message, request_id)`（§0.3）。

## 5. SSE 跨 chunk 拼接 & NDJSON >64KB（契约 §7 硬要求）

`las_sdk/sse.py` 的 `SSEBuffer` 是增量缓冲：
- `feed_bytes(chunk)` 可喂任意切分的 TCP chunk，内部按 `\n` 分行、未完成行留到下一次，**绝不假设一次 read 一帧**；
- 空行才触发事件；多行 `data:` 按 SSE 规范用 `\n` 连接；
- `iter_sse_events()` 基于 `httpx.Response.iter_lines()`，再交给 `SSEBuffer` 聚合事件；
- `iter_ndjson_objects()` 单行无长度上限（Python str 无 64KB 限制），直接 `json.loads`，**不做任何截断**，满足 §1.7 的 128KB 单行 artifact 场景。

## 6. 运行 example

```powershell
# 项目根 local-agent-stack/
$env:PYTHONIOENCODING = 'utf-8'
.\.venv\Scripts\python.exe sdk/python/example/main.py
```

`example/main.py` 依次证明：
`get_agent_card → send_message → stream_message → chat → chat(stream) → list_tools → call_tool`，
**全部打数据面 8080**。每一步都打印真实结果；下游未启动时如实报「不可达」并退出码 1，不伪造成功。

## 7. 文件清单

```
sdk/python/
├── las_sdk/
│   ├── __init__.py     # 统一导出
│   ├── client.py       # A2AClient（契约 §7 全部方法）
│   ├── sse.py          # SSEBuffer 跨 chunk + NDJSON >64KB + Event
│   ├── errors.py       # A2AError / A2ATimeoutError（原生 TimeoutError 子类）
│   ├── guardrail.py    # check_guardrail / check_tool / configure
│   └── mcp.py          # list_tools / call_tool（官方 mcp.types）
├── example/main.py     # 可运行全链路示例
└── README.md
```

## 8. 与 E2E 用例的关系

| 用例 | SDK 侧能力 |
|---|---|
| E08 | `send_message` 基于官方 `a2a.types` 打数据面，返回 Task |
| E09 / E24 | `stream_message` / `subscribe` generator + 跨 chunk SSE |
| E10 | 流式 error 帧不中断连接；`check_guardrail` 本地预检 |
| E11 | NDJSON >64KB 单行解析 |
| E13 / E14 | `chat()` 非流 / 流式 |
| E15 | embeddings 超时映射（SDK 侧 `UPSTREAM_TIMEOUT` → `TimeoutError`） |
| E18 / E19 | `list_tools` / `call_tool` 走 `POST /mcp` |
| E20 | 统一走数据面 8080，带治理头 |
