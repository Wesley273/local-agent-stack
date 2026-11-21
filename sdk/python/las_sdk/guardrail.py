# -*- coding: utf-8 -*-
"""客户端本地护栏检查（契约 §7 CheckGuardrail / §4 guardrails 快照）。

与数据面 / Sidecar 的服务端护栏互补：SDK 侧先做一次本地预检，
避免明显被拦截的请求浪费一次网络往返。规则与 `docs/01-协议契约.md` §4
的控制面 `guardrails` 快照默认一致（可用 configure() 覆盖）。
"""
from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Any, Iterable

DEFAULT_BLOCKED_WORDS: tuple[str, ...] = ("BLOCK_ME", "secret-leak")
DEFAULT_BLOCKED_TOOLS: tuple[str, ...] = ("dangerous_delete", "drop_database")
DEFAULT_MAX_PROMPT_CHARS = 8000
# 数据护栏：sk- 前缀密钥 / 超长 base64（契约 §6.1 DataGuardrail）
SK_PATTERN = re.compile(r"\bsk-[A-Za-z0-9_\-]{16,}\b")
LONG_B64_PATTERN = re.compile(r"[A-Za-z0-9+/]{512,}={0,2}")


@dataclass
class GuardrailConfig:
    blocked_words: list[str] = field(default_factory=lambda: list(DEFAULT_BLOCKED_WORDS))
    blocked_tools: list[str] = field(default_factory=lambda: list(DEFAULT_BLOCKED_TOOLS))
    max_prompt_chars: int = DEFAULT_MAX_PROMPT_CHARS
    enable_input: bool = True
    enable_output: bool = True


@dataclass
class GuardrailResult:
    """检查结果（契约 §7 check_guardrail(text) 的返回）。"""

    allowed: bool
    code: str = ""
    reason: str = ""
    rule: str = ""
    length: int = 0

    def __bool__(self) -> bool:
        return self.allowed

    def as_dict(self) -> dict[str, Any]:
        return {
            "allowed": self.allowed,
            "code": self.code,
            "reason": self.reason,
            "rule": self.rule,
            "length": self.length,
        }


_CONFIG = GuardrailConfig()


def configure(
    *,
    blocked_words: Iterable[str] | None = None,
    blocked_tools: Iterable[str] | None = None,
    max_prompt_chars: int | None = None,
    enable_input: bool | None = None,
    enable_output: bool | None = None,
) -> GuardrailConfig:
    """用控制面 `/v1/config` 的 guardrails 快照刷新本地规则。"""
    global _CONFIG
    if blocked_words is not None:
        _CONFIG.blocked_words = list(blocked_words)
    if blocked_tools is not None:
        _CONFIG.blocked_tools = list(blocked_tools)
    if max_prompt_chars is not None:
        _CONFIG.max_prompt_chars = int(max_prompt_chars)
    if enable_input is not None:
        _CONFIG.enable_input = bool(enable_input)
    if enable_output is not None:
        _CONFIG.enable_output = bool(enable_output)
    return _CONFIG


def get_config() -> GuardrailConfig:
    return _CONFIG


def check_guardrail(text: str, *, output: bool = False) -> GuardrailResult:
    """本地护栏检查（契约 §7 同名同语义）。

    :param text: 待检查文本
    :param output: True 表示检查输出（响应），False 表示输入（prompt）
    :return: GuardrailResult（allowed / code / reason / rule / length）

    命中时 code 为 `GUARDRAIL_REJECTED`（契约 §0.3 错误码表）。
    """
    cfg = _CONFIG
    s = text or ""
    length = len(s)

    enabled = cfg.enable_output if output else cfg.enable_input
    if not enabled:
        return GuardrailResult(allowed=True, length=length, rule="disabled")

    # 1) PromptGuardrail：命中 blockedWords
    for w in cfg.blocked_words:
        if w and w in s:
            return GuardrailResult(
                allowed=False,
                code="GUARDRAIL_REJECTED",
                reason=f"blocked word: {w}",
                rule="prompt.blocked_words",
                length=length,
            )

    # 2) PromptGuardrail：超 maxPromptChars
    if not output and cfg.max_prompt_chars > 0 and length > cfg.max_prompt_chars:
        return GuardrailResult(
            allowed=False,
            code="GUARDRAIL_REJECTED",
            reason=f"prompt too long: {length} > {cfg.max_prompt_chars}",
            rule="prompt.max_prompt_chars",
            length=length,
        )

    # 3) DataGuardrail：sk- 前缀密钥 / 超长 base64
    if SK_PATTERN.search(s) or LONG_B64_PATTERN.search(s):
        return GuardrailResult(
            allowed=False,
            code="GUARDRAIL_REJECTED",
            reason="sensitive data pattern detected",
            rule="data.sensitive_pattern",
            length=length,
        )

    return GuardrailResult(allowed=True, length=length, rule="accept")


def check_tool(name: str) -> GuardrailResult:
    """ToolGuardrail：检查工具名是否命中 blockedTools（契约 §6.1）。"""
    s = name or ""
    for t in _CONFIG.blocked_tools:
        if t and t == s:
            return GuardrailResult(
                allowed=False,
                code="GUARDRAIL_REJECTED",
                reason=f"blocked tool: {t}",
                rule="tool.blocked_tools",
                length=len(s),
            )
    return GuardrailResult(allowed=True, length=len(s), rule="accept")


__all__ = [
    "GuardrailConfig",
    "GuardrailResult",
    "check_guardrail",
    "check_tool",
    "configure",
    "get_config",
    "DEFAULT_BLOCKED_WORDS",
    "DEFAULT_BLOCKED_TOOLS",
]
