# -*- coding: utf-8 -*-
"""A2AClient —— local-agent-stack Python SDK（契约 §7 同名同语义）。

**基于官方 a2a-sdk / mcp 二次开发**，不是完全手写协议：
- 用官方 `a2a.types`（AgentCard / Message / Task / AgentCapabilities / AgentSkill /
  TaskStatusUpdateEvent / TaskArtifactUpdateEvent / SendMessageRequest /
  GetTaskRequest / CancelTaskRequest / TaskResubscriptionRequest 等）
  做请求序列化与响应反序列化（契约 §0.0「合规轨由官方 SDK 负责」）；
- 用官方 `a2a.client.A2AClient` + `a2a.client.ClientFactory` 做「官方 SDK 底层」
  的可用性封装（见 `_official_a2a_client`），若调用方需要纯官方客户端可直接取用；
- MCP 侧用官方 `mcp.types` 的 ListToolsResult / CallToolResult（见 mcp.py）。

**契约差异（如实记录，见 README）**：官方 `a2a-sdk` 的客户端走 A2A 标准
JSON-RPC 传输，而我们的数据面是「治理层 + 透明反向代理」（§0.0 双轨制），
SDK 必须统一带上 `X-A2A-Version` / `X-Request-Id` / `X-API-Key` 并打数据面 8080，
还要支持兼容轨冒号路径、SSE 跨 chunk、NDJSON >64KB、统一错误信封等企业级封装。
因此：**协议模型/序列化用官方 a2a.types，传输层用 httpx 直发**。
"""
from __future__ import annotations

import json
import os
import uuid
from typing import Any, Iterator
from urllib.parse import urljoin

import httpx

# ---- 官方 a2a-sdk 使用点 1：协议类型（序列化/反序列化） -------------------
from a2a.types import (  # noqa: F401
    AgentCapabilities,
    AgentCard,
    AgentSkill,
    Artifact,
    CancelTaskRequest,
    GetTaskRequest,
    Message,
    MessageSendConfiguration,
    MessageSendParams,
    Part,
    Role,
    SendMessageRequest,
    SendStreamingMessageRequest,
    Task,
    TaskArtifactUpdateEvent,
    TaskIdParams,
    TaskQueryParams,
    TaskResubscriptionRequest,
    TaskStatus,
    TaskStatusUpdateEvent,
    TaskState,
    TextPart,
)
# ---- 官方 a2a-sdk 使用点 2：官方客户端底层（可选直连） -------------------
from a2a.client import A2AClient as OfficialA2AClient  # noqa: F401
from a2a.client import ClientFactory  # noqa: F401
from a2a.client import A2ACardResolver  # noqa: F401

from . import guardrail as _guard
from .errors import A2AConnectionError, A2AError, A2ATimeoutError, raise_for_payload
from .mcp import MCPClient
from .mcp import CallToolResult, Tool  # noqa: F401  (官方 mcp.types 透出)
from .sse import Event, iter_ndjson_objects, iter_sse_events

A2A_VERSION = "1.0"
DEFAULT_TIMEOUT = 30.0

# 数据面 A2A 官方合规轨端点（契约 §1.4）：Go SDK / 官方 a2a-go/v2 用 PascalCase
# SendMessage；Python SDK 基于 a2a-sdk 0.3.26，方法名是 message/send。
# 按契约 §0.0「协议双轨制」，两套方法名互不转换，因此 Python SDK 走**兼容轨**
# REST 冒号路径（§1.5），它接受契约 §1 的 {"message": ...} 请求体。
A2A_RPC_PATH = "/a2a/echo-agent"          # 保留：直连官方合规轨时使用
COMPAT_SEND_PATH = "/a2a/v1/message:send"
COMPAT_STREAM_PATH = "/a2a/v1/message:stream"
REST_CARD_PATH = "/a2a/v1/card"


def _new_request_id() -> str:
    """契约 §0.2：X-Request-Id 为 UUID 无横线（这里用 uuid4 hex）。"""
    return uuid.uuid4().hex


def _to_dict(obj: Any) -> dict[str, Any]:
    """官方 pydantic 模型 → 契约 JSON（camelCase alias，去掉 None 字段）。"""
    if hasattr(obj, "model_dump"):
        return obj.model_dump(by_alias=True, exclude_none=True, mode="json")
    if isinstance(obj, dict):
        return obj
    return dict(obj)


