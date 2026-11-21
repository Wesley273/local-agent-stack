# -*- coding: utf-8 -*-
"""OpenAI 兼容响应 / SSE 流构造函数（契约 §2.1 / §2.2 / §2.3）。

本模块只负责「按契约拼 JSON 与拼 SSE 帧」，不含 HTTP 路由；
由 services/mock-llm/main.py 的 FastAPI handler 调用。

设计要点：
- usage.prompt_tokens / completion_tokens / total_tokens 全部是 int（契约 §2.1 硬要求）；
- SSE 每帧是 `data: <单行JSON>\\n\\n`，中文用 ensure_ascii=False 保证 UTF-8 不乱码；
- embedding 是纯 Python float 数组，超长输入只做 sha256，不复制/不截断。
"""
from __future__ import annotations

import hashlib
import json
import time
import uuid
from typing import Any, Iterable, Sequence

MOCK_CHAT_MODEL = "mock-1"
MOCK_EMBED_MODEL = "bge-m3"
EMBEDDING_DIM = 8

# 回显文本前缀带中文，用来验证 Content-Type 的 charset=utf-8 与 UTF-8 body 不乱码
ECHO_PREFIX = "[mock-llm 回显] "


def epoch_seconds() -> int:
    """OpenAI 的 created 字段：整数秒。"""
    return int(time.time())


def new_id(prefix: str = "chatcmpl") -> str:
    return f"{prefix}-{uuid.uuid4().hex[:24]}"


