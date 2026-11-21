"""
Python 示例 Agent（User Server / NDJSON）· 端口 9103。

基于官方 a2a-sdk 0.3.26 的 A2AStarletteApplication / DefaultRequestHandler /
InMemoryTaskStore，同时暴露 Sidecar 需要的 NDJSON 协议（契约 §1.7）。

双重身份：
  1) 合规轨：A2AStarletteApplication(agent_card, http_handler).build() 自带
     /（JSON-RPC）/ /.well-known/agent-card.json / /.well-known/agent.json；
  2) 兼容轨：Sidecar 通过 NDJSON /task（或 /run）连它取流。

启动：
    python examples/python-agent/main.py 9103
或：
    uvicorn examples.python-agent.main:app --host 127.0.0.1 --port 9103

端点（与 Go / Java Agent 完全一致）：
    GET  /health                         → {"status":"UP","service":"python-agent"}
    POST /task  |  POST /run             → application/x-ndjson 流
    GET  /.well-known/agent-card.json   → 官方 AgentCard（合规轨）
    POST /                              → 官方 JSON-RPC message/send（合规轨）

NDJSON 流逐行（§1.7）：
    {"type":"status","state":"working","progress":0.5}\n
    {"type":"artifact","artifactId":"art-1","text":"<回显内容>"}\n
    {"type":"done","state":"completed"}\n

long:N 输入 → artifact.text 产生 N 个字符的「单行」（E11 验证 >128KB 不截断）。
"""
from __future__ import annotations

import json
import sys
import uuid
from typing import Any

import uvicorn
from starlette.requests import Request
from starlette.responses import JSONResponse, Response, StreamingResponse
from starlette.routing import Route

# ---- 官方 a2a-sdk 0.3.26 API（参考 _probe/py/a2a_server_min.py）----
from a2a.server.apps import A2AStarletteApplication
from a2a.server.request_handlers import DefaultRequestHandler
from a2a.server.tasks import InMemoryTaskStore
from a2a.server.agent_execution import AgentExecutor, RequestContext
from a2a.server.events import EventQueue
from a2a.types import (
    AgentCapabilities,
    AgentCard,
    AgentSkill,
    Message,
    Part,
    Role,
    TaskState,
    TaskStatus,
    TaskStatusUpdateEvent,
    TextPart,
)
from a2a.utils import new_agent_text_message

SERVICE = "python-agent"
DEFAULT_PORT = 9103


# ---------------------------------------------------------------------------
# 回显 / 文本提取（兼容轨）
# ---------------------------------------------------------------------------

def echo_text(user_text: str) -> str:
    """普通输入原样回显（UTF-8，中文不乱码）；long:N → N 个字符的单行，不截断。"""
    if user_text is None:
        return ""
    if user_text.startswith("long:"):
        try:
            n = int(user_text[len("long:"):].strip())
            if n >= 0:
                return "A" * n  # 单行，长度精确；不自行截断
        except ValueError:
            return user_text
    return user_text


def extract_text(message: Any) -> str:
    """从各种 message 形态提取用户文本：parts[].text / text / content。"""
    if not isinstance(message, dict):
        return ""
    if message.get("text") is not None:
        return str(message["text"])
    if message.get("content") is not None:
        return str(message["content"])
    parts = message.get("parts") or []
    out = []
    for p in parts:
        if isinstance(p, dict) and (p.get("kind") in (None, "", "text") or "text" in p):
            out.append(str(p.get("text", "")))
    return "".join(out)


def ndjson_line(obj: dict) -> bytes:
    """一行一个 JSON 对象，UTF-8、无 BOM、\\n 结尾；超长 text 不截断。"""
    return (json.dumps(obj, ensure_ascii=False, separators=(",", ":")) + "\n").encode("utf-8")


# ---------------------------------------------------------------------------
# 官方 A2A SDK：AgentCard + AgentExecutor + DefaultRequestHandler（合规轨）
# ---------------------------------------------------------------------------

