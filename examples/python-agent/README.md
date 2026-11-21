# Python 示例 Agent（User Server / NDJSON）· 端口 9103

基于官方 **`a2a-sdk[http-server]==0.3.26`** 的示例 Agent，同时暴露 Sidecar 需要的 NDJSON 协议。

> 版本锁定 `0.3.26`（禁止 1.x：`a2a.server.apps` 被删、types 改 protobuf），见 `docs/06-依赖与版本.md` §4。

## 启动

```powershell
# 依赖（若 .venv 未安装）：
.\.venv\Scripts\python.exe -m pip install -i https://pypi.tuna.tsinghua.edu.cn/simple -r requirements.txt
# 启动：
.\.venv\Scripts\python.exe examples\python-agent\main.py 9103
# 或 uvicorn：
.\.venv\Scripts\python.exe -m uvicorn examples.python-agent.main:app --host 127.0.0.1 --port 9103
```

## 官方 SDK 使用位置（`main.py`）

| 官方 API | 用途 | 位置 |
|---|---|---|
| `a2a.server.apps.A2AStarletteApplication` | `.build(routes=[...])` 生成官方 Starlette app | `build_app` |
| `a2a.server.request_handlers.DefaultRequestHandler` | 装配 executor + task store | `build_a2a_app` |
| `a2a.server.tasks.InMemoryTaskStore` | 官方任务存储 | `build_a2a_app` |
| `a2a.server.agent_execution.AgentExecutor` / `RequestContext` | `EchoExecutor.execute/cancel` | `EchoExecutor` |
| `a2a.server.events.EventQueue.enqueue_event` | 发布官方事件 | `EchoExecutor` |
| `a2a.types.AgentCard/AgentSkill/AgentCapabilities/TaskState/TaskStatus/TaskStatusUpdateEvent` | 卡片与事件类型 | `build_agent_card`/`EchoExecutor` |
| `a2a.utils.new_agent_text_message` | 官方回复消息构造 | `EchoExecutor` |

`build()` 自带 `/`（JSON-RPC）、`/.well-known/agent-card.json`、`/.well-known/agent.json`；
通过 `build(routes=[Route(...)])` 把 NDJSON 用户路由并入同一 app。

## 端点（与 Go / Java Agent 完全一致）

| 方法 | 路径 | 返回 |
|---|---|---|
| GET | `/health` | `{"status":"UP","service":"python-agent"}` |
| POST | `/task` 或 `/run` | `application/x-ndjson` 流 |
| GET | `/.well-known/agent-card.json`、`/.well-known/agent.json` | 官方 AgentCard |
| POST | `/` | 官方 JSON-RPC `message/send` |

NDJSON 流（三行）：
```
{"type":"status","state":"working","progress":0.5}\n
{"type":"artifact","artifactId":"art-1","text":"<回显内容>"}\n
{"type":"done","state":"completed"}\n
```

- 普通输入：artifact.text = 用户文本原样（`ensure_ascii=False`，中文不乱码）
- `long:N`：artifact.text = N 个 `'A'` 的**单行**，`N=128000` 时单行 >128KB，yield 时不截断（E11）
- `StreamingResponse` 逐行 yield，不设 Content-Length

## 坑

- 版本必须锁 `0.3.26`；1.x 的 `a2a.server.apps` 模块被删除。
- 中文 Windows：NDJSON 用 `ensure_ascii=False` + `encode("utf-8")`，避免 `\\uXXXX` 与乱码。
- `TaskState.working` / `.completed` / `.canceled` 是 0.3.26 的枚举值（小写属性），按探针真实 API 写。
