# -*- coding: utf-8 -*-
"""Python SDK 可运行示例（契约 §7：example 证明能跑通全链路）。

链路：全部打数据面 **8080**（治理层 + 透明反向代理 → 下游 Sidecar / Mock LLM / MCP 注册中心）。

    get_agent_card → send_message → stream_message → chat → chat(stream)
    → list_tools → call_tool

运行（项目根 local-agent-stack/，需先起数据面与控制面/Sidecar/Mock LLM/MCP 注册中心）：
    $env:PYTHONIOENCODING = 'utf-8'
    .venv\\Scripts\\python.exe sdk/python/example/main.py

可用环境变量覆盖：
    LAS_BASE_URL  默认 http://127.0.0.1:8080
    LAS_API_KEY   默认 demo-key
    LAS_TIMEOUT   默认 30

说明：本示例对每一步做「有服务就验证、无服务就明确报告不可达」，
不会伪造成功；任何一步失败都会打印带 requestId 的真实错误，退出码非 0。
"""
from __future__ import annotations

import os
import sys
from pathlib import Path

# 允许 `python sdk/python/example/main.py` 直接运行（把 sdk/python 加入 sys.path）
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from las_sdk import (  # noqa: E402
    A2AClient,
    A2AError,
    A2ATimeoutError,
    Event,
    check_guardrail,
)

BASE_URL = os.environ.get("LAS_BASE_URL", "http://127.0.0.1:8080")
API_KEY = os.environ.get("LAS_API_KEY", "demo-key")
TIMEOUT = float(os.environ.get("LAS_TIMEOUT", "30"))


def _banner(title: str) -> None:
    print("\n" + "=" * 72)
    print(title)
    print("=" * 72)


def _preview(obj, limit: int = 320) -> str:
    try:
        import json as _json
        if hasattr(obj, "model_dump_json"):
            s = obj.model_dump_json(by_alias=True, exclude_none=True)
        else:
            s = _json.dumps(obj, ensure_ascii=False, default=str)
    except Exception:
        s = str(obj)
    return s[:limit] + ("..." if len(s) > limit else "")


def main() -> int:
    print(f"[example] base_url={BASE_URL} api_key={API_KEY} timeout={TIMEOUT}")
    print(f"[example] 全部请求打数据面 {BASE_URL}，自动带 X-A2A-Version / X-Request-Id / X-API-Key")

    client = A2AClient(BASE_URL, api_key=API_KEY, timeout=TIMEOUT)
    failures = 0

    def step(name: str, fn, *a, **kw):
        nonlocal failures
        _banner(f"STEP {name}")
        try:
            out = fn(*a, **kw)
            print(f"[OK] {name} -> {_preview(out)}")
            return out, True
        except A2ATimeoutError as e:
            print(f"[TIMEOUT] {name}: {e} (原生 TimeoutError 子类: {isinstance(e, TimeoutError)})")
            failures += 1
        except A2AError as e:
            print(f"[A2AError] {name}: {e}")
            failures += 1
        except Exception as e:  # noqa: BLE001
            print(f"[ERROR] {name}: {type(e).__name__}: {e}")
            failures += 1
        return None, False

    # 1) 发现 Agent
    card, ok = step("1 get_agent_card()", client.get_agent_card)
    if ok and card is not None:
        print("    AgentCard.name =", getattr(card, "name", "?"),
              "| url =", getattr(card, "url", "?"),
              "| type =", type(card).__name__, "(官方 a2a.types.AgentCard)")

    # 2) 发消息
    msg = {"role": "user", "parts": [{"kind": "text", "text": "hello from python sdk"}]}
    task, _ = step("2 send_message(message)", client.send_message, msg)
    task_id = None
    if task is not None:
        task_id = getattr(task, "id", None) or (task.get("id") if isinstance(task, dict) else None)
        if task_id:
            print("    taskId =", task_id)

    # 3) 流式发消息（generator yield Event）
    _banner("STEP 3 stream_message(message) — generator yield Event")
    try:
        events = list(client.stream_message("stream hello"))
        print(f"[OK] stream_message -> {len(events)} events")
        for i, ev in enumerate(events[:8]):
            print(f"    [{i}] {ev!r}")
        if len(events) > 8:
            print(f"    ... 共 {len(events)} 帧（省略 {len(events) - 8}）")
        if not any(isinstance(e, Event) for e in events):
            print("    [WARN] 没有收到 Event 类型帧")
    except (A2AError, Exception) as e:
        print(f"[ERROR] stream_message: {type(e).__name__}: {e}")
        failures += 1

    # 4) 模型网关非流
    chat_resp, _ = step(
        "4 chat(model='mock-1', messages=[...], stream=False)",
        client.chat,
        model="mock-1",
        messages=[{"role": "user", "content": "你好 mock-llm"}],
        max_tokens=128,
        temperature=0.2,
    )
    if isinstance(chat_resp, dict):
        usage = chat_resp.get("usage", {})
        print("    usage.total_tokens =", usage.get("total_tokens"),
              "| 是整数:", isinstance(usage.get("total_tokens"), int))

    # 5) 模型网关流式（generator yield chunk）
    _banner("STEP 5 chat(..., stream=True) — generator yield chunk")
    try:
        chunks = []
        for ch in client.chat(
            model="mock-1",
            messages=[{"role": "user", "content": "流式中文测试"}],
            stream=True,
        ):
            chunks.append(ch)
        print(f"[OK] chat(stream=True) -> {len(chunks)} chunks")
        for i, ch in enumerate(chunks[:5]):
            print(f"    [{i}] {_preview(ch, 200)}")
    except (A2AError, Exception) as e:
        print(f"[ERROR] chat(stream): {type(e).__name__}: {e}")
        failures += 1

    # 6) MCP 列工具
    tools, _ = step("6 list_tools() → POST /mcp tools/list", client.list_tools)
    if tools:
        names = [getattr(t, "name", None) or (t.get("name") if isinstance(t, dict) else "?") for t in tools]
        print("    tool names =", names[:10], f"(共 {len(tools)})")
        print("    tool type =", type(tools[0]).__name__, "(官方 mcp.types.Tool)")

    # 7) MCP 调工具
    tool_name = "echo"
    if tools:
        first = tools[0]
        tool_name = getattr(first, "name", None) or (first.get("name") if isinstance(first, dict) else tool_name) or tool_name
    res, _ = step(
        f"7 call_tool({tool_name!r}, **kwargs) → POST /mcp tools/call",
        client.call_tool,
        tool_name,
        text="hello from python sdk",
        verbose=True,
    )
    if res is not None:
        from las_sdk import MCPClient
        print("    content texts =", MCPClient.texts(res)[:2])

    # 8) 护栏本地检查（不需要服务）
    _banner("STEP 8 check_guardrail(text) — 本地检查")
    for text in ["normal text", "BLOCK_ME please", "x" * 9000]:
        g = check_guardrail(text)
        print(f"    len={len(text):5d} allowed={g.allowed} rule={g.rule} code={g.code or '-'}")

    client.close()

    _banner("RESULT")
    if failures:
        print(f"[example] {failures} 个步骤未能完成（多为数据面/下游未启动，属真实不可达，非伪造成功）。")
        print("[example] 请确认 8080 数据面 + 控制面 9000 + Sidecar 9100 + Mock LLM 9200 + MCP 9300 已启动。")
        return 1
    print("[example] 全链路 7 步全部打通：get_agent_card → send_message → stream_message → chat → chat(stream) → list_tools → call_tool")
    return 0


if __name__ == "__main__":
    os.environ.setdefault("PYTHONIOENCODING", "utf-8")
    sys.exit(main())