class EchoExecutor(AgentExecutor):
    """官方 a2a-sdk AgentExecutor：publish working / agent message / completed。"""

    async def execute(self, context: RequestContext, event_queue: EventQueue) -> None:
        user_text = context.get_user_input()
        task_id = context.task_id or str(uuid.uuid4())
        context_id = context.context_id or str(uuid.uuid4())

        await event_queue.enqueue_event(
            TaskStatusUpdateEvent(
                task_id=task_id,
                context_id=context_id,
                status=TaskStatus(state=TaskState.working),
                final=False,
            )
        )
        await event_queue.enqueue_event(
            new_agent_text_message(
                text=echo_text(user_text),
                task_id=task_id,
                context_id=context_id,
            )
        )
        await event_queue.enqueue_event(
            TaskStatusUpdateEvent(
                task_id=task_id,
                context_id=context_id,
                status=TaskStatus(state=TaskState.completed),
                final=True,
            )
        )

    async def cancel(self, context: RequestContext, event_queue: EventQueue) -> None:
        await event_queue.enqueue_event(
            TaskStatusUpdateEvent(
                task_id=context.task_id or str(uuid.uuid4()),
                context_id=context.context_id or str(uuid.uuid4()),
                status=TaskStatus(state=TaskState.canceled),
                final=True,
            )
        )


def build_agent_card(url: str) -> AgentCard:
    skill = AgentSkill(
        id="echo",
        name="Echo",
        description="原样回显输入文本，可产生超长 artifact 用于验证 64KB 边界",
        tags=["text", "test"],
        examples=["hello", "long:100000"],
        input_modes=["text"],
        output_modes=["text"],
    )
    return AgentCard(
        name=SERVICE,
        description="Python 示例回显 Agent（User Server / NDJSON）",
        url=url,
        version="1.0.0",
        default_input_modes=["text"],
        default_output_modes=["text"],
        capabilities=AgentCapabilities(streaming=True),
        skills=[skill],
    )


def build_a2a_app(url: str) -> A2AStarletteApplication:
    """用官方 DefaultRequestHandler + InMemoryTaskStore 装配。"""
    card = build_agent_card(url)
    request_handler = DefaultRequestHandler(
        agent_executor=EchoExecutor(),
        task_store=InMemoryTaskStore(),
    )
    return A2AStarletteApplication(agent_card=card, http_handler=request_handler)


# ---------------------------------------------------------------------------
# User Server 端点（NDJSON，契约 §1.7）
# ---------------------------------------------------------------------------

async def health(request: Request) -> Response:
    return JSONResponse({"status": "UP", "service": SERVICE})


async def ndjson_task(request: Request) -> Response:
    """POST /task 或 /run：返回 application/x-ndjson 流（§1.7）。"""
    if request.method != "POST":
        return JSONResponse(
            {"error": {"code": "INVALID_REQUEST", "message": "method not allowed"}},
            status_code=405,
        )
    try:
        raw = await request.body()  # 不假设 64KB；long:N 请求体很小但保持通用
        payload = json.loads(raw.decode("utf-8")) if raw else {}
    except Exception as exc:  # noqa: BLE001
        return Response(
            ndjson_line({"type": "error", "code": "INVALID_REQUEST",
                         "message": f"JSON parse failed: {exc}"}),
            media_type="application/x-ndjson",
        )

    user_text = extract_text(payload.get("message", {}))
    artifact_text = echo_text(user_text)
    task_id = payload.get("taskId", "task-1")

    async def stream():
        # 逐行 yield；artifact 一行可 >128KB，不截断
        yield ndjson_line({"type": "status", "state": "working", "progress": 0.5})
        yield ndjson_line({"type": "artifact", "artifactId": "art-1", "text": artifact_text})
        yield ndjson_line({"type": "done", "state": "completed"})

    return StreamingResponse(
        stream(),
        media_type="application/x-ndjson; charset=UTF-8",
        headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"},
    )


# ---------------------------------------------------------------------------
# app：官方 A2A 路由 + User Server NDJSON 路由
# ---------------------------------------------------------------------------

def build_app(url: str = "http://127.0.0.1:9103/"):
    a2a_app = build_a2a_app(url)
    user_routes = [
        Route("/health", health, methods=["GET"]),
        Route("/task", ndjson_task, methods=["POST"]),
        Route("/run", ndjson_task, methods=["POST"]),
    ]
    # build(routes=[...]) 把 NDJSON 路由并入官方 Starlette app
    return a2a_app.build(routes=user_routes)


app = build_app()


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else DEFAULT_PORT
    app = build_app(f"http://127.0.0.1:{port}/")
    uvicorn.run(app, host="127.0.0.1", port=port, log_level="info")
