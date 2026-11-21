# -*- coding: utf-8 -*-
"""SSE / NDJSON 解析（契约 §7 硬要求）。

核心约束：**必须跨 chunk 拼接**。
一个 `data:` 帧可能被 TCP 拆成多次 read，禁止假设一次 recv 一帧；
NDJSON 单行可能 >64KB，禁止用默认 bufio.Scanner 语义（Python 侧即禁止按 64KB 截断）。

实现：
- `SSEBuffer`：字节/文本增量喂入缓冲，按 `\\n` 分行，空行触发事件；
  `data:` 多行按 SSE 规范用 `\\n` 连接，天然支持一个帧被拆成任意多次 feed。
- `iter_sse_events(lines)`：把 `httpx.Response.iter_lines()` 产出的行转 Event。
- `iter_ndjson_objects(lines)`：NDJSON 解析，单行无大小限制（>64KB 直接 json.loads）。
- `Event`：契约 §7 `stream_message` / `subscribe` 的 yield 类型。
"""
from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any, Iterable, Iterator


@dataclass
class Event:
    """流式事件（契约 §7 stream_message / subscribe yield Event）。

    :param event: SSE `event:` 名，如 status-update / artifact-update / error；
                  OpenAI chat 流帧无 event 名时为 ""
    :param data: `data:` 行的原始字符串（`[DONE]` 时保留原值）
    :param json: 若 data 是合法 JSON 则解析后的对象，否则 None
    """

    event: str = ""
    data: str = ""
    json: Any = None
    raw_lines: list[str] = field(default_factory=list)

    @property
    def is_done(self) -> bool:
        return self.data.strip() == "[DONE]"

    @property
    def is_error(self) -> bool:
        return self.event == "error" or (
            isinstance(self.json, dict) and "error" in self.json
        )

    def __repr__(self) -> str:
        return f"Event(event={self.event!r}, data={self.data[:60]!r}, json={type(self.json).__name__})"


def _decode_line(line: Any) -> str:
    if isinstance(line, bytes):
        return line.decode("utf-8", errors="replace")
    return str(line)


def _parse_data(data_lines: list[str], event_name: str, raw_lines: list[str]) -> Event:
    # SSE 规范：多行 data 用 \n 连接成一个字符串
    data = "\n".join(data_lines)
    parsed: Any = None
    if data and data.strip() != "[DONE]":
        try:
            parsed = json.loads(data)
        except (ValueError, TypeError):
            parsed = None
    return Event(event=event_name, data=data, json=parsed, raw_lines=list(raw_lines))


class SSEBuffer:
    """增量 SSE 缓冲：跨任意多次 feed 拼接一个帧。

    用法：
        buf = SSEBuffer()
        for chunk in tcp_chunks:            # chunk 可以是任意切分
            for ev in buf.feed_bytes(chunk):
                ...                          # 只有遇到空行才 yield 完整事件
        for ev in buf.flush():              # 流结束，处理残留（半帧）
            ...
    """

    def __init__(self) -> None:
        self._buf = ""           # 未完成的行（跨 chunk 拼接）
        self._data_lines: list[str] = []
        self._event_name = ""
        self._raw_lines: list[str] = []

    def feed_bytes(self, chunk: bytes | str) -> list[Event]:
        """喂入一块 bytes / str，返回本次能凑齐的完整事件列表。"""
        text = _decode_line(chunk)
        return self._feed_text(text)

    def _feed_text(self, text: str) -> list[Event]:
        self._buf += text
        events: list[Event] = []
        # 统一处理 \r\n / \n / 裸 \r（SSE 行分隔符）
        normalized = self._buf.replace("\r\n", "\n").replace("\r", "\n")
        parts = normalized.split("\n")
        # 最后一段是不完整的行（可能为空，表示刚好以换行结束），留到下一次
        self._buf = parts[-1]
        for line in parts[:-1]:
            ev = self._consume_line(line)
            if ev is not None:
                events.append(ev)
        return events

    def _consume_line(self, line: str) -> Event | None:
        if line == "":
            # 空行 = 事件结束
            if not self._data_lines and not self._event_name and not self._raw_lines:
                return None
            ev = _parse_data(self._data_lines, self._event_name, self._raw_lines)
            self._data_lines = []
            self._event_name = ""
            self._raw_lines = []
            return ev
        self._raw_lines.append(line)
        if line.startswith(":"):
            return None  # 注释 / keep-alive
        if ":" in line:
            field_name, value = line.split(":", 1)
            if value.startswith(" "):
                value = value[1:]
        else:
            field_name, value = line, ""
        if field_name == "data":
            self._data_lines.append(value)
        elif field_name == "event":
            self._event_name = value
        # id / retry / 其它字段忽略
        return None

    def flush(self) -> list[Event]:
        """流结束时调用：把残留行当最后一行处理，并产出未闭合的最后一个事件。"""
        events: list[Event] = []
        if self._buf:
            ev = self._consume_line(self._buf)
            self._buf = ""
            if ev is not None:
                events.append(ev)
        if self._data_lines or self._event_name:
            events.append(_parse_data(self._data_lines, self._event_name, self._raw_lines))
            self._data_lines = []
            self._event_name = ""
            self._raw_lines = []
        return events