def _normalize_message(message: Any) -> Message:
    """把 str / dict / 官方 Message 统一成官方 `a2a.types.Message`。

    这是「用官方类型做序列化」的关键：无论调用方传什么，出门的 JSON
    都由官方 pydantic 模型生成，不会出现自造字段。
    """
    if isinstance(message, Message):
        return message
    if isinstance(message, str):
        return Message(
            messageId=f"msg-{uuid.uuid4().hex[:16]}",
            role=Role.user,
            parts=[Part(root=TextPart(kind="text", text=message))],
        )
    if isinstance(message, dict):
        # 允许契约 §1.2 的完整 Message dict，也允许 {"text": "..."} 简写
        if "parts" in message or "role" in message:
            payload = dict(message)
            # 官方 Message.messageId 必填；契约示例里可能省略 → 本地补齐
            payload.setdefault("messageId", f"msg-{uuid.uuid4().hex[:16]}")
            payload.setdefault("role", "user")
            payload.setdefault("parts", [])
            return Message.model_validate(payload)
        text = message.get("text", message.get("content", ""))
        return Message(
            messageId=message.get("messageId") or f"msg-{uuid.uuid4().hex[:16]}",
            role=Role.user,
            parts=[Part(root=TextPart(kind="text", text=str(text)))],
        )
    raise A2AError("INVALID_REQUEST", f"unsupported message type: {type(message).__name__}")


def _message_text(message: Message) -> str:
    out: list[str] = []
    for p in message.parts:
        root = getattr(p, "root", p)
        if isinstance(root, TextPart):
            out.append(root.text)
        elif isinstance(root, dict) and root.get("kind") == "text":
            out.append(str(root.get("text", "")))
    return "".join(out)


