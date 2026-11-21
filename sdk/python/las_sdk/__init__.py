# -*- coding: utf-8 -*-
"""las_sdk —— local-agent-stack Python SDK（契约 §7）。

基于官方 `a2a-sdk` / `mcp` 二次开发：
- 协议模型/序列化用 `a2a.types`，传输层用 httpx（见 client.py 顶部说明）；
- MCP 结果模型用 `mcp.types`（见 mcp.py）。

统一导出：
    from las_sdk import A2AClient, A2AError, A2ATimeoutError, Event, check_guardrail
"""
from __future__ import annotations

from .client import A2AClient, A2A_VERSION, DEFAULT_TIMEOUT
from .errors import (
    A2AConnectionError,
    A2AError,
    A2ATimeoutError,
    extract_unified_error,
    raise_for_payload,
)
from .guardrail import (
    GuardrailConfig,
    GuardrailResult,
    check_guardrail,
    check_tool,
    configure,
)
from .mcp import CallToolResult, ListToolsResult, MCPClient, TextContent, Tool
from .sse import Event, SSEBuffer, iter_ndjson_objects, iter_sse_events

__version__ = "1.0.0"

__all__ = [
    # 客户端
    "A2AClient",
    "A2A_VERSION",
    "DEFAULT_TIMEOUT",
    # 错误
    "A2AError",
    "A2ATimeoutError",
    "A2AConnectionError",
    "raise_for_payload",
    "extract_unified_error",
    # 流
    "Event",
    "SSEBuffer",
    "iter_sse_events",
    "iter_ndjson_objects",
    # 护栏
    "check_guardrail",
    "check_tool",
    "configure",
    "GuardrailConfig",
    "GuardrailResult",
    # MCP
    "MCPClient",
    "ListToolsResult",
    "CallToolResult",
    "Tool",
    "TextContent",
    "__version__",
]
