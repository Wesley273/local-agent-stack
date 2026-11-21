# -*- coding: utf-8 -*-
"""Mock LLM 服务（端口 9200）—— local-agent-stack 数据面的 OpenAI 兼容后端。

启动：
    .venv\\Scripts\\python.exe services/mock-llm/main.py 9200
    .venv\\Scripts\\python.exe services/mock-llm/main.py          # 读 LLM_PORT 或 9200
    LLM_PORT=9200 MOCK_MODE=slow .venv\\Scripts\\python.exe services/mock-llm/main.py

环境变量：
    MOCK_MODE   echo(默认) | slow —— slow 时 /v1/embeddings 先睡 2s，供数据面超时测试
    LLM_PORT    端口（命令行参数优先）
    LLM_HOST    监听地址，默认 127.0.0.1

实现：FastAPI + uvicorn（官方栈）。所有响应 Content-Type 带 charset=utf-8，
中文 body 用 ensure_ascii=False，不乱码。流式响应 StreamingResponse 不设 Content-Length。
"""
from __future__ import annotations

import json
import os
import sys
import time
import uuid
from datetime import datetime, timezone, timedelta
from pathlib import Path
from typing import Any

# 允许 `python services/mock-llm/main.py` 直接运行时 import 同目录模块
sys.path.insert(0, str(Path(__file__).resolve().parent))

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, StreamingResponse

import openai_compat as oc
import tools_echo as te

MOCK_MODE = os.environ.get("MOCK_MODE", "echo").strip().lower()  # echo | slow
SERVICE = "mock-llm"
VERSION = "1.0.0"
JSON_CTYPE = "application/json; charset=utf-8"
SSE_CTYPE = "text/event-stream; charset=utf-8"

app = FastAPI(title="local-agent-stack Mock LLM", version=VERSION, docs_url=None, redoc_url=None)


# ---------------------------------------------------------------------------
# 统一响应：契约 §0.4 / 硬要求「所有响应 Content-Type: application/json; charset=utf-8」
# 覆盖 FastAPI 默认（application/json 无 charset）与中文乱码问题。
# ---------------------------------------------------------------------------
class UTF8JSONResponse(JSONResponse):
    media_type = JSON_CTYPE

    def render(self, content: Any) -> bytes:
        return json.dumps(
            content,
            ensure_ascii=False,
            allow_nan=False,
            indent=None,
            separators=(",", ":"),
        ).encode("utf-8")


def now_rfc3339() -> str:
    """RFC3339 字符串（+08:00 时区，契约 §0 约定）。"""
    tz = timezone(timedelta(hours=8))
    return datetime.now(tz).isoformat(timespec="seconds")


@app.middleware("http")
async def request_id_and_utf8(request: Request, call_next):
    """全链路 X-Request-Id 透传 + 所有非流响应强制 UTF-8 JSON Content-Type。"""
    incoming = request.headers.get("X-Request-Id") or request.headers.get("x-request-id")
    rid = incoming or uuid.uuid4().hex
    request.state.request_id = rid
    response = await call_next(request)
    # 契约 §0.2：X-Request-Id 必须原样透传给所有下游 / 回给调用方
    response.headers["X-Request-Id"] = rid
    ctype = response.headers.get("content-type", "")
    if ctype.startswith("application/json") and "charset" not in ctype.lower():
        response.headers["content-type"] = JSON_CTYPE
    return response


# ---------------------------------------------------------------------------
# 1) 健康检查（契约 §0.4）
# ---------------------------------------------------------------------------
@app.get("/health")
async def health() -> UTF8JSONResponse:
    return UTF8JSONResponse(
        {
            "status": "UP",
            "service": SERVICE,
            "version": VERSION,
            "now": now_rfc3339(),
        }
    )


# ---------------------------------------------------------------------------
# 2) POST /v1/chat/completions（契约 §2.1）
# ---------------------------------------------------------------------------
async def _read_json(request: Request) -> dict[str, Any]:
    """读 JSON body；非法 JSON 返回契约 §0.3 的 INVALID_REQUEST 错误信封。"""
    raw = await request.body()
    if not raw:
        return {}
    try:
        obj = json.loads(raw.decode("utf-8"))
    except (ValueError, UnicodeDecodeError):
        return {"__invalid_json__": True}
    return obj if isinstance(obj, dict) else {"__invalid_json__": True}


def _invalid_request(request: Request, message: str) -> UTF8JSONResponse:
    rid = getattr(request.state, "request_id", uuid.uuid4().hex)
    return UTF8JSONResponse(
        status_code=400,
        content={
            "error": {
                "code": "INVALID_REQUEST",
                "message": message,
                "requestId": rid,
                "httpStatus": 400,
                "detail": {},
            }
        },
    )


