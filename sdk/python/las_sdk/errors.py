# -*- coding: utf-8 -*-
"""统一错误类型（契约 §7 / §0.3）。

- A2AError(code, message, request_id)：映射契约 §0.3 的错误信封 / JSON-RPC error.data.code
- 超时映射为原生 TimeoutError 的子类（契约 §7：「UPSTREAM_TIMEOUT 映射为语言原生 timeout 异常」）
"""
from __future__ import annotations

from typing import Any

from .sse import error_from_payload


class A2AError(Exception):
    """A2A / 数据面统一错误。

    :param code: 契约 §0.3 错误码，如 AUTH_REQUIRED / UPSTREAM_TIMEOUT / INVALID_REQUEST
    :param message: 人类可读信息
    :param request_id: 全链路 X-Request-Id（用于排查）
    :param http_status: 对应 HTTP 状态码（若有）
    :param detail: 原始 detail 对象
    """

    def __init__(
        self,
        code: str,
        message: str,
        request_id: str = "",
        http_status: int | None = None,
        detail: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = str(code)
        self.message = str(message)
        self.request_id = request_id or ""
        self.http_status = http_status
        self.detail = detail or {}

    def __str__(self) -> str:
        rid = f" requestId={self.request_id}" if self.request_id else ""
        st = f" httpStatus={self.http_status}" if self.http_status is not None else ""
        return f"A2AError({self.code}): {self.message}{rid}{st}"

    def __repr__(self) -> str:
        return (
            f"A2AError(code={self.code!r}, message={self.message!r}, "
            f"request_id={self.request_id!r}, http_status={self.http_status!r})"
        )


class A2ATimeoutError(A2AError, TimeoutError):
    """超时异常：既是 A2AError（带 code/request_id），又是 Python 原生 TimeoutError 子类。

    - 客户端本地超时 / connect timeout → code=UPSTREAM_TIMEOUT（§0.3 表 504）
    - 上游返回 UPSTREAM_TIMEOUT 错误信封 → 同样抛本类
    用 `except TimeoutError` 能捕获，满足契约「映射为原生 timeout 异常」。
    """

    def __init__(self, message: str = "upstream timeout", request_id: str = "", **kw: Any) -> None:
        A2AError.__init__(
            self,
            code="UPSTREAM_TIMEOUT",
            message=message,
            request_id=request_id,
            http_status=504,
            detail=kw.pop("detail", None),
        )
        TimeoutError.__init__(self, message)


class A2AConnectionError(A2AError, ConnectionError):
    """下游连接失败（UPSTREAM_ERROR / 502），同时是原生 ConnectionError 子类。"""

    def __init__(self, message: str = "upstream connection failed", request_id: str = "", **kw: Any) -> None:
        A2AError.__init__(
            self,
            code=kw.pop("code", "UPSTREAM_ERROR"),
            message=message,
            request_id=request_id,
            http_status=kw.pop("http_status", 502),
            detail=kw.pop("detail", None),
        )
        ConnectionError.__init__(self, message)


# 需要映射为原生 TimeoutError 的错误码（契约 §0.3）
TIMEOUT_CODES = frozenset({"UPSTREAM_TIMEOUT"})
CONNECTION_CODES = frozenset({"UPSTREAM_ERROR", "STREAM_CLOSED"})


def extract_unified_error(payload: Any) -> dict[str, Any] | None:
    """从响应体里提取契约 §0.3 统一错误信封。

    支持两种形态：
    1. `{"error": {"code":..., "message":..., "requestId":..., "httpStatus":..., "detail":{}}}`
    2. JSON-RPC `{"error": {"code": -32601, "message":..., "data": {"code":"INVALID_REQUEST"}}}`
    """
    return error_from_payload(payload)


def raise_for_payload(payload: Any, request_id: str = "", status_code: int = 200) -> None:
    """若 payload 是错误信封 / JSON-RPC error，则抛对应异常；否则正常返回。

    :raises A2ATimeoutError: code 为 UPSTREAM_TIMEOUT
    :raises A2AConnectionError: code 为 UPSTREAM_ERROR / STREAM_CLOSED
    :raises A2AError: 其余契约错误码
    """
    info = extract_unified_error(payload)
    if not info:
        return
    code = info["code"]
    message = info["message"]
    rid = info.get("request_id") or request_id
    http_status = info.get("http_status")
    if http_status is None:
        http_status = status_code
    detail = info.get("detail")

    if code in TIMEOUT_CODES:
        raise A2ATimeoutError(message, request_id=rid, detail=detail)
    if code in CONNECTION_CODES:
        raise A2AConnectionError(message, request_id=rid, detail=detail, code=code, http_status=http_status)
    raise A2AError(code, message, request_id=rid, http_status=http_status, detail=detail)


__all__ = [
    "A2AError",
    "A2ATimeoutError",
    "A2AConnectionError",
    "TIMEOUT_CODES",
    "CONNECTION_CODES",
    "extract_unified_error",
    "raise_for_payload",
]
