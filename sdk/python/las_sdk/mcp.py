# -*- coding: utf-8 -*-
"""MCP 工具能力（契约 §7 list_tools / call_tool、§5 POST /mcp JSON-RPC）。

传输：`POST /mcp` 的 JSON-RPC 2.0 `tools/list` / `tools/call`（契约 §5.1）。
协议类型：**基于官方 `mcp` 包二次开发** —— 用 `mcp.types.ListToolsResult` /
`CallToolResult` / `Tool` / `TextContent` 校验与反序列化注册中心返回，
避免自己手写协议模型（契约 §0.0「合规轨由官方 SDK 负责」）。

说明（契约差异）：官方 `mcp` 1.30.0 的 `FastMCP` / `ClientSession` 走
stdio / SSE / streamable-http 会话传输；而我们的数据面代理是单条
`POST /mcp` JSON-RPC（契约 §5.3 反向代理到 9300）。
因此这里用官方 `mcp.types` 做结果模型校验，传输层用 httpx 直发 JSON-RPC，
并在 `sdk/python/README.md` 如实记录该差异（不是完全手写协议）。
"""
from __future__ import annotations

import json
import uuid
from typing import Any

# ---- 官方 MCP SDK 使用点：mcp.types 结果模型 -------------------------------
from mcp.types import CallToolResult, ListToolsResult, Tool, TextContent  # noqa: F401
from mcp.types import JSONRPCError  # noqa: F401  (官方 JSON-RPC error 模型)

from .errors import A2AError, raise_for_payload
from .guardrail import check_tool


def _rpc_id() -> str:
    return f"mcp-{uuid.uuid4().hex[:16]}"


def _rpc_request(method: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
    body: dict[str, Any] = {"jsonrpc": "2.0", "id": _rpc_id(), "method": method}
    if params is not None:
        body["params"] = params
    return body


def _unwrap_result(payload: Any) -> Any:
    """从 JSON-RPC 响应里取 result；若是 error 则抛 A2AError。"""
    if not isinstance(payload, dict):
        raise A2AError("INVALID_REQUEST", "MCP response is not a JSON object", http_status=502)
    if "error" in payload:
        raise_for_payload(payload, status_code=502)
    return payload.get("result")


def parse_tools_list(result: Any) -> ListToolsResult:
    """用官方 `mcp.types.ListToolsResult` 反序列化 tools/list 的 result。

    若注册中心返回的是契约 §5.1 的简化结构（`nextCursor` 而非 `_meta`），
    model_validate 仍可容错；失败时降级为手工包装并保留原始 tools。
    """
    try:
        return ListToolsResult.model_validate(result)
    except Exception:
        # 兼容轨兜底：官方模型不接受时用最小字段重建，仍是官方类型实例
        tools_raw = (result or {}).get("tools", []) if isinstance(result, dict) else []
        return ListToolsResult(tools=tools_raw, nextCursor=(result or {}).get("nextCursor", ""))


def parse_call_tool_result(result: Any) -> CallToolResult:
    """用官方 `mcp.types.CallToolResult` 反序列化 tools/call 的 result。"""
    try:
        return CallToolResult.model_validate(result)
    except Exception:
        if isinstance(result, dict):
            content = result.get("content", [])
            return CallToolResult(content=content, isError=bool(result.get("isError", False)))
        return CallToolResult(content=[{"type": "text", "text": str(result)}], isError=False)


def tools_list_body() -> dict[str, Any]:
    """构造 `POST /mcp` 的 JSON-RPC tools/list 请求体。"""
    return _rpc_request("tools/list", {})


def tools_call_body(name: str, arguments: dict[str, Any]) -> dict[str, Any]:
    """构造 `POST /mcp` 的 JSON-RPC tools/call 请求体（契约 §5.1）。"""
    return _rpc_request("tools/call", {"name": name, "arguments": arguments})


class MCPClient:
    """走 `POST /mcp` 的极简 MCP 客户端（传输用 httpx，模型用官方 mcp.types）。

    不直接持有 base_url / headers，由 A2AClient 注入 `_post_json` 回调，
    保证统一走数据面、自动带 X-A2A-Version / X-Request-Id / X-API-Key。
    """

    def __init__(self, post_json: Any) -> None:
        self._post_json = post_json

    def list_tools(self) -> list[Tool]:
        """契约 §7 list_tools() → POST /mcp tools/list。返回官方 `mcp.types.Tool` 列表。"""
        payload = self._post_json("/mcp", tools_list_body(), stream=False)
        result = _unwrap_result(payload)
        parsed = parse_tools_list(result)
        return list(parsed.tools)

    def call_tool(self, name: str, arguments: dict[str, Any] | None = None) -> CallToolResult:
        """契约 §7 call_tool(name, **kwargs) → POST /mcp tools/call。

        先做本地 ToolGuardrail（blockedTools），再发请求；返回官方 CallToolResult。
        """
        args = dict(arguments or {})
        guard = check_tool(name)
        if not guard.allowed:
            raise A2AError(
                guard.code,
                guard.reason,
                http_status=200,
                detail={"tool": name, "rule": guard.rule},
            )
        payload = self._post_json("/mcp", tools_call_body(name, args), stream=False)
        result = _unwrap_result(payload)
        return parse_call_tool_result(result)

    @staticmethod
    def texts(result: CallToolResult) -> list[str]:
        """从官方 CallToolResult 里抽出所有 TextContent.text（契约 §5.1 content）。"""
        out: list[str] = []
        for c in result.content:
            if isinstance(c, TextContent):
                out.append(c.text)
            elif isinstance(c, dict) and c.get("type") == "text":
                out.append(str(c.get("text", "")))
        return out


__all__ = [
    "MCPClient",
    "tools_list_body",
    "tools_call_body",
    "parse_tools_list",
    "parse_call_tool_result",
    "ListToolsResult",
    "CallToolResult",
    "Tool",
    "TextContent",
]