@app.post("/v1/chat/completions")
async def chat_completions(request: Request):
    body = await _read_json(request)
    if body.get("__invalid_json__"):
        return _invalid_request(request, "JSON 解析失败 / 缺必填字段")

    messages = body.get("messages")
    if not isinstance(messages, list):
        return _invalid_request(request, "messages 必填且须为数组")

    model = body.get("model") or oc.MOCK_CHAT_MODEL
    stream = bool(body.get("stream", False))
    max_tokens = body.get("max_tokens")
    # temperature 仅用于回显式展示（契约里是请求字段，不影响 deterministic mock）

    if stream:
        # 契约 §2.1 / §3.4：流式不设 Content-Length、每帧即时 flush、中文 UTF-8
        def gen():
            for frame in oc.iter_chat_sse(model, messages, max_tokens):
                yield frame.encode("utf-8")

        return StreamingResponse(
            gen(),
            media_type=SSE_CTYPE,
            headers={
                "Cache-Control": "no-cache",
                "Connection": "keep-alive",
                "X-Accel-Buffering": "no",
                "X-Request-Id": getattr(request.state, "request_id", ""),
            },
        )

    payload = oc.build_chat_completion(model, messages, max_tokens, body.get("temperature"))
    return UTF8JSONResponse(payload)


# ---------------------------------------------------------------------------
# 3) POST /v1/embeddings（契约 §2.2 / E15）
#    必须能接受超长输入（>8000 字符，例如 124844 字节）而不报错。
#    uvicorn 默认不限制 body 大小；这里用 request.body() 直接读全部字节，
#    只做 sha256 摘要，不复制/截断，故 124844 字节无压力。
# ---------------------------------------------------------------------------
@app.post("/v1/embeddings")
async def embeddings(request: Request):
    if MOCK_MODE == "slow":
        # 用于复现数据面 embeddings 独立 90s 超时 / 客户端超时排查（E15）
        time.sleep(2)
    body = await _read_json(request)
    if body.get("__invalid_json__"):
        return _invalid_request(request, "JSON 解析失败")
    if "input" not in body:
        return _invalid_request(request, "input 必填")

    payload = oc.build_embeddings_response(
        body.get("model") or oc.MOCK_EMBED_MODEL,
        body.get("input"),
        body.get("encoding_format", "float"),
    )
    return UTF8JSONResponse(payload)


# ---------------------------------------------------------------------------
# 4) GET /v1/models（契约 §2.3）
# ---------------------------------------------------------------------------
@app.get("/v1/models")
async def list_models(request: Request) -> UTF8JSONResponse:
    host = request.headers.get("host", "127.0.0.1:9200")
    backend = f"http://{host}"
    return UTF8JSONResponse(oc.build_models_response(backend))


# ---------------------------------------------------------------------------
# 5) POST /tools/echo —— 真实回显 HTTP 请求（契约 §5.2 / E19）
#    注册中心 tools/call 回放时构造的 method/path/query/body 全部原样放进 JSON。
#    同时支持任意子路径与任意 HTTP 方法（GET/POST/PUT/DELETE/PATCH），
#    便于注册中心把 source.path 回放过来。
# ---------------------------------------------------------------------------
@app.api_route("/tools/echo", methods=["GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"])
@app.api_route("/tools/echo/{rest:path}", methods=["GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"])
async def tools_echo(request: Request, rest: str = "") -> UTF8JSONResponse:
    raw = await request.body()
    echo = te.echo_from_asgi_scope(request.scope, raw)
    return UTF8JSONResponse(echo)


# 通用兜底：任何路径都回显，便于 MCP 回放任意 source.path（E19）
@app.api_route("/{full_path:path}", methods=["GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"])
async def catch_all_echo(request: Request, full_path: str) -> UTF8JSONResponse:
    raw = await request.body()
    echo = te.echo_from_asgi_scope(request.scope, raw)
    echo["echo"]["note"] = "catch-all echo (MCP tools/call replay fallback)"
    return UTF8JSONResponse(echo)


# ---------------------------------------------------------------------------
# 启动入口：端口从命令行参数或 LLM_PORT 环境变量读
# ---------------------------------------------------------------------------
def resolve_port(argv: list[str]) -> int:
    if argv:
        try:
            return int(argv[0])
        except ValueError:
            pass
    env = os.environ.get("LLM_PORT", "9200")
    try:
        return int(env)
    except ValueError:
        return 9200


if __name__ == "__main__":
    import uvicorn

    port = resolve_port(sys.argv[1:])
    host = os.environ.get("LLM_HOST", "127.0.0.1")
    # Windows 控制台中文：设 PYTHONIOENCODING（本文件并不 print 中文，兜底）
    os.environ.setdefault("PYTHONIOENCODING", "utf-8")
    print(f"[mock-llm] MOCK_MODE={MOCK_MODE} listening http://{host}:{port}", flush=True)
    uvicorn.run(
        app,
        host=host,
        port=port,
        log_level="warning",
        access_log=False,
        # 关键：不限制请求体大小，保证 124844 字节 embeddings 输入可达
        limit_concurrency=None,
    )