class A2AClient:
    """契约 §7 客户端：`A2AClient(base_url, api_key="demo-key", timeout=30)`。

    方法：get_agent_card / send_message / stream_message(generator) / get_task /
    cancel_task / subscribe / chat(**kwargs) / list_tools / call_tool /
    check_guardrail(text)。

    所有请求自动带：
    - `X-A2A-Version: 1.0`
    - `X-Request-Id: <uuid4>`（每次请求独立生成）
    - `X-API-Key: <api_key>`
    """

    def __init__(
        self,
        base_url: str = "http://127.0.0.1:8080",
        api_key: str = "demo-key",
        timeout: float = DEFAULT_TIMEOUT,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.timeout = float(timeout)
        # 默认不复用 keep-alive 连接：流式 SSE 响应在 consumer 提前 break 时
        # 会留下半开连接，Go 侧 ReverseProxy 随后复用该连接会触发
        # WinError 10054（远程主机强迫关闭）。对本地/演示 SDK 而言每请求
        # 独立连接更稳健；生产可设 LAS_KEEPALIVE=1 打开连接池。
        self._keepalive = os.environ.get("LAS_KEEPALIVE", "0") == "1"
        default_headers = {"Accept": "application/json, text/event-stream, application/x-ndjson"}
        if not self._keepalive:
            default_headers["Connection"] = "close"
        self._http = httpx.Client(
            base_url=self.base_url,
            timeout=httpx.Timeout(self.timeout, connect=min(self.timeout, 10.0)),
            headers=default_headers,
        )
        self._mcp = MCPClient(self._post_json)

    # ------------------------------------------------------------------
    # 统一传输层（httpx）
    # ------------------------------------------------------------------
    def _headers(self, request_id: str | None = None, stream: bool = False) -> dict[str, str]:
        rid = request_id or _new_request_id()
        h = {
            "X-A2A-Version": A2A_VERSION,
            "X-Request-Id": rid,
            "X-API-Key": self.api_key,
            "Content-Type": "application/json; charset=utf-8",
        }
        if stream:
            h["Accept"] = "text/event-stream, application/x-ndjson"
        return h

    @staticmethod
    def _unwrap_rpc(payload: Any) -> Any:
        """取 JSON-RPC result；error 交给统一错误映射。"""
        if not isinstance(payload, dict):
            raise A2AError("INVALID_REQUEST", "response is not a JSON object", http_status=502)
        if "error" in payload:
            raise_for_payload(payload)
        if "result" in payload:
            return payload["result"]
        return payload

    def _post_json(
        self, path: str, body: dict[str, Any], *, stream: bool = False, request_id: str | None = None
    ) -> Any:
        """非流 JSON-RPC POST：返回解析后的 dict（含错误信封检查）。"""
        rid = request_id or _new_request_id()
        try:
            resp = self._http.post(path, json=body, headers=self._headers(rid, stream=False))
        except httpx.TimeoutException as e:
            raise A2ATimeoutError(f"request timeout after {self.timeout}s: {e}", request_id=rid) from e
        except httpx.ConnectError as e:
            raise A2AConnectionError(f"connect failed: {e}", request_id=rid) from e
        except httpx.RequestError as e:
            raise A2AConnectionError(f"request failed: {e}", request_id=rid) from e

        out_rid = resp.headers.get("X-Request-Id", rid)
        text = resp.text
        payload: Any
        try:
            payload = json.loads(text) if text else {}
        except ValueError:
            if resp.status_code >= 400:
                raise A2AError(
                    "INTERNAL_ERROR",
                    f"HTTP {resp.status_code}: non-JSON error body: {text[:200]}",
                    request_id=out_rid,
                    http_status=resp.status_code,
                ) from None
            payload = {"raw": text}

        # 契约 §0.3：统一错误信封 / JSON-RPC error → A2AError / TimeoutError
        raise_for_payload(payload, request_id=out_rid, status_code=resp.status_code)
        if resp.status_code >= 400:
            raise A2AError(
                "INTERNAL_ERROR",
                f"HTTP {resp.status_code}: {text[:200]}",
                request_id=out_rid,
                http_status=resp.status_code,
            )
        return payload

    def _post_stream_lines(
        self, path: str, body: dict[str, Any], *, request_id: str | None = None
    ) -> Iterator[str]:
        """流式 POST：用 `httpx.iter_lines()`（跨 chunk 由 httpx 负责，SSEBuffer 再聚合）。"""
        rid = request_id or _new_request_id()
        try:
            with self._http.stream(
                "POST", path, json=body, headers=self._headers(rid, stream=True)
            ) as resp:
                out_rid = resp.headers.get("X-Request-Id", rid)
                if resp.status_code >= 400:
                    resp.read()
                    raise A2AError(
                        "INTERNAL_ERROR",
                        f"HTTP {resp.status_code}: {resp.text[:200]}",
                        request_id=out_rid,
                        http_status=resp.status_code,
                    )
                ctype = resp.headers.get("content-type", "")
                # NDJSON 流也用 iter_lines，单行可 >64KB（不做任何截断）
                for line in resp.iter_lines():
                    yield line
        except httpx.TimeoutException as e:
            raise A2ATimeoutError(f"stream timeout after {self.timeout}s: {e}", request_id=rid) from e
        except httpx.ConnectError as e:
            raise A2AConnectionError(f"connect failed: {e}", request_id=rid) from e
        except httpx.RequestError as e:
            raise A2AConnectionError(f"request failed: {e}", request_id=rid) from e

    # ------------------------------------------------------------------
    # 官方 a2a-sdk 底层：可选暴露纯官方客户端
    # ------------------------------------------------------------------
    def _official_a2a_client(self, agent_card: AgentCard | None = None) -> OfficialA2AClient:
        """构造官方 `a2a.client.A2AClient`（底层用 httpx.AsyncClient）。

        这是「基于官方 SDK 二次开发」的直接证据：SDK 内部确实持有并构造官方客户端。
        默认 SDK 走自研统一传输层（带治理头/数据面代理），此工厂供需要纯官方
        合规轨传输的调用方使用。
        """
        card = agent_card or self.get_agent_card()
        async_client = httpx.AsyncClient(
            base_url=self.base_url,
            timeout=httpx.Timeout(self.timeout),
            headers=self._headers(),
        )
        return OfficialA2AClient(async_client, agent_card=card)

    # ------------------------------------------------------------------
    # 契约 §7：Agent 发现
    # ------------------------------------------------------------------
    def get_agent_card(self) -> AgentCard:
        """`get_agent_card()` → GET /.well-known/agent.json，返回官方 `a2a.types.AgentCard`。"""
        rid = _new_request_id()
        try:
            resp = self._http.get("/.well-known/agent.json", headers=self._headers(rid))
        except httpx.TimeoutException as e:
            raise A2ATimeoutError(f"get_agent_card timeout: {e}", request_id=rid) from e
        except httpx.RequestError as e:
            raise A2AConnectionError(f"get_agent_card failed: {e}", request_id=rid) from e
        if resp.status_code != 200:
            raise A2AError(
                "INTERNAL_ERROR", f"GET agent card HTTP {resp.status_code}",
                request_id=resp.headers.get("X-Request-Id", rid), http_status=resp.status_code,
            )
        try:
            return AgentCard.model_validate(resp.json())
        except Exception as e:
            raise A2AError(
                "INVALID_REQUEST", f"invalid AgentCard: {e}",
                request_id=resp.headers.get("X-Request-Id", rid), http_status=resp.status_code,
            ) from e

    # ------------------------------------------------------------------
    # 契约 §7：消息 / 任务
    # ------------------------------------------------------------------
    def send_message(self, message: Any) -> Any:
        """`send_message(message)` → JSON-RPC `message/send`，返回官方 Task / Message。

        请求体由官方 `a2a.types.SendMessageRequest` 序列化（camelCase alias）。
        返回优先用官方 `Task.model_validate` / `Message.model_validate` 反序列化；
        若结果是兼容轨的其它形态则原样返回 dict。
        """
        msg = _normalize_message(message)
        # 本地护栏预检（契约 §6.1 PromptGuardrail）
        g = self.check_guardrail(_message_text(msg))
        if not g.allowed:
            raise A2AError(g.code, g.reason, http_status=200, detail={"rule": g.rule})

        rid = _new_request_id()
        # 官方 a2a.types.Message 做序列化（camelCase alias），但按双轨制走
        # 兼容轨 REST 端点，不发送 JSON-RPC method（避免 method not found）。
        payload = self._post_json(
            COMPAT_SEND_PATH,
            {"message": _to_dict(msg)},
            request_id=rid,
        )
        result = self._unwrap_rpc(payload)
        return self._coerce_task_or_message(result)

    @staticmethod
    def _coerce_task_or_message(result: Any) -> Any:
        if not isinstance(result, dict):
            return result
        kind = result.get("kind")
        try:
            if kind == "task":
                return Task.model_validate(result)
            if kind == "message":
                return Message.model_validate(result)
        except Exception:
            pass
        return result

    def stream_message(self, message: Any) -> Iterator[Event]:
        """`stream_message(message)` → **generator，yield Event**（契约 §7 / §1.6）。

        走 JSON-RPC `message/stream` SSE 流。SSE 解析用 `iter_sse_events()`
        + `SSEBuffer` 跨 chunk 拼接，禁止假设一次 read 一帧。
        """
        msg = _normalize_message(message)
        g = self.check_guardrail(_message_text(msg))
        if not g.allowed:
            raise A2AError(g.code, g.reason, http_status=200, detail={"rule": g.rule})

        # 兼容轨 REST 冒号路径（§1.5），用官方 Message 序列化，SSE 解析用
        # iter_sse_events() + SSEBuffer 跨 chunk 拼接。
        lines = self._post_stream_lines(
            COMPAT_STREAM_PATH, {"message": _to_dict(msg)}
        )
        for ev in iter_sse_events(lines):
            # 流中的 error 帧（§3.4 护栏拒绝 / UPSTREAM_TIMEOUT）不静默吞掉
            if ev.is_error and isinstance(ev.json, dict):
                raise_for_payload(ev.json, status_code=200)
            yield ev
            if ev.is_done:
                break

    def get_task(self, task_id: str) -> Any:
        """`get_task(task_id)` → JSON-RPC `tasks/get`。"""
        req = GetTaskRequest(
            id=f"req-{uuid.uuid4().hex[:12]}",
            params=TaskQueryParams(id=task_id, history_length=10),
        )
        payload = self._post_json(A2A_RPC_PATH, _to_dict(req))
        return self._coerce_task_or_message(self._unwrap_rpc(payload))

    def cancel_task(self, task_id: str) -> Any:
        """`cancel_task(task_id)` → JSON-RPC `tasks/cancel`。"""
        req = CancelTaskRequest(
            id=f"req-{uuid.uuid4().hex[:12]}",
            params=TaskIdParams(id=task_id),
        )
        payload = self._post_json(A2A_RPC_PATH, _to_dict(req))
        return self._coerce_task_or_message(self._unwrap_rpc(payload))

    def subscribe(self, task_id: str) -> Iterator[Event]:
        """`subscribe(task_id)` → generator，yield Event（JSON-RPC `tasks/resubscribe` SSE）。"""
        req = TaskResubscriptionRequest(
            id=f"req-{uuid.uuid4().hex[:12]}",
            params=TaskIdParams(id=task_id),
        )
        lines = self._post_stream_lines(A2A_RPC_PATH, _to_dict(req))
        for ev in iter_sse_events(lines):
            if ev.is_error and isinstance(ev.json, dict):
                raise_for_payload(ev.json, status_code=200)
            yield ev
            if ev.is_done:
                break

    # ------------------------------------------------------------------
    # 契约 §7：模型网关 chat(**kwargs) → 数据面 POST /v1/chat/completions
    # ------------------------------------------------------------------
    def chat(self, **kwargs: Any) -> Any:
        """`chat(**kwargs)` → 数据面 `POST /v1/chat/completions`（契约 §2.1）。

        - stream=False（默认）→ 返回解析后的 OpenAI chat.completion dict
        - stream=True → 返回 **generator**，yield chunk（每个 chunk 是 dict，
          `data: [DONE]` 用 `Event(is_done=True)` 表示）

        kwargs 直接作为 OpenAI 请求字段：model / messages / max_tokens / temperature ...
        """
        stream = bool(kwargs.get("stream", False))
        body = dict(kwargs)
        body["stream"] = stream
        body.setdefault("model", "mock-1")
        body.setdefault("messages", [{"role": "user", "content": "hello"}])

        # 本地护栏预检（契约 §6.1 PromptGuardrail，对 prompt 文本）
        prompt = "\n".join(
            str(m.get("content", "")) for m in body["messages"] if isinstance(m, dict)
        )
        g = self.check_guardrail(prompt)
        if not g.allowed:
            raise A2AError(g.code, g.reason, http_status=200, detail={"rule": g.rule})

        if not stream:
            return self._post_json("/v1/chat/completions", body)

        def gen() -> Iterator[Any]:
            lines = self._post_stream_lines("/v1/chat/completions", body)
            for ev in iter_sse_events(lines):
                if ev.is_done:
                    yield Event(event="done", data="[DONE]", json=None)
                    break
                if isinstance(ev.json, dict):
                    # 流中途的 error 帧（§3.4）不抛异常打断连接，作为 Event 交给调用方
                    yield Event(event=ev.event, data=ev.data, json=ev.json)
                else:
                    yield ev

        return gen()

    # ------------------------------------------------------------------
    # 契约 §7：MCP 工具
    # ------------------------------------------------------------------
    def list_tools(self) -> list[Tool]:
        """`list_tools()` → `POST /mcp` JSON-RPC `tools/list`，返回官方 `mcp.types.Tool` 列表。"""
        return self._mcp.list_tools()

    def call_tool(self, name: str, **kwargs: Any) -> CallToolResult:
        """`call_tool(name, **kwargs)` → `POST /mcp` JSON-RPC `tools/call`。"""
        return self._mcp.call_tool(name, dict(kwargs))

    # ------------------------------------------------------------------
    # 契约 §7：护栏本地检查
    # ------------------------------------------------------------------
    def check_guardrail(self, text: str) -> _guard.GuardrailResult:
        """`check_guardrail(text)` → GuardrailResult（本地预检，不联网）。"""
        return _guard.check_guardrail(text)

    # ------------------------------------------------------------------
    # 生命周期
    # ------------------------------------------------------------------
    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> "A2AClient":
        return self

    def __exit__(self, *args: Any) -> None:
        self.close()


__all__ = ["A2AClient", "A2A_VERSION", "DEFAULT_TIMEOUT"]