def count_tokens(text: str) -> int:
    """粗粒度 token 估算，返回值保证是 int（契约要求 usage 全为整数）。

    用长度 / 4 的启发式，避免引入 tiktoken 之类额外依赖；
    对超长输入（124844 字节）也是 O(1) 计算，不会拖慢 embeddings。
    """
    if not text:
        return 0
    return max(1, (len(text) + 3) // 4)


def _content_to_text(content: Any) -> str:
    """把 OpenAI message.content（str 或多模态 list）取成纯文本。"""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        chunks: list[str] = []
        for part in content:
            if isinstance(part, dict):
                t = part.get("text")
                if isinstance(t, str):
                    chunks.append(t)
        return "".join(chunks)
    if content is None:
        return ""
    return str(content)


def last_user_text(messages: Sequence[Any]) -> str:
    """取最后一条 user message 的文本；没有 user 就退化到最后一条任意消息。"""
    last_any = ""
    for m in reversed(list(messages or [])):
        if not isinstance(m, dict):
            continue
        text = _content_to_text(m.get("content"))
        if text and not last_any:
            last_any = text
        if m.get("role") == "user" and text:
            return text
    return last_any


def echo_text(messages: Sequence[Any], max_tokens: Any = None) -> str:
    """生成回显式文本：把用户最后一条 message 拼回去（含中文以验证 UTF-8）。"""
    last = last_user_text(messages)
    text = f"{ECHO_PREFIX}{last}" if last else f"{ECHO_PREFIX}(empty)"
    # max_tokens 只做粗粒度截断，保证流式帧数量可控
    if isinstance(max_tokens, int) and max_tokens > 0:
        limit = max_tokens * 4
        if len(text) > limit:
            text = text[:limit]
    return text


def _prompt_text(messages: Sequence[Any]) -> str:
    parts: list[str] = []
    for m in messages or []:
        if isinstance(m, dict):
            parts.append(_content_to_text(m.get("content")))
    return "\n".join(parts)


def build_chat_completion(
    model: str,
    messages: Sequence[Any],
    max_tokens: Any = None,
    temperature: Any = None,
) -> dict[str, Any]:
    """§2.1 stream:false → 标准 OpenAI chat.completion 对象。"""
    content = echo_text(messages, max_tokens)
    prompt_tokens = count_tokens(_prompt_text(messages))
    completion_tokens = count_tokens(content)
    return {
        "id": new_id(),
        "object": "chat.completion",
        "created": epoch_seconds(),
        "model": model or MOCK_CHAT_MODEL,
        "choices": [
            {
                "index": 0,
                "message": {"role": "assistant", "content": content},
                "finish_reason": "stop",
            }
        ],
        "usage": {
            "prompt_tokens": int(prompt_tokens),
            "completion_tokens": int(completion_tokens),
            "total_tokens": int(prompt_tokens + completion_tokens),
        },
    }


def chat_chunk(
    chunk_id: str,
    created: int,
    model: str,
    delta: dict[str, Any],
    finish_reason: Any = None,
) -> dict[str, Any]:
    """单个 chat.completion.chunk。"""
    return {
        "id": chunk_id,
        "object": "chat.completion.chunk",
        "created": created,
        "model": model,
        "choices": [
            {"index": 0, "delta": delta, "finish_reason": finish_reason}
        ],
    }


def sse_data(payload: Any) -> str:
    """把 dict 或字符串编成一条 SSE `data:` 帧（严格单行 JSON + 空行结束）。"""
    if isinstance(payload, str):
        line = payload
    else:
        line = json.dumps(payload, ensure_ascii=False, separators=(",", ":"))
    # 契约：data 行内 JSON 必须是单行（不含裸换行）
    line = line.replace("\r", "\\r").replace("\n", "\\n")
    return f"data: {line}\n\n"


def split_content(text: str, max_frames: int = 32) -> list[str]:
    """把回显文本切成若干片段，用于「若干 delta:{"content":...} 帧」。"""
    if not text:
        return [""]
    n = max(1, min(len(text), max_frames))
    size = max(1, (len(text) + n - 1) // n)
    return [text[i : i + size] for i in range(0, len(text), size)]


def iter_chat_sse(
    model: str,
    messages: Sequence[Any],
    max_tokens: Any = None,
) -> Iterable[str]:
    """§2.1 stream:true → 生成 SSE 帧字符串序列（不设 Content-Length）。

    顺序严格：首帧 delta:{"role":"assistant"} → 若干 delta:{"content":"..."} → data: [DONE]
    """
    chunk_id = new_id()
    created = epoch_seconds()
    mdl = model or MOCK_CHAT_MODEL

    yield sse_data(chat_chunk(chunk_id, created, mdl, {"role": "assistant"}))
    for piece in split_content(echo_text(messages, max_tokens)):
        yield sse_data(chat_chunk(chunk_id, created, mdl, {"content": piece}))
    yield "data: [DONE]\n\n"


def _embedding_vector(text: str, dim: int = EMBEDDING_DIM) -> list[float]:
    """确定性的伪向量：sha256 → dim 个 [-1, 1] 之间的 Python float。"""
    digest = hashlib.sha256(text.encode("utf-8", errors="replace")).digest()
    vec: list[float] = []
    for i in range(dim):
        b = digest[i % len(digest)]
        val = (b / 255.0) * 2.0 - 1.0
        vec.append(float(round(val, 6)))
    return vec


def build_embeddings_response(
    model: str,
    inputs: Any,
    encoding_format: str = "float",
) -> dict[str, Any]:
    """§2.2 → object:"list"，data 里每个 embedding 是 float 数组，usage 全整数。

    inputs 允许 str 或 list[str]；超长输入（>8000 字符 / 124844 字节）原样接收，
    只做一次 sha256，不复制、不截断、不报错（E15 场景）。
    """
    if isinstance(inputs, str):
        items: list[str] = [inputs]
    elif isinstance(inputs, list):
        items = [x if isinstance(x, str) else str(x) for x in inputs]
    else:
        items = [str(inputs)]

    data = [
        {
            "object": "embedding",
            "index": i,
            "embedding": _embedding_vector(text),
        }
        for i, text in enumerate(items)
    ]
    prompt_tokens = sum(count_tokens(t) for t in items)
    return {
        "object": "list",
        "data": data,
        "model": model or MOCK_EMBED_MODEL,
        "usage": {
            "prompt_tokens": int(prompt_tokens),
            "total_tokens": int(prompt_tokens),
        },
    }


def build_models_response(backend: str) -> dict[str, Any]:
    """§2.3 GET /v1/models → mock-1 与 bge-m3。"""
    return {
        "object": "list",
        "data": [
            {
                "id": MOCK_CHAT_MODEL,
                "object": "model",
                "owned_by": "local",
                "priority": 10,
                "backend": backend,
                "enabled": True,
            },
            {
                "id": MOCK_EMBED_MODEL,
                "object": "model",
                "owned_by": "local",
                "priority": 5,
                "backend": backend,
                "enabled": True,
            },
        ],
    }