def iter_sse_events(lines: Iterable[Any]) -> Iterator[Event]:
    """把 `httpx.Response.iter_lines()` 的行序列转成 Event 序列。

    每条 line 视为「已经是一行」；仍通过 SSEBuffer 的空行分帧逻辑聚合，
    因此天然支持 data 被拆成多行，也支持跨 chunk（iter_lines 已负责按行切分，
    本函数不假设一次调用给到完整帧）。
    """
    buf = SSEBuffer()
    for line in lines:
        # iter_lines 给的是不带换行的一行；补一个 \n 交给缓冲的分帧逻辑
        text = _decode_line(line)
        for ev in buf.feed_bytes(text + "\n"):
            yield ev
    for ev in buf.flush():
        yield ev


def iter_ndjson_objects(lines: Iterable[Any]) -> Iterator[Any]:
    """NDJSON 解析：一行一个 JSON 对象，单行可 >64KB（契约 §1.7 / §7）。

    Python 的 str 没有 64KB 上限；这里不做任何按长度截断，直接 json.loads。
    空行跳过；非法行抛 ValueError（由调用方决定是否容错）。
    """
    for line in lines:
        s = _decode_line(line)
        if not s.strip():
            continue
        yield json.loads(s)


def error_from_payload(payload: Any) -> dict[str, Any] | None:
    """提取契约 §0.3 错误信封 / JSON-RPC error，返回标准化 dict。

    返回：{"code","message","request_id","http_status","detail"} 或 None。
    供 errors.py 的 raise_for_payload 使用（避免循环 import）。
    """
    if not isinstance(payload, dict):
        return None
    err = payload.get("error")
    if not isinstance(err, dict):
        return None

    # 形态 1：统一错误信封 {"error": {"code": "AUTH_REQUIRED", ...}}
    if isinstance(err.get("code"), str):
        return {
            "code": err["code"],
            "message": str(err.get("message", "")),
            "request_id": str(err.get("requestId") or err.get("request_id") or ""),
            "http_status": err.get("httpStatus") or err.get("http_status"),
            "detail": err.get("detail") if isinstance(err.get("detail"), dict) else {},
        }

    # 形态 2：JSON-RPC {"error": {"code": -32601, "message": "...", "data": {"code": "..."}}}
    data = err.get("data")
    if isinstance(data, dict) and isinstance(data.get("code"), str):
        return {
            "code": data["code"],
            "message": str(err.get("message", "")),
            "request_id": str(payload.get("id") or ""),
            "http_status": data.get("httpStatus"),
            "detail": data,
        }
    # JSON-RPC 数值错误码兜底：映射为 INVALID_REQUEST
    return {
        "code": "INVALID_REQUEST",
        "message": str(err.get("message", "json-rpc error")),
        "request_id": str(payload.get("id") or ""),
        "http_status": None,
        "detail": data if isinstance(data, dict) else {},
    }


__all__ = [
    "Event",
    "SSEBuffer",
    "iter_sse_events",
    "iter_ndjson_objects",
    "error_from_payload",
]
