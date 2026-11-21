# -*- coding: utf-8 -*-
"""/tools/echo —— 真实回显 HTTP 请求（method/path/query/body 原样放进 JSON）。

供 MCP 注册中心的 `tools/call` 回放验证（契约 §5.2 / E19）：
注册中心把 source.path/method 构造成真实 HTTP 请求打到 baseUrl（指向本服务），
本端点把收到的请求原样回显，注册中心再把本响应作为 `content[0].text` 返回。

返回：{"echo": {"method":..., "path":..., "query":..., "headers":..., "body":...}, "ok": true}
"""
from __future__ import annotations

import json
from typing import Any
from urllib.parse import parse_qs, urlsplit


def parse_query(raw_query_string: str) -> dict[str, Any]:
    """把 query string 解析成 dict；保留重复键为 list，单值降为标量。"""
    parsed = parse_qs(raw_query_string, keep_blank_values=True)
    out: dict[str, Any] = {}
    for k, vs in parsed.items():
        out[k] = vs if len(vs) > 1 else vs[0]
    return out


def decode_body(raw: bytes, content_type: str = "") -> Any:
    """body 原样放进 JSON：能 JSON 解析就放对象，否则放 UTF-8 文本。"""
    if not raw:
        return ""
    text = raw.decode("utf-8", errors="replace")
    if "json" in (content_type or "").lower() or text.lstrip().startswith(("{", "[")):
        try:
            return json.loads(text)
        except (ValueError, TypeError):
            return text
    return text


def build_echo(
    *,
    method: str,
    path: str,
    query_string: str,
    headers: dict[str, str],
    body: Any,
) -> dict[str, Any]:
    """组装 §5.2 / E19 需要的回显结构。"""
    return {
        "echo": {
            "method": method,
            "path": path,
            "query": parse_query(query_string),
            "headers": headers,
            "body": body,
        },
        "ok": True,
    }


def echo_from_asgi_scope(scope: dict[str, Any], raw_body: bytes) -> dict[str, Any]:
    """从 ASGI scope 构造回显（供 Starlette/FastAPI Request 使用）。"""
    path = scope.get("path", "/")
    raw_qs = scope.get("query_string", b"").decode("latin-1")
    header_map: dict[str, str] = {}
    for k, v in scope.get("headers", []) or []:
        key = k.decode("latin-1").lower()
        val = v.decode("latin-1")
        # 同名 header 合并，保持可 JSON 序列化
        header_map[key] = f"{header_map[key]},{val}" if key in header_map else val
    content_type = header_map.get("content-type", "")
    return build_echo(
        method=scope.get("method", "GET"),
        path=path,
        query_string=raw_qs,
        headers=header_map,
        body=decode_body(raw_body, content_type),
    )


def split_path_and_query(target: str) -> tuple[str, str]:
    """把 '/tools/echo?a=1&b=2' 拆成 ('/tools/echo', 'a=1&b=2')。"""
    u = urlsplit(target)
    return u.path, u.query
